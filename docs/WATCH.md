# Watch Daemon

The watch daemon monitors directories for new files and automatically ingests them into MPM's SQLite database.

## Starting and Stopping

```bash
mpm start        # Starts both MPM daemon and watch daemon
mpm watch        # Start watch daemon only (standalone)
mpm watch status # Check if running (via daemon proxy)
mpm watch start  # Start via daemon proxy
mpm watch stop   # Stop via daemon proxy
```

The watch daemon is started automatically by `mpm start`.

## What It Monitors

Three directories are watched (configured via `memory_dir`, `sessions_dir`, and `reference_dir`):

| Directory | Monitors | Action on Event |
|-----------|----------|-----------------|
| `memory_dir` | `*.md` | Ingest as LTM memory, delete file |
| `sessions_dir` | `*.lock` remove | Process matching `.jsonl` as session |
| `sessions_dir` | `sessions.json` | Snapshot to `system_config` table |
| `reference_dir` | `*` (any file) | Ingest as reference document, keep file |

Default paths:
- `memory_dir` → `~/.openclaw/workspace/memory`
- `sessions_dir` → `~/.openclaw/agents/main/sessions`
- `reference_dir` → `projects/mpm/reference`

All paths are configurable via `mpm_config.json`.

## Processing Routes

### Route A: Markdown Files (.md)

On `Create` or `Write` fsnotify events:

1. Read file content
2. Scan for sensitive content (17 patterns)
3. If clean: ingest as LTM memory with `weight=10`
4. Update JSONL mirror
5. Delete the `.md` file

Files are meant to be transient — write a thought to a `.md` file, the watch daemon picks it up and stores it permanently.

### Route B: Session Lock Files (.lock)

On `Remove` events for `.lock` files:

1. Derive matching `.jsonl` path from `.lock` path
2. **Skip if file modified within 120 seconds** — session still alive
3. **Wait 10 seconds** for OpenClaw to finish flushing writes
4. **Re-check**: if file modified during wait, skip entirely
5. Parse JSONL session file
6. Extract structured facts
7. Ingest as session memory
8. Update JSONL mirror
9. Delete the `.jsonl` file

This conservative delay prevents reading partially-written session files.

The `.lock` acts as a signal that the session is complete and ready for processing.

### Route C: sessions.json

On any fsnotify event for `sessions.json`:

1. Read the file
2. Parse session entries (id, model, skills, tokens, timestamps)
3. Write/update `system_config` table (hash-checked, no duplicate writes)
4. Do NOT delete `sessions.json`

### Route D: Reference Library Files (reference_dir)

On `Create` or `Write` fsnotify events for any file in the reference directory:

1. Detect file type (PDF, EPUB, markdown, plain text, etc.)
2. Parse content using pure-Go parsers (no external dependencies)
3. Chunk content into ~512-word segments
4. Ingest into `reference_docs` + `reference_chunks` tables
5. **Keep the original file** (unlike memory files, references are retained)

Supported formats: PDF (`.pdf`), EPUB (`.epub`), Markdown (`.md`), Plain text (`.txt`), HTML (`.html`), and others read as raw text.

Idempotent: if a file has already been ingested, it is skipped.

## Startup Sweep

On daemon start, performs a one-time sweep of all watch directories:

- **`.md` files** → processed immediately (safe: transient thought files)
- **`.jsonl` files** → processed only if no matching `.lock` exists AND file is older than 15 minutes
- **`workspace.json` / `config.json`** → processed only if older than 30 seconds
- **`sessions.json`** → **never processed** (live file, managed by OpenClaw)
- **reference files** → ingested after 5-second stabilization wait (idempotent)

The long delays on session files prevent races if the daemon restarts while OpenClaw is running a long session.

## System Files

The watch daemon skips system files entirely:
- `sessions.json` — **never processed** (live OpenClaw registry, changes on every message)
- `session.json` — skipped
- `workspace.json` — only processed on startup sweep if >30s old
- `config.json` — only processed on startup sweep if >30s old

`sessions.json` is handled via a separate mechanism, not the watch daemon.

## Verbose Mode

```bash
mpm watch --v        # Verbose output to stdout
mpm watch --dry-run  # Process but don't delete files
mpm watch --once     # Startup sweep only, then exit
```

## Troubleshooting

### Files not being processed
1. Check watch daemon is running: `mpm status`
2. Check paths are correct: `mpm doctor`
3. Try verbose mode: `mpm watch --v`
4. Check for `.lock` files blocking `.jsonl` processing

### "Sensitive content blocked"
Content matched a blocked pattern. Check `mirror.jsonl` for details.

### Reference files not ingested
1. Verify `reference_dir` exists and is readable
2. Check file type is supported (PDF, EPUB, md, txt)
3. Run verbose: `mpm watch --v` and look for `📚` prefix

---

**Last Updated:** 2026-04-03
