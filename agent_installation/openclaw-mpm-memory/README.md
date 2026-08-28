# @openclaw/mpm-memory

MPM-backed memory slot for OpenClaw. Routes `memory_search` and `memory_get`
through MPM's FTS5 + reinforcement-weighted recall, and injects MPM wake context
into every OpenClaw session via OpenClaw's native hook API.

## What this plugin does

| Surface | Mechanism | Status |
|---------|-----------|--------|
| `memory_search` / `memory_get` | Subprocess → `mpm call mpm_memory` | ✅ Works |
| Wake context injection | `session_start` → `agent_turn_prepare` hooks | ✅ Implemented |
| Provenance env vars | `resolve_exec_env` hook | ✅ Implemented |
| `session_end` → work completion | Not implemented | ✅ Correct — intentional |
| Heartbeat context | `heartbeat_prompt_contribution` hook | ✅ Implemented |
| Memory capability slot | `api.registerMemoryCapability` | ✅ Works |
| Compaction flush | Neutered at sink | ✅ Correct — intentional |

## Install

```bash
# 1. Link the plugin (dev / alpha-MV; no published tarball yet)
openclaw plugins install ./openclaw-mpm-memory --link

# 2. Install/verify MPM on PATH (idempotent — skips if already installed)
./install.sh

# 3. Enable the plugin entry
openclaw config set plugins.entries.openclaw-mpm-memory.enabled true

# 4. Switch the slot to this plugin
openclaw config set plugins.slots.memory openclaw-mpm-memory

# 5. (Optional, recommended) Silence memory-core
openclaw config set plugins.entries.memory-core.enabled false

# 6. Restart the gateway
openclaw gateway restart

# 7. Verify — should return ok:true with zero findings
openclaw doctor --lint --only core/doctor/memory-search --json
```

## Wake Context Injection

When a session starts, the plugin fetches MPM's wake context via
`mpm call mpm_context --format=system-prompt` and injects it into the agent
prompt via the `agent_turn_prepare` hook (pre-pended as `prependContext`).

The flow:

```
session_start fires
    │
    ▼
async fetch of MPM wake context (non-blocking)
    │
    ▼
agent_turn_prepare fires (next agent turn)
    │
    ├── await cached wake context promise
    │
    ▼
prependContext ← wake context injected into agent prompt
```

For heartbeat turns, `heartbeat_prompt_contribution` injects a brief MPM status
note (first 300 chars of wake context) without duplicating the full context.

## Provenance

`resolve_exec_env` contributes these env vars to every `exec` tool call:

| Env var | Value | Source |
|---------|-------|--------|
| `MPM_PROVENANCE_FRAMEWORK` | `"openclaw"` | Fixed |
| `MPM_PROVENANCE_SESSION_KEY` | OpenClaw `sessionKey` | Hook context |

`MPM_PROVENANCE_MODEL` and `MPM_PROVENANCE_INVOCATION_ID` are intentionally
**not** set — OpenClaw's hook context does not expose model name or
invocation ID. These fields are left unset rather than fabricated.

## `session_end` — No Work Completion

**Session end does not emit work completion.** This is intentional and correct.

The semantic contract:

```
Session ended       ≠ work completed
Work completed     ≠ work verified
```

Only an explicit agent action (`mpm call mpm_work --action complete`) emits
`WorkEventTypeClaimedComplete`. `session_end` is a lifecycle observation,
not an epistemic one. The agent may have been interrupted, hit a timeout,
or simply run out of context.

## Compaction Flush (Neutered at the Sink)

This plugin does **not** ingest OpenClaw's compaction flush output. The
`flushPlanResolver` returns a plan whose `relativePath` points at a
throwaway file (`local_flush_trash.md`) under the calling session's
`workspaceDir`. OpenClaw writes the file as part of its flush lifecycle —
satisfying its internal contract — but the file's content is discarded
unread.

**Why neuter the integration.** MPM already has two higher-fidelity
epistemic sources than OpenClaw's auto-generated transcript summary:

