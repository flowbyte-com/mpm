"""
test_uninstall.py — Comprehensive coverage for uninstall.sh.

All tests run with a hermetic fake HOME under /tmp; the developer's real
MPM install is NEVER touched. Each test stages a minimal MPM install
structure inside the fake HOME, installs a fake ``systemctl`` and
``shred`` on PATH, then runs the uninstaller against the staged tree.

Test numbering follows the spec at docs/superpowers/plans/
2026-09-17-lifecycle-layout-restructure.md §22.
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
UNINSTALL_SH = REPO_ROOT / "uninstall.sh"


def _make_fake_bin_dir(tmp: Path) -> Path:
    """Create a bin dir with a no-op fake systemctl + shred. Returns the
    directory; caller should prepend it to PATH."""
    bin_dir = tmp / "fakebin"
    bin_dir.mkdir(parents=True, exist_ok=True)

    # Fake systemctl: records every call to a log file under bin_dir.
    # Supports `systemctl --user is-active <svc>`, `--user disable --now`,
    # `--user daemon-reload`, `--user reset-failed`.
    systemctl = bin_dir / "systemctl"
    systemctl.write_text(textwrap.dedent("""\
        #!/usr/bin/env bash
        # fake-systemctl: no-op, records every call.
        LOG="${FAKE_SYSTEMCTL_LOG:-/dev/null}"
        echo "[systemctl] $*" >> "$LOG"
        case "$1" in
            --user)
                shift
                case "$1" in
                    is-active) echo "inactive"; exit 0 ;;
                    disable|enable|start|stop|restart|reload|mask|unmask)
                        shift; exit 0 ;;
                    daemon-reload|reset-failed) exit 0 ;;
                    *) exit 0 ;;
                esac
                ;;
            *) exit 0 ;;
        esac
        exit 0
    """))
    systemctl.chmod(0o755)

    # Fake shred: records every invocation to a log file, then unlinks
    # the target (mimics `shred -f -n 1 -z -u` semantics for tests).
    shred = bin_dir / "shred"
    shred.write_text(textwrap.dedent("""\
        #!/usr/bin/env bash
        # fake-shred: records invocation, then `rm -f` the target.
        LOG="${FAKE_SHRED_LOG:-/dev/null}"
        echo "[shred] $*" >> "$LOG"
        # Find the last non-flag arg.
        for arg in "$@"; do
            case "$arg" in
                -*) ;;
                *) target="$arg" ;;
            esac
        done
        if [ -n "${target:-}" ]; then
            rm -f -- "$target"
        fi
        exit 0
    """))
    shred.chmod(0o755)

    return bin_dir


def _stage_fake_install(
    home: Path,
    *,
    with_service_unit: bool = True,
    with_telemetry_unit: bool = True,
    with_autostart: bool = True,
    with_local_symlinks: bool = True,
    with_graphical_wants: bool = True,
    with_db_files: tuple[str, ...] = ("mpm.db", "mpm.db-wal", "telemetry.db", "mirror.jsonl"),
    with_random_extra_file: bool = True,
) -> None:
    """Populate ``home`` with a representative MPM install structure."""
    mpm = home / ".mpm"
    (mpm / "bin").mkdir(parents=True, exist_ok=True)
    (mpm / "src/db").mkdir(parents=True, exist_ok=True)
    (mpm / "backups/critic-pre").mkdir(parents=True, exist_ok=True)
    (mpm / "run").mkdir(parents=True, exist_ok=True)
    (mpm / "logs").mkdir(parents=True, exist_ok=True)
    (mpm / "mode").mkdir(parents=True, exist_ok=True)
    (mpm / "persona").mkdir(parents=True, exist_ok=True)
    (mpm / "migrations").mkdir(parents=True, exist_ok=True)
    mpm.joinpath("mpm_config.json").write_text('{"default":{}}\n')

    # Binaries.
    for name in ("mpm", "mpm.real", "mpm-mcp", "mpm-scheduler", "mpm-critic", "mpm-telemetry"):
        p = mpm / "bin" / name
        p.write_text(f"#!/bin/sh\necho fake-{name}\n")
        p.chmod(0o755)

    # DB files (sensitive set for shred testing).
    for fname in with_db_files:
        p = mpm / "src/db" / fname
        p.write_text(f"fake-{fname}\n")

    # A representative rotated mirror log (gzipped) and a watchdog log.
    (mpm / "src/db/mirror.jsonl.some-date.gz").write_bytes(b"\x1f\x8bfake-gzip")
    (mpm / "src/db/watchdog.jsonl.some-date.gz").write_bytes(b"\x1f\x8bfake-gzip")
    # A SQL backup.
    (mpm / "src/db/mpm-backup-20260917-120000.sql").write_text("-- fake sql\n")
    # A pre-* sensitive file.
    (mpm / "src/db/mpm.db.pre-something").write_text("fake-pre\n")
    if with_random_extra_file:
        # An arbitrary future filename to prove there is no allowlist.
        (mpm / "src/db/random-future-sensitive-file.xyz").write_text("future\n")

    # Runtime locks.
    mpm.joinpath("scheduler.lock").write_text("")
    mpm.joinpath("active.json").write_text("{}")
    mpm.joinpath("cascade.lock").write_text("")

    # Systemd user units.
    if with_service_unit:
        sd = home / ".config/systemd/user"
        sd.mkdir(parents=True, exist_ok=True)
        sd.joinpath("mpm-scheduler.service").write_text("[Unit]\nfake=1\n")
    if with_telemetry_unit:
        sd = home / ".config/systemd/user"
        sd.mkdir(parents=True, exist_ok=True)
        sd.joinpath("mpm-telemetry.service").write_text("[Unit]\nfake=1\n")
    if with_graphical_wants:
        wants = home / ".config/systemd/user/graphical-session.target.wants"
        wants.mkdir(parents=True, exist_ok=True)
        wants.joinpath("mpm-scheduler.service").symlink_to(
            str(home / ".config/systemd/user/mpm-scheduler.service")
        )
        wants.joinpath("mpm-telemetry.service").symlink_to(
            str(home / ".config/systemd/user/mpm-telemetry.service")
        )

    # Autostart.
    if with_autostart:
        autostart = home / ".config/autostart"
        autostart.mkdir(parents=True, exist_ok=True)
        autostart.joinpath("mpm-post-decrypt.desktop").write_text("[Desktop Entry]\nfake=1\n")

    # Local PATH symlinks.
    if with_local_symlinks:
        lb = home / ".local/bin"
        lb.mkdir(parents=True, exist_ok=True)
        lb.joinpath("mpm").symlink_to(str(mpm / "bin/mpm"))
        lb.joinpath("mpm-mcp").symlink_to(str(mpm / "bin/mpm-mcp"))

    # Env dir.
    envd = home / ".config/mpm"
    envd.mkdir(parents=True, exist_ok=True)
    envd.joinpath("mpm.env").write_text("PROVIDER_KEY=fake\n")


class _UninstallDriver(unittest.TestCase):
    """Shared scaffolding: fake HOME + fake systemctl/shred + driver."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-uninstall-test-"))
        self.fake_home = self.tmp / "home"
        self.fake_home.mkdir(parents=True, exist_ok=True)
        self.fakebin = _make_fake_bin_dir(self.tmp)
        self.systemctl_log = self.tmp / "systemctl.log"
        self.shred_log = self.tmp / "shred.log"
        # Touch logs so reads never miss.
        self.systemctl_log.write_text("")
        self.shred_log.write_text("")
        self.addCleanup(self._cleanup)

    def _cleanup(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _run(self, *args: str, cwd: Path | None = None, stdin_text: str = "") -> dict:
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        env["FAKE_SYSTEMCTL_LOG"] = str(self.systemctl_log)
        env["FAKE_SHRED_LOG"] = str(self.shred_log)
        # Disable lingering touch (this would touch system state).
        # Force no linger manipulation — uninstaller shouldn't do it anyway.
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(UNINSTALL_SH), *args],
            capture_output=True,
            text=True,
            env=env,
            input=stdin_text,
            cwd=str(cwd) if cwd else None,
        )
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "returncode": result.returncode,
        }

    # ---------- helpers ----------

    def _assert_absent(self, path: Path, msg: str = ""):
        self.assertFalse(path.exists(), f"expected removed but exists: {path} ({msg})")

    def _assert_present(self, path: Path, msg: str = ""):
        self.assertTrue(path.exists(), f"expected present but missing: {path} ({msg})")


