# mpm-opencode

OpenCode plugin that wires MPM's cognitive substrate in as **17 typed
tools** (a curated subset of the full 22-tool MPM registry).

## Parity with Claude Code Integration

This plugin achieves functional parity with the [Claude Code integration](agent_installation/mpm-claude-code/CLAUDE_CODE_INTEGRATION.md) through OpenCode's hook system:

| Claude Code Mechanism | OpenCode Equivalent |
|---|---|
| `SessionStart` hook → `~/.claude/hooks/mpm-session-start` | `experimental.chat.system.transform` hook → push into `output.system` |
| `MPM_PROVENANCE_*` env vars from `CLAUDE_*` runtime vars | `shell.env` hook + per-call provenance in `callMpmWithProvenance` |
| Session End ≠ Claimed Complete (semantic contract) | Same contract applies — use `mpm_work complete` explicitly |

The OpenCode plugin provides **automatic wake context injection** at session start (via `experimental.chat.system.transform`) and **provenance tracking** for all MPM calls (via `shell.env` hook and per-call env vars). The lifecycle surface is host-specific (ClaudeCode's `SessionStart` vs. OpenCode's `experimental.chat.system.transform`); the *behavior* — wake context delivered before the first model turn — is shared.

## Coverage

**17 OpenCode tools registered**, in two layers:

| Layer | Count | Source |
|---|---|---|
| Unified Domain Tools ("Fat RPC") | 14 | `src/index.ts` (hand-written) |
| Standalone tools | 3 | `src/index.ts` (hand-written) |

### 14 Domain Tools

The 14 Domain Tools cover the **core cognitive surface** exposed by this
adapter. The current MPM registry exposes **22 tools total** (21
Registry entries + the `mpm_help` discovery closure registered via
`cmd/mpm-mcp`), so this adapter is a hand-curated 17-tool subset.
Tools in the full registry not registered here (`mpm_work`,
`mpm_resolve`, `mpm_blob_read`, `mpm_blob_search`) remain reachable
via the canonical `mpm call <tool> --payload '<json>'` CLI fallback.
Each registered Domain Tool dispatches on an `action` enum with
free-form `params`:

> The default initial MCP surface that hosts receive at session start
> is the compact 3-tool surface (`mpm_memory`, `mpm_context`,
> `mpm_help`); `MPM_EXPOSE_ALL_TOOLS=1` in the MCP env block restores
> the full 22-tool surface. The OpenCode plugin's typed tools
> (`mpm__mpm_handoff`, `mpm__mpm_scratchpad`, etc.) are exposed
> directly regardless of the MCP filter because the OpenCode plugin
> registers its own typed tools via Pi's extension API, not via
> `mpm-mcp`'s `tools/list`.

| Domain | Actions |
|---|---|
| `mpm_memory` | save, query, shred, reinforce, weaken, snooze, set_weight, patch, promote, review, synthesize, challenge, commit_milestone |
| `mpm_handoff` | write, read, list, shred | (also reachable on the default compact MCP surface as `mpm_context action=write_handoff` / `read_handoff`) |
| `mpm_scratchpad` | flush, read, discard, promote |
| `mpm_wakes` | schedule, check, check_pending_event, list, digest, upsert_task, list_tasks, delete_task |
| `mpm_theories` | propose, resolve |
| `mpm_lessons` | save, search, list |
| `mpm_decisions` | record |
| `mpm_topics` | create, search, link |
| `mpm_references` | add, search, list |
| `mpm_evidence` | add, list |
| `mpm_confidence` | show, recompute, explain, history, changes, trend, quality |
| `mpm_context` | read_wake_context, read_directives, proactive_recall_hint, query_global_rules, record_global_rule, promote_to_global, route |
| `mpm_skills` | save, read, list, delete, promote_to_global |
| `mpm_system` | gc_run, compact, health_check, migrate, query_audit_log, list_clusters, snooze_cluster, resolve_cluster, annotate_cluster |

All 13 share the same `args` shape:

```ts
{
  action: string,            // required — which operation to run
  params?: Record<string, any> // optional — free-form, schema lives in mpm
}
```

### 3 Standalone Tools

These have their own narrower schemas (action dispatch is the wrong shape — they ARE the action):

| Tool | Purpose |
|---|---|
| `mpm_retrieval_diagnose` | Per-node diagnostic breakdown of a query (Base FTS Match, Reuse Count, Last Retrieved, Success Count). Layered on top of `mpm_memory/query` without altering ranking. |
| `mpm_log_to_changelog` | Self-report agent work tied to a git commit SHA. |
| `mpm_request_review` | Concurrent multi-component review. |

## Architecture

This is a **lightweight adapter**, not a port of the legacy 77-tool plugin.

```
┌──────────────┐   {action, params}     ┌──────────────────┐
│  OpenCode    │ ─────────────────────► │  mpm call <tool> │
│  tool({...}) │ ◄───────────────────── │  --payload <json>│
└──────────────┘   JSON envelope         └──────────────────┘
```

