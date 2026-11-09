#!/usr/bin/env python3
"""test_release_bundle_layout.py — the published archive carries the whole bundle.

WHAT THIS PROVES

A release bundle is a directory. The published archive is a TARBALL of
that directory, and the list of entries that go into it is written out
by hand in scripts/assemble-release-bundle.sh. A list written out by
hand is a list that can fall behind.

It fell behind. The host-asset declaration — the file that says which
integration assets the bundle carries, and with which digests — lives
in config/, and config/ was not in the tar list. The published archive
therefore carried the assets and NOT the declaration that proves them,
so the updater's validator, which checks a bundle against its own
declaration, refused every bundle the assembler produced.

The failure was silent: the assembler validated the CANDIDATE (which
has config/) and then published an archive without it.

THE RULE

Every top-level entry of the bundle must appear in the tar list. The
list is read out of the script, so a future entry cannot be added to
the bundle without being added to the archive.
"""

import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
ASSEMBLER = REPO_ROOT / "scripts" / "assemble-release-bundle.sh"

# The top-level entries a bundle has. Every one of them must be in the
# archive: a bundle the installer produces, a bundle the assembler
# produces and a bundle the updater builds all have exactly this
# shape.
BUNDLE_ENTRIES = ("bin", "agent_installation", "config", "RELEASE.json")

# schema/ is OPTIONAL: the lifecycle_events DDL is extracted on a
# best-effort basis, and the assembler falls back to an archive without
# it when the extraction produced nothing. It must appear in at least
# one of the archive lists, or the published bundle never carries the
# schema preflight.
OPTIONAL_ENTRIES = ("schema",)


def tar_lists() -> list[list[str]]:
    """Return the entry lists of every tar invocation in the assembler."""
    text = ASSEMBLER.read_text(encoding="utf-8")
    lists = []
    for match in re.finditer(r"tar\s+-C\s+\"\$CANDIDATE\"\s+-czf\s+\"\$OUT/\$ARCHIVE\"\s+([^\n;|]+)", text):
        entries = [e for e in match.group(1).split() if not e.startswith("2>")]
        if entries:
            lists.append(entries)
    return lists


class TestReleaseBundleLayout(unittest.TestCase):
    def test_the_assembler_names_a_tar_list(self):
        lists = tar_lists()
        self.assertTrue(lists, "the assembler has no tar invocation to check")

    def test_every_tar_list_covers_the_whole_bundle(self):
        for entries in tar_lists():
            for required in BUNDLE_ENTRIES:
                self.assertIn(
                    required,
                    entries,
                    f"the assembler's archive list {entries} omits {required!r}; "
                    "the published archive would not carry the whole bundle",
                )

    def test_every_tar_list_carries_the_host_asset_declaration(self):
        # config/ is where the host-asset declaration lives. Without it
        # the archive carries the assets and not the declaration that
        # proves them, and the updater refuses the bundle.
        for entries in tar_lists():
            self.assertIn(
                "config",
                entries,
                f"the assembler's archive list {entries} omits config/, so the "
                "published bundle carries no host-asset declaration",
            )

    def test_some_tar_list_carries_the_schema_preflight(self):
        lists = tar_lists()
        self.assertTrue(
            any(entry in entries for entries in lists for entry in OPTIONAL_ENTRIES),
            "no archive list carries the schema preflight, so the published bundle "
            "would never carry the lifecycle_events DDL",
        )


if __name__ == "__main__":
    unittest.main()
