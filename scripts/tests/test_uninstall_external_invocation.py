"""
test_uninstall_external_invocation.py — Behavioral regression coverage for
the REAL destructive uninstall path when the executing ``uninstall.sh``
lives OUTSIDE the install prefix it is about to destroy.

Companion to ``test_uninstall_self_staging.py``, which covers the
canonical in-tree case (PROJECT_ROOT == PREFIX, which requires
self-staging). This file covers the equally valid external-invocation
topology:

  PROJECT_ROOT != PREFIX

In this topology:

  - PROJECT_ROOT == some external checkout dir (e.g. ``$TMP/external-checkout``);
  - PREFIX == ``$HOME/.mpm`` (the canonical install prefix);
  - the executing script lives OUTSIDE PREFIX and is therefore NOT in
    the destructive path;
  - no staging is necessary because removing $PREFIX cannot interrupt
    the running process.

Destructive semantics must NOT depend on whether staging occurred.
Both topologies must leave PREFIX gone on success.

Acceptance for --shred --yes:

  exit 0
  $HOME/.mpm completely gone
  $HOME/.config/mpm/mpm.env gone
  external uninstall.sh still exists
  external checkout sentinel survives
  HOME outside sentinel survives
  NO self-staging message in output (staging is unnecessary)
  uninstall complete reached
  shred invoked on every expected canary
  no outside target/sentinel passed to shred

Acceptance for --purge --yes:

  exit 0
  $HOME/.mpm completely gone
  external uninstall.sh still exists
  external checkout sentinel survives
  HOME outside sentinel survives
  NO self-staging message in output

Acceptance for default mode (external):

  exit 0
  $HOME/.mpm REMAINS (default uninstall preserves the install tree)

The test does NOT touch ``/home/v/.mpm`` or ``~/workspace/projects/mpm``.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
UNINSTALL_SH = REPO_ROOT / "uninstall.sh"


def _read(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="ignore")


class _ExternalInvocationFixture(unittest.TestCase):
    """Shared scaffolding for external-invocation destructive rehearsal.

    Top-level layout::

        TMPBASE/
          home/                       # $HOME for the test
            .mpm/                     # target PREFIX
              src/db/...
              backups/...
              logs/...
              mpm_config.json
            .config/mpm/mpm.env
            outside-sentinel
          external-checkout/          # executing script checkout (NOT PREFIX)
            uninstall.sh
            external-sentinel
          staging-parent/             # isolated TMPDIR for any mktemp
          shred-wrapper.log           # wrapper log
    """

    CANARY_FILES = (
        "src/db/shred-test/db-canary",
        "backups/shred-test/backup-canary",
        "logs/shred-test/log-canary",
        "src/db/a/b/c/future-format.random-extension",
        "mpm_config.json",
    )

    FORBIDDEN_SHRED_PATHS = (
        "outside-sentinel",
        "external-sentinel",
    )

    def setUp(self):
        if not UNINSTALL_SH.exists():
            self.skipTest("uninstall.sh not found")

        real_home = os.environ.get("HOME", "")
        assert real_home, "test environment must define $HOME"
        self.real_home = real_home

        self.tmpbase = Path(tempfile.mkdtemp(prefix="mpm-extinv-"))
        self._assert_safe_isolation()

        self.home = self.tmpbase / "home"
        self.prefix = self.home / ".mpm"
        self.config = self.home / ".config" / "mpm"
        self.outside_sentinel = self.home / "outside-sentinel"

        self.ext_checkout = self.tmpbase / "external-checkout"
        self.ext_uninstall = self.ext_checkout / "uninstall.sh"
        self.ext_sentinel = self.ext_checkout / "external-sentinel"

        self.staging_parent = self.tmpbase / "staging-parent"
        self.staging_parent.mkdir(parents=True, exist_ok=True)

        self.shred_log = self.tmpbase / "shred-wrapper.log"
        self.stdout_log = self.tmpbase / "uninstall.stdout"
        self.stderr_log = self.tmpbase / "uninstall.stderr"

        # Place a real copy of uninstall.sh at the external checkout.
        self.ext_checkout.mkdir(parents=True, exist_ok=True)
        shutil.copy(UNINSTALL_SH, self.ext_uninstall)
        self.ext_uninstall.chmod(0o755)
        self.ext_sentinel.write_text("EXTERNAL CHECKOUT SURVIVED\n")

    def tearDown(self):
        shutil.rmtree(self.tmpbase, ignore_errors=True)

    # ------------------------------------------------------------------
    # Safety
    # ------------------------------------------------------------------
    def _assert_safe_isolation(self):
        real = Path(self.real_home).resolve()
        # TMPBASE must not be the real HOME.
        self.assertNotEqual(self.tmpbase.resolve(), real,
                            "TMPBASE must not be the real $HOME")
        # TMPBASE must not be under /home/v/ — there must be zero
        # possibility of touching the dev profile's real state.
        self.assertFalse(
            str(self.tmpbase.resolve()).startswith("/home/v/"),
            "TMPBASE must not be under /home/v/",
        )
        # TMPBASE must not be the dev repo or under it.
        self.assertFalse(
            str(self.tmpbase.resolve()).startswith(str(REPO_ROOT) + "/")
            or self.tmpbase.resolve() == REPO_ROOT,
            "TMPBASE must not be inside the dev repo",
        )

    # ------------------------------------------------------------------
    # Disposable-tree construction
    # ------------------------------------------------------------------
    def _build_target_pfx(self):
        mpm = self.prefix
        for d in ("src/db", "backups", "logs", "mode", "persona",
                  "migrations", "bin", "run"):
            (mpm / d).mkdir(parents=True, exist_ok=True)
            (mpm / d).chmod(0o700)
        for sub in ("src/db/shred-test", "backups/shred-test",
                    "logs/shred-test", "src/db/a/b/c"):
            (mpm / sub).mkdir(parents=True, exist_ok=True)
            (mpm / sub).chmod(0o700)
        # Canary regular files (one per sensitive root + deep + config).
        (mpm / "src/db/shred-test/db-canary").write_text("db-canary")
        (mpm / "backups/shred-test/backup-canary").write_text("backup-canary")
        (mpm / "logs/shred-test/log-canary").write_text("log-canary")
        (mpm / "src/db/a/b/c/future-format.random-extension").write_text(
            "deep-canary")
        (mpm / "mpm_config.json").write_text("config-canary")

        # XDG-style env file outside $PREFIX.
        self.config.mkdir(parents=True, exist_ok=True)
        self.config.chmod(0o700)
        (self.config / "mpm.env").write_text("env-canary")

        # Outside sentinel — must NOT be touched.
        self.outside_sentinel.write_text("OUTSIDE SENTINEL SURVIVED\n")

    def _build_shred_wrapper(self) -> Path:
        """Place a `shred` wrapper early on PATH that logs every
        invocation to ``self.shred_log`` and delegates to the real
        /usr/bin/shred."""
        wrapper_dir = self.staging_parent / "shred-wrapper"
        wrapper_dir.mkdir(parents=True, exist_ok=True)
        wrapper = wrapper_dir / "shred"
        wrapper.write_text(
            "#!/usr/bin/env bash\n"
            "echo \"$(date -u +%FT%TZ) SHRED-WRAPPER $@\" >> \"$SHRED_LOG_FILE\"\n"
            "exec /usr/bin/shred \"$@\"\n"
        )
        wrapper.chmod(0o755)
        return wrapper_dir

    # ------------------------------------------------------------------
    # Run helpers
    # ------------------------------------------------------------------
    def _run_external(self, *args: str) -> subprocess.CompletedProcess:
        env = {
            **os.environ,
            "HOME": str(self.home),
            "TMPDIR": str(self.staging_parent),
            "PATH": "/usr/bin:/bin",
            "SHRED_LOG_FILE": str(self.shred_log),
        }
        if "SHRED_LOG_FILE" in env and "--shred" in args:
            wrapper_dir = self._build_shred_wrapper()
            env["PATH"] = f"{wrapper_dir}:{env['PATH']}"
            env["SHRED_LOG_FILE"] = str(self.shred_log)
        return subprocess.run(
            ["bash", str(self.ext_uninstall), *args],
            capture_output=True,
            text=True,
            env=env,
        )

    # ------------------------------------------------------------------
    # A. DEFAULT mode — external uninstall must NOT remove the
    #    install prefix wholesale. Persistent state is preserved; only
    #    owned runtime artifacts are removed.
    # ------------------------------------------------------------------
    def test_A_default_uninstall_preserves_pfx_and_persistent_state(self):
        self._build_target_pfx()
        proc = self._run_external()  # no mode flag -> default

        # For default mode, the wrapper is irrelevant; we only need
        # stdout/stderr.
        self.stdout_log.write_text(proc.stdout)
        self.stderr_log.write_text(proc.stderr)

        # External invocation succeeds.
        self.assertEqual(proc.returncode, 0,
                         f"external default uninstall failed:\n"
                         f"stdout: {proc.stdout}\nstderr: {proc.stderr}")

        # External checkout survives (we just wrote a copy here).
        self.assertTrue(self.ext_uninstall.is_file(),
                        "external uninstall.sh must survive default uninstall")
        self.assertTrue(self.ext_sentinel.is_file(),
                        "external-checkout sentinel must survive")

        # HOME outside sentinel survives.
        self.assertTrue(self.outside_sentinel.is_file(),
                        "HOME outside sentinel must survive default uninstall")
        self.assertEqual(self.outside_sentinel.read_text(),
                         "OUTSIDE SENTINEL SURVIVED\n")

        # Install prefix is INTACT for default mode (this is the
        # critical regression guard — the fix must not turn every
        # uninstall into `rm -rf ~/.mpm`).
        self.assertTrue(self.prefix.is_dir(),
                        f"default uninstall must preserve PREFIX, "
                        f"but it was removed: {self.prefix}")
        # And the persistent state dirs are preserved.
        self.assertTrue((self.prefix / "src/db").is_dir())
        self.assertTrue((self.prefix / "backups").is_dir())
        self.assertTrue((self.prefix / "mpm_config.json").is_file())
        self.assertTrue((self.config / "mpm.env").is_file())

        # Output does not advertise destructive prefix removal in
        # default mode.
        self.assertNotIn("install prefix to delete", proc.stdout)

    # ------------------------------------------------------------------
    # B. DESTRUCTIVE --shred --yes — external invocation removes the
    #    target PREFIX without staging.
    # ------------------------------------------------------------------
    def test_B_external_shred_removes_pfx_without_self_staging(self):
        self._build_target_pfx()
        proc = self._run_external("--shred", "--yes")
        self.stdout_log.write_text(proc.stdout)
        self.stderr_log.write_text(proc.stderr)

        # Acceptance: exit 0.
        self.assertEqual(proc.returncode, 0,
                         f"external --shred --yes failed:\n"
                         f"stdout: {proc.stdout}\nstderr: {proc.stderr}")

        # NO self-staging message in output (staging is unnecessary
        # because the script lives outside PREFIX).
        self.assertNotIn("staged uninstaller to", proc.stdout,
                         "external invocation must not self-stage")
        self.assertNotIn("re-executing staged copy", proc.stdout,
                         "external invocation must not re-exec a staged copy")

        # PREFIX is gone.
        self.assertFalse(self.prefix.exists(),
                         f"external --shred must remove PREFIX; "
                         f"still present: {self.prefix}")
        # Standalone env file gone (with parent dir).
        self.assertFalse((self.config / "mpm.env").exists(),
                         "standalone mpm.env must be gone after external --shred")
        self.assertFalse(self.config.exists(),
                         "XDG config dir must be gone after external --shred")

        # External script/checkout survives (we didn't touch the dev repo).
        self.assertTrue(self.ext_uninstall.is_file(),
                        "external uninstall.sh must survive external --shred")
        self.assertTrue(self.ext_sentinel.is_file(),
                        "external-checkout sentinel must survive")
        self.assertEqual(self.ext_sentinel.read_text(),
                         "EXTERNAL CHECKOUT SURVIVED\n")

        # HOME outside sentinel survives.
        self.assertTrue(self.outside_sentinel.is_file(),
                        "HOME outside sentinel must survive external --shred")
        self.assertEqual(self.outside_sentinel.read_text(),
                         "OUTSIDE SENTINEL SURVIVED\n")

        # --- shred invocation evidence ---
        log_text = _read(self.shred_log)
        shred_lines = [
            line for line in log_text.splitlines()
            if "SHRED-WRAPPER" in line and "--version" not in line
        ]
        # Each expected target must appear in some shred invocation.
        for target in self.CANARY_FILES:
            self.assertTrue(
                any(target in line for line in shred_lines),
                f"shred was NOT invoked on expected target: {target}\n"
                f"shred log:\n{log_text}",
            )
        # The standalone env file's path includes .config/mpm/mpm.env.
        self.assertTrue(
            any(".config/mpm/mpm.env" in line for line in shred_lines),
            f"shred was NOT invoked on .config/mpm/mpm.env\n"
            f"shred log:\n{log_text}",
        )
        # Outside targets/sentinels must NOT appear.
        for forbidden in self.FORBIDDEN_SHRED_PATHS:
            self.assertFalse(
                any(forbidden in line for line in shred_lines),
                f"shred was unexpectedly invoked on: {forbidden}\n"
                f"shred log:\n{log_text}",
            )

        # --- log evidence for the new contract ---
        self.assertIn("install prefix to delete", proc.stdout,
                      "destructive plan must advertise install prefix deletion")
        self.assertIn("removed: " + str(self.prefix) + " (install prefix)",
                      proc.stdout,
                      "external --shred must log install-prefix removal")
        self.assertIn("install prefix gone:", proc.stdout,
                      "external --shred must log install-prefix confirmed-gone")
        self.assertIn("uninstall complete", proc.stdout,
                      "external --shred must reach successful completion")

    # ------------------------------------------------------------------
    # C. DESTRUCTIVE --purge --yes — same semantic invariant applies
    #    to purge. PREFIX must be removed even though no shred happens.
    # ------------------------------------------------------------------
    def test_C_external_purge_removes_pfx_without_self_staging(self):
        self._build_target_pfx()
        proc = self._run_external("--purge", "--yes")
        self.stdout_log.write_text(proc.stdout)
        self.stderr_log.write_text(proc.stderr)

        # Acceptance: exit 0.
        self.assertEqual(proc.returncode, 0,
                         f"external --purge --yes failed:\n"
                         f"stdout: {proc.stdout}\nstderr: {proc.stderr}")

        # NO self-staging.
        self.assertNotIn("staged uninstaller to", proc.stdout,
                         "external --purge must not self-stage")
        self.assertNotIn("re-executing staged copy", proc.stdout,
                         "external --purge must not re-exec a staged copy")

        # PREFIX is gone.
        self.assertFalse(self.prefix.exists(),
                         f"external --purge must remove PREFIX; "
                         f"still present: {self.prefix}")
        # Standalone env file gone.
        self.assertFalse((self.config / "mpm.env").exists(),
                         "standalone mpm.env must be gone after external --purge")
        self.assertFalse(self.config.exists(),
                         "XDG config dir must be gone after external --purge")

        # External script/checkout survives.
        self.assertTrue(self.ext_uninstall.is_file(),
                        "external uninstall.sh must survive external --purge")
        self.assertTrue(self.ext_sentinel.is_file(),
                        "external-checkout sentinel must survive")
        self.assertEqual(self.ext_sentinel.read_text(),
                         "EXTERNAL CHECKOUT SURVIVED\n")

        # HOME outside sentinel survives.
        self.assertTrue(self.outside_sentinel.is_file(),
                        "HOME outside sentinel must survive external --purge")
        self.assertEqual(self.outside_sentinel.read_text(),
                         "OUTSIDE SENTINEL SURVIVED\n")

        # Log evidence.
        self.assertIn("install prefix to delete", proc.stdout,
                      "destructive plan must advertise install prefix deletion")
        self.assertIn("removed: " + str(self.prefix) + " (install prefix)",
                      proc.stdout,
                      "external --purge must log install-prefix removal")
        self.assertIn("install prefix gone:", proc.stdout,
                      "external --purge must log install-prefix confirmed-gone")
        self.assertIn("uninstall complete", proc.stdout,
                      "external --purge must reach successful completion")

    # ------------------------------------------------------------------
    # D. Dry-run plan honesty for destructive modes.
    # ------------------------------------------------------------------
    def test_D_destructive_dry_run_plan_advertises_prefix_deletion(self):
        self._build_target_pfx()
        proc = self._run_external("--shred", "--dry-run", "--yes")
        self.stdout_log.write_text(proc.stdout)
        self.stderr_log.write_text(proc.stderr)

        self.assertEqual(proc.returncode, 0,
                         f"external --shred --dry-run failed:\n"
                         f"stdout: {proc.stdout}\nstderr: {proc.stderr}")

        # PREFIX must NOT actually be removed in dry-run.
        self.assertTrue(self.prefix.is_dir(),
                        "dry-run must not remove PREFIX")
        # Canary files must still exist (mutation-free).
        for rel in self.CANARY_FILES:
            self.assertTrue(
                (self.prefix / rel).is_file(),
                f"dry-run must not delete canary: {rel}",
            )

        # Plan must advertise the install-prefix deletion.
        self.assertIn("install prefix to delete", proc.stdout)
        self.assertIn(self.prefix.as_posix() + " (install prefix)",
                      proc.stdout)

        # Default dry-run does NOT advertise this.
        proc_def = self._run_external("--dry-run", "--yes")
        self.stdout_log.write_text(proc_def.stdout + proc.stdout)
        self.stderr_log.write_text(proc_def.stderr + proc.stderr)
        self.assertEqual(proc_def.returncode, 0)
        self.assertNotIn("install prefix to delete", proc_def.stdout,
                         "default dry-run must not advertise destructive prefix "
                         "deletion")


if __name__ == "__main__":
    unittest.main()
