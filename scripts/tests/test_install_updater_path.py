#!/usr/bin/env python3
"""test_install_updater_path.py — the `mpm-update` PATH convenience link.

WHY THIS EXISTS
---------------

The standalone updater is provisioned at ``<data root>/mpm-update`` — a
real file, deliberately outside ``current/`` so it survives version
switches and a broken active release. That is the right place for the
recovery executable, but it is not on PATH, so the documented recovery
incantation only works if the operator spells out the absolute path.

The installer therefore exposes ``~/.local/bin/mpm-update`` as a symlink
to the data-root copy. Two properties matter and are pinned here:

1. **The link names the data-root copy, never ``current/bin``.** A link
   through ``bin/`` would resolve into the ACTIVE RELEASE and die with
   it — the exact failure mode the stable copy exists to prevent.
2. **The installer never clobbers an unrelated executable.** The
   mpm/mpm-mcp links are created with ``rm -f`` first, which is fine for
   binaries the installer owns. An operator may already have their own
   ``mpm-update`` on PATH; that file is not the installer's to replace.

The link also has to SURVIVE an update: the updater replaces the
data-root file by ``rename(2)``, so the link keeps resolving without
being recreated. That is asserted by replacing the target the way the
updater does, and re-running the link.

HERMETICITY
-----------

Every root is a fresh temp directory and HOME is redirected into it. The
live installation is never read or written.
"""

import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO_ROOT / "install.sh"
UNINSTALL_SH = REPO_ROOT / "uninstall.sh"

STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def build_sourced_lib(text: str) -> str:
    out = []
    for line in text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out.append(line)
    return "\n".join(out) + "\n"


class UpdaterPathFixture(unittest.TestCase):
    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-updater-path-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.home = self._tmp / "home"
        self.home.mkdir()
        self.root = self._tmp / "mpm"
        (self.root / "bin").mkdir(parents=True)

        # phase_symlinks requires the two PATH binaries to exist.
        for name in ("mpm", "mpm-mcp"):
            p = self.root / "bin" / name
            p.write_text("#!/bin/sh\nexit 0\n")
            p.chmod(0o755)

        self.project = self._tmp / "project"
        self.project.mkdir()
        (self.project / "install.sh.lib").write_text(
            build_sourced_lib(INSTALL_SH.read_text())
        )

        self.link = self.home / ".local" / "bin" / "mpm-update"
        self.recovery = self.root / "mpm-update"

    # ------------------------------------------------------------------
    def drive(self, script: str = "phase_symlinks") -> subprocess.CompletedProcess:
        driver = self.project / "_drive.sh"
        driver.write_text(
            textwrap.dedent(f"""\
                #!/usr/bin/env bash
                set -uo pipefail
                export PROJECT_ROOT="{self.project}"
                source "{self.project}/install.sh.lib"
                PREFIX="{self.root}"
                DATA_ROOT="{self.root}"
                export PREFIX DATA_ROOT
                {script}
            """)
        )
        driver.chmod(0o755)
        env = dict(os.environ)
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        return subprocess.run(
            ["bash", str(driver)], capture_output=True, text=True, timeout=120, env=env
        )

    def make_recovery(self, name: str, body: str) -> None:
        p = self.root / name
        p.write_text(f"#!/bin/sh\necho '{body}'\n")
        p.chmod(0o755)

    def link_target(self) -> str:
        return os.readlink(self.link) if self.link.is_symlink() else ""


class TestLinkIsCreated(UpdaterPathFixture):
    def test_a_symlink_to_the_data_root_copy_is_created(self):
        self.make_recovery("mpm-update", "recovery")
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertTrue(self.link.is_symlink(), "no ~/.local/bin/mpm-update was created")
        self.assertEqual(self.link_target(), str(self.recovery))

    def test_the_link_does_not_point_into_the_active_release(self):
        self.make_recovery("mpm-update", "recovery")
        self.drive()
        self.assertTrue(self.link.is_symlink(), "no link was created, so this proves nothing")
        target = self.link_target()
        self.assertNotIn("current/", target,
                         "the convenience link must not resolve through the active release")
        self.assertFalse(target.startswith(str(self.root / "bin")),
                         "the convenience link must not point at bin/ (a symlink into current)")

    def test_the_link_is_executable_and_runs_the_recovery_binary(self):
        self.make_recovery("mpm-update", "RECOVERY-OK")
        self.drive()
        out = subprocess.run([str(self.link)], capture_output=True, text=True, timeout=30)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("RECOVERY-OK", out.stdout)

    def test_the_recovery_executable_stays_a_real_file(self):
        self.make_recovery("mpm-update", "recovery")
        self.drive()
        self.assertTrue(self.recovery.is_file())
        self.assertFalse(self.recovery.is_symlink(),
                         "the recovery executable must remain a real file")


