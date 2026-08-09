# Changelog

## 2026-08-09 — mpm-alpha: Epistemic Cascades, Universal Scheduler, Provenance

### Epistemic Cascades (#2, #5, #6)
- **feat(core): dependency-aware invalidation** — `epistemic_cascade_outbox` + `MaterializeCascadeIntents`; cascades propagate invalidations transitively (max depth 3, max retries 3) so shredding a foundational directive re-materializes dependents
- **refactor(core): stateless materializer** — dropped the goroutine-pool lifecycle (`Start`/`Stop`/`<-stopC`) in favor of a pure `NewCascadeMaterializer` factory + single-batch `MaterializeBatch`; tests moved to a state-machine model
- **feat(cli): `mpm cascade materialize`** — out-of-band drain with flock lockfile (`cascade.lock`) to prevent concurrent drains; `mpm cascade list-dead-letters` for dead-letter inspection
- **feat(scheduler): `cascade_drain` tick handler** — per-tick yield budget (30s time budget, batch size from `system_config.cascade_drain.max_intents_per_tick`, default 50) with yield-reason taxonomy (`queue_empty` / `budget_exhausted` / `context_cancelled` / `error`); panic-safety wrapper; audit row per batch; `cascade_summary` wake kind with idle-tick dedupe
- Docs: runbook for scheduler-driven cascade drain; adaptive-yield specification (stair-step omitted per design review)

### Universal Scheduler (`mpm-scheduler`)
- New binary: universal wake executor with flock singleton (`scheduler.lock`), 60s ticker, wake dispatch (`snapshot`, `critic_audit`, `gc`, `broadcast`, `cascade_summary`, `cascade_drain`)
- `RegisterTickHandler` for unconditional per-tick work; tick handlers run sequentially after wake dispatch so system wakes always fire on cadence
- Agentic Cron: `scheduled_tasks` polled each tick; `next_run_at` rollover + wake injection in one transaction (crash-safe)

### Artifact Provenance (alpha telemetry)
- `artifact_provenance` table + analytics views (`v_model_memory_yield`, …); SAVEPOINT-isolated writer with validation; env-var resolver with priority chain
- Hooks on `saveMemoryRow` / `AddLesson`; cascade materializer threads `parent_artifact_id`; CLI surface via `mpm provenance`
- Legacy `metadata.provenance` JSON injection removed

### Capability System
- CS-3 `mpm capability grant-operator` shipped — operator bootstrapping sequence complete

### mpm-lint AST Engine & Audit Gates
- Unified `mpm-lint` AST engine replacing the legacy per-audit binaries; gates for transactions, contexts, goroutines, mutex, sql, file descriptors, imports (architectural boundary rule), and scan-error handling
- Wired into pre-commit (consolidated gate run) and GitHub Actions (violations as PR annotations, `--gate` in build-test job)
- `scripts/stranger-test.sh` added as a permanent release gate; 67/68 silent-continue error sites hardened

### Database Reliability
- Foreign keys enforced on all pooled SQLite connections; connection-pool leak patches (`audit-closes` gate)
- Phase 7 of timestamp migration: `ErrTimestampsMigrationDeferred` deferral stripped, full INTEGER rebuild; recall/semantic-search LIKE-fallback column/scan alignment fixes

### Security
- Data-plane file permissions tightened to 0600; startup permissions check with auto-heal; auto-created directories restricted to 0700
- AGPL-3.0 license surfaced at top of README

### CLI / UX / CI
- Install default flipped to user-space (`~/bin`); mode/persona set tightened to 3+3 with safe fallback
- Route system: three-state gate (blank/auto/manual) for `handleRoute`; route_render supersedes route_apply; hermetic fixtures for route/recall tests
- CI: coverage reporting in gate job; `CGO_LDFLAGS=-lm` for FTS5 bm25; hermetic `TestCallRoute_ReturnsReport`; ingest no longer leaks roadmap text
- OpenClaw agent plugins introduced (auto-route, memory) under `agent-plugins/`

### Repo Hygiene
- Untracked `.opencode/`, `reference/` corpus, and `CLAUDE.md` from git (gitignored; kept locally); scrubbed runtime logs, duplicate phrase lists, ephemeral smoke scripts

## 2026-07-30 — Unix-Epoch Timestamp Migration

- **feat(core): unify all timestamp columns on unix-epoch seconds** — All 37 (table, column) pairs across 19 tables migrated to INTEGER seconds via `timestamps_unified_v1` sentinel. Go struct fields become `int64` / `*int64`. CLI inputs accept both RFC3339 and unix-epoch integers. Display formatting centralized at `FormatUnixSeconds` / `FormatOptionalUnixSeconds`. **Operators must take `mpm backup-db` before installing this release.**

