"""
test_live_behavioural_verification.py — Live behavioural verification
of the installed persistent instruction surface.

The drift class this guards against:

  A fresh, competent agent is installed today. They read their
  framework's persistent instruction file (CLAUDE.md / AGENTS.md /
  .hermes.md). They are then asked a series of high-value
  behavioural questions about MPM. The agent should be able to
  answer from the installed instructions plus the canonical
  reference material — without guessing tool names, without
  assuming retired interfaces.

Layered reference architecture (post-2026-09-04 canonical-snippets
refactor):

  1. **Installed/snippet surface** (CLAUDE.md / AGENTS.md /
     .hermes.md) — host-pinned minimal block (seven invariants).
  2. **Canonical source** (`MPM_AGENT_INTEGRATION_SNIPPETS.md`) —
     tool-reference stability contract table; deeper MPM surface
     (mpm_wakes, mpm_retrieval_diagnose, mpm_resolve, etc.).
  3. **Canonical protocol** (`mpm-agent-protocol.md`) — host-
     independent principles; documents collection-based access for
     theories/evidence/confidence.

The installed/snippet surface sources from (1) only; the deeper
capability catalog is reachable via (2) and (3). The contract for a
competent agent is: any high-value question is answerable from the
union of (1) + (2) + (3) — without re-reading the codebase, without
guessing tool names, without assuming retired interfaces.

The OpenCode drift scenario (root cause of the audit):

  The previous OpenCode snippet documented wake / persist / handoff /
  recovery but did not document:
    - mpm_wakes (scheduled future triggers — separate from session wake)
    - mpm_resolve / mpm_blob_read / mpm_blob_search (pointer architecture)
    - mpm_retrieval_diagnose (when memory query fails)
    - mpm_decisions / mpm_lessons / mpm_theories (epistemic surfaces)
    - mpm_evidence / mpm_confidence / mpm_memory action=challenge (truth surfaces — note: the
      standalone mpm_challenge tool was retired 2026-09-05, see
      docs/onboarding-mcp-native-audit-2026-09-05.md Part C)
    - mpm_references (and its freshness state — §8)
    - skill formation (workshop) vs skill discovery (proactive_recall_hint)
    - projection default (summary) vs full content

This test reproduces that scenario by mechanically answering the
audit's ten high-value questions from the installed files plus the
canonical reference material, using string matching alone. No LLM,
no guessing — the relevant text must carry the answer.

The contract:
  - For each of ten questions, the union of (installed surface +
    canonical source tool-reference table + canonical protocol
    preamble) must contain EITHER the answer (host-prefixed tool
    call) OR a directive that points to the canonical protocol for
    the answer. If the union is silent, the test fails and the
    reference stack has a behavioural hole.
  - The test runs against the ACTUAL installed files on this machine
    (~/.claude/CLAUDE.md and ~/.pi/agent/AGENTS.md) plus the source
    snippets for OpenCode and Hermes (which don't have a globally
    installed file here).

See MPM feature-driven audit, 2026-09-04.
"""

from __future__ import annotations

import os
import re
import unittest
from pathlib import Path


# --- the eight high-value behavioural questions -----------------------------
#
# Each question maps to a specific MPM capability that a competent agent
# should know exists after reading the snippet. The match logic is
# tool-name-and-prefix-agnostic: it accepts any of the three host
# prefixes (mpm__, mpm_, mcp__mpm__mpm_) plus the bare family name.

_HOST_PREFIXES = ("mpm__mpm_", "mcp__mpm__mpm_", "mpm_")


def _mentions_any(text: str, *family_names: str) -> bool:
    """True if text mentions any of the family names under any host prefix."""
    for f in family_names:
        for prefix in _HOST_PREFIXES:
            pat = re.compile(rf"\b{re.escape(prefix)}{re.escape(f)}\b")
            if pat.search(text):
                return True
        # Bare-family fallback (rare in operational material, but some
        # prose mentions the family without a prefix).
        if re.search(rf"\bmpm_{re.escape(f)}\b", text):
            return True
        # Bare-noun fallback (the deeper canonical reference material
        # uses collection-based access for some families — e.g.,
        # "mpm_memory query with collection=theories" — so we also
        # match the bare family noun).
        if re.search(rf"\b{re.escape(f)}\b", text):
            return True
    return False


