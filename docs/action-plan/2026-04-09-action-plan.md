# MPM Action Plan — 2026-04-09

## 🔴 CRITICAL

### 1. SQL Injection in `recall.go` — `keywordSearch()`
**File:** `cmd/mpm/recall.go:143`
**Severity:** CRITICAL
**Status:** ✅ FIXED

User query is concatenated directly into `FTS5 MATCH` and `LIKE` clauses:
```go
fts.content MATCH '` + query + `'
OR m.content LIKE '` + likePattern + `'
```
Fix: use parameterized queries — pass query as `?` bind value throughout.

---

### 2. `GetByID` returns `nil, nil` when DB is uninitialized
**File:** `internal/memory.go:697`
**Severity:** HIGH (root cause of "Memory not found" bug)
**Status:** ✅ FIXED

`GetByID` does not call `InitSQLite()` when `DB == nil`, unlike `GetRecent` which does:
```go
// GetRecent has this guard:
if s.DB == nil {
    if err := s.InitSQLite(); err != nil { ... }
}
// GetByID just does:
if s.DB == nil {
    return nil, nil  // silent nil,nil — treated as "not found"
}
```
Fix: add `InitSQLite()` guard at top of `GetByID`.

---

## 🟠 HIGH

### 3. `SearchSessions` searches wrong collection name
**File:** `internal/memory.go:1363`
**Severity:** HIGH — sessions are NEVER found via search
**Status:** ✅ FIXED

```go
// SearchSessions queries:
WHERE collection = 'sessions'   // plural

// handleSessionAdd stores as:
collection = 'session'           // singular
```
Fix: change `='sessions'` to `='session'`.

---

### 4. `GetByID` ignores `collection` parameter
**File:** `internal/memory.go:696`
**Severity:** HIGH
**Status:** ✅ FIXED

`collection` argument is accepted but never used in the WHERE clause. Any memory with matching `id` is returned regardless of collection.

Fix: add `AND collection = ?` to the WHERE clause.

---

### 5. Sensitive content filtering missing from `handleLessonAdd`
**File:** `cmd/mpm/handlers.go:1886`
**Severity:** HIGH — lessons bypass 17-pattern filtering
**Status:** ✅ FIXED

`handleLessonAdd` calls `lessonStore.AddLesson` which calls `DatabaseManager.AddLesson` — none call `isSensitiveContent()`. API keys, passwords, tokens could end up in lessons.

Fix: call `isSensitiveContent()` in `AddLesson` before storing.

---

## 🟡 MEDIUM

### 6. `topic_memberships` has duplicate PRIMARY KEY declarations
**File:** `internal/db.go:189-191`
**Severity:** MEDIUM — schema misleading
**Status:** ✅ FIXED

```sql
PRIMARY KEY (memory_id, topic_id),   -- IGNORED by SQLite
PRIMARY KEY (session_id, topic_id),  -- ACTUAL primary key
CHECK (memory_id IS NOT NULL OR session_id IS NOT NULL)
```
Fix: remove first `PRIMARY KEY` and add proper `UNIQUE` constraint if needed.

---

### 7. `FullTextSearch` bypasses FTS5
**File:** `internal/memory.go:907`
**Severity:** MEDIUM — inconsistent search results
**Status:** ✅ FIXED

`FullTextSearch` loads 1000 recent memories into Go and does `strings.Contains`, completely bypassing FTS5. Should use `QueryMemory` which properly uses FTS5.

Fix: refactor `FullTextSearch` to use FTS5 JOIN + LIKE fallback strategy.

---

### 8. `LessonStore` opens new DB connection per operation
**File:** `internal/lessons.go:24-29`
**Severity:** MEDIUM — likely cause of `lesson add` hang
**Status:** ✅ FIXED

Every `AddLesson`/`GetLesson`/`ListLessons` creates a new `DatabaseManager` and new SQLite connection. If DB is locked, each operation retries independently.

Fix: use a shared `DatabaseManager` instance (singleton pattern with `sync.Once`).

