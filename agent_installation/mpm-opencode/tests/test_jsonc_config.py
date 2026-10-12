"""test_jsonc_config.py — regression tests for opencode.jsonc migration.

The defect these pin: install.sh read the user's `opencode.jsonc` with
`json.loads`, which rejects the comments and trailing commas that the
`.jsonc` format exists to hold. The old code caught that rejection and
exited 0 — "malformed config, leave it alone" — so the shell reported a
successful migration while the stale `opencode-mpm` entry survived and
the OpenCode plugin loader broke.

Two properties matter beyond "it parses now":

  - A URL inside a string value must not be mangled. A regex-based
    comment stripper turns "https://opencode.ai/config.json" into
    "https:" — and every OpenCode config carries that $schema URL, so
    the naive fix corrupts essentially every real config.

  - Unrelated bytes must survive. Round-tripping through json.dump
    preserves the data and destroys every comment the operator wrote,
    in a format whose purpose is to hold comments.

All tests here are pure-function tests against migrate()/parse(); the
installer-level behaviour (exit codes, that install.sh aborts) is
covered in test_opencode_install.py.
"""

import json
import sys
import unittest
from pathlib import Path

sys.path.insert(
    0, str(Path(__file__).resolve().parent.parent / "scripts")
)

import jsonc_config as jc  # noqa: E402


CANONICAL = "file:///home/u/.mpm/agent_installation/mpm-opencode/dist/index.js"
LEGACY = "file:///home/u/.mpm/agent_installation/opencode-mpm/dist/index.js"


class StripJsonc(unittest.TestCase):
    """Comment stripping must be string-aware and offset-preserving."""

    def test_strips_line_comment(self):
        src = '{\n  // hi\n  "a": 1\n}'
        # Every byte of the comment becomes a space, so the result is the
        # same length as the input and later offsets stay valid.
        self.assertEqual(
            jc.strip_jsonc(src),
            "{\n" + " " * len("  // hi") + "\n" + '  "a": 1\n}',
        )

    def test_strips_block_comment(self):
        self.assertEqual(jc.strip_jsonc('{/*x*/"a":1}'), '{     "a":1}')

    def test_preserves_offsets(self):
        """Spans located in stripped text must index the original."""
        src = '{\n  // comment\n  "a": 1\n}'
        self.assertEqual(len(jc.strip_jsonc(src)), len(src))

    def test_preserves_newlines_so_line_numbers_survive(self):
        stripped = jc.strip_jsonc("{\n  // one\n  // two\n  \"a\": 1\n}")
        self.assertEqual(stripped.count("\n"), 4)

    def test_does_not_eat_url_inside_string(self):
        """The regression that a regex stripper cannot avoid."""
        src = '{"$schema": "https://opencode.ai/config.json"}'
        self.assertEqual(jc.strip_jsonc(src), src)
        self.assertEqual(
            jc.parse(src)["$schema"], "https://opencode.ai/config.json"
        )

    def test_does_not_eat_double_slash_inside_string(self):
        src = '{"url": "https://example.com//deep"}'
        self.assertEqual(jc.parse(src)["url"], "https://example.com//deep")

    def test_does_not_eat_block_comment_markers_inside_string(self):
        src = '{"note": "/* not a comment */"}'
        self.assertEqual(jc.parse(src)["note"], "/* not a comment */")

    def test_handles_escaped_quote_inside_string(self):
        src = '{"x": "he said \\"hi\\" // still in string"}'
        self.assertEqual(jc.parse(src)["x"], 'he said "hi" // still in string')

    def test_unterminated_block_comment_raises(self):
        with self.assertRaises(jc.JsoncError):
            jc.strip_jsonc("{ /* never closed")

    def test_unterminated_string_raises(self):
        with self.assertRaises(jc.JsoncError):
            jc.strip_jsonc('{"a": "unclosed}')


class StripTrailingCommas(unittest.TestCase):
    def test_removes_trailing_comma_in_array(self):
        self.assertEqual(
            jc.strip_trailing_commas('["a", "b",]'), '["a", "b" ]'
        )

    def test_removes_trailing_comma_in_object(self):
        self.assertEqual(jc.strip_trailing_commas('{"a": 1,}'), '{"a": 1 }')

    def test_keeps_separator_comma(self):
        self.assertEqual(jc.strip_trailing_commas('["a", "b"]'), '["a", "b"]')

    def test_does_not_touch_comma_inside_string(self):
        src = '{"x": "a,", "y": 1}'
        self.assertEqual(jc.strip_trailing_commas(src), src)


