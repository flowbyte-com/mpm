<!-- mpm_agent_integration_version: 1.2.0 -->

# MPM Agent Integration — Canonical Managed Instruction Snippets

> **Status:** canonical source. This file is the **single source of truth**
> for the **exact agent-facing instruction text** that host adapters place
> into their managed MPM section. Behavioural principles live in
> [`mpm-agent-protocol.md`](./mpm-agent-protocol.md); the wording and
> tool-invocation details that agents actually read live here.
>
> **Default assumption:** one universal managed block is shared across every
> supported host. Host-specific textual variants exist only where the
> implementation evidence proves them necessary. Tool-namespace prefixes
> are a transport fact (rendered into the installed block at install time)
> — not a textual variant of the block content. Host-specific file paths,
> hooks, recovery notes, and transport mechanics belong in the host
> adapter (header / footer), not in the universal block.
>
> **Default MCP surface (Sept 2026 launch):** the model initially sees
> three MCP tools (`mpm_memory`, `mpm_context`, `mpm_help`). The full
> 22-tool substrate (21 Registry entries + the `mpm_help` discovery
> closure) is reachable via `mpm_help list` discovery + the
> `mpm call <tool> --payload '…'` CLI fallback, and is fully restored
> on hosts that set `MPM_EXPOSE_ALL_TOOLS=1` in their MCP env block.
> `mpm_handoff` is reachable on the compact surface through
> `mpm_context action=write_handoff` / `read_handoff`. The canonical
> managed block below names the substrate path
> (`mpm_handoff` action `write`) for the `mpm call` CLI escape hatch
> and for full-surface hosts; the compact-surface MCP path is named
> inline in section 4 below.

---

# Copy/paste examples

Choose your host below and copy the complete block into the indicated file.

> The blocks below are produced by `scripts/render_managed_blocks.py`
> from the canonical managed block in this file. Drift detection tests
> (`tests/test_render_managed_blocks.py`) assert that each block below
> is byte-for-byte identical to the rendered canonical block, that each
> adapter template snippet matches the rendered version, and that the
> installer-written block matches the rendered version.

## Claude Code

**Target file:** `~/.claude/CLAUDE.md`
**Scope:** user-scope (applies to every Claude Code session for this
user on this machine)
**Marker convention:** the block must sit between
`<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->` and
`<!-- END MPM-MANAGED SECTION:claude-code-instructions -->`.
**Manual verification (no installer):** open a new Claude Code
session and ask "Do you have MPM behavioural instructions loaded?"
A correctly wired session will have a `SessionStart` hook installed
that delivers wake context automatically — the agent sees
`additionalContext` populated before the first turn. The compact
MCP surface is exactly `mpm__mpm_memory`, `mpm__mpm_context`,
`mpm__mpm_help`; handoff goes through `mpm__mpm_context` action
`write_handoff` / `read_handoff`.

```markdown
<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- The full behavioural protocol is canonical at ~/.mpm/agent_installation/mpm-agent-protocol.md -->

## MPM behavioural contract

You are operating with the MPM (Memory Persistence Module) substrate
on this machine. The following are the non-negotiable MPM behavioural
invariants that turn that capability into reliable behaviour. Edit the
canonical protocol, not this block, for behavioural changes.

1. **Wake is auto-injected on session start** for ClaudeCode, OpenClaw,
   OpenCode, and Pi. Each of these hosts installs a session-start
   hook that fetches MPM wake context and injects it into the
   system prompt before the first model turn. Hermes has no such
   hook — on Hermes, the agent must call `mpm__mpm_context` action
   `read_wake_context` once at the start of its first turn to
   obtain the wake payload. Arriving amnesic on a host that should
   be auto-injecting wake means the host integration is broken —
   diagnose the host adapter, not call `read_wake_context`. As a
   manual refresh path (mid-session, explicit refresh, or when the
   host integration is unavailable), `mpm__mpm_context` action
   `read_wake_context` is available. The wake payload carries
   orientation signals (mode, persona, recent topics, recent
   memories, recent milestones, last handoff, open work, overdue
   scheduled wakes, a bounded `<available_skills>` catalogue) AND
   the additive `<contextual_focus>` projection — bounded inherited
   working awareness from the routing pipeline (see §1.1).
   Decisions live in `mpm_decisions`; lessons in `mpm_lessons`. Use `params.projection:
   "compact"` for a small id+summary envelope — the full payload
   is the default.

1.1. **Interpret `<contextual_focus>` as inherited working
   awareness**, not as unquestionable truth. Each item carries
   `id / kind / artifact_id / pointer` (identity), `band / rationale /
   why_now` (why it surfaced), `status / detail / truncated` (bounded
   what-to-know), and supplementary `lifecycle_state` /
   `selection_triggers` / `compressed_related_ids` where useful. Treat
   it as bounded inherited context: use it as a durable pointer to
   investigate, follow `pointer` / `artifact_id` with the appropriate
   domain tool (`mpm_decisions show`, `mpm_lessons read`,
   `mpm_memory show`, `mpm_work show`, `mpm_resolve`, etc.) when
   detail is insufficient. Per-item status (`materialized` /
   `pointer_only` / `missing` / `unsupported` / `error`) describes
   whether bounded authoritative detail was available — never whether
   the item mattered; a `missing` item still carries the pointer and
   `why_now`. Selection order is preserved exactly; do not re-rank or
   re-order. If `<contextual_focus>.status == "degraded"` (the
   projection pipeline itself failed), continue using the legacy wake
   context and investigate MPM health only if the missing focus
   materially blocks work — do not chain the diagnostic
   `contextual_*` actions to compensate. An empty items array is not
   an error; continue from legacy wake context and current task.

1.2. **Recent history vs. contextual focus vs. diagnostic surfaces.**
   The wake payload's `<contextual_focus>` is the normal inherited
   working awareness to act on; it is a bounded pointer summary, NOT
   a ranking. Two adjacent `mpm__mpm_context` actions serve different
   questions:
   - `mpm__mpm_context` action `recent_activity` is the canonical way to
     ask "what recently happened?". It returns a chronological,
     observational feed of recent mutating actions across the substrate
     — newest-first, bounded by `limit`. The result is NOT relevance-ranked.
   - `mpm__mpm_context` actions `contextual_candidates`,
     `contextual_selection`, and `contextual_materialization` are
     routing / diagnostic surfaces for inspecting or debugging the
     contextual pipeline. They are NOT part of the normal session-start
     workflow and NOT a ranking mechanism. Do not chain the three to
     reconstruct the integrated result — the wake payload's
     `<contextual_focus>` already contains it.

2. **Persist during work, not only at the end.** Use `mpm__mpm_memory`
   action `save`, `mpm__mpm_decisions` action `record`,
   `mpm__mpm_lessons` action `save`, `mpm__mpm_topics`
   action `create`, and `mpm__mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Look beyond the compact tool surface.** When you need a
   capability, discover it before assuming it is unavailable. The
   compact native MCP surface intentionally exposes only `mpm_help`,
   `mpm_memory`, and `mpm_context`; the full registered substrate
   is broader (its count is dynamic — do not pin it). Do not assume
   a capability is missing merely because it does not appear in the
   initial `tools/list`. Use `mpm__mpm_help` action `list` to discover
   every registered tool (each entry carries `reach_via_cli`).
   Tools hidden from the default surface are reachable via the CLI
   escape hatch `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
   (works on every host). Hosts running with `MPM_EXPOSE_ALL_TOOLS=1`
   on their MCP env block restore the full registered surface
   natively. Do NOT hardcode a tool count in behaviour, tests, or
   prose.

4. **Discover, create, and refine MPM skills.** Skills are reusable
   procedures; treat them as a discover-then-creation lifecycle.
   Before doing repeatable or non-trivial work, look for an
   applicable existing skill via `mpm__mpm_context` action
   `proactive_recall_hint` (with `params: {conversation_text:
   "<recent task summary>"}`), read any surfaced skill via
   `mpm__mpm_skills` action `read`, and fall back to `mpm__mpm_skills`
   action `list` with `params: {scope: "all"}` (the bounded
   `<available_skills>` catalogue in the wake envelope is the
   lightweight first step — discovery adds a context-driven filter,
   it does not replace it). Don't auto-scan the entire skill store
   every turn. After a successful workflow that is non-trivial,
   performed successfully, likely to recur, useful to preserve
   procedurally, or improved by retaining its ordering / checks /
   constraints / failure recovery, capture it as a reusable
   procedure via `mpm__mpm_skills` action `save`, or `mpm__mpm_skills`
   action `workshop` if the workflow warrants the workshop path
   (`workshop` requires `intent`, `mode`, `task_context`,
   `workflow_description`, `failure_recovery`, `recent_actions`,
   `evidence` per the tool-reference stability contract). Avoid
   skill spam. Do NOT create a skill for trivial one-off actions,
   generic common knowledge, unproven procedures, or workflows not
   likely to recur. Intended lifecycle: discover → execute → learn
   → capture → reuse.

5. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff. On hosts using the default
   compact MCP surface (Claude Code, Hermes, OpenClaw), the MCP path
   is `mpm__mpm_context` action `write_handoff` with
   `params: {summary: "<required>", session_id: "<optional>",
   state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   The substrate path `mpm__mpm_handoff` action `write` with the same
   `params` shape remains valid via `mpm call mpm_handoff --payload
   '{"action":"write","params":{...}}'` and on hosts running with
   `MPM_EXPOSE_ALL_TOOLS=1` on their MCP env block (which restores
   the full registered MCP surface). `summary` is the only required field.
   Mid-session acknowledgements (`ok`, `thanks`, `ty`, `ack`) are NOT
   session-closing; don't write a handoff on every chat ack.

   For intra-session volatile working state, the substrate tool is
   `mpm__mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`); reachable via `mpm call mpm_scratchpad` on hosts
   using the compact MCP surface. Wake is how future-me starts;
   handoff is how future-me receives the previous session.

