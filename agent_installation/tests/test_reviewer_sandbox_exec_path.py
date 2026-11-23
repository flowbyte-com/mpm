"""
test_reviewer_sandbox_exec_path.py — the reviewer's ACTUAL execution
path is confined.

WHY THIS EXISTS
---------------
The namespace sandbox (`reviewer_bwrap.py`) was proved and committed,
and it was INERT: the reviewer's shell did not go through it.  A test
that exercises the helper directly proves the helper.  It does not
prove the boundary, because the defect being guarded against is
precisely that the reviewer's path does NOT go through the helper.

So this file drives the EXACT invocation the host plugin makes, and
proves two things together:

  1. the invocation the plugin performs confines the command — a
     disposable sentinel outside the sandbox is unreachable, the
     operator's home is unreachable, the trust store is unreachable,
     and there is no network; and
  2. that invocation is the reviewer's ONLY route — the definition
     closes the shell and every host-side file tool, so there is no
     unsandboxed fallback to fall back TO.

The invocation is checked against the plugin's SOURCE as well, so the
test cannot quietly drift away from what the plugin does.

THE SENTINELS ARE DISPOSABLE
-----------------------------
Every probe uses a directory this test creates and removes.  The live
substrate, the operator's checkout and the operator's credentials are
never opened: they are NAMED at most, and naming a path is not opening
it.
"""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SANDBOX_SCRIPT = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "scripts"
    / "reviewer_bwrap.py"
)
PLUGIN_SANDBOX = REPO_ROOT / "agent_installation" / "mpm-opencode" / "src" / "sandbox-exec.ts"
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


