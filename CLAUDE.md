# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**MPM** (Memory Persistence Module) is a SQLite-native memory and reasoning infrastructure for autonomous AI agents. It persists not just facts, but the *reasoning* behind them — decisions, theories, evidence, and lessons — so agents can continue building on prior knowledge rather than re-discovering conclusions.

Core capabilities (the "Epistemology Engine"):
- **Memories** — weighted, searchable facts with reinforcement and decay
- **Decisions** — architectural choices with context, choice, and rationale
- **Theories** — hypotheses with explicit validation status (pending/confirmed/disproven)
- **Lessons** — reusable knowledge that survives across tasks
- **Sessions** — operational context for resuming work
- **Challenges** — workflow for self-correcting stale knowledge

Everything lives in a single SQLite database (`src/db/mpm.db`) — no server, no daemon, no external services. The binary is the database.

## Build & Test

```bash
make build                          # Build to bin/mpm
make test                           # Run all Go tests (verbose, race-detector on)
make install BIN=mpm                # Install to /usr/local/bin/mpm
```

**Build requirements:** CGO with FTS5 enabled. The Makefile sets `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1` and uses `-tags fts5` for `mattn/go-sqlite3`. If invoking `go` directly, mirror those flags or FTS5 queries fail silently.

**Single test:**
```bash
go test -tags fts5 -v ./internal/... -run TestFunctionName
```

**Watch daemon test suite (slow):**
```bash
go test -tags fts5 -v ./cmd/mpm/... -run TestWatch
```

## Repository Layout

```
mpm/
├── cmd/mpm/                  # CLI entry, command router, handlers
│   ├── main.go               # Entry point; caps GOMAXPROCS at 32
│   ├── router.go             # Command → handler registry (~80 commands)
│   ├── handlers*.go          # Per-command handler implementations
│   ├── call.go               # `mpm call <tool> --payload <json>` universal machine interface
│   ├── web.go / web_handlers.go   # HTTP server + REST API
│   ├── stream.go             # SSE broker (live telemetry)
│   ├── watch.go              # fsnotify-based file ingestion daemon
│   ├── synthesize_cmds.go    # Memory dedup via LLM synthesis
│   ├── handlers_backup.go    # backup/restore-db (⚠ see Security section)
│   └── web/                  # Static frontend (app.js, index.html, style.css)
├── internal/                 # Core packages (no external consumers)
│   ├── db.go                 # DatabaseManager — single shared SQLite conn (WAL)
│   ├── memory.go             # MemoryStore, Memory struct, secret/poison scanner
│   ├── schema.go             # BaseTables, CommonIndexes, SafeMigrations
│   ├── search.go             # FTS5 query interface
│   ├── hybrid_search.go      # BM25 + semantic + reinforcement scoring
│   ├── synthesis_isolation.go  # SynthesisWorker — goroutine pool, DLQ
│   ├── dlq.go                # Dead-letter queue for failed synth attempts
│   ├── ingest.go             # External-Source → raw_memories pipeline
│   ├── idle_dream.go         # Background consolidation worker
│   ├── lessons.go            # Lesson CRUD
│   ├── reference_new.go      # Reference docs (PDF/EPUB/MD parsing)
│   ├── embeddings.go         # Embedding provider abstraction
│   ├── versioning.go         # Memory revisions (point-in-time reconstruction)
│   └── web_db.go             # Web-API-specific query helpers
├── src/db/mpm.db             # Single canonical database (WAL mode)
├── mode/ persona/            # JSON / Markdown configs for behavioral modes
└── docs/                     # security-review, SSE_TELEMETRY, MPM_WISHLIST
```

## Sibling projects (not under mpm/)

- **`../mpm-agent/`** — agent shell: `mini-bot` (CLI REPL), `mini-bot-telegram`, `mini-bot-mcp`. Separate Go module with its own `go.mod`, `mini-bot.db`, and `mini-bot-config.json`. Lives at github.com/flowbyte-com/mpm-agent. Build separately; it is *not* part of the main `mpm` binary, and it is *not* a plugin under `agent-plugins/`. The three agent-plugins/ entries (hermes, openclaw, opencode) are LLM/chat integrations that *call into* mpm; mpm-agent is a different kind of consumer — a self-contained agent that *uses* mpm as its memory store.

