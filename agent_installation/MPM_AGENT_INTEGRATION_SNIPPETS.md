<!-- mpm_agent_integration_version: 1.0.0 -->

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
A correctly wired session will be able to refer to `mpm__mpm_handoff`.

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

1. **Wake on session start.** Before any substantive work, read the
   MPM wake context via `mpm__mpm_context` action
   `read_wake_context`. Skipping wake means arriving amnesic and
   forcing the user to re-explain context that is already on file.
   The wake payload carries orientation signals (mode, persona,
   recent topics, recent memories, recent milestones, last handoff,
   open work, overdue scheduled wakes, and a bounded
   `<available_skills>` catalogue). Decisions and lessons are
   reachable via `mpm_decisions` / `mpm_lessons`, not carried in
   wake. Use `params.projection: "compact"`
   for a small id+summary envelope — the full payload is the
   default.

2. **Persist during work, not only at the end.** Use `mpm__mpm_memory`
   action `save`, `mpm__mpm_decisions` action `record`,
   `mpm__mpm_lessons` action `save`, `mpm__mpm_topics`
   action `create`, and `mpm__mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Skill discovery before reinventing.** When the current task
   context suggests a previously learned procedure would apply,
   query `mpm__mpm_context` action `proactive_recall_hint`
   with `params: {conversation_text: "<recent task summary>"}` and
   read each surfaced skill via `mpm__mpm_skills` action
   `read`. The wake payload already includes a bounded
   `<available_skills>` catalogue — discovery adds a
   context-driven filter on top. Reactive catalog fallback via
   `mpm__mpm_skills` action `list` with
   `params: {scope: "all"}`. Don't auto-scan every turn.

4. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff via `mpm__mpm_handoff`
   action `write` with `params: {summary: "<required>",
   session_id: "<optional>", state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   `summary` is the only required field. Mid-session acknowledgements
   (`ok`, `thanks`, `ty`, `ack`) are NOT session-closing; don't
   write a handoff on every chat ack.

   For intra-session volatile working state, use
   `mpm__mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`). Wake is how future-me starts; handoff is how
   future-me receives the previous session.

