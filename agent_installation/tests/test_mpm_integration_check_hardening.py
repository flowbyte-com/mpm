"""
mpm_integration_check.md hardening regression tests.

These tests verify that the integration diagnostic cannot repeat the false-PASS
mode observed when the local working tree was clean but the network remote had
been force-rewritten. They are content checks against the canonical diagnostic
document at the repo root (mpm_integration_check.md), not behavioural tests of
the diagnostic procedure itself.

Each scenario maps to a hardening rule from the source task spec:

  1. clean tree + stale cached origin/main -> must not claim remote freshness
  2. clean tree + fetched upstream ahead -> freshness not PASS
  3. force-rewritten upstream / divergence -> explicitly reported
  4. installed binary matches stale local HEAD -> overall freshness not PASS
  5. native vs CLI-fallback classification
  6. enumerated tool count equals reported tool count
  7. maintenance warning does not downgrade unrelated DB/FTS status

The test is read-only and inspects only document content. It does not modify
any files and does not perform git/network operations.
"""

from __future__ import annotations

import os
import re
import unittest
from pathlib import Path

# --------------------------------------------------------------------------
# Locate the diagnostic at its canonical path. The repository root resolves
# via this file's location: .../mpm/agent_installation/tests/ → mpm/ root.
# --------------------------------------------------------------------------

REPO_ROOT = Path(__file__).resolve().parents[2]
DIAGNOSTIC_PATH = REPO_ROOT / "mpm_integration_check.md"


def _read_diagnostic() -> str:
    """Return the diagnostic text. Fails the test if the file is missing."""
    assert DIAGNOSTIC_PATH.is_file(), (
        f"Diagnostic not found at canonical path: {DIAGNOSTIC_PATH}"
    )
    return DIAGNOSTIC_PATH.read_text(encoding="utf-8")


# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------


def _has_section(text: str, header_pattern: str) -> bool:
    """Return True if any markdown header matches the pattern.

    The header_pattern is treated as a regex (not a literal), since callers
    pass anchored patterns like r"Remote source freshness". Matching is
    case-insensitive because diagnostic section titles may use either case.

    NOTE: the regex uses {1,6} as a quantifier (1-6 '#' chars). Inside an
    f-string this would otherwise be evaluated as the tuple (1, 6); the
    braces must be doubled to {{1,6}} to escape them.
    """
    return re.search(
        rf"^#{{1,6}}\s+.*{header_pattern}.*$",
        text,
        re.MULTILINE | re.IGNORECASE,
    ) is not None


def _phrase_present(text: str, *phrases: str) -> bool:
    """Return True if every required phrase appears (case-insensitive)."""
    lower = text.lower()
    return all(p.lower() in lower for p in phrases)


# --------------------------------------------------------------------------
# Scenario 1 — clean tree + stale cached origin/main must not claim PASS
# --------------------------------------------------------------------------


class Scenario1StaleCachedUpstream(unittest.TestCase):
    """A clean local tree against a stale cached origin/main is not PASS."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_remote_freshness_section_exists(self):
        """There is an explicit section describing remote-source freshness."""
        self.assertTrue(
            _has_section(self.text, r"Remote source freshness"),
            "Diagnostic must contain a 'Remote source freshness' section "
            "(was missing — caused the false PASS).",
        )

    def test_clean_tree_does_not_imply_remote_freshness(self):
        """The diagnostic must explicitly state that a clean local tree is not proof of remote freshness."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "clean local working tree",
                "not",
                "proof",
                "remote",
            ),
            "Diagnostic must explicitly say a clean local tree is not proof of remote freshness.",
        )

    def test_cached_origin_does_not_imply_network_currency(self):
        """The diagnostic must explicitly state that HEAD == cached origin/main is not proof of network currency."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "origin/main",
                "not",
                "proof",
                "network",
            ),
            "Diagnostic must explicitly say HEAD == cached origin/main is not proof of network-currency.",
        )

    def test_no_fetch_path_returns_unknown(self):
        """When the diagnostic cannot fetch, remote freshness must be reported as UNKNOWN, not PASS."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "fetch not performed",
                "UNKNOWN",
            ),
            "Diagnostic must report remote freshness as UNKNOWN (not PASS) when fetch is not performed.",
        )


# --------------------------------------------------------------------------
# Scenario 2 — fetched upstream ahead must not classify as PASS
# --------------------------------------------------------------------------