## Core Architecture

**Single-process, shared-database model.** No socket IPC, no separate daemon. Commands execute in the same process as the file watcher. Background goroutines (synthesis, idle dream, lifecycle decay) share the single `DatabaseManager` connection.

```
┌─────────────────────────────────────────────────┐
│                    mpm binary                   │
│  CLI ──▶ router ──▶ handler ──┐                 │
│                               │                 │
│  background goroutines:       │                 │
│    • fsnotify watch           ▼                 │
│    • SynthesisWorker ──▶ DatabaseManager        │
│    • idle_dream        (single SQLite conn,     │
│    • lifecycle decay    WAL, FTS5, busy_timeout)│
│                               │                 │
│  web server (:18792)  ───────┘                 │
└─────────────────────────────┬───────────────────┘
                              ▼
                       src/db/mpm.db
```

**Key types:**
- `DatabaseManager` (`internal/db.go`) — the *only* connection pool. All writes go through it (often via `ExecTracked` for watchdog visibility). Enforces 5s `busy_timeout`, WAL, foreign keys.
- `MemoryStore` (`internal/memory.go`) — higher-level wrapper. **This is the only code path that runs the 20-pattern secret/poison regex check.** Lower-level `DatabaseManager.SaveMemory` skips it.
- `SynthesisWorker` (`internal/synthesis_isolation.go`) — isolated goroutine pool (`maxWorkers=3`) with its own event channel, semaphore, and DLQ for failed LLM synth attempts.
- `SSEBroker` (`cmd/mpm/stream.go`) — singleton; `call.go`, `handlers.go`, `watch.go` all broadcast to it. 50-event ring buffer for `Last-Event-ID` reconnect.

**Relevance scoring** (in `hybrid_search.go`):
`score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus`

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

## Database Schema

All tables are defined in `internal/schema.go` as `BaseTables` (DDL), `CommonIndexes` (indexes), and `SafeMigrations` (column additions for upgrade-in-place). The split exists so tests can construct an in-memory DB with just `BaseTables + CommonIndexes`.

Tables: `memories`, `sessions`, `topics`, `topic_memberships`, `lessons`, `system_config`, `raw_memories`, `external_db_cursors`, `reference_docs`, `reference_chunks`, `memory_revisions`.

FTS5 virtual tables are created in `db.go` init (not in `schema.go`) — they reference the `memories` table.

## Security — Read This Before Touching Write Paths

The 20-pattern secret/poison scanner (`isSensitiveContent` + `isPoisoned` in `internal/memory.go`) is **only invoked from `MemoryStore.AddMemory`**. Most other write paths call `DatabaseManager.SaveMemory` directly and **bypass the scan entirely**. See `docs/security-review-2026-06-15.md` for the full audit (24 findings).

**Before adding a new write path, route it through `MemoryStore.AddMemory`, or push the scanner down into `DatabaseManager.SaveMemory`.** The known bypass call sites:
- `cmd/mpm/watch.go:863, 889, 958` (watch daemon ingest)
- `cmd/mpm/web_handlers.go:97` (web `POST /api/memories`)
- `cmd/mpm/simple_cmds.go:97` (`mpm remember`)
- `cmd/mpm/handlers.go:4633` (`record_decision`)
- `internal/idle_dream.go:456`, `internal/synthesis_isolation.go:463`, `internal/synthesize.go:631`

