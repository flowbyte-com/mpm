"""
test_cross_adapter_contract_parity.py — Cross-adapter drift regression.

Pins the contract for the MPM behavioral instruction surface across all
four adapters and the canonical protocol. The drift class this guards
against: any single adapter (or the protocol itself) drifts out of sync
with the others, so a host plugin ends up teaching agents a stale or
incomplete subset of the MPM surface.

Drift signatures pinned (must be ABSENT from current operational material):
  - `mpm_session` as an operational tool (retired alias for mpm_handoff /
    mpm_scratchpad; the live surface is mpm_handoff + mpm_scratchpad)
  - `mpm_handoff(action: "end" / action="end")` — the retired action
    (`mpm_handoff` only supports write / read / list / shred)
  - `mpm_session.end`, `mpm__mpm_session`, `mpm__mpm_session.end`
  - Tool counts other than 22 (the canonical registry size)

Contract invariants pinned (must be PRESENT in current operational material):
  - mpm_work mention with its real action verbs (create / update / note /
    complete / resolve_contradiction)
  - mpm_handoff mention (write / read / list / shred)
  - mpm_scratchpad mention (flush / read / discard / promote)
  - Wake-on-session-start behaviour (either manual call or hook-driven)
  - Persist-during-work principle (memories / decisions / lessons / topics /
    references)
  - Skill discovery (proactive_recall_hint + skills read/list)
  - Recovery / CLI fallback (`mpm call <tool> --payload`)
  - The session-closure != work-completion distinction (protocol §4.1:
    `session ended != work completed != work verified`)

Installer contract pinned:
  - When re-running with current markers present, the installer must NOT
    short-circuit to a no-op based on marker presence alone. It must
    compare the current managed block to the freshly-built one and refresh
    when stale. This is the root-cause fix for the drift class the user
    encountered (an OpenCode session that still tried `mpm_session` after
    the templates were updated, because the installer refused to refresh).

See MPM cross-adapter integrity audit, 2026-09-04.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path("/home/v/workspace/projects/mpm/agent_installation")
CANONICAL_PROTOCOL = AGENT_INSTALLATION / "mpm-agent-protocol.md"

# The four behavioral-instruction surfaces. The two openclaw-mpm-*
# adapters (openclaw-mpm-memory, openclaw-mpm-auto-mode-persona) inject
# into the same ~/.claude/CLAUDE.md via claude-code-mpm and don't carry
# separate behavioral files, so they are covered by the claude-code-mpm
# tests below and by their own per-adapter regression suites.
#
# Each adapter specifies:
#   - `snippet`: the file containing the behavioral contract.
#   - `tool_prefix`: the host-specific tool namespace for typed tool-call
#     signatures (e.g., `mpm__mpm_work(action: ...)`). Used by the
#     paren/backtick action-mention tests.
#   - `persist_families`: the canonical family-name strings used in that
#     snippet's prose/mentions of the memory tool set. Different adapters
#     style the prefix differently (Claude Code uses `mpm__mpm_memory`
#     where the prefix is `mpm__mpm_` and the family is `memory`; Pi /
#     OpenCode / Hermes use `mpm_memory` with a separate prefix).
ADAPTERS = {
    "claude-code-mpm": {
        "snippet": AGENT_INSTALLATION / "claude-code-mpm/templates/CLAUDE.md.snippet",
        "tool_prefix": "mpm__mpm_",
        # The Claude snippet uses fully-prefixed tool names everywhere
        # (`mpm__mpm_memory`), never the bare family (`memory`).
        "persist_families": ("mpm__mpm_memory", "mpm__mpm_decisions",
                              "mpm__mpm_lessons", "mpm__mpm_topics",
                              "mpm__mpm_references"),
        "installer": AGENT_INSTALLATION / "claude-code-mpm/scripts/install_claude_instructions.py",
    },
    "opencode-mpm": {
        "snippet": AGENT_INSTALLATION / "opencode-mpm/templates/AGENTS.md.snippet",
        "tool_prefix": "mpm_",
        "persist_families": ("mpm_memory", "mpm_decisions", "mpm_lessons",
                              "mpm_topics", "mpm_references"),
        "installer": AGENT_INSTALLATION / "opencode-mpm/scripts/install_agents_instructions.py",
    },
    "pi-mpm": {
        "snippet": AGENT_INSTALLATION / "pi-mpm/templates/AGENTS.md.snippet",
        "tool_prefix": "mpm_",
        "persist_families": ("mpm_memory", "mpm_decisions", "mpm_lessons",
                              "mpm_topics", "mpm_references"),
        "installer": AGENT_INSTALLATION / "pi-mpm/scripts/install_agents_instructions.py",
    },
    "hermes-mpm": {
        "snippet": AGENT_INSTALLATION / "hermes-mpm/templates/hermes.md.snippet",
        "tool_prefix": "mcp__mpm__",
        "persist_families": ("mpm_memory", "mpm_decisions", "mpm_lessons",
                              "mpm_topics", "mpm_references"),
        "installer": AGENT_INSTALLATION / "hermes-mpm/scripts/install_hermes_instructions.py",
    },
}


# --- helpers ---------------------------------------------------------------


def _read(path: Path) -> str:
    if not path.exists():
        raise FileNotFoundError(f"required file missing: {path}")
    return path.read_text()


def _mentions_tool_family(text: str, prefix: str, family: str) -> bool:
    """True if `text` mentions `<family>` (with optional host prefix)."""
    pat = re.compile(rf"\b(?:{re.escape(prefix)})?{re.escape(family)}\b")
    return bool(pat.search(text))


def _mentions_action(text: str, prefix: str, family: str, action: str) -> bool:
    """True if `text` mentions `<family>` ... `<action>` together.

    Adapters document mpm_work / mpm_handoff / mpm_scratchpad in two
    different surface forms:

      A) `mpm_work` actions `create`/`update`/`note`/`complete`
         (Pi, Hermes — backticks, slash-separated action list, where
         the outer backticks wrap the entire list and inner backticks
         delimit each action)
      B) `mpm_<family>(action: "flush"|"read"|..., params: { ... })`
         (Claude Code, OpenCode — paren-style call signature with a
         pipe-separated, single-quoted action list after one `action:`
         keyword)

    We accept both. The `prefix` is the host-specific tool namespace
    (mcp__mpm__ / mpm__mpm_ / mpm_); the bare family name (e.g.
    `mpm_work`) is always acceptable in prose.
    """
    # Paren form (B): find `<family>(...)` and check whether `action`
    # appears as a token (quoted or unquoted) in the captured body. This
    # handles the pipe-separated form where `action:` appears only once
    # but the body lists multiple actions.
    pat_parens_body = re.compile(
        rf"(?:{re.escape(prefix)})?{re.escape(family)}\s*\(([^)]*?)\)",
        re.DOTALL,
    )
    for m in pat_parens_body.finditer(text):
        body = m.group(1)
        # The body looks like `action: "a1"|"a2"|... , params: {...}`.
        # Match the action as a token, optionally quoted, in the pipe list.
        if re.search(
            rf'[\"\']?\b{re.escape(action)}\b[\"\']?',
            body,
        ):
            return True

    # Backtick form (A): outer backticks wrap the whole action list, inner
    # backticks delimit each action (so the inside of the outer pair
    # contains backticks — must use `.*?` with re.DOTALL, not `[^`]*`).
    pat_backtick = re.compile(
        rf"`(?:{re.escape(prefix)})?{re.escape(family)}`\s+actions?\s+`"
        rf".*?\b{re.escape(action)}\b.*?`",
        re.DOTALL,
    )
    return bool(pat_backtick.search(text))


# Outer marker for "this is the historical archive; not operational". The
# audit only cares about drift in current operational material. VALIDATION-*
# files are explicitly archival and are out of scope.
def _is_archival(path: Path) -> bool:
    return path.name.startswith("VALIDATION-")


# --- contract class per adapter --------------------------------------------


class CanonicalProtocolContract(unittest.TestCase):
    """The canonical host-independent protocol must be the source of truth
    and must carry §4.1 (session != work)."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read(CANONICAL_PROTOCOL)

    def test_protocol_exists(self):
        self.assertTrue(CANONICAL_PROTOCOL.exists())

    def test_protocol_carries_session_work_distinction(self):
        # §4.1 is the canonical phrasing of the lifecycle split.
        self.assertIn("4.1 Work tracking is distinct from session closure", self.text)
        # The triple-inequality must be present (verbatim or near-verbatim).
        self.assertRegex(
            self.text,
            r"session ended\s*!=\s*work completed\s*!=\s*work verified",
        )

    def test_protocol_names_work_actions(self):
        for action in ("create", "update", "note", "complete", "resolve_contradiction"):
            self.assertRegex(
                self.text,
                rf"\bmpm_work\b[^)]*?\b{action}\b|\b{action}\b[^)]*?\bmpm_work\b",
                f"protocol must document mpm_work action '{action}'",
            )

    def test_protocol_names_handoff_actions(self):
        # write is the only handoff action needed for the contract; the rest
        # appear in the CLI error surface and don't need to be enumerated.
        self.assertIn("mpm_handoff", self.text)
        self.assertRegex(self.text, r"\bmpm_handoff\b[^)]*?\bwrite\b")

    def test_protocol_no_drift_signatures(self):
        for forbidden in ("mpm_session", "mpm__mpm_session"):
            self.assertNotIn(forbidden, self.text,
                            f"protocol must not reference retired alias '{forbidden}'")