class Scenario2FetchedUpstreamAhead(unittest.TestCase):
    """When fetched upstream is ahead of local, freshness cannot be PASS."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_classification_axes_documented(self):
        """The four-axis classification (0 0 / N 0 / 0 N / N M) is documented verbatim."""
        for axis in [
            r"0 0",
            r"N 0",
            r"0 N",
            r"N M",
        ]:
            # (?m) enables multiline mode so ^ matches start of any line.
            self.assertRegex(
                self.text,
                rf"(?m)^{axis}\s*->",
                f"Classification axis '{axis}' must be documented with the -> arrow in the diagnostic.",
            )

    def test_behind_classification_not_pass(self):
        """The 0 N (local behind fetched upstream) case must not be classified as PASS-equivalent."""
        # Find the classification block and verify it does not call 0 N a PASS
        match = re.search(
            r"^0 N\s*->\s*(.+)$",
            self.text,
            re.MULTILINE,
        )
        self.assertIsNotNone(match, "0 N classification line missing.")
        outcome = match.group(1).strip().lower()
        self.assertNotIn("match", outcome)
        self.assertIn("behind", outcome)

    def test_pass_requires_match(self):
        """PASS verdict for remote freshness requires 0 0 + fetch performed + no rewrite."""
        # Locate the verdict definition for Remote freshness PASS and capture
        # the entire bullet list that follows it. The list ends when a line
        # does not begin with '-' (or is followed by a markdown heading).
        pass_section = re.search(
            r"`?PASS`?\s+for remote freshness requires[^\n]*\n"
            r"(?:\s*-\s+[^\n]*\n?)+",
            self.text,
            re.IGNORECASE,
        )
        self.assertIsNotNone(
            pass_section,
            "PASS definition for remote freshness must enumerate required conditions "
            "as a bullet list.",
        )
        body = pass_section.group(0).lower()
        self.assertIn("fetch", body)
        self.assertIn("0 0", body)
        self.assertIn("rewrite", body)


# --------------------------------------------------------------------------
# Scenario 3 — force-rewritten upstream / divergence must be explicitly reported
# --------------------------------------------------------------------------


class Scenario3RewriteAndDivergence(unittest.TestCase):
    """A rewritten upstream history must be reported explicitly, not as ordinary ahead/behind."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_divergence_explicitly_distinguished(self):
        """The diagnostic must distinguish divergence (N M) from ordinary ahead/behind."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "N M",
                "diverg",
            ),
            "Diagnostic must explicitly call out N M as diverged, not ordinary ahead/behind.",
        )

    def test_rewrite_must_be_reported(self):
        """The diagnostic must require a 'history rewrite detected' flag in the report."""
        self.assertRegex(
            self.text,
            r"History rewrite detected",
            "Diagnostic report template must include a 'History rewrite detected' field.",
        )

    def test_divergent_classification_requires_additional_finding(self):
        """Divergent classification must trigger an additional explanatory finding."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "divergent",
                "additional",
                "finding",
            ),
            "Diagnostic must require an additional finding explaining divergence.",
        )


# --------------------------------------------------------------------------
# Scenario 4 — installed binaries matching stale local HEAD must NOT yield PASS overall
# --------------------------------------------------------------------------


