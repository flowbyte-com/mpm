# test_installer_registration_defects.py — the two installer defects found
# during the aeec40f1 deployment.
#
# WHAT THIS FILE PINS
# --------------------
# Two defects, both found by deploying aeec40f1 to a live runtime, both
# in the path that registers MPM's plugin with the OpenCode host.
#
# ── DEFECT 1: a DUPLICATE PLUGIN REGISTRATION ────────────────────────────
#
# Under the versioned-bundle layout, `~/.mpm/agent_installation` is a
# SYMLINK into the active release:
#
#     ~/.mpm/agent_installation -> current/agent_installation
#                                -> releases/<rev>/agent_installation
#
# The adapter's installer computed its canonical registration with
#
#     CANONICAL_ADAPTER_DIR="$(cd "$staged" && pwd -P)"
#
# and `pwd -P` resolves symlinks. So the registration became the
# REVISION-PINNED physical path
#
#     file:///home/v/.mpm/releases/<rev>/agent_installation/mpm-opencode
#
# while the config ALREADY carried the stable entry
#
#     file:///home/v/.mpm/agent_installation/mpm-opencode
#
# Two entries, two different strings, ONE directory. The dedupe in
# `jsonc_config.py` compares STRINGS, so it did not collapse them, and
# OpenCode would load the same plugin twice.
#
# Two things must hold:
#
#   a. the registration is the STABLE path — the one that rolls with the
#      release, and the one the versioned layout exists to provide;
#   b. a config that already carries BOTH spellings collapses to ONE,
#      because an operator who has already been through the buggy
#      install must not need to edit the file by hand.
#
# ── DEFECT 2: THE WRITER DESTROYS THE FILE'S MODE ───────────────────────
#
# `jsonc_config.py` wrote through a sibling temp file and `os.replace()`.
# `os.replace()` installs the NEW inode, so the original file's mode is
# gone and the replacement takes the default umask's mode instead.
#
# Measured on the live host during the deployment:
#
#     opencode.jsonc   600  ->  664
#
# A file the operator had deliberately restricted was silently widened.
# The mode must be preserved.
#
# EVERY test here runs against a disposable temporary tree. The live
# installation is never touched.
import json
import os
import stat
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))

import jsonc_config  # noqa: E402
from jsonc_config import check_entry, migrate, parse  # noqa: E402


def _parse_entries(text):
    """The plugin entries a config carries, across both key names."""
    data = parse(text)
    out = []
    for key in ("plugins", "plugin"):
        if isinstance(data.get(key), list):
            out.extend(data[key])
    return out


