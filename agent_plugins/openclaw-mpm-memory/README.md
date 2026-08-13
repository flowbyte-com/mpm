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

## Memory-flush path (compaction)

This plugin writes memory-flush output to disk so the MPM substrate can pick
it up. Concretely, the plugin's `flushPlanResolver` returns a real plan (not
`{ kind: "noop" }`) that asks OpenClaw to write flushed content to:

```
.mpm/run/ingest.md
```

Relative to the calling session's `workspaceDir` — for the main session
(`/home/v/`), this resolves to the canonical target:

```
/home/v/.mpm/run/ingest.md
```

The `mpm-scheduler` daemon's `openclaw_ingest` tick handler reads that file
on its next 60-second tick and inserts a `scheduled_wakes` row of `kind:
"ephemeral_compaction_ready"` with `metadata.source = "openclaw_ingest"`.
The wake surfaces on the agent's next `read_wake_context` call (via the
`overdue_wakes` field) for the agent to consume.

To enable the bridge end-to-end, you need:

1. **OpenClaw side** — flush plan enabled in user config:
   ```bash
   openclaw config set agents.defaults.compaction.memoryFlush.enabled true
   ```
2. **MPM side** — the scheduler daemon running with the `openclaw_ingest`
   tick handler registered (it is, by default, in current `mpm-scheduler`
   builds):
   ```bash
   systemctl --user status mpm-scheduler
   journalctl --user -u mpm-scheduler -n 20 --no-pager | grep -i ingest
   ```

**Failure mode if `mpm-scheduler` is NOT running:** the plugin will write
to `/home/v/.mpm/run/ingest.md` on every OpenClaw flush event, and the
file will silently pile up in `/home/v/.mpm/run/` with no automatic
cleanup. There is no daemon-side fallback; the scheduler is the only
drain mechanism. If you see multiple `ingest.md` / `ingest.md.processing`
/ `ingest.md.rejected` files accumulating, scheduler is the suspect.

**Size cap:** the ingest watcher caps each file at 64 KB. Larger content
is quarantined as `ingest.md.rejected` (rename, not delete) instead of
ingesting — either OpenClaw is producing too-large flushes or the
flush contained content that exceeded the size budget. Inspect the
quarantine file, then delete it manually.

**Symlink refusal:** the watcher uses `Lstat` and refuses to follow
symlinks. If `/home/v/.mpm/run/ingest.md` is a symlink, the watcher
will log a warning and skip — the wake will not be inserted.

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