def _load(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


sandbox = _load(SANDBOX_SCRIPT, "reviewer_bwrap")
installer = _load(INSTALLER, "install_reviewer_agent")


def _bwrap_usable() -> bool:
    """Is bubblewrap available AND able to build a namespace here?"""
    if not Path(sandbox.BWRAP).exists():
        return False
    try:
        result = subprocess.run(
            [
                sandbox.BWRAP,
                "--ro-bind",
                "/",
                "/",
                "--unshare-all",
                "--unshare-user",
                "--die-with-parent",
                "/bin/true",
            ],
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return result.returncode == 0


class SandboxInvocation:
    """The invocation the host plugin makes.

    `src/sandbox-exec.ts` spawns

        python3 <script> --base <base> --protect <dir>... exec

    with the command as a JSON payload on stdin, and reads the result
    envelope from file descriptor 3.  This class does exactly that,
    and nothing else, so a probe here is a probe of the real path.
    """

    def __init__(self, base: Path, protect: list[Path], source: Path | None = None) -> None:
        self.base = base
        self.protect = protect
        self.source = source

    def _command(self, *tail: str) -> list[str]:
        command = [
            sys.executable,
            str(SANDBOX_SCRIPT),
            "--base",
            str(self.base),
        ]
        for directory in self.protect:
            command += ["--protect", str(directory)]
        return command + list(tail)

    def init(self) -> dict:
        """Build the disposable tree — the plugin's `ensureTree` step."""
        tail = ["init"]
        if self.source is not None:
            tail += ["--source", str(self.source)]
        result = subprocess.run(
            self._command(*tail), capture_output=True, text=True, timeout=600
        )
        return {"code": result.returncode, "stdout": result.stdout, "stderr": result.stderr}

    def run(
        self,
        argv: list,
        cwd: str | None = "repo",
        timeout_ms: int = 120_000,
        env: dict[str, str] | None = None,
    ) -> dict:
        command = self._command("exec")
        payload = json.dumps(
            {"argv": argv, "cwd": cwd, "timeout_ms": timeout_ms}
        )
        # The envelope is written to file descriptor 3.  The pipe's read
        # end is passed into the child and dup2'd onto 3 there, exactly
        # as the plugin's `stdio: [pipe, pipe, pipe, pipe]` does.
        envelope_read, envelope_write = os.pipe()
        try:
            process = subprocess.Popen(
                command,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                pass_fds=(envelope_read,),
                text=True,
                env=env,
                preexec_fn=lambda: os.dup2(envelope_read, 3),
            )
        except BaseException:
            os.close(envelope_read)
            os.close(envelope_write)
            raise
        os.close(envelope_read)
        envelope_chunks: list[bytes] = []

        def _drain() -> None:
            while True:
                try:
                    chunk = os.read(envelope_write, 65536)
                except OSError:
                    # The write end was closed once the child exited.
                    break
                if not chunk:
                    break
                envelope_chunks.append(chunk)

        import threading

        reader = threading.Thread(target=_drain, daemon=True)
        reader.start()
        stdout, stderr = process.communicate(input=payload, timeout=timeout_ms / 1000 + 60)
        os.close(envelope_write)
        reader.join(timeout=5)
        envelope_text = b"".join(envelope_chunks).decode("utf-8", "replace")
        envelope = None
        for line in envelope_text.split("\n"):
            if line.strip():
                try:
                    envelope = json.loads(line)
                except json.JSONDecodeError:
                    envelope = None
        return {
            "code": process.returncode,
            "stdout": stdout,
            "stderr": stderr,
            "envelope": envelope,
        }


@unittest.skipUnless(_bwrap_usable(), "bubblewrap cannot build a namespace here")
class ReviewerExecutionPathTest(unittest.TestCase):
    """The reviewer's execution path, against disposable sentinels."""

    def setUp(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="mpm-review-path-"))
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)

        # The disposable copy of "the repository under review".
        self.source = self.root / "checkout"
        (self.source / "pkg").mkdir(parents=True)
        (self.source / "pkg" / "main.go").write_text(
            "package main\n\nfunc main() {}\n", encoding="utf-8"
        )
        (self.source / "README.md").write_text("# the repository\n", encoding="utf-8")

        # A DISPOSABLE sentinel that stands in for protected host data:
        # it lives outside the sandbox base, next to it.
        self.protected = self.root / "protected"
        self.protected.mkdir()
        (self.protected / "secret.txt").write_text(
            "DISPOSABLE SENTINEL - not real data\n", encoding="utf-8"
        )
        (self.protected / "fake-mpm.db").write_bytes(b"DISPOSABLE SENTINEL DB\n")

        self.base = self.root / "sandbox"
        self.invocation = SandboxInvocation(
            self.base, [self.source, self.protected], self.source
        )
        # The plugin builds the tree before it runs anything in it.
        built = self.invocation.init()
        self.assertEqual(built["code"], 0, built["stderr"])

    # ── the escape probes ───────────────────────────────────────────

    def test_the_sentinel_outside_the_sandbox_is_unreachable(self) -> None:
        """A command inside the namespace cannot read a protected file.

        This is the property a permission matcher cannot provide: an
        unbound path is not refused, it does not EXIST.
        """
        result = self.invocation.run(["/bin/cat", str(self.protected / "secret.txt")])
        self.assertNotEqual(
            result["code"],
            0,
            f"the sentinel was READ from inside the namespace: {result}",
        )
        self.assertNotIn("DISPOSABLE SENTINEL", result["stdout"])

    def test_the_sentinel_database_is_unreachable(self) -> None:
        """A database outside the sandbox is unreachable."""
        result = self.invocation.run(["/bin/cat", str(self.protected / "fake-mpm.db")])
        self.assertNotEqual(result["code"], 0)
        self.assertNotIn("DISPOSABLE SENTINEL", result["stdout"])

    def test_the_protected_directory_is_not_listed(self) -> None:
        """The protected directory is not even in the listing."""
        result = self.invocation.run(["/bin/ls", str(self.protected)])
        self.assertNotEqual(result["code"], 0)

    def test_the_operators_home_is_unreachable(self) -> None:
        """The operator's home directory does not exist inside.

        The path is only NAMED: it is never opened from inside the
        namespace.
        """
        home = sandbox.operator_home()
        self.assertIsNotNone(home)
        result = self.invocation.run(["/bin/ls", str(home)])
        self.assertNotEqual(result["code"], 0)
        result = self.invocation.run(["/bin/cat", str(home / ".mpm" / "src" / "db" / "mpm.db")])
        self.assertNotEqual(result["code"], 0)

    def test_the_trust_store_is_unreachable(self) -> None:
        """`/etc/ssl` is not bound.

        The binding list is an allow-list: a path in it is a path the
        reviewer can read, and the trust store is not something a code
        review needs.
        """
        result = self.invocation.run(["/bin/ls", "/etc/ssl"])
        self.assertNotEqual(result["code"], 0)

    def test_the_account_database_is_the_sandboxs_own(self) -> None:
        """The host's `/etc/passwd` is not bound either.

        The sandbox carries a synthetic one that names `reviewer`, so
        the operator's ACCOUNT DATABASE is not readable inside.

        What this does NOT claim: that the operator's name and home
        path are undisclosed everywhere.  They are not disclosed
        *here*, but the namespace's own mount table records the host
        paths of the mounts it was given, so the base's path under the
        operator's home is visible to anything that can read
        `/proc/self/mountinfo`.  That is METADATA about the host — the
        paths it names are not bound, so nothing there can be read
        through it — and it is stated as a residual risk rather than
        papered over.
        """
        result = self.invocation.run(["/bin/cat", "/etc/passwd"])
        self.assertEqual(result["code"], 0, result["stderr"])
        self.assertIn("reviewer", result["stdout"])
        home = sandbox.operator_home()
        if home is not None:
            self.assertNotIn(str(home), result["stdout"])

    def test_the_network_is_unreachable(self) -> None:
        """The namespace has no network."""
        result = self.invocation.run(
            ["/bin/sh", "-c", "getent hosts example.com >/dev/null 2>&1"]
        )
        self.assertNotEqual(result["code"], 0)

    def test_the_option_injection_is_refused(self) -> None:
        """A command vector cannot carry an option into the namespace.

        `--bind / /` would mount the host's root into the namespace:
        a complete escape.  The command is refused, and it is refused
        before the namespace is built.
        """
        result = self.invocation.run(
            ["/bin/sh", "-c", "echo ESCAPED; ls /"]
        )
        # The benign form works.
        self.assertEqual(result["code"], 0, result["stderr"])

        result = self.invocation.run(["--bind", "/", "/", "/bin/ls", "/"])
        self.assertNotEqual(result["code"], 0)
        self.assertNotIn("ESCAPED", result["stdout"])

    def test_a_command_beginning_with_an_option_is_refused(self) -> None:
        """A program named like an option is refused."""
        result = self.invocation.run(["--version"])
        self.assertNotEqual(result["code"], 0)

    def test_a_working_directory_outside_the_sandbox_is_refused(self) -> None:
        """The command cannot choose where it starts."""
        result = self.invocation.run(["/bin/pwd"], cwd="/etc")
        self.assertNotEqual(result["code"], 0)

    # ── the sandbox still does its job ──────────────────────────────

    def test_the_disposable_copy_of_the_repository_is_present(self) -> None:
        """A review can still read the code it is reviewing."""
        result = self.invocation.run(["/bin/cat", "/sandbox/repo/README.md"])
        self.assertEqual(result["code"], 0, result["stderr"])
        self.assertIn("the repository", result["stdout"])

    def test_the_disposable_tree_is_writable(self) -> None:
        """A review can still write its findings."""
        result = self.invocation.run(
            ["/bin/sh", "-c", "touch /sandbox/findings/finding-1 && echo WROTE"]
        )
        self.assertEqual(result["code"], 0, result["stderr"])
        self.assertIn("WROTE", result["stdout"])

    def test_the_environment_is_the_disposable_one(self) -> None:
        """`HOME`, `TMPDIR` and `MPM_WORKSPACE` point inside."""
        result = self.invocation.run(
            ["/bin/sh", "-c", "echo $HOME; echo $TMPDIR; echo $MPM_WORKSPACE"]
        )
        self.assertEqual(result["code"], 0, result["stderr"])
        self.assertIn("/sandbox/home", result["stdout"])
        self.assertIn("/sandbox/tmp", result["stdout"])
        self.assertIn("/sandbox/workspace", result["stdout"])

    def test_the_toolchain_is_present(self) -> None:
        """A review can still run the tests.

        The toolchain is bound READ-ONLY from wherever the HOST keeps
        it — `/usr` on a conventional install, a hosted tool cache on a
        CI runner — and the sandbox's PATH points at it.  A host with
        no Go toolchain has nothing to bind, so the assertion is
        SKIPPED there rather than failed: the property under test is
        "the toolchain the host has is runnable inside", not "every
        host has one".
        """
        if shutil.which("go") is None:
            self.skipTest("this host has no Go toolchain to bind")
        result = self.invocation.run(["/bin/sh", "-c", "go version"])
        self.assertEqual(result["code"], 0, result["stderr"])
        self.assertIn("go version", result["stdout"])

    def test_the_sandbox_command_cannot_read_a_provider_credential(self) -> None:
        """The command cannot see the reviewer's provider credential.

        The credential is inherited by the host-side wrapper when the
        OpenCode process starts it, so the wrapper's env is a real threat
        surface. The wrapper scrubs it, and bwrap clears the command env.
        This probe checks the command's own environment and /proc.
        """
        name = "MINIMAX_API_KEY"
        marker = "MARKER-NOT-A-REAL-CREDENTIAL"
        previous = os.environ.get(name)
        os.environ[name] = marker
        try:
            result = self.invocation.run(
                [
                    "/bin/sh",
                    "-c",
                    "env; echo ---; cat /proc/1/environ; echo ---; "
                    "cat /proc/self/environ; echo ---; ls /proc",
                ],
                cwd="tmp",
                env={
                    "PATH": "/usr/local/bin:/usr/bin:/bin",
                    "HOME": str(self.root / "test-home"),
                    name: marker,
                },
            )
        finally:
            if previous is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = previous
        combined = result["stdout"] + result["stderr"] + json.dumps(result["envelope"])
        self.assertNotIn(marker, combined)
        self.assertNotIn(name, combined)


