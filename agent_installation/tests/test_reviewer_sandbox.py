"""
test_reviewer_sandbox.py — the disposable sandbox a review subagent
runs in.

WHY THIS EXISTS
---------------
The reviewer's isolation is enforced at the PERMISSION layer: the
production MPM tools are denied by name, the MPM binaries are refused
at the command position, and a live MPM workspace cannot be opened with
the read tool.

That is a BARRIER, not a jail. The shell runs with the host user's
filesystem, process and network authority, and a permission rule is a
pattern match on a command string — a route the rules do not name is
still a route.

The sandbox is the second layer, and it is the one that does not depend
on the pattern being complete: inside it, `HOME`, `TMPDIR` and
`MPM_WORKSPACE` all point at disposable directories, so a command that
got through lands in the sandbox's own substrate and not in the
operator's runtime root.

WHAT THIS PINS
--------------
  1. The sandbox creates the four directories it promises.
  2. The environment it exports points at those directories — every
     one of them, and at nothing else.
  3. A command run through the sandbox sees that environment.
  4. The sandbox REFUSES to be created inside an MPM runtime root: a
     sandbox inside the thing it isolates is not a sandbox.
  5. The findings scratchpad is a directory on disk, not the
     substrate.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SANDBOX = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "scripts"
    / "reviewer_sandbox.py"
)


def _run(*args: str, base: Path | None = None) -> subprocess.CompletedProcess:
    """Run the sandbox launcher."""
    argv = [sys.executable, str(SANDBOX)]
    if base is not None:
        argv += ["--base", str(base)]
    argv += list(args)
    return subprocess.run(argv, capture_output=True, text=True, check=False)


class ReviewerSandboxTest(unittest.TestCase):
    """The disposable sandbox."""

    def test_the_launcher_exists(self) -> None:
        """A sandbox nobody can run is not a sandbox."""
        self.assertTrue(SANDBOX.exists(), "the reviewer sandbox launcher is missing")

    def test_init_creates_the_four_directories(self) -> None:
        """`init` creates HOME, TMPDIR, the workspace and the findings."""
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "base"
            result = _run("init", base=base)
            self.assertEqual(0, result.returncode, result.stderr)
            root = base / "mpm-review-sandbox"
            for name in ("home", "tmp", "workspace", "findings"):
                self.assertTrue(
                    (root / name).is_dir(),
                    f"the sandbox did not create {name}/ under {root}",
                )

    def test_the_environment_points_at_the_sandbox(self) -> None:
        """Every exported variable names a directory INSIDE the sandbox."""
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "base"
            result = _run("env", base=base)
            self.assertEqual(0, result.returncode, result.stderr)
            root = (base / "mpm-review-sandbox").resolve()
            exported = {}
            for line in result.stdout.splitlines():
                if not line.startswith("export "):
                    continue
                key, _, value = line[len("export "):].partition("=")
                exported[key] = value
            for key in ("HOME", "TMPDIR", "MPM_WORKSPACE", "MPM_REVIEW_FINDINGS"):
                self.assertIn(key, exported, f"the sandbox does not export {key}")
                target = Path(exported[key]).resolve()
                self.assertTrue(
                    str(target).startswith(str(root)),
                    f"{key} points at {target}, which is outside the sandbox {root}",
                )
            # The three that decide where the substrate writes.
            self.assertNotEqual(exported["HOME"], exported["MPM_WORKSPACE"])
            self.assertNotEqual(exported["TMPDIR"], exported["MPM_WORKSPACE"])

    def test_run_executes_the_command_inside_the_sandbox(self) -> None:
        """A command run through the sandbox sees the sandbox's HOME."""
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "base"
            result = _run(
                "run", "--", "sh", "-c", "printf '%s' \"$HOME\"",
                base=base,
            )
            self.assertEqual(0, result.returncode, result.stderr)
            home = Path(result.stdout).resolve()
            root = (base / "mpm-review-sandbox").resolve()
            self.assertTrue(
                str(home).startswith(str(root)),
                f"the command saw HOME={home}, which is outside the sandbox {root}",
            )

    def test_a_base_inside_a_runtime_root_is_refused(self) -> None:
        """A sandbox inside the thing it isolates is not a sandbox."""
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / ".mpm" / "review-sandbox"
            result = _run("init", base=base)
            self.assertNotEqual(
                0,
                result.returncode,
                "the sandbox accepted a base inside an MPM runtime root",
            )
            self.assertIn(
                "runtime root",
                result.stderr,
                f"the refusal does not say why: {result.stderr}",
            )
            self.assertFalse(
                base.exists(),
                "the refused sandbox still created its directory",
            )

    def test_the_findings_directory_is_a_directory_not_the_substrate(self) -> None:
        """The scratchpad is a directory on disk."""
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "base"
            result = _run("env", base=base)
            self.assertEqual(0, result.returncode, result.stderr)
            findings = None
            for line in result.stdout.splitlines():
                if line.startswith("export MPM_REVIEW_FINDINGS="):
                    findings = Path(line.split("=", 1)[1])
            self.assertIsNotNone(findings)
            self.assertTrue(findings.is_dir(), "the findings scratchpad is not a directory")
            # It holds nothing from the substrate: no database file.
            self.assertEqual(
                [],
                sorted(p.name for p in findings.glob("*.db")),
                "the findings scratchpad holds a database file; the scratchpad "
                "is a directory, not a substrate",
            )


if __name__ == "__main__":
    unittest.main()