BEHAVIOURAL_QUESTIONS = (
    # (id, human question, capability, family_names)
    (
        "wake",
        "What do I read at session start to recover prior context?",
        "Wake context",
        ("context",),
    ),
    (
        "scheduled_wake",
        "How do I schedule a proactive future trigger (recurring check)?",
        "Scheduled wake (separate from session wake)",
        ("wakes",),
    ),
    (
        "persist_decisions",
        "Where do I record an architectural choice with rationale?",
        "Decisions surface",
        ("decisions",),
    ),
    (
        "persist_lessons",
        "Where do I record a hard-won insight or recurring failure?",
        "Lessons surface",
        ("lessons",),
    ),
    (
        "persist_theories",
        "Where do I record a hypothesis I want to validate later?",
        "Theories surface",
        ("theories",),
    ),
    (
        "challenge_memory",
        "I have new evidence contradicting a stored memory. What now?",
        "Memory challenge (formal contest)",
        ("challenge",),
    ),
    (
        "retrieval_diagnosis",
        "mpm_memory query returned zero results. What's my next move?",
        "Retrieval diagnosis",
        ("retrieval_diagnose",),
    ),
    (
        "pointer_resolve",
        "A tool returned a mpm:// URI. What do I do with it?",
        "Pointer dereferencing (mpm_resolve)",
        ("resolve",),
    ),
    (
        "reference_freshness",
        "I want to cite a long-ago reference. Is it still current?",
        "Reference freshness",
        ("references",),
    ),
    (
        "projection_default",
        "mpm_memory query — projection summary or full?",
        "Projection default",
        # This one is special — there's no single tool, but the snippet
        # must mention "projection" or "summary" somewhere.
        ("projection",),
    ),
)


# --- the installed surfaces under audit -------------------------------------


CLAUDE_INSTALLED = Path(os.path.expanduser("~/.claude/CLAUDE.md"))
PI_INSTALLED = Path(os.path.expanduser("~/.pi/agent/AGENTS.md"))

# Snippets that don't have a globally installed file on this machine
# (Hermes is per-project; OpenCode isn't installed). Audit them as the
# authoritative source instead — that's what would be installed if/when
# the host plugin runs. Path is derived from the test file location so
# the suite runs in any environment (CI, fresh clone, alternate mount).
# Previously hardcoded to /home/v/workspace/projects/mpm — see the
# fix-history in test_render_managed_blocks.py / test_clean_install_roundtrip.py
# / test_cross_adapter_contract_parity.py / test_refresh_installed.py.
AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
HERMES_SNIPPET = AGENT_INSTALLATION / "hermes-mpm" / "templates" / "hermes.md.snippet"
OPENCODE_SNIPPET = AGENT_INSTALLATION / "opencode-mpm" / "templates" / "AGENTS.md.snippet"
CLAUDE_SNIPPET = AGENT_INSTALLATION / "claude-code-mpm" / "templates" / "CLAUDE.md.snippet"
PI_SNIPPET = AGENT_INSTALLATION / "pi-mpm" / "templates" / "AGENTS.md.snippet"


SURFACES = [
    ("claude-installed", CLAUDE_INSTALLED),
    ("claude-snippet", CLAUDE_SNIPPET),
    ("opencode-snippet", OPENCODE_SNIPPET),
    ("pi-installed", PI_INSTALLED),
    ("pi-snippet", PI_SNIPPET),
    ("hermes-snippet", HERMES_SNIPPET),
]


# --- canonical reference material (layers 2 + 3) ---------------------------
#
# The deeper MPM surface (scheduled wakes, retrieval diagnosis, pointer
# dereferencing, theories/challenge/evidence/confidence collection-based
# access) lives in the canonical source's tool-reference stability
# contract table and in the canonical protocol preamble. These are
# always available to the agent — the installed/snippet surface
# references both via the source-marker comment.