## 2026-07-07 — Architecture Split & Security Audit

### Audit & Remediation (15 findings patched)

**Critical**
- `handlers_backup.go` — replaced `exec.Command(sqlite3, ".read " + path)` with `os.ReadFile` + `db.Exec`
- `web.go` — added `http.Server` timeouts (Read/Write 30s, Idle 60s)
- `web_handlers.go` — added `http.MaxBytesReader` (10 MiB cap) to all 5 body-reading handlers

**High**
- `web.go` — added `withSecurityHeaders` middleware (X-Content-Type-Options, X-Frame-Options, Referrer-Policy); CORS preflight; `MkdirAll` before port file write
- `embeddings.go` — fixed JSON injection via `json.Marshal` instead of string concat; `sync.Once` caching for config probe

**Medium**
- `app.js` — token storage moved from `localStorage` to `sessionStorage` (with fallback)
- `web.go` / `web_handlers.go` — `serverError()` helper replaced 15 raw error-leak call sites
- `memory.go` / `web_db.go` — all `CURRENT_TIMESTAMP` comparisons against `expires_at` converted to `strftime('%s','now')`
- `handlers.go` — `getDB()` singleton via `sync.Once` with `closeDB()` helper
- `db.go` — startup auto-rotation for `watchdog.jsonl` and `mirror.jsonl`

**Low**
- `app.js` — `esc()` now escapes single quotes
- `web.go` — added `Access-Control-Allow-Origin: *` + OPTIONS preflight
- `handlers.go` — `closeDB()` logs errors via `usererror.Warn`
- `web.go` — `os.MkdirAll` before port file write

**Score: 90/100** (full report in `audit.md`)

### LessonType Validation
- Added `ValidateLessonType()` with strict allowlist map in `internal/core/db.go`
- Validated at all 4 entry points: core `AddLesson`, web API, CLI, MCP tool

### Phase 1 — Module Boundary Rename
- Renamed `internal/` → `internal/core/`
- Moved all 7 subpackages (`config`, `tools`, `synth`, `usererror`, `logging`, `mpmcli`, `seed`)
- Updated all import paths project-wide; build + tests pass

### Phase 2 — CoreDB Interface
- Defined `CoreDB` interface (~130 methods) in `internal/core/core.go` with `var _ CoreDB = (*DatabaseManager)(nil)` compile-time assertion
- Switched `WebServer.db`, `getDB()`, `openCallDM()`, `HandlerFunc`, all 60+ tool handlers, `MemoryStore.DM` to use `CoreDB`
- Exported `AdmitResult` / `AdmitChainEntry` (were unexported `admitResult` / `admitChainEntry`); build + tests pass

### Phase 3 — Runtime Isolation
- Added `NewSession() (CoreDB, error)` to `CoreDB` interface; `DatabaseManager` implementation opens an independent `*sql.DB` + inits schema + attaches shared DB
- Updated fire-and-forget synthesis goroutines to use `dm.NewSession()` instead of `mpminternal.NewDatabaseManager("")`
- Changed `AutoSynthesize`, `DetectNearMiss`, `logWatchdogOp` to accept `CoreDB` instead of `*DatabaseManager`
- Fixed `sqlopen_owner_test.go` directory paths; build + tests pass

### Phase 4 — Standalone Core Module
- Created `internal/core/go.mod` with `module github.com/flowbyte-com/mpm-core`
- Updated all `mpm/internal/core` → `github.com/flowbyte-com/mpm-core` import paths (~70 files)
- Main `go.mod` uses `replace github.com/flowbyte-com/mpm-core => ./internal/core` for local development
- Updated `Makefile` test target to run both modules; `go mod tidy` on both cleaned stale dependencies
- Updated `CLAUDE.md` paths, test commands, and module structure

### Files Changed
- `internal/core/` — new standalone Go module (moved from `internal/`)
- `internal/core/core.go` — `CoreDB` interface (new, ~130 methods)
- `internal/core/admission.go` — exported `AdmitResult`, `AdmitChainEntry`
- `internal/core/db.go` — `NewSession()`, `ValidateLessonType()`, `ValidLessonTypes`
- `internal/core/go.mod` — standalone module definition
- `cmd/mpm/` — all files updated to import from `github.com/flowbyte-com/mpm-core`
- `cmd/mpm-mcp/` — updated imports; `main.go` uses `getDB().NewSession()` for synthesis
- `go.mod` — added `replace` directive; removed stale dependencies
- `Makefile` — test target split across both modules
- `docs/ARCHITECTURE_SPLIT.md` — 4-phase migration plan (new)
- `audit.md` — comprehensive audit report (new)
