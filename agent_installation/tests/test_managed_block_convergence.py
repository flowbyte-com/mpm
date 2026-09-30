"""Regression tests for the shared managed-block convergence engine.

The invariant under test:

    A host instruction file may contain at most one effective MPM
    behavioural contract after reconciliation.

These tests are deliberately written against the shapes that actually
occur on real machines, not idealized ones. The `RealShape` class in
particular reproduces the file this work was written for: an OpenClaw
`SOUL.md` carrying another host's (Pi's) managed section whose outer END
marker was never written — the state that previously caused the
installer to append a second, contradictory behavioural contract.

No test touches a real installed file. Every one builds its own fixture.
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
SCRIPTS = AGENT_INSTALLATION / "scripts"
ADAPTERS = AGENT_INSTALLATION / "mpm-memory-openclaw"
INSTALLER = ADAPTERS / "scripts" / "install_openclaw_instructions.py"
SNIPPET = ADAPTERS / "templates" / "SOUL.md.snippet"

sys.path.insert(0, str(SCRIPTS))
import managed_block_convergence as conv  # noqa: E402


SECTION = (
    "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
    "<!-- generated: 20260101T000000Z -->\n"
    "<!-- NEW SECTION -->\n"
    "<!-- BEGIN MPM MANAGED BLOCK -->\n"
    "NEW CONTRACT BODY\n"
    "<!-- END MPM MANAGED BLOCK -->\n"
    "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->\n"
)

PERSONA = (
    "\n\n***The Ghost in the Circuit***\n\n"
    "**Fiercely Architectural:** 808 doesn't just execute commands.\n"
    "It evaluates the structure underneath them.\n"
)


def outer(ident: str) -> tuple[str, str]:
    return (
        f"<!-- BEGIN MPM-MANAGED SECTION:{ident} -->",
        f"<!-- END MPM-MANAGED SECTION:{ident} -->",
    )


def inner(body: str = "OLD CONTRACT") -> tuple[str, str]:
    return (
        "<!-- BEGIN MPM MANAGED BLOCK -->",
        f"<!-- END MPM MANAGED BLOCK -->\n{body}",
    )


def wrap(ident: str, body: str) -> str:
    b, e = outer(ident)
    ib, ie = inner()
    return f"{b}\n<!-- generated: 20250101T000000Z -->\n{ib}\n{ie}\n{e}\n"


class EngineBasics(unittest.TestCase):
    """Structural parsing of every marker form MPM has ever written."""

    def test_recognizes_every_current_host_id(self):
        for ident in ("claude-code-instructions", "opencode-instructions",
                      "pi-instructions", "openclaw-instructions"):
            with self.subTest(ident=ident):
                a = conv.analyze(wrap(ident, "x"))
                self.assertEqual(a.problems, [])
                self.assertEqual(len(a.regions), 1)
                self.assertEqual(a.regions[0].host_id, ident)

    def test_recognizes_legacy_unsuffixed_section_marker(self):
        text = (
            "<!-- BEGIN MPM-MANAGED SECTION -->\n"
            "<!-- BEGIN MPM MANAGED BLOCK -->\nold\n"
            "<!-- END MPM MANAGED BLOCK -->\n"
            "<!-- END MPM-MANAGED SECTION -->\n"
        )
        a = conv.analyze(text)
        self.assertEqual(a.problems, [])
        self.assertEqual(len(a.regions), 1)
        self.assertEqual(a.regions[0].host_id, "")

    def test_recognizes_hermes_hyphenated_block_anchor(self):
        text = (
            "<!-- BEGIN MPM-MANAGED BLOCK:mpm-hermes -->\n"
            "<!-- BEGIN MPM MANAGED BLOCK -->\nold\n"
            "<!-- END MPM MANAGED BLOCK -->\n"
            "<!-- END MPM-MANAGED BLOCK:mpm-hermes -->\n"
        )
        a = conv.analyze(text)
        self.assertEqual(a.problems, [])
        self.assertEqual(a.regions[0].host_id, "mpm-hermes")

    def test_recognizes_hermes_legacy_spaced_anchor(self):
        """The ORIGINAL Hermes anchor was spaced, not hyphenated. It is
        an outer anchor precisely BECAUSE it carries a suffix — which is
        what separates it from the inner contract."""
        text = (
            "<!-- BEGIN MPM MANAGED BLOCK:hermes-mpm -->\n"
            "<!-- BEGIN MPM MANAGED BLOCK -->\nold\n"
            "<!-- END MPM MANAGED BLOCK -->\n"
            "<!-- END MPM MANAGED BLOCK:hermes-mpm -->\n"
        )
        a = conv.analyze(text)
        self.assertEqual(a.problems, [])
        self.assertEqual(len(a.regions), 1)
        self.assertEqual(a.regions[0].host_id, "hermes-mpm")
        # The nested unsuffixed contract is counted, not treated as a
        # second outer region.
        self.assertEqual(len(a.inner_pairs), 1)

    def test_unrelated_plugin_markers_are_not_ours(self):
        """We must not swallow another plugin's managed block."""
        text = (
            "<!-- BEGIN some-other-plugin:managed -->\n"
            "user prose\n"
            "<!-- END some-other-plugin:managed -->\n"
        )
        a = conv.analyze(text)
        self.assertEqual(a.regions, [])
        self.assertEqual(conv.plan_convergence(text, SECTION, {"openclaw-instructions"}).action,
                         conv.PLAN_INSERT)


