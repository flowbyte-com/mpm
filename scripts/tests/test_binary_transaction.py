#!/usr/bin/env python3
"""Transactional five-binary promotion tests.

Pins the contract from the transactional-install tranche: for every forced
failure point during promotion, the destination prefix must end with exactly
the same five binary states it had before installation began.

The transaction itself lives in scripts/lib/binary_transaction.sh and is
driven here as a library in a child bash — the same source-the-real-code
technique scripts/tests/test_install_phase_binaries.py already uses, so these
tests exercise the shipped function rather than a reimplementation of it.

Hermeticity: every root is a fresh mktemp directory, HOME is redirected into
it so install.sh's readonly LOCAL_BIN resolves inside the temp tree, and the
build tree is a scratch directory distinct from the destination. Nothing
resolves into the real ~/.mpm.
"""

import hashlib
import os
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
LIB = REPO_ROOT / "scripts" / "lib" / "binary_transaction.sh"

BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


class TransactionBase(unittest.TestCase):
    """Shared fixture: an OLD set, a NEW set, and a destination holding OLD."""

    def setUp(self):
        # Build roots on the same filesystem as the real ~/.mpm rather than
        # /tmp (tmpfs), so a cross-device staging bug cannot hide here.
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-txn-", dir=str(Path.home())))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.home = self._tmp / "home"
        self.home.mkdir()
        self.build = self._tmp / "new"
        self.prefix = self._tmp / "prefix"
        self.bindir = self.prefix / "bin"
        self.bindir.mkdir(parents=True)

        self.pre_hashes = {}
        self._make_set(self.build, "NEW")
        self._make_set(self._tmp / "old", "OLD")
        self._install_old_set()

    def _make_set(self, d: Path, tag: str, mode: int = 0o755):
        d.mkdir(parents=True, exist_ok=True)
        for b in BINARIES:
            f = d / b
            f.write_text(f"#!/usr/bin/env bash\necho {tag}-{b}\n")
            f.chmod(mode)

    def _install_old_set(self, subset=None):
        """Populate the destination with the OLD release."""
        for b in subset if subset is not None else BINARIES:
            src = self._tmp / "old" / b
            dst = self.bindir / b
            shutil.copy2(src, dst)
            dst.chmod(0o755)
            self.pre_hashes[b] = sha(dst)

    # ------------------------------------------------------------------
    def _run(self, fail_after=None, extra_env=None):
        """Invoke the real transaction. Returns CompletedProcess."""
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        # PATH must include coreutils (mktemp/install/flock) but nothing that
        # could resolve into the real install prefix.
        env["PATH"] = "/usr/bin:/bin"
        if fail_after is not None:
            env["MPM_INSTALL_TEST_FAIL_AFTER"] = str(fail_after)
        if extra_env:
            env.update(extra_env)

        driver = self._tmp / "drive.sh"
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            . {LIB}
            mpm_promote_binaries "{self.build}" "{self.bindir}"
            exit $?
        """))
        driver.chmod(0o755)
        return subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=env,
        )

    # ------------------------------------------------------------------
    def _assert_equals_pre_state(self, msg):
        """The decisive assertion: every binary is byte-identical to PRE.

        Binaries that had no pre-install counterpart (a partial or fresh
        install) are absent from self.pre_hashes and must be absent from the
        destination — asserting that here too, so one helper covers both the
        "restore the old bytes" and "do not invent state" requirements.
        """
        for b in BINARIES:
            p = self.bindir / b
            if b not in self.pre_hashes:
                self.assertFalse(
                    p.exists(),
                    f"{msg}: {b} had no pre-install counterpart but is present "
                    f"after rollback; the prefix was not returned to its "
                    f"pre-install state",
                )
                continue
            self.assertTrue(p.exists(), f"{msg}: {b} is MISSING from the prefix")
            self.assertEqual(
                sha(p), self.pre_hashes[b],
                f"{msg}: {b} does not match its pre-install hash — the prefix "
                f"was not returned to its pre-install state",
            )

    def _assert_no_transaction_leftovers(self, msg):
        """No staging or rollback directories may survive."""
        for pattern in (".mpm-stage.*", ".mpm-rollback.*"):
            leftovers = sorted(self.bindir.glob(pattern))
            self.assertEqual(
                leftovers, [],
                f"{msg}: rollback/staging material left behind: {leftovers}",
            )

    def _assert_all_new(self, msg):
        for b in BINARIES:
            p = self.bindir / b
            self.assertTrue(p.exists(), f"{msg}: {b} missing after success")
            self.assertNotEqual(
                sha(p), self.pre_hashes[b],
                f"{msg}: {b} still holds the OLD content after a successful install",
            )

    # ------------------------------------------------------------------
    def _nonvacuity(self):
        """The fixtures must actually distinguish OLD from NEW, or every
        assertion below passes for the wrong reason."""
        for b in BINARIES:
            new = (self.build / b).read_text()
            old = (self.bindir / b).read_text()
            self.assertIn("NEW-", new, f"fixture {b} candidate is not tagged NEW")
            self.assertIn("OLD-", old, f"fixture {b} installed binary is not tagged OLD")
            self.assertNotEqual(sha(self.build / b), sha(self.bindir / b))


class TestSuccess(TransactionBase):
    def test_successful_install_promotes_all_five(self):
        self._nonvacuity()
        r = self._run()
        self.assertEqual(r.returncode, 0, f"expected commit:\n{r.stderr}")
        self._assert_all_new("after success")
        self._assert_no_transaction_leftovers("after success")

    def test_successful_install_leaves_source_tree_intact(self):
        self._run()
        for b in BINARIES:
            self.assertTrue(
                (self.build / b).is_file(),
                f"promotion consumed the build artifact {b}; build output must survive",
            )

    def test_installed_binaries_are_executable(self):
        self._run()
        for b in BINARIES:
            self.assertTrue(
                self.bindir.joinpath(b).stat().st_mode & stat.S_IXUSR,
                f"{b} is not executable after promotion",
            )


class TestFailureMatrix(TransactionBase):
    """§24 — failure at every promotion boundary.

    For each failure point: non-zero exit, and the prefix holds exactly the
    PRE hashes. This is the tranche's decisive acceptance test.
    """

    def test_failure_at_each_promotion_boundary_restores_pre_state(self):
        self._nonvacuity()
        for fail_after in range(len(BINARIES) + 1):
            with self.subTest(fail_after=fail_after):
                self.setUp()  # fresh OLD prefix for each boundary
                r = self._run(fail_after=fail_after)
                self.assertNotEqual(
                    r.returncode, 0,
                    f"fail_after={fail_after}: installer reported success "
                    f"despite an injected failure",
                )
                self._assert_equals_pre_state(f"fail_after={fail_after}")
                self._assert_no_transaction_leftovers(f"fail_after={fail_after}")

    def test_failure_before_any_promotion_touches_nothing(self):
        self._run(fail_after=0)
        self._assert_equals_pre_state("fail_before_promotion")
        self._assert_no_transaction_leftovers("fail_before_promotion")

    def test_rollback_message_is_truthful(self):
        """§26 — the operator must be told the previous set was restored."""
        r = self._run(fail_after=2)
        self.assertIn(
            "previous binary set restored", r.stderr,
            "rollback did not report that the previous binary set was restored",
        )
        self.assertNotIn(
            "deployed", r.stderr.lower().replace("deployed (", ""),
            "a failed promotion claimed a deployment occurred",
        )


class TestFreshInstall(TransactionBase):
    """§12 — first install: there is no OLD counterpart to restore."""

    def setUp(self):
        super().setUp()
        for b in BINARIES:
            self.bindir.joinpath(b).unlink()
        self.pre_hashes.clear()

    def test_fresh_install_failure_leaves_prefix_empty(self):
        r = self._run(fail_after=2)
        self.assertNotEqual(r.returncode, 0, "fresh install failure reported success")
        for b in BINARIES:
            p = self.bindir / b
            self.assertFalse(
                p.exists(),
                f"{b} was left behind by a failed FIRST install; there was no "
                f"old counterpart, so rollback must remove it rather than "
                f"leave a false complete installation",
            )
        self._assert_no_transaction_leftovers("fresh install failure")

    def test_fresh_install_success_installs_all_five(self):
        r = self._run()
        self.assertEqual(r.returncode, 0, f"fresh install failed:\n{r.stderr}")
        for b in BINARIES:
            self.assertTrue(self.bindir.joinpath(b).is_file(), f"{b} not installed")


class TestPartialExistingInstall(TransactionBase):
    """§13 — one OLD binary already missing before the install."""

    def test_partial_prefix_failure_restores_to_exact_pre_state(self):
        missing = "mpm-critic"
        self.bindir.joinpath(missing).unlink()
        # The missing binary has no PRE hash and must still be absent after.
        self.pre_hashes.pop(missing)

        r = self._run(fail_after=3)
        self.assertNotEqual(r.returncode, 0, "partial-prefix failure reported success")
        self._assert_equals_pre_state("partial prefix failure")
        self._assert_no_transaction_leftovers("partial prefix failure")

    def test_partial_prefix_success_installs_all_five(self):
        self.bindir.joinpath("mpm-critic").unlink()
        self.pre_hashes.pop("mpm-critic")
        r = self._run()
        self.assertEqual(r.returncode, 0, f"reinstall failed:\n{r.stderr}")
        for b in BINARIES:
            self.assertTrue(self.bindir.joinpath(b).is_file(), f"{b} not installed")


class TestCandidateValidation(TransactionBase):
    """§9 — all candidates validated before any destination is touched."""

    def test_missing_candidate_fails_before_promoting_anything(self):
        self.build.joinpath("mpm-telemetry").unlink()
        r = self._run()
        self.assertNotEqual(r.returncode, 0, "missing candidate reported success")
        self._assert_equals_pre_state("missing fifth candidate")
        self._assert_no_transaction_leftovers("missing fifth candidate")

    def test_non_executable_candidate_is_rejected_before_promotion(self):
        self.build.joinpath("mpm-critic").chmod(0o644)
        r = self._run()
        self.assertNotEqual(r.returncode, 0, "non-executable candidate accepted")
        self._assert_equals_pre_state("non-executable candidate")

    def test_directory_masquerading_as_binary_is_rejected(self):
        self.build.joinpath("mpm-mcp").unlink()
        self.build.joinpath("mpm-mcp").mkdir()
        r = self._run()
        self.assertNotEqual(r.returncode, 0, "a directory was accepted as a binary")
        self._assert_equals_pre_state("directory masquerading as binary")

    def test_unreadable_candidate_does_not_destroy_the_old_binary(self):
        """The pre-fix `install` unlinked the destination before reading the
        source, so an unreadable candidate DELETED the old binary."""
        self.build.joinpath("mpm-critic").chmod(0o000)
        try:
            r = self._run()
        finally:
            self.build.joinpath("mpm-critic").chmod(0o755)
        self.assertNotEqual(r.returncode, 0, "unreadable candidate reported success")
        self._assert_equals_pre_state("unreadable candidate")
        self._assert_no_transaction_leftovers("unreadable candidate")


class TestMetadataPreservation(TransactionBase):
    """§10 — rollback restores the old file's metadata, not just its bytes."""

    def test_rollback_restores_mode_and_ownership(self):
        # Give one old binary a distinctive mode the installer does not use.
        odd_mode = 0o750
        self.bindir.joinpath("mpm-critic").chmod(odd_mode)
        self.pre_hashes["mpm-critic"] = sha(self.bindir / "mpm-critic")
        st_before = self.bindir.joinpath("mpm-critic").stat()

        self._run(fail_after=3)

        st_after = self.bindir.joinpath("mpm-critic").stat()
        self.assertEqual(
            stat.S_IMODE(st_after.st_mode), stat.S_IMODE(st_before.st_mode),
            "rollback restored the old bytes but not the old mode",
        )
        self.assertEqual(
            st_after.st_uid, st_before.st_uid,
            "rollback changed the restored file's owner",
        )

    def test_promoted_binaries_are_executable(self):
        self._run()
        for b in BINARIES:
            mode = stat.S_IMODE(self.bindir.joinpath(b).stat().st_mode)
            self.assertTrue(mode & 0o111, f"{b} promoted with mode {oct(mode)}")