class AdapterContractMixin:
    """Mixin: each adapter snippet must cover the same set of contract
    keys, with the host-appropriate tool prefix (mcp__mpm__ / mpm__mpm_ /
    mpm_)."""

    snippet_path: Path
    tool_prefix: str

    @classmethod
    def setUpClass(cls):
        cls.text = _read(cls.snippet_path)

    def test_work_lifecycle_distinct_from_session(self):
        # Every adapter must teach the agent that session closure does not
        # auto-complete work — the OpenCode drift came partly from this
        # paragraph being missing in older templates.
        self.assertIn("Session closure is not work completion", self.text)

    def test_mentions_mpm_work_with_real_actions(self):
        # `mpm_work` appears with at least one of its real action verbs.
        # We accept the paren-style and the backtick-slash-style.
        for action in ("create", "complete"):
            pat = re.compile(
                rf"`?mpm_work`?\s*(?:\([^)]*?\b{action}\b[^)]*?\)|"
                rf"actions?\s+`.*?\b{action}\b.*?`)",
                re.DOTALL,
            )
            self.assertRegex(
                self.text, pat,
                f"{self.snippet_path.name}: must mention mpm_work action '{action}'",
            )

    def test_mentions_mpm_handoff_write(self):
        self.assertTrue(
            _mentions_action(self.text, self.tool_prefix, "mpm_handoff", "write"),
            f"{self.snippet_path.name}: must show mpm_handoff with action 'write'",
        )

    def test_mentions_mpm_scratchpad_actions(self):
        for action in ("flush", "read"):
            self.assertTrue(
                _mentions_action(self.text, self.tool_prefix, "mpm_scratchpad", action),
                f"{self.snippet_path.name}: must show mpm_scratchpad action '{action}'",
            )

    def test_mentions_persist_during_work(self):
        # The persist-during-work principle names the memory tool set.
        # Each adapter's snippet must mention at least 3 of its 5 families.
        # We check the prose family names directly (per-adapter, since the
        # prefix convention varies: Claude uses `mpm__mpm_memory` while Pi /
        # OpenCode / Hermes use `mpm_memory`).
        pat = re.compile(
            r"\b(?:" + "|".join(re.escape(f) for f in self.persist_families) + r")\b"
        )
        distinct = {m.group(0) for m in pat.finditer(self.text)}
        self.assertGreaterEqual(
            len(distinct), 3,
            f"{self.snippet_path.name}: persist-during-work principle must "
            f"name >=3 of {self.persist_families}; got {sorted(distinct)}",
        )

    def test_mentions_skill_discovery(self):
        # Either via `proactive_recall_hint` (the modern surface) or
        # `skills read/list` (the reactive catalog fallback).
        self.assertTrue(
            "proactive_recall_hint" in self.text or "skills" in self.text,
            f"{self.snippet_path.name}: must describe skill discovery",
        )

    def test_mentions_wake_protocol(self):
        # Either by calling `read_wake_context` explicitly or by saying
        # it's handled automatically by a hook/extension.
        self.assertTrue(
            "read_wake_context" in self.text or "wake" in self.text.lower(),
            f"{self.snippet_path.name}: must describe wake-on-session-start",
        )

    def test_mentions_recovery_cli_fallback(self):
        # `mpm call <tool> --payload ...` is the universal recovery path.
        self.assertIn("mpm call", self.text,
                      f"{self.snippet_path.name}: must mention the CLI fallback path")

    def test_mentions_skill_formation(self):
        # §3.1 of the canonical protocol — agents must know how to turn a
        # repeated procedure into a durable skill via the workshop, not just
        # how to discover one. The drift class this guards against: snippets
        # document skill discovery but not skill formation, leaving agents
        # to reinvent parallel skills instead of using `mpm_skills save` /
        # `workshop` action refine.
        # We accept either the modern tool-name form or the backtick form.
        save_pat = re.compile(
            rf"`?(?:{re.escape(self.tool_prefix)})?mpm_skills`?\s*(?:\([^)]*?\bsave\b[^)]*?\)|"
            rf"actions?\s+`.*?\bsave\b.*?`)",
            re.DOTALL,
        )
        # Workshop may appear after a backtick boundary (`mpm_skills` action
        # `workshop` ...) — accept either form.
        workshop_pat = re.compile(
            rf"(?:{re.escape(self.tool_prefix)})?mpm_skills\b.*?\bworkshop\b",
            re.DOTALL,
        )
        self.assertTrue(
            save_pat.search(self.text) is not None
            or ("save" in self.text.lower() and "skills" in self.text.lower()),
            f"{self.snippet_path.name}: must mention mpm_skills action 'save'",
        )
        self.assertRegex(
            self.text, workshop_pat,
            f"{self.snippet_path.name}: must mention mpm_skills action 'workshop'",
        )

    def test_mentions_pointer_native_results(self):
        # §7 of the canonical protocol — agents must use `mpm_resolve` /
        # `mpm_blob_read` / `mpm_blob_search` instead of inlining full
        # payloads. The drift class this guards against: snippets that
        # expose the pointer architecture but never tell agents to USE it,
        # so they inline large blobs and overflow context.
        resolve_pat = re.compile(
            rf"(?:{re.escape(self.tool_prefix)})?mpm_resolve\b",
        )
        self.assertRegex(
            self.text, resolve_pat,
            f"{self.snippet_path.name}: must mention mpm_resolve for pointer-native results",
        )
        # Pointer scheme must be referenced so the agent recognizes the URI.
        self.assertIn(
            "mpm://", self.text,
            f"{self.snippet_path.name}: must reference the mpm:// URI scheme",
        )

    def test_mentions_retrieval_diagnosis(self):
        # When `mpm_memory query` returns zero results or unexpected
        # ordering, agents must use `mpm_retrieval_diagnose` instead of
        # reformulating blindly.
        diagnose_pat = re.compile(
            rf"(?:{re.escape(self.tool_prefix)})?mpm_retrieval_diagnose\b",
        )
        self.assertRegex(
            self.text, diagnose_pat,
            f"{self.snippet_path.name}: must mention mpm_retrieval_diagnose for retrieval failures",
        )

    def test_mentions_epistemic_surfaces(self):
        # §7.1 of the canonical protocol — epistemic surfaces are NOT
        # interchangeable. Each adapter must name at least four of:
        # decisions, lessons, theories, evidence, confidence, challenge.
        # This is the contract for "durable knowledge types that an
        # agent must interpret correctly, not as generic memory items."
        # The families are the family-name suffixes; the host prefix is
        # applied via the prefix-substitution regex (so the test works
        # for `mpm_decisions` (Pi/OpenCode/Hermes) AND `mpm__mpm_decisions`
        # (Claude Code) and `mcp__mpm__mpm_decisions` (Hermes)).
        families = (
            "decisions", "lessons", "theories",
            "evidence", "confidence", "challenge",
        )
        present = []
        for f in families:
            # Match either `<prefix>mpm_<f>` or the bare `<f>` form (rare).
            # The bare family name is `mpm_<f>` — the regex anchors it.
            full = f"mpm_{f}"
            pat = re.compile(
                rf"(?:{re.escape(self.tool_prefix)})?{re.escape(full)}\b"
            )
            if pat.search(self.text):
                present.append(full)
        self.assertGreaterEqual(
            len(present), 4,
            f"{self.snippet_path.name}: must name >=4 epistemic surfaces "
            f"(decisions/lessons/theories/evidence/confidence/challenge); "
            f"found {present}",
        )

    def test_mentions_wake_vs_scheduled_wake(self):
        # §1 (passive wake context) vs `mpm_wakes` (proactive future
        # triggers) must NOT be conflated. Snippets that conflate them
        # cause agents to think reading wake context consumes a scheduled
        # wake, or that a session-end handoff is the same as scheduling
        # follow-up work.
        wakes_pat = re.compile(
            rf"(?:{re.escape(self.tool_prefix)})?mpm_wakes\b",
        )
        # The snippet must name the scheduled-wake surface AND distinguish
        # it from session-start wake.
        self.assertRegex(
            self.text, wakes_pat,
            f"{self.snippet_path.name}: must mention mpm_wakes (scheduled-wake surface)",
        )
        # The distinguishing phrase: "scheduled wake" or "vs" between
        # wake-context and mpm_wakes.
        self.assertTrue(
            "scheduled wake" in self.text.lower()
            or "wake context" in self.text.lower()
            or "wake-vs-scheduled-wake" in self.text.lower(),
            f"{self.snippet_path.name}: must distinguish session-start wake "
            f"context from scheduled wake (mpm_wakes)",
        )

    def test_mentions_reference_freshness(self):
        # §8 — references carry a freshness state the substrate classifies.
        # An agent reading a long-ago reference must query freshness, not
        # treat it as current authority.
        self.assertIn(
            "freshness", self.text.lower(),
            f"{self.snippet_path.name}: must mention reference freshness "
            f"so agents know to verify before relying on old references",
        )
        self.assertRegex(
            self.text, rf"(?:{re.escape(self.tool_prefix)})?mpm_references\b",
            f"{self.snippet_path.name}: must mention mpm_references (the ingest surface)",
        )

    def test_mentions_projection_default(self):
        # `mpm_memory query` defaults to `summary` projection. Agents that
        # always pull full content waste context. Snippets must name the
        # projection knob.
        self.assertTrue(
            "projection" in self.text.lower() or "summary" in self.text.lower(),
            f"{self.snippet_path.name}: must mention projection default "
            f"so agents don't always pull full content",
        )

    def test_no_drift_signature_mpm_session(self):
        # The retired alias must not appear as an operational tool in any
        # current snippet. The legacy alias is documented elsewhere (in the
        # registry migration notes), not in operational instructions.
        for forbidden in ("mpm_session", "mpm__mpm_session"):
            self.assertNotIn(
                forbidden, self.text,
                f"{self.snippet_path.name} still references retired alias '{forbidden}'",
            )

    def test_no_drift_signature_handoff_end(self):
        # The retired action `end` for mpm_handoff. The current contract is
        # write/read/list/shred only.
        # Allow the substring to appear inside a longer word ("end-to-end",
        # "backend"), so use a word-boundary regex targeting the action form.
        pat = re.compile(
            r"\bmpm_handoff\b[^)]*?action[:=]\s*[\"']end[\"']"
            r"|"
            r"action[:=]\s*[\"']end[\"'][^)]*?\bmpm_handoff\b"
        )
        self.assertIsNone(
            pat.search(self.text),
            f"{self.snippet_path.name} still references mpm_handoff action 'end'",
        )