class Convergence(unittest.TestCase):
    """The decision table the user's Phase 2 specifies."""

    def plan(self, text, ids=("openclaw-instructions",)):
        return conv.plan_convergence(text, SECTION, set(ids))

    def test_no_block_inserts(self):
        p = self.plan("# SOUL.md\n\npersona\n")
        self.assertEqual(p.action, conv.PLAN_INSERT)
        self.assertIn("<!-- NEW SECTION -->", p.text)
        self.assertIn("persona", p.text)

    def test_current_block_is_noop(self):
        # Build a genuinely current file by round-tripping. This host's
        # own stale block is a `replace`, not a `migrate` — the marker
        # id already matches, so nothing is being adopted.
        first = self.plan(wrap("openclaw-instructions", "x"))
        self.assertEqual(first.action, conv.PLAN_REPLACE)
        second = self.plan(first.text)
        self.assertEqual(second.action, conv.PLAN_NOOP)
        self.assertEqual(second.text, first.text)

    def test_stale_block_replaces(self):
        text = wrap("openclaw-instructions", "stale content")
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_REPLACE)
        self.assertNotIn("stale content", p.text)
        self.assertIn("<!-- NEW SECTION -->", p.text)

    def test_foreign_host_block_migrates(self):
        text = wrap("pi-instructions", "pi contract")
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_MIGRATE)
        self.assertIn("pi-instructions", p.reason)
        self.assertNotIn("pi contract", p.text)
        self.assertIn("<!-- NEW SECTION -->", p.text)

    def test_legacy_unsuffixed_block_is_replaced(self):
        """The pre-suffix `MPM-MANAGED SECTION` form carries no host id,
        so it is not attributable to a particular host. It is replaced
        in place rather than reported as a foreign-host migration, but
        the observable result is the same: one current, owned section."""
        text = (
            "<!-- BEGIN MPM-MANAGED SECTION -->\nold\n"
            "<!-- END MPM-MANAGED SECTION -->\n"
        )
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_REPLACE)
        self.assertIn("<!-- NEW SECTION -->", p.text)
        self.assertNotIn("old", p.text)

    def test_bare_contract_without_wrapper_migrates(self):
        text = "<!-- BEGIN MPM MANAGED BLOCK -->\nold\n<!-- END MPM MANAGED BLOCK -->\n"
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_MIGRATE)
        self.assertIn("<!-- NEW SECTION -->", p.text)

    def test_multiple_blocks_collapse_and_never_append(self):
        text = (
            wrap("pi-instructions", "one")
            + PERSONA
            + wrap("openclaw-instructions", "two")
        )
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_CONVERGE)
        self.assertEqual(p.text.count("<!-- NEW SECTION -->"), 1)
        # The persona between the two sections survives.
        self.assertIn("The Ghost in the Circuit", p.text)

    def test_inter_region_user_content_is_byte_identical_and_ordered(self):
        """The safety property: collapsing N MPM-owned regions into one
        must not touch the arbitrary user-authored text *between* them.

        The other multi-region test asserts only that the inter-region
        persona is still present. Presence is not enough — a splice that
        reorders two spans, re-indents one, collapses a blank-line run,
        or appends a newline still "contains" the persona. So this test
        compares the non-MPM segments of input and output as ordered
        lists of exact byte strings.

        The spans are deliberately adversarial: trailing whitespace, a
        tab, a run of blank lines, backticks, pipes, an em-dash, and text
        that itself looks like instructions. Any of those could be
        silently normalized by a careless reconstruction.
        """
        span_a = (
            "# SOUL.md\n\n"
            "**Fiercely Architectural:** 808 doesn't just execute "
            "commands.\tIt evaluates the structure underneath them.   \n\n"
            "\n\n"
            "| column | meaning |\n"
            "|--------|---------|\n"
            "Use `mpm call` — never a bare tool name.  \n"
        )
        span_b = (
            "\n<!-- a user comment that merely resembles a marker -->\n"
            "Prefer the `--json` form when parsing output. Do not call "
            "mcp_anything directly.\n\n"
        )
        span_c = "TRAILING SPAN WITH NO FINAL NEWLINE"
        text = (
            span_a
            + wrap("pi-instructions", "one")
            + span_b
            + wrap("opencode-instructions", "two")
            + span_c
            + wrap("claude-code-instructions", "three")
        )

        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_CONVERGE)

        def user_bytes(s: str) -> str:
            """Everything outside MPM-owned regions, concatenated in
            order.

            After convergence the three input regions have become one, so
            the input's three inter-region spans become one contiguous
            run. Comparing the concatenation is the honest form of the
            property: byte-for-byte and in order, with no reordering,
            no re-indenting, and no normalized whitespace.
            """
            analysis = conv.analyze(s)
            self.assertEqual(analysis.problems, [], "fixture must parse")
            parts: list[str] = []
            cursor = 0
            for r in sorted(analysis.regions, key=lambda x: x.start):
                parts.append(s[cursor:r.start])
                cursor = r.end
            parts.append(s[cursor:])
            return "".join(parts)

        self.assertEqual(
            user_bytes(p.text),
            span_a + span_b + span_c,
            "inter-region user content must survive byte-for-byte, in "
            "its original order, with nothing normalized or reordered",
        )
        # And the collapsed result is exactly one contract.
        self.assertEqual(p.text.count("<!-- NEW SECTION -->"), 1)
        self.assertEqual(
            p.text.count("<!-- BEGIN MPM MANAGED BLOCK -->"), 1
        )

    def test_repeated_reconciliation_is_byte_identical(self):
        text = PERSONA + wrap("pi-instructions", "one") + PERSONA
        first = self.plan(text)
        second = self.plan(first.text)
        self.assertEqual(second.action, conv.PLAN_NOOP)
        self.assertEqual(second.text, first.text)

    def test_user_content_outside_block_is_byte_identical(self):
        persona = (
            "# SOUL.md\n\n"
            "PERSONA LINE ONE\n\n"
            "PERSONA LINE TWO with `code` and | pipes |\n\n"
            "  trailing indentation   \n\tand a tab\n"
        )
        p = self.plan(persona + wrap("pi-instructions", "old"))
        # Everything before the region is untouched, and everything the
        # region displaced is gone. What matters is that the persona
        # bytes survive exactly, including whitespace and the em-dash /
        # backtick / pipe characters an LLM-written file tends to carry.
        self.assertTrue(p.text.startswith(persona))
        self.assertEqual(
            p.text[len(persona):], SECTION,
            "region replaced by something other than exactly the new section",
        )

    def test_ambiguous_multiple_regions_refuse_rather_than_append(self):
        """Two SECTIONS cannot both be MPM-owned regions of one file
        without ambiguity about which the operator meant. The engine
        must not silently pick one."""
        text = (
            "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\nA\n"
            "<!-- END MPM-MANAGED SECTION:pi-instructions -->\n"
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\nB\n"
        )
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_REFUSE)
        self.assertEqual(p.text, text)  # unchanged, nothing deleted
        self.assertNotIn("<!-- NEW SECTION -->", p.text)

    def test_unterminated_with_no_inner_pair_refuses(self):
        """Without a balanced inner contract there is no proof of where
        MPM's territory ends, so the true end of user content is
        unknowable. Refuse."""
        text = (
            "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\n"
            "some prose that is definitely user content\n"
        )
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_REFUSE)
        self.assertEqual(p.text, text)

    def test_unterminated_with_two_inner_pairs_refuses(self):
        text = (
            "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\n"
            + inner("one")[0] + "\none\n" + inner("one")[1]
            + inner("two")[0] + "\ntwo\n" + inner("two")[1]
        )
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_REFUSE)
        self.assertEqual(p.text, text)

    def test_truncated_inner_contract_refuses(self):
        text = (
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "<!-- BEGIN MPM MANAGED BLOCK -->\ntruncated...\n"
        )
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_REFUSE)
        self.assertEqual(p.text, text)

    def test_crossed_markers_refuse(self):
        text = (
            "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\n"
            "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->\n"
        )
        p = self.plan(text)
        self.assertEqual(p.action, conv.PLAN_REFUSE)