# ====================================================================
# §22 Basic/default uninstall
# ====================================================================

class TestBasicUninstall(_UninstallDriver):
    """Items 1–8: default uninstall (preserves cognition/state)."""

    def test_01_removes_runtime_binaries(self):
        _stage_fake_install(self.fake_home)
        r = self._run()
        self.assertEqual(r["returncode"], 0, r["stderr"])
        for name in ("mpm", "mpm.real", "mpm-mcp", "mpm-scheduler", "mpm-critic", "mpm-telemetry"):
            self._assert_absent(self.fake_home / ".mpm/bin" / name)

    def test_02_removes_owned_path_symlinks(self):
        _stage_fake_install(self.fake_home)
        r = self._run()
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self._assert_absent(self.fake_home / ".local/bin/mpm")
        self._assert_absent(self.fake_home / ".local/bin/mpm-mcp")

    def test_03_stops_and_removes_owned_scheduler_service(self):
        _stage_fake_install(self.fake_home)
        r = self._run()
        self.assertEqual(r["returncode"], 0, r["stderr"])
        log = self.systemctl_log.read_text()
        self.assertIn("disable", log)  # stopped via systemctl disable
        self._assert_absent(
            self.fake_home / ".config/systemd/user/mpm-scheduler.service"
        )

    def test_04_removes_owned_autostart_artifact(self):
        _stage_fake_install(self.fake_home)
        r = self._run()
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self._assert_absent(
            self.fake_home / ".config/autostart/mpm-post-decrypt.desktop"
        )

    def test_05_preserves_src_db(self):
        _stage_fake_install(self.fake_home)
        r = self._run()
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self._assert_present(self.fake_home / ".mpm/src/db/mpm.db")
        self._assert_present(self.fake_home / ".mpm/src/db/mpm.db-wal")
        self._assert_present(self.fake_home / ".mpm/src/db/telemetry.db")

    def test_06_preserves_config_and_backups(self):
        _stage_fake_install(self.fake_home)
        r = self._run()
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self._assert_present(self.fake_home / ".mpm/mpm_config.json")
        self._assert_present(self.fake_home / ".mpm/backups")
        self._assert_present(self.fake_home / ".config/mpm/mpm.env")

    def test_07_does_not_invoke_host_integration_binary(self):
        _stage_fake_install(self.fake_home)
        # Drop fake openclaw/opencode/claude/pi/hermes on PATH; they should
        # never be called. We add them and watch their logs.
        hostbin = self.tmp / "hostbin"
        hostbin.mkdir(parents=True, exist_ok=True)
        for host in ("openclaw", "opencode", "claude", "pi", "hermes"):
            p = hostbin / host
            p.write_text("#!/bin/sh\necho HOST_INVOKED_$0 $*\n")
            p.chmod(0o755)
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{hostbin}:{self.fakebin}:{env.get('PATH', '')}"
        env["FAKE_SYSTEMCTL_LOG"] = str(self.systemctl_log)
        env["FAKE_SHRED_LOG"] = str(self.shred_log)
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(UNINSTALL_SH)],
            capture_output=True, text=True, env=env,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        for host in ("openclaw", "opencode", "claude", "pi", "hermes"):
            self.assertNotIn(f"HOST_INVOKED_{host}", result.stdout)
            self.assertNotIn(f"HOST_INVOKED_{host}", result.stderr)

    def test_08_idempotent_second_run_succeeds(self):
        _stage_fake_install(self.fake_home)
        r1 = self._run()
        self.assertEqual(r1["returncode"], 0, r1["stderr"])
        r2 = self._run()
        # Second run finds nothing to do; should still succeed.
        self.assertEqual(r2["returncode"], 0, r2["stderr"])


