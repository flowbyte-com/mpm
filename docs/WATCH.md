# Watch Daemon

Auto-ingests content from filesystem and external databases. Started with `mpm start` or standalone via `mpm watch`.

## Starting

```bash
mpm watch              # Foreground daemon
mpm watch --v          # Verbose
mpm watch --dry-run    # Process but don't delete
mpm watch --once       # One-shot sweep, then exit
```

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

After each memory ingestion: scans LTM memories for shared tags. If 3+ memories share a tag → auto-create Topic + link via `topic_memberships`. Idempotent (cached in-memory).

## Startup Sweep

On daemon start: processes all `.md` files immediately. Skips `.jsonl` (processed only on `.lock` removal). Skips `sessions.json` (live registry).

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