---

### 9. AWS Secret Key regex too broad
**File:** `internal/memory.go:500`
**Severity:** MEDIUM — false positives
**Status:** ✅ FIXED

`[A-Za-z0-9/+=]{40}` matches any 40-char Base64 string. Real AWS secret keys follow a specific format.

Fix: removed AWS Secret Key pattern — only the Access Key ID (AKIA...) is reliably detectable.

---

### 10. `mpm_memory.db` deprecated file still exists
**File:** `src/db/mpm_memory.db` (0 bytes)
**Severity:** LOW — clutter
**Status:** ✅ FIXED

Fix: deleted `src/db/mpm_memory.db`.

---

## 📚 DOCS

### 11. `docs/README.md` TOC links are broken
**Severity:** CRITICAL — users get 404s
**Status:** ✅ FIXED

All subdirectory links (`getting-started/`, `commands/`, `advanced/`) point to files that were never created. Actual docs are flat in `docs/`.

Fix: rewritten TOC to match actual flat structure.

---

### 12. `docs/ARCHITECTURE.md` has stale path
**File:** `docs/ARCHITECTURE.md:76`
**Severity:** MEDIUM — confusing for new users
**Status:** ✅ FIXED

References `projects/mpm/src/db/mpm.db` — corrected to actual path with explanation.

---

### 13. `docs/SYNTH.md` describes unimplemented config
**Severity:** MEDIUM — documentation contradicts code
**Status:** ✅ FIXED

Claims `synth` section in `mpm_config.json` is configurable — no such config exists in code.

Fix: updated SYNTH.md to reflect current env-var behavior; noted `mpm_config.json` approach as planned.

---

### 14. `mpm fortune` and `mpm topic rm` documented but not implemented
**File:** `docs/COMMANDS.md`
**Severity:** MEDIUM
**Status:** ✅ FIXED

- `topic rm` — EXISTS (implemented as `topicRm` in `topic.go:33`). COMMANDS.md accurately reflects this.
- `mpm fortune` — NOT implemented. Removed entirely from router, COMMANDS.md, and daemon handler.

---

## 🧪 TEST GAPS

### 15. No tests for recall, watch, search, handlers
**Severity:** MEDIUM
**Status:** ✅ PARTIALLY DONE (targeted high-risk functions)

Added tests for the highest-risk functions:
- `internal/memory_test.go`: `GetByID`, `SearchSessions`, `FullTextSearch`, `TestSearchSessionsCollectionBug` (verifies fix for collection name bug)
- `cmd/mpm/recall_test.go`: `keywordSearch` (SQL injection fix, LIKE fallback, limit, empty DB, SQL injection attempt)

Still untested: `watch.go`, `handleMemoryShow`, `handleSessionShow`, `IngestOpenClaw`

---

## Bonus: Build Fix

### 16. `check_fts.go` and `check_db.go` conflict with `main`
**Severity:** HIGH — breaks `go test ./...`
**Status:** ✅ FIXED

Standalone debug utilities declared `func main()` in `package main`, conflicting with `cmd/mpm/main.go`. Fixed by adding `//go:build ignore` to exclude from normal builds.

### 17. `SearchSessions` never sets `mem.Collection` — found via new tests
**Severity:** HIGH — `Collection` always empty string
**Status:** ✅ FIXED

`SearchSessions` scanned 5 columns but never assigned the `Collection` field, leaving it as `""` for all results. Fixed by adding `collection` to the SELECT and scanning it into `m.Collection`.

---

## 🟢 Open Issues (from issues.md)

### 18. Synth OpenClaw-agnostic refactor
**Files:** `cmd/mpm/synthesize.go`, `internal/config/config.go`, `docs/SYNTH.md`
**Severity:** 🟢 FEATURE
**Status:** ✅ FIXED

Removed hardcoded OpenClaw gateway dependency. Synth credentials now come solely from `mpm_config.json` `synth` section or env vars.