class DisposableCopyMetadataTest(unittest.TestCase):
    """The disposable copy does not carry the operator's PATHS.

    A `git clone` records where it came from: the origin remote in
    `.git/config`, the reflog, and `FETCH_HEAD`.  Those are absolute
    paths under the operator's home, so they tell a less-trusted
    reviewer where the operator's checkout is — and therefore what the
    operator's account is called.

    The review needs none of it: the copy is already made and the
    remote is never used again.  So the records are removed.

    The history is NOT removed: it is the review material.
    """

    MARKER = "operator-checkout-path-marker"

    def setUp(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="mpm-review-metadata-"))
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)

        # A disposable git repository whose path carries a marker, so a
        # disclosure of the source path is detectable.
        self.source = self.root / self.MARKER
        (self.source).mkdir(parents=True)
        (self.source / "README.md").write_text("# the repository\n", encoding="utf-8")
        env = dict(os.environ)
        env.update(
            {
                "GIT_AUTHOR_NAME": "reviewer-fixture",
                "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
                "GIT_COMMITTER_NAME": "reviewer-fixture",
                "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
            }
        )
        for command in (
            ["git", "init", "--quiet"],
            ["git", "add", "-A"],
            ["git", "commit", "--quiet", "-m", "the first commit"],
        ):
            result = subprocess.run(
                command, cwd=self.source, capture_output=True, text=True, timeout=120, env=env
            )
            self.assertEqual(result.returncode, 0, result.stderr)

        self.base = self.root / "sandbox"
        self.invocation = SandboxInvocation(self.base, [self.source], self.source)
        built = self.invocation.init()
        self.assertEqual(built["code"], 0, built["stderr"])

        self.copy = self.base / "source" / "repo"

    def test_the_copy_carries_no_path_to_the_operator(self) -> None:
        """The clone's own metadata names no host location."""
        config = self.copy / ".git" / "config"
        self.assertTrue(config.is_file(), "the copy is not a git clone")
        text = config.read_text(encoding="utf-8", errors="replace")
        self.assertNotIn(self.MARKER, text, "the copy's config names the operator's checkout")
        self.assertNotIn("[remote", text, "the copy still carries a remote")
        for line in text.splitlines():
            if "=" not in line:
                continue
            value = line.split("=", 1)[1].strip()
            self.assertFalse(
                value.startswith("/") or value.startswith("~"),
                f"the copy's config names a host location: {line!r}",
            )

    def test_the_reflog_and_the_fetch_head_are_gone(self) -> None:
        """The records of the clone's source are removed, not rewritten."""
        self.assertFalse(
            (self.copy / ".git" / "logs").exists(),
            "the reflog still records where the clone came from",
        )
        self.assertFalse(
            (self.copy / ".git" / "FETCH_HEAD").exists(),
            "FETCH_HEAD still records where the clone came from",
        )

    def test_the_history_survives_the_scrub(self) -> None:
        """The scrub does not damage the review material."""
        result = subprocess.run(
            ["git", "-C", str(self.copy), "log", "--oneline"],
            capture_output=True,
            text=True,
            timeout=120,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("the first commit", result.stdout)

    @unittest.skipUnless(_bwrap_usable(), "bubblewrap cannot build a namespace here")
    def test_the_reviewer_cannot_read_the_operator_path_from_inside(self) -> None:
        """From inside the namespace, the copy discloses no host path."""
        result = self.invocation.run(["/bin/cat", "/sandbox/repo/.git/config"])
        self.assertEqual(result["code"], 0, result["stderr"])
        self.assertNotIn(self.MARKER, result["stdout"])
        self.assertNotIn("[remote", result["stdout"])


class SandboxFailsClosedTest(unittest.TestCase):
    """There is no unsandboxed fallback."""

    def setUp(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="mpm-review-failclosed-"))
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        self.base = self.root / "sandbox"

    def test_a_missing_bubblewrap_refuses_rather_than_running(self) -> None:
        """When the namespace cannot be built, the command does not run.

        A sandbox that cannot be established is a REFUSAL, not a
        degraded mode.  The marker file is the proof: if the command
        ran on the host, the marker would exist.
        """
        marker = self.root / "ran-on-the-host"
        original = sandbox.BWRAP
        sandbox.BWRAP = str(self.root / "no-such-bwrap")
        try:
            tree = sandbox.build_tree(self.base, None)
            with self.assertRaises(sandbox.SandboxRefused):
                sandbox.execute(
                    tree, ["/bin/sh", "-c", f"touch {marker}"]
                )
        finally:
            sandbox.BWRAP = original
        self.assertFalse(
            marker.exists(),
            "the command RAN ON THE HOST when the sandbox could not be built",
        )

    def test_a_refused_command_does_not_run(self) -> None:
        """A command the validator refuses does not run at all."""
        marker = self.root / "ran-on-the-host"
        tree = sandbox.build_tree(self.base, None)
        with self.assertRaises(sandbox.SandboxRefused):
            sandbox.validate_command(["--bind", "/", "/"])
        self.assertFalse(marker.exists())

    def test_a_command_that_begins_with_an_option_does_not_run(self) -> None:
        """The option-injection vector is refused before anything runs."""
        marker = self.root / "ran-on-the-host"
        tree = sandbox.build_tree(self.base, None)
        with self.assertRaises(sandbox.SandboxRefused):
            sandbox.execute(tree, ["--bind", "/", "/", "/bin/sh", "-c", f"touch {marker}"])
        self.assertFalse(marker.exists())


