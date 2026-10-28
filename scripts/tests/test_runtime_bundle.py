#!/usr/bin/env python3
"""test_runtime_bundle.py — the versioned runtime bundle library.

Pins the contract of scripts/lib/runtime_bundle.sh:

  * a bundle is VALIDATED before it can become the active runtime, and
    the validation refuses the four ways a bundle can be wrong: an
    incomplete binary set, Git metadata, PERSISTENT STATE, and an
    identity that disagrees with the binaries inside;
  * a release directory is created by an atomic rename, and an existing
    release is never silently replaced;
  * the active pointer is swapped atomically, and the previous pointer
    can be restored;
  * the layout conversion turns real directories into symlinks into the
    active bundle and PRESERVES the originals as rollback material.

The library is driven as a library in a child bash — the same
source-the-real-code technique scripts/tests/test_binary_transaction.py
uses — so these tests exercise the shipped functions rather than a
reimplementation of them.

Hermeticity: every root is a fresh mktemp directory and nothing
resolves into the real ~/.mpm.
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
LIB = REPO_ROOT / "scripts" / "lib" / "runtime_bundle.sh"

BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")
OPENCODE_MANIFEST = (
    "server.js",
    "dist/index.v2.js",
    "dist/wake-context.js",
    "dist/event-envelope.js",
    "dist/event-mapper.js",
    "dist/event-subscriber.js",
    "dist/workspace.js",
    "dist/resolve-mpm-binary.js",
    "package.json",
)


def sh(script: str, **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["bash", "-c", script],
        capture_output=True,
        text=True,
        timeout=120,
        **kwargs,
    )


class BundleFixture(unittest.TestCase):
    """Shared scaffolding: build a bundle, drive the library, assert."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-bundle-test-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)
        self.data_root = self._tmp / "data"
        self.data_root.mkdir()

    # ------------------------------------------------------------------
    # fixture
    # ------------------------------------------------------------------
    def make_bundle(self, name: str = "candidate", rev: str = "abc1234",
                    commit: str = "a" * 40, binaries=BINARIES) -> Path:
        """Build a well-formed candidate bundle."""
        bundle = self._tmp / name
        (bundle / "bin").mkdir(parents=True, exist_ok=True)
        for b in binaries:
            p = bundle / "bin" / b
            p.write_text(
                "#!/usr/bin/env bash\n"
                "if [ \"${1:-}\" = '--version' ]; then\n"
                f"  echo 'MPM 0.1.0-dev+g{commit[:8]}'\n"
                f"  echo '  commit:  {commit}'\n"
                "fi\n"
                f"echo '# {b} of {commit}'\n"
            )
            p.chmod(0o755)
        (bundle / "RELEASE.json").write_text(
            '{\n'
            f'  "revision": "{rev}",\n'
            f'  "commit": "{commit}",\n'
            '  "short_commit": "aaaaaaaa",\n'
            '  "build_epoch": 1793000000,\n'
            '  "version": "0.1.0-dev",\n'
            '  "built_at": "2026-10-28T00:00:00Z",\n'
            '  "bundle_layout": "versioned-bundle/1"\n'
            '}\n'
        )
        return bundle

    # ------------------------------------------------------------------
    # driver
    # ------------------------------------------------------------------
    def drive(self, script: str) -> subprocess.CompletedProcess:
        """Run `script` with the library sourced and the fixture paths
        exported. The script is expected to `exit` with the status the
        test wants to assert on."""
        driver = self._tmp / "drive.sh"
        driver.write_text(
            textwrap.dedent(f"""\
                set -uo pipefail
                . "{LIB}"
                DATA_ROOT="{self.data_root}"
                export DATA_ROOT
                {script}
            """)
        )
        driver.chmod(0o755)
        return subprocess.run(
            ["bash", str(driver)], capture_output=True, text=True, timeout=120
        )