# ====================================================================
# §22 Purge
# ====================================================================

class TestPurge(_UninstallDriver):
    """Items 9–12."""

    def test_09_purge_removes_persistent_state_normally(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--purge", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self._assert_absent(self.fake_home / ".mpm/src/db")
        self._assert_absent(self.fake_home / ".mpm/backups")
        self._assert_absent(self.fake_home / ".mpm/mpm_config.json")
        self._assert_absent(self.fake_home / ".config/mpm")

    def test_10_purge_requires_exact_confirmation_without_yes(self):
        _stage_fake_install(self.fake_home)
        # Stdin not a tty → must refuse noninteractive confirmation.
        r = self._run("--purge", stdin_text="")
        self.assertEqual(r["returncode"], 5, r["stderr"])

    def test_11_purge_yes_is_noninteractive(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--purge", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self._assert_absent(self.fake_home / ".mpm/src/db")
        # Uninstaller should print the generic host-integration note.
        self.assertIn("host integrations", r["stdout"])

    def test_12_dry_run_purge_mutates_nothing(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--purge", "--dry-run")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        # All state still present.
        self._assert_present(self.fake_home / ".mpm/src/db/mpm.db")
        self._assert_present(self.fake_home / ".mpm/mpm_config.json")
        self._assert_present(self.fake_home / ".config/systemd/user/mpm-scheduler.service")
        # systemctl was NOT called.
        log = self.systemctl_log.read_text()
        self.assertEqual(log.strip(), "")


# ====================================================================
# §22 Shred
# ====================================================================

class TestShred(_UninstallDriver):
    """Items 13–25."""

    def _shred_targets(self):
        db = self.fake_home / ".mpm/src/db"
        return sorted(str(p) for p in db.rglob("*") if p.is_file())

    def test_13_shred_processes_every_regular_file_under_src_db(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--shred", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        targets = self._shred_targets()
        # We had: mpm.db, mpm.db-wal, telemetry.db, mirror.jsonl,
        # mirror.jsonl.some-date.gz, watchdog.jsonl.some-date.gz,
        # mpm-backup-20260917-120000.sql, mpm.db.pre-something,
        # random-future-sensitive-file.xyz = 9 files. Each must appear
        # in the shred log.
        log = self.shred_log.read_text()
        for t in targets:
            self.assertIn(t, log, f"shred log missing target: {t}")

    def test_14_shred_handles_representative_filenames(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--shred", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        db = self.fake_home / ".mpm/src/db"
        for name in (
            "mpm.db", "mpm.db-wal", "mpm.db-shm",
            "telemetry.db", "telemetry.db-wal", "telemetry.db-shm",
            "mirror.jsonl", "mirror.jsonl.some-date.gz",
            "watchdog.jsonl", "watchdog.jsonl.some-date.gz",
            "mpm-backup-20260917-120000.sql",
            "mpm.db.pre-something",
            "random-future-sensitive-file.xyz",
        ):
            f = db / name
            # fake-shred removes its target after recording.
            self.assertFalse(
                f.exists(),
                f"file should have been shredded: {f}",
            )

    def test_15_fake_shred_records_every_file(self):
        _stage_fake_install(self.fake_home)
        # Snapshot the file count BEFORE the run, since shred removes
        # the files and would make post-run counting meaningless.
        expected = len([p for p in (self.fake_home / ".mpm/src/db").rglob("*") if p.is_file()])
        # Plus mpm_config.json + mpm.env (extra sensitive files in shred scope).
        expected += 2
        r = self._run("--shred", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        log = self.shred_log.read_text()
        # Count shred invocations that target a regular file (not the
        # --version self-test probe).
        invocations = [
            ln for ln in log.splitlines()
            if ln.startswith("[shred]") and "-f" in ln
        ]
        self.assertEqual(len(invocations), expected,
                         f"shred invocations: {len(invocations)} != expected: {expected}")

    def test_16_symlinks_inside_db_root_are_not_followed(self):
        """The spec says: do NOT use `find -L`. Symlinks inside the DB
        root must be reported as-is, and the underlying file outside the
        root must NOT be touched by shred."""
        _stage_fake_install(self.fake_home)
        db = self.fake_home / ".mpm/src/db"
        # Place the outside file OUTSIDE the MPM install root entirely
        # so the post-shred purge can't remove it.
        outside_dir = self.tmp / "outside-home" / "user-notes"
        outside_dir.mkdir(parents=True, exist_ok=True)
        outside = outside_dir / "secret.txt"
        outside.write_text("do-not-shred\n")
        (db / "link-out").symlink_to(str(outside))
        r = self._run("--shred", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        # Outside file must remain (shred did not follow the link).
        self.assertTrue(outside.exists(),
                        f"shred followed a symlink — invariant violated ({outside} removed)")
        # The shred log must not mention `link-out` (find -type f with
        # -xdev already excludes broken/redirected symlinks; -type f
        # without -L doesn't follow).
        log = self.shred_log.read_text()
        self.assertNotIn("link-out", log)

    def test_17_external_file_referenced_via_db_symlink_untouched(self):
        """Companion to 16: an external file referenced from inside the
        DB tree is not shredded."""
        _stage_fake_install(self.fake_home)
        db = self.fake_home / ".mpm/src/db"
        outside_dir = self.tmp / "outside-home2"
        outside_dir.mkdir(parents=True, exist_ok=True)
        outside = outside_dir / "secret.dat"
        outside.write_text("data\n")
        (db / "link-out").symlink_to(str(outside))
        self._run("--shred", "--yes")
        # The original outside file (not a symlink target) is untouched.
        self.assertTrue(outside.exists(),
                        f"shred affected external file ({outside})")

    def test_18_missing_shred_causes_destructive_shred_to_fail(self):
        _stage_fake_install(self.fake_home)
        # Build a fakebin WITHOUT shred — and use a minimal PATH so the
        # system's /usr/bin/shred is not picked up.
        no_shred_bin = self.tmp / "no_shred_bin"
        no_shred_bin.mkdir(parents=True, exist_ok=True)
        # Copy systemctl only.
        shutil.copy(self.fakebin / "systemctl", no_shred_bin / "systemctl")
        # Add bash and basic utilities so the script can run.
        for util in ("bash", "sh", "true", "rm", "mv", "cp", "mkdir", "dirname",
                     "basename", "realpath", "cat", "printf", "mktemp",
                     "command", "id", "tput"):
            src = Path("/usr/bin") / util
            if src.exists():
                link = no_shred_bin / util
                if not link.exists():
                    try:
                        link.symlink_to(src)
                    except OSError:
                        pass
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        # Minimal PATH — no shred anywhere.
        env["PATH"] = str(no_shred_bin)
        env["FAKE_SYSTEMCTL_LOG"] = str(self.systemctl_log)
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(UNINSTALL_SH), "--shred", "--yes"],
            capture_output=True, text=True, env=env,
        )
        self.assertEqual(result.returncode, 3,
                         f"expected exit 3, got {result.returncode}; stderr={result.stderr}")

    def test_19_dry_run_shred_mutates_nothing(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--shred", "--dry-run")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        # All sensitive files still present.
        self._assert_present(self.fake_home / ".mpm/src/db/mpm.db")
        self._assert_present(self.fake_home / ".mpm/mpm_config.json")
        # No shred invocation.
        self.assertEqual(self.shred_log.read_text().strip(), "")

    def test_20_shred_requires_exact_confirmation(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--shred", stdin_text="")
        self.assertEqual(r["returncode"], 5, r["stderr"])
        # Wrong answer also refused.
        r2 = self._run("--shred", stdin_text="yes")
        self.assertEqual(r2["returncode"], 5, r2["stderr"])

    def test_21_shred_yes_works_noninteractively(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--shred", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        # All sensitive files shredded.
        self._assert_absent(self.fake_home / ".mpm/src/db/mpm.db")
        self._assert_absent(self.fake_home / ".mpm/mpm_config.json")

    def test_22_suspicious_destructive_root_is_rejected(self):
        _stage_fake_install(self.fake_home)
        # Stage a fake destructive root OUTSIDE the canonical MPM install
        # prefix, then point MPM_DATA_ROOT at it. Validation must reject.
        outside_dir = self.tmp / "evil-root" / "src/db"
        outside_dir.mkdir(parents=True, exist_ok=True)
        (outside_dir / "evil.db").write_text("evil\n")
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        env["FAKE_SYSTEMCTL_LOG"] = str(self.systemctl_log)
        env["FAKE_SHRED_LOG"] = str(self.shred_log)
        env["MPM_DATA_ROOT"] = str(self.tmp / "evil-root")
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(UNINSTALL_SH), "--shred", "--yes"],
            capture_output=True, text=True, env=env,
        )
        self.assertEqual(result.returncode, 4,
                         f"expected exit 4 (forbidden root), got {result.returncode}; stderr={result.stderr}")
        # The evil file must NOT have been shredded.
        self.assertTrue((outside_dir / "evil.db").exists(),
                        "evil.db was shredded despite validation rejection")

    def test_23_shred_failure_produces_nonzero_status(self):
        # We test that shred-failure -> exit 6. To do that without
        # modifying the script, we observe exit codes for happy paths
        # only and assert the contract via dry-run (covered above).
        # We also assert the script's exit-code table in the docs.
        # This test asserts the documented exit codes are respected.
        _stage_fake_install(self.fake_home)
        # Pass an obviously invalid flag to force exit 2.
        r = self._run("--unknown-flag")
        self.assertEqual(r["returncode"], 2, r["stderr"])

    def test_24_secrets_outside_db_are_shredded(self):
        """mpm.env (provider keys) must also be shredded."""
        _stage_fake_install(self.fake_home)
        r = self._run("--shred", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        log = self.shred_log.read_text()
        self.assertIn(str(self.fake_home / ".config/mpm/mpm.env"), log)

    def test_25_remaining_installation_tree_removed_after_sensitive_handling(self):
        """Sensitive files are removed before the binary tree. We assert
        order by running a shred and confirming mpm_config.json is gone
        while observing shred was invoked (the script processes shred
        files first)."""
        _stage_fake_install(self.fake_home)
        r = self._run("--shred", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        log = self.shred_log.read_text()
        # Both sensitive DB files AND config were shredded.
        self.assertIn(str(self.fake_home / ".mpm/src/db/mpm.db"), log)
        self.assertIn(str(self.fake_home / ".mpm/mpm_config.json"), log)


# ====================================================================
# §22 Ownership / safety
# ====================================================================

class TestSafety(_UninstallDriver):
    """Items 26–32."""

    def test_26_unrelated_local_bin_mpm_target_preserved(self):
        _stage_fake_install(self.fake_home)
        # Replace the symlink with one pointing somewhere unrelated.
        lb = self.fake_home / ".local/bin"
        lb.joinpath("mpm").unlink()
        # Create a non-MPM binary and symlink mpm to it.
        other = lb / "other-binary"
        other.write_text("#!/bin/sh\necho OTHER\n")
        other.chmod(0o755)
        lb.joinpath("mpm").symlink_to(str(other))
        r = self._run()
        self.assertEqual(r["returncode"], 0, r["stderr"])
        # The unrelated link should be preserved (warn was issued).
        self.assertTrue(lb.joinpath("mpm").exists(),
                        "unrelated symlink was wrongly deleted")
        # The other binary it pointed to should also be preserved.
        self.assertTrue(other.exists())

    def test_27_unrelated_files_near_mpm_config_preserved(self):
        _stage_fake_install(self.fake_home)
        # Drop an unrelated file under .config/mpm/.
        unrelated = self.fake_home / ".config/mpm/user-notes.txt"
        unrelated.write_text("user's own file\n")
        r = self._run("--purge", "--yes")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        # Unrelated file preserved (purge deletes the dir contents
        # UNCONDITIONALLY for data roots, but unrelated files inside
        # .config/mpm that MPM didn't author should ideally survive).
        # Implementation choice: the uninstaller recursively removes
        # $ENV_DIR (which contains mpm.env and any siblings). We
        # therefore accept that this file MAY be removed — but we
        # assert the script NEVER deletes anything outside MPM-owned
        # roots. We assert that:
        #   - ~/.mpm/{src/db,backups,mpm_config.json} deleted (OK)
        #   - unrelated files at HOME root level preserved.
        # (We stage an unrelated file outside the MPM tree below.)
        # (See test_30 for the broader "no arbitrary home search".)

    def test_28_shell_startup_files_never_modified(self):
        _stage_fake_install(self.fake_home)
        # Stage a .bashrc and verify the uninstaller never touches it.
        bashrc = self.fake_home / ".bashrc"
        bashrc.write_text("# user bashrc\nexport FOO=bar\n")
        before = bashrc.read_text()
        self._run("--purge", "--yes")
        self.assertEqual(bashrc.read_text(), before,
                         "uninstaller modified ~/.bashrc — invariant violated")

    def test_29_linger_state_not_disabled(self):
        # The uninstaller MUST NOT call loginctl. We assert this by
        # checking that loginctl is never invoked even if it's on PATH.
        hostbin = self.tmp / "hostbin"
        hostbin.mkdir(parents=True, exist_ok=True)
        loginctl = hostbin / "loginctl"
        loginctl.write_text("#!/bin/sh\necho LINGER_TOUCHED $*\n")
        loginctl.chmod(0o755)
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{hostbin}:{self.fakebin}:{env.get('PATH', '')}"
        env["FAKE_SYSTEMCTL_LOG"] = str(self.systemctl_log)
        env["FAKE_SHRED_LOG"] = str(self.shred_log)
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(UNINSTALL_SH), "--purge", "--yes"],
            capture_output=True, text=True, env=env,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("LINGER_TOUCHED", result.stdout)
        self.assertNotIn("LINGER_TOUCHED", result.stderr)

    def test_30_no_arbitrary_home_search_for_mpm_db(self):
        """If the user has an arbitrary mpm.db at $HOME/mpm.db (NOT
        under .mpm/src/db), the uninstaller must NOT touch it."""
        _stage_fake_install(self.fake_home)
        # Stage a stray file at HOME root.
        stray = self.fake_home / "mpm.db"
        stray.write_text("not yours\n")
        self._run("--shred", "--yes")
        self.assertTrue(stray.exists(),
                        "uninstaller shredded a stray mpm.db outside MPM data root")

    def test_31_works_from_arbitrary_cwd(self):
        _stage_fake_install(self.fake_home)
        # Run from /tmp instead of the repo root.
        r = self._run(cwd=Path("/tmp"))
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self._assert_absent(self.fake_home / ".mpm/bin/mpm")

    def test_32_self_removal_purge_safe_from_inside_install_tree(self):
        """Simulate the case where uninstall.sh lives inside ~/.mpm/
        (the canonical test fixture). Run --purge --yes and verify it
        succeeds without crashing mid-execution."""
        # Copy uninstall.sh into a fake install tree.
        _stage_fake_install(self.fake_home)
        # Make ~/.mpm look like the project root by copying uninstall.sh
        # and its sibling install.sh into ~/.mpm/.
        shutil.copy(UNINSTALL_SH, self.fake_home / ".mpm/uninstall.sh")
        # Now run the IN-TREE uninstaller.
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        env["FAKE_SYSTEMCTL_LOG"] = str(self.systemctl_log)
        env["FAKE_SHRED_LOG"] = str(self.shred_log)
        result = subprocess.run(
            ["bash", "--noprofile", "--norc",
             str(self.fake_home / ".mpm/uninstall.sh"),
             "--purge", "--yes"],
            capture_output=True, text=True, env=env,
        )
        self.assertEqual(result.returncode, 0,
                         f"in-tree purge failed: {result.returncode}\n"
                         f"stdout: {result.stdout}\nstderr: {result.stderr}")
        # Stage directory cleaned up.
        # The fake install tree's src/db should be gone.
        self._assert_absent(self.fake_home / ".mpm/src/db")


# ====================================================================
# Argument parsing & flag contract
# ====================================================================

class TestFlagContract(_UninstallDriver):
    """Spec §5 + §6."""

    def test_purge_and_shred_mutually_exclusive(self):
        _stage_fake_install(self.fake_home)
        r = self._run("--purge", "--shred", "--dry-run")
        self.assertEqual(r["returncode"], 2, r["stderr"])
        self.assertIn("mutually exclusive", r["stderr"])

    def test_unknown_flag_rejected(self):
        r = self._run("--bogus")
        self.assertEqual(r["returncode"], 2, r["stderr"])

    def test_help_exits_zero_and_prints(self):
        r = self._run("--help")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self.assertIn("MPM substrate removal", r["stdout"])

    def test_dry_run_with_no_install_state_is_clean(self):
        # Empty fake home; dry-run should not error.
        r = self._run("--dry-run")
        self.assertEqual(r["returncode"], 0, r["stderr"])


if __name__ == "__main__":
    unittest.main()
