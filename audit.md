# MPM Audit — 2026-07-23

## Fix 1: SQL typo `memory.tags` → `memories.tags`

**File:** `internal/core/web_db.go:1142`

**Finding:** The `GetMemoryStats` query used `json_each(memory.tags)` instead of `json_each(memories.tags)`. This caused the `by_tag` stat to always be empty — `json_each` received no rows, so the tag-frequency breakdown silently returned zero results.

**Fix:** Changed to `json_each(memories.tags)`.

**Risk:** Low/Medium — stats are informational, but silent omission masked the bug.

---

## Fix 2: `exec.LookPath` called with full command string

**File:** `cmd/mpm/main.go:1060`

**Finding:** The doctor check's tool table stored `"fzf --version"`, `"sqlite3 --version"`, `"git --version"` as the command string. `exec.LookPath` was called with the full string including arguments, which always fails — every installed tool was reported as "Not found". The version-inspection path used `tool.name` correctly, so fix was structural: remove the `command` field entirely and use `tool.name` for both LookPath and version exec.

**Fix:** Removed the `command` field from the tool struct; LookPath uses `tool.name`; version uses `exec.Command(tool.name, "--version")`.

**Risk:** Low — cosmetic (doctor check always reported optional tools as missing), but eroded trust in diagnostics.

---

## Fix 3: Git argument injection in `FetchGitLog`

**File:** `internal/core/changelog.go:228-240`

**Finding:** `GitLogOptions.Since` and `GitLogOptions.Until` were concatenated directly into `git log` arguments. While no external input surface currently feeds these fields, any future path that accepts user-controlled ref strings (e.g., a CLI `--since` flag) could inject arbitrary git arguments.

**Fix:** Added `validateGitRef()` with regex `^[a-zA-Z0-9._\-/@]+$`, called at the top of `FetchGitLog`. Empty strings pass through (meaning no bound).

**Risk:** Low (no current injection surface) — defense-in-depth.

---

## Fix 4: Missing `rows.Err()` checks

**Files:** `internal/core/db.go`, `internal/core/web_db.go`, `internal/core/memory.go`

**Finding:** 13 `rows.Next()` loops across 3 files lacked the required `rows.Err()` check after loop exit. If an iteration error occurred mid-scan (e.g., disk I/O failure, constraint violation on a joined temp table), the loop would silently return partial results as if they were complete.

**Locations fixed:**
- `db.go:1208` — `scanGlobalRuleRows`
- `db.go:2175` — `GetAllSystemConfigs`
- `db.go:2797` — `SearchLessons` (FTS5 path)
- `db.go:2830` — `searchLessonsLike`
- `web_db.go:131` — `QueryMemories`
- `web_db.go:225` — `SearchMemories`
- `web_db.go:720` — `ListReferences`
- `web_db.go:827` — `SearchReferences`
- `web_db.go:1102` — `SearchTopics`
- `web_db.go:1388` — `GetMemoriesForExport`
- `memory.go:936` — `GetRecent`
- `memory.go:1394` — `ConsolidateMemories`

**Risk:** Low (I/O errors are rare in practice) — defensive correctness per Go best practice.

---

## Fix 5: SchemaPrefix SQL injection surface

**File:** `internal/core/hybrid_search.go`

**Finding:** `SchemaPrefix` is interpolated directly into SQL table-reference strings across 11 functions (searchFTS5, searchLike, VectorMatch, IVFSearch, LoadVectorCentroids, AssignToCluster, etc.). While production only ever sets it to `""` or `"shared."` (both from constants), the field is exported and unvalidated — any future caller that populates it from dynamic input would open an injection vector.

**Fix:** Added `ValidateSchemaPrefix()` with regex `^$|^[a-zA-Z_][a-zA-Z0-9_]*\.$`, called at the top of exported entry points (`HybridSearch`, `VectorMatch`).

**Risk:** Low (no current dynamic-population path) — defense-in-depth for the exported API surface.

---

## Not Addressed (Low Risk / Already Mitigated)

- **ATTACH DATABASE path** (`db.go:812`): Shared DB path comes from `MPM_SHARED_DB` env var. Single-quote escaping is already applied. Path traversal to SQLite via env var requires local access; acceptable risk.
- **EDITOR env var** (`handlers_watch.go:37`): `exec.Command` doesn't invoke a shell, so no shell injection. Argument injection requires controlling the editor binary itself, which requires local access.
- **Log file permissions** (`active_state.go`): 0644 is standard for non-sensitive logs. Audit data does not contain credentials.
- **Backup path traversal** (`handlers_backup.go:84-97`): Already mitigated with `filepath.Clean` + prefix check.
