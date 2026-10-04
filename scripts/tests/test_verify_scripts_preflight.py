"""
test_verify_scripts_preflight.py — Regression coverage for the
2026-10-04 audit finding: scripts/verify-m3-audit-fixes.sh and
scripts/verify-wishlist-fixes.sh silently pass on a missing/wrong-DB
because their default `MPM = ./bin/mpm` and `DB = ./src/db/mpm.db`
are CWD-relative.

Pre-fix defects:

  1. Run from anywhere but the repo root → MPM defaults to
     `./bin/mpm` (does not exist relative to CWD). Each `$MPM call ...`
     writes "no such file or directory" to stderr, which the script
     redirects to /dev/null in `call_only`. The assertions that depend
     on `$(... | python3 -c '...')` then resolve to empty strings,
     which fall through to `assert_fail`. Net effect: when mpm is
     missing, the script produces all-FAIL instead of all-PASS — the
     failure was visible, but for the wrong reason (missing binary,
     not the production semantics).

  2. When the mpm binary IS reachable but the DB is missing
     (`./src/db/mpm.db` doesn't exist from CWD), sqlite3 silently
     returns empty for every query. The result is `[[ -z "$ID" ]]`
     triggering assert_fail in many places, but the canary-style
     assertions in W-011 ("rm without --force refuses") can pass
     vacuously because `$("$MPM" rm "$PRIME_ID" 2>&1 | grep -c ...)`
     evaluates `$PRIME_ID` as empty when the prime-directive lookup
     failed, and an empty id passed to `mpm rm` exits non-zero with
     a "no such memory" error that grep still matches as a refused
     delete. So a mis-run on a missing DB can produce a vacuous
     green pass for some assertions.

The fix:

  Both scripts now resolve MPM and DB from BASH_SOURCE[0] (this
  script's own location), so they always target the right binary/DB
  regardless of CWD. They also pre-flight check both paths and refuse
  to run if either is missing, returning exit code 2 instead of
  silently producing misleading results.

This module asserts the preflight behavior on a hermetic copy of each
verify script under a tempdir with no MPM binary, no DB, and a
deliberately bogus MPM= / DB= override. Nothing here ever invokes the
real mpm binary or touches a real DB.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
VERIFY_M3 = REPO_ROOT / "scripts" / "verify-m3-audit-fixes.sh"
VERIFY_WISHLIST = REPO_ROOT / "scripts" / "verify-wishlist-fixes.sh"


class _VerifyPreflightDriver(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-verify-preflight-"))
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))

    def _run(self, script: Path, *, mpm: str | None, db: str | None) -> dict:
        env = os.environ.copy()
        # Force a deterministic PATH that does NOT contain mpm/sqlite3
        # binaries from the host, so the only resolution paths are
        # via the script's own defaults or the MPM=/DB= overrides.
        env["PATH"] = "/usr/bin:/bin"
        # Unset any inherited MPM / DB overrides the harness user might
        # have set; the test always supplies them explicitly.
        env.pop("MPM", None)
        env.pop("DB", None)
        if mpm is not None:
            env["MPM"] = mpm
        if db is not None:
            env["DB"] = db
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(script), "--help"],
            capture_output=True,
            text=True,
            env=env,
            timeout=60,
        )
        return {
            "returncode": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
        }

    def cleanup(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)


class VerifyM3Preflight(unittest.TestCase):
    """verify-m3-audit-fixes.sh preflight must catch missing MPM/DB."""

    def _run(self, *, mpm: str | None, db: str | None) -> dict:
        d = _VerifyPreflightDriver()
        d.setUp()
        self.addCleanup(d._cleanup)
        return d._run(VERIFY_M3, mpm=mpm, db=db)

    def test_missing_mpm_exits_2(self) -> None:
        # No MPM=, no DB=, no MPM binary on the (stripped) PATH.
        # The preflight must catch the missing MPM and exit 2 — the
        # pre-fix behavior was to silently fall through to all-FAIL
        # or all-PASS depending on which assertion fired first.
        d = _VerifyPreflightDriver()
        d.setUp()
        self.addCleanup(d.cleanup)
        env = os.environ.copy()
        env["PATH"] = "/usr/bin:/bin"
        # Force MPM to a path that does not exist so the BASH_SOURCE
        # default does not accidentally resolve to a real binary.
        env["MPM"] = str(d.tmp / "no-such-mpm")
        env.pop("DB", None)
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(VERIFY_M3)],
            capture_output=True, text=True, env=env, timeout=60,
        )
        self.assertEqual(
            result.returncode, 2,
            "missing MPM binary must trigger exit 2; "
            f"got {result.returncode}\nstderr:\n{result.stderr}",
        )
        self.assertIn("MPM binary not found", result.stderr)

    def test_missing_db_exits_2(self) -> None:
        d = _VerifyPreflightDriver()
        d.setUp()
        self.addCleanup(d.cleanup)
        # Create a fake mpm that exists + is executable, but no DB.
        fakebin = d.tmp / "bin"
        fakebin.mkdir(parents=True, exist_ok=True)
        fake_mpm = fakebin / "mpm"
        fake_mpm.write_text("#!/bin/sh\necho ok\n")
        fake_mpm.chmod(0o755)
        env = os.environ.copy()
        env["PATH"] = f"{fakebin}:/usr/bin:/bin"
        env["MPM"] = str(fake_mpm)
        env.pop("DB", None)
        # Override DB to a path that does not exist.
        env["DB"] = str(d.tmp / "no-such.db")
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(VERIFY_M3)],
            capture_output=True, text=True, env=env, timeout=60,
        )
        self.assertEqual(
            result.returncode, 2,
            "missing DB must trigger exit 2; "
            f"got {result.returncode}\nstderr:\n{result.stderr}",
        )
        self.assertIn("MPM database not found", result.stderr)

    def test_does_not_use_cwd_relative_defaults(self) -> None:
        """Source-level guard against regression: the default MPM and
        DB must be derived from BASH_SOURCE[0], not from CWD."""
        src = VERIFY_M3.read_text()
        # The old defaults were exactly "./bin/mpm" and "./src/db/mpm.db".
        self.assertNotIn('"./bin/mpm"', src)
        self.assertNotIn('"./src/db/mpm.db"', src)
        # The new defaults must reference BASH_SOURCE-derived paths.
        self.assertIn('BASH_SOURCE[0]', src)
        self.assertIn('REPO_ROOT', src)


class VerifyWishlistPreflight(unittest.TestCase):
    """verify-wishlist-fixes.sh preflight must catch missing MPM/DB."""

    def test_missing_mpm_exits_2(self) -> None:
        d = _VerifyPreflightDriver()
        d.setUp()
        self.addCleanup(d.cleanup)
        env = os.environ.copy()
        env["PATH"] = "/usr/bin:/bin"
        env["MPM"] = str(d.tmp / "no-such-mpm")
        env.pop("DB", None)
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(VERIFY_WISHLIST)],
            capture_output=True, text=True, env=env, timeout=60,
        )
        self.assertEqual(
            result.returncode, 2,
            "missing MPM binary must trigger exit 2; "
            f"got {result.returncode}\nstderr:\n{result.stderr}",
        )
        self.assertIn("MPM binary not found", result.stderr)

    def test_missing_db_exits_2(self) -> None:
        d = _VerifyPreflightDriver()
        d.setUp()
        self.addCleanup(d.cleanup)
        fakebin = d.tmp / "bin"
        fakebin.mkdir(parents=True, exist_ok=True)
        fake_mpm = fakebin / "mpm"
        fake_mpm.write_text("#!/bin/sh\necho ok\n")
        fake_mpm.chmod(0o755)
        env = os.environ.copy()
        env["PATH"] = f"{fakebin}:/usr/bin:/bin"
        env["MPM"] = str(fake_mpm)
        env.pop("DB", None)
        env["DB"] = str(d.tmp / "no-such.db")
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(VERIFY_WISHLIST)],
            capture_output=True, text=True, env=env, timeout=60,
        )
        self.assertEqual(
            result.returncode, 2,
            "missing DB must trigger exit 2; "
            f"got {result.returncode}\nstderr:\n{result.stderr}",
        )
        self.assertIn("MPM database not found", result.stderr)

    def test_does_not_use_cwd_relative_defaults(self) -> None:
        src = VERIFY_WISHLIST.read_text()
        self.assertNotIn('"./bin/mpm"', src)
        self.assertNotIn('"./src/db/mpm.db"', src)
        self.assertIn('BASH_SOURCE[0]', src)
        self.assertIn('REPO_ROOT', src)


class VerifyScriptsShareContract(unittest.TestCase):
    """Both verify scripts must apply the same preflight rules so that
    running either produces consistent semantics across the suite."""

    def test_preflight_intent_is_documented_in_both(self) -> None:
        for path in (VERIFY_M3, VERIFY_WISHLIST):
            src = path.read_text()
            self.assertIn(
                "see #T-2026-10-04 verify-DB-targeting audit",
                src,
                f"{path.name} missing the audit-reference header "
                "that explains why preflight exists",
            )
            # Both must refuse with exit 2 on missing inputs.
            self.assertRegex(src, r'exit 2')


if __name__ == "__main__":
    unittest.main()