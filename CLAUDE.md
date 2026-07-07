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
go test -tags fts5 -v ./cmd/mpm/... -run TestFunctionName     # Runtime tests (main module)
cd internal/core && go test -tags fts5 -v ./... -run TestName # Core tests (separate module)
```

**The Core module is now standalone** — `internal/core/go.mod` defines `github.com/flowbyte-com/mpm-core`. The main `go.mod` uses a `replace` directive for local development. `make test` runs both modules; reach for `cd internal/core && go test ...` when you want Core in isolation (no watch, no synthesis, no idle_dream goroutines).

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
├── internal/core/            # **Separate Go module** — github.com/flowbyte-com/mpm-core
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
│   ├── web_db.go             # Web-API-specific query helpers
│   ├── core.go               # CoreDB interface (~130 methods) + compile-time assertion
│   ├── admission.go          # AdmitResult, AdmitChainEntry (exported)
│   └── go.mod                # Standalone module; main go.mod has replace directive
├── src/db/mpm.db             # Single canonical database (WAL mode)
├── mode/ persona/            # JSON / Markdown configs for behavioral modes
└── docs/                     # ARCHITECTURE_SPLIT.md (shipped retro), external-review-chatgpt-2026-06-28.md, WISHLIST.md
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
- `DatabaseManager` (`internal/core/db.go`) — the *only* connection pool. All writes go through it (often via `ExecTracked` for watchdog visibility). Enforces 5s `busy_timeout`, WAL, foreign keys.
- `MemoryStore` (`internal/core/memory.go`) — higher-level wrapper. Note: the 20-pattern secret/poison scanner (`isSensitiveContent` + `isPoisoned`) was pushed down into `SaveMemoryNode` in the 2026-07-07 security push, so **all** write paths go through it (not just `MemoryStore.AddMemory`). Coverage is enforced by the static-analysis test `TestScannerCoverage_AllMemoriesWritersScanContent`.
- `SynthesisWorker` (`internal/core/synthesis_isolation.go`) — isolated goroutine pool (`maxWorkers=3`) with its own event channel, semaphore, and DLQ for failed LLM synth attempts.
- `SSEBroker` (`cmd/mpm/stream.go`) — singleton; `call.go`, `handlers.go`, `watch.go` all broadcast to it. 50-event ring buffer for `Last-Event-ID` reconnect.

**Relevance scoring** (in `hybrid_search.go`):
`score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus`

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

## Database Schema

All tables are defined in `internal/core/schema.go` as `BaseTables` (DDL), `CommonIndexes` (indexes), and `SafeMigrations` (column additions for upgrade-in-place). The split exists so tests can construct an in-memory DB with just `BaseTables + CommonIndexes`.

Tables: `memories`, `sessions`, `topics`, `topic_memberships`, `lessons`, `system_config`, `raw_memories`, `external_db_cursors`, `reference_docs`, `reference_chunks`, `memory_revisions`.

FTS5 virtual tables are created in `db.go` init (not in `schema.go`) — they reference the `memories` table.

## Security — Read This Before Touching Write Paths

**The 2026-07-07 security audit (`audit.md`) is the source of truth for the current security posture.** It scored 90/100, closed 15 findings, and pushed the scanner down into a centralized location so coverage is structural rather than opt-in. The earlier `docs/security-review-2026-06-15.md` (referenced in pre-audit commits) is superseded by `audit.md`.

**Where the moat lives now (read these to understand the new guarantees):**

- **Scanner is in `SaveMemoryNode`** (`internal/core/memory.go`). Every write path goes through it. Coverage is enforced by `TestScannerCoverage_AllMemoriesWritersScanContent`. To add a new write path, you do not need to remember the scanner — it is structurally downstream.
- **Auth is fail-closed by default.** `authValid()` / `withAuth()` reject requests when `web_token` is empty. The `--allow-anonymous` flag is the explicit opt-in for local dev. The pre-audit "fail-open when token empty" footgun is gone.
- **HTTP body limit is 10 MiB** via `http.MaxBytesReader` on every body-reading handler (`cmd/mpm/web_handlers.go`). `http.Server` has `ReadTimeout: 30s`, `WriteTimeout: 30s`, `IdleTimeout: 60s`.
- **Browser security headers** are set by `withSecurityHeaders` middleware in `cmd/mpm/web.go`: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`, plus CORS `*` + `OPTIONS` preflight.
- **`mpm restore-db`** reads the SQL dump with `os.ReadFile` and runs it through `db.Exec(string(content))` on `mattn/go-sqlite3` directly. The pre-audit `exec.Command(sqlite3, ".read "+path)` shell-out (which would have honoured `.shell` directives inside a tampered dump) is gone.
- **Embedding probe cached via `sync.Once`.** `DefaultEmbeddingConfig()` no longer opens a new HTTP client on every CLI invocation — pre-audit, every `mpm add` / `mpm remember` / `mpm propose_theory` blocked up to 2s on the Ollama probe timeout.
- **`esc()` in `cmd/mpm/web/app.js`** escapes single quotes too. Future entity cards should use `addEventListener` + `textContent` rather than the legacy inline `onclick=` pattern (XSS-by-interpolation class).

**Open items the audit flagged but did not close (low-impact):**
- No CSRF protection on state-changing endpoints.
- `innerHTML` still used in some legacy render paths in `app.js` — escaped, but `textContent`-from-data would be safer.
- No `Content-Security-Policy` header yet.
- `CURRENT_TIMESTAMP` is still used for *setting* `deleted_at`; `strftime('%s','now')` is used for *comparing* against it (intentional, but worth documenting).

