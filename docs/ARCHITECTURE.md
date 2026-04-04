# MPM Architecture

## System Overview

MPM is a Unix socket daemon (`mpm start`) with a CLI wrapper. The CLI proxies commands to the daemon via a Unix socket. A watch daemon (`mpm watch`, started automatically alongside) monitors the filesystem for new content to ingest.

```
CLI (mpm) ─────────────────────────────────────────────────────
  mpm memory add "..."      mpm status      mpm persona
──────────────────────────────────────┬─────────────────────────
                                     │ Unix socket
                                     ▼
┌──────────────────────────────────────────────────────────────┐
│  Daemon (mpm start)                                         │
│  • SQLite operations (memories, sessions, topics, etc.)      │
│  • JSONL mirror updates                                      │
│  • Session synthesis trigger                                 │
│  • Responds to all CLI commands via socket                  │
└────────────────────────────┬───────────────────────────────┘
                             │
          ┌──────────────────┴──────────────────┐
          ▼                                      ▼
┌─────────────────────┐          ┌─────────────────────────────────┐
│  SQLite: mpm.db     │          │  Watch Daemon (mpm watch)        │
│  • memories          │          │  • fsnotify on memory/ dirs       │
│  • sessions          │          │  • .md → memory (Route A)         │
│  • topics            │          │  • .lock rm → session (Route B)  │
│  • topic_memberships  │          │  • sessions.json → system_config  │
│  • modes             │          └─────────────────────────────────┘
│  • personas          │
│  • system_config     │
│  • reference_docs    │
│  • reference_chunks  │
│  • lessons           │
└─────────────────────┘
          │
          ▼
┌─────────────────────┐
│  mirror.jsonl        │  ← Append-only audit log
└─────────────────────┘
```

## Daemon Lifecycle

- **`mpm start`** — spawns the daemon subprocess, auto-starts the watch daemon, acquires the socket lock
- **`mpm stop`** — gracefully stops daemon + watch daemon
- **`mpm restart`** — stop + start
- Stale socket cleanup is automatic on startup

## Watch Daemon

Two directories are monitored:

| Watch Dir | Config Key | Monitors | Action |
|-----------|------------|----------|--------|
| `memory_dir` | `memory_dir` | `*.md` | Sanitize → ingest as memory → delete file |
| `sessions_dir` | `sessions_dir` | `*.lock` removed | Derive `.jsonl` → parse → ingest as session → delete `.jsonl` |
| `sessions_dir` | `sessions_dir` | `sessions.json` | Snapshot to `system_config` (hash-checked, no dup writes) |
| `reference_dir` | `reference_dir` | `*` (any file) | Parse content → chunk → ingest into reference library, keep file |

See [WATCH.md](WATCH.md) for full details.

## Database Schema (`mpm.db`)

All tables live in a single SQLite database at `projects/mpm/src/db/mpm.db`.

### Core Tables

| Table | Purpose |
|-------|---------|
| `memories` | Long-term distilled memories with FTS5 index |
| `sessions` | Session transcripts (auto-ingested from `.jsonl`) |
| `topics` | Topic tree with hierarchical parent links |
| `topic_memberships` | Memory-to-topic and session-to-topic links |
| `modes` | Mode configurations (JSON per mode) |
| `personas` | Persona configurations (JSON per persona) |
| `system_config` | OpenClaw `sessions.json` snapshots, hash-checked |
| `reference_docs` | Reference library documents |
| `reference_chunks` | Reference document chunks for vector search |
| `lessons` | Learned lessons (warnings, practices, insights) |

### FTS5 Indexes

Each core table has a corresponding FTS5 virtual table with INSERT/UPDATE/DELETE triggers:

| FTS5 Table | Content |
|------------|---------|
| `memories_fts` | `content, tags` |
| `sessions_fts` | `content, session_id, content_hash` |
| `topics_fts` | `name, description, tags` |
| `references_fts` | `title, tags, content` |
| `lessons_fts` | `content, tags` |

Query strategy in `QueryMemory`:
1. **FTS5 MATCH** — ranked BM25 results via `JOIN memories_fts`
2. **LIKE fallback** — if FTS query fails (malformed query syntax)
3. **Recent rows** — if query is empty

### Migrations

New columns are added via `ALTER TABLE ... ADD COLUMN` with `IF NOT EXISTS` semantics — SQLite ignores duplicate column errors, so existing databases get new columns automatically on restart.

## Shred Protocol

Hard delete — `DELETE` + `VACUUM`. Not soft delete. Triggers fire the FTS5 `DELETE` triggers so the search index stays in sync.

```bash
mpm memory shred <id>      # One record
mpm shred memories -f     # All memories
mpm shred database -f     # Entire database
```

## Sensitive Content Blocking

All content (memory, session, topic) is scanned against 17 regex patterns before any database write. Blocked content is logged to `mirror.jsonl` but never stored.

Blocked: API keys (OpenAI, GitHub, AWS, Stripe, Slack), JWTs, private keys, SSH keys, database URLs, general `password=`/`secret=` patterns.

See [security/SECURITY.md](security/SECURITY.md) for the full pattern list.

## Path Resolution

No hardcoded paths. All paths derived from:

1. `MPM_WORKSPACE` env var (highest priority)
2. Executable-relative resolution
3. Current working directory (fallback)

See [PATH_CONFIG.md](PATH_CONFIG.md) for full details.

## Build Tags

| Tag | Effect |
|-----|--------|
| none | Standard build (default) |

## Why MPM?

MPM is designed as the **front cortex** for AI agents — the persistent state layer between otherwise stateless LLM calls.

Just as the human brain's prefrontal cortex maintains context, goals, and learned patterns across interactions, MPM provides:

- **Memory** — distilled experiences and facts from past sessions
- **Sessions** — raw transcripts for audit and context replay
- **Topics** — hierarchical organization of knowledge by domain
- **Lessons** — distilled "don't do X" / "do Y" / "X leads to Y" patterns
- **References** — searchable document library for domain knowledge
- **Modes & Personas** — behavioral context switching without prompt engineering

The synthesis pipeline continuously distills sessions → memories → topics, while lessons capture hard-won wisdom. The watch daemon silently ingests new content, keeping MPM's model of the world current.

This makes MPM ideal for:
- Long-running agentic workflows that span multiple sessions
- Projects where institutional knowledge needs to persist between developers
- AI-assisted coding where context from previous sessions informs current decisions

