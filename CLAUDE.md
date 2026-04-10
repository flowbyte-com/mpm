# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MPM (Memory-Persona-Mode) is a SQLite-native agent state management system for OpenClaw agents. It provides persistent memory, session management, topic organization, mode/persona configurations, and a reference document library — all indexed with FTS5 full-text search.

## Building & Testing

```bash
cd /home/v/.openclaw/workspace/flowbyte/mpm
make build    # Build binary to bin/mpm
make test     # Run Go tests
make install  # Install to /usr/local/bin/mpm
make clean    # Remove bin/
```

### Key Commands

```bash
mpm recall <query>   # Query memories (uses FTS5 full-text search)
mpm mode [name]     # Switch mode
mpm persona [name]   # Switch persona
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
| `cmd/mpm/handlers.go` | Command handlers (memory, mode, persona, etc.) |
| `cmd/mpm/watch.go` | Watch daemon (fsnotify-based file watcher) |
| `cmd/mpm/tui.go` | Terminal UI (lipgloss-based dashboard) |
| `internal/db.go` | DatabaseManager, SQLite schema, CRUD, FTS5 |
| `internal/memory.go` | Memory operations |
| `internal/session.go` | Session operations |
| `internal/search.go` | Search operations |
| `internal/config/config.go` | Path resolution |

## Sensitive Content Blocking

All content (memory, session, topic) is scanned against 17 regex patterns before any database write. Blocked content is logged to `mirror.jsonl` but never stored. Patterns include API keys (OpenAI, GitHub, AWS, Stripe, Slack), JWTs, private keys, SSH keys, database URLs, and general `password=`/`secret=` patterns.

## Shred Protocol

Hard delete — `DELETE` + `VACUUM`. Triggers fire the FTS5 `DELETE` triggers so the search index stays in sync.