def _make_adapter_test_case(name: str, info: dict) -> type:
    """Build a TestCase subclass for each adapter."""
    cls = type(
        f"Adapter_{name.replace('-', '_')}",
        (AdapterContractMixin, unittest.TestCase),
        {"snippet_path": info["snippet"], "tool_prefix": info["tool_prefix"]},
    )
    return cls


# --- installer content-aware behaviour -------------------------------------


class InstallerContentAwareRefresh(unittest.TestCase):
    """The drift class we just closed: installer no-op'd when markers were
    present, even if the snippet had been updated. Every installer must
    compare the *content* of the current managed block to the freshly-built
    one and refresh when stale.

    We test the contract by source inspection rather than by running the
    installer end-to-end (which would require host-specific paths). The
    invariants are:
      1. The "already managed" branch exists.
      2. It extracts the current managed block (regex against BEGIN/END).
      3. It compares the extracted block to a freshly-built one.
      4. If they differ, it writes the new block (refresh); otherwise no-op.
    """

    def _assert_content_aware(self, installer_path: Path):
        text = _read(installer_path)
        # Markers for the modern managed-block form. Claude Code / OpenCode /
        # Pi use `BEGIN MPM-MANAGED SECTION`, Hermes uses `BEGIN MPM-MANAGED
        # BLOCK`. Both forms must coexist (the drift class we just closed
        # was independent of marker convention).
        self.assertRegex(text, r"BEGIN\s+MPM-MANAGED\s+(SECTION|BLOCK)",
                         f"{installer_path.name}: missing managed-block marker")
        # Must extract the current block, not just check marker presence.
        # The two installers we just patched both expose `_extract_managed_block`
        # (snake_case, returning the BEGIN..END span). Some pre-existing
        # installers achieve the same effect inline via a helper like
        # `_find_mpm_section`. Accept either shape.
        has_extract_helper = bool(
            re.search(r"def\s+_extract_managed_block", text)
            or re.search(r"def\s+_find_mpm_section", text)
            or re.search(r"def\s+_find_managed", text)
        )
        # Pi and Hermes achieve content-aware refresh inline via a
        # 'replaced'/'no-op' branch over a `new_text == text` comparison.
        # Accept any of these forms:
        has_inline_comparison = bool(
            re.search(r"new_managed\s*==", text)
            or re.search(r"existing_managed_block\s*==", text)
            or re.search(r"new_text\s*==\s*text", text)
            or re.search(r"_extract_managed_block\s*\(\s*existing\s*\)", text)
        )
        # Hermes specifically: the install() function returns 'no-op' IFF
        # new_text == text, which is the content-aware check. Accept this
        # as an additional shape.
        has_hermes_pattern = bool(
            re.search(r"if\s+new_text\s*==\s*text\s*:\s*return\s*[\"']no-op[\"']", text)
        )
        self.assertTrue(
            has_extract_helper or has_inline_comparison or has_hermes_pattern,
            f"{installer_path.name}: 'already managed' branch must compare "
            f"extracted current block to a freshly-built one — found neither "
            f"a helper (e.g., _extract_managed_block, _find_mpm_section) nor "
            f"an inline content comparison (e.g., new_managed == ..., new_text "
            f"== text).",
        )

    def test_claude_code_installer_is_content_aware(self):
        self._assert_content_aware(ADAPTERS["claude-code-mpm"]["installer"])

    def test_opencode_installer_is_content_aware(self):
        self._assert_content_aware(ADAPTERS["opencode-mpm"]["installer"])

    def test_pi_installer_is_content_aware(self):
        self._assert_content_aware(ADAPTERS["pi-mpm"]["installer"])

    def test_hermes_installer_is_content_aware(self):
        self._assert_content_aware(ADAPTERS["hermes-mpm"]["installer"])


