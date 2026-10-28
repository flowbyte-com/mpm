"""Tests for the documentation freshness gate.

The point of this file is **non-vacuity**. A checker that passes on a broken
tree is worse than no checker: it converts "nobody looked" into "the gate is
green". So every check is driven against a deliberately broken fixture and
asserted to fail, and against the same fixture repaired and asserted to pass.

The negative controls matter as much as the positive ones. `test_prose_that_
merely_uses_the_verb_make` exists because the first implementation of the
Makefile check produced thirteen false positives on English sentences, which is
exactly how a real gate acquires a reputation for crying wolf.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / "scripts"))

from docs_check import __main__ as cli  # noqa: E402
from docs_check import checks, exemptions, registry  # noqa: E402

# A minimal but genuinely parseable registry. Tests that exercise tool-name or
# action checks use this instead of the real one so a registry refactor does
# not break the gate's own tests.
MINI_REGISTRY = """package tools

import "encoding/json"

var Registry = []Tool{
\t{
\t\tName: "mpm_memory",
\t\tSchema: json.RawMessage(`{
\t\t\t"type": "object",
\t\t\t"properties": {
\t\t\t\t"action": {"type": "string", "enum": ["save","query","delete"]}
\t\t\t}
\t\t}`),
\t},
\t{
\t\tName: "mpm_context",
\t\tSchema: json.RawMessage(`{
\t\t\t"type": "object",
\t\t\t"properties": {
\t\t\t\t"action": {"type": "string", "enum": ["read_wake_context","route"]}
\t\t\t}
\t\t}`),
\t},
\t{
\t\tName: "mpm_resolve",
\t\tSchema: json.RawMessage(`{"type": "object"}`),
\t},
}
"""

MINI_COMPACT = """package main

var CompactMpmContextActionVocabulary = "read_wake_context | write_handoff | " +
\t"read_handoff | route."

func contextAdapter() {}
"""

MINI_MAKEFILE = """BUILD_DIR := .build/bin

.PHONY: build test docs-check

build:
\t@echo build

test:
\t@echo test

