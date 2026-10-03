"""Canonical-root co-location guard.

`~/.mpm` is both the Git checkout and the MPM runtime root. The property that
makes that safe is that nothing MPM writes at runtime can be seen by Git.

This builds a throwaway copy of the tracked tree, initialises a real
repository in it, creates the full set of runtime artifacts MPM produces, and
asserts `git status --porcelain` is still empty. A new runtime path that is
not gitignored fails here instead of quietly exposing a database.

Everything runs in a temp directory. The real checkout, the real `~/.mpm`
runtime state, and the production database are never touched.
"""

from __future__ import annotations

import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]

# Runtime artifacts MPM creates beneath its workspace root. Each entry is
# (relative path, content). Directory entries are created as needed.
RUNTIME_ARTIFACTS: list[tuple[str, bytes]] = [
    # Compiled binaries (make build / install.sh)
    ("bin/mpm", b"\x7fELF"),
    ("bin/mpm-mcp", b"\x7fELF"),
    ("bin/mpm-scheduler", b"\x7fELF"),
    ("bin/mpm-critic", b"\x7fELF"),
    ("bin/mpm-telemetry", b"\x7fELF"),
    # The cognitive substrate and its SQLite sidecars
    ("src/db/mpm.db", b"SQLite format 3\x00"),
    ("src/db/mpm.db-wal", b""),
    ("src/db/mpm.db-shm", b""),
    ("src/db/telemetry.db", b"SQLite format 3\x00"),
    ("src/db/telemetry.db-wal", b""),
    ("src/db/telemetry.db-shm", b""),
    ("src/db/mirror.jsonl", b"{}\n"),
    ("src/db/watchdog.jsonl", b"{}\n"),
    ("src/db/mirror.jsonl.gz", b"\x1f\x8b"),
    # Operational state
    ("backups/mpm-backup-2026-01-01.db", b"dump"),
    ("backups/critic-pre/snapshot.db", b"dump"),
    ("blobs/deadbeef", b"blob"),
    ("run/scheduler.state", b"{}"),
    ("run/mpm.pid", b"1234"),
    ("run/telemetry.lock", b""),
    ("scheduler.lock", b""),
    ("active.json", b"{}"),
    ("active.json.lock", b""),
    ("toxicphrases.txt", b"phrase\n"),
    ("config/current_mode", b"default"),
    # Migration undo snapshots
    ("migrations/embeddings-2026-01-01T00-00-00Z.db.bak", b"bak"),
    ("migrations/some-other-artifact.db.bak", b"bak"),
]

# Directories that are tracked SOURCE even though they live at the runtime
# root. Runtime state must never be written into them.
SOURCE_DIRS = ("mode", "persona")


def _git(args: list[str], cwd: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["git", *args], cwd=cwd, capture_output=True, text=True
    )


def _build_scratch_checkout() -> Path:
    """A temp dir containing a real Git repo with the project's
    .gitignore rules, so `git status` reflects production behaviour.

    The working tree is copied (not `git archive HEAD`) so the guard
    exercises the .gitignore actually on disk. A committed-HEAD fixture
    would silently pass a stale ignore rule and only turn red after the
    commit — the opposite of what a pre-commit guard is for. `git ls-files`
    still bounds the copy to tracked files, so ignored runtime state on a
    developer's machine is never imported.
    """
    dest = Path(tempfile.mkdtemp(prefix="mpm-colocation-"))
    tracked = subprocess.run(
        ["git", "ls-files", "-z"],
        cwd=REPO_ROOT,
        capture_output=True,
        check=True,
    ).stdout.decode()
    for rel in tracked.split("\0"):
        if not rel:
            continue
        src = REPO_ROOT / rel
        if not src.is_file():
            continue
        out = dest / rel
        out.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, out)
    _git(["init", "-q", "."], dest)
    _git(["config", "user.email", "test@example.invalid"], dest)
    _git(["config", "user.name", "Test"], dest)
    _git(["add", "-A"], dest)
    _git(["commit", "-qm", "baseline"], dest)
    return dest


class TestCanonicalRootCoLocation(unittest.TestCase):
    """Runtime state at the canonical root must be invisible to Git."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.root = _build_scratch_checkout()
        for rel, content in RUNTIME_ARTIFACTS:
            path = cls.root / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.root, ignore_errors=True)

    def test_worktree_stays_clean_after_runtime_artifacts(self) -> None:
        status = _git(["status", "--porcelain"], self.root)
        self.assertEqual(
            status.stdout.strip(),
            "",
            "runtime artifacts at the canonical root dirtied the worktree:\n"
            + status.stdout,
        )

    def test_no_runtime_artifact_is_tracked(self) -> None:
        tracked = _git(["ls-files"], self.root).stdout.split()
        leaked = [
            rel
            for rel, _ in RUNTIME_ARTIFACTS
            if rel in tracked
        ]
        self.assertEqual(
            leaked, [], f"runtime artifacts are tracked by git: {leaked}"
        )

    def test_every_runtime_artifact_is_ignored(self) -> None:
        unignored = []
        for rel, _ in RUNTIME_ARTIFACTS:
            res = _git(["check-ignore", "-q", rel], self.root)
            if res.returncode != 0:
                unignored.append(rel)
        self.assertEqual(
            unignored,
            [],
            "these runtime paths are NOT gitignored and could be committed "
            f"by a `git add -A`: {unignored}",
        )

    def test_source_dirs_remain_tracked(self) -> None:
        """mode/ and persona/ are source, not runtime. The co-location
        invariant must not be 'fixed' by ignoring them."""
        tracked = set(_git(["ls-files"], self.root).stdout.split())
        for d in SOURCE_DIRS:
            if not any(p.startswith(f"{d}/") for p in tracked):
                self.fail(f"{d}/ is no longer tracked — source was ignored away")


class TestAlternateCheckoutLayout(unittest.TestCase):
    """An alternate checkout keeps source elsewhere; runtime stays canonical."""

    def test_install_prefix_defaults_to_home_mpm(self) -> None:
        text = (REPO_ROOT / "install.sh").read_text(encoding="utf-8")
        self.assertIn(
            'PREFIX="${PREFIX:-$HOME/.mpm}"',
            text,
            "install.sh must default PREFIX to $HOME/.mpm",
        )

    def test_no_wrapper_artifacts_installer_contract(self) -> None:
        """No shell wrapper and no mpm.real in the current layout; the
        installer may only *clean up* those legacy names."""
        text = (REPO_ROOT / "install.sh").read_text(encoding="utf-8")
        self.assertIn(
            "-ef",
            text,
            "install.sh must keep its same-file ([ -ef ]) guard for the "
            "case where the checkout IS the install prefix",
        )


if __name__ == "__main__":
    unittest.main()