class TestBundleValidation(BundleFixture):
    """A bundle must be proven before it can become the active runtime."""

    def test_a_well_formed_bundle_validates(self):
        bundle = self.make_bundle()
        r = self.drive(f'rb_validate_bundle "{bundle}" "abc1234"; echo "rc=$?"')
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_a_missing_binary_is_refused(self):
        bundle = self.make_bundle()
        (bundle / "bin" / "mpm-telemetry").unlink()
        r = self.drive(f'rb_validate_bundle "{bundle}"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("mpm-telemetry", r.stderr)

    def test_a_non_executable_binary_is_refused(self):
        bundle = self.make_bundle()
        (bundle / "bin" / "mpm-critic").chmod(0o644)
        r = self.drive(f'rb_validate_bundle "{bundle}"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("not executable", r.stderr)

    def test_a_directory_where_a_binary_belongs_is_refused(self):
        bundle = self.make_bundle()
        (bundle / "bin" / "mpm").unlink()
        (bundle / "bin" / "mpm").mkdir()
        r = self.drive(f'rb_validate_bundle "{bundle}"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("not a regular file", r.stderr)

    def test_a_bundle_carrying_git_metadata_is_refused(self):
        bundle = self.make_bundle()
        (bundle / ".git").mkdir()
        r = self.drive(f'rb_validate_bundle "{bundle}"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("Git-free", r.stderr)

    def test_a_bundle_carrying_state_is_refused(self):
        # THE POINT. A bundle that carried the operator's data would make
        # every version switch a chance to replace it.
        for state in ("src/db", "backups", "mpm_config.json", "mode", "persona"):
            with self.subTest(state=state):
                bundle = self.make_bundle(name=f"state-{state.replace('/', '-')}")
                target = bundle / state
                if "." in state.split("/")[-1]:
                    target.write_text("{}")
                else:
                    target.mkdir(parents=True, exist_ok=True)
                r = self.drive(f'rb_validate_bundle "{bundle}"; echo "rc=$?"')
                self.assertIn("rc=1", r.stdout, r.stderr)
                self.assertIn("persistent state", r.stderr)

    def test_a_bundle_without_release_json_is_refused(self):
        bundle = self.make_bundle()
        (bundle / "RELEASE.json").unlink()
        r = self.drive(f'rb_validate_bundle "{bundle}"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("RELEASE.json", r.stderr)

    def test_a_revision_mismatch_is_refused(self):
        bundle = self.make_bundle(rev="abc1234")
        r = self.drive(f'rb_validate_bundle "{bundle}" "other"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("expected", r.stderr)

    def test_an_identity_that_disagrees_with_the_binaries_is_refused(self):
        # A bundle that is a MIXTURE of two builds must never be
        # installed. The validator asks the binary itself what it is.
        bundle = self.make_bundle(rev="abc1234", commit="b" * 40)
        fake = bundle / "bin" / "mpm"
        fake.write_text(
            "#!/usr/bin/env bash\n"
            "if [ \"$1\" = '--version' ]; then\n"
            f"  echo '  commit: {'c' * 40}'\n"
            "fi\n"
        )
        fake.chmod(0o755)
        r = self.drive(f'rb_validate_bundle "{bundle}" "abc1234"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("mixture", r.stderr)


class TestBundleInstall(BundleFixture):
    """A release is created by an atomic rename, and never replaced."""

    def test_a_release_is_installed(self):
        bundle = self.make_bundle()
        r = self.drive(
            f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"; echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)
        self.assertTrue((self.data_root / "releases" / "abc1234").is_dir())
        self.assertFalse(bundle.exists(), "the candidate must be MOVED, not copied")

    def test_an_existing_release_is_never_replaced(self):
        bundle = self.make_bundle()
        self.drive(f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"')
        marker = self.data_root / "releases" / "abc1234" / "MARKER"
        marker.write_text("the operator put this here\n")

        second = self.make_bundle(name="second")
        r = self.drive(
            f'rb_install_release "{second}" "{self.data_root}" "abc1234"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("immutable", r.stderr)
        self.assertTrue(marker.exists(), "the existing release must be untouched")


class TestActivePointer(BundleFixture):
    """`current` is a symlink, and promotion is a rename(2) over it."""

    def test_the_pointer_is_swapped_and_read_back(self):
        bundle = self.make_bundle()
        self.drive(f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"')
        r = self.drive(
            f'rb_activate "{self.data_root}" "abc1234"; '
            f'rb_current_target "{self.data_root}"; echo "rc=$?"'
        )
        self.assertIn("releases/abc1234", r.stdout)
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_activating_a_release_that_does_not_exist_is_refused(self):
        r = self.drive(f'rb_activate "{self.data_root}" "nope"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("no such release", r.stderr)

    def test_the_pointer_can_be_restored(self):
        first = self.make_bundle(name="first")
        self.drive(f'rb_install_release "{first}" "{self.data_root}" "rev1"')
        self.drive(f'rb_activate "{self.data_root}" "rev1"')

        second = self.make_bundle(name="second", rev="rev2")
        self.drive(f'rb_install_release "{second}" "{self.data_root}" "rev2"')
        self.drive(f'rb_activate "{self.data_root}" "rev2"')

        r = self.drive(
            f'rb_restore_pointer "{self.data_root}" "releases/rev1"; '
            f'rb_current_target "{self.data_root}"; echo "rc=$?"'
        )
        self.assertIn("releases/rev1", r.stdout)

    def test_the_pointer_swap_leaves_no_temporary_behind(self):
        bundle = self.make_bundle()
        self.drive(f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"')
        self.drive(f'rb_activate "{self.data_root}" "abc1234"')
        leftovers = list(self.data_root.glob(".current.swap.*"))
        self.assertEqual(leftovers, [], f"a swap temporary was left behind: {leftovers}")


class TestLayoutConversion(BundleFixture):
    """The conversion preserves the operator's directories."""

    def _make_checkout_style_data_root(self):
        (self.data_root / "bin").mkdir()
        (self.data_root / "bin" / "mpm").write_text("old binary\n")
        (self.data_root / "agent_installation").mkdir()
        (self.data_root / "agent_installation" / "adapter").mkdir()
        (self.data_root / "agent_installation" / "adapter" / "file.txt").write_text("x\n")
        # The state must survive the conversion untouched.
        (self.data_root / "src" / "db").mkdir(parents=True)
        (self.data_root / "src" / "db" / "mpm.db").write_text("state\n")
        (self.data_root / "mpm_config.json").write_text("{}\n")
        (self.data_root / "mode").mkdir()
        (self.data_root / "mode" / "default.md").write_text("operator\n")

    def test_real_directories_become_symlinks_and_are_preserved(self):
        self._make_checkout_style_data_root()
        bundle = self.make_bundle()
        self.drive(f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"')
        self.drive(f'rb_activate "{self.data_root}" "abc1234"')

        r = self.drive(
            f'rb_convert_layout "{self.data_root}" "{self.data_root}" "releases/abc1234"; '
            f'echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

        for name in ("bin", "agent_installation"):
            p = self.data_root / name
            self.assertTrue(p.is_symlink(), f"{name} must be a symlink")
            self.assertEqual(os.readlink(p), f"current/{name}")

        # The originals are preserved, not deleted.
        legacy = list(self.data_root.glob(".bin.legacy.*"))
        self.assertEqual(len(legacy), 1, "the previous bin/ must be preserved")
        self.assertTrue((legacy[0] / "mpm").is_file())
        legacy_ai = list(self.data_root.glob(".agent_installation.legacy.*"))
        self.assertEqual(len(legacy_ai), 1, "the previous agent_installation/ must be preserved")
        self.assertTrue((legacy_ai[0] / "adapter" / "file.txt").is_file())

        # And the record points at them.
        recorded = (self.data_root / ".legacy-layout-paths").read_text().split()
        self.assertEqual(len(recorded), 2)

        # The state is untouched.
        self.assertTrue((self.data_root / "src" / "db" / "mpm.db").is_file())
        self.assertTrue((self.data_root / "mpm_config.json").is_file())
        self.assertTrue((self.data_root / "mode" / "default.md").is_file())

    def test_conversion_is_idempotent(self):
        self._make_checkout_style_data_root()
        bundle = self.make_bundle()
        self.drive(f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"')
        self.drive(f'rb_activate "{self.data_root}" "abc1234"')
        self.drive(f'rb_convert_layout "{self.data_root}" "{self.data_root}" "releases/abc1234"')
        r = self.drive(
            f'rb_convert_layout "{self.data_root}" "{self.data_root}" "releases/abc1234"; '
            f'echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)
        self.assertEqual(
            len(list(self.data_root.glob(".bin.legacy.*"))),
            1,
            "a second conversion must not move the symlink aside again",
        )


class TestOpenCodeManifestSync(unittest.TestCase):
    """The bundle's payload manifest and the adapter's must agree.

    A payload the adapter installs but the bundle omits is a bundle that
    registers a plugin which loads NOTHING — the silent failure the
    staging validator exists to prevent.
    """

    MANIFEST = REPO_ROOT / "scripts" / "runtime_payloads.json"

    def test_the_manifest_exists_and_declares_the_adapter(self):
        import json

        doc = json.loads(self.MANIFEST.read_text())
        self.assertIn("payloads", doc)
        adapters = [p["adapter"] for p in doc["payloads"]]
        self.assertIn("mpm-opencode", adapters)

    def test_the_two_manifests_agree(self):
        import json

        adapter = REPO_ROOT / "agent_installation" / "mpm-opencode" / "install.sh"
        text = adapter.read_text()
        m = re.search(
            r"OPENCODE_RUNTIME_MANIFEST=\((.*?)\)", text, re.DOTALL
        )
        self.assertIsNotNone(m, "the adapter must define OPENCODE_RUNTIME_MANIFEST")
        adapter_manifest = tuple(
            line.strip() for line in m.group(1).split() if line.strip()
        )
        doc = json.loads(self.MANIFEST.read_text())
        entry = next(p for p in doc["payloads"] if p["adapter"] == "mpm-opencode")
        self.assertEqual(
            set(entry["files"]),
            set(adapter_manifest),
            "the bundle's runtime payload manifest has drifted from the "
            "adapter's; a bundle missing a file the adapter installs would "
            "register a plugin that loads nothing",
        )

    def test_the_library_manifest_matches_the_data_file(self):
        import json

        doc = json.loads(self.MANIFEST.read_text())
        entry = next(p for p in doc["payloads"] if p["adapter"] == "mpm-opencode")
        self.assertEqual(tuple(entry["files"]), OPENCODE_MANIFEST)

    def test_the_root_installer_names_no_adapter(self):
        # The substrate installer is host-agnostic: it must not know
        # about agent hosts. The payload knowledge is DATA.
        install = (REPO_ROOT / "install.sh").read_text()
        self.assertNotIn("mpm-opencode", install)
        self.assertNotIn("runtime_payloads", install)


if __name__ == "__main__":
    unittest.main()
