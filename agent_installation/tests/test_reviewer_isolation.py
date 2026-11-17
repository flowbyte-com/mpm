"""
test_reviewer_isolation.py — the reviewer's isolation, under the
CAPABILITY-REMOVAL architecture.

WHY THIS EXISTS
---------------
A review subagent runs in a child session with the parent's tool
surface. On 2026-11-15 a review subagent wrote a memory into the
operator's LIVE MPM database through the `mpm_memory` tool, against an
explicit instruction not to touch it.

The first fix was a PERMISSION LAYER of pattern rules: deny the MPM
tools by name, deny the binaries wherever they are named, deny the live
workspace paths. Twenty-three independent reviews found that the
pattern was never complete — a rule that does not name a route is not a
closure, and the pattern matcher is not a boundary.

The second fix is CAPABILITY REMOVAL, and it is the one this file
pins:

  1. the reviewer's definition is DEFAULT-DENY — every action is closed
     until exactly one is re-opened;
  2. the one action re-opened is a HOST-CONTROLLED sandboxed execution
     tool, and the HOST controls the sandbox's base, its repository copy
     and its protected set;
  3. the host shell and the host-side file tools are closed outright,
     because they run in the host's process and no permission rule can
     confine them;
  4. the install of that definition FAILS CLOSED, because a deployment
     step that fails open silently downgrades the guarantee.

WHAT THIS PINS
--------------
  1. The definition carries a default deny, opens exactly one action,
     and closes every host-side capability — the shell, the file tools,
     Code Mode, the network tools, the session mutations and the whole
     MPM registry.
  2. The registry is the authority for the MPM tool names: a NEW tool
     cannot be added without a matching deny rule, and a STALE rule
     for a tool that no longer exists fails the test.
  3. The installer REFUSES a definition that does not satisfy that
     policy, verifies the copy it wrote, and never reports success for
     an install it could not verify.
  4. The plugin registers the sandboxed tool, and the tool has no
     unsandboxed mode: the command always goes through the sandbox.
  5. The install flow treats a failed reviewer install as FATAL.
"""

from __future__ import annotations

import importlib.util
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
REGISTRY = REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go"
REVIEWER = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "templates"
    / "agents"
    / "mpm-reviewer.md"
)
INSTALLER = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "scripts"
    / "install_reviewer_agent.py"
)
SANDBOX_SCRIPT = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "scripts"
    / "reviewer_bwrap.py"
)
PLUGIN_V2 = REPO_ROOT / "agent_installation" / "mpm-opencode" / "src" / "index.v2.ts"
PLUGIN_SANDBOX = REPO_ROOT / "agent_installation" / "mpm-opencode" / "src" / "sandbox-exec.ts"
INSTALL_SH = REPO_ROOT / "agent_installation" / "mpm-opencode" / "install.sh"