5. **Session closure is not work completion.** When work spans
   sessions or requires verification, track it separately via
   `mpm_work`. Lifecycle: action `create` to open,
   `update`/`note` during, `complete` to finish. `session ended`
   is **not** the same event as `work completed` is **not** the
   same event as `work verified`. The agent decides when work is
   done — host session termination does NOT auto-complete a work
   item. Tools not exposed by your host's native transport remain
   reachable via the CLI fallback (see #7).

6. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

7. **Recovery / fallback.** If the host's native MPM transport
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
**Manual verification (no installer):** the `opencode-mpm` plugin
reads `AGENTS.md` at session start. Verify with `opencode run`
against a project containing the file.

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

1. **Wake on session start.** Before any substantive work, read the
   MPM wake context via `mpm_context` action
   `read_wake_context`. Skipping wake means arriving amnesic and
   forcing the user to re-explain context that is already on file.
   The wake payload carries orientation signals (mode, persona,
   recent topics, recent memories, recent milestones, last handoff,
   open work, overdue scheduled wakes, and a bounded
   `<available_skills>` catalogue). Decisions and lessons are
   reachable via `mpm_decisions` / `mpm_lessons`, not carried in
   wake. Use `params.projection: "compact"`
   for a small id+summary envelope — the full payload is the
   default.

2. **Persist during work, not only at the end.** Use `mpm_memory`
   action `save`, `mpm_decisions` action `record`,
   `mpm_lessons` action `save`, `mpm_topics`
   action `create`, and `mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Skill discovery before reinventing.** When the current task
   context suggests a previously learned procedure would apply,
   query `mpm_context` action `proactive_recall_hint`
   with `params: {conversation_text: "<recent task summary>"}` and
   read each surfaced skill via `mpm_skills` action
   `read`. The wake payload already includes a bounded
   `<available_skills>` catalogue — discovery adds a
   context-driven filter on top. Reactive catalog fallback via
   `mpm_skills` action `list` with
   `params: {scope: "all"}`. Don't auto-scan every turn.

4. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff via `mpm_handoff`
   action `write` with `params: {summary: "<required>",
   session_id: "<optional>", state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   `summary` is the only required field. Mid-session acknowledgements
   (`ok`, `thanks`, `ty`, `ack`) are NOT session-closing; don't
   write a handoff on every chat ack.

   For intra-session volatile working state, use
   `mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`). Wake is how future-me starts; handoff is how
   future-me receives the previous session.

5. **Session closure is not work completion.** When work spans
   sessions or requires verification, track it separately via
   `mpm_work`. Lifecycle: action `create` to open,
   `update`/`note` during, `complete` to finish. `session ended`
   is **not** the same event as `work completed` is **not** the
   same event as `work verified`. The agent decides when work is
   done — host session termination does NOT auto-complete a work
   item. Tools not exposed by your host's native transport remain
   reachable via the CLI fallback (see #7).

6. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

7. **Recovery / fallback.** If the host's native MPM transport
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
(default). The agent should be able to refer to `mpm_handoff`.

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

1. **Wake on session start.** Before any substantive work, read the
   MPM wake context via `mpm_context` action
   `read_wake_context`. Skipping wake means arriving amnesic and
   forcing the user to re-explain context that is already on file.
   The wake payload carries orientation signals (mode, persona,
   recent topics, recent memories, recent milestones, last handoff,
   open work, overdue scheduled wakes, and a bounded
   `<available_skills>` catalogue). Decisions and lessons are
   reachable via `mpm_decisions` / `mpm_lessons`, not carried in
   wake. Use `params.projection: "compact"`
   for a small id+summary envelope — the full payload is the
   default.

2. **Persist during work, not only at the end.** Use `mpm_memory`
   action `save`, `mpm_decisions` action `record`,
   `mpm_lessons` action `save`, `mpm_topics`
   action `create`, and `mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Skill discovery before reinventing.** When the current task
   context suggests a previously learned procedure would apply,
   query `mpm_context` action `proactive_recall_hint`
   with `params: {conversation_text: "<recent task summary>"}` and
   read each surfaced skill via `mpm_skills` action
   `read`. The wake payload already includes a bounded
   `<available_skills>` catalogue — discovery adds a
   context-driven filter on top. Reactive catalog fallback via
   `mpm_skills` action `list` with
   `params: {scope: "all"}`. Don't auto-scan every turn.

4. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff via `mpm_handoff`
   action `write` with `params: {summary: "<required>",
   session_id: "<optional>", state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   `summary` is the only required field. Mid-session acknowledgements
   (`ok`, `thanks`, `ty`, `ack`) are NOT session-closing; don't
   write a handoff on every chat ack.

   For intra-session volatile working state, use
   `mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`). Wake is how future-me starts; handoff is how
   future-me receives the previous session.

5. **Session closure is not work completion.** When work spans
   sessions or requires verification, track it separately via
   `mpm_work`. Lifecycle: action `create` to open,
   `update`/`note` during, `complete` to finish. `session ended`
   is **not** the same event as `work completed` is **not** the
   same event as `work verified`. The agent decides when work is
   done — host session termination does NOT auto-complete a work
   item. Tools not exposed by your host's native transport remain
   reachable via the CLI fallback (see #7).

6. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

7. **Recovery / fallback.** If the host's native MPM transport
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
convention. The installer uses a leading `<!-- BEGIN MPM MANAGED BLOCK:hermes-mpm -->`
comment block as a stable re-install anchor; the canonical
managed-block markers from this file (the universal
`<!-- BEGIN MPM MANAGED BLOCK -->` pair) live inside it.
**Manual verification (no installer):** start a Hermes session.
The persona stays in `~/.hermes/SOUL.md`; the MPM behavioural
contract lands via `.hermes.md`. The agent should be able to
refer to `mcp__mpm__mpm_handoff`.

```markdown
<!-- BEGIN MPM MANAGED BLOCK:hermes-mpm -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- The full behavioural protocol is canonical at ~/.mpm/agent_installation/mpm-agent-protocol.md -->

## MPM behavioural contract

You are operating with the MPM (Memory Persistence Module) substrate
on this machine. The following are the non-negotiable MPM behavioural
invariants that turn that capability into reliable behaviour. Edit the
canonical protocol, not this block, for behavioural changes.

1. **Wake on session start.** Before any substantive work, read the
   MPM wake context via `mcp__mpm__mpm_context` action
   `read_wake_context`. Skipping wake means arriving amnesic and
   forcing the user to re-explain context that is already on file.
   The wake payload carries orientation signals (mode, persona,
   recent topics, recent memories, recent milestones, last handoff,
   open work, overdue scheduled wakes, and a bounded
   `<available_skills>` catalogue). Decisions and lessons are
   reachable via `mpm_decisions` / `mpm_lessons`, not carried in
   wake. Use `params.projection: "compact"`
   for a small id+summary envelope — the full payload is the
   default.

2. **Persist during work, not only at the end.** Use `mcp__mpm__mpm_memory`
   action `save`, `mcp__mpm__mpm_decisions` action `record`,
   `mcp__mpm__mpm_lessons` action `save`, `mcp__mpm__mpm_topics`
   action `create`, and `mcp__mpm__mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Skill discovery before reinventing.** When the current task
   context suggests a previously learned procedure would apply,
   query `mcp__mpm__mpm_context` action `proactive_recall_hint`
   with `params: {conversation_text: "<recent task summary>"}` and
   read each surfaced skill via `mcp__mpm__mpm_skills` action
   `read`. The wake payload already includes a bounded
   `<available_skills>` catalogue — discovery adds a
   context-driven filter on top. Reactive catalog fallback via
   `mcp__mpm__mpm_skills` action `list` with
   `params: {scope: "all"}`. Don't auto-scan every turn.

4. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff via `mcp__mpm__mpm_handoff`
   action `write` with `params: {summary: "<required>",
   session_id: "<optional>", state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   `summary` is the only required field. Mid-session acknowledgements
   (`ok`, `thanks`, `ty`, `ack`) are NOT session-closing; don't
   write a handoff on every chat ack.

   For intra-session volatile working state, use
   `mcp__mpm__mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`). Wake is how future-me starts; handoff is how
   future-me receives the previous session.

5. **Session closure is not work completion.** When work spans
   sessions or requires verification, track it separately via
   `mpm_work`. Lifecycle: action `create` to open,
   `update`/`note` during, `complete` to finish. `session ended`
   is **not** the same event as `work completed` is **not** the
   same event as `work verified`. The agent decides when work is
   done — host session termination does NOT auto-complete a work
   item. Tools not exposed by your host's native transport remain
   reachable via the CLI fallback (see #7).

6. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

7. **Recovery / fallback.** If the host's native MPM transport
   becomes unavailable mid-session (MCP server down, plugin crashed,
   `mpm__*` tools returning connection errors, etc.), fall back to
   the documented CLI path:
   `mpm call <tool> --payload '{"action":"<op>","params":{...}}'`
   Do not abandon persistence when the preferred integration breaks.
   `mpm call` writes to the same substrate with the same
   provenance attribution as the native transport.
<!-- END MPM MANAGED BLOCK -->
<!-- END MPM MANAGED BLOCK:hermes-mpm -->
```

## OpenClaw

OpenClaw uses runtime injection rather than a persistent MPM
instruction block.

There is no MPM-managed Markdown file you write to. OpenClaw's
`openclaw-mpm-memory` and `openclaw-mpm-auto-mode-persona`
plugins handle wake-context injection at session start — the
agent receives the same seven invariants automatically through
the system prompt, never through a file you maintain.

**To install OpenClaw MPM integration:**

```
git clone <repo> ~/.openclaw/workspace/projects/mpm  # or symlink
cd ~/.openclaw/workspace/projects/mpm/openclaw-mpm-memory
./install.sh
```

For installation details and validation evidence, see
[`openclaw-mpm-memory/README.md`](./openclaw-mpm-memory/README.md)
and the OpenClaw row in `INSTALL.md`.

If you are not using OpenClaw, you can stop reading. The rest of
this file documents the persistent-file hosts and the universal
canonical managed block.

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

1. **Wake on session start.** Before any substantive work, read the
   MPM wake context via `mpm_context` action
   `read_wake_context`. Skipping wake means arriving amnesic and
   forcing the user to re-explain context that is already on file.
   The wake payload carries orientation signals (mode, persona,
   recent topics, recent memories, recent milestones, last handoff,
   open work, overdue scheduled wakes, and a bounded
   `<available_skills>` catalogue). Decisions and lessons are
   reachable via `mpm_decisions` / `mpm_lessons`, not carried in
   wake. Use `params.projection: "compact"`
   for a small id+summary envelope — the full payload is the
   default.

2. **Persist during work, not only at the end.** Use `mpm_memory`
   action `save`, `mpm_decisions` action `record`,
   `mpm_lessons` action `save`, `mpm_topics`
   action `create`, and `mpm_references` action `add`
   for any durable knowledge a future session would otherwise have
   to rediscover. Heuristic: if losing this on session-end would
   force you to rediscover it next time, persist now.

3. **Skill discovery before reinventing.** When the current task
   context suggests a previously learned procedure would apply,
   query `mpm_context` action `proactive_recall_hint`
   with `params: {conversation_text: "<recent task summary>"}` and
   read each surfaced skill via `mpm_skills` action
   `read`. The wake payload already includes a bounded
   `<available_skills>` catalogue — discovery adds a
   context-driven filter on top. Reactive catalog fallback via
   `mpm_skills` action `list` with
   `params: {scope: "all"}`. Don't auto-scan every turn.

4. **Handoff before genuine session closure.** Before any turn that
   closes the session, write a handoff via `mpm_handoff`
   action `write` with `params: {summary: "<required>",
   session_id: "<optional>", state: "clean"|"crashed"|"interrupted"|"force_end",
   commitments: ["<optional>"], open_questions: ["<optional>"]}`.
   `summary` is the only required field. Mid-session acknowledgements
   (`ok`, `thanks`, `ty`, `ack`) are NOT session-closing; don't
   write a handoff on every chat ack.

   For intra-session volatile working state, use
   `mpm_scratchpad` actions `flush`, `read`,
   `discard`, or `promote` (params: `{session_id, thesis,
   supporting}`). Wake is how future-me starts; handoff is how
   future-me receives the previous session.

5. **Session closure is not work completion.** When work spans
   sessions or requires verification, track it separately via
   `mpm_work`. Lifecycle: action `create` to open,
   `update`/`note` during, `complete` to finish. `session ended`
   is **not** the same event as `work completed` is **not** the
   same event as `work verified`. The agent decides when work is
   done — host session termination does NOT auto-complete a work
   item. Tools not exposed by your host's native transport remain
   reachable via the CLI fallback (see #7).

6. **MPM is the source of truth for cross-session continuity.**
   Anything that should survive across sessions goes to MPM, not
   just transient conversation context.

7. **Recovery / fallback.** If the host's native MPM transport
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
| Claude Code | `~/.claude/CLAUDE.md` (user-scope) or `<project>/CLAUDE.md` (project-scope via `--scope project`) | `<!-- BEGIN/END MPM-MANAGED SECTION:claude-code-instructions -->` | `claude --version && head -5 ~/.claude/CLAUDE.md` |
| OpenCode | `~/.config/opencode/AGENTS.md` (user) or `<project>/AGENTS.md` (project) | `<!-- BEGIN/END MPM-MANAGED SECTION:opencode-instructions -->` | open a project in OpenCode; agent can refer to `mpm_handoff` |
| Pi | `~/.pi/agent/AGENTS.md` (global) or `<project>/AGENTS.md` (per-project; Pi walks up from cwd) | `<!-- BEGIN/END MPM-MANAGED SECTION:pi-instructions -->` | start a Pi session against the containing project |
| Hermes | `<project>/.hermes.md` (or `HERMES.md`) | `<!-- BEGIN/END MPM MANAGED BLOCK:hermes-mpm -->` (installer anchor); canonical block markers nested inside | start a Hermes session in the project; agent can refer to `mcp__mpm__mpm_handoff` |

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
| `mpm_lessons` | `save`, `search`, `list` | `save` defaults `type` to `"insight"` if absent; valid values: `"insight"`, `"warning"`, `"practice"` |
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

- `log_to_changelog` (`fact`, `commit_hash`, `tags`)
- `request_review` (`components`, `prompt`)

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
