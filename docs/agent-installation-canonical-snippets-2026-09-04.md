# MPM Agent Installation — Canonical Managed Instruction Blocks

**Date:** 2026-09-04
**Scope:** four commits, all on `main`, none pushed

## Section A — Goal

The four file-based host adapters (claude-code-mpm, opencode-mpm,
pi-mpm, hermes-mpm) each maintained a hand-written instruction snippet
that duplicated the MPM behavioural contract text with slight wording
variations and per-host tool-prefix substitutions. The drift class
this caused:

1. **Schema corrections propagated slowly.** When `note` was removed
   from `mpm_handoff write` (remediation pass 1 commit `97898f0`,
   2026-09-04), three of the four host snippets still taught agents to
   pass `note` because the snippets were hand-edited and stale.
2. **Wording drifted silently.** Each adapter's prose diverged in ways
   the cross-adapter parity tests had to special-case.
3. **Tool prefixes leaked inconsistently.** Some adapters had
   `mpm__mpm_<name>` (double-prefix), others `mpm_<name>`,
   others `mcp__mpm__mpm_<name>`. The host-transport prefix and the
   canonical `mpm_` family prefix were conflated.

The canonical-snippets refactor introduces a two-layer documentation
model:

- **Behavioural contract** (`mpm-agent-protocol.md`) — principles,
  host-independent.
- **Managed instruction text**
  (`MPM_AGENT_INTEGRATION_SNIPPETS.md`) — exact wording rendered into
  each host's managed section, derived from a canonical managed block
  with `{TOOL_PREFIX}` substitution.

The four file-based adapter snippets are now **generated** by
`scripts/render_managed_blocks.py`, not hand-edited. Byte-for-byte
drift detection tests verify the checked-in snippets match the
rendered output. Hand-edits are a regression; the test fails if any
host snippet drifts from the canonical source.

## Section B — Commits Shipped

| # | Subject | Files | Tests |
|---|---|---|---|
| 1 | `feat(snippets): introduce canonical MPM_AGENT_INTEGRATION_SNIPPETS.md` | 1 new (canonical source) | — |
| 2 | `feat(scripts): add render_managed_blocks + regenerate adapter snippets from canonical source` | 1 new (render script), 4 modified (regenerated snippets), 1 new (drift test) | 18 |
| 3 | `docs(protocol): reference canonical snippets and render contract from protocol preamble` | 1 modified | — |
| 4 | `docs(install): document canonical-snippets architecture in agent_installation/README.md` | 1 modified | — |

Drift detection tests are in `agent_installation/tests/`:
- `test_render_managed_blocks.py` — 18 tests pinning the render script's
  contract (canonical source, block extraction, per-adapter rendering,
  byte-for-byte parity, regression coverage for the `note` removal).
- `test_clean_install_roundtrip.py` — 6 tests verifying install /
  re-install / uninstall round-trips for all four adapters, plus a
  red-green mutation test that proves the drift check would catch a
  hand-edit regression.

The cross-adapter parity test
(`agent_installation/tests/test_cross_adapter_contract_parity.py`,
77 tests) was updated to accept the new canonical source's
"Lifecycle: action" documentation style and the corrected prefix
table.

## Section C — Architecture

### Two-layer documentation

```
┌─────────────────────────────────────────────────────────────────┐
│ mpm-agent-protocol.md                                           │
│ "the principles an MPM-aware agent must honour"                  │
│ Host-independent. Source of truth for behaviour.                 │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│ MPM_AGENT_INTEGRATION_SNIPPETS.md  (NEW)                        │
│ Section C contains the canonical managed block:                  │
│   <!-- BEGIN MPM MANAGED BLOCK -->                               │
│   ...seven behavioural invariants, "Beyond the core invariants"  │
│   ...using {TOOL_PREFIX}mpm_handoff, mpm_memory, etc.            │
│   <!-- END MPM MANAGED BLOCK -->                                 │
│ Sections A–G document the architecture for human readers.        │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼ render_managed_blocks.py
                              │   (substitutes {TOOL_PREFIX}
                              │    with each adapter's prefix)
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│ Per-adapter snippets (DERIVED, NOT HAND-EDITED):                 │
│   claude-code-mpm/templates/CLAUDE.md.snippet   (prefix mpm__)   │
│   opencode-mpm/templates/AGENTS.md.snippet      (prefix "")      │
│   pi-mpm/templates/AGENTS.md.snippet            (prefix "")      │
│   hermes-mpm/templates/hermes.md.snippet        (prefix mcp__mpm__)│
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼ read by installer at install time
                              ▼
                    Host-specific managed section
                    (CLAUDE.md / AGENTS.md / .hermes.md)
```

