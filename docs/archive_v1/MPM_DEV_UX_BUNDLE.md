# TASK: Implement the Developer UX Bundle (Wishlist #4, #5, #9, #11)

The backend architecture is completely audited and stabilized. We are now executing the final four Quality-of-Life improvements to upgrade the CLI experience.

Please implement the following features:

---

## 1. Structured Recall Filters (Wishlist #4)
Upgrade `mpm recall` to support semantic chaining.

* **Implementation:** Add `--weight-below` and `--before` flags to `cmd/mpm/recall.go`.
  * `--collection` flag **already exists** (line 38, `collection := fs.String("collection", "memories", ...)`) — do NOT re-implement it, just use the existing one.
  * `--before` is net new (existing code has `--since` and `--until`, not `--before`).
  * `--weight-below` is net new.
* **Behavior:** Dynamically build the SQLite `WHERE` clause to apply these filters alongside the existing FTS5 text search. If no FTS5 query is provided, the filters still work — acting as a structured list of memories.

---

## 2. Interactive Memory Add (Wishlist #5)
Create an ergonomic way to draft multi-line memories without fighting terminal string escaping.

* **Implementation:** Add a `-i`/`--interactive` flag to `handleMemoryAdd` in `cmd/mpm/handlers.go`.
* **Behavior:**
  1. If `-i` is passed, create a temporary `.md` file in `/tmp/mpm_*`.
  2. Open it using `$EDITOR` (fallback: `nano`, then `vim` if neither is set).
  3. After the user saves and exits, read the file contents and delete the temp file.
  4. **Show a preview** of the drafted content and ask for explicit `[y/N]` confirmation before proceeding with ingestion.
  5. On confirmation: proceed with the standard ingestion pipeline (`AddMemory`, auto-synthesis, etc.).
  6. On decline or error: discard and exit cleanly.
* **Constraint:** The preview + confirmation step is mandatory — opening the editor alone is not sufficient.

---

## 3. CLI Subcommand Aliasing (Wishlist #9)
Allow the tool to adapt to user muscle memory.

* **Implementation:** Add an `"aliases"` map to `mpm_config.json` (e.g., `"mem": "recall --collection memories"`) and implement a pre-processor in `cmd/mpm/main.go` that intercepts `os.Args[1:]` before handing off to the router.
* **Behavior:**
  * If the first argument matches a defined alias key, replace `os.Args` with the expanded tokens.
  * Aliases may expand to multiple arguments (e.g., `"ls"` → `["recall", "--collection", "memories"]`).
  * Chain expansion is not required (e.g., `mpm mem foo` does not need to resolve `mem` and then treat `foo` as an argument to the expanded alias — just basic direct expansion).
* **Edge cases:**
  * `mpm help` and `mpm --version`/`-v` must resolve before alias processing (return immediately if detected).
  * Unknown commands pass through unmodified to the router.
* **Config loading:** Parse aliases from `mpm_config.json` via the existing `config.LoadConfig()` flow. Unrecognized aliases are silently ignored (no error).

---

## 4. `mpm doctor --explain` (Wishlist #11)
Provide premium visibility into FTS5 index health as the memory database scales.

* **Implementation:** Add `--explain` flag to the `doctor` command.
* **Behavior:** When `--explain` is passed, execute `EXPLAIN QUERY PLAN` on the core FTS5 query used by `mpm recall`:
  ```sql
  EXPLAIN QUERY PLAN
  SELECT m.id, m.collection, m.content, m.session_id, m.tags, m.metadata, m.embedding, m.created_at, fts.rank
  FROM memories m
  JOIN memories_fts fts ON fts.rowid = m.rowid
  WHERE memories_fts MATCH '<query>'
    AND m.collection = '<collection>'
    AND m.deleted_at IS NULL
  ORDER BY rank
  LIMIT <limit>
  ```
* Print the resulting tree directly to stdout so the user can verify index usage vs. table scan. This helps diagnose regressions as the memories table scales.
* Run against the actual `memories_fts` FTS5 virtual table — not a mock query.

---

## Codebase Rigidity Rules:
* All DB access must continue to route via `DatabaseManager`.
* Do not alter the core schema or background workers. Keep all changes restricted to CLI flag parsing, routing, and user input handling.
* All tests must pass before declaring the bundle complete.

---

## Files to Modify

- `cmd/mpm/recall.go` — add `--weight-below` and `--before` flags (re-use existing `--collection`)
- `cmd/mpm/handlers.go` — add `-i`/`--interactive` to `handleMemoryAdd` with temp file + editor flow + preview confirmation
- `cmd/mpm/main.go` — alias pre-processor (runs before router, respects `help`/`--version` early return)
- `cmd/mpm/simple_cmds.go` or `cmd/mpm/main.go` — add `--explain` to `doctor` command with `EXPLAIN QUERY PLAN` execution and stdout output
- `internal/config/config.go` — extend `Config` struct to support `Aliases map[string]string`

## Verification

- `go test -tags fts5 ./...` — all pass
- `mpm recall --weight-below 3 --before 2026-05-01` — returns only memories below weight 3 created before 2026-05-01
- `mpm add -i` — opens editor, on save shows preview, on `y` saves to memory
- `mpm mem` (if aliased) — expands to `mpm recall --collection memories` and executes correctly
- `mpm doctor --explain` — prints EXPLAIN QUERY PLAN output for the core FTS5 recall query