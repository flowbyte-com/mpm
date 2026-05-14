# CLAUDE.md

MPM (Memory Persistence Module) — SQLite-native agent state management for OpenClaw agents.

## Build & Test

```bash
make build    # Build bin/mpm (CGO + fts5)
make test     # go test -tags fts5 ./...
make install  # sudo install to /usr/local/bin/mpm
make clean    # Remove bin/

go test -v ./internal/... -run TestFunctionName  # Single test
```

## Architecture

Unified single process — all commands execute in-process. No daemon subprocess or Unix socket IPC.

```
CLI ──> Router ──> Handler (in-process) ──> SQLite (mpm.db)
                         │
                         └──> WorkerPool (goroutine pool)
                               ├── fsnotify watcher ──> .md → LTM memory
                               ├── .lock removed → session facts
                               └── external DB polling
```

All commands work standalone (no separate daemon): `add`, `ls`, `show`, `rm`, `recall`, `promote`, `reinforce`, `weaken`, `set-weight`, `shred`, `stats`, `prune`, `export`, `topic`, `reference`, `ingest`, `mode`, `persona`, `session`, `lesson`, `memory`, `compile`, `llm`.

## Key Source Files

| File | Purpose |
|------|---------|
| `cmd/mpm/main.go` | CLI entry, doctor diagnostics, help system |
| `cmd/mpm/router.go` | Command registry, flag parsing, dispatch |
| `cmd/mpm/handlers.go` | All handlers (memory, session, topic, lesson, mode, persona, reference, watch lifecycle) |
| `cmd/mpm/worker.go` | WorkerPool, WatchEvent types, event processors (runs fsnotify + external DB in-goroutine) |
| `cmd/mpm/simple_cmds.go` | Standalone memory ops (add, ls, show, rm, promote, etc.) |
| `cmd/mpm/maint_cmds.go` | Stats, prune, export, maintain |
| `cmd/mpm/recall.go` | FTS5 search with time-range filters |
| `cmd/mpm/watch.go` | fsnotify goroutine, external DB polling goroutine, topic clustering |
| `cmd/mpm/daily_review.go` | Daily review report |
| `cmd/mpm/dashboard.go` | Bubbletea TUI dashboard |
| `cmd/mpm/web.go` | Web UI server (embedded static assets) |
| `cmd/mpm/web_handlers.go` | REST API handlers |
| `cmd/mpm/ingest.go` | External SQLite ingest pipeline |
| `cmd/mpm/synthesize.go` | LLM session synthesis |
| `cmd/mpm/topic.go` | Topic management (create, add, remove, list, show, delete) |
| `cmd/mpm/tui.go` | Bubbletea TUI for mode/persona selection |
| `internal/db.go` | DatabaseManager, unified schema, CRUD, FTS5, migrations |
| `internal/memory.go` | MemoryStore, sensitive content blocking, poison phrases, self-maintenance |
| `internal/web_db.go` | Web/database query methods |
| `internal/schema.go` | Schema definitions (11 tables, 19 indexes, 15 migrations) |
| `internal/mode.go` | ModeManager (JSON file-based) |
| `internal/persona.go` | PersonaManager (JSON file-based) |
| `internal/lessons.go` | LessonStore (wrapper over DatabaseManager) |
| `internal/session.go` | SessionStore (snapshot, query, recent) |
| `internal/search.go` | SearchWithSnippet, Shred via DatabaseManager |
| `internal/ingest.go` | OpenClaw ingest pipeline, schema detection, staging |
| `internal/reference_new.go` | ReferenceDB, PDF/EPUB parsing, chunking |
| `internal/config/config.go` | Path resolution, config loading |

## Database

`src/db/mpm.db` — SQLite with FTS5. Tables: `memories`, `sessions`, `topics`, `topic_memberships`, `modes`, `personas`, `reference_docs`, `reference_chunks`, `lessons`, `system_config`, `raw_memories`, `external_db_cursors`.

FTS5 virtual tables with auto-sync triggers. Query: FTS5 MATCH → LIKE fallback → recent rows.

## Memory Model

- `weight` (1-100), `reinforcement_count`, `is_long_term` (weight >= 10), `expires_at`, `last_accessed_at`
- Collections: `memories` (general), `session` (session facts), `lessons` (learned wisdom)
- Relevance: `(reinforcement_count * 2) + (weight * 1.5) + recency_bonus`

## File Watcher

File watcher runs as a **detached background process** — `mpm watch start` spawns a child process that persists independently of the terminal. The child writes its PID to `{MPM_DIR}/watch.pid` and blocks until signaled. Controlled via `mpm watch [start|stop|status]`:

- `watch start` — parent checks if watcher already running (PID file + signal 0 probe), spawns child with `--bg` flag, exits immediately. Child: writes PID, registers `SIGTERM`/`Interrupt` handler, blocks on `select{}`
- `watch stop` — reads PID, sends `os.Interrupt`, cleans up PID file
- `watch status` — reads PID, checks alive via signal 0, reports worker pool stats

Graceful shutdown: signal → drain worker pool → delete `watch.pid` → exit.

## Security

17 regex patterns block API keys/tokens/secrets before DB write. Blocked content → `mirror.jsonl`. Toxic phrase detection via `toxicphrases.txt`. Shred: DELETE + VACUUM (hard delete).

## Path Resolution

`MPM_WORKSPACE` env var → executable-relative → CWD. No hardcoded paths.