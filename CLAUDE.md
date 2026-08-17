# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**MPM** is an observable substrate for long-lived autonomous systems. Persistent memory is just one observable. Capabilities, decisions, theories, and execution telemetry are all governed by the same self-observing foundation.

The substrate persists not just facts, but the *reasoning* behind them — so agents can continue building on prior knowledge rather than re-discovering conclusions.

Core capabilities (the "Epistemology Engine"):
- **Memories** — weighted, searchable facts with reinforcement and decay
- **Decisions** — architectural choices with context, choice, and rationale
- **Theories** — hypotheses with explicit validation status (pending/confirmed/disproven)
- **Lessons** — reusable knowledge that survives across tasks
- **Sessions** — operational context for resuming work
- **Challenges** — workflow for self-correcting stale knowledge

Everything lives in a single SQLite database (`src/db/mpm.db`). `mpm` is a headless daemon — a hardened SQLite data plane with an autonomous 03:00 UTC diagnostic critic and an MCP server (`mpm-mcp`) for machine-to-machine integration. No external services, no vector database.

### The Projection Test

Before adding any new table, column, cache, score, or summary, apply this test:

1. Can this be computed from authoritative state at read time?
2. Will read latency be acceptable at expected rates?

If **yes** to both: Do not persist it. Compute at read.

If **no**: The burden of proof is on persistence. Explain why persistence is necessary and what guarantees it won't drift.

*Rule: A derived value may only be persisted if it is updated atomically with every authoritative mutation. Otherwise, it must be computed on read.*

*(Note: Database indexes are not persistence of state. Add them freely.)*

This is the operational form of the [Projection Principle](docs/architecture.md). Both say the same thing; the Test is what you apply at PR review, the Principle is what the architecture is built on.

## Build & Test

```bash
make build                          # Build to bin/mpm
make test                           # Run all Go tests (verbose, race-detector on)
make install                        # Install to $HOME/.mpm/bin/ (canonical; no sudo)
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
│   ├── synthesize_cmds.go    # Memory dedup via LLM synthesis
│   └── handlers_backup.go    # backup/restore-db (⚠ see Security section)
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
│   ├── web_db.go             # Shared query helpers (CLI + MCP consumers)
│   ├── core.go               # CoreDB interface (~180 methods) + compile-time assertion
│   ├── admission.go          # AdmitResult, AdmitChainEntry (exported)
│   └── go.mod                # Standalone module; main go.mod has replace directive
├── src/db/mpm.db             # Single canonical database (WAL mode)
├── mode/ persona/            # JSON / Markdown configs for behavioral modes
└── docs/                     # ARCHITECTURE_SPLIT.md (shipped retro), WISHLIST.md
```

## Sibling projects (not under mpm/)

- **`../mpm-agent/`** — agent shell: `mini-bot` (CLI REPL), `mini-bot-telegram`, `mini-bot-mcp`. Separate Go module with its own `go.mod`, `mini-bot.db`, and `mini-bot-config.json`. Lives at github.com/flowbyte-com/mpm-agent. Build separately; it is *not* part of the main `mpm` binary, and it is *not* a plugin under `agent-plugins/`. The three agent-plugins/ entries (hermes, openclaw, opencode) are LLM/chat integrations that *call into* mpm; mpm-agent is a different kind of consumer — a self-contained agent that *uses* mpm as its memory store.

## Core Architecture

**Single-process, shared-database model.** No socket IPC, no separate daemon. Commands execute in the same process as the background goroutines (synthesis, idle dream, lifecycle decay) that share the single `DatabaseManager` connection. The watch daemon was deprecated in commit `6588cb8` and hard-removed in `215fd09` — file ingestion now flows through `mpm cascade materialize` invoked from cron / systemd.