docs-check:
\t@echo docs
"""


class Fixture:
    """A throwaway repository-shaped tree."""

    def __init__(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)

    def close(self) -> None:
        self._tmp.cleanup()

    def write(self, relpath: str, content: str) -> Path:
        path = self.root / relpath
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(textwrap.dedent(content), encoding="utf-8")
        return path

    def with_registry(self) -> "Fixture":
        self.write("internal/core/tools/registry_list.go", MINI_REGISTRY)
        self.write("cmd/mpm-mcp/tools.go", MINI_COMPACT)
        return self

    def with_makefile(self) -> "Fixture":
        self.write("Makefile", MINI_MAKEFILE)
        return self

    def run(self, selected: set[str], base: str = "HEAD") -> checks.Report:
        return cli.run(root=self.root, selected=selected, base=base)


class GateTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self.fx = Fixture()
        self.addCleanup(self.fx.close)

    def assert_error_mentioning(self, report: checks.Report, needle: str) -> None:
        self.assertFalse(report.ok(), "expected a failing report")
        messages = [f.message for f in report.errors]
        self.assertTrue(
            any(needle in m for m in messages),
            f"no error mentioned {needle!r}; got {messages}",
        )


# ── Links ────────────────────────────────────────────────────────────────────


class TestLinks(GateTestCase):
    def test_valid_relative_link_passes(self):
        self.fx.write("docs/a.md", "[b](b.md)\n")
        self.fx.write("docs/b.md", "# b\n")
        report = self.fx.run({"links"})
        self.assertTrue(report.ok(), [f.message for f in report.errors])

    def test_broken_relative_link_fails(self):
        self.fx.write("docs/a.md", "[b](missing.md)\n")
        report = self.fx.run({"links"})
        self.assert_error_mentioning(report, "missing.md")

    def test_link_inside_a_fence_is_not_a_link(self):
        # Regression: the real repository had a code fence containing `](`
        # that a naive scanner reported as a broken link to "(".
        self.fx.write(
            "docs/a.md",
            """
            # a

            ```text
            [not a link](not-a-real-target.md)
            ```
            """,
        )
        report = self.fx.run({"links"})
        self.assertTrue(report.ok(), [f.message for f in report.errors])

    def test_external_links_are_ignored(self):
        self.fx.write("docs/a.md", "[x](https://example.com/nope) [y](mailto:a@b.c)\n")
        self.assertTrue(self.fx.run({"links"}).ok())


# ── Anchors ──────────────────────────────────────────────────────────────────


class TestAnchors(GateTestCase):
    def test_valid_anchor_passes(self):
        self.fx.write("docs/a.md", "[x](b.md#the-heading)\n")
        self.fx.write("docs/b.md", "# The heading\n")
        self.assertTrue(self.fx.run({"anchors"}).ok())

    def test_missing_anchor_fails(self):
        self.fx.write("docs/a.md", "[x](b.md#no-such-heading)\n")
        self.fx.write("docs/b.md", "# The heading\n")
        self.assert_error_mentioning(self.fx.run({"anchors"}), "no-such-heading")

    def test_explicit_html_anchor_is_accepted(self):
        self.fx.write("docs/a.md", "[x](b.md#custom)\n")
        self.fx.write("docs/b.md", '# B\n<a name="custom"></a>\n')
        self.assertTrue(self.fx.run({"anchors"}).ok())


# ── Missing paths ────────────────────────────────────────────────────────────


class TestMissingPaths(GateTestCase):
    def test_existing_path_passes(self):
        self.fx.write("docs/a.md", "See `docs/b.md`.\n")
        self.fx.write("docs/b.md", "# b\n")
        self.assertTrue(self.fx.run({"missing-paths"}).ok())

    def test_missing_path_fails(self):
        self.fx.write("docs/a.md", "See `docs/gone.md`.\n")
        self.assert_error_mentioning(self.fx.run({"missing-paths"}), "docs/gone.md")

    def test_doc_relative_path_resolves(self):
        # agent_installation/INSTALL.md says `scripts/render_managed_blocks.py`
        # meaning agent_installation/scripts/…
        self.fx.write("agent_installation/INSTALL.md", "See `scripts/render.py`.\n")
        self.fx.write("agent_installation/scripts/render.py", "\n")
        self.assertTrue(self.fx.run({"missing-paths"}).ok())

    def test_unique_suffix_resolves(self):
        self.fx.write("README.md", "See `scripts/tool.py`.\n")
        self.fx.write("agent_installation/scripts/tool.py", "\n")
        self.assertTrue(self.fx.run({"missing-paths"}).ok())

    def test_exemption_is_honoured(self):
        self.fx.write("internal/x.go", "// see docs/guide.md\n")
        self.fx.write("docs/a.md", "See `docs/guide.md`.\n")
        self.assertTrue(
            self.fx.run({"missing-paths"}).ok(),
            "docs/guide.md is a registered illustrative-fixture exemption",
        )

    def test_historical_paths_are_not_required_to_exist(self):
        self.fx.write("docs/library/old.md", "See `docs/legacy-thing.md`.\n")
        self.assertTrue(self.fx.run({"missing-paths"}).ok())


# ── Tool names ───────────────────────────────────────────────────────────────


class TestToolNames(GateTestCase):
    def setUp(self) -> None:
        super().setUp()
        self.fx.with_registry()

    def test_registered_tool_passes(self):
        self.fx.write("docs/a.md", "Run `mpm call mpm_memory --payload '{}'`.\n")
        self.assertTrue(self.fx.run({"tool-names"}).ok())

    def test_unknown_tool_fails(self):
        self.fx.write("docs/a.md", "Run `mpm call mpm_provenance --payload '{}'`.\n")
        self.assert_error_mentioning(self.fx.run({"tool-names"}), "mpm_provenance")

    def test_exempted_token_passes(self):
        self.fx.write("docs/a.md", "Run `mpm call mpm_help`.\n")
        self.assertTrue(self.fx.run({"tool-names"}).ok())

    def test_retirement_note_is_not_a_defect(self):
        self.fx.write(
            "docs/a.md",
            "The old `mpm call mpm_provenance` call was removed because that "
            "tool does not exist.\n",
        )
        self.assertTrue(self.fx.run({"tool-names"}).ok())


# ── Tool actions ─────────────────────────────────────────────────────────────


class TestToolActions(GateTestCase):
    def setUp(self) -> None:
        super().setUp()
        self.fx.with_registry()

    def test_registered_action_passes(self):
        self.fx.write("docs/a.md", "Use `mpm_memory action=save`.\n")
        self.assertTrue(self.fx.run({"tool-actions"}).ok())

    def test_unregistered_action_fails(self):
        self.fx.write("docs/a.md", "Use `mpm_memory action=query_history`.\n")
        report = self.fx.run({"tool-actions"})
        self.assert_error_mentioning(report, "query_history")
        # The message must name the valid set, or the reader still has to guess.
        self.assertTrue(any("save" in f.message for f in report.errors))

    def test_compact_only_action_is_accepted(self):
        # write_handoff is not in the registered enum but IS served by the
        # compact MCP wrapper. Rejecting it would make the gate wrong about
        # correct documentation, which is the worst failure mode available.
        self.fx.write("docs/a.md", "Use `mpm_context action=write_handoff`.\n")
        surface = registry.load(self.fx.root)
        self.assertTrue(surface.is_compact_only("mpm_context", "write_handoff"))
        self.assertTrue(self.fx.run({"tool-actions"}).ok())

    def test_tool_without_actions_is_not_flagged_for_having_none(self):
        self.fx.write("docs/a.md", "Use `mpm_resolve` with an id.\n")
        self.assertTrue(self.fx.run({"tool-actions"}).ok())


# ── Makefile targets ─────────────────────────────────────────────────────────


class TestMakeTargets(GateTestCase):
    def setUp(self) -> None:
        super().setUp()
        self.fx.with_makefile()

    def test_existing_target_in_fence_passes(self):
        self.fx.write("docs/a.md", "```bash\nmake test\n```\n")
        self.assertTrue(self.fx.run({"make-targets"}).ok())

    def test_target_with_trailing_comment_passes(self):
        self.fx.write("docs/a.md", "```bash\nmake build   # compile everything\n```\n")
        self.assertTrue(self.fx.run({"make-targets"}).ok())

    def test_inline_code_target_passes(self):
        self.fx.write("docs/a.md", "Run `make docs-check` before pushing.\n")
        self.assertTrue(self.fx.run({"make-targets"}).ok())

    def test_missing_target_fails(self):
        self.fx.write("docs/a.md", "```bash\nmake test-race\n```\n")
        self.assert_error_mentioning(self.fx.run({"make-targets"}), "test-race")

    def test_prose_that_merely_uses_the_verb_make_is_ignored(self):
        self.fx.write(
            "docs/a.md",
            """
            Do not make normal review backlog equivalent to failure.
            A scratch file must not make every build look dirty.
            The purpose is to make visible what is already captured.
            """,
        )
        self.assertTrue(
            self.fx.run({"make-targets"}).ok(),
            "English prose must not be read as a Makefile invocation",
        )

    def test_unparseable_makefile_is_an_error_not_a_pass(self):
        self.fx.write("Makefile", "# no targets at all\n")
        self.fx.write("docs/a.md", "```bash\nmake anything\n```\n")
        self.assert_error_mentioning(self.fx.run({"make-targets"}), "vacuously")


# ── Build paths ──────────────────────────────────────────────────────────────


class TestBuildPaths(GateTestCase):
    def test_correct_build_dir_passes(self):
        self.fx.write("docs/a.md", "Binaries land in `.build/bin/mpm`.\n")
        self.assertTrue(self.fx.run({"build-paths"}).ok())

    def test_install_prefix_passes(self):
        self.fx.write("docs/a.md", "The installed binary is `~/.mpm/bin/mpm`.\n")
        self.assertTrue(self.fx.run({"build-paths"}).ok())

    def test_bare_bin_dir_fails(self):
        self.fx.write("docs/a.md", "`make build` writes `bin/mpm`.\n")
        self.assert_error_mentioning(self.fx.run({"build-paths"}), "bin/mpm")


# ── Managed-block parity ────────────────────────────────────────────────────


class TestManagedBlock(GateTestCase):
    def test_missing_renderer_fails(self):
        report = self.fx.run({"managed-block"})
        self.assert_error_mentioning(report, "renderer missing")

    def test_real_repository_is_in_parity(self):
        # Drives the production renderer against the real tree.
        report = cli.run(root=REPO_ROOT, selected={"managed-block"}, base="HEAD")
        self.assertTrue(report.ok(), [f.message for f in report.errors])


# ── Library metadata ────────────────────────────────────────────────────────


class TestLibrary(GateTestCase):
    def _entry(self, name: str, label: str | None) -> None:
        header = f"> **Library status: {label}.**\n" if label else ""
        self.fx.write(f"docs/library/{name}", f"# {name}\n\n{header}\nBody.\n")

    def test_labelled_and_indexed_entry_passes(self):
        self._entry("a.md", "Current")
        self.fx.write("docs/library/README.md", "# index\n\n[a](a.md)\n")
        self.assertTrue(self.fx.run({"library-status"}).ok())

    def test_unlabelled_entry_fails(self):
        self._entry("a.md", None)
        self.fx.write("docs/library/README.md", "# index\n\n[a](a.md)\n")
        self.assert_error_mentioning(self.fx.run({"library-status"}), "no `> **Library status")

    def test_unindexed_entry_fails(self):
        self._entry("a.md", "Historical")
        self.fx.write("docs/library/README.md", "# index\n\nnothing here\n")
        self.assert_error_mentioning(self.fx.run({"library-status"}), "not listed")


# ── Retired adapters ────────────────────────────────────────────────────────


class TestRetiredAdapters(GateTestCase):
    def test_retired_adapter_path_fails(self):
        self.fx.write(
            "docs/INSTALL.md",
            "Run `agent_installation/mpm-hermes/install.sh` to wire it up.\n",
        )
        self.assert_error_mentioning(
            self.fx.run({"retired-adapter-paths"}), "mpm-hermes"
        )

    def test_retirement_sentence_passes(self):
        self.fx.write(
            "docs/INSTALL.md",
            "The `agent_installation/mpm-hermes/` adapter was retired and its "
            "directory is gone.\n",
        )
        self.assertTrue(self.fx.run({"retired-adapter-paths"}).ok())

    def test_brand_mention_is_review_not_error(self):
        self.fx.write("docs/SPEC.md", "OpenClaw receives the wake context first.\n")
        report = self.fx.run({"retired-host-mentions"})
        self.assertTrue(report.ok(), "brand mentions must not fail CI")
        self.assertTrue(any(f.check == "retired-host-mentions" for f in report.reviews))

    def test_dated_report_is_not_reviewed(self):
        self.fx.write("docs/ISSUE-21-PHASE-B-REPORT.md", "We wired OpenClaw.\n")
        report = self.fx.run({"retired-host-mentions"})
        self.assertEqual(report.reviews, [])


# ── Stale status ────────────────────────────────────────────────────────────


class TestStaleStatus(GateTestCase):
    def test_stale_phrase_is_review_only(self):
        self.fx.write("docs/SPEC.md", "The feature is awaiting approval.\n")
        report = self.fx.run({"stale-status"})
        self.assertTrue(report.ok(), "stale status must not fail CI")
        self.assertTrue(report.reviews)

    def test_historical_document_is_ignored(self):
        self.fx.write("docs/library/design.md", "The feature is awaiting approval.\n")
        self.assertEqual(self.fx.run({"stale-status"}).reviews, [])


# ── Docs impact ─────────────────────────────────────────────────────────────


class TestDocsImpact(GateTestCase):
    def _git(self, *args: str) -> None:
        subprocess.run(
            ["git", *args],
            cwd=str(self.fx.root),
            check=True,
            capture_output=True,
        )

    def _commit(self, message: str) -> None:
        self._git("add", "-A")
        self._git(
            "-c", "user.email=t@example.com", "-c", "user.name=t",
            "commit", "-q", "-m", message,
        )

    def _repo(self, *, doc_changed: bool, trailer: str = "") -> None:
        self.fx.write("README.md", "# m\n")
        self.fx.write("docs/SPEC.md", "# spec\n")
        self.fx.write("internal/core/tools/registry_list.go", MINI_REGISTRY)
        self.fx.write("cmd/mpm-mcp/tools.go", MINI_COMPACT)
        self._git("init", "-q")
        self._commit("base")

        self.fx.write(
            "internal/core/tools/registry_list.go",
            MINI_REGISTRY + "\n// touch\n",
        )
        if doc_changed:
            self.fx.write("docs/SPEC.md", "# spec\n\nnew section\n")
        body = f"change the registry\n\n{trailer}\n" if trailer else "change the registry\n"
        self._commit(body)

    def test_registry_change_without_doc_is_a_review(self):
        self._repo(doc_changed=False)
        report = self.fx.run({"docs-impact"}, base="HEAD~1")
        self.assertTrue(report.ok(), "docs-impact must never fail CI")
        self.assertTrue(any(f.check == "docs-impact" for f in report.reviews))

    def test_registry_change_with_doc_is_quiet(self):
        self._repo(doc_changed=True)
        report = self.fx.run({"docs-impact"}, base="HEAD~1")
        self.assertEqual([f for f in report.reviews if f.check == "docs-impact"], [])

    def test_justified_none_trailer_discharges_the_finding(self):
        self._repo(doc_changed=False, trailer="Docs-Impact: none — the registry entry is an alias only")
        report = self.fx.run({"docs-impact"}, base="HEAD~1")
        self.assertEqual([f for f in report.reviews if f.check == "docs-impact"], [])
        self.assertTrue(any("waived" in n for n in report.notes))

    def test_trailer_quoted_in_a_fenced_example_does_not_waive(self):
        """The gate must not be waivable by documenting the waiver syntax.

        This repository's own commit message for the gate quotes the trailer
        format inside a code fence. The first implementation scanned every
        line of every message and read that documentation as a waiver,
        suppressing a real finding — a gate that can be waived by quoting its
        own rules is not a gate.
        """
        commit_log = (
            "change the registry\n\n"
            "A finding is discharged by a commit trailer:\n\n"
            "```\n"
            "Docs-Impact: none — the registry entry is a rename of an untouched action\n"
            "```\n"
            "\n===COMMIT===\n"
        )
        self.assertIsNone(checks._impact_justification(commit_log))

    def test_trailer_in_the_middle_of_the_body_does_not_waive(self):
        commit_log = (
            "change the registry\n\n"
            "Docs-Impact: none — I thought about it\n\n"
            "Then a long explanation follows, which means the line above was "
            "prose and not a trailer block.\n"
            "\n===COMMIT===\n"
        )
        self.assertIsNone(checks._impact_justification(commit_log))

    def test_indented_trailer_does_not_waive(self):
        commit_log = "change\n\n    Docs-Impact: none — quoted\n\n===COMMIT===\n"
        self.assertIsNone(checks._impact_justification(commit_log))

    def test_real_final_paragraph_trailer_waives(self):
        commit_log = (
            "change the registry\n\nLong body.\n\n"
            "Docs-Impact: none — rename of an untouched action\n"
            "\n===COMMIT===\n"
        )
        self.assertEqual(
            checks._impact_justification(commit_log),
            ("none", "rename of an untouched action"),
        )

    def test_bare_none_without_reason_is_not_a_waiver(self):
        self._repo(doc_changed=False, trailer="Docs-Impact: none")
        report = self.fx.run({"docs-impact"}, base="HEAD~1")
        self.assertTrue(any(f.check == "docs-impact" for f in report.reviews))

    def test_unknown_base_is_reported_not_silently_passed(self):
        self._repo(doc_changed=False)
        report = self.fx.run({"docs-impact"}, base="no-such-ref")
        self.assertTrue(
            any("could not diff" in n for n in report.notes),
            "an unresolvable base must be visible, not a quiet pass",
        )


# ── Frozen snapshots ────────────────────────────────────────────────────────


class TestFrozenSnapshots(GateTestCase):
    """`agent_installation/measure/fixtures/` holds verbatim 3.0.0 text.

    Its links point at paths that existed then. Editing a snapshot to satisfy
    a link checker would destroy the only reason it is kept, so every
    structural check skips the directory — and this test proves that skipping
    is scoped, not a blanket amnesty.
    """

    def test_broken_link_inside_a_snapshot_is_ignored(self):
        self.fx.write(
            "agent_installation/measure/fixtures/snippets-3.0.0.md",
            "[gone](./mpm-memory-openclaw/README.md)\n",
        )
        self.assertTrue(self.fx.run({"links"}).ok())

    def test_retired_adapter_inside_a_snapshot_is_ignored(self):
        self.fx.write(
            "agent_installation/measure/fixtures/snippets-3.0.0.md",
            "Run `agent_installation/mpm-memory-openclaw/install.sh`.\n",
        )
        self.assertTrue(self.fx.run({"retired-adapter-paths"}).ok())

    def test_missing_path_inside_a_snapshot_is_ignored(self):
        self.fx.write(
            "agent_installation/measure/fixtures/protocol-3.0.0.md",
            "See `docs/CONTEXT_EXPOSURE.md`.\n",
        )
        self.assertTrue(self.fx.run({"missing-paths"}).ok())

    def test_the_same_broken_link_outside_a_snapshot_still_fails(self):
        # The negative control: the exemption must not have swallowed the check.
        self.fx.write(
            "docs/real.md",
            "[gone](./mpm-memory-openclaw/README.md)\n",
        )
        self.assert_error_mentioning(self.fx.run({"links"}), "mpm-memory-openclaw")


# ── Whole-gate properties ───────────────────────────────────────────────────


class TestGateProperties(unittest.TestCase):
    def test_checks_run_counts_every_selected_check(self):
        report = cli.run(root=REPO_ROOT, selected=set(cli.ALL_CHECKS), base="HEAD")
        self.assertEqual(report.checks_run, len(cli.ALL_CHECKS))

    def test_real_repository_passes(self):
        report = cli.run(root=REPO_ROOT, selected=set(cli.ALL_CHECKS), base="HEAD")
        self.assertTrue(report.ok(), [f.message for f in report.errors])

    def test_json_report_is_machine_readable(self):
        report = cli.run(root=REPO_ROOT, selected={"links"}, base="HEAD")
        payload = json.loads(report.to_json())
        self.assertIn("ok", payload)
        self.assertIn("checks_run", payload)
        self.assertEqual(payload["checks_run"], 1)

    def test_registry_parser_sees_the_real_registry(self):
        surface = registry.load(REPO_ROOT)
        self.assertEqual(surface.errors, [])
        self.assertGreaterEqual(
            len(surface.tools), 20, "registry parse has stopped seeing tools"
        )
        for expected in ("mpm_memory", "mpm_context", "mpm_work", "mpm_handoff"):
            self.assertIn(expected, surface.tools)
        self.assertIn("save", surface.tools["mpm_memory"])

    def test_gate_does_not_touch_databases_services_or_credentials(self):
        """The gate must be runnable on a clean checkout with no MPM state."""
        source = "\n".join(
            (REPO_ROOT / "scripts" / "docs_check" / name).read_text(encoding="utf-8")
            for name in ("__init__.py", "__main__.py", "checks.py", "registry.py",
                         "makefile.py", "exemptions.py")
        )
        for forbidden in (
            "import sqlite3",
            "import socket",
            "import urllib",
            "import requests",
            "import http.client",
            "subprocess.run([\"mpm",
        ):
            self.assertNotIn(forbidden, source)


if __name__ == "__main__":
    unittest.main()
