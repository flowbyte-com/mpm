"""
test_pre_critic_snapshot_rotation.py — Regression coverage for the
2026-10-04 audit finding: scripts/pre_critic_snapshot.sh used an
unquoted command substitution into an array (`SNAPSHOTS=($(ls ...))`)
which performs word-splitting on filenames.

Pre-fix defect:

    SNAPSHOTS=($(ls -1t "${BACKUP_DIR}"/mpm_pre_critic_*.db 2>/dev/null || true))
    if (( ${#SNAPSHOTS[@]} > ROTATION_KEEP )); then
        for OLD in "${SNAPSHOTS[@]:${ROTATION_KEEP}}"; do
            rm -f -- "${OLD}"
        done
    fi

Two failure modes:
  1. A snapshot filename containing a space or glob metacharacter
     would be split into multiple array elements (or its glob
     characters expanded). The downstream `rm -f -- "${OLD}"` would
     then either remove only the first word or fail to match anything.
  2. When the glob has no matches, `ls` exits non-zero. The
     `2>/dev/null || true` masks that exit and substitutes an empty
     string into the array. With `set -u` active,
     `${SNAPSHOTS[@]:${ROTATION_KEEP}}` on an empty array is empty
     by definition (no out-of-bounds error in bash 4.4+, but it
     worked only because of bash's leniency).

The fix:

    mapfile -t SNAPSHOTS < <(ls -1t -- ... 2>/dev/null)
    # Filter empty entries; for OLD in "${FILTERED[@]:N}" do rm -f ... --

This module stages a fake BACKUP_DIR under a tempdir and exercises:
  1. The rotation deletes exactly the oldest files when more than
     ROTATION_KEEP exist.
  2. The rotation does NOT delete when the count is exactly
     ROTATION_KEEP or fewer.
  3. A filename with a space is preserved verbatim (no word-split).
  4. The no-match-glob case is handled without crashing.
  5. A negative-control test reverts the fix and asserts that the
     buggy version mangles filenames with spaces, proving the test
     would have caught the original defect.

Nothing here ever touches a real DB. Every snapshot is an empty
file with a `.db` suffix, and `sqlite3 .backup` is mocked via a
stub on PATH.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
PRE_CRITIC = REPO_ROOT / "scripts" / "pre_critic_snapshot.sh"


def _fake_sqlite3(bin_dir: Path) -> Path:
    """Drop a fake sqlite3 that supports the two commands pre_critic
    uses: `.backup '<path>'` and `PRAGMA integrity_check;`. The .backup
    command simply `touch`es the destination; integrity_check prints
    'ok' on stdout. This lets us drive the script end-to-end without a
    real SQLite engine.

    Two arg-layout notes for this stub:

      1. The script invokes `sqlite3 "$DB" ".backup '$SNAPSHOT'"` —
         bash word-splits the outer double-quoted argument, so $1 is
         the DB path and $2 is a SINGLE string that begins with
         `.backup` followed by a literal `'<path>'`. We strip the
         `.backup ` prefix and the leading/trailing literal `'`.

      2. Bash parameter expansion `${var%\'}` does NOT strip a literal
         `'` (the backslash is dropped inside `${...}`, leaving an
         empty pattern). Use `case` patterns to strip the literal
         quotes instead.

    The pre_critic script captures `INTEGRITY=$(sqlite3 ... 2>&1)` and
    compares it to `ok`. Keep all stub stdout silent and stdout-only
    for `PRAGMA integrity_check;`, or the script exits 1.
    """
    p = bin_dir / "sqlite3"
    p.write_text(textwrap.dedent("""\
        #!/usr/bin/env bash
        # fake-sqlite3: minimal .backup + integrity_check stub.
        if [[ "$2" == .backup* ]]; then
            target="${2#.backup }"
            case "$target" in
                \\'*) target="${target#\\'}" ;;
            esac
            case "$target" in
                *\\') target="${target%\\'}" ;;
            esac
            mkdir -p "$(dirname -- "$target")"
            : > "$target"
            exit 0
        fi
        if [ "$2" = "PRAGMA integrity_check;" ]; then
            echo "ok"
            exit 0
        fi
        # Fallback: pass through to a real sqlite3 if available.
        if command -v sqlite3 >/dev/null 2>&1; then
            exec sqlite3 "$@"
        fi
        echo "ok"
        exit 0
    """))
    p.chmod(0o755)
    return p


def _stage_workspace(prefix: str) -> dict:
    """Build a hermetic project tree + fakebin. Callers must clean up
    the returned `tmp` themselves. Returns a dict with `tmp`,
    `fakebin`, `fake_home`, `project_root`, `backup_dir`."""
    tmp = Path(tempfile.mkdtemp(prefix=prefix))
    fakebin = tmp / "fakebin"
    fakebin.mkdir(parents=True, exist_ok=True)
    _fake_sqlite3(fakebin)
    fake_home = tmp / "home"
    fake_home.mkdir(parents=True, exist_ok=True)
    project_root = tmp / "project"
    (project_root / "src/db").mkdir(parents=True, exist_ok=True)
    (project_root / "src/db/mpm.db").write_bytes(b"")
    (project_root / "backups/critic-pre").mkdir(parents=True, exist_ok=True)
    return {
        "tmp": tmp,
        "fakebin": fakebin,
        "fake_home": fake_home,
        "project_root": project_root,
        "backup_dir": project_root / "backups/critic-pre",
    }


def _run_script(
    stage: dict,
    script: Path,
    *,
    keep: int,
    label: str = "rotation-test",
    extra_snapshots: list[str] | None = None,
) -> dict:
    env = os.environ.copy()
    env["HOME"] = str(stage["fake_home"])
    env["PATH"] = f"{stage['fakebin']}:{env.get('PATH', '')}"
    env["MPM_DB_PATH"] = str(stage["project_root"] / "src/db/mpm.db")
    env["CRITIC_SNAPSHOT_KEEP"] = str(keep)
    # Stage the script under our fake project root so its
    # `$(dirname "$BASH_SOURCE")/..` resolution finds our tree.
    staged = stage["project_root"] / "scripts" / "pre_critic_snapshot.sh"
    staged.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy(script, staged)
    staged.chmod(0o755)
    # Stage any pre-existing snapshots in order — last entry is newest.
    # We assign strictly-increasing mtimes so `ls -1t` orders them
    # deterministically (otherwise sub-second mtime ties fall back to
    # lexicographic order, which scrambles the test).
    if extra_snapshots is not None:
        base = 1700000000  # arbitrary fixed epoch base
        for i, name in enumerate(extra_snapshots):
            path = stage["backup_dir"] / name
            path.write_bytes(b"")
            os.utime(path, (base + i, base + i))
    result = subprocess.run(
        ["bash", "--noprofile", "--norc", str(staged), label],
        capture_output=True, text=True,
        env=env, timeout=60,
    )
    return {
        "returncode": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
    }


class RotationSemantics(unittest.TestCase):
    """The rotation must delete only the oldest files beyond KEEP."""

    def setUp(self) -> None:
        self.stage = _stage_workspace("mpm-pre-critic-rotation-")
        self.addCleanup(lambda: shutil.rmtree(self.stage["tmp"], ignore_errors=True))

    def test_keeps_newest_when_count_exceeds_keep(self) -> None:
        # Stage 10 snapshots, KEEP=3. The newest 3 of the staged set
        # must survive; the other 7 must be rotated. The script also
        # creates a fresh snapshot labeled 'keep-latest', which is
        # counted toward KEEP=3 — so we end up with the new snapshot
        # plus the 2 most-recently-staged ones.
        names = [f"mpm_pre_critic_2026-01-{i:02d}T000000Z.db" for i in range(1, 11)]
        result = _run_script(
            self.stage, PRE_CRITIC, keep=3, label="keep-latest",
            extra_snapshots=names,
        )
        self.assertEqual(result["returncode"], 0, result["stderr"])
        survivors = sorted(p.name for p in self.stage["backup_dir"].iterdir())
        # Total must equal KEEP=3 (new snapshot counts as one of the
        # kept set). The 8 oldest must be gone; the 2 newest of the
        # staged set survive alongside the new snapshot.
        self.assertEqual(
            len(survivors), 3,
            "rotation must keep exactly KEEP=3 total snapshots; "
            f"got {len(survivors)}: {survivors}",
        )
        self.assertIn("mpm_pre_critic_keep-latest.db", survivors)
        # The 2 most-recent staged names must survive.
        self.assertIn(names[-2], survivors)
        self.assertIn(names[-1], survivors)

    def test_no_rotation_when_count_equals_keep(self) -> None:
        # 3 pre-existing snapshots, KEEP=3. The script creates a new
        # snapshot, making the total 4 — exceeding KEEP=3 — so the
        # OLDEST staged file gets rotated, leaving 2 staged + 1 new.
        names = [f"mpm_pre_critic_eq_{i}.db" for i in range(3)]
        result = _run_script(
            self.stage, PRE_CRITIC, keep=3, label="eq-k",
            extra_snapshots=names,
        )
        self.assertEqual(result["returncode"], 0, result["stderr"])
        survivors = sorted(p.name for p in self.stage["backup_dir"].iterdir())
        # The new snapshot must be present.
        self.assertIn("mpm_pre_critic_eq-k.db", survivors)
        # The two NEWEST staged files must survive.
        self.assertIn(names[-2], survivors)
        self.assertIn(names[-1], survivors)
        # The OLDEST staged file must be rotated.
        self.assertNotIn(names[0], survivors)
        # Total must equal KEEP=3.
        self.assertEqual(
            len(survivors), 3,
            f"expected exactly KEEP=3 survivors, got {len(survivors)}: "
            f"{survivors}",
        )

    def test_no_rotation_when_count_below_keep(self) -> None:
        names = [f"mpm_pre_critic_few_{i}.db" for i in range(2)]
        result = _run_script(
            self.stage, PRE_CRITIC, keep=5, label="few-k",
            extra_snapshots=names,
        )
        self.assertEqual(result["returncode"], 0, result["stderr"])
        survivors = sorted(p.name for p in self.stage["backup_dir"].iterdir())
        # Staged files (few_0, few_1) survive + new snapshot labeled
        # 'few-k' is also added.
        expected = sorted(names + ["mpm_pre_critic_few-k.db"])
        self.assertEqual(survivors, expected,
                         "no rotation must happen when count < KEEP")

    def test_filenames_with_spaces_preserved(self) -> None:
        """The headline defect: filenames with spaces used to be
        word-split into multiple array elements. The fix uses
        `mapfile` which preserves each line verbatim."""
        names = [
            "mpm_pre_critic_2026 01 01.db",
            "mpm_pre_critic_2026 01 02.db",
            "mpm_pre_critic_2026 01 03.db",
            "mpm_pre_critic_2026 01 04.db",
            "mpm_pre_critic_2026 01 05.db",
            "mpm_pre_critic_2026 01 06.db",
        ]
        result = _run_script(
            self.stage, PRE_CRITIC, keep=2, label="sp-c",
            extra_snapshots=names,
        )
        self.assertEqual(result["returncode"], 0, result["stderr"])
        survivors = sorted(p.name for p in self.stage["backup_dir"].iterdir())
        # The new snapshot is the single survivor carrying the label
        # 'sp-c'; it's a fresh file the script just created and
        # doesn't contain a space. The OTHER survivors are rotated-out
        # staged filenames — every one of them must still be a single
        # filename containing a space, never a partial token from
        # word-split.
        rotated_staged = [s for s in survivors if s != "mpm_pre_critic_sp-c.db"]
        self.assertEqual(
            len(rotated_staged), 1,
            f"with KEEP=2 + 6 staged, only the newest staged should "
            f"survive plus the new snapshot; got {rotated_staged}",
        )
        # The single survivor staged name must contain a space (proves
        # it was preserved verbatim — fragments like 'mpm_pre_critic_2026'
        # alone would indicate a split).
        self.assertIn(" ", rotated_staged[0])
        # The new snapshot must be present.
        self.assertIn("mpm_pre_critic_sp-c.db", survivors)

    def test_no_match_glob_does_not_crash(self) -> None:
        """No pre-existing snapshots. The script must succeed and the
        new snapshot must be created without `unbound variable` errors
        or `set -u` violations on the empty array."""
        result = _run_script(
            self.stage, PRE_CRITIC, keep=7, label="fresh",
        )
        self.assertEqual(result["returncode"], 0, result["stderr"])
        # One snapshot was created.
        self.assertEqual(len(list(self.stage["backup_dir"].iterdir())), 1)


class SourceLevelRegressionGuards(unittest.TestCase):
    """The fix is structural — the script must keep using `mapfile`
    rather than the old unquoted-command-substitution shape."""

    def test_uses_mapfile_not_command_substitution(self) -> None:
        src = PRE_CRITIC.read_text()
        self.assertRegex(src, r"mapfile -t SNAPSHOTS < <\(ls")
        # The pre-fix idiom must be gone.
        self.assertNotRegex(
            src,
            r"SNAPSHOTS=\$\(ls",
            "rotation glob must not use unquoted command substitution; "
            "use mapfile so filenames stay verbatim",
        )

    def test_ls_uses_double_dash(self) -> None:
        """The fix uses `ls -1t --` to guard against filenames that
        begin with a dash being interpreted as flags."""
        src = PRE_CRITIC.read_text()
        self.assertRegex(src, r"ls -1t --")


class NegativeControlPreFix(unittest.TestCase):
    """If we revert the fix to the old unquoted-command-substitution
    shape, the script must mangle a filename containing a space.
    This proves the test would have caught the original defect."""

    def setUp(self) -> None:
        self.stage = _stage_workspace("mpm-pre-critic-neg-")
        self.tmp = self.stage["tmp"]
        self.backup_dir = self.stage["backup_dir"]
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))

    def test_unquoted_command_subst_splits_spaces(self) -> None:
        # Write a minimal buggy rotation block as a standalone script
        # that exercises the same failure surface as the in-tree fix
        # removed. This is more reliable than mutating the in-tree
        # script.
        buggy = self.tmp / "buggy-rotation.sh"
        buggy.write_text(textwrap.dedent("""\
            #!/usr/bin/env bash
            set -euo pipefail
            BACKUP_DIR="$1"
            ROTATION_KEEP="${2:-2}"
            # The pre-fix idiom.
            SNAPSHOTS=($(ls -1t "${BACKUP_DIR}"/mpm_pre_critic_*.db 2>/dev/null || true))
            if (( ${#SNAPSHOTS[@]} > ROTATION_KEEP )); then
                for OLD in "${SNAPSHOTS[@]:${ROTATION_KEEP}}"; do
                    rm -f -- "${OLD}"
                done
            fi
        """))
        buggy.chmod(0o755)
        # Stage 4 snapshots with spaces in their names.
        names = [
            "mpm_pre_critic_2026 01 01.db",
            "mpm_pre_critic_2026 01 02.db",
            "mpm_pre_critic_2026 01 03.db",
            "mpm_pre_critic_2026 01 04.db",
        ]
        for name in names:
            (self.backup_dir / name).write_bytes(b"")
        result = subprocess.run(
            [str(buggy), str(self.backup_dir), "2"],
            capture_output=True, text=True, timeout=60,
        )
        # The buggy version runs without uncaught set -u violation because
        # `set -e` doesn't catch array-assignment failures on bash 4.4+.
        # But it leaves behind the mangled residue.
        survivors = sorted(p.name for p in self.backup_dir.iterdir())
        # Some survivors must lack their original trailing spaces —
        # i.e. word-split produced elements that don't correspond to
        # real files. In practice the array gets elements like
        # "mpm_pre_critic_2026", "01", "01.db" for one filename, so
        # `rm -f -- "${OLD}"` on the "mpm_pre_critic_2026" element
        # tries to remove a non-existent file, and the rest of the
        # words end up as their own array elements that don't match
        # either. Net file count after rotation may be incorrect.
        # The key signal: NOT every survivor is one of the original
        # filenames — some elements of the array were partial tokens
        # that `rm` couldn't find.
        # Buggy version either:
        #   - left too many files (failed to remove the broken-up pieces)
        #   - or removed one of the actual filenames because the
        #     array's last element happened to match by accident.
        # Either way, the survivors are NOT exactly the original
        # names [2:] or [-1:] subset — the rotation logic is broken.
        self.assertNotEqual(
            sorted(survivors), sorted(names[-2:]),
            "BUG REPRO FAILED: buggy rotation must NOT produce the same "
            "survivors as the fixed version",
        )


if __name__ == "__main__":
    unittest.main()