# Watch Daemon

File watcher for automatic memory ingestion. Runs as a detached background process — `mpm watch start` spawns a child process that persists independently of the terminal that started it.

## Architecture

The watcher operates in two modes:

- **Detached mode** (default): `mpm watch start` spawns a background child process that blocks on `select{}` until signaled. The parent CLI exits immediately after spawning.
- **In-process mode**: `startWatchGoroutine()` / `stopWatchGoroutine()` manage the watcher goroutines directly within the current process (used by `watch restart` when no detached watcher is running).

The child process writes its PID to `watch.pid` (in the MPM data directory) for inter-process communication with `stop` and `status` commands.

## Starting

```bash
mpm watch start      # Spawn detached background watcher (returns immediately)
mpm watch stop       # Signal the detached watcher to shut down gracefully
mpm watch status     # Check if watcher is running
mpm watch restart    # Blocked while detached watcher is running (use stop + start instead)
```

**Detached persistence:** The child process survives terminal close. To persist across reboots, use a process supervisor (systemd, supervisord) or run with `nohup`.

**PID file:** Written to `{MPM_DIR}/watch.pid` with `0600` permissions (owner-only read/write).

## Config Sources (`mpm_config.json`)

| Field | Watches | Action |
|-------|---------|--------|
| `memory_dirs[]` | `.md` files (Create/Write) | Sanitize → ingest as LTM (weight=10) → delete `.md` |
| `sessions_dirs[]` | `.lock` removal | Derive `.jsonl` → parse facts → ingest → delete `.jsonl` |
| `sessions_dirs[]` | `workspace.json`, `config.json` | Snapshot to `system_config` (hash-checked, no delete) |
| `external_dbs[]` | Polling (per-db interval) | Copy new memories by cursor, dedup by `source_id` |

If `memory_dirs`/`sessions_dirs` are unset, defaults to `$MPM_WORKSPACE/memory/` and `~/.openclaw/agents/main/sessions`.

## Processing Details

**Memory files (.md)**: Transient files — write a thought, daemon picks it up, stores permanently, deletes source.

**Session locks (.lock)**: `.lock` removal signals session completion. 60s guard + 3s grace period prevent premature reads. Facts extracted via regex patterns, ingested as session memories. Async LLM synthesis triggered after.

**System config**: `workspace.json` and `config.json` are hash-checked — no duplicate writes. Source files preserved.

**External DBs**: Queries `SELECT id, content, session_id, tags, created_at FROM memories WHERE deleted_at IS NULL AND created_at > ?`. Cursor persisted to `external_db_cursors` table.

## Topic Clustering

After each memory ingestion: scans LTM memories for shared tags. If 3+ memories share a tag → auto-create Topic + link via `topic_memberships`. Database is sole source of truth — no in-memory cache.

## Startup Sweep

On daemon start: processes all `.md` files immediately. Skips `.jsonl` (processed only on `.lock` removal). Skips `sessions.json` (live registry).

## Graceful Shutdown

The detached child handles `SIGTERM` / `os.Interrupt` (Ctrl+C) by:
1. Cancelling the watcher context
2. Draining the worker pool
3. Deleting `watch.pid`
4. Exiting cleanly

## Path Management

```bash
mpm watch add-path <path> --type memory|sessions
mpm watch remove-path <path> --type memory|sessions
mpm watch list-paths
```

## Files Never Processed

- `sessions.json` / `session.json` — live session registries
- `session *.json` (any suffix including Nextcloud sync conflicts)
- Any file with `conflicted` in name
- `.jsonl` without matching `.lock` removal event