class TestIdempotence(UpdaterPathFixture):
    def test_running_twice_is_stable(self):
        self.make_recovery("mpm-update", "recovery")
        first = self.drive()
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        target_after_first = self.link_target()

        second = self.drive()
        self.assertEqual(second.returncode, 0, second.stdout + second.stderr)
        self.assertEqual(self.link_target(), target_after_first)
        self.assertTrue(self.link.is_symlink())

    def test_a_retargeted_symlink_is_repointed(self):
        self.make_recovery("mpm-update", "recovery")
        self.link.parent.mkdir(parents=True, exist_ok=True)
        self.link.symlink_to("/some/stale/place")
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.link_target(), str(self.recovery),
                         "an existing symlink must be repointed at the data-root copy")


class TestNeverClobbersForeignFiles(UpdaterPathFixture):
    def test_a_real_file_at_the_destination_is_not_overwritten(self):
        self.make_recovery("mpm-update", "recovery")
        self.link.parent.mkdir(parents=True, exist_ok=True)
        self.link.write_text("#!/bin/sh\necho 'OPERATOR OWNED'\n")
        self.link.chmod(0o755)

        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(self.link.is_symlink(),
                         "the installer replaced an operator-owned file with a symlink")
        self.assertIn("OPERATOR OWNED", self.link.read_text())
        # And it says so, rather than failing silently.
        self.assertIn("NOT a symlink", r.stdout + r.stderr)


class TestNonBundleLayout(UpdaterPathFixture):
    def test_no_link_is_created_when_there_is_no_standalone_updater(self):
        # The non-bundle layout has no release-scoped updater; the
        # installer must not advertise a path that does not exist.
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(self.link.exists() or self.link.is_symlink())
        self.assertIn("not created", r.stdout + r.stderr)


class TestSurvivesAnUpdate(UpdaterPathFixture):
    def test_the_link_follows_an_atomic_replacement_of_the_target(self):
        """The updater replaces the data-root file by rename(2).

        The link must keep resolving to the new file without being
        recreated — that is what makes the convenience path safe across
        updates.
        """
        self.make_recovery("mpm-update", "VERSION-ONE")
        self.drive()
        self.assertEqual(self.link_target(), str(self.recovery))

        # Replace exactly the way rb_provision_stable_updater does:
        # write a new file beside the destination, rename over it.
        staged = self.root / ".mpm-update.stage.test"
        staged.write_text("#!/bin/sh\necho 'VERSION-TWO'\n")
        staged.chmod(0o755)
        os.replace(staged, self.recovery)

        out = subprocess.run([str(self.link)], capture_output=True, text=True, timeout=30)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("VERSION-TWO", out.stdout,
                      "the convenience link stopped resolving after an atomic update")
        self.assertTrue(self.link.is_symlink(), "the link was replaced during the update")


class TestUninstallRemovesOnlyOwnedLinks(UpdaterPathFixture):
    def _drive_uninstall(self) -> subprocess.CompletedProcess:
        # A disposable HOME so uninstall's own HOME-derived paths are safe.
        env = dict(os.environ)
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        # --dry-run prints the plan without mutating anything.
        return subprocess.run(
            ["bash", "--noprofile", "--norc", str(UNINSTALL_SH), "--dry-run"],
            capture_output=True, text=True, timeout=180, env=env,
        )

    def test_an_owned_link_is_listed_for_removal(self):
        self.make_recovery("mpm-update", "recovery")
        self.link.parent.mkdir(parents=True, exist_ok=True)
        self.link.symlink_to(self.recovery)

        r = self._drive_uninstall()
        self.assertIn(str(self.link), r.stdout + r.stderr)

    def test_a_foreign_link_is_refused(self):
        self.make_recovery("mpm-update", "recovery")
        self.link.parent.mkdir(parents=True, exist_ok=True)
        self.link.symlink_to("/usr/local/bin/something-else")

        r = self._drive_uninstall()
        combined = r.stdout + r.stderr
        self.assertIn("refusing to delete foreign executable", combined)
        self.assertTrue(self.link.is_symlink(), "a foreign link was removed")


if __name__ == "__main__":
    unittest.main()
