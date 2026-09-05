"""
test_cross_adapter_contract_parity.py — Cross-adapter drift regression.

Pins the contract for the MPM behavioural instruction surface across the
canonical source, the canonical protocol, and all four persistent-file
adapters. The drift class this guards against: any single layer drifts
out of sync with the others, so a host plugin ends up teaching agents a
stale or incomplete subset of the MPM surface.

Layer model (post-2026-09-04 canonical-snippets refactor):

  1. **Canonical protocol** (`mpm-agent-protocol.md`) — host-independent
     principles; documents §4.1 (session != work) and the rationale.
  2. **Canonical source** (`MPM_AGENT_INTEGRATION_SNIPPETS.md`) — owns
     the universal managed block AND the tool-reference stability
     contract table. The block teaches the seven invariants; the table
     pins the exact action enums and parameters that agents see.
  3. **Per-host rendered snippet** — derived (not hand-edited). Tests
     verify the seven invariants made it through transport-namespace
     substitution.

Drift signatures pinned (must be ABSENT from current operational material):
  - `mpm_session` as an operational tool (retired alias for mpm_handoff /
    mpm_scratchpad; the live surface is mpm_handoff + mpm_scratchpad)
  - `mpm_handoff(action: "end" / action="end")` — the retired action
    (`mpm_handoff` only supports write / read / list / shred)
  - `mpm_session.end`, `mpm__mpm_session`, `mpm__mpm_session.end`

Adapter-rendered-block invariants pinned (must be PRESENT in every host
snippet — the seven non-negotiable invariants survive transport
substitution):
  - mpm_work mention with its real action verbs (create / update / note /
    complete / resolve_contradiction)
  - mpm_handoff mention (write / read / list / shred)
  - mpm_scratchpad mention (flush / read / discard / promote)
  - Wake-on-session-start behaviour (read_wake_context or hook-driven)
  - Persist-during-work principle (memories / decisions / lessons / topics /
    references)
  - Skill discovery (proactive_recall_hint + skills read/list)
  - Recovery / CLI fallback (`mpm call <tool> --payload`)
  - The session-closure != work-completion distinction (verbatim)
  - The projection knob (`projection: "compact"`) is mentioned so agents
    don't always pull full content

Tool-reference-stability-contract invariants (must be PRESENT in the
canonical source's tool-reference table — these are the deeper MPM
surface that the agent-facing block intentionally does not enumerate
per brief §13's "avoid protocol duplication" rule):
  - mpm_skills action `workshop` documented (skill formation)
  - mpm_retrieval_diagnose documented (diagnose retrieval failures)
  - mpm_resolve documented (pointer-native results)
  - mpm:// URI scheme referenced (pointer architecture)
  - mpm_wakes documented (scheduled-wake surface distinct from session-start)
  - mpm_references + freshness states documented
  - mpm_handoff write params do NOT include `note` (regression for the
    2026-09-04 schema fix that removed the stale `note` field)

No-duplicate-managed-section contract (brief §11 Test 5):
  - Each rendered snippet contains exactly one
    `<!-- BEGIN MPM MANAGED BLOCK -->` ... `<!-- END MPM MANAGED BLOCK -->`
    pair.
  - After fresh install + reinstall (idempotent), the target file has
    exactly one host-specific managed section.

Installer contract pinned:
  - When re-running with current markers present, the installer must NOT
    short-circuit to a no-op based on marker presence alone. It must
    compare the current managed block to the freshly-built one and refresh
    when stale. This is the root-cause fix for the drift class the user
    encountered (an OpenCode session that still tried `mpm_session` after
    the templates were updated, because the installer refused to refresh).

OpenClaw runtime binding (brief §11 Test 6):
  - OpenClaw uses runtime injection, not a persistent managed file.
    Verified by the absence of openclaw adapters from the
    `render_managed_blocks.py` ADAPTERS list (covered in
    `test_render_managed_blocks.py::AdapterExclusion`). No persistent-
    block search is needed.

See MPM cross-adapter integrity audit, 2026-09-04.
"""

from __future__ import annotations

