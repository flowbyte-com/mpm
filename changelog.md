# Changelog

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