class ReviewerRouteClosureTest(unittest.TestCase):
    """The confined invocation is the reviewer's ONLY route."""

    def setUp(self) -> None:
        self.rules = installer.parse_permissions(
            REVIEWER.read_text(encoding="utf-8")
        )
        self.grouped: dict[str, list[tuple[str, str]]] = {}
        for rule in self.rules:
            self.grouped.setdefault(str(rule.get("action", "")), []).append(
                (str(rule.get("resource", "")), str(rule.get("effect", "")))
            )

    def test_the_only_allowed_action_is_the_sandboxed_tool(self) -> None:
        """There is no second route."""
        allowed = {
            action
            for action, entries in self.grouped.items()
            if any(effect == "allow" for _resource, effect in entries)
        }
        self.assertEqual(allowed, {"sandbox_exec"})

    def test_the_shell_is_closed(self) -> None:
        """The unsandboxed execution route is closed."""
        self.assertIn(("*", "deny"), self.grouped.get("shell", []))

    def test_the_host_side_file_tools_are_closed(self) -> None:
        """The host's filesystem is not reachable through a file tool."""
        for action in ("read", "write", "edit", "patch", "grep", "glob", "list"):
            self.assertIn(("*", "deny"), self.grouped.get(action, []))

    def test_the_browser_and_the_capability_loader_are_closed(self) -> None:
        """Two host-side capabilities that the sandbox cannot reach.

        The browser opens a `file://` URL and moves server-local bytes;
        the capability loader loads text from elsewhere into the
        session. Neither runs inside the namespace, so neither is
        covered by the sandbox.
        """
        for action in ("browser", "skill"):
            self.assertIn(("*", "deny"), self.grouped.get(action, []))


class PluginInvocationAgreementTest(unittest.TestCase):
    """The test's invocation IS the plugin's invocation."""

    def setUp(self) -> None:
        self.source = PLUGIN_SANDBOX.read_text(encoding="utf-8")

    def test_the_plugin_spawns_the_script_with_the_same_shape(self) -> None:
        """Base, protect, `exec`, JSON on stdin, envelope on fd 3."""
        self.assertIn('"--base"', self.source)
        self.assertIn('"--protect"', self.source)
        self.assertIn('"exec"', self.source)
        self.assertIn("JSON.stringify(", self.source)
        self.assertIn("stdio: [\"pipe\", \"pipe\", \"pipe\", \"pipe\"]", self.source)
        self.assertIn("child.stdio[3]", self.source)

    def test_the_plugin_never_runs_a_command_on_the_host(self) -> None:
        """The only process the tool spawns is the sandbox script."""
        self.assertNotIn("execSync(", self.source)
        self.assertNotIn("exec(", self.source)
        self.assertNotIn("child_process.exec", self.source)




if __name__ == "__main__":
    unittest.main()