# --- adapter test case classes (built dynamically) -------------------------


class Test_ClaudeCode(AdapterContractMixin, unittest.TestCase):
    snippet_path = ADAPTERS["claude-code-mpm"]["snippet"]
    tool_prefix = ADAPTERS["claude-code-mpm"]["tool_prefix"]
    persist_families = ADAPTERS["claude-code-mpm"]["persist_families"]


class Test_OpenCode(AdapterContractMixin, unittest.TestCase):
    snippet_path = ADAPTERS["opencode-mpm"]["snippet"]
    tool_prefix = ADAPTERS["opencode-mpm"]["tool_prefix"]
    persist_families = ADAPTERS["opencode-mpm"]["persist_families"]


class Test_Pi(AdapterContractMixin, unittest.TestCase):
    snippet_path = ADAPTERS["pi-mpm"]["snippet"]
    tool_prefix = ADAPTERS["pi-mpm"]["tool_prefix"]
    persist_families = ADAPTERS["pi-mpm"]["persist_families"]


class Test_Hermes(AdapterContractMixin, unittest.TestCase):
    snippet_path = ADAPTERS["hermes-mpm"]["snippet"]
    tool_prefix = ADAPTERS["hermes-mpm"]["tool_prefix"]
    persist_families = ADAPTERS["hermes-mpm"]["persist_families"]


if __name__ == "__main__":
    unittest.main(verbosity=2)