class TestRollbackFailure(TransactionBase):
    """§18 — restoration itself can fail; report it honestly."""

    def test_rollback_failure_is_reported_and_material_preserved(self):
        """Make restoration impossible by removing rollback write access.

        The rollback directory is created 0700 inside the destination, so
        making the DESTINATION read-only prevents the rename back — the
        realistic shape of a rollback that cannot complete.
        """
        # Find the rollback dir by running the transaction once to discover
        # the naming scheme is unstable (mktemp), so instead: run with an
        # injected failure and assert on the report. To force restoration
        # failure deterministically, replace `mv` with a failing stub.
        stub = self._tmp / "stub"
        stub.mkdir()
        # `mv` that fails ONLY when moving a binary BACK OUT of a rollback
        # directory — i.e. the restoration step. Backup (into the rollback
        # dir) and promotion (out of the staging dir) must still work, so the
        # transaction actually reaches the rollback phase.
        (stub / "mv").write_text(
            "#!/usr/bin/env bash\n"
            'src=""\n'
            'for a in "$@"; do case "$a" in -*) continue;; esac; src="$a"; break; done\n'
            'case "$src" in *.mpm-rollback.*/*) exit 1;; esac\n'
            'exec /usr/bin/mv "$@"\n'
        )
        (stub / "mv").chmod(0o755)

        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env["PATH"] = f"{stub}:/usr/bin:/bin"
        env["MPM_INSTALL_TEST_FAIL_AFTER"] = "2"

        driver = self._tmp / "drive2.sh"
        driver.write_text(
            f'#!/usr/bin/env bash\nset -uo pipefail\n. {LIB}\n'
            f'mpm_promote_binaries "{self.build}" "{self.bindir}"\n'
            f"exit $?\n"
        )
        driver.chmod(0o755)
        r = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=env,
        )

        self.assertEqual(
            r.returncode, 11,
            f"expected the rollback-incomplete code 11, got {r.returncode}:\n{r.stderr}",
        )
        self.assertIn("ROLLBACK FAILED", r.stderr)
        self.assertIn("rollback material is preserved", r.stderr)
        # The recovery copy must survive — deleting it would destroy the
        # operator's only route back.
        self.assertTrue(
            sorted(self.bindir.glob(".mpm-rollback.*")),
            "rollback material was deleted even though restoration failed",
        )


