# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MPM (Memory-Persona-Mode) is a SQLite-native agent state management system for OpenClaw agents. Everything is a **memory** — sessions, topics, lessons, references are just collections with different metadata. This makes the CLI simple and the system extensible.

## Building & Testing

**Note:** `~/mpm` is the canonical install location. This workspace uses
`MPM_WORKSPACE` or `/home/v/.openclaw/workspace/projects/mpm`.

```bash
make build    # Build binary to bin/mpm
make test     # Run Go tests
make install  # Install to /usr/local/bin/mpm
make clean    # Remove bin/
```

### Simplified Commands

```bash
# Memory operations (standalone - no daemon needed)
mpm add <content>                    # Add memory
mpm ls                               # List memories
mpm show <id>                        # Show memory details
mpm rm <id>                          # Soft delete memory
mpm recall <query>                   # Search memories (FTS5)
mpm shred <id>                       # Secure delete

# Memory importance
mpm promote <id>                     # Make LTM (weight=10)
mpm reinforce <id> [delta]           # Increment reinforcement
mpm weaken <id> [delta]              # Decrement reinforcement
mpm set-weight <id> <0-100>          # Set weight directly

# Management
mpm stats                            # Memory statistics
mpm prune                            # Prune expired/old
mpm export                           # Export to JSON/CSV

# Daemon-required (runtime agent config)
mpm mode <name>                      # Switch mode
mpm persona <name>                   # Switch persona
mpm prime-directives                 # Show 808 directives
```

Run a single test:
```bash
go test -v ./internal/... -run TestFunctionName
```

## Architecture

MPM runs as **two independent daemon processes**:

```
CLI (mpm) ────── Unix socket ──────> Main Daemon ──> SQLite (mpm.db)
                                          │
                                          └──> Watch Daemon (fsnotify)
                                                     ├── memory/*.md  → ingest as memory
                                                     ├── sessions/*.lock → derive session
                                                     └── reference/* → chunk and index
```

**Main daemon**: Handles CLI commands via Unix socket, manages SQLite, compiles modes/personas.
**Watch daemon**: Filesystem monitoring via `fsnotify`, auto-ingests content independently.

Start/stop: `mpm start`, `mpm stop`, `mpm restart`

## Memory Model

Everything is a **memory**. Collections distinguish types:

| Collection | Purpose |
|------------|---------|
| `memories` | General memories |
| `session` | Session facts |
| `lessons` | Learned lessons |

Metadata controls importance:
- `weight` (1-100) — importance level
- `reinforcement_count` — times memory proved useful
- `is_long_term` — promoted to LTM (weight ≥ 10)
- `expires_at` — optional TTL for ephemeral memories
- `last_accessed_at` — tracks recency

## Database

Location: `src/db/mpm.db` (SQLite with FTS5 indexes)

Key tables: `memories`, `sessions`, `topics`, `topic_memberships`, `modes`, `personas`, `reference_docs`, `reference_chunks`, `lessons`

FTS5 virtual tables with INSERT/UPDATE/DELETE triggers keep search indexes in sync. Query strategy in `QueryMemory`: FTS5 MATCH → LIKE fallback → recent rows.

## Path Resolution

No hardcoded paths. Resolution order:
1. `MPM_WORKSPACE` env var
2. Executable-relative resolution
3. CWD fallback

## Key Source Files

| File | Purpose |
|------|---------|
| `cmd/mpm/main.go` | CLI entry point, command router |
| `cmd/mpm/router.go` | Command registry and socket communication |
| `cmd/mpm/simple_cmds.go` | Simplified memory commands (add, ls, show, rm, etc.) |
| `cmd/mpm/maint_cmds.go` | Maintenance commands (stats, prune, export) |
| `cmd/mpm/recall.go` | Search with time-range filters |
| `cmd/mpm/watch.go` | Watch daemon + external DB polling |
| `cmd/mpm/daily_review.go` | Daily review TUI command |
| `cmd/mpm/dashboard.go` | Dashboard TUI component |
| `cmd/mpm/web.go` | Web UI server |
| `cmd/mpm/web_handlers.go` | Web UI HTTP handlers |
| `cmd/mpm/ingest.go` | Filesystem ingest command |
| `internal/db.go` | DatabaseManager, SQLite schema, CRUD, FTS5 |
| `internal/memory.go` | MemoryStore operations |
| `internal/web_db.go` | Web/database methods (reinforce, stats, etc.) |
| `internal/schema.go` | Database schema definitions |
| `internal/config/config.go` | Path resolution |

## Sensitive Content Blocking

All content (memory, session, topic) is scanned against 17 regex patterns before any database write. Blocked content is logged to `mirror.jsonl` but never stored. Patterns include API keys (OpenAI, GitHub, AWS, Stripe, Slack), JWTs, private keys, SSH keys, database URLs, and general `password=`/`secret=` patterns.

## Shred Protocol

Hard delete — `DELETE` + `VACUUM`. Triggers fire the FTS5 `DELETE` triggers so the search index stays in sync.
