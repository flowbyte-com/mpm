"""The OS-CONFINED sandbox's contract.

The reviewer's isolation has three layers, and only the last is a
boundary:

  1. the PERMISSION layer — a default-deny tool catalogue;
  2. the ENVIRONMENT sandbox (`reviewer_sandbox.py`) — disposable
     `HOME`, `TMPDIR` and `MPM_WORKSPACE`;
  3. the NAMESPACE sandbox (`reviewer_bwrap.py`) — bubblewrap, where a
     path that is not bound does not exist inside.

These tests pin the third layer's CONTRACT — the shape of the namespace,
and what the namespace can and cannot see — so a change that quietly
loosens it fails a test rather than passing unnoticed.

The live probe needs bubblewrap and an unprivileged user namespace.
Where they are absent the live part SKIPS, and the contract part still
runs: the argument list is a pure function of the tree.
"""

from __future__ import annotations

import importlib.util
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

HERE = Path(__file__).resolve().parent
SCRIPT = HERE.parent / "mpm-opencode" / "scripts" / "reviewer_bwrap.py"


def _load():
    spec = importlib.util.spec_from_file_location("reviewer_bwrap", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


sandbox = _load()


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


class ReviewerBwrapTest(unittest.TestCase):
    """The namespace's shape."""

    def setUp(self) -> None:
        self.base = Path(tempfile.mkdtemp(prefix="mpm-review-sbx-test-"))
        self.addCleanup(shutil.rmtree, self.base, ignore_errors=True)
        self.tree = sandbox.build_tree(self.base, None)
        self.fds = sandbox.open_tree(self.tree)
        self.addCleanup(sandbox.close_tree, self.fds)

    def test_the_runtime_is_bound_read_only(self) -> None:
        """The runtime is READ-ONLY inside the namespace.

        A writable runtime is not confinement: the reviewer could
        replace a tool the next command runs.  So every runtime bind
        is `--ro-bind`.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        for path in sandbox.RUNTIME_DIRS:
            if not Path(path).exists():
                continue
            index = args.index(path)
            self.assertEqual(
                args[index - 1],
                "--ro-bind",
                f"the runtime bind for {path} is not read-only",
            )

    def test_only_the_disposable_tree_is_writable(self) -> None:
        """Exactly the reviewer's own tree is bound read-write."""
        args = sandbox.bwrap_args(self.tree, self.fds)
        writable = [
            args[i + 2]
            for i, token in enumerate(args)
            if token == "--bind-fd" and i + 2 < len(args)
        ]
        self.assertEqual(
            sorted(writable),
            sorted(f"{sandbox.PREFIX}/{name}" for name in sandbox.TREE),
            "the namespace binds something else read-write",
        )

    def test_nothing_is_bound_read_write_by_path(self) -> None:
        """A writable bind is given by DESCRIPTOR, never by path.

        A descriptor names the inode, so a path that is swapped for a
        symlink after the check cannot change what is mounted.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        self.assertNotIn("--bind", args)

    def test_the_environment_is_cleared_and_then_set(self) -> None:
        """`--clearenv` is emitted before the `--setenv` flags.

        bubblewrap CLEARS the environment first and then applies the
        `--setenv` variables, whatever order the two appear in — so the
        order here is not what makes it work, it is what makes it READ
        correctly.  What matters is that the environment IS cleared:
        without it the reviewer would inherit the operator's
        environment, API keys included.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        self.assertIn("--clearenv", args)
        self.assertLess(
            args.index("--clearenv"),
            args.index("--setenv"),
            "the environment is cleared AFTER it is set",
        )
        for key, value in (
            ("HOME", f"{sandbox.PREFIX}/home"),
            ("TMPDIR", f"{sandbox.PREFIX}/tmp"),
            ("MPM_WORKSPACE", f"{sandbox.PREFIX}/workspace"),
        ):
            self.assertIn(value, args, f"{key} is not the disposable one")

    def test_the_host_toolchain_is_bound_read_only(self) -> None:
        """The toolchain is bound READ-ONLY, from wherever the host keeps it.

        The static runtime list is the SYSTEM's toolchain, which lives
        under /usr on a conventional install.  A host that keeps it
        elsewhere — a CI runner's hosted tool cache — would otherwise
        give the reviewer no compiler, and the review would quietly
        degrade to reading code.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        for index, root in enumerate(self.tree.get("toolchain_roots", [])):
            fd = self.fds[f"toolchain:{index}"]
            position = args.index(str(root))
            self.assertEqual(
                args[position - 2],
                "--ro-bind-fd",
                f"the toolchain bind for {root} is not a read-only descriptor bind",
            )
            self.assertEqual(
                args[position - 1],
                str(fd),
                f"the toolchain bind for {root} does not carry its descriptor",
            )
            self.assertIn(
                f"{root}/bin",
                sandbox.sandbox_path(self.tree.get("toolchain_roots")),
                f"the sandbox's PATH does not reach the toolchain at {root}",
            )

    def test_a_discovered_root_outside_usr_is_bound_by_descriptor(self) -> None:
        """The POSITIVE case: a real root outside /usr is bound by fd.

        On a conventional host the discovery finds nothing (the Go
        toolchain lives under /usr, which the static runtime list already
        binds), so the descriptor-bind branch would otherwise never be
        exercised.  Here the discovery is pointed at a disposable root,
        and the bind is checked against the descriptor that names it.
        """
        synthetic = self.base / "toolchain"
        (synthetic / "bin").mkdir(parents=True)
        original = sandbox.toolchain_roots
        sandbox.toolchain_roots = lambda: [synthetic]
        try:
            tree = sandbox.build_tree(self.base / "tree", None)
            fds = sandbox.open_tree(tree)
        finally:
            sandbox.toolchain_roots = original
        self.addCleanup(sandbox.close_tree, fds)

        args = sandbox.bwrap_args(tree, fds)
        position = args.index(str(synthetic))
        self.assertEqual(
            args[position - 2],
            "--ro-bind-fd",
            "a discovered root outside /usr is not bound by descriptor",
        )
        self.assertEqual(
            args[position - 1],
            str(fds["toolchain:0"]),
            "the bind does not carry the descriptor that names the root",
        )
        self.assertEqual(
            os.fstat(fds["toolchain:0"]).st_ino,
            os.stat(synthetic).st_ino,
            "the descriptor does not name the discovered root",
        )
        self.assertIn(
            f"{synthetic}/bin",
            sandbox.sandbox_path(tree.get("toolchain_roots")),
            "the sandbox's PATH does not reach the discovered root",
        )

    def test_a_toolchain_root_is_bound_by_descriptor_not_by_path(self) -> None:
        """A discovered root is bound the way the reviewer's own tree is.

        A path-based bind leaves a window between the check and the
        mount, in which the root can be swapped for another target.  The
        reviewer's tree does not have that window, and neither does the
        toolchain.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        for root in self.tree.get("toolchain_roots", []):
            self.assertNotIn(
                "--ro-bind",
                args[max(0, args.index(str(root)) - 2) : args.index(str(root))],
                f"the toolchain root {root} is bound by path",
            )

    def test_the_home_is_protected_whole(self) -> None:
        """The HOME is a protected root — not only its sensitive parts.

        A DISCOVERED toolchain root is a directory the reviewer can
        READ.  If the home itself, or any part of it, could be a
        discovered root, the namespace would carry the operator's
        credentials with it.

        The base is judged more leniently on purpose (the plugin puts it
        inside the home), so this rule lives with the DISCOVERY, not
        with the base.
        """
        home = sandbox.operator_home()
        if home is None:
            self.skipTest("the passwd database yielded no home")
        self.assertTrue(
            sandbox._is_protected_path(home),
            "the operator's home itself is not protected",
        )
        self.assertTrue(
            sandbox._is_protected_path(home / "sdk"),
            "a directory inside the operator's home is not protected",
        )
        self.assertTrue(
            sandbox._is_protected_path(home.parent),
            "an ancestor of the operator's home is not protected",
        )
        self.assertTrue(
            sandbox._is_protected_path(Path("/")),
            "the filesystem root is not protected",
        )

    def test_the_toolchain_root_derivation_strips_only_a_bin(self) -> None:
        """A PATH entry cannot collapse onto something larger than itself.

        The defect this closes: the derivation stripped one directory
        level unconditionally, so a PATH entry whose `bin` directory is
        a direct child of the home — `~/bin` — yielded the HOME itself
        as the discovered root.
        """
        self.assertEqual(
            sandbox._toolchain_root_for(Path("/opt/toolchain/bin/go")),
            Path("/opt/toolchain"),
        )
        self.assertEqual(
            sandbox._toolchain_root_for(Path("/opt/toolchain/go")),
            Path("/opt/toolchain"),
            "a program that is not under a `bin` directory lost a level",
        )

    def test_a_toolchain_root_inside_a_protected_root_is_refused(self) -> None:
        """A hostile PATH must not point the sandbox at the operator's keys.

        The discovery is host-side, but the HOST's PATH is not a
        security authority either: a root that resolves inside a
        protected directory is not bound.
        """
        home = sandbox.operator_home()
        if home is None:
            self.skipTest("the passwd database yielded no home")
        hostile = home / ".ssh" / "toolchain"
        self.assertTrue(
            sandbox._is_protected_path(hostile),
            "a path inside the operator's credentials is not protected",
        )

    def test_the_network_and_the_privileges_are_dropped(self) -> None:
        """No network, no shared namespaces, no surviving parent."""
        args = sandbox.bwrap_args(self.tree, self.fds)
        for flag in ("--unshare-all", "--die-with-parent", "--new-session"):
            self.assertIn(flag, args, f"the namespace does not carry {flag}")

    def test_a_nested_user_namespace_is_refused(self) -> None:
        """The sandbox cannot be used to build another one.

        `--disable-userns` refuses `unshare(CLONE_NEWUSER)` inside, and
        `--assert-userns-disabled` makes the sandbox REFUSE TO START if
        that refusal cannot be enforced.  A sandbox that cannot enforce
        it does not start.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        self.assertIn("--disable-userns", args)
        self.assertIn("--assert-userns-disabled", args)

    def test_the_hosts_tmp_is_not_the_sandboxes_tmp(self) -> None:
        """/tmp inside the namespace is a PRIVATE tmpfs.

        The host's /tmp holds other people's scratch, so a review must
        not see it.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        self.assertIn("--tmpfs", args)
        index = args.index("--tmpfs")
        self.assertEqual(args[index + 1], "/tmp")

    def test_the_hosts_etc_is_not_the_sandboxes_etc(self) -> None:
        """`/etc` is SYNTHETIC, and bound by descriptor.

        The host's `/etc` carries the trust store, the private keys and
        the operator's account database.  The sandbox's own `/etc`
        carries a passwd and a group file that name `reviewer`, and the
        `alternatives` symlinks.
        """
        args = sandbox.bwrap_args(self.tree, self.fds)
        # The synthetic `/etc` is one of the READ-ONLY descriptor binds;
        # the others are the host's discovered toolchain roots, so the
        # bind is located by its DESTINATION and not by its position.
        etc_binds = [
            args[i + 1]
            for i, token in enumerate(args)
            if token == "--ro-bind-fd" and i + 2 < len(args) and args[i + 2] == "/etc"
        ]
        self.assertEqual(len(etc_binds), 1, "the synthetic /etc is not bound exactly once")
        self.assertEqual(etc_binds[0], str(self.fds["etc"]))
        for host_only in ("/etc/ssl", "/etc/passwd", "/etc/group"):
            self.assertNotIn(
                host_only,
                sandbox.RUNTIME_DIRS,
                f"{host_only} is bound into the namespace",
            )

    def test_a_live_runtime_root_is_refused(self) -> None:
        """The sandbox refuses to operate inside a live runtime root."""
        home = sandbox.operator_home()
        self.assertIsNotNone(home)
        with self.assertRaises(sandbox.SandboxRefused):
            sandbox.refuse_inside(home / ".mpm", "it is a runtime root")
        with self.assertRaises(sandbox.SandboxRefused):
            sandbox.refuse_inside(Path("/"), "it is the root")
        with self.assertRaises(sandbox.SandboxRefused):
            sandbox.refuse_inside(home, "it is the operator's home")

    def test_a_disposable_base_is_accepted(self) -> None:
        """A disposable base is accepted — the refusal is not a blanket."""
        sandbox.refuse_inside(self.base, "it is a disposable tree")


@unittest.skipUnless(_bwrap_usable(), "bubblewrap cannot build a namespace here")
class ReviewerBwrapLiveTest(unittest.TestCase):
    """What the namespace can and cannot see."""

    def setUp(self) -> None:
        self.base = Path(tempfile.mkdtemp(prefix="mpm-review-sbx-live-"))
        self.addCleanup(shutil.rmtree, self.base, ignore_errors=True)
        self.tree = sandbox.build_tree(self.base, None)
        self.fds = sandbox.open_tree(self.tree)
        self.addCleanup(sandbox.close_tree, self.fds)

    def _run(self, script: str) -> str:
        result = subprocess.run(
            sandbox.bwrap_args(self.tree, self.fds) + ["--", "/bin/sh", "-c", script],
            capture_output=True,
            text=True,
            timeout=120,
            pass_fds=tuple(self.fds.values()),
        )
        self.assertEqual(
            result.returncode,
            0,
            f"the namespace did not start: {result.stderr}",
        )
        return result.stdout

    def test_the_protected_tree_is_invisible(self) -> None:
        """A disposable sentinel tree is ABSENT from inside.

        This is the property a permission matcher cannot provide: an
        unbound path is not refused, it does not exist.
        """
        protected = self.base / "protected"
        protected.mkdir(parents=True, exist_ok=True)
        (protected / "sentinel.txt").write_text("sentinel\n", encoding="utf-8")
        out = self._run(f'if [ -e "{protected}" ]; then echo REACHABLE; else echo INVISIBLE; fi')
        self.assertIn("INVISIBLE", out, f"the protected tree is reachable: {out}")

    def test_the_live_runtime_root_is_invisible(self) -> None:
        """The operator's runtime root is absent from inside.

        The path is only NAMED — it is never opened, never read, and
        never stat'ed from inside the namespace.
        """
        home = sandbox.operator_home()
        self.assertIsNotNone(home)
        root = home / ".mpm"
        out = self._run(f'if [ -e "{root}" ]; then echo REACHABLE; else echo INVISIBLE; fi')
        self.assertIn("INVISIBLE", out, f"the runtime root is reachable: {out}")

    def test_the_operators_home_is_invisible(self) -> None:
        """The operator's home directory is absent from inside."""
        home = sandbox.operator_home()
        self.assertIsNotNone(home)
        out = self._run(f'if [ -d "{home}" ]; then echo REACHABLE; else echo INVISIBLE; fi')
        self.assertIn("INVISIBLE", out, f"the operator's home is reachable: {out}")

    def test_the_trust_store_is_invisible(self) -> None:
        """`/etc/ssl` is absent from inside."""
        out = self._run('if [ -e /etc/ssl ]; then echo REACHABLE; else echo INVISIBLE; fi')
        self.assertIn("INVISIBLE", out, f"the trust store is reachable: {out}")

    def test_the_disposable_tree_is_writable(self) -> None:
        """A review can still do its job inside the namespace."""
        out = self._run(
            f'touch "{sandbox.PREFIX}/workspace/probe" && echo WRITABLE'
        )
        self.assertIn("WRITABLE", out, f"the workspace is not writable: {out}")

    def test_there_is_no_network(self) -> None:
        """The namespace has no network."""
        out = self._run(
            "if getent hosts example.com >/dev/null 2>&1; then echo RESOLVES; "
            "else echo NONE; fi"
        )
        self.assertIn("NONE", out, f"the namespace resolves names: {out}")

    def test_the_host_root_cannot_be_mounted_by_a_command(self) -> None:
        """A command vector cannot carry an option into the namespace.

        Without the `--` separator, `--bind / /` mounts the host's root
        into the namespace: a complete escape.
        """
        args = sandbox.bwrap_args(self.tree, self.fds) + ["--", "/bin/sh", "-c", "echo OK"]
        result = subprocess.run(
            args, capture_output=True, text=True, timeout=120,
            pass_fds=tuple(self.fds.values()),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("OK", result.stdout)


@unittest.skipUnless(_bwrap_usable(), "bubblewrap cannot build a namespace here")
class ReviewerHomeBindTest(unittest.TestCase):
    """A PATH entry under the HOME must not make the home a bind root.

    THE DEFECT THIS PINS
    --------------------
    The toolchain discovery derived a root by stripping one directory
    level from the program's path, so a PATH entry whose `bin` directory
    is a direct child of the home — `~/bin` — yielded the HOME ITSELF.
    The home was then bound READ-ONLY into the namespace, and a
    reviewer could read the operator's credentials with a plain `cat`.

    The reproduction runs inside a private mount namespace that carries
    its OWN /home (a tmpfs), so nothing outside it is touched and the
    operator's own home is never read, written or bound.
    """

    def test_a_home_relative_toolchain_root_is_refused(self) -> None:
        # The reproduction needs an unprivileged USER namespace.  Where
        # the host does not grant one — a CI runner, for instance — the
        # live part SKIPS, and the refusal rules are still pinned by the
        # contract tests above.
        try:
            probe = subprocess.run(
                ["unshare", "-rm", "true"],
                capture_output=True,
                text=True,
                timeout=60,
            )
        except (OSError, subprocess.SubprocessError):
            self.skipTest("unprivileged user namespaces are not available here")
        if probe.returncode != 0:
            self.skipTest(
                "this host does not grant an unprivileged user namespace: "
                f"{probe.stderr.strip()[:200]}"
            )

        script = "\n".join(
            [
                "mount -t tmpfs tmpfs /home 2>/dev/null || exit 9",
                "mkdir -p /home/gate4/bin /home/gate4/.ssh",
                'echo SECRET > /home/gate4/secret.txt',
                'echo KEY > /home/gate4/.ssh/id_rsa',
                "printf '#!/bin/sh\\nexit 0\\n' > /home/gate4/bin/go",
                "chmod 755 /home/gate4/bin/go",
                (
                    "HOME=/home/gate4 PATH=/home/gate4/bin:/usr/bin:/bin GOROOT= "
                    f"python3 -c \"import sys; sys.path.insert(0, {str(SCRIPT.parent)!r}); "
                    "import reviewer_bwrap as rb; print('ROOTS:'); "
                    "[print(r) for r in rb.toolchain_roots()]\""
                ),
            ]
        )
        try:
            result = subprocess.run(
                ["unshare", "-rm", "bash", "-c", script],
                capture_output=True,
                text=True,
                timeout=120,
            )
        except (OSError, subprocess.SubprocessError):
            self.skipTest("unprivileged user namespaces are not available here")
        if result.returncode == 9:
            self.skipTest("a private /home could not be mounted here")
        output = result.stdout + result.stderr
        self.assertNotIn(
            "/home/gate4\n",
            output,
            f"the home was discovered as a toolchain root: {output}",
        )
        self.assertIn(
            "ROOTS:",
            output,
            f"the discovery did not answer: {output}",
        )


class ReviewerWrapperEnvironmentTest(unittest.TestCase):
    """The wrapper passes a SCRUBBED environment to bubblewrap.

    `--clearenv` clears the COMMAND's environment. Bubblewrap's own init
    (`/proc/1` inside the namespace) inherits whatever the wrapper's child
    process receives, so the wrapper must not pass the inference credential
    to it. These tests are deterministic: they do not need a namespace.
    """

    CREDENTIALS = (
        "MINIMAX_API_KEY",
        "DEEPSEEK_API_KEY",
        "MPM_OPENCODE_REVIEWER_KEY",
        "MPM_OPENCODE_SIDECAR_KEY",
        "OPENAI_API_KEY",
        "ANTHROPIC_API_KEY",
    )

    def test_the_scrub_list_names_every_provider_credential(self) -> None:
        # Hardcoded on purpose: deriving the expectation from the same
        # tuple would not fail if an entry were deleted from it.
        for name in self.CREDENTIALS:
            self.assertIn(name, sandbox.SCRUBBED_ENVIRONMENT)

    def test_the_passing_rule_drops_every_credential(self) -> None:
        source = {"PATH": "/usr/bin:/bin", "HOME": "/home/operator"}
        for name in self.CREDENTIALS:
            source[name] = "marker-" + name
        env = sandbox.sandbox_environment(source)
        self.assertEqual(env.get("PATH"), "/usr/bin:/bin")
        self.assertEqual(env.get("HOME"), "/home/operator")
        for name in self.CREDENTIALS:
            self.assertNotIn(name, env)

    def test_scrub_credentials_removes_the_names_from_the_mapping(self) -> None:
        # `sandbox_environment` filters independently, so a test that only
        # calls `execute` would not notice `main` losing this call. This
        # pins the wrapper's own mapping scrub.
        with mock.patch.dict(os.environ, {}, clear=False):
            for name in self.CREDENTIALS:
                os.environ[name] = "marker-" + name
            sandbox.scrub_credentials()
            for name in self.CREDENTIALS:
                self.assertNotIn(name, os.environ)

    def test_execute_gives_bubblewrap_the_scrubbed_environment(self) -> None:
        captured = {}

        def fake_run(args, **kwargs):
            captured.update(kwargs)
            return SimpleNamespace(returncode=0, stdout="", stderr="")

        tree = {"base": Path(tempfile.gettempdir())}
        markers = {name: "marker-" + name for name in self.CREDENTIALS}
        markers["PATH"] = "/usr/bin:/bin"
        with mock.patch.dict(
            os.environ,
            markers,
            clear=False,
        ), mock.patch.object(
            sandbox.os.path, "exists", return_value=True
        ), mock.patch.object(
            sandbox, "open_tree", return_value={}
        ), mock.patch.object(
            sandbox, "close_tree"
        ), mock.patch.object(
            sandbox, "namespace_argv", return_value=["/usr/bin/bwrap", "--", "/bin/true"]
        ), mock.patch.object(
            sandbox.subprocess, "run", side_effect=fake_run
        ):
            result = sandbox.execute(tree, ["/bin/true"])

        self.assertTrue(result["ok"], result)
        self.assertIn("env", captured, "bubblewrap was spawned without an explicit env")
        self.assertEqual(captured["env"].get("PATH"), "/usr/bin:/bin")
        for name in self.CREDENTIALS:
            self.assertNotIn(name, captured["env"])


if __name__ == "__main__":
    unittest.main()