6. **Track durable objectives as work items.** When a task is
   meaningfully multi-step, likely to span turns or sessions,
   dependent on later information, waiting on follow-up, requiring
   verification, containing important intermediate progress, or
   costly to reconstruct after interruption, open a `mpm_work`
   item reasonably early. Use `mpm__mpm_work` action `create` to open,
   `mpm__mpm_work` action `update` for material state / plan / status
   changes, `mpm__mpm_work` action `note` to retain meaningful progress
   / evidence / context, and `mpm__mpm_work` action `complete` with
   `params: {work_id}` to finish. Use `mpm__mpm_work` action `reopen`
   when completion is invalidated or new evidence demands more
   work, and `mpm__mpm_work` action `cancel` when an objective is
   intentionally abandoned. Do not create a work item for every
   small user request. Conceptual distinction: scratchpad is
   volatile intra-session working state (substrate tool
   `mpm_scratchpad`); work is a durable objective + progress
   lifecycle; handoff is session transition (see #5). Tools not
   exposed by your host's transport remain reachable via the CLI
   fallback (see #10).

7. **Session closure is not work completion.** Three events are
   distinct: `session ended` (the host process exits), `work completed`
   (`mpm__mpm_work` action `complete` invoked), and `work verified`
   (objective actually achieved, including any evidence accumulation
   via `mpm__mpm_work` action `resolve_contradiction`). Host session
   termination does NOT auto-complete a work item. The agent decides
   when work is done.

8. **Acquire and retain authoritative references.** When work
   depends on external facts or APIs that are version-sensitive,
   implementation-sensitive, likely to be reused, expensive to
   repeatedly rediscover, or important enough that primary-source
   accuracy matters, prefer authoritative / primary documentation
   where practical. Use whichever web / file / download capability
   the host already exposes (web search, browser, shell, curl,
   file tools — whatever is available and permitted) to obtain the
   source material, then register it via `mpm__mpm_references` action
   `add` (params carry the source material and its provenance).
   Preserve enough provenance / version / freshness information to
   judge later suitability — `mpm__mpm_references` action `read`
   surfaces freshness states (`current`, `stale`, `version-bound`,
   `historical`, `unknown`). Reuse an appropriate current reference
   rather than repeatedly rediscovering the same documentation.
   Common workflow classes that benefit: framework / library
   development, API integration, vendor SDK work, standards /
   specification work, version-sensitive infrastructure /
   configuration. The rule is general; apply it whenever
   primary-source accuracy and reusability matter.

9. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

10. **Recovery / fallback.** If the host's native MPM transport
    becomes unavailable mid-session (MCP server down, plugin crashed,
    `mpm__*` tools returning connection errors, etc.), fall back to
    the documented CLI path:
    `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
    Do not abandon persistence when the preferred integration breaks.
    `mpm call` writes to the same substrate with the same
    provenance attribution as the native transport.
<!-- END MPM MANAGED BLOCK -->
<!-- END MPM-MANAGED SECTION:claude-code-instructions -->
```

## OpenCode

**Target file:** `<project>/AGENTS.md` (project scope) or
`~/.config/opencode/AGENTS.md` (user scope)
**Scope:** both project and user scopes supported; picker lives in
the plugin install hook
**Marker convention:** the block must sit between
`<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->` and
`<!-- END MPM-MANAGED SECTION:opencode-instructions -->`.
**Manual verification (no installer):** the `mpm-opencode` plugin
reads `AGENTS.md` at session start AND injects wake context
automatically via its `experimental.chat.system.transform` hook.
Verify with `opencode run` against a project containing the file;
wake should appear in the system prompt before the first turn.

```markdown
<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- The full behavioural protocol is canonical at ~/.mpm/agent_installation/mpm-agent-protocol.md -->

## MPM behavioural contract

You are operating with the MPM (Memory Persistence Module) substrate
on this machine. The following are the non-negotiable MPM behavioural
invariants that turn that capability into reliable behaviour. Edit the
canonical protocol, not this block, for behavioural changes.

1. **Wake is auto-injected on session start** for ClaudeCode, OpenClaw,
   OpenCode, and Pi. Each of these hosts installs a session-start
   hook that fetches MPM wake context and injects it into the
   system prompt before the first model turn. Hermes has no such
   hook — on Hermes, the agent must call `mpm_context` action
   `read_wake_context` once at the start of its first turn to
   obtain the wake payload. Arriving amnesic on a host that should
   be auto-injecting wake means the host integration is broken —
   diagnose the host adapter, not call `read_wake_context`. As a
   manual refresh path (mid-session, explicit refresh, or when the
   host integration is unavailable), `mpm_context` action
   `read_wake_context` is available. The wake payload carries
   orientation signals (mode, persona, recent topics, recent
   memories, recent milestones, last handoff, open work, overdue
   scheduled wakes, a bounded `<available_skills>` catalogue) AND
   the additive `<contextual_focus>` projection — bounded inherited
   working awareness from the routing pipeline (see §1.1).
   Decisions live in `mpm_decisions`; lessons in `mpm_lessons`. Use `params.projection:
   "compact"` for a small id+summary envelope — the full payload
   is the default.

1.1. **Interpret `<contextual_focus>` as inherited working
   awareness**, not as unquestionable truth. Each item carries
   `id / kind / artifact_id / pointer` (identity), `band / rationale /
   why_now` (why it surfaced), `status / detail / truncated` (bounded
   what-to-know), and supplementary `lifecycle_state` /
   `selection_triggers` / `compressed_related_ids` where useful. Treat
   it as bounded inherited context: use it as a durable pointer to
   investigate, follow `pointer` / `artifact_id` with the appropriate
   domain tool (`mpm_decisions show`, `mpm_lessons read`,
   `mpm_memory show`, `mpm_work show`, `mpm_resolve`, etc.) when
   detail is insufficient. Per-item status (`materialized` /
   `pointer_only` / `missing` / `unsupported` / `error`) describes
   whether bounded authoritative detail was available — never whether
   the item mattered; a `missing` item still carries the pointer and
   `why_now`. Selection order is preserved exactly; do not re-rank or
   re-order. If `<contextual_focus>.status == "degraded"` (the
   projection pipeline itself failed), continue using the legacy wake
   context and investigate MPM health only if the missing focus
   materially blocks work — do not chain the diagnostic
   `contextual_*` actions to compensate. An empty items array is not
   an error; continue from legacy wake context and current task.

1.2. **Recent history vs. contextual focus vs. diagnostic surfaces.**
   The wake payload's `<contextual_focus>` is the normal inherited
   working awareness to act on; it is a bounded pointer summary, NOT
   a ranking. Two adjacent `mpm_context` actions serve different
   questions:
   - `mpm_context` action `recent_activity` is the canonical way to
     ask "what recently happened?". It returns a chronological,
     observational feed of recent mutating actions across the substrate
     — newest-first, bounded by `limit`. The result is NOT relevance-ranked.
   - `mpm_context` actions `contextual_candidates`,
     `contextual_selection`, and `contextual_materialization` are
     routing / diagnostic surfaces for inspecting or debugging the
     contextual pipeline. They are NOT part of the normal session-start
     workflow and NOT a ranking mechanism. Do not chain the three to
     reconstruct the integrated result — the wake payload's
     `<contextual_focus>` already contains it.

2. **Persist during work, not only at the end.** Use `mpm_memory`
   action `save`, `mpm_decisions` action `record`,
   `mpm_lessons` action `save`, `mpm_topics`
   action `create`, and `mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Look beyond the compact tool surface.** When you need a
   capability, discover it before assuming it is unavailable. The
   compact native MCP surface intentionally exposes only `mpm_help`,
   `mpm_memory`, and `mpm_context`; the full registered substrate
   is broader (its count is dynamic — do not pin it). Do not assume
   a capability is missing merely because it does not appear in the
   initial `tools/list`. Use `mpm_help` action `list` to discover
   every registered tool (each entry carries `reach_via_cli`).
   Tools hidden from the default surface are reachable via the CLI
   escape hatch `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
   (works on every host). Hosts running with `MPM_EXPOSE_ALL_TOOLS=1`
   on their MCP env block restore the full registered surface
   natively. Do NOT hardcode a tool count in behaviour, tests, or
   prose.

4. **Discover, create, and refine MPM skills.** Skills are reusable
   procedures; treat them as a discover-then-creation lifecycle.
   Before doing repeatable or non-trivial work, look for an
   applicable existing skill via `mpm_context` action
   `proactive_recall_hint` (with `params: {conversation_text:
   "<recent task summary>"}`), read any surfaced skill via
   `mpm_skills` action `read`, and fall back to `mpm_skills`
   action `list` with `params: {scope: "all"}` (the bounded
   `<available_skills>` catalogue in the wake envelope is the
   lightweight first step — discovery adds a context-driven filter,
   it does not replace it). Don't auto-scan the entire skill store
   every turn. After a successful workflow that is non-trivial,
   performed successfully, likely to recur, useful to preserve
   procedurally, or improved by retaining its ordering / checks /
   constraints / failure recovery, capture it as a reusable
   procedure via `mpm_skills` action `save`, or `mpm_skills`
   action `workshop` if the workflow warrants the workshop path
   (`workshop` requires `intent`, `mode`, `task_context`,
   `workflow_description`, `failure_recovery`, `recent_actions`,
   `evidence` per the tool-reference stability contract). Avoid
   skill spam. Do NOT create a skill for trivial one-off actions,
   generic common knowledge, unproven procedures, or workflows not
   likely to recur. Intended lifecycle: discover → execute → learn
   → capture → reuse.

5. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff. On hosts using the default
   compact MCP surface (Claude Code, Hermes, OpenClaw), the MCP path
   is `mpm_context` action `write_handoff` with
   `params: {summary: "<required>", session_id: "<optional>",
   state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   The substrate path `mpm_handoff` action `write` with the same
   `params` shape remains valid via `mpm call mpm_handoff --payload
   '{"action":"write","params":{...}}'` and on hosts running with
   `MPM_EXPOSE_ALL_TOOLS=1` on their MCP env block (which restores
   the full registered MCP surface). `summary` is the only required field.
   Mid-session acknowledgements (`ok`, `thanks`, `ty`, `ack`) are NOT
   session-closing; don't write a handoff on every chat ack.

   For intra-session volatile working state, the substrate tool is
   `mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`); reachable via `mpm call mpm_scratchpad` on hosts
   using the compact MCP surface. Wake is how future-me starts;
   handoff is how future-me receives the previous session.

6. **Track durable objectives as work items.** When a task is
   meaningfully multi-step, likely to span turns or sessions,
   dependent on later information, waiting on follow-up, requiring
   verification, containing important intermediate progress, or
   costly to reconstruct after interruption, open a `mpm_work`
   item reasonably early. Use `mpm_work` action `create` to open,
   `mpm_work` action `update` for material state / plan / status
   changes, `mpm_work` action `note` to retain meaningful progress
   / evidence / context, and `mpm_work` action `complete` with
   `params: {work_id}` to finish. Use `mpm_work` action `reopen`
   when completion is invalidated or new evidence demands more
   work, and `mpm_work` action `cancel` when an objective is
   intentionally abandoned. Do not create a work item for every
   small user request. Conceptual distinction: scratchpad is
   volatile intra-session working state (substrate tool
   `mpm_scratchpad`); work is a durable objective + progress
   lifecycle; handoff is session transition (see #5). Tools not
   exposed by your host's transport remain reachable via the CLI
   fallback (see #10).

7. **Session closure is not work completion.** Three events are
   distinct: `session ended` (the host process exits), `work completed`
   (`mpm_work` action `complete` invoked), and `work verified`
   (objective actually achieved, including any evidence accumulation
   via `mpm_work` action `resolve_contradiction`). Host session
   termination does NOT auto-complete a work item. The agent decides
   when work is done.

8. **Acquire and retain authoritative references.** When work
   depends on external facts or APIs that are version-sensitive,
   implementation-sensitive, likely to be reused, expensive to
   repeatedly rediscover, or important enough that primary-source
   accuracy matters, prefer authoritative / primary documentation
   where practical. Use whichever web / file / download capability
   the host already exposes (web search, browser, shell, curl,
   file tools — whatever is available and permitted) to obtain the
   source material, then register it via `mpm_references` action
   `add` (params carry the source material and its provenance).
   Preserve enough provenance / version / freshness information to
   judge later suitability — `mpm_references` action `read`
   surfaces freshness states (`current`, `stale`, `version-bound`,
   `historical`, `unknown`). Reuse an appropriate current reference
   rather than repeatedly rediscovering the same documentation.
   Common workflow classes that benefit: framework / library
   development, API integration, vendor SDK work, standards /
   specification work, version-sensitive infrastructure /
   configuration. The rule is general; apply it whenever
   primary-source accuracy and reusability matter.

9. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

10. **Recovery / fallback.** If the host's native MPM transport
    becomes unavailable mid-session (MCP server down, plugin crashed,
    `mpm__*` tools returning connection errors, etc.), fall back to
    the documented CLI path:
    `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
    Do not abandon persistence when the preferred integration breaks.
    `mpm call` writes to the same substrate with the same
    provenance attribution as the native transport.
<!-- END MPM MANAGED BLOCK -->
<!-- END MPM-MANAGED SECTION:opencode-instructions -->
```

## Pi

**Target file:** `<project>/AGENTS.md` (per-project) or
`~/.pi/agent/AGENTS.md` (global). Pi walks AGENTS.md from cwd up;
the global file applies to every project.
**Marker convention:** the block must sit between
`<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->` and
`<!-- END MPM-MANAGED SECTION:pi-instructions -->`. The same
flag-style markers apply at every Pi-supported installation depth.
**Manual verification (no installer):** start a Pi session against
a directory containing the file with `--no-context-files` disabled
(default). The agent should have wake context auto-injected by the
`mpm-pi` extension's `session_start` hook, and should be able to
record a handoff via the extension's typed transport (or via
`mpm call mpm_handoff --payload '{"action":"write","params":…}'`
on hosts without the full surface).

```markdown
<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- The full behavioural protocol is canonical at ~/.mpm/agent_installation/mpm-agent-protocol.md -->

## MPM behavioural contract

You are operating with the MPM (Memory Persistence Module) substrate
on this machine. The following are the non-negotiable MPM behavioural
invariants that turn that capability into reliable behaviour. Edit the
canonical protocol, not this block, for behavioural changes.

1. **Wake is auto-injected on session start** for ClaudeCode, OpenClaw,
   OpenCode, and Pi. Each of these hosts installs a session-start
   hook that fetches MPM wake context and injects it into the
   system prompt before the first model turn. Hermes has no such
   hook — on Hermes, the agent must call `mpm_context` action
   `read_wake_context` once at the start of its first turn to
   obtain the wake payload. Arriving amnesic on a host that should
   be auto-injecting wake means the host integration is broken —
   diagnose the host adapter, not call `read_wake_context`. As a
   manual refresh path (mid-session, explicit refresh, or when the
   host integration is unavailable), `mpm_context` action
   `read_wake_context` is available. The wake payload carries
   orientation signals (mode, persona, recent topics, recent
   memories, recent milestones, last handoff, open work, overdue
   scheduled wakes, a bounded `<available_skills>` catalogue) AND
   the additive `<contextual_focus>` projection — bounded inherited
   working awareness from the routing pipeline (see §1.1).
   Decisions live in `mpm_decisions`; lessons in `mpm_lessons`. Use `params.projection:
   "compact"` for a small id+summary envelope — the full payload
   is the default.

1.1. **Interpret `<contextual_focus>` as inherited working
   awareness**, not as unquestionable truth. Each item carries
   `id / kind / artifact_id / pointer` (identity), `band / rationale /
   why_now` (why it surfaced), `status / detail / truncated` (bounded
   what-to-know), and supplementary `lifecycle_state` /
   `selection_triggers` / `compressed_related_ids` where useful. Treat
   it as bounded inherited context: use it as a durable pointer to
   investigate, follow `pointer` / `artifact_id` with the appropriate
   domain tool (`mpm_decisions show`, `mpm_lessons read`,
   `mpm_memory show`, `mpm_work show`, `mpm_resolve`, etc.) when
   detail is insufficient. Per-item status (`materialized` /
   `pointer_only` / `missing` / `unsupported` / `error`) describes
   whether bounded authoritative detail was available — never whether
   the item mattered; a `missing` item still carries the pointer and
   `why_now`. Selection order is preserved exactly; do not re-rank or
   re-order. If `<contextual_focus>.status == "degraded"` (the
   projection pipeline itself failed), continue using the legacy wake
   context and investigate MPM health only if the missing focus
   materially blocks work — do not chain the diagnostic
   `contextual_*` actions to compensate. An empty items array is not
   an error; continue from legacy wake context and current task.

1.2. **Recent history vs. contextual focus vs. diagnostic surfaces.**
   The wake payload's `<contextual_focus>` is the normal inherited
   working awareness to act on; it is a bounded pointer summary, NOT
   a ranking. Two adjacent `mpm_context` actions serve different
   questions:
   - `mpm_context` action `recent_activity` is the canonical way to
     ask "what recently happened?". It returns a chronological,
     observational feed of recent mutating actions across the substrate
     — newest-first, bounded by `limit`. The result is NOT relevance-ranked.
   - `mpm_context` actions `contextual_candidates`,
     `contextual_selection`, and `contextual_materialization` are
     routing / diagnostic surfaces for inspecting or debugging the
     contextual pipeline. They are NOT part of the normal session-start
     workflow and NOT a ranking mechanism. Do not chain the three to
     reconstruct the integrated result — the wake payload's
     `<contextual_focus>` already contains it.

2. **Persist during work, not only at the end.** Use `mpm_memory`
   action `save`, `mpm_decisions` action `record`,
   `mpm_lessons` action `save`, `mpm_topics`
   action `create`, and `mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Look beyond the compact tool surface.** When you need a
   capability, discover it before assuming it is unavailable. The
   compact native MCP surface intentionally exposes only `mpm_help`,
   `mpm_memory`, and `mpm_context`; the full registered substrate
   is broader (its count is dynamic — do not pin it). Do not assume
   a capability is missing merely because it does not appear in the
   initial `tools/list`. Use `mpm_help` action `list` to discover
   every registered tool (each entry carries `reach_via_cli`).
   Tools hidden from the default surface are reachable via the CLI
   escape hatch `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
   (works on every host). Hosts running with `MPM_EXPOSE_ALL_TOOLS=1`
   on their MCP env block restore the full registered surface
   natively. Do NOT hardcode a tool count in behaviour, tests, or
   prose.

4. **Discover, create, and refine MPM skills.** Skills are reusable
   procedures; treat them as a discover-then-creation lifecycle.
   Before doing repeatable or non-trivial work, look for an
   applicable existing skill via `mpm_context` action
   `proactive_recall_hint` (with `params: {conversation_text:
   "<recent task summary>"}`), read any surfaced skill via
   `mpm_skills` action `read`, and fall back to `mpm_skills`
   action `list` with `params: {scope: "all"}` (the bounded
   `<available_skills>` catalogue in the wake envelope is the
   lightweight first step — discovery adds a context-driven filter,
   it does not replace it). Don't auto-scan the entire skill store
   every turn. After a successful workflow that is non-trivial,
   performed successfully, likely to recur, useful to preserve
   procedurally, or improved by retaining its ordering / checks /
   constraints / failure recovery, capture it as a reusable
   procedure via `mpm_skills` action `save`, or `mpm_skills`
   action `workshop` if the workflow warrants the workshop path
   (`workshop` requires `intent`, `mode`, `task_context`,
   `workflow_description`, `failure_recovery`, `recent_actions`,
   `evidence` per the tool-reference stability contract). Avoid
   skill spam. Do NOT create a skill for trivial one-off actions,
   generic common knowledge, unproven procedures, or workflows not
   likely to recur. Intended lifecycle: discover → execute → learn
   → capture → reuse.

5. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff. On hosts using the default
   compact MCP surface (Claude Code, Hermes, OpenClaw), the MCP path
   is `mpm_context` action `write_handoff` with
   `params: {summary: "<required>", session_id: "<optional>",
   state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   The substrate path `mpm_handoff` action `write` with the same
   `params` shape remains valid via `mpm call mpm_handoff --payload
   '{"action":"write","params":{...}}'` and on hosts running with
   `MPM_EXPOSE_ALL_TOOLS=1` on their MCP env block (which restores
   the full registered MCP surface). `summary` is the only required field.
   Mid-session acknowledgements (`ok`, `thanks`, `ty`, `ack`) are NOT
   session-closing; don't write a handoff on every chat ack.

   For intra-session volatile working state, the substrate tool is
   `mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`); reachable via `mpm call mpm_scratchpad` on hosts
   using the compact MCP surface. Wake is how future-me starts;
   handoff is how future-me receives the previous session.

6. **Track durable objectives as work items.** When a task is
   meaningfully multi-step, likely to span turns or sessions,
   dependent on later information, waiting on follow-up, requiring
   verification, containing important intermediate progress, or
   costly to reconstruct after interruption, open a `mpm_work`
   item reasonably early. Use `mpm_work` action `create` to open,
   `mpm_work` action `update` for material state / plan / status
   changes, `mpm_work` action `note` to retain meaningful progress
   / evidence / context, and `mpm_work` action `complete` with
   `params: {work_id}` to finish. Use `mpm_work` action `reopen`
   when completion is invalidated or new evidence demands more
   work, and `mpm_work` action `cancel` when an objective is
   intentionally abandoned. Do not create a work item for every
   small user request. Conceptual distinction: scratchpad is
   volatile intra-session working state (substrate tool
   `mpm_scratchpad`); work is a durable objective + progress
   lifecycle; handoff is session transition (see #5). Tools not
   exposed by your host's transport remain reachable via the CLI
   fallback (see #10).

7. **Session closure is not work completion.** Three events are
   distinct: `session ended` (the host process exits), `work completed`
   (`mpm_work` action `complete` invoked), and `work verified`
   (objective actually achieved, including any evidence accumulation
   via `mpm_work` action `resolve_contradiction`). Host session
   termination does NOT auto-complete a work item. The agent decides
   when work is done.

8. **Acquire and retain authoritative references.** When work
   depends on external facts or APIs that are version-sensitive,
   implementation-sensitive, likely to be reused, expensive to
   repeatedly rediscover, or important enough that primary-source
   accuracy matters, prefer authoritative / primary documentation
   where practical. Use whichever web / file / download capability
   the host already exposes (web search, browser, shell, curl,
   file tools — whatever is available and permitted) to obtain the
   source material, then register it via `mpm_references` action
   `add` (params carry the source material and its provenance).
   Preserve enough provenance / version / freshness information to
   judge later suitability — `mpm_references` action `read`
   surfaces freshness states (`current`, `stale`, `version-bound`,
   `historical`, `unknown`). Reuse an appropriate current reference
   rather than repeatedly rediscovering the same documentation.
   Common workflow classes that benefit: framework / library
   development, API integration, vendor SDK work, standards /
   specification work, version-sensitive infrastructure /
   configuration. The rule is general; apply it whenever
   primary-source accuracy and reusability matter.

9. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

10. **Recovery / fallback.** If the host's native MPM transport
    becomes unavailable mid-session (MCP server down, plugin crashed,
    `mpm__*` tools returning connection errors, etc.), fall back to
    the documented CLI path:
    `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
    Do not abandon persistence when the preferred integration breaks.
    `mpm call` writes to the same substrate with the same
    provenance attribution as the native transport.
<!-- END MPM MANAGED BLOCK -->
<!-- END MPM-MANAGED SECTION:pi-instructions -->
```

## Hermes

**Target file:** `<project>/.hermes.md` (or `HERMES.md`).
**Marker convention:** Hermes does not enforce a managed-block
convention. The installer uses a leading `<!-- BEGIN MPM MANAGED BLOCK:mpm-hermes -->`
comment block as a stable re-install anchor; the canonical
managed-block markers from this file (the universal
`<!-- BEGIN MPM MANAGED BLOCK -->` pair) live inside it.
**Manual verification (no installer):** start a Hermes session.
The persona stays in `~/.hermes/SOUL.md`; the MPM behavioural
contract lands via `.hermes.md`. The compact MCP surface is
`mcp__mpm__mpm_memory`, `mcp__mpm__mpm_context`,
`mcp__mpm__mpm_help`; handoff goes through `mcp__mpm__mpm_context`
action `write_handoff` / `read_handoff`. The full 22-tool
surface is restored by setting `MPM_EXPOSE_ALL_TOOLS=1` on the
MCP env block (which exposes `mcp__mpm__mpm_handoff` directly).

```markdown
<!-- BEGIN MPM MANAGED BLOCK:mpm-hermes -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- The full behavioural protocol is canonical at ~/.mpm/agent_installation/mpm-agent-protocol.md -->

## MPM behavioural contract

You are operating with the MPM (Memory Persistence Module) substrate
on this machine. The following are the non-negotiable MPM behavioural
invariants that turn that capability into reliable behaviour. Edit the
canonical protocol, not this block, for behavioural changes.

1. **Wake is auto-injected on session start** for ClaudeCode, OpenClaw,
   OpenCode, and Pi. Each of these hosts installs a session-start
   hook that fetches MPM wake context and injects it into the
   system prompt before the first model turn. Hermes has no such
   hook — on Hermes, the agent must call `mcp__mpm__mpm_context` action
   `read_wake_context` once at the start of its first turn to
   obtain the wake payload. Arriving amnesic on a host that should
   be auto-injecting wake means the host integration is broken —
   diagnose the host adapter, not call `read_wake_context`. As a
   manual refresh path (mid-session, explicit refresh, or when the
   host integration is unavailable), `mcp__mpm__mpm_context` action
   `read_wake_context` is available. The wake payload carries
   orientation signals (mode, persona, recent topics, recent
   memories, recent milestones, last handoff, open work, overdue
   scheduled wakes, a bounded `<available_skills>` catalogue) AND
   the additive `<contextual_focus>` projection — bounded inherited
   working awareness from the routing pipeline (see §1.1).
   Decisions live in `mpm_decisions`; lessons in `mpm_lessons`. Use `params.projection:
   "compact"` for a small id+summary envelope — the full payload
   is the default.

1.1. **Interpret `<contextual_focus>` as inherited working
   awareness**, not as unquestionable truth. Each item carries
   `id / kind / artifact_id / pointer` (identity), `band / rationale /
   why_now` (why it surfaced), `status / detail / truncated` (bounded
   what-to-know), and supplementary `lifecycle_state` /
   `selection_triggers` / `compressed_related_ids` where useful. Treat
   it as bounded inherited context: use it as a durable pointer to
   investigate, follow `pointer` / `artifact_id` with the appropriate
   domain tool (`mpm_decisions show`, `mpm_lessons read`,
   `mpm_memory show`, `mpm_work show`, `mpm_resolve`, etc.) when
   detail is insufficient. Per-item status (`materialized` /
   `pointer_only` / `missing` / `unsupported` / `error`) describes
   whether bounded authoritative detail was available — never whether
   the item mattered; a `missing` item still carries the pointer and
   `why_now`. Selection order is preserved exactly; do not re-rank or
   re-order. If `<contextual_focus>.status == "degraded"` (the
   projection pipeline itself failed), continue using the legacy wake
   context and investigate MPM health only if the missing focus
   materially blocks work — do not chain the diagnostic
   `contextual_*` actions to compensate. An empty items array is not
   an error; continue from legacy wake context and current task.

1.2. **Recent history vs. contextual focus vs. diagnostic surfaces.**
   The wake payload's `<contextual_focus>` is the normal inherited
   working awareness to act on; it is a bounded pointer summary, NOT
   a ranking. Two adjacent `mcp__mpm__mpm_context` actions serve different
   questions:
   - `mcp__mpm__mpm_context` action `recent_activity` is the canonical way to
     ask "what recently happened?". It returns a chronological,
     observational feed of recent mutating actions across the substrate
     — newest-first, bounded by `limit`. The result is NOT relevance-ranked.
   - `mcp__mpm__mpm_context` actions `contextual_candidates`,
     `contextual_selection`, and `contextual_materialization` are
     routing / diagnostic surfaces for inspecting or debugging the
     contextual pipeline. They are NOT part of the normal session-start
     workflow and NOT a ranking mechanism. Do not chain the three to
     reconstruct the integrated result — the wake payload's
     `<contextual_focus>` already contains it.

2. **Persist during work, not only at the end.** Use `mcp__mpm__mpm_memory`
   action `save`, `mcp__mpm__mpm_decisions` action `record`,
   `mcp__mpm__mpm_lessons` action `save`, `mcp__mpm__mpm_topics`
   action `create`, and `mcp__mpm__mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Look beyond the compact tool surface.** When you need a
   capability, discover it before assuming it is unavailable. The
   compact native MCP surface intentionally exposes only `mpm_help`,
   `mpm_memory`, and `mpm_context`; the full registered substrate
   is broader (its count is dynamic — do not pin it). Do not assume
   a capability is missing merely because it does not appear in the
   initial `tools/list`. Use `mcp__mpm__mpm_help` action `list` to discover
   every registered tool (each entry carries `reach_via_cli`).
   Tools hidden from the default surface are reachable via the CLI
   escape hatch `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
   (works on every host). Hosts running with `MPM_EXPOSE_ALL_TOOLS=1`
   on their MCP env block restore the full registered surface
   natively. Do NOT hardcode a tool count in behaviour, tests, or
   prose.

4. **Discover, create, and refine MPM skills.** Skills are reusable
   procedures; treat them as a discover-then-creation lifecycle.
   Before doing repeatable or non-trivial work, look for an
   applicable existing skill via `mcp__mpm__mpm_context` action
   `proactive_recall_hint` (with `params: {conversation_text:
   "<recent task summary>"}`), read any surfaced skill via
   `mcp__mpm__mpm_skills` action `read`, and fall back to `mcp__mpm__mpm_skills`
   action `list` with `params: {scope: "all"}` (the bounded
   `<available_skills>` catalogue in the wake envelope is the
   lightweight first step — discovery adds a context-driven filter,
   it does not replace it). Don't auto-scan the entire skill store
   every turn. After a successful workflow that is non-trivial,
   performed successfully, likely to recur, useful to preserve
   procedurally, or improved by retaining its ordering / checks /
   constraints / failure recovery, capture it as a reusable
   procedure via `mcp__mpm__mpm_skills` action `save`, or `mcp__mpm__mpm_skills`
   action `workshop` if the workflow warrants the workshop path
   (`workshop` requires `intent`, `mode`, `task_context`,
   `workflow_description`, `failure_recovery`, `recent_actions`,
   `evidence` per the tool-reference stability contract). Avoid
   skill spam. Do NOT create a skill for trivial one-off actions,
   generic common knowledge, unproven procedures, or workflows not
   likely to recur. Intended lifecycle: discover → execute → learn
   → capture → reuse.

5. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff. On hosts using the default
   compact MCP surface (Claude Code, Hermes, OpenClaw), the MCP path
   is `mcp__mpm__mpm_context` action `write_handoff` with
   `params: {summary: "<required>", session_id: "<optional>",
   state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   The substrate path `mcp__mpm__mpm_handoff` action `write` with the same
   `params` shape remains valid via `mpm call mpm_handoff --payload
   '{"action":"write","params":{...}}'` and on hosts running with
   `MPM_EXPOSE_ALL_TOOLS=1` on their MCP env block (which restores
   the full registered MCP surface). `summary` is the only required field.
   Mid-session acknowledgements (`ok`, `thanks`, `ty`, `ack`) are NOT
   session-closing; don't write a handoff on every chat ack.

   For intra-session volatile working state, the substrate tool is
   `mcp__mpm__mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`); reachable via `mpm call mpm_scratchpad` on hosts
   using the compact MCP surface. Wake is how future-me starts;
   handoff is how future-me receives the previous session.

6. **Track durable objectives as work items.** When a task is
   meaningfully multi-step, likely to span turns or sessions,
   dependent on later information, waiting on follow-up, requiring
   verification, containing important intermediate progress, or
   costly to reconstruct after interruption, open a `mpm_work`
   item reasonably early. Use `mcp__mpm__mpm_work` action `create` to open,
   `mcp__mpm__mpm_work` action `update` for material state / plan / status
   changes, `mcp__mpm__mpm_work` action `note` to retain meaningful progress
   / evidence / context, and `mcp__mpm__mpm_work` action `complete` with
   `params: {work_id}` to finish. Use `mcp__mpm__mpm_work` action `reopen`
   when completion is invalidated or new evidence demands more
   work, and `mcp__mpm__mpm_work` action `cancel` when an objective is
   intentionally abandoned. Do not create a work item for every
   small user request. Conceptual distinction: scratchpad is
   volatile intra-session working state (substrate tool
   `mpm_scratchpad`); work is a durable objective + progress
   lifecycle; handoff is session transition (see #5). Tools not
   exposed by your host's transport remain reachable via the CLI
   fallback (see #10).

7. **Session closure is not work completion.** Three events are
   distinct: `session ended` (the host process exits), `work completed`
   (`mcp__mpm__mpm_work` action `complete` invoked), and `work verified`
   (objective actually achieved, including any evidence accumulation
   via `mcp__mpm__mpm_work` action `resolve_contradiction`). Host session
   termination does NOT auto-complete a work item. The agent decides
   when work is done.

8. **Acquire and retain authoritative references.** When work
   depends on external facts or APIs that are version-sensitive,
   implementation-sensitive, likely to be reused, expensive to
   repeatedly rediscover, or important enough that primary-source
   accuracy matters, prefer authoritative / primary documentation
   where practical. Use whichever web / file / download capability
   the host already exposes (web search, browser, shell, curl,
   file tools — whatever is available and permitted) to obtain the
   source material, then register it via `mcp__mpm__mpm_references` action
   `add` (params carry the source material and its provenance).
   Preserve enough provenance / version / freshness information to
   judge later suitability — `mcp__mpm__mpm_references` action `read`
   surfaces freshness states (`current`, `stale`, `version-bound`,
   `historical`, `unknown`). Reuse an appropriate current reference
   rather than repeatedly rediscovering the same documentation.
   Common workflow classes that benefit: framework / library
   development, API integration, vendor SDK work, standards /
   specification work, version-sensitive infrastructure /
   configuration. The rule is general; apply it whenever
   primary-source accuracy and reusability matter.

9. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

10. **Recovery / fallback.** If the host's native MPM transport
    becomes unavailable mid-session (MCP server down, plugin crashed,
    `mpm__*` tools returning connection errors, etc.), fall back to
    the documented CLI path:
    `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
    Do not abandon persistence when the preferred integration breaks.
    `mpm call` writes to the same substrate with the same
    provenance attribution as the native transport.
<!-- END MPM MANAGED BLOCK -->
<!-- END MPM MANAGED BLOCK:mpm-hermes -->
```

## OpenClaw
OpenClaw follows the same **persistent managed instruction** model as
the other four hosts. The agent's `SOUL.md` (resolved per-agent from
`openclaw.json` → `agents.entries.<id>.workspace`) is the canonical
behavioural-contract surface; `mpm-memory-openclaw/install.sh` writes
the MPM managed block to that path on install and refreshes it on
reinstall. The persona (808) and any user content above/below the
managed markers is preserved.

The MPM integrations on OpenClaw are **three layered components**, not
substitutes for one another:

```
SOUL.md                    persistent MPM behavioural contract
  + user/persona content     (managed markers surround the MPM block)

mpm-memory-openclaw         dynamic session wake/context injection
                            (session_start → agent_turn_prepare,
                            returning prependContext)

mpm-auto-mode-persona-openclaw
                            dynamic mode/persona routing
                            (message:received + agent:bootstrap)
```

Runtime wake injection is **not** a substitute for the persistent
block. The wake payload delivers *what context exists now*; the
SOUL.md block delivers *how MPM must be used across the session*.
OpenClaw requires both.

**Wake** is delivered automatically by the plugin's
`session_start` → `agent_turn_prepare` typed-hook chain (returning
`prependContext`). **Handoff** is delivered via the default compact
MCP surface — call `mpm_context` action `write_handoff` with
`params: {summary, state, commitments, open_questions}`. Do NOT
call `mcp__mpm__mpm_handoff` (the substrate `mpm_handoff` tool is
not exposed on the default 3-tool surface; reach it only via the
`mpm call mpm_handoff` CLI escape hatch if the MCP transport is
unavailable).

**To install OpenClaw MPM integration:**

```
git clone <repo> ~/.openclaw/workspace/projects/mpm  # or symlink
cd ~/.openclaw/workspace/projects/mpm/mpm-memory-openclaw
./install.sh
```

The install writes the MPM managed block into the active agent's
`SOUL.md` (resolved from `agents.entries.<id>.workspace` in
`openclaw.json`), preserves existing persona/user content outside
the markers, and refreshes stale managed content on reinstall.

For installation details and validation evidence, see
[`mpm-memory-openclaw/README.md`](./mpm-memory-openclaw/README.md)
and the OpenClaw row in `INSTALL.md`.

```markdown
<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- The full behavioural protocol is canonical at ~/.mpm/agent_installation/mpm-agent-protocol.md -->

## MPM behavioural contract

You are operating with the MPM (Memory Persistence Module) substrate
on this machine. The following are the non-negotiable MPM behavioural
invariants that turn that capability into reliable behaviour. Edit the
canonical protocol, not this block, for behavioural changes.

1. **Wake is auto-injected on session start** for ClaudeCode, OpenClaw,
   OpenCode, and Pi. Each of these hosts installs a session-start
   hook that fetches MPM wake context and injects it into the
   system prompt before the first model turn. Hermes has no such
   hook — on Hermes, the agent must call `mcp__mpm__mpm_context` action
   `read_wake_context` once at the start of its first turn to
   obtain the wake payload. Arriving amnesic on a host that should
   be auto-injecting wake means the host integration is broken —
   diagnose the host adapter, not call `read_wake_context`. As a
   manual refresh path (mid-session, explicit refresh, or when the
   host integration is unavailable), `mcp__mpm__mpm_context` action
   `read_wake_context` is available. The wake payload carries
   orientation signals (mode, persona, recent topics, recent
   memories, recent milestones, last handoff, open work, overdue
   scheduled wakes, a bounded `<available_skills>` catalogue) AND
   the additive `<contextual_focus>` projection — bounded inherited
   working awareness from the routing pipeline (see §1.1).
   Decisions live in `mpm_decisions`; lessons in `mpm_lessons`. Use `params.projection:
   "compact"` for a small id+summary envelope — the full payload
   is the default.

1.1. **Interpret `<contextual_focus>` as inherited working
   awareness**, not as unquestionable truth. Each item carries
   `id / kind / artifact_id / pointer` (identity), `band / rationale /
   why_now` (why it surfaced), `status / detail / truncated` (bounded
   what-to-know), and supplementary `lifecycle_state` /
   `selection_triggers` / `compressed_related_ids` where useful. Treat
   it as bounded inherited context: use it as a durable pointer to
   investigate, follow `pointer` / `artifact_id` with the appropriate
   domain tool (`mpm_decisions show`, `mpm_lessons read`,
   `mpm_memory show`, `mpm_work show`, `mpm_resolve`, etc.) when
   detail is insufficient. Per-item status (`materialized` /
   `pointer_only` / `missing` / `unsupported` / `error`) describes
   whether bounded authoritative detail was available — never whether
   the item mattered; a `missing` item still carries the pointer and
   `why_now`. Selection order is preserved exactly; do not re-rank or
   re-order. If `<contextual_focus>.status == "degraded"` (the
   projection pipeline itself failed), continue using the legacy wake
   context and investigate MPM health only if the missing focus
   materially blocks work — do not chain the diagnostic
   `contextual_*` actions to compensate. An empty items array is not
   an error; continue from legacy wake context and current task.

1.2. **Recent history vs. contextual focus vs. diagnostic surfaces.**
   The wake payload's `<contextual_focus>` is the normal inherited
   working awareness to act on; it is a bounded pointer summary, NOT
   a ranking. Two adjacent `mcp__mpm__mpm_context` actions serve different
   questions:
   - `mcp__mpm__mpm_context` action `recent_activity` is the canonical way to
     ask "what recently happened?". It returns a chronological,
     observational feed of recent mutating actions across the substrate
     — newest-first, bounded by `limit`. The result is NOT relevance-ranked.
   - `mcp__mpm__mpm_context` actions `contextual_candidates`,
     `contextual_selection`, and `contextual_materialization` are
     routing / diagnostic surfaces for inspecting or debugging the
     contextual pipeline. They are NOT part of the normal session-start
     workflow and NOT a ranking mechanism. Do not chain the three to
     reconstruct the integrated result — the wake payload's
     `<contextual_focus>` already contains it.

2. **Persist during work, not only at the end.** Use `mcp__mpm__mpm_memory`
   action `save`, `mcp__mpm__mpm_decisions` action `record`,
   `mcp__mpm__mpm_lessons` action `save`, `mcp__mpm__mpm_topics`
   action `create`, and `mcp__mpm__mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Look beyond the compact tool surface.** When you need a
   capability, discover it before assuming it is unavailable. The
   compact native MCP surface intentionally exposes only `mpm_help`,
   `mpm_memory`, and `mpm_context`; the full registered substrate
   is broader (its count is dynamic — do not pin it). Do not assume
   a capability is missing merely because it does not appear in the
   initial `tools/list`. Use `mcp__mpm__mpm_help` action `list` to discover
   every registered tool (each entry carries `reach_via_cli`).
   Tools hidden from the default surface are reachable via the CLI
   escape hatch `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
   (works on every host). Hosts running with `MPM_EXPOSE_ALL_TOOLS=1`
   on their MCP env block restore the full registered surface
   natively. Do NOT hardcode a tool count in behaviour, tests, or
   prose.

4. **Discover, create, and refine MPM skills.** Skills are reusable
   procedures; treat them as a discover-then-creation lifecycle.
   Before doing repeatable or non-trivial work, look for an
   applicable existing skill via `mcp__mpm__mpm_context` action
   `proactive_recall_hint` (with `params: {conversation_text:
   "<recent task summary>"}`), read any surfaced skill via
   `mcp__mpm__mpm_skills` action `read`, and fall back to `mcp__mpm__mpm_skills`
   action `list` with `params: {scope: "all"}` (the bounded
   `<available_skills>` catalogue in the wake envelope is the
   lightweight first step — discovery adds a context-driven filter,
   it does not replace it). Don't auto-scan the entire skill store
   every turn. After a successful workflow that is non-trivial,
   performed successfully, likely to recur, useful to preserve
   procedurally, or improved by retaining its ordering / checks /
   constraints / failure recovery, capture it as a reusable
   procedure via `mcp__mpm__mpm_skills` action `save`, or `mcp__mpm__mpm_skills`
   action `workshop` if the workflow warrants the workshop path
   (`workshop` requires `intent`, `mode`, `task_context`,
   `workflow_description`, `failure_recovery`, `recent_actions`,
   `evidence` per the tool-reference stability contract). Avoid
   skill spam. Do NOT create a skill for trivial one-off actions,
   generic common knowledge, unproven procedures, or workflows not
   likely to recur. Intended lifecycle: discover → execute → learn
   → capture → reuse.

5. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff. On hosts using the default
   compact MCP surface (Claude Code, Hermes, OpenClaw), the MCP path
   is `mcp__mpm__mpm_context` action `write_handoff` with
   `params: {summary: "<required>", session_id: "<optional>",
   state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   The substrate path `mcp__mpm__mpm_handoff` action `write` with the same
   `params` shape remains valid via `mpm call mpm_handoff --payload
   '{"action":"write","params":{...}}'` and on hosts running with
   `MPM_EXPOSE_ALL_TOOLS=1` on their MCP env block (which restores
   the full registered MCP surface). `summary` is the only required field.
   Mid-session acknowledgements (`ok`, `thanks`, `ty`, `ack`) are NOT
   session-closing; don't write a handoff on every chat ack.

   For intra-session volatile working state, the substrate tool is
   `mcp__mpm__mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`); reachable via `mpm call mpm_scratchpad` on hosts
   using the compact MCP surface. Wake is how future-me starts;
   handoff is how future-me receives the previous session.

6. **Track durable objectives as work items.** When a task is
   meaningfully multi-step, likely to span turns or sessions,
   dependent on later information, waiting on follow-up, requiring
   verification, containing important intermediate progress, or
   costly to reconstruct after interruption, open a `mpm_work`
   item reasonably early. Use `mcp__mpm__mpm_work` action `create` to open,
   `mcp__mpm__mpm_work` action `update` for material state / plan / status
   changes, `mcp__mpm__mpm_work` action `note` to retain meaningful progress
   / evidence / context, and `mcp__mpm__mpm_work` action `complete` with
   `params: {work_id}` to finish. Use `mcp__mpm__mpm_work` action `reopen`
   when completion is invalidated or new evidence demands more
   work, and `mcp__mpm__mpm_work` action `cancel` when an objective is
   intentionally abandoned. Do not create a work item for every
   small user request. Conceptual distinction: scratchpad is
   volatile intra-session working state (substrate tool
   `mpm_scratchpad`); work is a durable objective + progress
   lifecycle; handoff is session transition (see #5). Tools not
   exposed by your host's transport remain reachable via the CLI
   fallback (see #10).

7. **Session closure is not work completion.** Three events are
   distinct: `session ended` (the host process exits), `work completed`
   (`mcp__mpm__mpm_work` action `complete` invoked), and `work verified`
   (objective actually achieved, including any evidence accumulation
   via `mcp__mpm__mpm_work` action `resolve_contradiction`). Host session
   termination does NOT auto-complete a work item. The agent decides
   when work is done.

8. **Acquire and retain authoritative references.** When work
   depends on external facts or APIs that are version-sensitive,
   implementation-sensitive, likely to be reused, expensive to
   repeatedly rediscover, or important enough that primary-source
   accuracy matters, prefer authoritative / primary documentation
   where practical. Use whichever web / file / download capability
   the host already exposes (web search, browser, shell, curl,
   file tools — whatever is available and permitted) to obtain the
   source material, then register it via `mcp__mpm__mpm_references` action
   `add` (params carry the source material and its provenance).
   Preserve enough provenance / version / freshness information to
   judge later suitability — `mcp__mpm__mpm_references` action `read`
   surfaces freshness states (`current`, `stale`, `version-bound`,
   `historical`, `unknown`). Reuse an appropriate current reference
   rather than repeatedly rediscovering the same documentation.
   Common workflow classes that benefit: framework / library
   development, API integration, vendor SDK work, standards /
   specification work, version-sensitive infrastructure /
   configuration. The rule is general; apply it whenever
   primary-source accuracy and reusability matter.

9. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

10. **Recovery / fallback.** If the host's native MPM transport
    becomes unavailable mid-session (MCP server down, plugin crashed,
    `mpm__*` tools returning connection errors, etc.), fall back to
    the documented CLI path:
    `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
    Do not abandon persistence when the preferred integration breaks.
    `mpm call` writes to the same substrate with the same
    provenance attribution as the native transport.
<!-- END MPM MANAGED BLOCK -->
<!-- END MPM-MANAGED SECTION:openclaw-instructions -->
```
---

# The canonical managed block (host-neutral)

The block below is the **universal managed block** — host-neutral,
transport-rendered at install time. The canonical block uses bare
canonical tool names (`mpm_handoff`, `mpm_memory`, ...). The host
adapter's renderer applies each host's tool-namespace prefix at
install time, producing the rendered output that goes into the
agent's persistent instruction file. The result is one canonical
block, four transport-rendered variants, byte-equivalent for
behaviour.

To regenerate the four host variants (after editing this canonical
block or any host's header/footer):

```
python3 scripts/render_managed_blocks.py
```

To verify byte-for-byte parity without writing:

```
python3 scripts/render_managed_blocks.py --check
```

The four copy/paste examples at the top of this file are produced
by the same render, byte-equivalent to what the per-host adapter
template snippet contains and what each per-host installer writes
into the target persistent file.

```text
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- The full behavioural protocol is canonical at ~/.mpm/agent_installation/mpm-agent-protocol.md -->

## MPM behavioural contract

You are operating with the MPM (Memory Persistence Module) substrate
on this machine. The following are the non-negotiable MPM behavioural
invariants that turn that capability into reliable behaviour. Edit the
canonical protocol, not this block, for behavioural changes.

1. **Wake is auto-injected on session start** for ClaudeCode, OpenClaw,
   OpenCode, and Pi. Each of these hosts installs a session-start
   hook that fetches MPM wake context and injects it into the
   system prompt before the first model turn. Hermes has no such
   hook — on Hermes, the agent must call `mpm_context` action
   `read_wake_context` once at the start of its first turn to
   obtain the wake payload. Arriving amnesic on a host that should
   be auto-injecting wake means the host integration is broken —
   diagnose the host adapter, not call `read_wake_context`. As a
   manual refresh path (mid-session, explicit refresh, or when the
   host integration is unavailable), `mpm_context` action
   `read_wake_context` is available. The wake payload carries
   orientation signals (mode, persona, recent topics, recent
   memories, recent milestones, last handoff, open work, overdue
   scheduled wakes, a bounded `<available_skills>` catalogue) AND
   the additive `<contextual_focus>` projection — bounded inherited
   working awareness from the routing pipeline (see §1.1).
   Decisions live in `mpm_decisions`; lessons in `mpm_lessons`. Use `params.projection:
   "compact"` for a small id+summary envelope — the full payload
   is the default.

1.1. **Interpret `<contextual_focus>` as inherited working
   awareness**, not as unquestionable truth. Each item carries
   `id / kind / artifact_id / pointer` (identity), `band / rationale /
   why_now` (why it surfaced), `status / detail / truncated` (bounded
   what-to-know), and supplementary `lifecycle_state` /
   `selection_triggers` / `compressed_related_ids` where useful. Treat
   it as bounded inherited context: use it as a durable pointer to
   investigate, follow `pointer` / `artifact_id` with the appropriate
   domain tool (`mpm_decisions show`, `mpm_lessons read`,
   `mpm_memory show`, `mpm_work show`, `mpm_resolve`, etc.) when
   detail is insufficient. Per-item status (`materialized` /
   `pointer_only` / `missing` / `unsupported` / `error`) describes
   whether bounded authoritative detail was available — never whether
   the item mattered; a `missing` item still carries the pointer and
   `why_now`. Selection order is preserved exactly; do not re-rank or
   re-order. If `<contextual_focus>.status == "degraded"` (the
   projection pipeline itself failed), continue using the legacy wake
   context and investigate MPM health only if the missing focus
   materially blocks work — do not chain the diagnostic
   `contextual_*` actions to compensate. An empty items array is not
   an error; continue from legacy wake context and current task.

1.2. **Recent history vs. contextual focus vs. diagnostic surfaces.**
   The wake payload's `<contextual_focus>` is the normal inherited
   working awareness to act on; it is a bounded pointer summary, NOT
   a ranking. Two adjacent `mpm_context` actions serve different
   questions:
   - `mpm_context` action `recent_activity` is the canonical way to
     ask "what recently happened?". It returns a chronological,
     observational feed of recent mutating actions across the substrate
     — newest-first, bounded by `limit`. The result is NOT relevance-ranked.
   - `mpm_context` actions `contextual_candidates`,
     `contextual_selection`, and `contextual_materialization` are
     routing / diagnostic surfaces for inspecting or debugging the
     contextual pipeline. They are NOT part of the normal session-start
     workflow and NOT a ranking mechanism. Do not chain the three to
     reconstruct the integrated result — the wake payload's
     `<contextual_focus>` already contains it.

2. **Persist during work, not only at the end.** Use `mpm_memory`
   action `save`, `mpm_decisions` action `record`,
   `mpm_lessons` action `save`, `mpm_topics`
   action `create`, and `mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Look beyond the compact tool surface.** When you need a
   capability, discover it before assuming it is unavailable. The
   compact native MCP surface intentionally exposes only `mpm_help`,
   `mpm_memory`, and `mpm_context`; the full registered substrate
   is broader (its count is dynamic — do not pin it). Do not assume
   a capability is missing merely because it does not appear in the
   initial `tools/list`. Use `mpm_help` action `list` to discover
   every registered tool (each entry carries `reach_via_cli`).
   Tools hidden from the default surface are reachable via the CLI
   escape hatch `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
   (works on every host). Hosts running with `MPM_EXPOSE_ALL_TOOLS=1`
   on their MCP env block restore the full registered surface
   natively. Do NOT hardcode a tool count in behaviour, tests, or
   prose.

4. **Discover, create, and refine MPM skills.** Skills are reusable
   procedures; treat them as a discover-then-creation lifecycle.
   Before doing repeatable or non-trivial work, look for an
   applicable existing skill via `mpm_context` action
   `proactive_recall_hint` (with `params: {conversation_text:
   "<recent task summary>"}`), read any surfaced skill via
   `mpm_skills` action `read`, and fall back to `mpm_skills`
   action `list` with `params: {scope: "all"}` (the bounded
   `<available_skills>` catalogue in the wake envelope is the
   lightweight first step — discovery adds a context-driven filter,
   it does not replace it). Don't auto-scan the entire skill store
   every turn. After a successful workflow that is non-trivial,
   performed successfully, likely to recur, useful to preserve
   procedurally, or improved by retaining its ordering / checks /
   constraints / failure recovery, capture it as a reusable
   procedure via `mpm_skills` action `save`, or `mpm_skills`
   action `workshop` if the workflow warrants the workshop path
   (`workshop` requires `intent`, `mode`, `task_context`,
   `workflow_description`, `failure_recovery`, `recent_actions`,
   `evidence` per the tool-reference stability contract). Avoid
   skill spam. Do NOT create a skill for trivial one-off actions,
   generic common knowledge, unproven procedures, or workflows not
   likely to recur. Intended lifecycle: discover → execute → learn
   → capture → reuse.

5. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff. On hosts using the default
   compact MCP surface (Claude Code, Hermes, OpenClaw), the MCP path
   is `mpm_context` action `write_handoff` with
   `params: {summary: "<required>", session_id: "<optional>",
   state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   The substrate path `mpm_handoff` action `write` with the same
   `params` shape remains valid via `mpm call mpm_handoff --payload
   '{"action":"write","params":{...}}'` and on hosts running with
   `MPM_EXPOSE_ALL_TOOLS=1` on their MCP env block (which restores
   the full registered MCP surface). `summary` is the only required field.
   Mid-session acknowledgements (`ok`, `thanks`, `ty`, `ack`) are NOT
   session-closing; don't write a handoff on every chat ack.

   For intra-session volatile working state, the substrate tool is
   `mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`); reachable via `mpm call mpm_scratchpad` on hosts
   using the compact MCP surface. Wake is how future-me starts;
   handoff is how future-me receives the previous session.

6. **Track durable objectives as work items.** When a task is
   meaningfully multi-step, likely to span turns or sessions,
   dependent on later information, waiting on follow-up, requiring
   verification, containing important intermediate progress, or
   costly to reconstruct after interruption, open a `mpm_work`
   item reasonably early. Use `mpm_work` action `create` to open,
   `mpm_work` action `update` for material state / plan / status
   changes, `mpm_work` action `note` to retain meaningful progress
   / evidence / context, and `mpm_work` action `complete` with
   `params: {work_id}` to finish. Use `mpm_work` action `reopen`
   when completion is invalidated or new evidence demands more
   work, and `mpm_work` action `cancel` when an objective is
   intentionally abandoned. Do not create a work item for every
   small user request. Conceptual distinction: scratchpad is
   volatile intra-session working state (substrate tool
   `mpm_scratchpad`); work is a durable objective + progress
   lifecycle; handoff is session transition (see #5). Tools not
   exposed by your host's transport remain reachable via the CLI
   fallback (see #10).

7. **Session closure is not work completion.** Three events are
   distinct: `session ended` (the host process exits), `work completed`
   (`mpm_work` action `complete` invoked), and `work verified`
   (objective actually achieved, including any evidence accumulation
   via `mpm_work` action `resolve_contradiction`). Host session
   termination does NOT auto-complete a work item. The agent decides
   when work is done.

8. **Acquire and retain authoritative references.** When work
   depends on external facts or APIs that are version-sensitive,
   implementation-sensitive, likely to be reused, expensive to
   repeatedly rediscover, or important enough that primary-source
   accuracy matters, prefer authoritative / primary documentation
   where practical. Use whichever web / file / download capability
   the host already exposes (web search, browser, shell, curl,
   file tools — whatever is available and permitted) to obtain the
   source material, then register it via `mpm_references` action
   `add` (params carry the source material and its provenance).
   Preserve enough provenance / version / freshness information to
   judge later suitability — `mpm_references` action `read`
   surfaces freshness states (`current`, `stale`, `version-bound`,
   `historical`, `unknown`). Reuse an appropriate current reference
   rather than repeatedly rediscovering the same documentation.
   Common workflow classes that benefit: framework / library
   development, API integration, vendor SDK work, standards /
   specification work, version-sensitive infrastructure /
   configuration. The rule is general; apply it whenever
   primary-source accuracy and reusability matter.

9. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

10. **Recovery / fallback.** If the host's native MPM transport
    becomes unavailable mid-session (MCP server down, plugin crashed,
    `mpm__*` tools returning connection errors, etc.), fall back to
    the documented CLI path:
    `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
    Do not abandon persistence when the preferred integration breaks.
    `mpm call` writes to the same substrate with the same
    provenance attribution as the native transport.
<!-- END MPM MANAGED BLOCK -->
```

# Manual installation per host

Each persistent-file host installs the universal block by render-then-paste
(or by running the per-host installer, which does the same job). The
target file, host wrapper markers, and verification command for each host.

| Host | Target file | Marker convention | Verification command |
|---|---|---|---|
| Claude Code | `~/.claude/CLAUDE.md` (user-scope) or `<project>/CLAUDE.md` (project-scope via `--scope project`) | `<!-- BEGIN/END MPM-MANAGED SECTION:claude-code-instructions -->` | `claude --version && head -5 ~/.claude/CLAUDE.md && jq '.hooks.SessionStart' ~/.claude/settings.json` |
| OpenCode | `~/.config/opencode/AGENTS.md` (user) or `<project>/AGENTS.md` (project) | `<!-- BEGIN/END MPM-MANAGED SECTION:opencode-instructions -->` | open a project in OpenCode; agent's system prompt contains MPM wake content |
| Pi | `~/.pi/agent/AGENTS.md` (global) or `<project>/AGENTS.md` (per-project; Pi walks up from cwd) | `<!-- BEGIN/END MPM-MANAGED SECTION:pi-instructions -->` | start a Pi session against the containing project; agent's system prompt contains MPM wake content |
| Hermes | `<project>/.hermes.md` (or `HERMES.md`) | `<!-- BEGIN/END MPM MANAGED BLOCK:mpm-hermes -->` (installer anchor); canonical block markers nested inside | start a Hermes session in the project; compact MCP surface contains `mcp__mpm__mpm_context` action `read_wake_context` |

For each host, when editing the persistent file by hand:

- Keep the canonical managed block between the host wrapper markers.
- Do not edit the host wrapper markers themselves.
- Do not put MPM instructions in `SOUL.md` (Hermes) or any persona file —
  persona and protocol are kept separate.
- Do not duplicate this block. Each target file receives one and only one
  MPM-managed section.

# Tool-reference stability contract

The block above pins the action enum values that agents see. Changes to
the underlying schema require regeneration of this file. Verified against
the live registry at `internal/core/tools/registry_list.go` on the date
in this file's git log.

| Tool | Action values | Required params on write |
|---|---|---|
| `mpm_handoff` | `write`, `read`, `list`, `shred` | `write` requires `summary`. `state` enum: `clean` `crashed` `interrupted` `force_end`. No `note` parameter. |
| `mpm_scratchpad` | `flush`, `read`, `discard`, `promote` | — |
| `mpm_memory` | `save`, `query`, `shred`, `reinforce`, `weaken`, `snooze`, `set_weight`, `patch`, `promote`, `review`, `synthesize`, `challenge`, `commit_milestone` | `query` defaults `projection: "summary"`; full via `projection: "full"` |
| `mpm_decisions` | `record`, `supersede`, `invalidate` | — |
| `mpm_lessons` | `save`, `search`, `list`, `delete`, `restore`, `shred` | `save` defaults `type` to `"insight"` if absent; valid values: `"insight"`, `"warning"`, `"practice"`; lifecycle: `delete` is reversible soft delete (sets tombstone), `restore` clears the tombstone, `shred` is irreversible hard delete. All three accept canonical `id` (alias: `lesson_id`). |
| `mpm_topics` | `create`, `search`, `link` | — |
| `mpm_references` | `add`, `read`, `search`, `list` | freshness surfaced as `current` `stale` `version-bound` `historical` `unknown` |
| `mpm_context` | `read_wake_context`, `read_directives`, `proactive_recall_hint`, `query_global_rules`, `record_global_rule`, `promote_to_global`, `route` | `read_wake_context` accepts `params.projection: "compact"` |
| `mpm_skills` | `save`, `read`, `list`, `delete`, `promote_to_global`, `workshop` | `workshop` requires `intent`, `mode`, `task_context`, `workflow_description`, `failure_recovery`, `recent_actions`, `evidence` |
| `mpm_wakes` | `schedule`, `check`, `check_pending_event`, `list`, `digest`, `upsert_task`, `list_tasks`, `delete_task` | — |
| `mpm_work` | `create`, `list`, `show`, `update`, `complete`, `cancel`, `history`, `note`, `reopen`, `resolve_contradiction` | `complete` requires `work_id`; host session termination does NOT auto-trigger |
| `mpm_resolve` | (no `action`) | `uri: mpm://...` |
| `mpm_blob_read` | (no `action`) | `id`, `offset`, `max_bytes` |
| `mpm_blob_search` | (no `action`) | `id`, `query` |
| `mpm_memory` action=`challenge` | `memory_id`, `evidence` | Weaken a memory; creates a pending theory contesting it. Companion: `restore_challenge` action undoes. |
| `mpm_retrieval_diagnose` | (no `action`) | `query`, `limit`, `collection`, `scope`, `trace` |
| `mpm_system` | `gc_run`, `compact`, `health_check`, `migrate`, `query_audit_log`, `list_clusters`, `snooze_cluster`, `resolve_cluster`, `annotate_cluster`, `critic_findings` | — |

Companion standalones (transport-namespace may differ per host):

- `mpm_log_to_changelog` (`fact`, `commit_hash`, `tags`, plus optional `confirms_lesson_id`/`confirms_decision_id`/`confirms_theory_id` and `contradicts_lesson_id`/`contradicts_decision_id`/`contradicts_theory_id` for explicit epistemic assertions — see `docs/epistemic-confirmation.md`)
- `mpm_request_review` (`components`, `prompt`)

# Maintenance

Edit the canonical managed block above to change what every host
learns. Then run the render script to propagate. Drift detection
tests fail if the rendered output and the checked-in artifacts
diverge.

## Updating the managed instructions

1. Edit the canonical managed block above.
2. Update the four copy/paste examples at the top of this file to
   match the new canonical block (render script does this).
3. Update the behavioural protocol
   ([`mpm-agent-protocol.md`](./mpm-agent-protocol.md)) if the
   underlying contract changed.
4. Run `python3 scripts/render_managed_blocks.py` to regenerate
   the four host template snippets.
5. Run `python3 scripts/render_managed_blocks.py --check` for an
   idempotency assertion (no install required for verification).
6. Run `make test` (or the relevant Go test target) to cover the
   full parity + clean-install + cross-adapter test surface.

## Ownership rule

The render script's output is the **only** source of truth for the
text inside each host's managed section. Do not hand-edit the per-host
`templates/*.snippet` files or any installed persistent-file block —
those changes will be lost on the next render or install. If the
canonical block needs to change, edit it in this file.

Host-specific instructions (file paths, scope choices, hook wiring,
recovery notes) belong in the per-host adapter's header/footer
sections, **outside** the canonical managed block. Add or change
those directly in `scripts/render_managed_blocks.py` (the
`ADAPTERS` table carries per-host `header` and `footer` strings).

# Versioning

This file's top-of-file HTML comment carries the **managed-instruction
contract version**:

```
<!-- mpm_agent_integration_version: MAJOR.MINOR.PATCH -->
```

The version pins the contract that adapters render into each host's
managed section. It is independent of the MPM software release
version (e.g., `v0.1.0-alpha-final`). Parity byte-for-byte remains
the authoritative drift detector; the version marker is a coarse
signal for downstream adapters that want to pin to a known contract
shape.

## When to bump

- **patch** (1.0.0 → 1.0.1) — typo fixes, comment clarifications,
  formatting, prose tightening; no semantic change to the rendered
  contract. Example: fixing a wording glitch inside the canonical
  block.
- **minor** (1.0.0 → 1.1.0) — additive — new optional tool/param
  documented in the tool-reference stability contract table, new
  tool added, new host added, expanded examples; existing
  contracts still resolve under the old version.
- **major** (1.0.0 → 2.0.0) — breaking — managed-block marker
  convention changes, required field added to an existing tool,
  action rename, render-contract change that would force adapter
  re-verification. Host adapters MUST be re-verified against the
  new contract.

Bump this marker when the canonical managed block or the
tool-reference stability contract table changes in a way that
matters to consumers. The marker does **not** replace byte-for-byte
parity; the render script's `--check` mode remains the authoritative
drift detector.
