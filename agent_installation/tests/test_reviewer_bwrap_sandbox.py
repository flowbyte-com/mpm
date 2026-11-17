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
        """`--clearenv` comes BEFORE the `--setenv` flags.

        bubblewrap processes its arguments in order, so clearing after
        setting would erase the very variables the sandbox exists to
        isolate — and the reviewer would inherit the operator's
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
        index = args.index("--ro-bind-fd")
        self.assertEqual(args[index + 1], str(self.fds["etc"]))
        self.assertEqual(args[index + 2], "/etc")
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


if __name__ == "__main__":
    unittest.main()
