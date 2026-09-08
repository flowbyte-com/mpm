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

# 6. (Required since 0.1.3) Opt in to typed hooks so wake-context injection works
# Both flags are required: `agent_turn_prepare` falls in OpenClaw's
# `promptInjectionHookNameSet` (requires `allowPromptInjection=true`) AND in
# `conversationHookNameSet` (requires `allowConversationAccess=true` for
# non-bundled plugins). Without these, the plugin runs but the typed hooks
# are silently blocked at register time.
openclaw config set plugins.entries.openclaw-mpm-memory.hooks.allowConversationAccess true
openclaw config set plugins.entries.openclaw-mpm-memory.hooks.allowPromptInjection true

# 7. Restart the gateway
openclaw gateway restart

# 8. Verify — should return ok:true with zero findings
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
async fetch of MPM wake context (non-blocking, 5000 ms ceiling)
    │
    ▼
agent_turn_prepare fires (next agent turn)
    │
    ├── await cached wake context promise (1500 ms race ceiling)
    │
    ▼
prependContext ← wake context injected into agent prompt
```

**First-turn fallback (lifecycle race).** If `agent_turn_prepare` fires
before `session_start` (or in any session where the cache is empty), the
plugin synchronously fetches wake context with the same race ceiling and
injects it. This eliminates the session_start race; subsequent turns
hit the cache.

For heartbeat turns, `heartbeat_prompt_contribution` injects a brief MPM status
note (first 300 chars of wake context) without duplicating the full context.

**This is the only delivery path for wake context.** OpenClaw does not
require any persistent MPM instruction block in `SOUL.md`, `AGENTS.md`,
or any other markdown file. The runtime hook chain — `session_start`
(starting the fetch) followed by `agent_turn_prepare` (awaiting and
returning the result as `prependContext`) — is what surfaces the wake
payload to the model. A regression guard in
`tests/runtime_injection.test.js` pins this contract.

### Per-session cache key

OpenClaw's `PluginAgentTurnPrepareEvent` payload carries
`{ prompt, messages, queuedInjections }` only — `sessionKey` is on the
hook *context* (`ctx.sessionKey`), not on the event. The plugin reads
from `ctx` first and falls back to `event.sessionKey`. This is what
makes `session_start` (which stores under the real sessionKey) and
`agent_turn_prepare` (which looks it up) match.

### OpenClaw automatic-memory-context path

The gateway has a separate automatic BOOTSTRAP.md / USER.md memory-context
pipeline that consults the chosen memory slot's runtime. The plugin's
runtime exposes `classifyWorkspaceMemoryPaths` (returning `[]` for the
MPM case) so the gateway stops excluding the slot for "missing
provenance classification". Without this method, the gateway logs
`excluding automatic memory context: selected memory runtime does not
support provenance classification` and skips the slot — independent of
the typed-hook wake path.

## Provenance

`resolve_exec_env` contributes these env vars to every `exec` tool call:

| Env var | Value | Source |
|---------|-------|--------|
| `MPM_PROVENANCE_FRAMEWORK` | `"openclaw"` | Fixed |
| `MPM_PROVENANCE_SESSION_KEY` | OpenClaw `sessionKey` | Hook context |

`MPM_PROVENANCE_MODEL` and `MPM_PROVENANCE_INVOCATION_ID` are intentionally
**not** set — OpenClaw's hook context does not expose model name or
invocation ID. These fields are left unset rather than fabricated.

## Handoff path on the compact MCP surface

The default 3-tool MCP surface (`mpm_memory`, `mpm_context`, `mpm_help`)
exposed via `mpm-mcp` does **not** include the substrate `mpm_handoff`
tool. To record a handoff from inside an OpenClaw session, call the
compact-surface path:

```text
mpm_context
  action=write_handoff
  params:
    summary      (required, non-empty)
    state        (optional: clean | crashed | interrupted | force_end)
    commitments  (optional: string[])
    open_questions (optional: string[])
    session_id   (optional)
```

The `mpm_context` MCP tool description advertises this action explicitly
("`write_handoff / read_handoff` (session continuity)"); the
`contextAdapter` closure in `cmd/mpm-mcp/tools.go` rewrites the
model-facing name to the substrate action (`write` / `read`) before
forwarding to the underlying `mpm_handoff` handler. **Do not** call
`mcp__mpm__mpm_handoff` — that tool is not on the default compact
surface and the call will be rejected. The substrate `mpm_handoff`
tool remains reachable only via the `mpm call mpm_handoff --payload
'{"action":"write","params":{"summary":"..."}}'` CLI escape hatch,
used when the MCP transport is unavailable or `MPM_EXPOSE_ALL_TOOLS=1`
is set in the MCP env block.

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
        // ALWAYS use an absolute path here. The OpenClaw gateway runs as a
        // systemd --user service with a stripped PATH; the bare default
        // "mpm" (PATH-resolved) fails at runtime with `spawn mpm ENOENT`.
        // install.sh detects and writes the absolute path automatically
        // on this host — see the systemd gotcha note below.
        mpmBin: "$HOME/.mpm/bin/mpm",
        timeoutMs: 5000,           // subprocess timeout; default 5000
        scope: "all",              // "all" | "local" | "shared"; default "all"
        limitDefault: 6            // default memory_search limit; 1-50
      }
    }
  },
  slots: { memory: "openclaw-mpm-memory" }
}
```

### systemd / PATH-resolved gotcha

The OpenClaw gateway is launched by `systemd --user` and inherits a
deliberately minimal `PATH` (typically `/usr/local/bin:/usr/bin`). Even if
your interactive shell has `mpm` on PATH (e.g. via `~/.local/bin` from a
profile), the gateway's subprocess will not see it. This is by design — the
service-level PATH is independent of the user-shell PATH for predictability
and security.

**Symptom:** plugin logs `health_check failed — mpm mpm_system failed:
spawn mpm ENOENT` at gateway boot, and every `memory_search` /
`memory_get` returns `{disabled:true}`.

**Fix:** set `mpmBin` to the absolute path of the `mpm` binary:

```bash
openclaw config set plugins.entries.openclaw-mpm-memory.config.mpmBin "$(command -v mpm)"
openclaw gateway restart
```

The bundled `install.sh` does this automatically when an `openclaw` CLI is
on PATH, so fresh installs are unaffected. This hit was filed and fixed in
0.1.3 (2026-09-02) after a clean reinstall on a host where the interactive
shell PATH differed from the systemd unit's PATH.

## Failure Modes (All Fail-Open)

| Condition | Behaviour |
|---|---|
| `mpm` not on PATH / binary not found | `memory_search` returns `{disabled:true, error:"...ENOENT..."}`; health-check log surfaces the config-set hint (added 0.1.3) |
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
