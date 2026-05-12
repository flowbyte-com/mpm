# MPM Todo — Prioritized

## 🔴 Must Fix (broken functionality)

### 1. `mpm recall` returns no results — FTS5 query broken

**Priority:** High
**Affected:** `cmd/mpm/recall.go` → `keywordSearch()`

**Problem:** `recall` always returns "No memories found" while `mpm memory search` works correctly.

**Root cause:** `recall.go` uses column-specific FTS5 syntax (`fts.content MATCH ?`) which doesn't work in a JOIN context. The correct approach is table-level `memories_fts MATCH ?` with `ORDER BY fts.rank`.

**Fix:** Align `keywordSearch()` with `memory.go`'s `QueryMemory` FTS5 approach:
- Use `memories_fts MATCH ?` instead of `fts.content MATCH ?`
- Change `LEFT JOIN` → `JOIN`
- Add `ORDER BY fts.rank`

---

### 2. `mpm memory show <id>` returns "Memory not found" for valid IDs

**Priority:** High
**Affected:** `cmd/mpm/handlers.go` → `handleMemoryShow`

**Problem:** `mpm memory add` succeeds but immediately calling `mpm memory show <id>` returns "Memory not found" even though `mpm memory list` shows the memory.

**Root cause:** `store.GetByID()` query may fail due to type mismatch or wrong column. Needs SQL inspection.

**Fix:** Inspect `store.GetByID()` in `memory.go` — verify `WHERE id = ? AND deleted_at IS NULL` and confirm UUID comparison works correctly.

---

### 3. `mpm lesson add` hangs indefinitely

**Priority:** Medium
**Affected:** `cmd/mpm/handlers.go` → `handleLessonAdd`

**Problem:** `lesson add` hangs (SIGKILL after timeout). Likely a worker queue deadlock or missing response channel.

**Fix:** Add debug logging to find where it hangs. Check if `handleLessonAdd` waits on something that never fires.

---

## 🟡 Should Fix (correctness / UX)

### 4. `mpm recall` and `mpm memory search` have overlapping responsibilities

**Priority:** Medium
**Affected:** `cmd/mpm/recall.go` vs `internal/memory.go`

**Problem:** Two different search implementations with different results is confusing. `recall` uses broken FTS5; `memory search` loads 1000 recent rows and does `strings.Contains` in Go.

**Fix:** Either fix `recall` to use `QueryMemory` from `memory.go`, or deprecate `recall` entirely and point users to `memory search`.

---

### 5. `mpm memory list` truncation inconsistent

**Priority:** Low-Medium
**Affected:** `cmd/mpm/handlers.go` → `handleMemoryList`

**Problem:** Truncation logic exists (500 chars) but full content sometimes bleeds through.

**Fix:** Audit `handleMemoryList` — ensure truncation is applied consistently before output.

---

### 6. Synth null handling — LLM returns `null` instead of `[]`

**Priority:** Medium
**Affected:** `cmd/mpm/synthesize.go`

**Problem:** If LLM returns `null` for `topics` or `memories`, `json.Unmarshal` into `[]string` fails and synthesis errors out.

**Fix:** Default to empty arrays when field is `null` or missing.

---

### 7. Synth success/failure reporting unclear

**Priority:** Low
**Affected:** `cmd/mpm/synthesize.go`

**Problem:** Success message says "✅ X facts" even when all DB writes failed. No differentiation between "LLM failed" vs "DB write failed".

**Fix:** Report DB write failures explicitly. If LLM succeeded but 0 facts stored, say "0 facts stored" not "✅ 0 facts".

---

## 🟢 Nice to Have

### 8. Synth model configurable via `mpm_config.json`

**Priority:** Low
**Affected:** `cmd/mpm/synthesize.go`

**Status:** Spec'd in `docs/SYNTH.md` — not yet implemented.

**Fix:** `callSynthesisLLM()` should read `synth` section from config instead of relying on env vars or OpenClaw config. Support Ollama/LM Studio via `base_url`.

---

## Completed (historical — for reference)

- ✅ Multi-source watch daemon (memory_dirs, sessions_dirs, external_dbs)
- ✅ Detached watcher with PID file lifecycle
- ✅ Graceful SIGTERM/Interrupt shutdown
- ✅ PID timing fix (writeWatchPID after pool start)
- ✅ Systemd unit for reboot persistence
- ✅ FTS5 triggers use `CREATE TRIGGER IF NOT EXISTS`
- ✅ Common indexes use `CREATE INDEX IF NOT EXISTS`
- ✅ Removed dual DB (`mpm_memory.db` deleted)
- ✅ Unified single-process architecture (no daemon subprocess)