class TestConcurrentInstallers(TransactionBase):
    """§19 — a per-prefix lock serialises promotion."""

    def test_second_installer_refuses_while_lock_is_held(self):
        # Hold the lock with flock on the same fd/path the library uses.
        lock_path = self.prefix / ".mpm-install.lock"
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        holder = subprocess.Popen(
            ["flock", "-n", str(lock_path), "sleep", "10"],
        )
        try:
            import time
            # Give flock a moment to acquire.
            for _ in range(50):
                time.sleep(0.05)
                if lock_path.exists():
                    break
            r = self._run()
            self.assertEqual(
                r.returncode, 12,
                f"a concurrent installer was allowed to proceed:\n{r.stderr}",
            )
            self.assertIn("lock", r.stderr.lower())
            self._assert_equals_pre_state("concurrent installer refused")
        finally:
            holder.terminate()
            holder.wait(timeout=10)

    def test_lock_is_released_after_a_normal_run(self):
        r = self._run()
        self.assertEqual(r.returncode, 0, f"first run failed:\n{r.stderr}")
        # A second run must succeed, proving no stale lock was left held.
        r2 = self._run()
        self.assertEqual(
            r2.returncode, 0,
            f"second run refused — the lock was not released:\n{r2.stderr}",
        )


class TestCustomPrefix(TransactionBase):
    """§29 — the transaction must not assume ~/.mpm."""

    def test_transaction_stays_inside_a_custom_prefix(self):
        custom = self._tmp / "somewhere" / "else" / "custom-root"
        custom_bin = custom / "bin"
        custom_bin.mkdir(parents=True)
        for b in BINARIES:
            shutil.copy2(self._tmp / "old" / b, custom_bin / b)

        driver = self._tmp / "custom.sh"
        driver.write_text(
            f'#!/usr/bin/env bash\nset -uo pipefail\n. {LIB}\n'
            f'mpm_promote_binaries "{self.build}" "{custom_bin}"\n'
            f"exit $?\n"
        )
        driver.chmod(0o755)
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env["PATH"] = "/usr/bin:/bin"
        r = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=env,
        )
        self.assertEqual(r.returncode, 0, f"custom-prefix install failed:\n{r.stderr}")
        for b in BINARIES:
            self.assertIn(
                "NEW-", custom_bin.joinpath(b).read_text(),
                f"{b} was not promoted into the custom prefix",
            )

    def test_no_path_resolves_into_the_real_runtime(self):
        driver_src = LIB.read_text()
        # The library must never hardcode an absolute default prefix; the
        # destination always comes from the caller.
        self.assertNotIn("$HOME/.mpm/bin", driver_src.replace("$PREFIX/bin", ""))
        self.assertNotIn("~/.mpm", driver_src)