**Other known-bad patterns to avoid:**
- `authValid()` in `cmd/mpm/stream.go:160` and `withAuth()` in `cmd/mpm/web.go:132` **fail open** when `web_token` is empty (the default). Combined with the bypass above, default `mpm web` on a LAN is fully unauthenticated. Fix in flight per the audit remediation plan.
- `mpm call add_reference --payload '{"filepath":"…"}'` reads any file the mpm user can read with no allow-list or size cap (`cmd/mpm/call.go:694-721`).
- `mpm restore-db` pipes a dump to `sqlite3` via stdin — a tampered `.sql` with a leading `.shell` line gets executed as a shell command. Use `mattn/go-sqlite3` `db.Exec(string(content))` instead (`cmd/mpm/handlers_backup.go:166-180`).
- HTTP server has no timeouts, no body size cap, no security headers, CORS `*` on SSE. See audit items #4, #14, #15, #16.
- `cmd/mpm/web/app.js` builds HTML via string concat; some `onclick=` interpolations of `m.id` skip the `q()` escape. Future caller-controlled IDs become stored XSS.

## Configuration

`mpm_config.json` in the workspace root. Loaded by `internal/config/config.go`. Contains:
- `memory_dirs`, `sessions_dirs` — watched paths
- `synth.{model, api_key, base_url, max_tokens, timeout_seconds}` — LLM config for synthesis
- `web_token` — bearer token for the web/SSE API (optional in current code → unauthenticated default)

The file is written 0600 by `SaveConfig` but the shipped sample ships with `0775` and a real `synth.api_key`. **Never commit a populated `mpm_config.json`.** Only `mpm_config.json.example` (template) is safe in git.

## Path Resolution

`MPM_WORKSPACE` env var → `$HOME/.openclaw/workspace/projects/mpm` (compile-time default in `main.go:40`) → CWD fallback. No hardcoded paths.

## Threading & Resource Limits

`main.go:36` caps `runtime.GOMAXPROCS(32)` at process init. This is deliberate — the default (unlimited) can spawn hundreds of OS threads on a many-core machine and OOM the host. Don't lower it without measuring; don't raise it.

## Testing Notes

- `internal/*_test.go` — fast, table-driven, mostly in-memory. Use these for unit changes.
- `cmd/mpm/*_test.go` — integration tests against a temp database. Slower; some (e.g. `watch_lifecycle_test.go`, `recall_test.go`) spawn real goroutines.
- `reliability_sprint_test.go`, `lifecycle_decay_test.go`, `synthesis_isolation_test.go` — exercise the failure paths (DLQ overflow, SQLite BUSY races, weight-floor split-brain). Read these when changing concurrency or persistence semantics.
- Tests assume FTS5 is compiled in. CI must export `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5` or tests will panic.

## Gotchas

- **Single shared connection.** All goroutines go through one `*sql.DB` with `SetMaxOpenConns(1)`-ish behavior. Don't `sql.Open` new connections inside hot paths — use `DatabaseManager`. The hostile audit (memory: `mpm-hostile-audit-2026-06-03`) had a bug from `RunLifecycleDecayAndArchival` opening a new connection per tick and exhausting the WAL pool.

  **Enforced by** `internal/sqlopen_owner_test.go`: a static-analysis test that fails if `sql.Open` appears outside the whitelist (db.go for the main connection, adapters.go/ingest.go for foreign sqlite files, main.go/route_render.go/handlers_backup.go for read-only opens). Add a new call site only with a justifying comment in the whitelist.
- **Watch daemon** is detached: `mpm watch start --bg` spawns a child that `select{}`s on signals. Parent exits immediately. PID file is `watch.pid` in the workspace.
- **SSE broker** is a package-level singleton. To broadcast a new event type, add a `Broadcast(...)` helper in `stream.go` — don't instantiate your own broker.
- **Frontend (`cmd/mpm/web/app.js`)** uses inline `onclick=` attributes and string-concat HTML rendering. If you add new entity types (cards/menus), prefer `addEventListener` + `textContent` from the start; the audit flagged this as a future-XSS hazard.
- **Memory scoring uses `reinforcement_count` and `weight` independently** — bumping one doesn't bump the other. `mpm reinforce` and `mpm set-weight` are separate commands for a reason.
- **`call.go`** is the universal machine interface. Adding a new tool? Register it in `call.go` so other processes (mpm-agent, OpenClaw, hermes, opencode) can invoke it via `mpm call <name> --payload <json>`.