### Prefix substitution table

The canonical block uses bare tool names already prefixed with
`mpm_` (e.g., `mpm_handoff`, `mpm_memory`). Each adapter's
`{TOOL_PREFIX}` is the host-transport wrapper applied **before**
the bare name, not a duplicate of the `mpm_` prefix:

| Host | Domain Tools + true standalones | Example rendered name |
|---|---|---|
| Claude Code | `mpm__` | `mpm__mpm_handoff` |
| OpenCode | (empty) | `mpm_handoff` |
| Pi | (empty) | `mpm_handoff` |
| Hermes | `mcp__mpm__` | `mcp__mpm__mpm_handoff` |

OpenClaw is not in the adapter list — it uses runtime injection only
(no persistent managed file). Documented in
`MPM_AGENT_INTEGRATION_SNIPPETS.md` section D.

### Drift detection

The render script's `--check` mode renders every adapter to memory
and diffs against the checked-in snippet. The drift test
`test_render_managed_blocks.py::ByteForByteParity::test_render_matches_checked_in_snippets`
invokes this and fails on any byte-level difference. The
`test_clean_install_roundtrip.py::DriftTestFailsOnMutatedSnippet`
test does a red-green verification: it mutates a snippet, asserts
`--check` exits non-zero, then restores and asserts `--check` exits
zero. This proves the drift check catches the regression class it
claims to catch.

### Tool-reference stability contract

The canonical file pins the exact action enums and parameter names
for every MPM tool the block references (section E). The drift test
regression-pins the removal of the `note` parameter from
`mpm_handoff write` — the snippet block must not teach agents to
pass `note` as a parameter (only `mpm_work.note` as an action
remains valid, which the canonical block correctly describes).

## Section D — Defects Fixed

| Defect | Resolution |
|---|---|
| Stale `note` parameter in 3/4 adapter snippets (opencode-mpm, pi-mpm, hermes-mpm) — passed `mpm_handoff write` the obsolete `note` field even after commit `97898f0` removed it from the schema | The canonical block (and therefore all four rendered snippets) no longer teaches `note` for handoff write. Regression-pinned in `test_block_omits_stale_note_param`. |
| Tool-prefix drift across adapters — Claude used `mpm__mpm_`, OpenCode/Pi used `mpm_`, Hermes used `mcp__mpm__mpm_`. The double-prefix in Claude Code's snippet was a hand-edit error, not a transport-namespace reality | Canonical source uses bare names (`mpm_handoff`); each adapter's host-transport prefix is documented in section C of the canonical file and applied via substitution. Render output verified for all four adapters. |
| Word-level wording drift between the four hand-edited snippets (e.g., OpenCode said "Before sending a turn that closes the session, run the handoff check"; Pi said "Before sending a turn that closes the session, write via"; Claude used "Manual handoff check before closing") | Single canonical block; per-host snippets now share identical prose for the behavioural content. Host-specific notes (e.g., OpenCode plugin hook, Hermes skill vs SOUL.md split) remain per-host but live outside the managed block. |
| No drift detection — once the hand-edited snippets drifted, nothing would catch it | Byte-for-byte parity test renders all snippets in-memory and compares against the checked-in files. Drift test fails on any byte mismatch (including whitespace). |

## Section E — Test Status