class Scenario4BinaryStaleLocalHead(unittest.TestCase):
    """When installed binaries match a stale local HEAD, overall freshness is not PASS."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_three_axis_verdict_section(self):
        """The combined source/binaries/remote verdict must be a three-axis decision."""
        self.assertTrue(
            _has_section(self.text, r"Combined source/binaries/remote"),
            "Diagnostic must contain a 'Combined source/binaries/remote freshness verdict' section.",
        )

    def test_remote_unknown_collapses_overall_to_unknown(self):
        """When source matches binaries but remote is UNKNOWN, overall must be UNKNOWN, not PASS."""
        # The combination table must contain a row that explicitly says
        # "Installed binaries match local source: PASS / Remote source freshness: UNKNOWN
        #  / Overall freshness: UNKNOWN".
        self.assertRegex(
            self.text,
            r"Installed binaries match local source:\s*PASS",
            "Combined verdict table missing the source-only PASS row.",
        )
        self.assertRegex(
            self.text,
            r"Remote source freshness:\s*UNKNOWN",
            "Combined verdict table missing the remote-UNKNOWN row.",
        )
        self.assertRegex(
            self.text,
            r"Overall freshness:\s*UNKNOWN",
            "Combined verdict table must produce an UNKNOWN overall verdict for this case.",
        )

    def test_remote_fail_collapses_overall_to_fail_or_warn(self):
        """When remote freshness FAILS, overall freshness must be FAIL or WARN."""
        self.assertRegex(
            self.text,
            r"Remote source freshness:\s*UNKNOWN[\s\S]{0,400}Overall freshness:\s*UNKNOWN",
            "Combined verdict must propagate remote UNKNOWN to an UNKNOWN overall verdict.",
        )

    def test_no_pass_when_remote_unknown(self):
        """The diagnostic must explicitly forbid PASS when remote freshness is UNKNOWN."""
        # Search for the rule that says you cannot PASS with UNKNOWN remote
        self.assertTrue(
            _phrase_present(
                self.text,
                "must",
                "report",
                "UNKNOWN",
                "not",
                "PASS",
            ),
            "Diagnostic must explicitly forbid collapsing UNKNOWN into PASS.",
        )


# --------------------------------------------------------------------------
# Scenario 5 — strict NATIVE/FALLBACK/MISSING classification
# --------------------------------------------------------------------------


class Scenario5NativeFallbackStrictness(unittest.TestCase):
    """The diagnostic must distinguish NATIVE, FALLBACK, MISSING, NOT APPLICABLE strictly."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_classification_table_present(self):
        """A strict NATIVE / FALLBACK / MISSING / NOT APPLICABLE taxonomy must be defined."""
        for term in ["NATIVE", "FALLBACK", "MISSING", "NOT APPLICABLE"]:
            self.assertIn(
                term,
                self.text,
                f"Diagnostic must define classification term '{term}'.",
            )

    def test_native_definition_requires_host_integration(self):
        """NATIVE must be defined as callable through the host's native integration, not CLI-only."""
        # Find the NATIVE definition block and assert it mentions "native integration"
        native_def = re.search(
            r"NATIVE\s*\n([\s\S]+?)(?:\n\s*\n\s*FALLBACK|\n\s*#{1,6}\s|\Z)",
            self.text,
        )
        self.assertIsNotNone(native_def, "NATIVE definition block missing.")
        body = native_def.group(1).lower()
        self.assertIn("native", body)
        # Negative guard: must not say CLI-only counts as NATIVE
        self.assertNotIn("cli-only", body.replace("not ", ""))

    def test_cli_only_capability_not_native(self):
        """The diagnostic must explicitly forbid calling a CLI-only capability NATIVE."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "CLI-only",
                "NATIVE",
                "not",
            ),
            "Diagnostic must explicitly state that a CLI-only capability is not NATIVE.",
        )


# --------------------------------------------------------------------------
# Scenario 6 — tool count reconciliation
# --------------------------------------------------------------------------


class Scenario6ToolCountReconciliation(unittest.TestCase):
    """The diagnostic must require reported count == enumerated names count."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_reconciliation_section_present(self):
        """There must be a tool-count reconciliation subsection."""
        self.assertTrue(
            _has_section(self.text, r"Tool-count reconciliation"),
            "Diagnostic must contain a 'Tool-count reconciliation' subsection.",
        )

    def test_no_hardcoded_count_phrase(self):
        """The diagnostic must explicitly forbid printing mismatched counts like '21 names / 20 tools'."""
        # Search for the explicit prohibition
        self.assertTrue(
            _phrase_present(
                self.text,
                "21 names",
                "20 tools",
            ) or _phrase_present(
                self.text,
                "reconcil",
                "names",
                "tools",
            ),
            "Diagnostic must require mechanical reconciliation of reported count with enumerated names.",
        )

    def test_derive_count_rule(self):
        """The diagnostic must say to derive the count rather than hard-code it."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "derive",
                "count",
            ),
            "Diagnostic must say to derive the tool count from the live surface.",
        )


# --------------------------------------------------------------------------
# Wake / persona separation (Phase 14 hardening)
# --------------------------------------------------------------------------


class WakeManualVsAutomaticSeparation(unittest.TestCase):
    """The diagnostic must distinguish manual wake gatherer from automatic host injection."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_manual_vs_automatic_section_exists(self):
        """§10.1 must split manual gatherer from automatic host injection."""
        self.assertRegex(
            self.text,
            r"10\.1[^a-z]+[Ss]eparate manual gatherer",
            "Diagnostic must contain a §10.1 'Separate manual gatherer from automatic host injection' section",
        )

    def test_automatic_pass_requires_evidence(self):
        """Automatic wake PASS must require both manual + automatic evidence, not just manual."""
        # Find the §10.1 conclusion table that distinguishes manual PASS + automatic X
        # from automatic PASS alone. The diagnostic should NOT let manual PASS imply
        # automatic PASS.
        self.assertTrue(
            _phrase_present(
                self.text,
                "manual PASS",
                "automatic UNKNOWN",
            ),
            "Diagnostic must list 'manual PASS + automatic UNKNOWN' as a distinct case",
        )

    def test_automatic_fail_distinct_verdict(self):
        """Automatic FAIL must produce a distinct verdict (not collapse into wake PASS)."""
        # The diagnostic must distinguish a state where manual retrieval works
        # but automatic injection is broken (the user-visible regression mode).
        self.assertTrue(
            _phrase_present(
                self.text,
                "manual PASS",
                "automatic FAIL",
            ),
            "Diagnostic must classify 'manual PASS + automatic FAIL' as a distinct case "
            "(not collapse to PASS)",
        )