class TestServiceOrdering(TransactionBase):
    """§15/§20 — no service restart may precede the transaction commit."""

    def test_install_sh_restarts_service_only_after_binary_phase(self):
        install = (REPO_ROOT / "install.sh").read_text()
        binaries_at = install.index("\n    phase_binaries\n")
        service_at = install.index("\n    phase_service\n")
        self.assertLess(
            binaries_at, service_at,
            "phase_service must run after phase_binaries; a restart onto a "
            "rolled-back or mixed release is exactly what this prevents",
        )

    def test_transaction_library_contains_no_systemctl(self):
        self.assertNotIn(
            "systemctl", LIB.read_text(),
            "the promotion library must not restart services; that is the "
            "caller's job, after commit",
        )


class TestDryRun(TransactionBase):
    """§27 — dry-run text must describe the real mechanism."""

    def test_dry_run_text_describes_staging_not_bare_install(self):
        install = (REPO_ROOT / "install.sh").read_text()
        dry = install[install.index("mode_dry_run()"):install.index("mode_validate()")]
        self.assertIn(
            "transaction", dry.lower(),
            "dry-run narration does not mention the transactional promotion, "
            "so it misdescribes what the installer now does",
        )
        # The commit point must be stated, because "did the install really
        # happen" is exactly what a dry run is read to answer.
        self.assertIn(
            "commit", dry.lower(),
            "dry-run narration does not state the commit point",
        )
        # The old narration listed five `install -m 0755` lines, which is no
        # longer what happens.
        self.assertNotIn(
            "install -m 0755 <BUILD_DIR>/mpm ",
            dry,
            "dry-run still narrates a bare per-file install, not the transaction",
        )


