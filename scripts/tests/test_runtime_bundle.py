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
        # An isolated HOME. Nothing in these tests may resolve into the
        # real ~/.mpm, and the driver inherits this one instead.
        self.home = self._tmp / "home"
        self.home.mkdir()

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
                HOME="{self.home}"
                export DATA_ROOT HOME
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


class TestLayoutPathSemantics(BundleFixture):
    """The versioned layout has ONE root.

    The stable paths are written with the RELATIVE target
    "current/<name>", so they resolve inside the install prefix, while
    the active pointer lives at <data root>/current. When the prefix
    and the data root are different directories the conversion created
    a "current/<name>" symlink that resolved against a `current` that
    did not exist — a dangling symlink naming no binary and no host
    asset — and reported success.

    The supported semantics are therefore: the install prefix and the
    data root must be the SAME directory. An unsupported combination
    is refused BEFORE any filesystem mutation, so it costs the
    operator nothing.
    """

    def _checkout_style_root(self, root: Path):
        (root / "bin").mkdir(parents=True)
        (root / "bin" / "mpm").write_text("old binary\n")
        (root / "agent_installation").mkdir()
        (root / "agent_installation" / "adapter").mkdir()
        (root / "agent_installation" / "adapter" / "file.txt").write_text("x\n")

    def _install_a_release(self):
        """A release with a real bin/ and agent_installation/, activated."""
        bundle = self.make_bundle()
        self.drive(f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"')
        self.drive(f'rb_activate "{self.data_root}" "abc1234"')
        # The bundle carries the payload the stable paths name.
        for name in ("bin", "agent_installation"):
            target = self.data_root / "releases" / "abc1234" / name
            target.mkdir(parents=True, exist_ok=True)
            (target / "payload.txt").write_text(f"{name}\n")

    # ---- the supported case ----

    def test_the_default_layout_converts_and_the_symlinks_resolve(self):
        # THE POSITIVE CASE. prefix == data_root, the documented layout:
        # the stable paths are siblings of the `current` pointer, so the
        # relative target resolves into the active bundle.
        self._checkout_style_root(self.data_root)
        self._install_a_release()

        r = self.drive(
            f'rb_convert_layout "{self.data_root}" "{self.data_root}" "releases/abc1234"; '
            f'echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

        for name in ("bin", "agent_installation"):
            p = self.data_root / name
            self.assertTrue(p.is_symlink(), f"{name} must be a symlink")
            self.assertEqual(os.readlink(p), f"current/{name}")
            # THE ASSERTION THAT MATTERS: the symlink RESOLVES. A
            # dangling symlink still satisfies is_symlink().
            self.assertTrue(
                (p / "payload.txt").is_file(),
                f"{name} must resolve into the active bundle, not dangle",
            )

    # ---- the unsupported case ----

    def test_a_distinct_prefix_is_refused(self):
        prefix = self._tmp / "prefix"
        prefix.mkdir()
        self._checkout_style_root(prefix)
        self._install_a_release()

        r = self.drive(
            f'rb_convert_layout "{self.data_root}" "{prefix}" "releases/abc1234"; '
            f'echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("SAME directory", r.stderr)
        self.assertIn(str(prefix), r.stderr)
        self.assertIn(str(self.data_root), r.stderr)

    def test_a_refused_conversion_touches_nothing(self):
        # The refusal must happen BEFORE any filesystem mutation: the
        # operator's directory is still there, nothing was moved aside,
        # and no dangling symlink was left behind.
        prefix = self._tmp / "prefix"
        prefix.mkdir()
        self._checkout_style_root(prefix)
        self._install_a_release()

        r = self.drive(
            f'rb_convert_layout "{self.data_root}" "{prefix}" "releases/abc1234"; '
            f'echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)

        for name in ("bin", "agent_installation"):
            p = prefix / name
            self.assertFalse(p.is_symlink(), f"{name} must NOT have become a symlink")
            self.assertTrue(p.is_dir(), f"{name} must still be the operator's directory")
        self.assertEqual(
            list(prefix.glob(".*.legacy.*")),
            [],
            "a refused conversion must not move anything aside",
        )
        self.assertFalse(
            (self.data_root / ".legacy-layout-paths").exists(),
            "a refused conversion must not write a rollback record",
        )

    def test_an_empty_prefix_is_refused(self):
        self._install_a_release()
        r = self.drive(
            f'rb_convert_layout "{self.data_root}" "" "releases/abc1234"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("install prefix", r.stderr)

    def test_an_empty_data_root_is_refused(self):
        self._install_a_release()
        r = self.drive(
            f'rb_convert_layout "" "{self.data_root}" "releases/abc1234"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("data root", r.stderr)

    # ---- the compatibility rule itself ----

    def test_two_paths_that_name_the_same_directory_are_compatible(self):
        # The rule is about the DIRECTORY, not about the spelling: a
        # trailing slash and a symlinked home both name one directory.
        r = self.drive(
            f'rb_layout_paths_compatible "{self.data_root}" "{self.data_root}/"; '
            f'echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

        link = self._tmp / "linked-root"
        os.symlink(self.data_root, link)
        r = self.drive(
            f'rb_layout_paths_compatible "{self.data_root}" "{link}"; echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_two_paths_that_name_different_directories_are_incompatible(self):
        other = self._tmp / "other"
        other.mkdir()
        r = self.drive(
            f'rb_layout_paths_compatible "{self.data_root}" "{other}"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("SAME directory", r.stderr)

    def test_a_path_that_does_not_exist_yet_is_accepted_when_both_name_it(self):
        # A fresh installation's root does not exist yet. That is not an
        # error: the question is whether the two paths name ONE
        # directory, and a path that does not exist yet still names one.
        missing = self._tmp / "not-created-yet"
        r = self.drive(
            f'rb_layout_paths_compatible "{missing}" "{missing}"; echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_a_missing_path_that_names_a_different_directory_is_refused(self):
        missing = self._tmp / "not-created-yet"
        other = self._tmp / "also-not-created-yet"
        r = self.drive(
            f'rb_layout_paths_compatible "{missing}" "{other}"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("SAME directory", r.stderr)


class TestRollbackValidation(BundleFixture):
    """The rollback must fail CLOSED.

    The rollback record is the only statement of what was moved aside,
    so it is read as a statement that can be false:

      * a truncated record must not be read as a shorter list — the
        dropped entry is the operator's directory;
      * a malformed entry must not be acted on — acting on it would move
        an arbitrary path into the install prefix;
      * a missing record must not hide conversion material that is
        still sitting in the prefix;
      * a failed revert must not consume the record — the record is the
        operator's evidence.

    And above all: a rollback that restored nothing must never be
    reported as a successful recovery.
    """

    def _converted_state(self, names=("bin", "agent_installation"), record=None):
        """The state rb_convert_layout leaves behind: each named directory
        moved aside to <prefix>/.<name>.legacy.<pid>, a `current/<name>`
        symlink in its place, and the rollback record naming the moved
        directories. `record` overrides the record's contents."""
        prefix = self.data_root
        for name in names:
            legacy = prefix / f".{name}.legacy.4242"
            legacy.mkdir(parents=True)
            (legacy / "payload.txt").write_text(f"{name} payload\n")
            os.symlink(f"current/{name}", prefix / name)
        record_path = prefix / ".legacy-layout-paths"
        if record is None:
            record_path.write_text(
                "".join(f"{prefix}/.{n}.legacy.4242\n" for n in names)
            )
        else:
            record_path.write_text(record)
        return prefix

    # ---- the valid record ----

    def test_a_valid_record_restores_every_directory(self):
        self._converted_state()
        r = self.drive(f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"')
        self.assertIn("rc=0", r.stdout, r.stderr)

        for name in ("bin", "agent_installation"):
            p = self.data_root / name
            self.assertFalse(p.is_symlink(), f"{name} must be a real directory again")
            self.assertTrue((p / "payload.txt").is_file(), f"{name} must be restored")
        self.assertEqual(list(self.data_root.glob(".*.legacy.*")), [])

    def test_a_successful_revert_consumes_the_record(self):
        # The record is consumed so a second revert does not try to
        # restore the same directories again.
        self._converted_state(names=("bin",))
        self.drive(f'rb_revert_layout "{self.data_root}" "{self.data_root}"')
        self.assertEqual(
            (self.data_root / ".legacy-layout-paths").read_text(), ""
        )

        r = self.drive(f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"')
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_an_empty_record_is_a_no_op(self):
        (self.data_root / ".legacy-layout-paths").write_text("")
        r = self.drive(f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"')
        self.assertIn("rc=0", r.stdout, r.stderr)

    # ---- the truncated record ----

    def test_a_truncated_record_still_restores_its_last_entry(self):
        # THE REGRESSION. A record whose last line has no trailing
        # newline is TRUNCATED. `read` alone drops that line, so the
        # rollback used to restore nothing and report success.
        prefix = self.data_root
        legacy = prefix / ".bin.legacy.4242"
        legacy.mkdir(parents=True)
        (legacy / "payload.txt").write_text("bin payload\n")
        os.symlink("current/bin", prefix / "bin")
        (prefix / ".legacy-layout-paths").write_text(f"{legacy}")  # no newline

        r = self.drive(f'rb_revert_layout "{prefix}" "{prefix}"; echo "rc=$?"')
        self.assertIn("rc=0", r.stdout, r.stderr)
        self.assertFalse((prefix / "bin").is_symlink(), "bin must be restored")
        self.assertTrue((prefix / "bin" / "payload.txt").is_file())
        self.assertEqual(list(prefix.glob(".*.legacy.*")), [])

    # ---- the malformed record ----

    def test_a_malformed_record_is_refused_and_nothing_is_moved(self):
        # A record naming an arbitrary path must NOT act on it: acting
        # on it would MOVE that path into the install prefix.
        victim = self._tmp / "victim"
        victim.mkdir()
        (victim / "precious.txt").write_text("precious\n")
        (self.data_root / ".legacy-layout-paths").write_text(f"{victim}\n")

        r = self.drive(
            f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn("malformed", r.stderr)
        self.assertIn(str(victim), r.stderr)

        self.assertTrue((victim / "precious.txt").is_file(), "the path must not be moved")
        self.assertFalse(
            (self.data_root / "victim").exists(),
            "an arbitrary path must not be moved into the install prefix",
        )

    def test_a_malformed_record_keeps_its_evidence(self):
        victim = self._tmp / "victim"
        victim.mkdir()
        record = self.data_root / ".legacy-layout-paths"
        record.write_text(f"{victim}\n")
        self.drive(f'rb_revert_layout "{self.data_root}" "{self.data_root}"')
        self.assertIn(
            str(victim),
            record.read_text(),
            "a failed revert must leave the operator their evidence",
        )

    def test_a_record_naming_a_path_outside_the_prefix_is_refused(self):
        # The shape is right but the location is wrong: a preserved
        # directory lives INSIDE the install prefix.
        outside = self._tmp / "outside"
        outside.mkdir()
        (outside / ".bin.legacy.4242").mkdir()
        (self.data_root / ".legacy-layout-paths").write_text(
            f"{outside}/.bin.legacy.4242\n"
        )
        r = self.drive(
            f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertTrue((outside / ".bin.legacy.4242").is_dir(), "must not be moved")

    # ---- the missing record ----

    def test_a_missing_record_with_nothing_to_restore_is_a_no_op(self):
        # The fresh-installation case: there was no conversion, so there
        # is nothing to undo. This must keep succeeding.
        r = self.drive(
            f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)
        self.assertFalse(
            (self.data_root / ".legacy-layout-paths").exists(),
            "a rollback with nothing to undo must not leave a record behind",
        )

    def test_a_partially_completed_conversion_is_not_a_successful_no_op(self):
        # The conversion moved a directory aside and was interrupted
        # before it could record it. A missing record must not hide
        # material that is still sitting in the prefix.
        prefix = self.data_root
        legacy = prefix / ".bin.legacy.4242"
        legacy.mkdir(parents=True)
        (legacy / "payload.txt").write_text("bin payload\n")
        os.symlink("current/bin", prefix / "bin")
        # NO record was written.

        r = self.drive(f'rb_revert_layout "{prefix}" "{prefix}"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn(str(legacy), r.stderr)
        self.assertIn("restore it by hand", r.stderr)
        self.assertTrue(legacy.is_dir(), "the material must still be there to restore")

    def test_an_unrecorded_directory_is_named_in_the_record(self):
        # The operator is pointed at the record when a rollback does not
        # complete, so the record must name what is still pending.
        prefix = self.data_root
        legacy = prefix / ".bin.legacy.4242"
        legacy.mkdir(parents=True)
        self.drive(f'rb_revert_layout "{prefix}" "{prefix}"')
        record = prefix / ".legacy-layout-paths"
        self.assertTrue(record.exists(), "the pending material must be recorded")
        self.assertIn(str(legacy), record.read_text())

    # ---- a failed revert ----

    def test_a_failed_revert_preserves_the_record(self):
        # The installer tells the operator to read the record when a
        # rollback does not complete. A failed revert must not empty it.
        record = self.data_root / ".legacy-layout-paths"
        missing = self.data_root / ".bin.legacy.4242"
        record.write_text(f"{missing}\n")

        r = self.drive(
            f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertIn(str(missing), record.read_text())
        self.assertIn("missing", r.stderr)

    def test_a_failed_revert_does_not_nest_the_directory(self):
        # `mv src dst` where dst is an existing directory moves src INTO
        # it. The operator's directory must not end up one level deeper
        # with the rollback reported as a success.
        prefix = self.data_root
        legacy = prefix / ".bin.legacy.4242"
        legacy.mkdir(parents=True)
        (legacy / "payload.txt").write_text("bin payload\n")
        # Something real is sitting where the conversion put its symlink.
        (prefix / "bin").mkdir()
        (prefix / "bin" / "operator.txt").write_text("operator\n")
        (prefix / ".legacy-layout-paths").write_text(f"{legacy}\n")

        r = self.drive(f'rb_revert_layout "{prefix}" "{prefix}"; echo "rc=$?"')
        self.assertIn("rc=1", r.stdout, r.stderr)
        self.assertTrue(
            (prefix / "bin" / "operator.txt").is_file(),
            "the operator's directory must not be overwritten",
        )
        self.assertFalse(
            (prefix / "bin" / ".bin.legacy.4242").exists(),
            "the preserved directory must not be nested inside the destination",
        )

    # ---- the round trip ----

    def test_a_conversion_and_its_revert_round_trip(self):
        # The behaviour that already worked must still work.
        (self.data_root / "bin").mkdir()
        (self.data_root / "bin" / "mpm").write_text("old binary\n")
        (self.data_root / "agent_installation").mkdir()
        (self.data_root / "agent_installation" / "adapter").mkdir()
        (self.data_root / "agent_installation" / "adapter" / "file.txt").write_text("x\n")

        bundle = self.make_bundle()
        self.drive(f'rb_install_release "{bundle}" "{self.data_root}" "abc1234"')
        self.drive(f'rb_activate "{self.data_root}" "abc1234"')

        r = self.drive(
            f'rb_convert_layout "{self.data_root}" "{self.data_root}" "releases/abc1234"; '
            f'echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

        r = self.drive(
            f'rb_revert_layout "{self.data_root}" "{self.data_root}"; echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stderr)
        self.assertTrue((self.data_root / "bin" / "mpm").is_file())
        self.assertTrue(
            (self.data_root / "agent_installation" / "adapter" / "file.txt").is_file()
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

    def test_the_rehearsal_reads_the_same_manifest(self):
        # The rehearsal used to carry a THIRD hard-coded copy of the
        # payload allowlist — the adapter's own, the installer's, and
        # the rehearsal's. Two were guarded by a contract test; the
        # third was not, and it went stale the moment the data manifest
        # was introduced. A copy that nothing checks is a copy that
        # drifts.
        #
        # The rehearsal must READ the manifest, and must not name a
        # payload file of its own.
        build_bundle = (REPO_ROOT / "scripts" / "rehearsal" / "build-bundle.sh").read_text()
        self.assertIn(
            "runtime_payloads.json",
            build_bundle,
            "the rehearsal must read the runtime payload manifest, not carry its own copy",
        )
        self.assertIn("rb_payload_records", build_bundle)

        # No hard-coded payload allowlist: the dist/ file list belongs
        # to the manifest alone.
        self.assertNotIn(
            "dist/index.v2.js",
            build_bundle,
            "the rehearsal hard-codes a payload file list; it must read the manifest",
        )


class TestRehearsalPrerequisites(unittest.TestCase):
    """The rehearsal must refuse a missing prerequisite, not skip it.

    The payload's compiled artefacts are build output, and they are not
    in a Git archive. The rehearsal compiles them with the adapter's
    OWN pinned toolchain.

    The old form guarded the whole step with "if node_modules exists".
    On a fresh checkout that directory does not exist, so the payload
    was silently NOT compiled — and the rehearsal failed much later,
    in the bundle assembly, with "manifest entry not found in source:
    dist/index.v2.js". That message names a missing artefact; the
    actual missing thing was a prerequisite three steps away.
    """

    SETUP = REPO_ROOT / "scripts" / "rehearsal" / "setup.sh"

    def test_a_missing_compiler_is_a_refusal(self):
        text = self.SETUP.read_text()
        self.assertIn(
            "node_modules/.bin/tsc",
            text,
            "the rehearsal must check for the adapter's own pinned compiler",
        )
        self.assertIn(
            "npm ci",
            text,
            "the refusal must name the canonical remedy",
        )

    def test_the_payload_step_is_not_conditional(self):
        text = self.SETUP.read_text()
        # The old guard: the whole step skipped when node_modules was
        # absent.
        self.assertNotIn(
            'if [ -d "$REPO/agent_installation/mpm-opencode/node_modules" ]',
            text,
            "the payload step must not be skipped when the dependencies are absent",
        )
        # And the dependencies are LINKED, not copied: 89 MB of
        # third-party packages, of which the rehearsal needs only the
        # compiler.
        self.assertIn("ln -sfn", text)
        self.assertNotIn("cp -a \"$REPO/agent_installation/mpm-opencode/node_modules\"", text)


if __name__ == "__main__":
    unittest.main()
