# Watch Daemon

The watch daemon monitors directories and external databases for new content and automatically ingests them into MPM's SQLite database.

## Starting and Stopping

```bash
mpm start        # Starts both MPM daemon and watch daemon
mpm watch        # Start watch daemon only (standalone)
mpm watch status # Check if running (via daemon proxy)
mpm watch start  # Start via daemon proxy
mpm watch stop   # Stop via daemon proxy
```

The watch daemon is started automatically by `mpm start`.

## Configuration

All sources are configured via `mpm_config.json`. Each source is optional — the daemon starts only if at least one source is configured.

```json
{
  "memory_dirs": ["/path/to/memory"],
  "sessions_dirs": ["/path/to/sessions"],
  "external_dbs": [
    {
      "path": "~/.openclaw/memory/main.sqlite",
      "label": "openclaw",
      "interval_seconds": 30
    }
  ]
}
```

### Config Fields

| Field | Type | Description |
|-------|------|-------------|
| `memory_dirs` | `string[]` | Directories to watch for `.md` memory files |
| `sessions_dirs` | `string[]` | Directories to watch for `.jsonl` session files |
| `external_dbs` | `object[]` | External SQLite DBs to poll for memories |

**Path resolution:** Paths starting with `~` are expanded to `$HOME`. Relative paths are resolved relative to `MPM_WORKSPACE`.

**Defaults:** If `memory_dirs` or `sessions_dirs` are omitted, they default to MPM's internal workspace directories. Set to an empty array `[]` to disable.

## What It Monitors

| Source | Type | Action | Condition |
|--------|------|--------|-----------|
| `memory_dirs[]` | fsnotify | Inject `.md` files as LTM memories | Array has entries |
| `sessions_dirs[]` | fsnotify | Ingest session `.jsonl` files | Array has entries |
| `external_dbs[]` | polling | Ingest memories from external SQLite | Array has entries |

### Default Internal Paths

If `memory_dirs` / `sessions_dirs` are not specified, the daemon uses MPM's internal workspace directories:
- `memory_dir` → `MPM_WORKSPACE/memory`
- `sessions_dir` → `MPM_WORKSPACE/sessions`

### External SQLite Database (polling)

Each entry in `external_dbs` is polled independently:

```json
{
  "path": "~/.openclaw/memory/main.sqlite",
  "label": "openclaw",
  "interval_seconds": 30
}
```

**Fields:**
- `path` — SQLite DB path (supports `~` and relative paths)
- `label` — Label added to ingested memories (e.g. `"source: openclaw"`)
- `interval_seconds` — Poll interval (default: 30)

**Why polling?** SQLite doesn't support fsnotify. Changes are detected by polling `MAX(created_at)`.

**Schema expected in external DB:**
```sql
SELECT id, content, session_id, tags, created_at
FROM memories
WHERE deleted_at IS NULL
ORDER BY created_at ASC
```

Memories are **copied** (not moved) — the source DB is never modified.

**First poll:** Ingestion starts from the newest memory currently in the external DB. On subsequent polls, only newer memories are ingested.

**Cursor persistence:** The cursor (last-seen `created_at`) is held in memory. On daemon restart, re-ingestion from the newest existing memory may occur. To prevent duplicates, the external DB should have unique `id` values and MPM should skip already-ingested IDs.

## Processing Routes

### Route A: Markdown Files (.md)

On `Create` or `Write` fsnotify events across all `memory_dirs`:

1. Read file content
2. Scan for sensitive content (17 patterns)
3. If clean: ingest as LTM memory with `weight=10`
4. Update JSONL mirror
5. Delete the `.md` file

Files are transient — write a thought to a `.md` file, the watch daemon picks it up and stores it permanently.

### Route B: Session Lock Files (.lock)

On `Remove` events for `.lock` files across all `sessions_dirs`:

1. Derive matching `.jsonl` path from the `.lock` path
2. **Skip if file modified within 120 seconds** — session still alive
3. **Wait 10 seconds** for OpenClaw to finish flushing writes
4. **Re-check**: if file modified during wait, skip entirely
5. Parse JSONL session file
6. Extract structured facts
7. Ingest as session memory
8. Update JSONL mirror
9. Delete the `.jsonl` file

The `.lock` acts as a signal that the session is complete.

### Route C: External SQLite Databases (polling)

On each poll interval for each configured `external_db`:

1. Query for memories with `created_at` > last-seen cursor
2. For each row: insert into MPM's `memories` table with `source: <label>`
3. Skip if a memory with the same `id` already exists (idempotent)
4. Update cursor to `MAX(created_at)` of ingested rows

### Route D: sessions.json

On any fsnotify event for `sessions.json` (in any `sessions_dirs`):

1. Read the file
2. Parse session entries (id, model, skills, tokens, timestamps)
3. Write/update `system_config` table (hash-checked, no duplicate writes)
4. Do NOT delete `sessions.json`

### Route E: Reference Library Files (reference_dir)

On `Create` or `Write` fsnotify events for any file in MPM's reference directory:

1. Detect file type (PDF, EPUB, markdown, plain text, HTML)
2. Parse content using pure-Go parsers
3. Chunk content into ~512-word segments
4. Ingest into `reference_docs` + `reference_chunks` tables
5. **Keep the original file**

Idempotent: if a file has already been ingested, it is skipped.

## Startup Sweep

On daemon start, performs a one-time sweep of all configured directories:

- **`.md` files** → processed immediately
- **`.jsonl` files** → processed only if no matching `.lock` exists AND file is older than 15 minutes
- **`sessions.json`** → **never processed**
- **External DBs** → first poll runs immediately on startup

## System Files

The watch daemon skips:
- `sessions.json` — **never processed** (live registry)
- `session.json` — skipped
- `workspace.json` / `config.json` — only processed on startup if >30s old

## Verbose Mode

```bash
mpm watch --v        # Verbose output to stdout
mpm watch --dry-run  # Process but don't delete files
mpm watch --once     # Startup sweep only, then exit
```

## Troubleshooting

### Watch daemon not starting
- Check at least one source is configured
- Run `mpm doctor` to verify paths

### External DB not ingesting
- Verify the path points to a valid SQLite file with a `memories` table
- Check `external_db.enabled` is not explicitly false
- Try verbose mode: `mpm watch --v`

### Files not being processed
1. Check watch daemon is running: `mpm status`
2. Check paths are correct: `mpm doctor`
3. Try verbose mode: `mpm watch --v`
4. Check for `.lock` files blocking `.jsonl` processing

---

**Last Updated:** 2026-04-09