```
┌─────────────────────────────────────────────────┐
│                    mpm binary                   │
│  CLI ──▶ router ──▶ handler ──┐                 │
│                               │                 │
│  background goroutines:       │                 │
│    • SynthesisWorker ──▶ DatabaseManager        │
│    • idle_dream        (single SQLite conn,     │
│    • lifecycle decay    WAL, FTS5, busy_timeout)│
└─────────────────────────────┬───────────────────┘
                              ▼
                       src/db/mpm.db
```

**Key types:**
- `DatabaseManager` (`internal/core/db.go`) — the *only* connection pool. All writes go through it (often via `ExecTracked` for watchdog visibility). Enforces 5s `busy_timeout`, WAL, foreign keys.
- `MemoryStore` (`internal/core/memory.go`) — higher-level wrapper. Note: the 19-pattern secret/poison scanner (`isSensitiveContent` + `isPoisoned`) was pushed down into `SaveMemoryNode` in the 2026-07-07 security push, so **all** write paths go through it (not just `MemoryStore.AddMemory`). Coverage is enforced by the static-analysis test `TestScannerCoverage_AllMemoriesWritersScanContent`.
- `SynthesisWorker` (`internal/core/synthesis_isolation.go`) — isolated goroutine pool (`maxWorkers=3`) with its own event channel, semaphore, and DLQ for failed LLM synth attempts.

**Relevance scoring** (in `hybrid_search.go`):
`score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus`

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

## Database Schema

All tables are defined in `internal/core/schema.go` as `BaseTables` (DDL), `CommonIndexes` (indexes), and `SafeMigrations` (column additions for upgrade-in-place). The split exists so tests can construct an in-memory DB with just `BaseTables + CommonIndexes`.

Tables: `memories`, `sessions`, `topics`, `topic_memberships`, `lessons`, `system_config`, `raw_memories`, `external_db_cursors`, `reference_docs`, `reference_chunks`, `memory_revisions`.

FTS5 virtual tables are created in `db.go` init (not in `schema.go`) — they reference the `memories` table.

## Security — Read This Before Touching Write Paths

**The 2026-07-07 security push is the source of truth for the current security posture.** It scored 90/100, closed 15 findings, and pushed the scanner down into a centralized location so coverage is structural rather than opt-in.

**Where the moat lives now (read these to understand the new guarantees):**

- **Scanner is in `SaveMemoryNode`** (`internal/core/memory.go`). Every write path goes through it. Coverage is enforced by `TestScannerCoverage_AllMemoriesWritersScanContent`. To add a new write path, you do not need to remember the scanner — it is structurally downstream.
- **No HTTP server in mpm.** The compiled binary is CLI-only; HTTP/MCP surfaces belong to consumer binaries (`mpm-agent`, future `mpm-mcp`, etc.) and they implement their own auth and transport limits at the protocol boundary. Do not add HTTP handlers, body-size limits, or auth middleware here.
- **`mpm restore-db`** reads the SQL dump with `os.ReadFile` and runs it through `db.Exec(string(content))` on `mattn/go-sqlite3` directly. The pre-audit `exec.Command(sqlite3, ".read "+path)` shell-out (which would have honoured `.shell` directives inside a tampered dump) is gone.
- **Embedding probe cached via `sync.Once`.** `DefaultEmbeddingConfig()` no longer opens a new HTTP client on every CLI invocation — pre-audit, every `mpm add` / `mpm remember` / `mpm propose_theory` blocked up to 2s on the Ollama probe timeout.

**Open items the audit flagged but did not close (low-impact):**
- All timestamp columns are INTEGER Unix-epoch seconds (unified by `timestamps_unified_v1` migration). The `deleted_at_unified_v1` precedent no longer applies separately — both migrations wrap in the same `DatabaseManager.init` transaction. **Operators must `mpm backup-db` before installing this release.**

**Before adding a new external surface** (CLI command, MCP tool, agent-plugins entry), add it to the audit by re-running the relevant section.

## Configuration