import importlib.util
import re
import sys
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path("/home/v/workspace/projects/mpm/agent_installation")
CANONICAL_PROTOCOL = AGENT_INSTALLATION / "mpm-agent-protocol.md"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"


# --- single source of truth for adapter metadata ---------------------------
#
# The render script's ADAPTERS list is the canonical source for adapter
# names, tool prefixes, snippet paths, and copy/paste outer markers.
# This test derives its ADAPTERS dict from that list, layering on only
# test-specific concerns (installer paths and per-host install args).
# Adding a new persistent-file adapter means editing ONE list
# (render_managed_blocks.ADAPTERS) plus ONE map below (_INSTALLERS),
# not three independent lists in two files.


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks", RENDER_SCRIPT,
    )
    assert spec and spec.loader, "render script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


_render = _load_render_module()


# Canonical memory-family tool names. Each adapter renders these as
# `<tool_prefix>mpm_<family>` (the bare family name `mpm_memory` already
# includes the `mpm_` leading prefix, so the rendered form is just
# `<tool_prefix>mpm_memory`).
_PERSIST_FAMILIES = ("memory", "decisions", "lessons", "topics", "references")

# Per-host installer script paths. The render script has no knowledge of
# installer binaries — installers are a test/install concern only. Each
# adapter has a distinct installer path; the test cannot derive this from
# the adapter name alone because the conventions differ (e.g.,
# `install_claude_instructions.py` vs `install_agents_instructions.py`).
_INSTALLERS = {
    "claude-code-mpm": AGENT_INSTALLATION / "claude-code-mpm/scripts/install_claude_instructions.py",
    "opencode-mpm":    AGENT_INSTALLATION / "opencode-mpm/scripts/install_agents_instructions.py",
    "pi-mpm":          AGENT_INSTALLATION / "pi-mpm/scripts/install_agents_instructions.py",
    "hermes-mpm":      AGENT_INSTALLATION / "hermes-mpm/scripts/install_hermes_instructions.py",
}

# Per-host extra CLI args for the installer. Each adapter's installer
# takes different scope flags and home-dir overrides; this is a
# test-only concern (the render script does not invoke installers).
_EXTRA_INSTALL_ARGS = {
    "claude-code-mpm": ["--scope", "user", "--home", str(Path.home())],
    "opencode-mpm":    ["--scope", "user"],
    "pi-mpm":          [],
    "hermes-mpm":      [],
}

# Outer markers the installer actually emits in the target file. These
# are distinct from `copy_paste_outer_*` in render_managed_blocks.ADAPTERS,
# which marks the canonical-source copy/paste section wrapper (used by
# the render script for byte-parity verification). The installer emits
# different outer anchors (e.g., hermes uses MPM-MANAGED BLOCK with a
# hyphen; others use MPM-MANAGED SECTION). The cross-adapter test counts
# the installer-emitted markers, not the canonical-source wrappers.
_INSTALL_OUTER_MARKERS = {
    "claude-code-mpm": (
        "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
        "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->",
    ),
    "opencode-mpm": (
        "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
        "<!-- END MPM-MANAGED SECTION:opencode-instructions -->",
    ),
    "pi-mpm": (
        "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
        "<!-- END MPM-MANAGED SECTION:pi-instructions -->",
    ),
    "hermes-mpm": (
        "<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->",
        "<!-- END MPM-MANAGED BLOCK:hermes-mpm -->",
    ),
}


def _build_test_adapter_info() -> dict:
    """Build the test's ADAPTERS dict from render_managed_blocks.ADAPTERS.

    Returns a name → info dict with:
      - `snippet`: absolute path to the rendered template snippet
      - `tool_prefix`: the host's transport-namespace prefix
      - `persist_families`: tuple of fully-qualified persistence tool names
      - `installer`: absolute path to the adapter's installer script
      - `begin`/`end`: outer wrapper markers used to count managed sections
      - `extra_args`: CLI args to pass to the installer when invoked by tests
    """
    out: dict = {}
    for a in _render.ADAPTERS:
        name = a["name"]
        if name not in _INSTALLERS:
            raise KeyError(
                f"adapter {name!r} is in render_managed_blocks.ADAPTERS but "
                f"missing from this test's _INSTALLERS map — add the "
                f"installer path there to centralize adapter metadata."
            )
        snippet = _render.snippet_path(AGENT_INSTALLATION, a)
        prefix = a["tool_prefix"]
        families = tuple(f"{prefix}mpm_{fam}" for fam in _PERSIST_FAMILIES)
        install_begin, install_end = _INSTALL_OUTER_MARKERS[name]
        out[name] = {
            "snippet": snippet,
            "tool_prefix": prefix,
            "persist_families": families,
            "installer": _INSTALLERS[name],
            "begin": install_begin,
            "end": install_end,
            "extra_args": _EXTRA_INSTALL_ARGS.get(name, []),
        }
    return out


