#!/usr/bin/env python3
"""test_release_bundle_layout.py — the published archive carries the whole bundle.

WHAT THIS PROVES

A release bundle is a directory. The published archive is a TARBALL of
that directory, and the entries that go into it used to be written out
by hand in scripts/assemble-release-bundle.sh — while a test compared
that hand-written list against a COPY of the same list. Two copies of
a list is exactly how the archive came to omit config/, the directory
that holds the host-asset declaration: the assets were in the tarball
and the declaration that proves them was not, and the assembler
validated the CANDIDATE (which does have config/), so the failure was
silent.

THE RULE

The bundle's top-level shape is declared ONCE, in
scripts/lib/runtime_bundle.sh, as RB_BUNDLE_ENTRIES. The assembler
reads that list, and this test pins the two together. A bundle entry
added to that list is in the archive BY CONSTRUCTION — there is no
second list to forget.

The test asserts the assembler USES the shared list rather than
repeating it. That is the property the old test could not check: it
compared two hand-written lists, so both could be wrong together.
"""

import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
ASSEMBLER = REPO_ROOT / "scripts" / "assemble-release-bundle.sh"
LIBRARY = REPO_ROOT / "scripts" / "lib" / "runtime_bundle.sh"


def library_bundle_entries() -> list[str]:
    """Return RB_BUNDLE_ENTRIES, the bundle's declared top-level shape."""
    text = LIBRARY.read_text(encoding="utf-8")
    match = re.search(r'^RB_BUNDLE_ENTRIES="([^"]*)"', text, re.MULTILINE)
    if not match:
        raise AssertionError(
            "scripts/lib/runtime_bundle.sh declares no RB_BUNDLE_ENTRIES; "
            "the bundle's shape must be declared once, and read by both the "
            "assembler and this test")
    return match.group(1).split()


def assembler_tar_lists() -> list[list[str]]:
    """Return the entry lists of every tar invocation in the assembler."""
    text = ASSEMBLER.read_text(encoding="utf-8")
    lists = []
    for match in re.finditer(
            r"tar\s+-C\s+\"\$CANDIDATE\"\s+-czf\s+\"\$OUT/\$ARCHIVE\"\s+([^\n;|]+)",
            text):
        entries = [e for e in match.group(1).split() if not e.startswith("2>")]
        if entries:
            lists.append(entries)
    return lists


class TestReleaseBundleLayout(unittest.TestCase):
    def test_the_bundle_shape_is_declared_once(self):
        entries = library_bundle_entries()
        self.assertTrue(entries, "RB_BUNDLE_ENTRIES names no entries")

    def test_the_assembler_reads_the_shared_list(self):
        # THE property the old test could not check. The assembler must
        # NOT repeat the list: a repeat is a second copy, and a second
        # copy is how the archive and the bundle drifted apart.
        text = ASSEMBLER.read_text(encoding="utf-8")
        self.assertIn(
            "$RB_BUNDLE_ENTRIES",
            text,
            "the assembler hand-writes the archive's entry list instead of "
            "reading RB_BUNDLE_ENTRIES; a hand-written list is a second copy "
            "of the bundle's shape, and the two drift apart",
        )

    def test_the_archive_list_is_the_bundles_declared_shape(self):
        expected = library_bundle_entries()
        lists = assembler_tar_lists()
        self.assertTrue(lists, "the assembler has no tar invocation to check")
        for entries in lists:
            if "$RB_BUNDLE_ENTRIES" in entries:
                continue  # the shared list, expanded below
            self.assertEqual(
                entries,
                expected,
                f"the assembler's archive list {entries} is not the bundle's "
                f"declared shape {expected}",
            )

    def test_the_archive_carries_the_whole_declared_shape(self):
        expected = library_bundle_entries()
        lists = assembler_tar_lists()
        self.assertTrue(lists, "the assembler has no tar invocation to check")
        for entries in lists:
            if "$RB_BUNDLE_ENTRIES" in entries:
                self.assertEqual(
                    entries,
                    ["$RB_BUNDLE_ENTRIES"],
                    "the assembler expands the shared list by hand",
                )
            else:
                self.assertEqual(entries, expected)

    def test_the_archive_carries_the_host_asset_declaration(self):
        # config/ is where the host-asset declaration lives. Without it
        # the archive carries the assets and not the declaration that
        # proves them, and the updater refuses the bundle.
        self.assertIn("config", library_bundle_entries())

    def test_the_archive_carries_the_schema_preflight(self):
        # schema/ carries the substrate's lifecycle_events DDL. Without
        # it the schema-collision preflight degrades to a no-op.
        self.assertIn("schema", library_bundle_entries())

    def test_the_archive_carries_the_payload_and_the_identity(self):
        entries = library_bundle_entries()
        for required in ("bin", "agent_installation", "RELEASE.json"):
            self.assertIn(required, entries)


if __name__ == "__main__":
    unittest.main()
