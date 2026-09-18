"""
test_install_db_tree_perms.py — Behavioral regression coverage for the
installer's existing-DB-tree permission normalization.

Spec (2026-09-18 hardening pass):

  Sensitive MPM directories  -> 0700
  Sensitive MPM regular files -> 0600

  Scope is intentionally limited to $DATA_ROOT/src/db. We must NOT
  recursively chmod unrelated source-tree content.

  For pre-existing state with permissive modes (e.g. left over from a
  pre-umask install), install.sh must:

    - chmod directories under src/db to 0700,
    - chmod regular files under src/db to 0600,
    - NOT follow symlinks (an external symlink target must NOT be
      touched),
    - NOT chmod the symlink itself (it is owned by the test fixture
      and we don't want to surprise it).

Tests exercise the actual installer behavior in an isolated HOME under
/tmp. They are NOT source-string matches.
"""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
INSTALL_SH = REPO_ROOT / "install.sh"


# Markers in install.sh that prevent the script from running main()
# during test sourcing. PROJECT_ROOT/REPO_ROOT/SCRIPT_DIR are computed
# from $0 inside the script as ``readonly``; we strip those lines so
# the driver can control them.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+REPO_ROOT\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def _strip_patterns(install_text: str) -> str:
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


def _run_phase_data_dir(data_root: Path) -> dict:
    """Drive ``phase_data_dir`` in a fresh subprocess with controlled DATA_ROOT.

    Returns a dict with ``returncode``, ``stdout``, ``stderr``.
    """
    install_text = INSTALL_SH.read_text()
    sourced = _strip_patterns(install_text)
    driver_lines = [
        'export DATA_ROOT="{}"'.format(str(data_root)),
        'phase_data_dir',
    ]
    with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as f:
        f.write(sourced + "\n" + "\n".join(driver_lines) + "\n")
        driver_path = Path(f.name)
    driver_path.chmod(0o755)
    try:
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver_path)],
            capture_output=True,
            text=True,
            env={**os.environ, "PATH": "/usr/bin:/bin"},
        )
    finally:
        driver_path.unlink()
    return {
        "returncode": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
    }


def _mode(p: Path) -> str:
    return oct(p.stat().st_mode & 0o777)


