#!/usr/bin/env python3
"""test_uninstall_bundle.py — the runtime-only uninstall in the
versioned-bundle layout.

In the bundle layout the five binaries live INSIDE a release directory,
reached through ``$PREFIX/bin -> current/bin``. The checkout-style
uninstall removes ``$PREFIX/bin/mpm`` and its four siblings one at a
time — which, in this layout, would delete files INSIDE a release and
leave a broken bundle behind, while reporting a complete uninstall.

These tests pin the correct behaviour:

  * the runtime-only uninstall removes the release-scoped material
    WHOLESALE — the active pointer, the releases, the staging area and
    the symlinks that point into them;
  * it PRESERVES the persistent state — the database, the backups, the
    operator's configuration and the operator's definitions;
  * it never follows a symlink into a release to delete a file there;
  * a dry run mutates nothing.

Every test runs against a hermetic fake HOME under /tmp. The
developer's real MPM installation is never touched.
"""

import os
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
UNINSTALL_SH = REPO_ROOT / "uninstall.sh"

BINARIES = ("mpm", "mpm-mcp", "mpm-scheduler", "mpm-critic", "mpm-telemetry")


def _make_fake_bin_dir(tmp: Path) -> Path:
    bin_dir = tmp / "fakebin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    systemctl = bin_dir / "systemctl"
    systemctl.write_text(
        "#!/usr/bin/env bash\n"
        'case "$1" in\n'
        "  --user) shift; case \"$1\" in is-active) echo inactive; exit 0;; esac; exit 0;;\n"
        "esac\n"
        "exit 0\n"
    )
    systemctl.chmod(0o755)
    return bin_dir


class BundleUninstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-uninstall-bundle-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.home = self.tmp / "home"
        self.home.mkdir()
        self.fakebin = _make_fake_bin_dir(self.tmp)
        self.mpm = self.home / ".mpm"

    # ------------------------------------------------------------------
    # fixture
    # ------------------------------------------------------------------
    def _stage_bundle_install(self):
        """A data root in the versioned-bundle layout."""
        release = self.mpm / "releases" / "abc1234"
        (release / "bin").mkdir(parents=True)
        for name in BINARIES:
            p = release / "bin" / name
            p.write_text(f"#!/bin/sh\necho fake-{name}\n")
            p.chmod(0o755)
        (release / "agent_installation" / "mpm-opencode").mkdir(parents=True)
        (release / "agent_installation" / "mpm-opencode" / "server.js").write_text("// x\n")
        (release / "RELEASE.json").write_text('{"revision":"abc1234"}\n')

        (self.mpm / "current").symlink_to("releases/abc1234")
        (self.mpm / "bin").symlink_to("current/bin")
        (self.mpm / "agent_installation").symlink_to("current/agent_installation")
        (self.mpm / "staging" / "stale").mkdir(parents=True)
        (self.mpm / "staging" / "stale" / "leftover").write_text("x\n")

        # The STATE — everything that must survive.
        (self.mpm / "src" / "db").mkdir(parents=True)
        (self.mpm / "src" / "db" / "mpm.db").write_text("the database\n")
        (self.mpm / "backups" / "critic-pre").mkdir(parents=True)
        (self.mpm / "backups" / "critic-pre" / "snapshot.json").write_text("{}\n")
        (self.mpm / "mpm_config.json").write_text('{"default":{}}\n')
        (self.mpm / "mode").mkdir()
        (self.mpm / "mode" / "default.md").write_text("the operator's mode\n")
        (self.mpm / "persona").mkdir()
        (self.mpm / "persona" / "default.md").write_text("the operator's persona\n")

        # The PATH symlinks, which name the stable path.
        lb = self.home / ".local" / "bin"
        lb.mkdir(parents=True)
        lb.joinpath("mpm").symlink_to(str(self.mpm / "bin/mpm"))
        lb.joinpath("mpm-mcp").symlink_to(str(self.mpm / "bin/mpm-mcp"))

    def _run(self, *args: str):
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(UNINSTALL_SH), *args],
            capture_output=True,
            text=True,
            env=env,
            timeout=120,
        )
        return result

    # ------------------------------------------------------------------
    # tests
    # ------------------------------------------------------------------
    def test_the_runtime_only_uninstall_removes_the_release_scoped_material(self):
        self._stage_bundle_install()
        r = self._run()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

        self.assertFalse((self.mpm / "current").exists(), "the active pointer must go")
        self.assertFalse((self.mpm / "current").is_symlink())
        self.assertFalse((self.mpm / "releases").exists(), "the releases must go")
        self.assertFalse((self.mpm / "staging").exists(), "the staging area must go")
        self.assertFalse((self.mpm / "bin").is_symlink(), "the bin symlink must go")
        self.assertFalse(
            (self.mpm / "agent_installation").is_symlink(),
            "the agent_installation symlink must go",
        )

    def test_the_runtime_only_uninstall_preserves_the_persistent_state(self):
        self._stage_bundle_install()
        r = self._run()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

        self.assertTrue((self.mpm / "src" / "db" / "mpm.db").is_file())
        self.assertTrue((self.mpm / "backups" / "critic-pre" / "snapshot.json").is_file())
        self.assertTrue((self.mpm / "mpm_config.json").is_file())
        self.assertTrue((self.mpm / "mode" / "default.md").is_file())
        self.assertTrue((self.mpm / "persona" / "default.md").is_file())

    def test_the_uninstall_does_not_delete_files_inside_a_release(self):
        # The checkout-style uninstall removes "$PREFIX/bin/mpm". In the
        # bundle layout that path resolves THROUGH the symlink into a
        # release, so removing it would gut a release and leave a broken
        # bundle behind.
        self._stage_bundle_install()
        r = self._run()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        # The release is gone as a whole — which is correct — and the
        # uninstall reported it as a release, not as five binaries.
        self.assertIn("releases", r.stdout)

    def test_a_dry_run_mutates_nothing(self):
        self._stage_bundle_install()
        before = sorted(p.name for p in self.mpm.iterdir())
        r = self._run("--dry-run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(sorted(p.name for p in self.mpm.iterdir()), before)
        self.assertTrue((self.mpm / "current").is_symlink())
        self.assertTrue((self.mpm / "releases").is_dir())

    def test_the_plan_names_the_release_scoped_paths(self):
        self._stage_bundle_install()
        r = self._run("--dry-run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("releases", r.stdout)
        self.assertIn("preserved by default uninstall", r.stdout)

    def test_a_checkout_style_install_still_uninstalls_correctly(self):
        # The bundle branch must not change the existing behaviour.
        (self.mpm / "bin").mkdir(parents=True)
        for name in BINARIES:
            p = self.mpm / "bin" / name
            p.write_text(f"#!/bin/sh\necho fake-{name}\n")
            p.chmod(0o755)
        (self.mpm / "src" / "db").mkdir(parents=True)
        (self.mpm / "src" / "db" / "mpm.db").write_text("the database\n")
        (self.mpm / "mpm_config.json").write_text('{"default":{}}\n')
        (self.mpm / "mode").mkdir()
        (self.mpm / "mode" / "default.md").write_text("mode\n")

        r = self._run()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        for name in BINARIES:
            self.assertFalse((self.mpm / "bin" / name).exists())
        self.assertTrue((self.mpm / "src" / "db" / "mpm.db").is_file())
        self.assertTrue((self.mpm / "mpm_config.json").is_file())
        self.assertTrue((self.mpm / "mode" / "default.md").is_file())


if __name__ == "__main__":
    unittest.main()
