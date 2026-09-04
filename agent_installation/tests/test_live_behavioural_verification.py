"""
test_live_behavioural_verification.py — Live behavioural verification
of the installed persistent instruction surface.

The drift class this guards against:

  A fresh, competent agent is installed today. They read their
  framework's persistent instruction file (CLAUDE.md / AGENTS.md /
  .hermes.md). They are then asked a series of high-value
  behavioural questions about MPM. The agent should be able to
  answer from the installed instructions alone — without re-reading
  the canonical protocol, without guessing tool names, without
  assuming retired interfaces.

The OpenCode drift scenario (root cause of the audit):

  The previous OpenCode snippet documented wake / persist / handoff /
  recovery but did not document:
    - mpm_wakes (scheduled future triggers — separate from session wake)
    - mpm_resolve / mpm_blob_read / mpm_blob_search (pointer architecture)
    - mpm_retrieval_diagnose (when memory query fails)
    - mpm_decisions / mpm_lessons / mpm_theories (epistemic surfaces)
    - mpm_evidence / mpm_confidence / mpm_challenge (truth surfaces)
    - mpm_references (and its freshness state — §8)
    - skill formation (workshop) vs skill discovery (proactive_recall_hint)
    - projection default (summary) vs full content

  An agent receiving those snippet instructions, asked "where do I
  record a hard-won insight?", would not know `mpm_lessons` exists.
  Asked "how do I schedule a follow-up check tomorrow?", they would
  not know `mpm_wakes` exists. Asked "I queried memory and got
  nothing — what now?", they would reformulate instead of using
  `mpm_retrieval_diagnose`.

This test reproduces that scenario by mechanically answering the
audit's eight high-value questions from the installed files, using
string matching alone. No LLM, no guessing — the installed text must
carry the answer.

The contract:
  - For each of eight questions, the installed file must contain
    EITHER the answer (host-prefixed tool call) OR a directive that
    points to the canonical protocol for the answer. If the file is
    silent, the test fails and the snippet has a behavioural hole.
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


CLAUDE_INSTALLED = Path("/home/v/.claude/CLAUDE.md")
PI_INSTALLED = Path("/home/v/.pi/agent/AGENTS.md")

# Snippets that don't have a globally installed file on this machine
# (Hermes is per-project; OpenCode isn't installed). Audit them as the
# authoritative source instead — that's what would be installed if/when
# the host plugin runs.
HERMES_SNIPPET = Path(
    "/home/v/workspace/projects/mpm/agent_installation/hermes-mpm/templates/hermes.md.snippet"
)
OPENCODE_SNIPPET = Path(
    "/home/v/workspace/projects/mpm/agent_installation/opencode-mpm/templates/AGENTS.md.snippet"
)
CLAUDE_SNIPPET = Path(
    "/home/v/workspace/projects/mpm/agent_installation/claude-code-mpm/templates/CLAUDE.md.snippet"
)
PI_SNIPPET = Path(
    "/home/v/workspace/projects/mpm/agent_installation/pi-mpm/templates/AGENTS.md.snippet"
)


SURFACES = [
    ("claude-installed", CLAUDE_INSTALLED),
    ("claude-snippet", CLAUDE_SNIPPET),
    ("opencode-snippet", OPENCODE_SNIPPET),
    ("pi-installed", PI_INSTALLED),
    ("pi-snippet", PI_SNIPPET),
    ("hermes-snippet", HERMES_SNIPPET),
]


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

        failures = []
        for name, text, qid, question, capability in rows:
            # Special handling for projection default: search for the
            # word "projection" (case-insensitive) OR "summary" near
            # memory.
            if qid == "projection_default":
                ok = (
                    "projection" in text.lower()
                    or re.search(r"\bsummary\b", text, re.IGNORECASE) is not None
                )
            else:
                # Find the families for this question
                families = next(
                    f for (_id, _q, _c, f) in BEHAVIOURAL_QUESTIONS if _id == qid
                )
                ok = _mentions_any(text, *families)
            if not ok:
                failures.append((name, qid, question, capability))

        if failures:
            msg_lines = [
                "Behavioural holes detected — these questions have no "
                "answer in the installed/snippet surface:",
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
    installed file MUST contain section 7 (Beyond the core invariants).
    """

    def test_claude_installed_has_section_7(self):
        if not CLAUDE_INSTALLED.exists():
            self.skipTest(f"not installed: {CLAUDE_INSTALLED}")
        text = CLAUDE_INSTALLED.read_text()
        self.assertIn(
            "Beyond the core invariants", text,
            "claude-installed: missing section 7 — installer may not have "
            "refreshed the managed block, or snippet was rolled back",
        )

    def test_pi_installed_has_section_7(self):
        if not PI_INSTALLED.exists():
            self.skipTest(f"not installed: {PI_INSTALLED}")
        text = PI_INSTALLED.read_text()
        self.assertIn(
            "Beyond the core invariants", text,
            "pi-installed: missing section 7 — installer may not have "
            "refreshed the managed block, or snippet was rolled back",
        )

    def test_installed_files_match_snippets_in_section_7(self):
        """If the installed file's managed section doesn't include section 7
        but the snippet does, the installer drifted. Detect it."""
        pairs = [
            ("claude", CLAUDE_INSTALLED, CLAUDE_SNIPPET),
            ("pi", PI_INSTALLED, PI_SNIPPET),
        ]
        for name, installed, snippet in pairs:
            if not installed.exists():
                continue
            ins_text = installed.read_text()
            snip_text = snippet.read_text()
            # Snippet must have section 7 — that's the contract.
            self.assertIn(
                "Beyond the core invariants", snip_text,
                f"{name}-snippet: missing section 7",
            )
            # Installed file must have section 7 too.
            self.assertIn(
                "Beyond the core invariants", ins_text,
                f"{name}-installed: missing section 7; installer likely "
                f"no-op'd instead of refreshing",
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
