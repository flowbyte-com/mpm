# opencode-mpm

OpenCode plugin that wires MPM's cognitive substrate in as 16 typed tools.

## Coverage

**16 OpenCode tools registered**, in two layers:

| Layer | Count | Source |
|---|---|---|
| Unified Domain Tools ("Fat RPC") | 13 | `src/index.ts` (hand-written) |
| Standalone tools | 3 | `src/index.ts` (hand-written) |

### 13 Domain Tools

The 13 Domain Tools cover the full cognitive surface from mpm's registry, each dispatching on an `action` enum with free-form `params`:

| Domain | Actions |
|---|---|
| `mpm_memory` | save, query, shred, reinforce, weaken, snooze, set_weight, patch, promote, review, synthesize, challenge, commit_milestone |
| `mpm_session` | end, handoff, list_handoffs, flush, read, discard, promote_scratchpad |
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
| `explain_retrieval` | Per-node diagnostic breakdown of a query (Base FTS Match, Reuse Count, Last Retrieved, Success Count). Layered on top of `mpm_memory/query` without altering ranking. |
| `log_to_changelog` | Self-report agent work tied to a git commit SHA. |
| `request_review` | Concurrent multi-component review. |

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
⚠ opencode-mpm BOOT WARNING: mpm health check failed
  reason: spawn failed: spawn mpm ENOENT
  check that mpm exists and is healthy.
  tools will fail-open on each call until mpm is reachable.
```

This is the same defensive boot hook we added to `@openclaw/mpm-memory` (see `agent_plugins/openclaw-mpm-memory/index.js`). The check fires loud on every OpenCode restart so the operator sees it. Per-call failures still fail-open (each tool returns a soft error envelope) so a transient mpm blip doesn't kill the turn.

Triggers DEGRADED on:
- spawn / timeout / parse failure
- `payload.ok === false`
- `payload.scheduler.state !== "ok"`
- `payload.scheduler.last_status === "error"`

## What replaces

The legacy plugin (`agent-plugins/opencode-mpm-plugin` in pCloud) generated one tool per (action, artifact-type) pair — 18 tool definitions spanning memory / lessons / topics / references / wake / decisions / etc. It was bound to the **old 77-tool schema** that pre-dated the 13-aggregator collapse (2026-08-11). It is **dead code** and should be removed.

The lightweight adapter:
- 16 tools instead of 18 named-individually (`mpm_memory` replaces `query_long_term_memory`, `save_to_memory`, `challenge_memory`, etc.)
- One Zod schema shape for the 13 Domain Tools (free-form `params` validated by mpm backend)
- No prompt bloat — the description strings are short, and the heavy `params` documentation lives in the mpm backend where it can be evolved without disrupting the plugin
- No domain logic — no LLM calls, no caching, no local state

## Install

```bash
# 1. install the plugin into opencode's plugin dir
ln -s /path/to/agent_plugins/opencode-mpm ~/.config/opencode/plugin/opencode-mpm

# 2. make sure ~/.mpm/bin/mpm is on PATH (or set MPM_BINARY)
export PATH=$HOME/.mpm/bin:$PATH

# 3. (re)start opencode — the plugin will register and emit the boot health check
```

The plugin reads `MPM_BINARY` from env (defaults to `mpm`) and `MPM_WORKSPACE` from env (defaults to `<cwd>`).

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
console.log("tools registered:", Object.keys(hooks.tool).length);  // → 16
'
```

Expected: `tools registered: 16` (silent on a healthy machine; emits BOOT WARNING on a broken mpm install).