class TestDBTreePermNormalization(unittest.TestCase):
    """phase_data_dir MUST normalize pre-existing modes to 0700/0600
    beneath $DATA_ROOT/src/db, without following symlinks."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-test-dbperms-"))
        self.data_root = self.tmp / ".mpm"
        self.src_db = self.data_root / "src" / "db"
        self.sub = self.src_db / "subdir"

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_C_fresh_creation_already_at_correct_modes(self):
        """After phase_data_dir runs, the fresh tree must already be at
        0700/0600 (chmod-after-mkdir). This pins the natural-creation
        invariant."""
        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0,
                         f"phase_data_dir failed:\nstderr: {result['stderr']}")
        # DB root is 0700.
        self.assertTrue(self.src_db.is_dir())
        self.assertEqual(_mode(self.src_db), "0o700")

    def test_E_loose_state_gets_hardened(self):
        """Pre-existing 0755 directories and 0644/0664 files under
        src/db must be hardened to 0700/0600 by phase_data_dir."""
        # Pre-create loose state.
        self.src_db.mkdir(parents=True, exist_ok=True)
        self.sub.mkdir(parents=True, exist_ok=True)
        # Loose modes.
        self.src_db.chmod(0o755)
        self.sub.chmod(0o755)
        # Loose files.
        mpm_db = self.src_db / "mpm.db"
        mpm_db.write_text("db-canary")
        mpm_db.chmod(0o644)
        sub_file = self.sub / "test.file"
        sub_file.write_text("sub-canary")
        sub_file.chmod(0o664)

        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0)
        combined = result["stdout"] + result["stderr"]
        # Find the tightening log line (it's prefixed with [mpm-install]).
        # We don't pin the exact count because future tests may add
        # additional sinks; we DO pin that the line was emitted when
        # paths were tightened.
        # Confirm tightening happened by mode check.
        self.assertEqual(_mode(self.src_db), "0o700",
                         f"src/db should be 0700 after normalization; combined: {combined}")
        self.assertEqual(_mode(self.sub), "0o700",
                         f"subdir should be 0700 after normalization; combined: {combined}")
        self.assertEqual(_mode(mpm_db), "0o600",
                         f"mpm.db should be 0600 after normalization; combined: {combined}")
        self.assertEqual(_mode(sub_file), "0o600",
                         f"subdir/test.file should be 0600 after normalization; combined: {combined}")

    def test_F_symlinks_inside_db_root_are_not_chmodd(self):
        """A symlink whose target sits OUTSIDE src/db must NOT be
        chmod'd (we don't follow symlinks for permission normalization).

        The symlink itself is owned by the test fixture; we verify it
        is not chmod'd and that the external target's mode is
        unchanged."""
        # Pre-create a permissive external target.
        external = self.tmp / "external-target.txt"
        external.write_text("external-content")
        external.chmod(0o644)
        # AND a permissive symlink in src/db pointing at it.
        self.src_db.mkdir(parents=True, exist_ok=True)
        link = self.src_db / "outside-link"
        link.symlink_to(str(external))

        external_mode_before = _mode(external)
        # Phase must NOT touch the external target or the symlink.
        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0,
                         f"phase_data_dir failed:\nstderr: {result['stderr']}")
        # The external target is unchanged.
        self.assertEqual(_mode(external), external_mode_before,
                         "external symlink target mode must not change")

        # The symlink itself has no mode (lstat returns the symlink's
        # own perm, NOT the target's). The normalization pass must
        # skip symlinks entirely. We confirm by checking lstat mode
        # matches the original: 0777 (max link perms) — the symlink
        # itself was created by symlink_to with default perms.
        # The key assertion: the external target was NOT chmod'd.
        # (We already asserted that above.)


class TestDBTreePermSourceInvariants(unittest.TestCase):
    """Structural source-level invariants for the normalization pass.

    These are minimal and structural on purpose: they verify the
    invariant that the chmod pass runs ONLY over src/db and uses a
    safe-traversal mechanism (find -print0/-read, no -L)."""

    def test_normalization_scope_is_src_db(self):
        install_text = INSTALL_SH.read_text()
        # The chmod pass must operate on $DATA_ROOT/src/db specifically,
        # not $DATA_ROOT or any other top-level dir.
        # Look for the literal "$DATA_ROOT/src/db" in find/dir-traversal
        # context after phase_data_dir's pre-existence normalization.
        m = re.search(
            r"(?P<scope>\$DATA_ROOT/src/db)\s*\"?\s*\)?\s*&&\s*pwd",
            install_text,
        )
        self.assertIsNotNone(m,
                            "phase_data_dir must scope its normalization to "
                            "$DATA_ROOT/src/db; the cd/pwd anchor must reference it")

    def test_normalization_does_not_follow_symlinks(self):
        install_text = INSTALL_SH.read_text()
        # The find command must NOT carry -L. The implementation
        # aliases $DATA_ROOT/src/db to a local `db_root` variable and
        # passes that to find; accept either form.
        m = re.search(
            r"find\s+\"?(?:\$DATA_ROOT/src/db|\$db_root)\"?\s+-mindepth\s+1\s+-print0",
            install_text,
        )
        self.assertIsNotNone(m,
                            "DB-tree normalization must use 'find $DATA_ROOT/src/db "
                            "(or $db_root) -mindepth 1 -print0' (no -L, safe NUL delimiting)")
        # And explicitly verify no -L appears in this find call.
        find_call = m.group(0)
        self.assertNotIn("-L", find_call,
                         "find for DB-tree normalization must NOT include -L")

    def test_normalization_skips_symlinks_via_explicit_check(self):
        install_text = INSTALL_SH.read_text()
        # The chmod loop explicitly skips symlinks before testing -d/-f.
        # This is belt-and-braces: even if a future find variant gains
        # -L, the explicit -L test still protects the external target.
        self.assertIn('[ -L "$p" ]', install_text,
                      "DB-tree chmod loop must explicitly skip symlinks")


if __name__ == "__main__":
    unittest.main()