The runtime config (`mpm_config.json` in `$MPM_WORKSPACE`) is loaded by `internal/core/config/config.go` and written 0600 by `SaveConfig`. Contains:
- `memory_dirs`, `sessions_dirs` — watched paths
- `synth.{model, api_key, base_url, max_tokens, timeout_seconds}` — LLM config for synthesis

Only `mpm_config.json.example` (template) is tracked in git. Populated runtime configs are blocked by `.gitignore` (`**/mpm_config.json`) so accidental commits are caught before review.

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

## Substrate Defense Triad (2026-08-17)

Three mandatory patterns surfaced during the pre-alpha hardening arc (Days 1–5, 2026-08-13 → 2026-08-17). Each addresses a class of silent-promotion bug that mock-schema unit tests passed against but live operations broke — schema validation cascades (Day 1–2), NULL scan panics on empty aggregates (Day 3), orphaned state-file writers (Day 4), and multi-agent write contention (Day 5). All three are now load-bearing in production; adding new write paths or cross-process bridges without honoring them is a pre-commit-defeating offense.

The mpm-lint --gate enforces the structural version of these (zero `ctx-in-scope-missing`, zero `no-close`, zero `rows-discarded`). The semantic version — the patterns below — is enforced by review. If you find yourself reaching for a different shape, the right move is to extend the lint gate, not the runtime tolerance.

### 1. Atomic State Swap (cross-process bridges)

**Invariant.** A shared state file (`scheduler.state`, lock sentinels, snapshot manifests, etc.) is read by at least two processes simultaneously: the writer (daemon / scheduler tick) and one or more readers (CLI invocations, agent plugins). A direct write to the target path creates an observable window where readers ingest a truncated or empty payload mid-write.

**Rule.** Marshal to a sibling `<target>.tmp` file in the same filesystem directory, then commit via `os.Rename`. `rename(2)` is atomic on POSIX; readers observe either the previous valid state or the new valid state, never a partial buffer. Failures during the write or rename phase must clean up the dangling `.tmp` so the next tick doesn't inherit a stale buffer.

**Canonical implementation.** `internal/scheduler/state.go:persistState` (the cross-process heartbeat written every tick):

```go
// internal/scheduler/state.go
func (s *Scheduler) persistState() {
    data, err := json.Marshal(schedulerState{ ... })
    if err != nil { s.log.Warn("state marshal failed", "err", err); return }

    target := StateFilePath()
    if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil { ... }
    tmp := target + ".tmp"
    if err := os.WriteFile(tmp, data, 0o600); err != nil { ... }
    if err := os.Rename(tmp, target); err != nil {
        _ = os.Remove(tmp)
        s.log.Warn("state rename failed", "err", err)
        return
    }
}
```