# The test's view of adapter metadata. Single source of truth:
# render_managed_blocks.ADAPTERS for snippet paths / tool prefixes /
# copy/paste markers; this test's _INSTALLERS + _EXTRA_INSTALL_ARGS
# for installer-specific concerns.
ADAPTERS = _build_test_adapter_info()


# --- helpers ---------------------------------------------------------------


def _read(path: Path) -> str:
    if not path.exists():
        raise FileNotFoundError(f"required file missing: {path}")
    return path.read_text()


def _mentions_tool_family(text: str, prefix: str, family: str) -> bool:
    """True if `text` mentions `<family>` (with optional host prefix).

    Accepts both the bare form (`mpm_memory`) and the prefixed form
    (`mpm__mpm_memory`, `mcp__mpm__mpm_memory`). The bare form requires
    word boundaries; the prefixed form does NOT require a leading word
    boundary because the host prefix typically ends in `_` (a word
    character) — concatenating `mcp__mpm__` + `mpm_memory` has no
    word boundary between the two segments.
    """
    bare = rf"\b{re.escape(family)}\b"
    prefixed = re.escape(prefix + family) if prefix else None
    if prefixed and re.search(prefixed, text):
        return True
    return bool(re.search(bare, text))


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
    if pat_backtick.search(text):
        return True

    # Lifecycle form (C): used by the canonical source post-2026-09-04.
    # Sentence pattern: `mpm_<family>`. ... Lifecycle: action `<a1>`,
    # `<a2>`/`<a3>` during, `<a4>` to finish. The family name is in
    # backticks (possibly host-prefixed); the action verb appears in
    # backticks anywhere within ~600 chars of the family name.
    family_full = re.escape(prefix + family) if prefix else re.escape(family)
    pat_lifecycle = re.compile(
        rf"`{family_full}`\.{{0,40}}Lifecycle:\s+action\s+`{re.escape(action)}`",
        re.DOTALL,
    )
    if pat_lifecycle.search(text):
        return True

    # Bare-named Lifecycle form: same as above but the family mention is
    # unprefixed (e.g., when reading the canonical source directly).
    if prefix:
        bare_family = re.escape(family)
        pat_lifecycle_bare = re.compile(
            rf"`{bare_family}`\.{{0,40}}Lifecycle:\s+action\s+`{re.escape(action)}`",
            re.DOTALL,
        )
        if pat_lifecycle_bare.search(text):
            return True

    return False


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
        # We accept three documentation styles:
        #   A) `mpm_work` actions `create`/`update`/`note`/`complete` (backtick)
        #   B) `mpm_work(action: "create"|"update"|..., params: {...})` (paren)
        #   C) `mpm_work`. Lifecycle: action `create` to open, `update`/`note`
        #      during, `complete` to finish. (sentence form used by the
        #      canonical source post-2026-09-04)
        for action in ("create", "complete"):
            prefix_pat = re.escape(self.tool_prefix) if self.tool_prefix else ""
            pat = re.compile(
                rf"`?{prefix_pat}mpm_work`?\s*(?:"
                rf"\([^)]*?\b{action}\b[^)]*?\)"
                rf"|actions?\s+`.*?\b{action}\b.*?`"
                rf"|.{{0,500}}Lifecycle:.{{0,200}}`{action}`"
                rf")",
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


# --- canonical-source tool-reference stability contract ---------------------
#
# Brief §13: the agent-facing managed block does NOT enumerate every MPM
# tool (that would duplicate the protocol preamble and inflate the
# instruction surface). The deeper tool reference lives in the canonical
# source's "Tool-reference stability contract" table — this class pins
# that table's contents so the deeper surface does not drift silently.


class ToolReferenceStabilityContract(unittest.TestCase):
    """The canonical source's tool-reference stability contract table is
    the deeper MPM surface that the agent-facing block intentionally does
    not enumerate. Each entry below documents a specific tool/action the
    substrate exposes; an agent that needs the deeper surface reads the
    table (or, for principles, the protocol preamble)."""

    @classmethod
    def setUpClass(cls):
        cls.text = _read(CANONICAL_SOURCE)
        # The tool-reference stability contract is the markdown table
        # between the headings `# Tool-reference stability contract` and
        # the next `## ` heading (or end of file).
        m = re.search(
            r"#\s+Tool-reference stability contract\s*\n(.*?)(?=^#\s|\Z)",
            cls.text,
            re.DOTALL | re.MULTILINE,
        )
        assert m, "canonical source missing 'Tool-reference stability contract' section"
        cls.table = m.group(1)

    def test_section_present(self):
        self.assertIn("# Tool-reference stability contract", self.text,
                      "canonical source must carry the tool-reference stability contract section")

    def test_mpm_skills_workshop_documented(self):
        # Brief §3.1 (skill formation): the workshop action is the canonical
        # way to draft a skill from non-obvious experience. The deeper
        # surface lives here, not in the agent-facing block.
        self.assertIn("mpm_skills", self.table)
        self.assertIn("workshop", self.table,
                      "tool-reference table must document mpm_skills action 'workshop'")

    def test_mpm_resolve_documented(self):
        # Brief §7 (pointer-native results): agents must use mpm_resolve
        # to dereference mpm:// URIs instead of inlining payloads.
        self.assertIn("mpm_resolve", self.table)
        self.assertIn("mpm://", self.table,
                      "tool-reference table must reference the mpm:// URI scheme")

    def test_mpm_retrieval_diagnose_documented(self):
        # When mpm_memory query returns zero results, agents use
        # mpm_retrieval_diagnose to inspect BM25 / ranking.
        self.assertIn("mpm_retrieval_diagnose", self.table,
                      "tool-reference table must document mpm_retrieval_diagnose")

    def test_mpm_wakes_documented(self):
        # §1 wake context is passive; mpm_wakes is the proactive future-
        # trigger surface. Both must be documented in the deeper table
        # (the agent-facing block intentionally only mentions wake-on-
        # session-start, not scheduled wakes).
        self.assertIn("mpm_wakes", self.table,
                      "tool-reference table must document mpm_wakes")

    def test_mpm_references_and_freshness_documented(self):
        # §8 references carry a freshness state the substrate classifies.
        self.assertIn("mpm_references", self.table)
        self.assertIn("freshness", self.table.lower(),
                      "tool-reference table must document reference freshness states")

    def test_mpm_handoff_no_note_param(self):
        # Regression for the 2026-09-04 audit finding: the canonical
        # source must NOT advertise a `note` parameter on mpm_handoff
        # write (the schema removed it). This pins the doc and the
        # substrate in lock-step.
        # Find the mpm_handoff row in the table.
        m = re.search(r"\|\s*`?mpm_handoff`?\s*\|([^|\n]*)\|", self.table)
        self.assertIsNotNone(m, "mpm_handoff row not found in tool-reference table")
        row = m.group(1)
        self.assertIn("write", row)
        self.assertNotIn("note", row,
                         f"mpm_handoff row still advertises stale 'note' param: {row!r}")

    def test_mpm_memory_action_challenge_documented(self):
        # The challenge surface is reachable via mpm_memory action=challenge
        # (the standalone mpm_challenge tool was retired 2026-09-05; see
        # docs/onboarding-mcp-native-audit-2026-09-05.md Part C). The
        # tool-reference table must document the action surface where
        # agents will look for it.
        self.assertIn("action=`challenge`", self.table,
                      "tool-reference table must document mpm_memory action=`challenge` (the canonical challenge surface since 2026-09-05)")

    def test_mpm_memory_projection_default_pinned(self):
        # `mpm_memory query` defaults to `projection: "summary"`; full
        # content requires `projection: "full"`. This is the only knob
        # agents need to keep context bounded.
        # Capture the entire row (all three columns), not just the first.
        m = re.search(
            r"\|\s*`?mpm_memory`?\s*\|[^\n]*\|",
            self.table,
        )
        self.assertIsNotNone(m, "mpm_memory row not found in tool-reference table")
        row = m.group(0)
        self.assertIn("projection", row.lower(),
                      "mpm_memory row must document the projection knob")


# --- brief §11 Test 5: no-duplicate managed sections ------------------------


class NoDuplicateManagedSections(unittest.TestCase):
    """Brief §11 Test 5: the target host file MUST NOT contain two MPM
    managed sections. The render path produces one canonical block per
    host; the installer must NOT accumulate a second block on re-run.
    """

    # All adapter-specific fields come from the centralized ADAPTERS dict
    # (installer path, snippet path, begin/end markers, extra CLI args).
    # See `_build_test_adapter_info()` for the derivation chain.

    def _run(self, args: list[str]) -> subprocess.CompletedProcess:
        import subprocess
        import sys as _sys
        return subprocess.run(
            [_sys.executable, *args],
            capture_output=True, text=True, timeout=30,
        )

    def test_rendered_snippet_has_exactly_one_managed_block(self):
        """The checked-in rendered snippet must contain exactly one
        BEGIN/END MPM MANAGED BLOCK pair. Multiple pairs would mean
        the canonical source's last-occurrence extraction broke."""
        import re as _re
        for name, info in ADAPTERS.items():
            with self.subTest(adapter=name):
                text = info["snippet"].read_text(encoding="utf-8")
                begins = _re.findall(r"<!-- BEGIN MPM MANAGED BLOCK -->", text)
                ends = _re.findall(r"<!-- END MPM MANAGED BLOCK -->", text)
                self.assertEqual(
                    len(begins), 1,
                    f"{name}: rendered snippet has {len(begins)} BEGIN markers "
                    f"(expected exactly 1)",
                )
                self.assertEqual(
                    len(ends), 1,
                    f"{name}: rendered snippet has {len(ends)} END markers "
                    f"(expected exactly 1)",
                )

    def test_reinstall_does_not_duplicate_managed_section(self):
        """Running the installer twice must NOT accumulate a second
        managed section. The drift class this guards against: an
        installer that appends a fresh block on re-run instead of
        replacing the existing one — produces a file with two MPM
        blocks, conflicting guidance, and inflated context cost."""
        import tempfile
        for name, info in ADAPTERS.items():
            with self.subTest(adapter=name):
                with tempfile.TemporaryDirectory() as tmp:
                    target = Path(tmp) / info["snippet"].name
                    args = [str(info["installer"]),
                            *info["extra_args"],
                            "--target", str(target),
                            "--snippet", str(info["snippet"])]
                    p1 = self._run(args)
                    self.assertEqual(p1.returncode, 0, p1.stderr)
                    p2 = self._run(args)
                    self.assertEqual(p2.returncode, 0, p2.stderr)

                    text = target.read_text(encoding="utf-8")
                    n_begin = text.count(info["begin"])
                    n_end = text.count(info["end"])
                    self.assertEqual(
                        n_begin, 1,
                        f"{name}: after 2 installs, target has {n_begin} "
                        f"BEGIN markers (expected exactly 1)",
                    )
                    self.assertEqual(
                        n_end, 1,
                        f"{name}: after 2 installs, target has {n_end} "
                        f"END markers (expected exactly 1)",
                    )


# --- brief §11 Test 6: OpenClaw runtime binding -----------------------------
#
# OpenClaw uses runtime injection rather than a persistent MPM-managed
# file. The contract is verified through the absence of openclaw adapters
# in the render script's ADAPTERS list (covered in
# `test_render_managed_blocks.py::AdapterExclusion`). No persistent-block
# search is needed. This comment is a placeholder so future maintainers
# know why no class exists here for OpenClaw.


if __name__ == "__main__":
    unittest.main(verbosity=2)