class Parse(unittest.TestCase):
    def test_parses_commented_jsonc(self):
        data = jc.parse('{\n  // c\n  "a": 1,\n}\n')
        self.assertEqual(data, {"a": 1})

    def test_malformed_raises_jsonc_error(self):
        with self.assertRaises(jc.JsoncError):
            jc.parse('{"a": @}')

    def test_error_mentions_line_and_column(self):
        with self.assertRaises(jc.JsoncError) as ctx:
            jc.parse('{\n  "a": @\n}')
        self.assertIn("line", str(ctx.exception))


class Migrate(unittest.TestCase):
    def test_migrates_legacy_entry(self):
        src = '{\n  "plugin": [\n    "%s"\n  ]\n}\n' % LEGACY
        out, changed, _summary = jc.migrate(src, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(jc.parse(out)["plugin"], [CANONICAL])

    def test_migrates_commented_config(self):
        """The original defect: a commented config must actually migrate."""
        src = (
            "{\n"
            "  // plugins I use\n"
            '  "$schema": "https://opencode.ai/config.json",\n'
            '  "plugin": [\n    "%s",\n  ],\n' % LEGACY
            + "}\n"
        )
        out, changed, _summary = jc.migrate(src, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(jc.parse(out)["plugin"], [CANONICAL])
        self.assertNotIn("opencode-mpm/dist", out)

    def test_preserves_comments_and_unrelated_keys(self):
        src = (
            "{\n"
            "  // top note\n"
            '  "$schema": "https://opencode.ai/config.json",  /* inline */\n'
            '  "theme": "dark",\n'
            '  "model": "anthropic/claude",\n'
            '  "plugin": [\n    "%s",\n    "other-plugin",  // why I need it\n  ],\n'
            % LEGACY
            + "}\n"
        )
        out, _, _summary = jc.migrate(src, CANONICAL)
        self.assertIn("// top note", out)
        self.assertIn("/* inline */", out)
        self.assertIn("// why I need it", out)
        data = jc.parse(out)
        self.assertEqual(data["theme"], "dark")
        self.assertEqual(data["model"], "anthropic/claude")
        self.assertEqual(
            data["$schema"], "https://opencode.ai/config.json"
        )

    def test_preserves_unrelated_plugin_entries(self):
        src = '{\n  "plugin": [\n    "%s",\n    "keep-me",\n    "also-keep"\n  ]\n}\n' % LEGACY
        out, _, _summary = jc.migrate(src, CANONICAL)
        self.assertEqual(
            jc.parse(out)["plugin"], [CANONICAL, "keep-me", "also-keep"]
        )

    def test_adds_canonical_when_absent(self):
        src = '{\n  "plugin": [\n    "only-other"\n  ]\n}\n'
        out, changed, _summary = jc.migrate(src, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(jc.parse(out)["plugin"], [CANONICAL, "only-other"])

    def test_adds_plugin_key_when_missing(self):
        src = '{\n  "theme": "dark"\n}\n'
        out, changed, _summary = jc.migrate(src, CANONICAL)
        self.assertTrue(changed)
        data = jc.parse(out)
        self.assertEqual(data["plugin"], [CANONICAL])
        self.assertEqual(data["theme"], "dark")

    def test_empty_object(self):
        out, changed, _summary = jc.migrate("{\n}\n", CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(jc.parse(out), {"plugin": [CANONICAL]})

    def test_string_plugin_form_is_refused_not_coerced(self):
        """A bare-string `plugin` is an error, not something to normalise.

        The earlier implementation wrapped a string in a list and
        re-rendered it. That invented structure the operator never wrote,
        and because the spliced span was a string with no brackets to
        absorb the layout it emitted `]}` glued onto the closing brace.
        Refusing is non-destructive and names the fix.
        """
        src = '{\n  "plugin": "%s"\n}\n' % LEGACY
        with self.assertRaises(jc.JsoncError) as ctx:
            jc.migrate(src, CANONICAL)
        self.assertIn("must be an array of strings", str(ctx.exception))
        self.assertIn("refusing to guess", str(ctx.exception))

    def test_already_canonical_is_byte_identical_noop(self):
        src = '{\n  // note\n  "plugin": [\n    "%s",\n  ],\n}\n' % CANONICAL
        out, changed, _summary = jc.migrate(src, CANONICAL)
        self.assertFalse(changed)
        self.assertEqual(out, src)

    def test_repeated_migrate_is_byte_identical(self):
        """Idempotency must be structural, not merely semantic."""
        src = (
            "{\n  // note\n"
            '  "$schema": "https://opencode.ai/config.json",\n'
            '  "plugin": [\n    "%s",\n    "other",  // keep\n  ],\n}\n' % LEGACY
        )
        once, _, _s1 = jc.migrate(src, CANONICAL)
        twice, changed, _s2 = jc.migrate(once, CANONICAL)
        self.assertFalse(changed)
        self.assertEqual(twice, once)

    def test_nested_plugin_key_is_not_matched(self):
        """A nested 'plugin' key is unrelated configuration."""
        src = '{\n  "a": {\n    "plugin": [\n      "%s"\n    ]\n  }\n}\n' % LEGACY
        out, _, _summary = jc.migrate(src, CANONICAL)
        data = jc.parse(out)
        self.assertEqual(data["a"]["plugin"], [LEGACY])
        self.assertEqual(data["plugin"], [CANONICAL])

    def test_path_prefix_containing_mpm_opencode_is_not_legacy(self):
        """/tmp/mpm-opencode-debug-XYZ must not read as a stale entry."""
        other = "file:///tmp/mpm-opencode-debug-XYZ/dist/index.js"
        src = '{\n  "plugin": [\n    "%s"\n  ]\n}\n' % other
        out, _, _summary = jc.migrate(src, CANONICAL)
        self.assertEqual(jc.parse(out)["plugin"], [CANONICAL, other])

    def test_malformed_config_raises_and_is_not_rewritten(self):
        src = '{\n  "plugin": [\n    "%s",\n' % LEGACY  # truncated
        with self.assertRaises(jc.JsoncError):
            jc.migrate(src, CANONICAL)

    def test_non_object_root_raises(self):
        with self.assertRaises(jc.JsoncError):
            jc.migrate("[1, 2, 3]\n", CANONICAL)

    def test_non_list_plugin_raises(self):
        with self.assertRaises(jc.JsoncError):
            jc.migrate('{\n  "plugin": 42\n}\n', CANONICAL)

    def test_indentation_matches_surrounding_config(self):
        src = (
            "{\n"
            '    "$schema": "https://opencode.ai/config.json",\n'
            '    "plugin": [\n        "%s"\n    ]\n'
            "}\n" % LEGACY
        )
        out, _, _summary = jc.migrate(src, CANONICAL)
        # The closing bracket lines up with the key it belongs to.
        self.assertIn('\n    ]\n', out)
        self.assertEqual(jc.parse(out)["plugin"], [CANONICAL])


class CheckEntry(unittest.TestCase):
    """--verify must check the canonical entry is present, not just that
    the legacy one is absent."""

    def test_present(self):
        src = '{\n  "plugin": [\n    "%s"\n  ]\n}\n' % CANONICAL
        self.assertTrue(jc.check_entry(src, CANONICAL))

    def test_absent_when_only_unrelated_plugins(self):
        """The gap: this previously passed verify with MPM unregistered."""
        src = '{\n  "plugin": [\n    "someone-elses-plugin"\n  ]\n}\n'
        self.assertFalse(jc.check_entry(src, CANONICAL))

    def test_absent_when_key_missing(self):
        self.assertFalse(jc.check_entry('{\n  "theme": "dark"\n}\n', CANONICAL))

    def test_present_in_commented_config(self):
        src = '{\n  // c\n  "plugin": [\n    "%s",\n  ],\n}\n' % CANONICAL
        self.assertTrue(jc.check_entry(src, CANONICAL))

    def test_string_form(self):
        self.assertTrue(
            jc.check_entry('{\n  "plugin": "%s"\n}\n' % CANONICAL, CANONICAL)
        )

    def test_url_in_other_field_does_not_count(self):
        """A grep-based check would match the canonical substring here."""
        src = '{\n  "docs": "%s"\n}\n' % CANONICAL
        self.assertFalse(jc.check_entry(src, CANONICAL))

    def test_unparseable_raises(self):
        with self.assertRaises(jc.JsoncError):
            jc.check_entry("{ not json", CANONICAL)


class LineEndingAndEncoding(unittest.TestCase):
    """The three defects found by probing the editor after it shipped.

    Each of these passed the original 40-test suite because every fixture
    was LF-only and BOM-free, so nothing ever exercised the code paths
    where the file's own bytes could be lost.
    """

    # ---- CRLF ----

    def test_crlf_file_keeps_crlf_on_every_line(self):
        src = (
            '{\r\n'
            '  "model": "x",\r\n'
            '  "plugin": [\r\n'
            '    "%s"\r\n'
            '  ]\r\n'
            '}\r\n' % LEGACY
        )
        out, changed, _ = jc.migrate(src, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(
            out.count("\n") - out.count("\r\n"),
            0,
            "migration introduced bare-LF lines into a CRLF config",
        )
        self.assertIn('  "model": "x",\r\n', out)

    def test_crlf_related_lines_are_byte_identical(self):
        """Only the plugin array may differ; nothing else moves."""
        before = '{\r\n  "$schema": "https://opencode.ai/config.json",\r\n  "theme": "opencode",\r\n'
        src = before + '  "plugin": [\r\n    "%s"\r\n  ]\r\n}\r\n' % LEGACY
        out, _, _ = jc.migrate(src, CANONICAL)
        self.assertTrue(out.startswith(before))

    def test_detect_newline_picks_dominant(self):
        self.assertEqual(jc.detect_newline("a\nb\n"), "\n")
        self.assertEqual(jc.detect_newline("a\r\nb\r\n"), "\r\n")
        # Mixed resolves to the majority rather than flipping the file.
        self.assertEqual(jc.detect_newline("a\r\nb\r\nc\n"), "\r\n")

    def test_crlf_repeated_migrate_is_byte_identical(self):
        src = '{\r\n  "plugin": [\r\n    "%s"\r\n  ]\r\n}\r\n' % LEGACY
        once, _, _ = jc.migrate(src, CANONICAL)
        twice, changed, _ = jc.migrate(once, CANONICAL)
        self.assertFalse(changed)
        self.assertEqual(once, twice)

    # ---- BOM ----

    def test_bom_is_not_a_parse_error(self):
        """U+FEFF is neither whitespace nor comment, so it used to make
        every parse fail and a valid config get refused as malformed."""
        body = '{\n  "plugin": ["%s"]\n}\n' % LEGACY
        text = "﻿" + body
        self.assertEqual(jc.parse(text)["plugin"], [LEGACY])
        out, changed, _ = jc.migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(out[0], "﻿")

    def test_bom_file_without_legacy_still_reports_honestly(self):
        text = '﻿{\n  "plugin": ["other"]\n}\n'
        out, changed, summary = jc.migrate(text, CANONICAL)
        self.assertTrue(changed)
        self.assertEqual(out[0], "﻿")
        self.assertIn("no legacy entry was present", summary)

    # ---- messages describe what happened ----

    def test_summary_distinguishes_replace_from_add(self):
        _, _, replaced = jc.migrate(
            '{\n  "plugin": ["%s"]\n}\n' % LEGACY, CANONICAL
        )
        self.assertIn("replaced 1 legacy", replaced)

        _, _, added = jc.migrate('{\n  "plugin": ["other"]\n}\n', CANONICAL)
        self.assertIn("no legacy entry was present", added)

    def test_summary_reports_noop(self):
        _, changed, summary = jc.migrate(
            '{\n  "plugin": ["%s"]\n}\n' % CANONICAL, CANONICAL
        )
        self.assertFalse(changed)
        self.assertEqual(summary, "already canonical")

    def test_summary_counts_multiple_legacy_entries(self):
        _, _, summary = jc.migrate(
            '{\n  "plugin": ["%s", "%s"]\n}\n' % (LEGACY, LEGACY), CANONICAL
        )
        self.assertIn("replaced 2 legacy", summary)


if __name__ == "__main__":
    unittest.main()