class TestBackupPhaseFailure(TransactionBase):
    """§22/§G — a failure during the BACKUP step must not delete anything.

    The backup step moves the OLD binaries into the rollback directory one at
    a time. If it fails partway, the binaries it never reached are still
    sitting in the destination, untouched and still owned by the operator.
    Rollback must leave them alone.

    It used to destroy them. Rollback decided what to remove by asking
    "is there no backup copy for this one?" — but "no backup copy" describes
    two different situations that look identical on disk:

      * a first install, where WE created the file and must remove it, and
      * a partially-completed backup, where the file is the operator's
        ORIGINAL and must be left exactly as it is.

    Guessing wrong there deletes binaries that were never at risk.
    """

    def _stub_mv_failing_backup_of(self, binary):
        """A `mv` that fails only when backing up `binary` into the rollback dir.

        The backup direction is <bindir>/<binary> -> <bindir>/.mpm-rollback.*/
        <binary>. Restoration moves the other way, out of the rollback
        directory, so it still works and the transaction genuinely reaches the
        point where it needs to undo its work.
        """
        stub = self._tmp / "stub-backup"
        stub.mkdir(exist_ok=True)
        (stub / "mv").write_text(
            "#!/usr/bin/env bash\n"
            'src=""; dst=""\n'
            'for a in "$@"; do\n'
            '  case "$a" in -*) continue;; esac\n'
            '  if [ -z "$src" ]; then src="$a"; else dst="$a"; fi\n'
            "done\n"
            f'case "$dst" in\n'
            f'  */.mpm-rollback.*/{binary}) exit 1;;\n'
            "esac\n"
            'exec /usr/bin/mv "$@"\n'
        )
        (stub / "mv").chmod(0o755)
        return stub

    def test_backup_failure_does_not_delete_never_backed_up_binaries(self):
        stub = self._stub_mv_failing_backup_of("mpm-critic")
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env["PATH"] = f"{stub}:/usr/bin:/bin"

        driver = self._tmp / "drive-backup-fail.sh"
        driver.write_text(
            f'#!/usr/bin/env bash\nset -uo pipefail\n. {LIB}\n'
            f'mpm_promote_binaries "{self.build}" "{self.bindir}"\n'
            f"exit $?\n"
        )
        driver.chmod(0o755)
        r = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=env,
        )

        self.assertNotEqual(
            r.returncode, 0,
            f"a failed backup must not report success:\n{r.stderr}",
        )
        # The decisive assertion: every binary is byte-identical to PRE. The
        # three binaries the backup never reached were never in danger, and
        # must still be exactly as the operator left them.
        self._assert_equals_pre_state("backup failed partway")
        self._assert_no_transaction_leftovers("backup failed partway")