def _load(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


installer = _load(INSTALLER, "install_reviewer_agent")
sandbox_module = _load(SANDBOX_SCRIPT, "reviewer_bwrap")


def registry_tools() -> set[str]:
    """Every tool the registry publishes."""
    text = REGISTRY.read_text(encoding="utf-8")
    return set(re.findall(r'"(mpm_[a-z_]+)"', text))


def definition_rules() -> list:
    """The definition's permission rules, parsed by the installer."""
    return installer.parse_permissions(REVIEWER.read_text(encoding="utf-8"))


def rules_by_action(rules: list) -> dict:
    """The rules, grouped by action."""
    grouped: dict[str, list[tuple[str, str]]] = {}
    for rule in rules:
        action = str(rule.get("action", ""))
        grouped.setdefault(action, []).append(
            (str(rule.get("resource", "")), str(rule.get("effect", "")))
        )
    return grouped


class ReviewerDefinitionTest(unittest.TestCase):
    """The definition's permission set."""

    def setUp(self) -> None:
        self.rules = definition_rules()
        self.grouped = rules_by_action(self.rules)

    def test_the_default_is_deny(self) -> None:
        """Every action is closed until something re-opens it.

        An omitted permission rule is not a closure: a capability the
        host adds later would be open by default.  The default deny is
        what makes the omission safe.
        """
        self.assertIn(
            "*",
            self.grouped,
            "the definition has no default deny",
        )
        self.assertIn(
            ("*", "deny"),
            self.grouped["*"],
            "the default deny does not cover every resource",
        )

    def test_exactly_one_action_is_allowed(self) -> None:
        """The sandboxed execution tool is the ONLY way out."""
        allowed = {
            action
            for action, entries in self.grouped.items()
            if any(effect == "allow" for _resource, effect in entries)
        }
        self.assertEqual(
            allowed,
            {"sandbox_exec"},
            "the reviewer may use something other than the sandboxed tool",
        )

    def test_the_sandboxed_tool_is_allowed_for_every_resource(self) -> None:
        """`sandbox_exec` is allowed for every resource."""
        self.assertIn(("*", "allow"), self.grouped["sandbox_exec"])

    def test_the_host_shell_is_closed(self) -> None:
        """The shell is closed ENTIRELY, not closed for some binaries.

        A rule that refuses a command string is a pattern; the shell
        itself is a host process with the host user's authority.
        """
        self.assertIn(("*", "deny"), self.grouped["shell"])

    def test_the_host_side_file_tools_are_closed(self) -> None:
        """The file tools run in the HOST's process, outside the sandbox."""
        for action in ("read", "write", "edit", "patch", "grep", "glob", "list"):
            self.assertIn(
                ("*", "deny"),
                self.grouped.get(action, []),
                f"the host-side file tool `{action}` is not closed",
            )

    def test_the_external_directory_boundary_is_closed(self) -> None:
        """An external path is checked before its own read rule."""
        self.assertIn(("*", "deny"), self.grouped["external_directory"])

    def test_code_mode_is_closed(self) -> None:
        """Code Mode reaches every tool the host publishes.

        Closing it closes the browser family with it: the browser is a
        host-side capability that no permission rule can confine.
        """
        self.assertIn(("*", "deny"), self.grouped["execute"])

    def test_the_browser_is_closed_by_name(self) -> None:
        """The browser is a host-side capability, and it is named.

        The default deny covers it, but a named deny is a stronger
        statement: the browser can open a `file://` URL and move
        server-local bytes, so it would reach the operator's
        filesystem from outside the namespace.
        """
        self.assertIn(("*", "deny"), self.grouped.get("browser", []))

    def test_the_capability_loader_is_closed_by_name(self) -> None:
        """The `skill` action loads a capability into the session.

        The host does not gate it for the built-in agents, so a rule
        that names it is the strongest statement this definition can
        make.
        """
        self.assertIn(("*", "deny"), self.grouped.get("skill", []))

    def test_the_network_tools_are_closed(self) -> None:
        """`webfetch` and `websearch` reach the network outside the sandbox."""
        for action in ("webfetch", "websearch"):
            self.assertIn(("*", "deny"), self.grouped.get(action, []))

    def test_the_session_mutations_are_closed(self) -> None:
        """A reviewer reports findings; it does not move its own session."""
        for action in ("subagent", "question"):
            self.assertIn(("*", "deny"), self.grouped.get(action, []))

    def test_the_mcp_resource_surface_is_closed(self) -> None:
        """A resource can carry the substrate's bytes just as a tool can."""
        self.assertIn(("*", "deny"), self.grouped.get("opencode_*", []))

    def test_every_registry_tool_is_denied(self) -> None:
        """Every tool the registry publishes is denied by name."""
        for tool in sorted(registry_tools()):
            self.assertIn(
                ("*", "deny"),
                self.grouped.get(tool, []),
                f"the registry tool `{tool}` is not denied",
            )

    def test_the_deny_list_has_no_stale_entries(self) -> None:
        """A rule for a tool that no longer exists is dead weight.

        It hides a real gap: the reader of the definition believes the
        list is exhaustive, and a name the registry no longer publishes
        is a name the list no longer proves anything about.
        """
        registered = registry_tools()
        named = {
            action
            for action in self.grouped
            if action.startswith("mpm_") and action != "*"
        }
        self.assertEqual(
            sorted(named - registered),
            [],
            "the definition denies MPM tools the registry no longer publishes",
        )

    def test_the_definition_passes_the_installers_policy(self) -> None:
        """The canonical definition satisfies the installer's own check."""
        self.assertEqual(installer.check_policy(self.rules), [])

    def test_the_prose_states_the_boundary(self) -> None:
        """The prose says what is enforced, and what is not.

        A definition that describes a boundary it does not have is the
        failure mode this whole work exists to prevent.
        """
        text = REVIEWER.read_text(encoding="utf-8")
        self.assertIn("sandbox_exec", text)
        self.assertIn("no unsandboxed", text.lower())
        self.assertIn("absent", text.lower())


class ReviewerInstallerTest(unittest.TestCase):
    """The install fails closed."""

    def setUp(self) -> None:
        self.dir = Path(tempfile.mkdtemp(prefix="mpm-review-install-"))
        self.addCleanup(_rmtree, self.dir)
        self.target = self.dir / "agents"

    def _run(self, *args: str, template: Path | None = None) -> subprocess.CompletedProcess:
        command = [sys.executable, str(INSTALLER), "--target", str(self.target), *args]
        if template is not None:
            command += ["--template", str(template)]
        return subprocess.run(command, capture_output=True, text=True, timeout=120)

    def test_the_canonical_template_installs(self) -> None:
        """The canonical definition installs, and is verified."""
        result = self._run("--force")
        self.assertEqual(result.returncode, 0, result.stderr)
        installed = self.target / "mpm-reviewer.md"
        self.assertTrue(installed.exists())
        self.assertEqual(
            installed.read_bytes(),
            REVIEWER.read_bytes(),
            "the installed copy differs from the template",
        )

    def test_the_installed_copy_is_not_world_readable(self) -> None:
        """The definition decides what a reviewer may reach."""
        result = self._run("--force")
        self.assertEqual(result.returncode, 0, result.stderr)
        mode = (self.target / "mpm-reviewer.md").stat().st_mode & 0o777
        self.assertEqual(mode, 0o600, f"the installed definition has mode {mode:o}")

    def test_a_definition_without_the_default_deny_is_refused(self) -> None:
        """A definition that opens the world is not installed."""
        broken = self.dir / "broken.md"
        broken.write_text(
            "---\n"
            "description: broken\n"
            "mode: subagent\n"
            "permissions:\n"
            "  - action: sandbox_exec\n"
            "    resource: \"*\"\n"
            "    effect: allow\n"
            "---\n\nbroken\n",
            encoding="utf-8",
        )
        result = self._run("--force", template=broken)
        self.assertNotEqual(result.returncode, 0, "a defective definition was installed")
        self.assertFalse((self.target / "mpm-reviewer.md").exists())

    def test_a_definition_that_allows_a_host_capability_is_refused(self) -> None:
        """A definition that re-opens the shell is not installed."""
        broken = self.dir / "shell.md"
        broken.write_text(
            "---\n"
            "description: broken\n"
            "mode: subagent\n"
            "permissions:\n"
            "  - action: \"*\"\n"
            "    resource: \"*\"\n"
            "    effect: deny\n"
            "  - action: sandbox_exec\n"
            "    resource: \"*\"\n"
            "    effect: allow\n"
            "  - action: shell\n"
            "    resource: \"*\"\n"
            "    effect: allow\n"
            "---\n\nbroken\n",
            encoding="utf-8",
        )
        result = self._run("--force", template=broken)
        self.assertNotEqual(result.returncode, 0, "a shell-opening definition was installed")
        self.assertIn("shell", result.stderr)

    def test_a_definition_that_omits_the_sandboxed_tool_is_refused(self) -> None:
        """A definition that closes everything is not an isolation either."""
        broken = self.dir / "closed.md"
        broken.write_text(
            "---\n"
            "description: broken\n"
            "mode: subagent\n"
            "permissions:\n"
            "  - action: \"*\"\n"
            "    resource: \"*\"\n"
            "    effect: deny\n"
            "---\n\nbroken\n",
            encoding="utf-8",
        )
        result = self._run("--force", template=broken)
        self.assertNotEqual(result.returncode, 0)

    def test_a_symlinked_target_is_refused(self) -> None:
        """An installer that follows a symlink can write somewhere else."""
        self.target.mkdir(parents=True, exist_ok=True)
        elsewhere = self.dir / "elsewhere.md"
        elsewhere.write_text("untouched\n", encoding="utf-8")
        (self.target / "mpm-reviewer.md").symlink_to(elsewhere)
        result = self._run("--force")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(elsewhere.read_text(encoding="utf-8"), "untouched\n")

    def test_an_existing_definition_is_not_overwritten_without_force(self) -> None:
        """The install does not silently replace an operator's own copy."""
        self.target.mkdir(parents=True, exist_ok=True)
        existing = self.target / "mpm-reviewer.md"
        existing.write_text("operator's own\n", encoding="utf-8")
        result = self._run()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(existing.read_text(encoding="utf-8"), "operator's own\n")

    def test_a_failed_install_leaves_no_staging_file(self) -> None:
        """A refused install leaves nothing behind."""
        broken = self.dir / "broken.md"
        broken.write_text("not a definition at all\n", encoding="utf-8")
        result = self._run("--force", template=broken)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.target / "mpm-reviewer.md.new").exists())