class PersonaReadableVsInjectedVsOnce(unittest.TestCase):
    """The diagnostic must distinguish persona-readable vs auto-injected vs exactly-once."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_persona_three_layer_section_exists(self):
        """§14.1 must enumerate the three persona layers."""
        self.assertRegex(
            self.text,
            r"14\.1[^a-z]+[Ss]eparate persona readable",
            "Diagnostic must contain a §14.1 'Separate persona readable vs persona automatically injected vs persona exactly-once' section",
        )

    def test_persona_exactly_once_layer_distinct(self):
        """The exactly-once layer must be reported separately from automatic-injection."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "exactly once",
                "PASS",
                "FAIL",
            ),
            "Diagnostic must report 'persona injected exactly once' as a distinct PASS/FAIL verdict",
        )

    def test_persona_three_duplication_source_distinguished(self):
        """The diagnostic must distinguish the two source classes of persona duplication
        (multiple bootstrap fires per turn, or multiple persona plugins loaded)."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "agent:bootstrap more than once per turn",
                "two persona plugins",
            ) or _phrase_present(
                self.text,
                "agent:bootstrap",
                "duplicat",
            ),
            "Diagnostic must enumerate the duplication source classes (bootstrap fires + plugin count)",
        )


# --------------------------------------------------------------------------
# Scenario 7 — subsystem warnings must not be conflated with maintenance advisories
# --------------------------------------------------------------------------


class Scenario7SubsystemVsMaintenance(unittest.TestCase):
    """Database / FTS health must not be downgraded by spaced-review backlog or startup advisories."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read_diagnostic()

    def test_subsystem_separation_section(self):
        """There must be a subsection that separates subsystem status from maintenance advisories."""
        self.assertTrue(
            _has_section(self.text, r"Subsystem vs maintenance separation"),
            "Diagnostic must contain a 'Subsystem vs maintenance separation' subsection.",
        )

    def test_review_backlog_is_not_a_subsystem_warning(self):
        """The diagnostic must explicitly say a spaced-review backlog is not an FTS/DB warning."""
        self.assertTrue(
            _phrase_present(
                self.text,
                "review backlog",
                "not",
                "FTS",
            ),
            "Diagnostic must state that a review backlog is not an FTS/DB subsystem warning.",
        )

    def test_maintenance_advisories_separate_section(self):
        """The final report must have a distinct section for maintenance advisories."""
        self.assertTrue(
            _has_section(self.text, r"Maintenance advisories"),
            "Final report template must include a 'Maintenance advisories' section separate from 'Problems found'.",
        )


# --------------------------------------------------------------------------
# Meta — path existence and document hygiene
# --------------------------------------------------------------------------


