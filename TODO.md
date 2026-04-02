# MPM Data Architecture — TODO

**Last updated:** 2026-04-02
**Status:** ✅ Implementation Complete

---

## Priority 1 — Critical (Foundation)

### ~~TODO-001: Fix `QueryMemory` to use FTS5 MATCH~~ ✅ DONE (prior implementation verified)
**Problem:** `QueryMemory()` ignored the query string entirely — `ORDER BY id LIMIT n`. FTS5 triggers were synced but never queried.
**Fix:** Three-tier search strategy:
1. **FTS5 MATCH** (`WHERE memories_fts MATCH ?`) with `JOIN memories_fts fts ON m.rowid = fts.rowid ORDER BY fts.rank` — proper ranked full-text results
2. **FALLBACK LIKE** if FTS query fails (malformed query) — `WHERE content LIKE ?`
3. **RECENT** if query is empty — `ORDER BY created_at DESC`

**Files:** `internal/memory.go`

### ~~TODO-002: Add `topic_memberships` table to `InitSQLite`~~ ✅ DONE (prior implementation verified)
**Problem:** `ingest.go` inserts into `topic_memberships` but the table is never created.
**Fix:** `topic_memberships` table created in `InitSQLite()` with memory_id, session_id, topic_id columns and indexes.
**Files:** `internal/memory.go`

### ~~TODO-003: Add `sessionID` param to `AddMemory()`~~ ✅ DONE (2026-04-02)
**Problem:** `AddMemory()` signature is `AddMemory(content, collection, tags, metadata, source)` — no sessionID.
**Fix:** 
- Added `sessionID string` as 6th parameter to `AddMemory()` in `internal/memory.go`
- Set `mem.SessionID = sessionID` in memory struct and passed to INSERT
- Updated all callers in `handlers.go` (handleMemoryAdd, handleSessionAdd) to pass `""` for sessionID
- Updated internal caller in `MemoryStore.PromoteTopicToMemory()` 
**Files:** `internal/memory.go`, `cmd/mpm/handlers.go`

### ~~TODO-004: Add `parent_topic_id` to topics table~~ ✅ DONE (prior implementation verified)
**Problem:** Topics are flat. No hierarchy for nested concepts.
**Fix:** `parent_topic_id TEXT` column added via migration in `InitSQLite()`.
**Files:** `internal/memory.go`

---

## Priority 2 — Important (Organization)

### ~~TODO-005: Auto-tagging in ingest~~ ✅ DONE (2026-04-02)
**Problem:** Memories get only basic `["topic", sanitized_name]` tags. Not findable without manual effort.
**Fix:** Added `extractAutoTags()` function in `internal/ingest.go` that extracts:
- **Project tags** from source path (`flowbyte/mpm` → `project:mpm`)
- **Date tags** from path or content (`YYYY-MM-DD` pattern)
- **Keyword tags** via pattern matching (`bug`, `feature`, `decision`, `lesson`, `fix`, `refactor`, `todo`, `hack`, `note`)
- **Entity tags** via `@(\w+)` regex → `person:name`
Auto-tags are merged with existing tags in both main body and topic memories.
**Files:** `internal/ingest.go`

### ~~TODO-006: Add `reference_id` to memories~~ ✅ DONE (2026-04-02)
**Problem:** No `reference_id` column in memories table.
**Fix:** 
- Added `ALTER TABLE memories ADD COLUMN reference_id TEXT` migration in `InitSQLite()`
- Added `ReferenceID string` field to `Memory` struct
- Updated INSERT in `AddMemory()` to include `reference_id` column
**Files:** `internal/memory.go`

### ~~TODO-007: Topic memberships for memories~~ ✅ DONE (2026-04-02)
**Problem:** `topic_memberships` has `memory_id` column but no code inserts memories into it — only sessions are tracked.
**Fix:** In `InsertOpenClawDocument()`, after inserting a topic memory with `memoryID`, now also executes:
```sql
INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id) VALUES (?, ?)
```
This creates the memory-to-topic link.
**Files:** `internal/ingest.go`

### ~~TODO-008: Periodic semantic dedup job~~ ✅ DONE (2026-04-02)
**Problem:** No dedup command or function exists.
**Fix:**
- Added `DedupeMemories()` to `internal/memory.go`:
  - Exact dedup: finds memories with same content, keeps oldest, soft-deletes rest
  - Near dedup: uses `hashSimilarity()` (cosine similarity on HashEmbed vectors), threshold > 0.9
  - Soft-delete via `deleted_at = CURRENT_TIMESTAMP`
- Added `handleDedupe()` to `cmd/mpm/handlers.go`
- Added `dedup` case to `handleConnection()` in `cmd/mpm/main.go`
- Command: `mpm dedup`
**Files:** `internal/memory.go`, `cmd/mpm/handlers.go`, `cmd/mpm/main.go`

---

## Priority 3 — Nice to Have (Polish)

### ~~TODO-009: Consolidate `ReferenceStore` → single SQLite table~~ ✅ DONE (2026-04-02)
**Problem:** `reference_new.go` has SQLite store but ReferenceStore in memory.go is orphaned JSON-based code.
**Fix:**
- Added `CREATE TABLE IF NOT EXISTS references (id, title, file_path, tags, content, content_hash, created_at)` to `InitSQLite()`
- Added `references_fts` FTS5 virtual table (title, tags, content)
- Added triggers for auto-sync: `references_ai`, `references_ad`, `references_au`
- No reference handlers exist in CLI — table + FTS5 now available for future wiring
**Files:** `internal/memory.go`

### ✅ TODO-010: Mirror write/read decision — DEFERRED
**Problem:** Mirror is still written on every add, still read by `GetRecent`.
**Fix (deferred):** Mirror write kept for audit trail. Read path fixed in TODO-012. Full mirror removal deferred to future decision.
**Files:** N/A

### ~~TODO-011: MetadataFilter SQLite pushdown~~ ✅ DONE (2026-04-02)
**Problem:** `MetadataFilter()` reads all from mirror then filters in Go.
**Fix:** Rewrote `MetadataFilter()` in `internal/memory.go` to use parameterized SQLite queries with:
- `json_extract(metadata, '$.key') = ?` for string equality
- `EXISTS (SELECT 1 FROM json_each(tags) WHERE value IN (...))` for tag matching
- `json_extract(metadata, '$.created') >= ?` / `<= ?` for range filters
- Dynamic WHERE clause generation from filter map
**Files:** `internal/memory.go`

### ~~TODO-012: `GetRecent` from SQLite~~ ✅ DONE (2026-04-02)
**Problem:** `GetRecent()` reads from mirror.jsonl.
**Fix:**
- Replaced `GetRecent()` body with SQLite query: `SELECT id, collection, content, session_id, tags, metadata, embedding, created_at FROM memories WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT ?`
- Replaced `GetByID()` to query SQLite directly via `QueryRow`, no longer calls `GetRecent()`
- Both functions now parse JSON tags/metadata/embedding from DB
**Files:** `internal/memory.go`

---

## Ideas for Later (Backlog)

- [ ] LLM-based topic extraction (semantic, not just pattern matching)
- [ ] Flashcard / key-fact mode: distill long memories into one-liners
- [ ] `mpm brain` command: interactive CLI for browsing the brain by topic hierarchy
- [ ] Export topic trees as markdown outlines
- [ ] Import entire reference libraries (e.g. `mpm import ~/notes --project=mpm`)
- [ ] Topic merge: combine two topics and deduplicate their memories
- [ ] Time-based memory views: "what did I work on last week?"

---

## Completed

- All 12 TODO items implemented as of 2026-04-02