class TestSignalCleanup(TransactionBase):
    """§22 — a signal during promotion must not strand the prefix.

    Mid-promotion the OLD binaries are already in the rollback directory and
    only some NEW ones are in place, so simply dying leaves the prefix with
    binaries MISSING from it. The transaction installs INT/TERM handlers to
    restore the previous set before exiting.
    """

    def _drive_slowly(self, bindir, dest):
        """A driver whose `mv` sleeps, so a signal reliably lands mid-promotion."""
        d = self._tmp / "drive-slow.sh"
        d.write_text(
            f'#!/usr/bin/env bash\nset -uo pipefail\n. {LIB}\n'
            'mv() { sleep 0.5; command mv "$@"; }\n'
            f'mpm_promote_binaries "{bindir}" "{dest}"\n'
            f"exit $?\n"
        )
        d.chmod(0o755)
        return d

    def _assert_restored_after_signal(self, signum):
        import signal as _signal
        driver = self._drive_slowly(self.build, self.bindir)
        proc = subprocess.Popen(
            ["bash", "--noprofile", "--norc", str(driver)],
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
            text=True, env={**os.environ, "HOME": str(self.home)},
        )
        try:
            # Let it get past staging and into the backup/promotion window.
            _signal.pause  # no-op; keeps the import obviously used
            import time
            time.sleep(1.1)
            proc.send_signal(signum)
            out, err = proc.communicate(timeout=30)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.communicate()

        self._assert_equals_pre_state(f"SIG{signum} mid-promotion")
        self._assert_no_transaction_leftovers(f"SIG{signum} mid-promotion")

    def test_sigterm_during_promotion_restores_previous_set(self):
        import signal
        self._assert_restored_after_signal(signal.SIGTERM)

    def test_sigint_during_promotion_restores_previous_set(self):
        import signal
        self._assert_restored_after_signal(signal.SIGINT)

    def test_library_installs_signal_handlers(self):
        """The handlers must exist in the shipped source, not just in a test."""
        src = LIB.read_text()
        self.assertIn("trap ", src,
                      "the transaction installs no signal handlers, so an "
                      "interrupted promotion leaves binaries missing from "
                      "the prefix")
        self.assertIn("_bt_tx_signal", src)


if __name__ == "__main__":
    unittest.main()