Each tool is a thin transport shim that stringifies its payload and spawns `mpm call <tool> --payload '<json>'` as a subprocess. The mpm backend owns:
- Schema validation per action
- SQLite persistence
- Cross-domain invariants (e.g., what happens when a `mpm_lessons/save` cites a `mpm_memory` id)
- The FTS5 + semantic retrieval pipeline
- The scheduled wake / cognitive substrate infrastructure

The plugin deliberately holds **no domain logic** — no LLM calls, no caching, no state. It's an honest thin shim.

## Boot-time health check

On plugin initialization, the plugin pings `mpm call mpm_system action:health_check` with a **2s timeout**. If the ping fails or the response reports degraded scheduler/DB state, the plugin emits a loud warning to the OpenCode boot log:

```
⚠ mpm-opencode BOOT WARNING: mpm health check failed
  reason: spawn failed: spawn mpm ENOENT
  check that mpm exists and is healthy.
  tools will fail-open on each call until mpm is reachable.
```

This is the same defensive boot hook we added to `@openclaw/mpm-memory` (see `agent_installation/mpm-memory-openclaw/index.js`). The check fires loud on every OpenCode restart so the operator sees it. Per-call failures still fail-open (each tool returns a soft error envelope) so a transient mpm blip doesn't kill the turn.

Triggers DEGRADED on:
- spawn / timeout / parse failure
- `payload.ok === false`
- `payload.scheduler.state !== "ok"`
- `payload.scheduler.last_status === "error"`

## Hooks: Wake Context & Provenance

### `experimental.chat.system.transform` — Wake Context Injection

This hook runs as part of OpenCode's chat-prompt preparation, before the first model turn. It calls `mpm_context.read_wake_context` with `format: "system-prompt"` and pushes the returned context into the `output.system` array that OpenCode ships as the model-facing system prompt.

- Only injects once per session (cached by `sessionID`)
- This is OpenCode's native lifecycle surface — it is **not** equivalent to ClaudeCode's `SessionStart` hook or `mpm-wake.sh`. The behaviour is the same (wake before first turn); the implementation is host-specific.
- The wake context includes: active mode, persona, pending work, recent memories, recent milestones, last handoff, overdue wakes, and the bounded `<available_skills>` catalogue.

### `shell.env` — Provenance Environment Variables

This hook sets `MPM_PROVENANCE_*` environment variables for shell commands, ensuring that any `mpm call` invoked directly from the shell (e.g., user typing commands) has proper provenance tracking.

Variables set:
| Variable | Value |
|---|---|
| `MPM_PROVENANCE_FRAMEWORK` | `opencode` |
| `MPM_PROVENANCE_FRAMEWORK_SESSION_ID` | OpenCode `ctx.sessionID` |

Additionally, **every programmatic tool call** (via the 17 registered typed tools) includes provenance env vars via `callMpmWithProvenance`:
| Variable | Value |
|---|---|
| `MPM_PROVENANCE_FRAMEWORK` | `opencode` |
| `MPM_PROVENANCE_MODEL` | Current model ID (e.g., `deepseek-v4-flash-free`) |
| `MPM_PROVENANCE_INVOCATION_ID` | Unique per-call UUID (`inv_...`) |
| `MPM_PROVENANCE_FRAMEWORK_SESSION_ID` | OpenCode `ctx.sessionID` |

Stage 2C.2 (2026-09-20): OpenCode's `ctx.sessionID` is the host's
continuing conversation/session identity, NOT an invocation-lineage
ancestor. It maps to the canonical framework-session slot so the
substrate records it as framework-owned session identity on
`tool_invocations.framework_session_id`,
`artifact_provenance.framework_session_id`, and
`session_handoffs.framework_session_id`. The
`MPM_PROVENANCE_PARENT_INVOCATION_ID` slot stays empty unless OpenCode
separately exposes a true causal parent invocation identifier.