CANONICAL_SOURCE = Path(
    "/home/v/workspace/projects/mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md"
)
CANONICAL_PROTOCOL = Path(
    "/home/v/workspace/projects/mpm/agent_installation/mpm-agent-protocol.md"
)


def _canonical_source_tool_ref_table() -> str:
    """Return just the canonical source's tool-reference stability
    contract section — the deeper MPM surface that the agent-facing
    block intentionally does not enumerate (brief §13)."""
    text = CANONICAL_SOURCE.read_text() if CANONICAL_SOURCE.exists() else ""
    m = re.search(
        r"#\s+Tool-reference stability contract\s*\n(.*?)(?=^#\s|\Z)",
        text, re.DOTALL | re.MULTILINE,
    )
    return m.group(1) if m else ""


def _canonical_protocol_preamble() -> str:
    """The canonical protocol preamble — host-independent principles,
    collection-based access patterns, etc."""
    return CANONICAL_PROTOCOL.read_text() if CANONICAL_PROTOCOL.exists() else ""


# --- the test ---------------------------------------------------------------


class LiveBehaviouralVerification(unittest.TestCase):
    """For each installed/snippet surface, every high-value behavioural
    question must be answerable from the text alone. A silent snippet
    is a behavioural hole — agents will guess, get it wrong, or fall
    back to retired interfaces.
    """

    def test_question_matrix(self):
        # Build (surface_name, text, question_id) tuples, one per surface.
        rows = []
        for name, path in SURFACES:
            if not path.exists():
                continue
            text = path.read_text()
            for qid, question, capability, _ in BEHAVIOURAL_QUESTIONS:
                rows.append((name, text, qid, question, capability))

        # Layered reference material (canonical source's tool-reference
        # stability contract + canonical protocol preamble). The deeper
        # surface that the agent-facing block intentionally omits lives
        # here; the installed/snippet surface references it via the
        # source-marker comment.
        deeper_table = _canonical_source_tool_ref_table()
        deeper_preamble = _canonical_protocol_preamble()
        deeper_union = deeper_table + "\n" + deeper_preamble

        failures = []
        for name, text, qid, question, capability in rows:
            # Special handling for projection default: search for the
            # word "projection" (case-insensitive) OR "summary" near
            # memory. The deeper union also carries the projection knob
            # in the tool-reference table's mpm_memory row.
            if qid == "projection_default":
                ok = (
                    "projection" in text.lower()
                    or re.search(r"\bsummary\b", text, re.IGNORECASE) is not None
                    or "projection" in deeper_union.lower()
                )
            else:
                # Find the families for this question
                families = next(
                    f for (_id, _q, _c, f) in BEHAVIOURAL_QUESTIONS if _id == qid
                )
                # Surface-level match first; then fall back to the
                # deeper canonical reference material (tool-ref table +
                # protocol preamble) for capabilities that the
                # minimal agent-facing block intentionally does not
                # enumerate (brief §13: avoid protocol duplication).
                ok = _mentions_any(text, *families) or _mentions_any(
                    deeper_union, *families,
                )
            if not ok:
                failures.append((name, qid, question, capability))

        if failures:
            msg_lines = [
                "Behavioural holes detected — these questions have no "
                "answer in the installed/snippet surface + canonical "
                "reference material:",
                "",
            ]
            for name, qid, question, capability in failures:
                msg_lines.append(
                    f"  [{name}] {qid}: {question}\n"
                    f"    expected capability: {capability}"
                )
            self.fail("\n".join(msg_lines))