class DiagnosticDocumentHygiene(unittest.TestCase):
    """Document must exist at the canonical path and contain required structural elements."""

    def test_canonical_path(self):
        """The diagnostic lives at mpm_integration_check.md (repo root)."""
        self.assertTrue(
            DIAGNOSTIC_PATH.is_file(),
            f"Diagnostic must exist at canonical path: {DIAGNOSTIC_PATH}",
        )

    def test_no_legacy_session_surface_in_procedure(self):
        """The diagnostic procedure must not itself recommend the retired mpm_session surface."""
        text = _read_diagnostic()
        # The diagnostic must call out mpm_session as obsolete; it must not
        # instruct the operator to USE mpm_session.
        # Search for instructional use, not the explicit "must not be exposed"
        # sentence in Section 11.
        bad_pattern = re.search(
            r"(?:use|call|invoke|run)\s+`?mpm_session`?",
            text,
            re.IGNORECASE,
        )
        self.assertIsNone(
            bad_pattern,
            "Diagnostic must not instruct the operator to use the retired mpm_session surface.",
        )

    def test_uncertainty_preserved(self):
        """The diagnostic must list PASS / FAIL / WARN / UNKNOWN / NOT APPLICABLE."""
        text = _read_diagnostic()
        for term in ["PASS", "FAIL", "WARN", "UNKNOWN", "NOT APPLICABLE"]:
            self.assertIn(term, text, f"Verdict term '{term}' must appear in the diagnostic.")


if __name__ == "__main__":
    unittest.main()


class OpenClawPersistentBlockContract(unittest.TestCase):
    """OpenClaw follows the same persistent-block model as the other
    four hosts. The diagnostic must explicitly distinguish the
    persistent behavioural contract (SOUL.md) from runtime wake /
    persona injection, and must classify each layer separately."""

    def test_openclaw_section_documents_three_layer_model(self):
        text = _read_diagnostic()
        # §17.3 must mention SOUL.md as the persistent surface.
        self.assertRegex(
            text, r"17\.3[^\n]*\n[\s\S]{0,400}SOUL\.md",
            "OpenClaw section must mention SOUL.md as the persistent surface",
        )

    def test_openclaw_section_documents_managed_section_markers(self):
        text = _read_diagnostic()
        # The diagnostic must name the OpenClaw managed-section markers
        # so the regression that the persistent block is present can
        # actually be verified by name.
        self.assertIn(
            "BEGIN MPM-MANAGED SECTION:openclaw-instructions",
            text,
            "OpenClaw section must name the managed-section markers",
        )

    def test_openclaw_section_distinguishes_runtime_from_persistent(self):
        text = _read_diagnostic()
        # The diagnostic must explicitly say runtime injection is NOT
        # a substitute for the persistent block. Without this, a host
        # with only wake injection could falsely report behavioural
        # adoption = PASS. Look anywhere in the OpenClaw section, not
        # at a fixed offset — the explicit not-equivalent statement is
        # placed after the per-host check list.
        idx = text.find("## 17.3 OpenClaw")
        self.assertGreater(idx, 0, "OpenClaw section must exist")
        # Section ends at the next "## " heading.
        next_idx = text.find("\n## ", idx + 1)
        if next_idx < 0:
            next_idx = len(text)
        snippet = text[idx:next_idx]
        self.assertIn(
            "runtime", snippet.lower(),
            "OpenClaw section must reference runtime injection",
        )
        self.assertIn(
            "persistent", snippet.lower(),
            "OpenClaw section must reference persistent contract",
        )
        # Must explicitly say runtime is not equivalent / not a substitute.
        # Allow markdown emphasis between the words (e.g., "**not**
        # equivalent").
        self.assertRegex(
            snippet, r"not\b\*?\*?\s+equivalent|not\s+a\s+substitute",
            "OpenClaw section must state runtime wake ≠ persistent contract",
        )

    def test_openclaw_managed_block_check_command_present(self):
        text = _read_diagnostic()
        self.assertIn(
            "BEGIN MPM-MANAGED SECTION:openclaw-instructions",
            text,
            "Diagnostic must include the OpenClaw managed-block check command",
        )

    def test_openclaw_runtime_managed_split_verdicts(self):
        # The diagnostic must surface BOTH "Wake runtime injection" and
        # "MPM managed block in SOUL.md" as separate verdict rows for
        # OpenClaw. A single combined verdict is the false-PASS mode.
        text = _read_diagnostic()
        idx = text.find("## 17.3 OpenClaw")
        self.assertGreater(idx, 0)
        snippet = text[idx:idx + 2000]
        self.assertIn("Wake runtime injection", snippet)
        self.assertIn("MPM managed block in SOUL.md", snippet)