These match the [Claude Code integration's provenance variables](agent_installation/mpm-claude-code/CLAUDE_CODE_INTEGRATION.md#12-provenance-environment-variables).

### Semantic Contract: Session End ≠ Claimed Complete

> **An OpenCode session ending does not automatically emit `claimed_complete`.**
> Only emit it when the agent explicitly closes work via `mpm call mpm_work` with `{"action":"complete","params":{"work_id":"..."}}`.

This is the same semantic contract as the [Claude Code integration](agent_installation/mpm-claude-code/CLAUDE_CODE_INTEGRATION.md#33-semantic-contract-session-end--claimed-complete). A session ending is a lifecycle event, not an epistemic one — the agent may have been interrupted, hit a timeout, or simply run out of context. Emitting `claimed_complete` on session end would silently undo the verification model.

The correct pattern:
```
OpenCode starts
    ↓
MPM wake context injected into session (via system.transform hook)
    ↓
OpenCode works; may record evidence via mpm call tools
    ↓
OpenCode explicitly calls: mpm call mpm_work --payload '{"action":"complete","params":{"work_id":"xyz"}}'
    ↓
MPM emits WorkEventTypeClaimedComplete
    ↓
Evidence accumulates in background
    ↓
MPM DeriveWorkVerification derives verification status
```

## What replaces

The legacy plugin (`agent-plugins/mpm-opencode-plugin` in pCloud) generated one tool per (action, artifact-type) pair — 18 tool definitions spanning memory / lessons / topics / references / wake / decisions / etc. It was bound to the **old 77-tool schema** that pre-dated the 13-aggregator collapse (2026-08-11). It is **dead code** and should be removed.

The lightweight adapter:
- 17 typed tools exposed by this plugin (a curated subset of the full 22-tool MPM registry; the legacy plugin named 18 individually: `query_long_term_memory`, `save_to_memory`, `challenge_memory`, etc. — note: `challenge_memory` is gone from the registry, use `mpm_memory` action=`challenge` instead)
- One Zod schema shape for the 14 Domain Tools (free-form `params` validated by mpm backend)
- No prompt bloat — the description strings are short, and the heavy `params` documentation lives in the mpm backend where it can be evolved without disrupting the plugin
- No domain logic — no LLM calls, no caching, no local state

## Install

```bash
# 1. install the plugin into opencode's plugin dir
ln -s /path/to/agent_installation/mpm-opencode ~/.config/opencode/plugin/mpm-opencode

# 2. make sure ~/.mpm/bin/mpm is on PATH (or set MPM_BINARY)
export PATH=$HOME/.mpm/bin:$PATH

# 3. install the AGENTS.md behavioral section (user scope):
python3 ~/.mpm/agent_installation/mpm-opencode/scripts/install_agents_instructions.py \
    --scope user \
    --target ~/.config/opencode/AGENTS.md \
    --snippet ~/.mpm/agent_installation/mpm-opencode/templates/AGENTS.md.snippet

# 4. (re)start opencode — the plugin will register, emit the boot health check,
#    and fire `experimental.chat.system.transform` on the first session turn.
```

There is no shell-based `install.sh` for this adapter — the installer is
`scripts/install_agents_instructions.py`. Manual `ln -s` is required for the
plugin symlink because OpenCode's plugin loader does not auto-link from
arbitrary paths; it reads `~/.config/opencode/plugin/*` directly.

The plugin reads `MPM_BINARY` from env (explicit per-call override)
and otherwise resolves the MPM binary deterministically via
`src/resolve-mpm-binary.ts` (canonical `$HOME/.mpm/bin/mpm` →
`$HOME/.local/bin/mpm` → last-resort `command -v mpm`). The resolved
absolute path is used by every subprocess invocation — boot health
check, tool calls, wake-context fetch — so the integration does not
depend on `$PATH` being sane in the OpenCode runtime env. The
`MPM_WORKSPACE` env defaults to `$HOME/.mpm` (computed by
`src/workspace.ts`).

The behavioral section in step 3 is **required** for the agent to
honor the MPM persistent-state, skill-discovery, and handoff
invariants. The plugin wires wake-context injection automatically via
`experimental.chat.system.transform`; the AGENTS.md managed section
delivers the rest of the protocol.

## Verification

```bash
# Plugin loaded? List its typed tools in an OpenCode session:
#   :tools — expect 17 mpm__ prefixed tool registrations

# AGENTS.md has exactly one managed block:
grep -c '<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->' \
  ~/.config/opencode/AGENTS.md
# expect: 1

# Tool count — 17 typed tools registered by this adapter:
opencode # then :tools — count entries prefixed with `mpm__`

# End-to-end via the plugin's exposed tool (in OpenCode):
#   "use mpm__mpm_memory action=save to remember that mpm-opencode integration smoke test passed"
mpm recall --semantic "mpm-opencode integration smoke test"
```

## Uninstall

```bash
# 1. Remove the plugin symlink:
rm ~/.config/opencode/plugin/mpm-opencode

# 2. Strip the managed section from AGENTS.md (user scope):
python3 ~/.mpm/agent_installation/mpm-opencode/scripts/install_agents_instructions.py \
    --scope user --target ~/.config/opencode/AGENTS.md --uninstall

# 3. (For project scope, repeat with --scope project and the project --target.)
```

The uninstall path strips both the current `opencode-instructions`
managed block and any legacy `MPM-MANAGED SECTION` block (without
`:opencode-instructions` suffix). If the file would be empty
afterwards, the file is unlinked. A backup is written before any
mutation.

## Build

```bash
npm install
npm run build
```

The build emits `dist/index.js` (the runtime entry) and `dist/index.d.ts` (the type declarations). The plugin loads from `dist/index.js` per the `main` field in `package.json`.

## Manual smoke test

```bash
PATH=$HOME/.mpm/bin:$PATH node --input-type=module -e '
const { default: plugin } = await import("./dist/index.js");
const hooks = await plugin.server({
  client: {}, project: { id: "test", worktree: "/tmp" },
  directory: "/tmp", worktree: "/tmp",
  experimental_workspace: { register: () => {} },
  serverUrl: new URL("http://localhost:8080"),
  $: () => ({ text: () => "" })
});
console.log("tools registered:", Object.keys(hooks.tool).length);  // → 17
'
```

Expected: `tools registered: 17` (silent on a healthy machine; emits BOOT WARNING on a broken mpm install).