class BundleLayoutRegistrationTests(unittest.TestCase):
    """The registration must be the STABLE path, and there must be ONE."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mpm-reg-")
        self.addCleanup(self._tmp.cleanup)
        self.root = os.path.realpath(self._tmp.name)

        # The versioned-bundle layout, reproduced exactly:
        #   <data root>/agent_installation -> current/agent_installation
        #   <data root>/current -> releases/<rev>
        self.data_root = os.path.join(self.root, ".mpm")
        self.rev = "aeec40f1f6f5edfbd07b1fd962b54de123d996a6"
        self.release_dir = os.path.join(self.data_root, "releases", self.rev)
        self.package = os.path.join(self.release_dir, "agent_installation", "mpm-opencode")
        os.makedirs(self.package)
        # `current` is a relative symlink, as the installer writes it.
        os.symlink("releases/" + self.rev, os.path.join(self.data_root, "current"))
        # `agent_installation` is a relative symlink into the active release.
        os.symlink("current/agent_installation",
                   os.path.join(self.data_root, "agent_installation"))

        self.stable_entry = "file://" + os.path.join(self.data_root, "agent_installation", "mpm-opencode")
        self.revision_entry = "file://" + self.package

    def test_the_two_entries_name_the_same_directory(self):
        """The precondition: they LOOK different but ARE the same dir.

        If this ever stops being true the defect cannot occur, and this
        test would be asserting a fiction. It is asserted rather than
        assumed.
        """
        self.assertNotEqual(self.stable_entry, self.revision_entry,
                            "the two spellings must differ as strings")
        self.assertEqual(
            os.path.realpath(self.stable_entry[len("file://"):]),
            os.path.realpath(self.revision_entry[len("file://"):]),
            "the two spellings must resolve to the same directory — that is "
            "what makes the duplicate a duplicate rather than two plugins",
        )

    def test_a_revision_pinned_entry_alone_migrates_to_the_stable_canonical(self):
        """A config carrying only the revision-pinned entry is repaired.

        This is the state an operator is left in by the buggy install:
        the entry is correct in effect but pinned to a revision, so it
        breaks at the NEXT upgrade.
        """
        config = json.dumps({"plugins": [self.revision_entry]}, indent=2)
        updated, changed, _summary = migrate(config, self.stable_entry)
        self.assertTrue(changed, "the revision-pinned entry must be rewritten")
        self.assertEqual(
            _parse_entries(updated), [self.stable_entry],
            "the registration must become the stable path, which rolls with "
            "the release",
        )

    def test_both_spellings_together_collapse_to_one_registration(self):
        """THE DEFECT: two entries, one directory, must become one entry.

        This is the exact state the live host was left in. The installer
        appended the revision-pinned entry beside the stable one, and the
        string-keyed dedupe did not collapse them.
        """
        config = json.dumps(
            {"plugins": [self.stable_entry, self.revision_entry]}, indent=2)
        updated, changed, _summary = migrate(config, self.stable_entry)
        entries = _parse_entries(updated)
        self.assertEqual(
            entries, [self.stable_entry],
            "two spellings of ONE directory must collapse to ONE registration. "
            f"Got: {entries!r}",
        )
        self.assertNotIn(self.revision_entry, updated,
                         "the revision-pinned spelling must not survive — it "
                         "would load the plugin a second time and would break "
                         "at the next upgrade")

    def test_an_unrelated_plugin_in_the_same_directory_shape_survives(self):
        """The collapse must be by RESOLVED IDENTITY, not by shape.

        A different directory that merely LOOKS similar must not be
        swallowed by the canonical registration.
        """
        other = os.path.join(self.root, "elsewhere", "mpm-opencode")
        os.makedirs(other)
        other_entry = "file://" + other
        config = json.dumps(
            {"plugins": [self.stable_entry, other_entry]}, indent=2)
        updated, changed, _summary = migrate(config, self.stable_entry)
        entries = _parse_entries(updated)
        self.assertEqual(
            entries, [self.stable_entry, other_entry],
            "a DIFFERENT directory must survive even though it shares the "
            "adapter's basename. Got: {entries!r}",
        )

    def test_a_previous_releases_copy_is_normalised_to_the_canonical(self):
        """A registration left behind by an EARLIER release is MPM's too.

        After the next upgrade the revision-pinned path names a
        DIFFERENT directory from the canonical one, so an identity
        comparison alone would leave it behind — and OpenCode would load
        that dead release's plugin alongside the current one.
        """
        old_rev = "3d63244e1e64955db2db243e70e279d370ebf1d6"
        old_package = os.path.join(
            self.data_root, "releases", old_rev, "agent_installation", "mpm-opencode")
        os.makedirs(old_package)
        old_entry = "file://" + old_package

        # It is genuinely a different directory now.
        self.assertNotEqual(
            os.path.realpath(old_package),
            os.path.realpath(self.stable_entry[len("file://"):]),
            "a previous release's directory is a different directory")

        config = json.dumps({"plugins": [old_entry]}, indent=2)
        updated, changed, _summary = migrate(config, self.stable_entry)
        self.assertTrue(changed, "the stale registration must be rewritten")
        self.assertEqual(
            _parse_entries(updated), [self.stable_entry],
            "MPM owns exactly one registration; a previous release's copy of "
            "the plugin is MPM's registration and must be normalised to the "
            "canonical one, not left behind as a second load",
        )

    def test_check_entry_reports_the_duplicate(self):
        """`--check-entry` must not report a healthy config as healthy.

        The check is what the installer consults to decide whether the
        registration is correct. If it counts the revision-pinned entry
        as a duplicate, it must say so.
        """
        config = json.dumps(
            {"plugins": [self.stable_entry, self.revision_entry]}, indent=2)
        self.assertFalse(
            check_entry(config, self.stable_entry),
            "a config carrying two spellings of one directory must NOT pass "
            "the duplicate check",
        )

    def test_check_entry_accepts_the_single_stable_entry(self):
        config = json.dumps({"plugins": [self.stable_entry]}, indent=2)
        self.assertTrue(
            check_entry(config, self.stable_entry),
            "a config carrying exactly the stable registration must pass",
        )


class FileModePreservationTests(unittest.TestCase):
    """The writer must not widen a file the operator restricted."""

    def _write_config(self, path, entries):
        with open(path, "w", encoding="utf-8", newline="") as fh:
            fh.write(json.dumps({"plugins": entries}, indent=2))

    def _mode_of(self, path):
        return stat.S_IMODE(os.stat(path).st_mode)

    def test_a_migration_preserves_the_original_mode(self):
        """THE DEFECT: the mode was widened from 600 to 664.

        Measured on the live host: the operator's `opencode.jsonc` was
        0600 before the install and 0664 after it.
        """
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            self._write_config(path, ["file:///opt/team/linter-plugin"])
            os.chmod(path, 0o600)
            self.assertEqual(self._mode_of(path), 0o600)

            rc = jsonc_config.main(
                ["x", path, "file://" + os.path.join(tmp, "mpm-opencode")])
            self.assertEqual(rc, 0, "the migration must succeed")
            self.assertEqual(
                self._mode_of(path), 0o600,
                "the migration must preserve the file's mode. The writer "
                "installs a NEW inode through os.replace(), so the original "
                "mode has to be copied onto the replacement explicitly.",
            )

    def test_a_migration_preserves_a_group_readable_mode(self):
        """The mode is the operator's choice; 0644 must survive too."""
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            self._write_config(path, ["file:///opt/team/linter-plugin"])
            os.chmod(path, 0o644)
            rc = jsonc_config.main(
                ["x", path, "file://" + os.path.join(tmp, "mpm-opencode")])
            self.assertEqual(rc, 0)
            self.assertEqual(self._mode_of(path), 0o644,
                             "a 0644 config must stay 0644")

    def test_a_no_op_migration_leaves_the_file_alone(self):
        """A config already carrying the canonical entry is not rewritten."""
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            canonical = "file://" + os.path.join(tmp, "mpm-opencode")
            self._write_config(path, [canonical])
            os.chmod(path, 0o600)
            with open(path, "r", encoding="utf-8", newline="") as fh:
                before = fh.read()
            rc = jsonc_config.main(["x", path, canonical])
            self.assertEqual(rc, 0)
            with open(path, "r", encoding="utf-8", newline="") as fh:
                self.assertEqual(fh.read(), before)
            self.assertEqual(self._mode_of(path), 0o600)

    def test_a_config_the_installer_just_created_keeps_its_mode(self):
        """The installer creates the file, then the migrator rewrites it.

        install.sh creates a missing opencode.jsonc with the umask's
        mode and then hands it to the migrator. The rewrite must not
        change the mode the file was just created with.
        """
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            umask = os.umask(0)
            os.umask(umask)
            expected = 0o666 & ~umask
            # What install.sh's `cat > "$OPENCODE_CONFIG"` produces.
            with open(path, "w", encoding="utf-8", newline="") as fh:
                fh.write("{}\n")
            os.chmod(path, expected)
            canonical = "file://" + os.path.join(tmp, "mpm-opencode")
            rc = jsonc_config.main(["x", path, canonical])
            self.assertEqual(rc, 0)
            self.assertEqual(
                self._mode_of(path), expected,
                "the mode the file was created with must survive the rewrite")


if __name__ == "__main__":
    unittest.main(verbosity=2)
