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
2. **The installer never clobbers an unrelated executable — or an
   unrelated symlink.** The mpm/mpm-mcp links are created with ``rm -f``
   first, which is fine for binaries the installer owns. An operator may
   already have their own ``mpm-update`` on PATH as a real file *or* as
   a symlink to a different executable; neither is the installer's to
   replace. Ownership is positively established: only a link that
   already resolves to the data-root updater is treated as ours (and is
   then left untouched, not recreated). Everything else is preserved
   byte-for-byte with a warning naming the direct executable path.

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

    def test_an_owned_link_is_left_intact_not_repointed(self):
        # An already-owned link must survive a second install byte-for-byte:
        # same target string AND same link inode (no ln call at all).
        self.make_recovery("mpm-update", "recovery")
        self.drive()
        self.assertEqual(self.link_target(), str(self.recovery))
        ino_before = os.lstat(self.link).st_ino

        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.link_target(), str(self.recovery))
        self.assertEqual(os.lstat(self.link).st_ino, ino_before,
                         "an owned link was recreated instead of left intact")


class TestForeignSymlinksArePreserved(UpdaterPathFixture):
    """A symlink's existence is not evidence of MPM ownership.

    Only a link that resolves to the standalone updater itself may be
    kept (untouched); every other symlink — foreign target, dangling
    target, directory target — must survive the install byte-for-byte,
    with a warning pointing at the direct executable path.
    """

    def _foreign_exe(self, body: str = "FOREIGN TOOL") -> Path:
        other = self._tmp / "other-tool"
        other.mkdir(exist_ok=True)
        exe = other / "mpm-update"
        exe.write_text(f"#!/bin/sh\necho '{body}'\n")
        exe.chmod(0o755)
        return exe

    def test_a_foreign_symlink_keeps_its_target(self):
        self.make_recovery("mpm-update", "recovery")
        exe = self._foreign_exe()
        self.link.parent.mkdir(parents=True, exist_ok=True)
        self.link.symlink_to(exe)

        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(os.readlink(self.link), str(exe),
                         "the installer hijacked an operator-owned symlink")
        self.assertIn("FOREIGN TOOL", exe.read_text())
        out = subprocess.run([str(self.link)], capture_output=True, text=True, timeout=30)
        self.assertIn("FOREIGN TOOL", out.stdout,
                      "the link no longer resolves to the foreign executable")
        combined = r.stdout + r.stderr
        self.assertIn("leaving it untouched", combined)
        self.assertIn(str(self.recovery), combined,
                      "the warning must point at the direct executable path")

    def test_a_dangling_foreign_symlink_survives(self):
        self.make_recovery("mpm-update", "recovery")
        self.link.parent.mkdir(parents=True, exist_ok=True)
        dangling = self._tmp / "gone" / "mpm-update"
        self.link.symlink_to(dangling)

        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertTrue(self.link.is_symlink(), "a dangling link was deleted")
        self.assertEqual(os.readlink(self.link), str(dangling))
        self.assertFalse(dangling.exists(),
                         "the installer created a file at the dangling target")
        self.assertIn("leaving it untouched", r.stdout + r.stderr)

    def test_a_symlink_to_a_directory_is_preserved_and_never_entered(self):
        self.make_recovery("mpm-update", "recovery")
        target_dir = self._tmp / "operator-dir"
        target_dir.mkdir()
        sentinel = target_dir / "sentinel.txt"
        sentinel.write_text("operator data\n")
        self.link.parent.mkdir(parents=True, exist_ok=True)
        self.link.symlink_to(target_dir)

        before = sorted(p.name for p in target_dir.iterdir())
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertTrue(self.link.is_symlink(), "a directory symlink was replaced")
        self.assertEqual(os.readlink(self.link), str(target_dir))
        self.assertEqual(sorted(p.name for p in target_dir.iterdir()), before,
                         "the installer wrote inside a directory behind the link")
        self.assertEqual(sentinel.read_text(), "operator data\n")
        self.assertIn("leaving it untouched", r.stdout + r.stderr)


class TestNeverClobbersForeignFiles(UpdaterPathFixture):
    def test_a_real_file_at_the_destination_is_not_overwritten(self):
        self.make_recovery("mpm-update", "recovery")
        self.link.parent.mkdir(parents=True, exist_ok=True)
        body = b"#!/bin/sh\necho 'OPERATOR OWNED'\n"
        self.link.write_bytes(body)
        self.link.chmod(0o755)

        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(self.link.is_symlink(),
                         "the installer replaced an operator-owned file with a symlink")
        self.assertEqual(self.link.read_bytes(), body,
                         "an operator-owned executable was modified byte-for-byte")
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

        # A second install must not disturb the owned link either: same
        # target string, same link inode (untouched, not recreated).
        ino_before = os.lstat(self.link).st_ino
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.link_target(), str(self.recovery))
        self.assertEqual(os.lstat(self.link).st_ino, ino_before,
                         "a reinstall recreated an owned link instead of leaving it intact")


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