class ReviewerPluginWiringTest(unittest.TestCase):
    """The plugin registers the tool, and the tool is always sandboxed."""

    def test_the_plugin_registers_the_sandboxed_tool(self) -> None:
        """The reviewer's one tool is registered by the HOST's plugin."""
        source = PLUGIN_V2.read_text(encoding="utf-8")
        self.assertIn("SANDBOX_EXEC_TOOL", source)
        self.assertIn("editor.add(", source)
        self.assertIn("new SandboxManager(", source)

    def test_the_tool_name_is_the_one_the_definition_opens(self) -> None:
        """The name the definition opens is the name the plugin registers."""
        source = PLUGIN_SANDBOX.read_text(encoding="utf-8")
        match = re.search(r'SANDBOX_EXEC_TOOL\s*=\s*"([^"]+)"', source)
        self.assertIsNotNone(match, "the sandboxed tool has no name")
        name = match.group(1)
        rules = definition_rules()
        grouped = rules_by_action(rules)
        self.assertIn(("*", "allow"), grouped.get(name, []))

    def test_the_command_is_data_and_never_configuration(self) -> None:
        """The command reaches the sandbox as JSON, not as an argv tail.

        A command that arrived as arguments could be parsed by the
        wrapper; a command that arrives as JSON on stdin cannot.
        """
        source = PLUGIN_SANDBOX.read_text(encoding="utf-8")
        self.assertIn('JSON.stringify(', source)
        self.assertIn('"exec"', source)
        self.assertIn("SANDBOX_SCRIPT", source)

    def test_the_tool_has_no_privileged_mode(self) -> None:
        """There is no unsandboxed path in the tool's implementation.

        The tool does not know whether it is being used by the reviewer
        or by the operator, and it must not need to: it is always
        confined.  So there is no flag, no environment variable and no
        branch that runs a command on the host.
        """
        source = PLUGIN_SANDBOX.read_text(encoding="utf-8")
        # The only spawn is the sandbox script itself.
        spawns = re.findall(r"spawn\(\s*([A-Za-z_.\"\'][^,)]*)", source)
        for target in spawns:
            self.assertIn(
                "python",
                target,
                f"the tool spawns something other than the sandbox: {target}",
            )
        for forbidden in ("UNSANDBOXED", "noSandbox", "skipSandbox", "bypass"):
            self.assertNotIn(
                forbidden,
                source,
                f"the tool has an unsandboxed mode named {forbidden}",
            )

    def test_the_default_base_is_one_the_sandbox_accepts(self) -> None:
        """The plugin's default base must pass the sandbox's own guard.

        The plugin resolves the base under the operator's own cache
        directory — which is inside the operator's home.  The sandbox
        refuses a base that is inside a protected root, and the home
        holds the credentials and the live runtime root, so the two
        must agree: a base the plugin chooses must be a base the
        sandbox will actually run in.  A disagreement would make the
        tool permanently refuse, which reads as a safeguard and is
        the opposite.
        """
        source = PLUGIN_SANDBOX.read_text(encoding="utf-8")
        self.assertIn('".cache"', source)
        self.assertIn('"mpm-opencode"', source)
        self.assertIn('"review-sandbox"', source)

        home = sandbox_module.operator_home()
        self.assertIsNotNone(home)
        base = home / ".cache" / "mpm-opencode" / "review-sandbox"
        resolved = sandbox_module.resolve_base(base)
        self.assertEqual(resolved, base)

    def test_the_install_flow_stages_the_sandbox(self) -> None:
        """The staged runtime package carries the sandbox script."""
        text = INSTALL_SH.read_text(encoding="utf-8")
        self.assertIn("scripts/reviewer_bwrap.py", text)
        self.assertIn("dist/sandbox-exec.js", text)

    def test_a_failed_reviewer_install_is_fatal(self) -> None:
        """The install must not continue with a weaker isolation.

        A deployment step that fails open converts an error into a
        SILENT DOWNGRADE of the guarantee.
        """
        text = INSTALL_SH.read_text(encoding="utf-8")
        self.assertIn("install_reviewer_agent", text)
        self.assertRegex(
            text,
            r"if ! install_reviewer_agent; then\s*\n(?:.*\n)*?\s*exit 1",
            "a failed reviewer install does not stop the install",
        )