class RealShape(unittest.TestCase):
    """The exact file this work was written for.

    An OpenClaw `SOUL.md` carrying a Pi-rendered MPM managed block whose
    OUTER END marker was never written. Before convergence, the OpenClaw
    installer matched none of its own markers, fell through to the
    append branch, and produced a file with TWO behavioural contracts
    whose tool prefixes contradicted each other.
    """

    def _real_shape(self) -> str:
        return (
            "# SOUL.md\n\n"
            "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\n"
            "> Pi header prose that is MPM-generated, not user content.\n"
            "<!-- BEGIN MPM MANAGED BLOCK -->\n"
            "PI-RENDERED CONTRACT BODY\n"
            "<!-- END MPM MANAGED BLOCK -->\n"
            + PERSONA
        )

    def test_analyses_as_malformed_foreign(self):
        a = conv.analyze(self._real_shape())
        self.assertTrue(a.problems)
        # ...but the damage is deterministically repairable.
        self.assertIsNotNone(conv.repair_extent_or_none(self._real_shape()))

    def test_converges_to_exactly_one_block(self):
        p = conv.plan_convergence(
            self._real_shape(), SECTION, {"openclaw-instructions"},
        )
        self.assertEqual(p.action, conv.PLAN_REPAIR)
        self.assertEqual(p.text.count("<!-- BEGIN MPM-MANAGED SECTION:"), 1)
        self.assertEqual(p.text.count("<!-- BEGIN MPM MANAGED BLOCK -->"), 1)
        self.assertNotIn("PI-RENDERED CONTRACT BODY", p.text)
        self.assertNotIn("pi-instructions", p.text)
        self.assertIn("NEW CONTRACT BODY", p.text)

    def test_persona_preserved_exactly(self):
        p = conv.plan_convergence(
            self._real_shape(), SECTION, {"openclaw-instructions"},
        )
        self.assertIn("***The Ghost in the Circuit***", p.text)
        self.assertIn("**Fiercely Architectural:**", p.text)
        self.assertTrue(p.text.startswith("# SOUL.md"))

    def test_second_pass_is_byte_identical(self):
        first = conv.plan_convergence(
            self._real_shape(), SECTION, {"openclaw-instructions"},
        )
        second = conv.plan_convergence(
            first.text, SECTION, {"openclaw-instructions"},
        )
        self.assertEqual(second.action, conv.PLAN_NOOP)
        self.assertEqual(second.text, first.text)