**When to apply.** Any file written by one process and read by another in the same tick window. Direct `os.WriteFile` / `O_TRUNC` is forbidden for state files even if the write amp is negligible (it's the race window that matters, not the bytes).

### 2. Defensive SQL Aggregates (NULL safety)

**Invariant.** SQL standard semantics return `0` for `COUNT(*)` over an empty set but return `NULL` for `MAX()`, `MIN()`, `AVG()`, and most custom aggregate expressions. Scanning a database `NULL` directly into a concrete Go primitive (`string`, `int64`, `float64`) is a runtime scan failure with the message `converting NULL to <type> is unsupported`. The 2026-08-13 silent-continue → hard-fail conversion exposed this class across the codebase; it is now a permanent hazard.

**Rule.** Every aggregate query must either (a) wrap target columns in `COALESCE(AGG(...), fallback)` at the SQL boundary or (b) bind to native `sql.Null*` types during row scanning. For nullable scalar columns on standard tables (not aggregates, but the same failure class), use `sql.NullString` / `sql.NullInt64` rather than concrete types.

**Canonical implementation.** `cmd/mpm/handlers_status.go:getSynthesisStats` (the fix shipped on Day 4, 2026-08-17):

```go
// CORRECT: SQL-side fallback prevents the Scan panic on empty result sets.
if err := dm.SQLDB().QueryRow(`
    SELECT COUNT(*), COALESCE(MAX(json_extract(metadata, '$.synthesized_at')), '')
    FROM memories
    WHERE deleted_at IS NULL
    AND json_extract(metadata, '$.synthesized') = 'true'
`).Scan(&count, &lastTime); err != nil {
    usererror.Warn("getSynthesisStats: ...: %v", err)
}
if lastTime == "" { lastTime = "never" }
```

For nullable scalars, use the `sql.Null*` types rather than empty-string coercion:

```go
type Lesson struct {
    ID              string         `json:"id"`
    SourceSessionID sql.NullString `json:"source_session_id"` // null in JSON, not ""
}

// Convert empty strings to true database NULLs before insert.
func sourceSessionIDOrNil(s string) interface{} {
    if strings.TrimSpace(s) == "" { return nil }
    return s
}
```

**When to apply.** Any new aggregate query, plus any nullable scalar column where the empty-string sentinel could collide with legitimate values. The mpm-lint --gate does not yet audit this class — review is the enforcement layer for now.

### 3. Write-Path Read-Back Assertions (persistence guarantees)

**Invariant.** An `INSERT` or `UPDATE` returning `nil` from `db.Exec()` does not prove persistence — the row may have been silently dropped by a CHECK constraint, an `INSTEAD OF` trigger that swallowed it, or a transaction that rolled back after the statement returned. Worse, tables backed by SQLite views with `INSTEAD OF` triggers legitimately report `RowsAffected() == 0` on successful execution (the trigger fires its own INSERT internally; the outer statement counts zero affected rows). RowsAffected is unreliable alone.

**Rule.** Every write operation that is not inside an active transaction must perform an immediate read-back assertion (`GetX(id)`) before returning the success payload. Combine with a view-aware `RowsAffected` check when the table might be a view. The read-back is the load-bearing assertion; the RowsAffected check is a fast-path early bail.

**Canonical implementation.** `internal/core/db.go:AddLesson` (the existing pattern that all new write paths must match):

```go
res, err := dm.db.Exec(`INSERT INTO lessons (...) VALUES (...)`, ...)
if err != nil { return nil, fmt.Errorf("insert lesson: %w", err) }

// View-aware RowsAffected: views with INSTEAD OF triggers legitimately
// report 0 on success. Only treat zero rows as an error on direct tables.
if _, err := res.RowsAffected(); err != nil {
    if !dm.lessonsIsView() {
        return nil, fmt.Errorf("insert rows-affected: %w", err)
    }
}

// MANDATORY: read-back assertion proves persistence to the substrate.
persisted, err := dm.GetLesson(id)
if err != nil {
    return nil, fmt.Errorf("write verification failed for %s: %w", id, err)
}
return persisted, nil
```

**When to apply.** Every new `AddX` / `SetX` / `UpdateX` write path in `internal/core/db.go` and `internal/core/<store>.go`. The `mpm_lessons save` and `mpm_decisions record` paths were the two silent-promotion-by-edge-case bugs the 2026-08-13 hardening surfaced; both now follow this rule. Future writers (memory revisions, evidence ledger writes, theory state transitions) must match. The audit verification `cmd/mpm/call_evidence_test.go` and the test_evidence scan (`internal/audit/`) are the structural reinforcement — review the existing `AddLesson` first when adding any new writer.

## Gotchas

- **Single shared connection pool.** All goroutines go through one `*sql.DB`. Don't `sql.Open` new connections inside hot paths — use `DatabaseManager`. The hostile audit (memory: `mpm-hostile-audit-2026-06-03`) had a bug from `RunLifecycleDecayAndArchival` opening a new connection per tick and exhausting the WAL pool.

  ### H-5: Why `SetMaxOpenConns(1)` is intentionally NOT applied

  SQLite's locking model allows many concurrent readers but only one writer at a time. The 2026-08-14 pre-alpha audit proposed `db.SetMaxOpenConns(1)` to force every read and write through the same connection — the single-threaded mental model that maps cleanly onto SQLite's single-writer guarantee. **The fix was tried and reverted.**

  **The architectural intent.** With `MaxOpenConns(1)`, every `db.Query` / `db.Exec` blocks until the lone connection is free, which would have serialized all SQLite traffic and matched the documentation's "strict bottleneck" claim. It looked like the cleanest way to enforce the invariant.

  **The reality.** The codebase never called `SetMaxOpenConns(1)`. By default `database/sql` leaves max open connections unbounded, so the driver quietly opens multiple connections. The original single-connection guarantee was always *single-connection-via-discipline*, not single-connection-via-runtime-choke-point.

  **The deadlock trap.** Adding `db.SetMaxOpenConns(1)` to a Go application that uses transactions creates a textbook deadlock:

  1. Code calls `tx, _ := db.Begin()`. Go checks out the only available connection and binds it to `tx`.
  2. Inside the transaction, a helper function executes a quick lookup via the bare `db` instance — `db.QueryRow(...)` — instead of passing `tx` through.
  3. `db.QueryRow` tries to acquire a connection from the pool to run the query.
  4. Pool is empty (`MaxOpenConns(1)` is fully occupied by step 1). The query blocks waiting for the connection.
  5. The transaction cannot commit or roll back because the helper hasn't returned. **Deadlock.**

  This is not a hypothetical — `mpm-mcp` and the synthesis workers pass `*sql.DB` into helpers that run auxiliary queries, and the call graph was not designed around passing `*sql.Tx` everywhere. We hit this on the first test run after applying the fix; the test stack showed `database/sql.(*DB).connectionOpener` blocked while another goroutine held a transaction. The revert is in commit `e748400` (or earlier; see the explanatory comment block at `internal/core/db.go:NewDatabaseManager`).

  **How MPM resolves it instead.** Three safe mechanisms, each load-bearing:

  - **WAL mode** — concurrent readers + queued writers; no application-level bottleneck needed.
  - **`busy_timeout=5000`** — SQLite waits up to 5s for a lock to clear instead of instantly returning `SQLITE_BUSY`. Absorbs brief contention without surfacing an error.
  - **Code discipline** — proper transaction scoping (`*sql.Tx` threaded through helpers, not bare `*sql.DB` lookups inside transactions) plus `ExecTracked`/`QueryTracked` retry-backoff on the write path.

  Anyone proposing `SetMaxOpenConns(1)` in a future PR must (a) read the deadlock trap section above and (b) refactor every helper that takes `*sql.DB` and runs queries inside an enclosing transaction to take `*sql.Tx` instead. That refactor is invasive and out of scope for the alpha drop.

  **Enforced by** `internal/core/sqlopen_owner_test.go`: a static-analysis test that fails if `sql.Open` appears outside the whitelist (db.go for the main connection, adapters.go/ingest.go for foreign sqlite files, main.go/route_render.go/handlers_backup.go for read-only opens). Add a new call site only with a justifying comment in the whitelist.
- **Watch daemon** was deprecated in commit `6588cb8` and hard-removed in `215fd09`. File ingestion now flows through `mpm cascade materialize` (operators invoke from cron / systemd) — see `README.md` §5 for the rationale. Do not reintroduce a watch daemon without first reading the deprecation rationale and confirming the architectural intent has changed.
- **Memory scoring uses `reinforcement_count` and `weight` independently** — bumping one doesn't bump the other. `mpm reinforce` and `mpm set-weight` are separate commands for a reason.
- **`call.go`** is the universal machine interface. Adding a new tool? Register it in `call.go` so other processes (mpm-agent, OpenClaw, hermes, opencode) can invoke it via `mpm call <name> --payload <json>`.