Changes:
- Removed `getOpenClawModelBaseURL` function (was reading `~/.openclaw/openclaw.json`)
- Added `SynthConfig` struct to `internal/config/config.go`
- `callSynthesisLLM` resolves credentials: config.api_key → MINIMAX_API_KEY → error
- Base URL defaults per model (MiniMax → api.minimax.io, local → localhost:11434)
- `SynthesisResult` fields changed to `json.RawMessage` for null-safe handling
- Output messages clarified for 0-fact and transient sessions
- `docs/SYNTH.md` updated to reflect config-driven approach

---

### 19. Multi-source watch daemon (external_dbs polling)
**File:** `cmd/mpm/watch.go`, `internal/config/config.go`, `internal/ingest.go`
**Status:** ✅ FIXED

Watch daemon now polls `external_dbs` from `mpm_config.json`. Each DB gets its own goroutine with cursor-based polling at configured `interval_seconds`. Cursors persisted in `external_db_cursors` table so restarts don't re-ingest.

Changes:
- `Config` struct now has `ExternalDbs []ExternalDB` with `Path`, `Label`, `IntervalSeconds`
- `resolveWatchDirs` updated to use `GetMemoryDirs()`/`GetSessionsDirs()` arrays
- Added `startExternalDBPolling`, `pollExternalDB`, `pollOnce` to `watch.go`
- Added `external_db_cursors` table to `InitSQLite`
- Added `GetExternalDBCursor`, `SetExternalDBCursor` to `ingest.go`

---

### 20. Memory paths generalization
**File:** `internal/config/config.go`
**Status:** ✅ FIXED

Config now supports arrays for `memory_dirs` and `sessions_dirs` in `mpm_config.json`. Added `GetMemoryDirs()`, `GetSessionsDirs()`, `GetExternalDbs()` helper methods with backward compat for singular `memory_dir`/`sessions_dir`.

---

### 21. External DB session aggregation
**Status:** ✅ ALREADY WORKS

OpenClaw does not store sessions in its SQLite DB (only `chunks` table for memories). Sessions are stored as `.jsonl` files in `~/.openclaw/agents/main/sessions` — this directory is already monitored by MPM's watch daemon via `sessions_dirs` fsnotify. Sessions from OpenClaw are automatically ingested into MPM's `sessions` table. No DB-level aggregation needed.

---

## Summary

| Item | Status |
|------|--------|
| 1. SQL injection in recall.go | ✅ FIXED |
| 2. GetByID nil DB bug | ✅ FIXED |
| 3. SearchSessions wrong collection | ✅ FIXED |
| 4. GetByID ignores collection param | ✅ FIXED |
| 5. Lesson sensitive content filtering | ✅ FIXED |
| 6. topic_memberships duplicate PK | ✅ FIXED |
| 7. FullTextSearch bypasses FTS5 | ✅ FIXED |
| 8. LessonStore connection per op | ✅ FIXED |
| 9. AWS Secret Key regex too broad | ✅ FIXED |
| 10. Deprecated mpm_memory.db | ✅ FIXED |
| 11. docs/README.md broken links | ✅ FIXED |
| 12. ARCHITECTURE.md stale path | ✅ FIXED |
| 13. SYNTH.md unimplemented config | ✅ FIXED |
| 14. fortune/topic rm docs | ✅ FIXED |
| 15. Test coverage gaps | ✅ PARTIALLY DONE |
| 16. check_fts.go conflict | ✅ FIXED |
| 17. SearchSessions Collection field | ✅ FIXED |
| 19. Multi-source watch daemon | ✅ FIXED |
| 20. Memory paths generalization | ✅ FIXED |
| 21. External DB session aggregation | ✅ ALREADY WORKS |
| 18. Synth OpenClaw-agnostic refactor | ✅ FIXED |
| 19. Multi-source watch daemon | 🟡 NOT STARTED |
| 20. Memory paths generalization | 🟡 NOT STARTED |
| 21. External DB session aggregation | 🟡 NOT STARTED |