- **Scratchpad** — agent-curated Working Context, deliberate, ephemeral.
- **Memories** (`mpm_memory`) — explicit facts with tags, weight, and
  reinforcement, retrievable via FTS5 + hybrid scoring.

Capturing OpenClaw's lossy auto-summary would add a third memory surface
with lower fidelity. The integration boundary is deleted at the sink, not
the source. See decision `40544f5a04a2aac7` (2026-08-13) for full
rationale; this implements the *"Truth once. Views everywhere."* doctrine.

## Configuration

```json5
plugins: {
  entries: {
    "openclaw-mpm-memory": {
      enabled: true,
      config: {
        mpmBin: "mpm",            // path to mpm; resolves from PATH by default
        timeoutMs: 5000,           // subprocess timeout; default 5000
        scope: "all",              // "all" | "local" | "shared"; default "all"
        limitDefault: 6            // default memory_search limit; 1-50
      }
    }
  },
  slots: { memory: "openclaw-mpm-memory" }
}
```

## Failure Modes (All Fail-Open)

| Condition | Behaviour |
|---|---|
| `mpm` not on PATH | `memory_search` returns `{disabled:true, error:"...not on PATH..."}` |
| `mpm` exits non-zero | Tool result includes last 500 chars of stderr/stdout as `error` |
| Subprocess timeout | `error: "mpm ... timed out after Nms"` |
| MPM returns zero hits | `results: []`, `total: 0` — normal |
| `memory_get` on non-virtual path | `{notFound:true, supportedPrefix:"mpm://memory/"}` |
| `memory_get` for unknown id | `{notFound:true}` |
| Wake context fetch fails | Agent turn proceeds without wake context (graceful degradation) |

No error throws into the agent turn — every surface has a fail-open path.

## OpenClaw Limitations (Genuine — Not Fixable by this Plugin)

These are framework-level limitations of OpenClaw's plugin API, not bugs in
this plugin:

| Limitation | Impact |
|---|---|
| No `SessionStart`/`SessionEnd` equivalent hooks | ✅ **Resolved** — OpenClaw DOES have `session_start`/`session_end` hooks (discovered 2026-08-25). This plugin uses them. |
| OpenClaw does not expose `model` in hook context | `MPM_PROVENANCE_MODEL` not set |
| OpenClaw does not expose invocation ID in hook context | `MPM_PROVENANCE_INVOCATION_ID` not set |
| `session_end` is a lifecycle event, not epistemic | ✅ **Correct** — plugin does not emit work completion |

## Tool Schemas

Agent-facing tool schemas accurately declare parameters already supported and validated by the underlying handlers. This improves machine-readable discoverability — for code assist, type checking, and agentic tool-routing — without changing tool behaviour or API semantics.

## Result Shape

`memory_search` returns OpenClaw-shaped hits with virtual paths
(`mpm://memory/<id>`):

```json
{
  "results": [
    {
      "path": "mpm://memory/abc123",
      "startLine": 1,
      "endLine": 12,
      "score": 0.87,
      "snippet": "...full recalled content (truncated at 1200 chars)...",
      "source": "mpm",
      "collection": "memories",
      "tags": ["tag1", "tag2"],
      "weight": 87,
      "reinforcementCount": 4,
      "createdAt": "2026-07-30T08:14:05Z",
      "rank": 1
    }
  ],
  "backend": "mpm",
  "query": "...",
  "total": 1,
  "mpmScope": "all"
}
```

## Roadmap

- **mcp fast-path:** Spawn `mpm-mcp` once at plugin register time, route
  calls over MCP `tools/call` instead of per-call `mpm call` subprocess.
- **score fusion:** Combine MPM bm25 + reinforcement into a hybrid score.
- **memory_get by content:** Allow `path: "?query=foo bar"` for ad-hoc reads.

## License

Same as MPM. See `https://flowbyte.com/mpm`.
