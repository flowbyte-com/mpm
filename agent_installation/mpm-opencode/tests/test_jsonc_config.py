"""test_jsonc_config.py — regression tests for opencode.jsonc MPM registration.

Two defect classes are pinned here.

2026-09-17 (namespace):  install.sh read the user's opencode.jsonc with
`json.loads`, which rejects the comments and trailing commas the `.jsonc`
format exists to hold. The failure was treated as "leave it alone", so a
user with a commented config got a silent no-op while the stale
`opencode-mpm` entry survived.

2026-10-16 (registration shape):  install.sh registered
`file://…/mpm-opencode/dist/index.js` — a FILE url. OpenCode 2 refuses a
file target ("configured plugin path must be a directory"), and after the
host stopped warning on it the entry was silently dropped: the plugin
registry lists zero non-builtin plugins while every command reports
success. The canonical registration is the PACKAGE DIRECTORY
`file://…/mpm-opencode`; the V2 loader resolves it by looking for a
root-level `server.*` / `index.*` entry inside the registered directory
and never consults package.json.

Contract pinned by this file (module level; installer flows live in
test_opencode_install.py):

  migrate
    1.  fresh config, no plugin array           -> adds `plugins` member
    2.  broken file-path entry (2026-10 bug)    -> replaced with the dir
    3.  legacy opencode-mpm entry               -> replaced with the dir
    4.  already-correct directory registration  -> byte-identical no-op
    5.  repeated migration                      -> byte-identical no-op
    6.  unrelated entries + comments + trailing commas -> preserved
    7.  CRLF line endings                       -> preserved
    8.  BOM-prefixed config                     -> preserved
    9.  both `plugin` and `plugins` keys        -> merged into `plugins`
    10. legacy `plugin` key migration           -> key renamed
    11. refusal of a non-array plugin key       -> error, file untouched

  check_entry
    12. canonical directory registration        -> accepted
    13. broken file-path registration           -> REJECTED
    14. legacy namespace registration           -> REJECTED

  remove_mpm (uninstall)
    15. removes every MPM-owned shape, keeps unrelated entries
    16. removes an emptied plugin key entirely
    17. no-op on configs without MPM entries

Negative controls: tests 13/14 fail the OLD contract (the old installer
accepted any entry matching the canonical prefix and only ever checked
for `plugin`-key entries), which is exactly how the broken file-path
registration survived a successful `--verify` on 2026-10-16.
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))

import jsonc_config  # noqa: E402
from jsonc_config import (  # noqa: E402
    JsoncError,
    check_entry,
    migrate,
    parse,
    remove_mpm,
)

HOME = "/home/testuser"
ADAPTER = HOME + "/.mpm/agent_installation/mpm-opencode"
CANONICAL = "file://" + ADAPTER

# The registration shapes this installer wrote historically.
BROKEN_FILE_ENTRY = CANONICAL + "/dist/index.js"
LEGACY_DIR = HOME + "/.mpm/agent_installation/opencode-mpm"
LEGACY_ENTRY = "file://" + LEGACY_DIR + "/dist/index.js"
UNRELATED = "file:///opt/team/linter-plugin"


def _parse_entries(text):
    """The plugin entries a config carries, across both key names."""
    data = parse(text)
    out = []
    for key in ("plugins", "plugin"):
        if isinstance(data.get(key), list):
            out.extend(data[key])
    return out


class MigrationTests(unittest.TestCase):
    def test_1_fresh_config_with_no_plugin_array(self):
        text = '{\n  "$schema": "https://opencode.ai/config.json"\n}\n'
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        data = parse(updated)
        self.assertEqual(data["plugins"], [CANONICAL])
        self.assertNotIn("plugin", data)
        # The $schema URL must not have been touched.
        self.assertEqual(data["$schema"], "https://opencode.ai/config.json")

    def test_1b_fresh_install_when_config_file_does_not_exist_yet(self):
        # The installer creates the file before migrating; the migration of
        # an empty (new) file is the add-member path.
        text = "{}\n"
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(parse(updated), {"plugins": [CANONICAL]})

    def test_2_broken_file_path_entry_is_replaced_with_package_dir(self):
        text = '{\n  "plugins": ["%s"]\n}\n' % BROKEN_FILE_ENTRY
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(_parse_entries(updated), [CANONICAL])
        self.assertNotIn("/dist/index.js", updated)

    def test_3_legacy_opencode_mpm_entry_is_replaced(self):
        text = '{\n  "plugins": ["%s"]\n}\n' % LEGACY_ENTRY
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(_parse_entries(updated), [CANONICAL])
        self.assertNotIn("opencode-mpm", updated)

    def test_3b_legacy_bare_directory_entry_is_replaced(self):
        text = '{\n  "plugins": ["file://%s"]\n}\n' % LEGACY_DIR
        updated, _changed, _summary = migrate(text, CANONICAL)
        self.assertEqual(_parse_entries(updated), [CANONICAL])

    def test_4_already_correct_registration_is_a_noop(self):
        text = '{\n  "plugins": ["%s"]\n}\n' % CANONICAL
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertFalse(changed)
        self.assertEqual(updated, text)

    def test_4b_canonical_registered_under_legacy_key_is_normalized(self):
        text = '{\n  "plugin": ["%s"]\n}\n' % CANONICAL
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(_parse_entries(updated), [CANONICAL])
        self.assertNotIn('"plugin"', updated)

    def test_5_repeated_migration_is_byte_identical(self):
        text = '{\n  "plugins": ["%s",\n    "%s"]\n}\n' % (BROKEN_FILE_ENTRY, UNRELATED)
        once, _, _ = migrate(text, CANONICAL)
        twice, changed, _ = migrate(once, CANONICAL)
        self.assertFalse(changed)
        self.assertEqual(twice, once)

    def test_6_unrelated_entries_and_comments_survive(self):
        text = (
            "{\n"
            '  // team linter, loaded project-locally\n'
            '  "plugins": [\n'
            '    "%s"\n'
            "  ],\n"
            "  /* theme pin\n     for the whole machine */\n"
            '  "theme": "dark"\n'
            "}\n"
        ) % UNRELATED
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(_parse_entries(updated), [UNRELATED, CANONICAL])
        self.assertIn("team linter, loaded project-locally", updated)
        self.assertIn("theme pin", updated)
        self.assertIn('"theme": "dark"', updated)

    def test_6b_comment_trailing_the_migrated_entry_survives(self):
        text = (
            "{\n"
            '  "plugins": [\n'
            '    "%s" // mpm memory substrate\n'
            "  ]\n"
            "}\n"
        ) % BROKEN_FILE_ENTRY
        updated, _, _summary = migrate(text, CANONICAL)
        self.assertIn("mpm memory substrate", updated)
        self.assertEqual(_parse_entries(updated), [CANONICAL])

    def test_6c_trailing_comma_in_unrelated_position_survives(self):
        text = (
            "{\n"
            '  "plugins": [\n'
            '    "%s",\n'
            "  ],\n"
            '  "model": "x/1",\n'
            "}\n"
        ) % BROKEN_FILE_ENTRY
        updated, _, _ = migrate(text, CANONICAL)
        self.assertEqual(_parse_entries(updated), [CANONICAL])
        self.assertIn('"model": "x/1",', updated)

    def test_7_crlf_endings_are_preserved(self):
        text = (
            "{\r\n" '  "plugins": ["%s"]\r\n' "}\r\n"
        ) % BROKEN_FILE_ENTRY
        updated, _, _ = migrate(text, CANONICAL)
        self.assertIn("\r\n", updated)
        self.assertNotIn("] \n", updated)
        self.assertEqual(_parse_entries(updated), [CANONICAL])

    def test_8_bom_prefixed_config_is_preserved(self):
        text = _BOM = "﻿" + '{\n  "plugins": ["%s"]\n}\n' % BROKEN_FILE_ENTRY
        updated, _, _ = migrate(text, CANONICAL)
        self.assertTrue(updated.startswith("﻿"))
        self.assertEqual(_parse_entries(updated), [CANONICAL])

    def test_9_both_keys_are_merged_into_plugins(self):
        text = (
            "{\n"
            '  "plugin": ["%s"],\n'
            '  "plugins": ["%s"]\n'
            "}\n"
        ) % (UNRELATED, BROKEN_FILE_ENTRY)
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(_parse_entries(updated), [CANONICAL, UNRELATED])
        self.assertNotIn('"plugin"', updated)
        data = parse(updated)
        self.assertIn("plugins", data)
        self.assertNotIn("plugin", data)

    def test_10_legacy_key_with_stale_entry_is_renamed(self):
        text = '{\n  "plugin": ["%s"]\n}\n' % LEGACY_ENTRY
        updated, changed, _summary = migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(_parse_entries(updated), [CANONICAL])
        self.assertNotIn('"plugin"', updated)

    def test_11_non_array_plugin_key_is_refused_without_writing(self):
        text = '{\n  "plugins": "file://%s"\n}\n' % ADAPTER
        before = text
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            with open(path, "w", encoding="utf-8", newline="") as fh:
                fh.write(text)
            rc = jsonc_config.main(["jsonc_config.py", path, CANONICAL])
            self.assertEqual(rc, 1)
            with open(path, "r", encoding="utf-8", newline="") as fh:
                self.assertEqual(fh.read(), before)

    def test_11b_malformed_jsonc_is_refused_without_writing(self):
        text = '{\n  "plugins": ["%s"\n' % BROKEN_FILE_ENTRY  # unterminated
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            with open(path, "w", encoding="utf-8", newline="") as fh:
                fh.write(text)
            rc = jsonc_config.main(["jsonc_config.py", path, CANONICAL])
            self.assertEqual(rc, 1)
            with open(path, "r", encoding="utf-8", newline="") as fh:
                self.assertEqual(fh.read(), text)

    def test_12_url_in_string_value_is_not_mangled(self):
        text = (
            "{\n"
            '  "$schema": "https://opencode.ai/config.json",\n'
            '  "plugins": ["%s"]\n'
            "}\n"
        ) % BROKEN_FILE_ENTRY
        updated, _, _ = migrate(text, CANONICAL)
        self.assertIn("https://opencode.ai/config.json", updated)
        self.assertEqual(parse(updated)["$schema"], "https://opencode.ai/config.json")

    def test_13_segment_match_only(self):
        """An unrelated directory whose name CONTAINS the adapter name is untouched."""
        decoy = "file:///tmp/mpm-opencode-debug-XYZ/dist/index.js"
        text = '{\n  "plugins": ["%s"]\n}\n' % decoy
        updated, changed, _summary = migrate(text, CANONICAL)
        # The decoy is unrelated; it survives and the canonical entry is added.
        self.assertEqual(_parse_entries(updated), [decoy, CANONICAL])
        self.assertTrue(changed)


class CheckTests(unittest.TestCase):
    def test_canonical_registration_is_accepted(self):
        text = '{\n  "plugins": ["%s"]\n}\n' % CANONICAL
        self.assertTrue(check_entry(text, CANONICAL))

    def test_canonical_under_legacy_key_is_accepted(self):
        text = '{\n  "plugin": ["%s"]\n}\n' % CANONICAL
        self.assertTrue(check_entry(text, CANONICAL))

    def test_broken_file_path_registration_is_REJECTED(self):
        # Negative control: the old --check-entry accepted this shape,
        # which is how a broken config passed install --verify.
        text = '{\n  "plugins": ["%s"]\n}\n' % BROKEN_FILE_ENTRY
        self.assertFalse(check_entry(text, CANONICAL))

    def test_legacy_namespace_registration_is_REJECTED(self):
        text = '{\n  "plugins": ["%s"]\n}\n' % LEGACY_ENTRY
        self.assertFalse(check_entry(text, CANONICAL))

    def test_verification_fails_when_config_is_missing(self):
        with tempfile.TemporaryDirectory() as tmp:
            missing = os.path.join(tmp, "nope.jsonc")
            rc = jsonc_config.main(
                ["jsonc_config.py", "--check-entry", missing, CANONICAL]
            )
            self.assertEqual(rc, 1)

    def test_check_entry_cli_exit_codes(self):
        with tempfile.TemporaryDirectory() as tmp:
            good = os.path.join(tmp, "good.jsonc")
            bad = os.path.join(tmp, "bad.jsonc")
            with open(good, "w", encoding="utf-8") as fh:
                fh.write('{\n  "plugins": ["%s"]\n}\n' % CANONICAL)
            with open(bad, "w", encoding="utf-8") as fh:
                fh.write('{\n  "plugins": ["%s"]\n}\n' % BROKEN_FILE_ENTRY)
            self.assertEqual(
                jsonc_config.main(["x", "--check-entry", good, CANONICAL]), 0
            )
            self.assertEqual(
                jsonc_config.main(["x", "--check-entry", bad, CANONICAL]), 1
            )


class RemoveTests(unittest.TestCase):
    def test_removes_every_mpm_owned_shape_and_keeps_unrelated(self):
        text = (
            "{\n"
            '  "plugins": [\n'
            '    "%s",\n'
            '    "%s",\n'
            '    "%s",\n'
            '    "%s"\n'
            "  ]\n"
            "}\n"
        ) % (UNRELATED, CANONICAL, BROKEN_FILE_ENTRY, LEGACY_ENTRY)
        updated, changed, _summary = remove_mpm(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(_parse_entries(updated), [UNRELATED])
        self.assertIn(UNRELATED, updated)

    def test_removes_emptied_key_entirely(self):
        text = '{\n  "theme": "dark",\n  "plugins": ["%s"]\n}\n' % CANONICAL
        updated, changed, _summary = remove_mpm(text, CANONICAL)
        self.assertTrue(changed)
        self.assertNotIn("plugins", updated)
        self.assertEqual(parse(updated), {"theme": "dark"})

    def test_keeps_non_mpm_entries_when_partial(self):
        text = (
            "{\n"
            '  "plugin": ["%s",\n'
            '    "%s"],\n'
            '  "plugins": ["%s"]\n'
            "}\n"
        ) % (LEGACY_ENTRY, UNRELATED, CANONICAL)
        updated, changed, _summary = remove_mpm(text, CANONICAL)
        self.assertTrue(changed)
        # plugin key kept its unrelated entry and is emptied of MPM's;
        # plugins key lost its only entry and is gone.
        data = parse(updated)
        self.assertEqual(data.get("plugin"), [UNRELATED])
        self.assertNotIn("plugins", data)

    def test_noop_when_no_mpm_entries(self):
        text = '{\n  "plugins": ["%s"]\n}\n' % UNRELATED
        updated, changed, _summary = remove_mpm(text, CANONICAL)
        self.assertFalse(changed)
        self.assertEqual(updated, text)

    def test_uninstall_cli_roundtrip(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            original = (
                "{\n"
                "  // mpm (removed by hand later)\n"
                '  "plugins": ["%s"],\n'
                '  "theme": "dark"\n'
                "}\n"
            ) % CANONICAL
            with open(path, "w", encoding="utf-8", newline="") as fh:
                fh.write(original)
            rc = jsonc_config.main(["x", "--remove-mpm", path, CANONICAL])
            self.assertEqual(rc, 0)
            with open(path, "r", encoding="utf-8", newline="") as fh:
                text = fh.read()
            self.assertNotIn("plugins", text)
            self.assertIn('"theme": "dark"', text)
            self.assertIn("mpm (removed by hand later)", text)
            # Uninstalling again is a clean no-op.
            rc = jsonc_config.main(["x", "--remove-mpm", path, CANONICAL])
            self.assertEqual(rc, 0)
            with open(path, "r", encoding="utf-8", newline="") as fh:
                self.assertEqual(fh.read(), text)


class CliRoundTripTests(unittest.TestCase):
    def test_migrate_then_install_idempotent_via_cli(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "opencode.jsonc")
            with open(path, "w", encoding="utf-8", newline="") as fh:
                fh.write('{\n  "plugins": ["%s"]\n}\n' % BROKEN_FILE_ENTRY)
            self.assertEqual(jsonc_config.main(["x", path, CANONICAL]), 0)
            with open(path, "r", encoding="utf-8", newline="") as fh:
                once = fh.read()
            self.assertEqual(
                jsonc_config.main(["x", path, CANONICAL]), 0
            )
            with open(path, "r", encoding="utf-8", newline="") as fh:
                self.assertEqual(fh.read(), once)


if __name__ == "__main__":
    unittest.main(verbosity=2)