class ReviewerSandboxScriptTest(unittest.TestCase):
    """The sandbox script's hardening."""

    def setUp(self) -> None:
        self.sandbox = _load(SANDBOX_SCRIPT, "reviewer_bwrap")
        self.base = Path(tempfile.mkdtemp(prefix="mpm-review-sbx-contract-"))
        self.addCleanup(_rmtree, self.base)

    def test_the_trust_store_is_not_bound(self) -> None:
        """`/etc/ssl` is the trust store, and a review does not need it.

        The binding list is an ALLOW-LIST: a path in it is a path the
        reviewer can read.
        """
        self.assertNotIn("/etc/ssl", self.sandbox.RUNTIME_DIRS)
        self.assertNotIn("/etc/passwd", self.sandbox.RUNTIME_DIRS)
        self.assertNotIn("/etc/group", self.sandbox.RUNTIME_DIRS)

    def test_the_writable_tree_is_bound_by_descriptor(self) -> None:
        """The writable part is bound by an `O_NOFOLLOW` descriptor.

        A descriptor names the INODE, so a path that is swapped for a
        symlink after the check cannot change what is mounted.
        """
        tree = self.sandbox.build_tree(self.base, None)
        fds = self.sandbox.open_tree(tree)
        self.addCleanup(self.sandbox.close_tree, fds)
        args = self.sandbox.bwrap_args(tree, fds)
        binds = [
            args[i + 2]
            for i, token in enumerate(args)
            if token in ("--bind-fd", "--ro-bind-fd") and i + 2 < len(args)
        ]
        self.assertEqual(
            sorted(binds),
            sorted(
                [f"/sandbox/{name}" for name in self.sandbox.TREE] + ["/etc"]
            ),
            "the writable tree is not bound by descriptor",
        )
        # Nothing is bound read-write BY PATH.
        self.assertNotIn("--bind", args)

    def test_the_command_is_separated_from_the_options(self) -> None:
        """The `--` separator is the boundary against option injection.

        Without it, a command vector could carry `--bind / /` and mount
        the host's root into the namespace.
        """
        tree = self.sandbox.build_tree(self.base, None)
        fds = self.sandbox.open_tree(tree)
        self.addCleanup(self.sandbox.close_tree, fds)
        args = self.sandbox.namespace_argv(tree, fds, ["/bin/true"])
        self.assertIn("--", args)
        self.assertEqual(args[-1], "/bin/true")
        self.assertLess(args.index("--"), len(args) - 1)

    def test_a_command_that_begins_with_an_option_is_refused(self) -> None:
        """A program named like an option is an attempt at the parser."""
        with self.assertRaises(self.sandbox.SandboxRefused):
            self.sandbox.validate_command(["--bind", "/", "/"])

    def test_the_protection_list_comes_from_the_passwd_database(self) -> None:
        """`$HOME` is agent-influenceable, so it cannot be the authority."""
        home = self.sandbox.operator_home()
        self.assertIsNotNone(home, "the passwd database yielded no home")
        roots = self.sandbox.protected_roots()
        self.assertIn(
            home / ".mpm",
            roots,
            "the live runtime root is not in the protected set",
        )
        self.assertIn(
            home / ".ssh",
            roots,
            "the credentials are not in the protected set",
        )
        self.assertIn(
            home / ".config",
            roots,
            "the private configuration is not in the protected set",
        )

    def test_the_plugin_default_base_is_accepted(self) -> None:
        """The base the plugin actually uses must not be refused.

        The plugin puts the sandbox in the operator's own cache
        directory, which is inside the operator's home.  A guard that
        refused every base inside the home would refuse the one the
        tool uses, and the tool would then never run at all — a
        failure dressed as a safeguard.
        """
        home = self.sandbox.operator_home()
        self.assertIsNotNone(home)
        base = home / ".cache" / "mpm-opencode" / "review-sandbox"
        resolved = self.sandbox.resolve_base(base)
        self.assertTrue(str(resolved).startswith(str(home)))

    def test_the_home_itself_is_refused(self) -> None:
        """The base may not BE the operator's home."""
        home = self.sandbox.operator_home()
        self.assertIsNotNone(home)
        with self.assertRaises(self.sandbox.SandboxRefused):
            self.sandbox.resolve_base(home)

    def test_a_forged_home_does_not_remove_the_protection(self) -> None:
        """Setting `HOME` to somewhere else must not UNPROTECT the home.

        This is the failure the protection list had: it was derived
        from `$HOME`, so an agent that set `HOME` chose what was
        protected.
        """
        import os

        real_home = self.sandbox.operator_home()
        forged = self.base / "forged-home"
        forged.mkdir(parents=True, exist_ok=True)
        previous = os.environ.get("HOME")
        os.environ["HOME"] = str(forged)
        try:
            roots = self.sandbox.protected_roots()
            self.assertIn(
                real_home / ".mpm",
                roots,
                "forging HOME removed the live runtime root from the protected set",
            )
            self.assertIn(
                real_home / ".ssh",
                roots,
                "forging HOME removed the credentials from the protected set",
            )
            with self.assertRaises(self.sandbox.SandboxRefused):
                self.sandbox.resolve_base(real_home / ".mpm")
        finally:
            if previous is None:
                os.environ.pop("HOME", None)
            else:
                os.environ["HOME"] = previous

    def test_a_symlinked_base_is_refused(self) -> None:
        """A symlinked base can be swapped for another target."""
        link = self.base / "link"
        link.symlink_to(self.base / "real")
        with self.assertRaises(self.sandbox.SandboxRefused):
            self.sandbox.resolve_base(link)

    def test_a_base_inside_a_protected_root_is_refused(self) -> None:
        """The sandbox is never created inside the thing it isolates."""
        home = self.sandbox.operator_home()
        with self.assertRaises(self.sandbox.SandboxRefused):
            self.sandbox.resolve_base(home / ".mpm" / "sbx")
        with self.assertRaises(self.sandbox.SandboxRefused):
            self.sandbox.resolve_base(Path("/"))
        with self.assertRaises(self.sandbox.SandboxRefused):
            self.sandbox.resolve_base(Path("/etc"))

    def test_a_disposable_base_is_accepted(self) -> None:
        """The refusal is not a blanket."""
        self.sandbox.resolve_base(self.base)


def _rmtree(path: Path) -> None:
    import shutil

    shutil.rmtree(path, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