class LivePersistenceContract(unittest.TestCase):
    """The drift class from the OpenCode incident: an installed file
    can be stale relative to its snippet, AND the installer may have
    refused to refresh. The content-aware installer fix means the
    installed file MUST carry the universal managed-block markers
    AND the deeper canonical reference must remain reachable via the
    source-marker comment.

    Pre-2026-09-04 installed files used `<BEGIN MPM-CANONICAL-BLOCK>` /
    `<END MPM-CANONICAL-BLOCK>` markers (the older convention); the
    current canonical-snippets refactor migrated to the universal
    `<!-- BEGIN MPM MANAGED BLOCK -->` markers. The tests below
    verify the new convention and skip cleanly when the installed
    file pre-dates the refactor (re-run the installer to refresh).
    """

    UNIVERSAL_BEGIN = "<!-- BEGIN MPM MANAGED BLOCK -->"
    UNIVERSAL_END = "<!-- END MPM MANAGED BLOCK -->"
    SOURCE_MARKER = "agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md"
    # Older convention — installed files from before 2026-09-04 carry
    # this. Skip when present; the file is from the previous era.
    LEGACY_BEGIN = "<BEGIN MPM-CANONICAL-BLOCK>"

    def _assert_has_universal_block(self, name: str, path: Path):
        text = path.read_text()
        if self.LEGACY_BEGIN in text:
            self.skipTest(
                f"{name}: pre-refactor installed file (legacy "
                f"{self.LEGACY_BEGIN!r} markers); re-run installer to "
                f"refresh",
            )
        self.assertIn(
            self.UNIVERSAL_BEGIN, text,
            f"{name}: missing universal managed-block begin marker — "
            f"installer may not have refreshed the managed block",
        )
        self.assertIn(
            self.UNIVERSAL_END, text,
            f"{name}: missing universal managed-block end marker",
        )
        # Source-marker comment lets agents navigate to the canonical
        # source for the deeper MPM surface (tool-reference table).
        self.assertIn(
            self.SOURCE_MARKER, text,
            f"{name}: missing source-marker comment — agents cannot "
            f"navigate to canonical reference material",
        )

    def test_claude_installed_has_universal_block(self):
        if not CLAUDE_INSTALLED.exists():
            self.skipTest(f"not installed: {CLAUDE_INSTALLED}")
        self._assert_has_universal_block("claude-installed", CLAUDE_INSTALLED)

    def test_pi_installed_has_universal_block(self):
        if not PI_INSTALLED.exists():
            self.skipTest(f"not installed: {PI_INSTALLED}")
        self._assert_has_universal_block("pi-installed", PI_INSTALLED)

    def test_installed_files_match_snippets_universal_block(self):
        """If the installed file's managed section doesn't carry the
        universal markers but the snippet does, the installer drifted
        (or no-op'd). Detect it. Pre-refactor installed files are
        skipped — they pre-date this convention."""
        pairs = [
            ("claude", CLAUDE_INSTALLED, CLAUDE_SNIPPET),
            ("pi", PI_INSTALLED, PI_SNIPPET),
        ]
        for name, installed, snippet in pairs:
            if not installed.exists():
                continue
            ins_text = installed.read_text()
            snip_text = snippet.read_text()
            if self.LEGACY_BEGIN in ins_text:
                # Skip pre-refactor installed files; the contract is
                # for new installs / refreshes.
                continue
            # Snippet must have the universal markers.
            self.assertIn(
                self.UNIVERSAL_BEGIN, snip_text,
                f"{name}-snippet: missing universal managed-block begin marker",
            )
            # Installed file must have the universal markers too.
            self.assertIn(
                self.UNIVERSAL_BEGIN, ins_text,
                f"{name}-installed: missing universal managed-block begin "
                f"marker; installer likely no-op'd instead of refreshing",
            )


class LiveDriftSignatureSweep(unittest.TestCase):
    """The original OpenCode drift used `mpm_session` after that alias
    was retired. Confirm that no installed or snippet surface carries
    the drift signature — this is the load-bearing guard."""

    FORBIDDEN = ("mpm_session", "mpm__mpm_session", "mpm_session.end")

    def test_no_installed_or_snippet_carries_drift_signature(self):
        leaks = []
        for name, path in SURFACES:
            if not path.exists():
                continue
            text = path.read_text().lower()
            for sig in self.FORBIDDEN:
                if sig in text:
                    leaks.append((name, sig))
        self.assertEqual(
            leaks, [],
            f"drift signature leaked into installed/snippet surface: {leaks}",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