class InstallerEndToEnd(unittest.TestCase):
    """Drive the real OpenClaw installer over fixture files.

    These run the actual script as a subprocess so the contract under
    test is the one `make refresh-installed` exercises, not a
    reimplementation of it.
    """

    def run_installer(self, target: Path, *extra: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, str(INSTALLER),
             "--home", str(target.parent),
             "--target", str(target),
             "--snippet", str(SNIPPET), *extra],
            capture_output=True, text=True,
        )

    def test_repairs_foreign_malformed_section_on_disk(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "SOUL.md"
            target.write_text(
                "# SOUL.md\n\n"
                "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\n"
                "> pi header\n"
                "<!-- BEGIN MPM MANAGED BLOCK -->\nold\n"
                "<!-- END MPM MANAGED BLOCK -->\n"
                + PERSONA,
                encoding="utf-8",
            )
            r = self.run_installer(target)
            self.assertEqual(r.returncode, 0, r.stderr)
            text = target.read_text(encoding="utf-8")
            self.assertEqual(
                text.count("<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->"), 1,
            )
            self.assertEqual(text.count("<!-- BEGIN MPM MANAGED BLOCK -->"), 1)
            self.assertNotIn("mcp__mpm__", text)
            self.assertIn("The Ghost in the Circuit", text)

    def test_never_writes_a_second_contract(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "SOUL.md"
            target.write_text(
                wrap("pi-instructions", "one") + PERSONA
                + wrap("openclaw-instructions", "two"),
                encoding="utf-8",
            )
            r = self.run_installer(target)
            self.assertEqual(r.returncode, 0, r.stderr)
            text = target.read_text(encoding="utf-8")
            self.assertEqual(text.count("<!-- BEGIN MPM MANAGED BLOCK -->"), 1)

    def test_backup_written_before_destructive_change(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "SOUL.md"
            original = wrap("pi-instructions", "old") + PERSONA
            target.write_text(original, encoding="utf-8")
            r = self.run_installer(target)
            self.assertEqual(r.returncode, 0, r.stderr)
            backups = list(Path(tmp).glob("SOUL.md.bak.*"))
            self.assertEqual(len(backups), 1, "no backup was taken")
            self.assertEqual(backups[0].read_text(encoding="utf-8"), original)

    def test_refuses_on_ambiguous_markers_without_writing(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "SOUL.md"
            ambiguous = (
                "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\n"
                "prose that is user content\n"
            )
            target.write_text(ambiguous, encoding="utf-8")
            r = self.run_installer(target)
            self.assertEqual(r.returncode, 3, r.stdout + r.stderr)
            self.assertEqual(
                target.read_text(encoding="utf-8"), ambiguous,
                "installer modified an ambiguous file",
            )
            self.assertEqual(list(Path(tmp).glob("*.bak.*")), [])

    def test_idempotent_second_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "SOUL.md"
            target.write_text(PERSONA, encoding="utf-8")
            self.assertEqual(self.run_installer(target).returncode, 0)
            once = target.read_bytes()
            self.assertEqual(self.run_installer(target).returncode, 0)
            self.assertEqual(target.read_bytes(), once)

    def test_uninstall_removes_foreign_section_too(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "SOUL.md"
            target.write_text(
                wrap("pi-instructions", "old") + PERSONA, encoding="utf-8",
            )
            r = self.run_installer(target, "--uninstall")
            self.assertEqual(r.returncode, 0, r.stderr)
            text = target.read_text(encoding="utf-8")
            self.assertNotIn("MPM MANAGED BLOCK", text)
            self.assertIn("The Ghost in the Circuit", text)


class IntegrationModeRendering(unittest.TestCase):
    """The CLI-fallback fixture must not receive unusable identifiers.

    Phase 4 requires proving that reconciliation does not introduce
    `mcp__...` tool names on a host that has no MCP transport. This is
    the assertion that would have caught the original defect.
    """

    def _cli_fallback_home(self, root: Path) -> Path:
        """Reproduce the real machine's CLI-fallback shape: no plugin,
        no MCP server, `mpm` on PATH, workspace naming MPM."""
        (root / ".mpm" / "bin").mkdir(parents=True)
        (root / ".mpm" / "bin" / "mpm").write_text("#!/bin/sh\n", encoding="utf-8")
        ws = root / ".openclaw" / "workspace"
        ws.mkdir(parents=True)
        (root / ".openclaw" / "openclaw.json").write_text(
            '{"agents": {"defaults": {"workspace": "%s"}}}' % ws,
            encoding="utf-8",
        )
        (ws / "AGENTS.md").write_text(
            "MPM is your native SQLite-backed persistence layer. "
            "Use `mpm call <tool> --payload` to reach it.\n",
            encoding="utf-8",
        )
        return ws / "SOUL.md"

    def test_cli_fallback_gets_bare_names_not_mcp_prefix(self):
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp)
            target = self._cli_fallback_home(home)
            target.write_text("# SOUL.md\n" + PERSONA, encoding="utf-8")
            r = self.run_installer(target)
            self.assertEqual(r.returncode, 0, r.stderr)
            text = target.read_text(encoding="utf-8")
            # The behavioural contract names the capability bare...
            self.assertIn("fetch it through `mpm_context`", text)
            # ...and never with an MCP-client transport prefix, because
            # this host has no MCP server and no such namespace.
            self.assertNotIn("mcp__mpm__mpm_context", text)
            self.assertNotIn("mcp__", text)
            # The host-specific footer still teaches the working call.
            self.assertIn("mpm call mpm_context", text)

    def test_native_plugin_mode_also_gets_bare_names(self):
        """Plugin mode registers only mpm_memory_search / mpm_memory_get
        and reaches mpm_context by shelling out, so the prefix would be
        wrong there too."""
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp)
            (home / ".openclaw" / "extensions" / "mpm-memory-openclaw").mkdir(
                parents=True,
            )
            ws = home / ".openclaw" / "workspace"
            ws.mkdir(parents=True)
            target = ws / "SOUL.md"
            target.write_text("# SOUL.md\n" + PERSONA, encoding="utf-8")
            r = self.run_installer(target)
            self.assertEqual(r.returncode, 0, r.stderr)
            text = target.read_text(encoding="utf-8")
            self.assertIn("fetch it through `mpm_context`", text)
            self.assertNotIn("mcp__", text)

    def test_snippet_itself_carries_no_mcp_prefix(self):
        """Guards the render, not just the install: the checked-in
        snippet must not reintroduce the prefix."""
        self.assertNotIn("mcp__", SNIPPET.read_text(encoding="utf-8"))

    def run_installer(self, target: Path, *extra: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, str(INSTALLER),
             "--home", str(target.parent),
             "--target", str(target),
             "--snippet", str(SNIPPET), *extra],
            capture_output=True, text=True,
        )


if __name__ == "__main__":
    unittest.main()
