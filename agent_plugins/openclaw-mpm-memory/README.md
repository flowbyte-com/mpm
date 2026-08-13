# @openclaw/mpm-memory

MPM-backed memory slot for OpenClaw. Routes `memory_search` and `memory_get`
through MPM's FTS5 + reinforcement-weighted recall, so `openclaw doctor` stops
warning about "No active memory plugin is registered."

## Why this exists

OpenClaw's `core/doctor/memory-search` check refuses to clear unless
`plugins.slots.memory` resolves to a plugin that is *not* the default
(`memory-core`):

```js
// dist/doctor-memory-search-CqQuutkO.js:222-233
function hasActiveAlternateMemoryPluginSlot(cfg) {
  ...
  if (memorySlot === defaultSlotIdForKey("memory")) return false; // ← line 227
  ...
}
```

`memory-core` is the default. Accepting the default is treated as a *deliberate
choice not made* — the doctor nudges you to pick a non-default plugin. This
plugin is that pick, and it makes MPM the agent's memory surface.

## What it replaces

- **Default slot occupant:** `memory-core` (sqlite-vec + BM25 + embeddings +
  dreaming subagent). Heavy.
- **This plugin:** `openclaw-mpm-memory` (subprocess → `mpm call mpm_memory`
  with action:query). Light.

The trade is honest: MPM is FTS5-only in this adapter — no semantic vector
recall. If you need embeddings, run `memory-core` for `memory_search` and
let MPM be 808's long-term substrate via `mpm__*` native tools (hybrid mode).
This plugin is for setups where MPM is the *primary* memory layer.

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

After step 1 you can also just run `openclaw doctor --fix` — it will
detect and surface the slot/entry configuration if anything is missing.

This plugin does not touch `agents.defaults.compaction.memoryFlush.enabled`.
OpenClaw's dist default applies; the neutered flush path is safe under both
`true` and `false`. See "Compaction flush (neutered at the sink)" below for
the architectural rationale.

## Configuration

```json5
plugins: {
  entries: {
    "openclaw-mpm-memory": {
      enabled: true,
      config: {
        mpmBin: "mpm",            // path to mpm; resolves from PATH by default
        timeoutMs: 5000,          // subprocess timeout; default 5000
        scope: "all",             // "all" | "local" | "shared"; default "all"
        limitDefault: 6           // default memory_search limit; 1-50
      }
    }
  },
  slots: { memory: "openclaw-mpm-memory" }
}
```

## Compaction flush (neutered at the sink)

This plugin does **not** ingest OpenClaw's compaction flush output. The
plugin's `flushPlanResolver` returns a plan whose `relativePath` points at
a throwaway file (`local_flush_trash.md`) under the calling session's
`workspaceDir`. OpenClaw writes the file as part of its flush lifecycle —
satisfying its internal contract — but the file's content is discarded
unread. No scheduler handler is wired to it; no watcher picks it up.

**Why neuter the integration.** MPM already has two higher-fidelity
epistemic sources than OpenClaw's auto-generated transcript summary:

- **Scratchpad** — agent-curated Working Context, deliberate, ephemeral.
- **Memories** (`mpm_memory`) — explicit facts with tags, weight, and
  reinforcement, retrievable via FTS5 + hybrid scoring.

Capturing OpenClaw's lossy auto-summary would add a third memory surface
with lower fidelity than the two we already have. The integration
boundary is deleted at the sink, not the source. See decision
`40544f5a04a2aac7` (2026-08-13) for full rationale; this implements the
AGENTS.md doctrine *"Truth once. Views everywhere."*

**Do not "fix" the path back to `.mpm/run/ingest.md`.** That was the
original integration, and it is the bug this neuter removes — see the
`Error: Invalid memory flush target path` incident of 2026-08-13 where a
config flip wedged the gateway and required a sessions-clear + bounce
recovery.

## Result shape

`memory_search` returns OpenClaw-shaped hits with virtual paths
(`mpm://memory/<id>`). The recalled content lives in `snippet` so agents
rarely need `memory_get`:

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
      "tags": ["...", "..."],
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

If MPM is unreachable the tool returns the standard OpenClaw unavailable shape:

```json
{ "disabled": true, "unavailable": true, "error": "...", "backend": "mpm" }
```

## Failure modes (all fail-open)

| Condition                          | Behaviour                                                                       |
| ---------------------------------- | ------------------------------------------------------------------------------- |
| `mpm` not on PATH                  | `memory_search` returns `{disabled:true, error:"...not on PATH..."}`            |
| `mpm` exits non-zero               | Tool result includes the last 500 chars of stderr/stdout as `error`              |
| Subprocess timeout                 | `error: "mpm mpm_memory timed out after 5000ms"`                                |
| MPM returns zero hits              | `results: []`, `total: 0` — normal                                              |
| `memory_get` on non-virtual path   | `{notFound:true, supportedPrefix:"mpm://memory/"}`                              |
| `memory_get` for unknown id        | `{notFound:true}`                                                               |

No error throws into the agent turn — the tool always returns a result envelope.

## Roadmap (deliberately deferred)

- **mcp fast-path:** Spawn `mpm-mcp` once at plugin register time, route calls
  over MCP `tools/call` instead of per-call `mpm call` subprocess. ~10× faster.
- **score fusion:** Combine MPM bm25 + reinforcement into a hybrid score
  instead of just weight/100.
- **memory_get by content:** Allow `path: "?query=foo bar"` for ad-hoc reads.
- **promptBuilder:** Inject current mode/persona from MPM into the bootstrap
  prompt (deferred to mpm-auto-route, which already does this).

## Why not just flip `memory-core.enabled: true`?

Doesn't work. The doctor check short-circuits on `slot === default *before*
entry.enabled is even read` (see the diagnostic above). The slot must point at
a non-default plugin id.

## License

Same as MPM. See `https://flowbyte.com/mpm`.