**Before adding a new external surface** (HTTP handler, CLI command, MCP tool, agent-plugins entry), add it to the audit by re-running the relevant section.

## Configuration

`mpm_config.json` in the workspace root. Loaded by `internal/core/config/config.go`. Contains:
- `memory_dirs`, `sessions_dirs` — watched paths
- `synth.{model, api_key, base_url, max_tokens, timeout_seconds}` — LLM config for synthesis
- `web_token` — bearer token for the web/SSE API (optional in current code → unauthenticated default)

The file is written 0600 by `SaveConfig` but the shipped sample ships with `0775` and a real `synth.api_key`. **Never commit a populated `mpm_config.json`.** Only `mpm_config.json.example` (template) is safe in git.

## Path Resolution

`MPM_WORKSPACE` env var → `$HOME/.openclaw/workspace/projects/mpm` (compile-time default in `main.go:40`) → CWD fallback. No hardcoded paths.

## Threading & Resource Limits

`main.go:36` caps `runtime.GOMAXPROCS(32)` at process init. This is deliberate — the default (unlimited) can spawn hundreds of OS threads on a many-core machine and OOM the host. Don't lower it without measuring; don't raise it.

## Testing Notes

- `internal/core/*_test.go` — fast, table-driven, mostly in-memory. Use these for unit changes.
- `cmd/mpm/*_test.go` — integration tests against a temp database. Slower; some (e.g. `watch_lifecycle_test.go`, `recall_test.go`) spawn real goroutines.
- `reliability_sprint_test.go`, `lifecycle_decay_test.go`, `synthesis_isolation_test.go` — exercise the failure paths (DLQ overflow, SQLite BUSY races, weight-floor split-brain). Read these when changing concurrency or persistence semantics.
- Tests assume FTS5 is compiled in. CI must export `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5` or tests will panic.
- Core tests run from `internal/core/` (separate Go module); runtime tests from repo root, using the `replace` directive in `go.mod`.
- `scripts/smoke_shared.sh` — hermetic integration script for the multi-agent shared-epistemology federated path. Boots an isolated MPM_WORKSPACE + MPM_SHARED_DB, seeds 5 sentinels via `record_global_rule`, asserts that scope=all returns hits without manual FTS backfill. Pre-2026-07-07 this path was silently broken for fresh shared DBs (lazy-backfill design only fired for QueryGlobalRules; HybridSearch bypassed it). The 4 shared_memories_* triggers (in `internal/core/db.go attachShared`) are the structural fix.

## Shared DB invariants (2026-07-07)

- **`shared.memories_fts` is auto-synced by triggers**, not by call-site discipline. Do not add manual `INSERT INTO shared.memories_fts SELECT ...` to write paths — the triggers fire for free, and a manual backfill will double-add rows.
- The FTS table is **standalone FTS5** (no `content=` option), with `porter unicode61` tokenizer matching local `memories_fts`. Plain `DELETE FROM memories_fts WHERE rowid = old.rowid` works correctly. A previous "contentless" design (content='memories') required the FTS5 `'delete'` special command, which was incompatible with the trigger-in-shared-DB pattern. Don't re-introduce the `content=` option without reworking the trigger design.
- **`VectorMatch` has a circuit breaker** (`MPM_MAX_VECTOR_SCAN`, default 5000). Above the threshold it errors instead of OOM. Set to 0 to disable. The real fix is an ANN index — see WISHLIST.md.
- **HybridSearch `scope=all` with multi-token queries against small corpora can return 0 hits** because the per-token BM25 scores compete and the Shared Premium multiplier doesn't lift them above the retrieval threshold. This is standard IDF behaviour, not a bug; for testing prefer single-token queries. The 2026-07-07 smoke script uses `query: "rule"` for exactly this reason.

## Gotchas

- **Single shared connection.** All goroutines go through one `*sql.DB` with `SetMaxOpenConns(1)`-ish behavior. Don't `sql.Open` new connections inside hot paths — use `DatabaseManager`. The hostile audit (memory: `mpm-hostile-audit-2026-06-03`) had a bug from `RunLifecycleDecayAndArchival` opening a new connection per tick and exhausting the WAL pool.

  **Enforced by** `internal/core/sqlopen_owner_test.go`: a static-analysis test that fails if `sql.Open` appears outside the whitelist (db.go for the main connection, adapters.go/ingest.go for foreign sqlite files, main.go/route_render.go/handlers_backup.go for read-only opens). Add a new call site only with a justifying comment in the whitelist.
- **Watch daemon** is detached: `mpm watch start --bg` spawns a child that `select{}`s on signals. Parent exits immediately. PID file is `watch.pid` in the workspace.
- **SSE broker** is a package-level singleton. To broadcast a new event type, add a `Broadcast(...)` helper in `stream.go` — don't instantiate your own broker.
- **Frontend (`cmd/mpm/web/app.js`)** uses inline `onclick=` attributes and string-concat HTML rendering. If you add new entity types (cards/menus), prefer `addEventListener` + `textContent` from the start; the audit flagged this as a future-XSS hazard.
- **Memory scoring uses `reinforcement_count` and `weight` independently** — bumping one doesn't bump the other. `mpm reinforce` and `mpm set-weight` are separate commands for a reason.
- **`call.go`** is the universal machine interface. Adding a new tool? Register it in `call.go` so other processes (mpm-agent, OpenClaw, hermes, opencode) can invoke it via `mpm call <name> --payload <json>`.