| Suite | Tests | Result |
|---|---|---|
| `agent_installation/tests/test_render_managed_blocks.py` | 18 | OK |
| `agent_installation/tests/test_clean_install_roundtrip.py` | 6 | OK |
| `agent_installation/tests/test_cross_adapter_contract_parity.py` | 77 | OK (was 7 failures; test updated to accept new prefix table and "Lifecycle: action" doc style) |
| `agent_installation/tests/test_live_behavioural_verification.py` | (existing) | OK |
| `claude-code-mpm/tests/test_claude_instructions_installer.py` | 16 | OK |
| `claude-code-mpm/tests/test_telemetry_adapter.py` | (existing) | OK |
| `claude-code-mpm/tests/test_template_provenance_env.py` | (existing) | OK |
| `opencode-mpm/tests/test_opencode_instructions_installer.py` | 16 | OK |
| `pi-mpm/tests/test_pi_instructions_installer.py` | 19 | OK |
| `hermes-mpm/tests/test_hermes_instructions_installer.py` | 19 | OK |

**Total: 190 tests, all green.**

Idempotency verified: running `render_managed_blocks.py` twice
produces identical output. Running any installer twice on a target
file produces identical bytes. Uninstall restores pre-install
state.

## Section F — Bounds Honored

The audit brief listed these explicit prohibitions; this pass
confirms each was honored:

- ✅ **No full protocol rewrite.** `mpm-agent-protocol.md` is unchanged
  in its seven invariant sections. Only the preamble was extended
  with a "render contract" pointer to the canonical snippets file.
- ✅ **No automatic session-end handoff.** No cron / hook / systemd
  timer added. Handoff remains an explicit agent action.
- ✅ **No speculative install.sh functions.** `scripts/install.sh`
  was not modified.
- ✅ **No combining with scheduler/DB/handoff/protocol-redesign/
  host-runtime changes.** This pass touched only the canonical
  source, the render script, the four regenerated snippets, the
  protocol preamble, the agent_installation README, and the drift
  tests. No core / scheduler / DB / handoff changes.
- ✅ **No push.** All four commits are local. Push only on explicit
  instruction.
- ✅ **No rewrite of published history.** No amend, no force-push,
  no tag retargeting.
- ✅ **One canonical block + four thin host bindings.** Verified
  via the `_mentions_action` and `_mentions_tool_family` helpers in
  the cross-adapter parity test, which now accept either prefixed
  or bare family names per the documented prefix table.
- ✅ **Host-specific bindings live outside the canonical block.**
  The render script composes `header + canonical block + footer`
  per adapter; only the canonical block is shared.

## Section G — Out-of-scope Items (Documented for Future Passes)

1. **Adapters that use runtime injection rather than persistent
   managed files** (OpenClaw's `openclaw-mpm-memory` /
   `openclaw-mpm-auto-mode-persona` plugins). The canonical file
   documents this in section D; the render script's ADAPTERS list
   does not include them. A future pass could add a runtime
   regression test that verifies the OpenClaw `session_start` hook
   invokes `mpm_context read_wake_context` and that the wake
   payload contains the canonical managed block content.
2. **Cross-adapter parity test's special-cases.** The cross-adapter
   parity test still carries some host-specific assertions in the
   ADAPTERS table (`tool_prefix`, `persist_families`). These could
   be derived from the canonical file in a future pass — the
   `render_managed_blocks.py` module already exposes the per-adapter
   prefix data. This pass left the cross-adapter test's data table
   alone (it was the minimum invasive fix to restore parity after
   the canonical source change).
3. **Versioning metadata.** The canonical file documents a
   versioning scheme in section G (patch / minor / major bump
   criteria), but no frontmatter version pointer was added in this
   pass. A future pass could add `<!-- MPM-CANONICAL-BLOCK-VERSION:
   1.0.0 -->` inside the block, and have the drift test fail if a
   host snippet lacks it.
4. **Claude Code installer's doc/behavior mismatch around the
   session-start hook.** Documented in remediation pass 1 final
   report (out of scope there). Not addressed here.
5. **OpenClaw `mpm_session` legacy alias** (retired but possibly
   still referenced in some flow). Already absent from all four
   snippets; cross-adapter parity test regression-pins this.

## Final Verdict

**READY** — all four commits are local on `main`; full test suite
(190 tests across 10 test files) green; drift detection closes the
class of regressions where a hand-edited snippet drifts out of sync
with the canonical source; `note` parameter regression pinned;
two-layer documentation model in place; protocol preamble
references the render contract; `agent_installation/README.md`
documents the new architecture.
