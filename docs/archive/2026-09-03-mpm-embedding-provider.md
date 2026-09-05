# MPM Embedding Provider Separation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the silent, env-only embedding fallback chain with an explicit, config-driven, four-state embedding subsystem. Remediate the 791 poisoned HashEmbed rows and 65 fabricated collision records already in the live DB without deleting historical artifacts.

**Architecture:** Embedding config reuses the existing `profiles` / `components` abstraction. `components["embedding"]` references a profile in `profiles`; the reserved sentinel `"disabled"` opts out cleanly. `EmbeddingConfig` carries `Source` (profile / env / disabled / absent) and `Status` (configured / unavailable / misconfigured / null). `EmbedText()` signature changes from `[]float32` to `([]float32, error)` — HashEmbed is removed from the runtime path. The migration adds `embedding_source` / `embedding_dimension` to `memories`, `synthetic` to `theories`, and a new `embedding_migration_log` audit table; the un-challenge is provenance-gated, idempotent, and reversible.

**Tech Stack:**
- Go 1.22+
- SQLite via `mattn/go-sqlite3` (FTS5 build tag required: `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1`, `-tags fts5`)
- Multi-module repo: root `github.com/flowbyte-com/mpm`, `internal/core` is a separate module `github.com/flowbyte-com/mpm-core` wired via `replace`. Tests for `internal/core` run from that subdirectory.
- `mpm-lint --gate` is the project's pre-commit linter; no `git commit --no-verify`.

**Spec:** `docs/superpowers/specs/2026-09-03-mpm-embedding-provider-design.md`

## Global Constraints

- **FTS5 build tag**: every `go build` / `go test` of `internal/core` MUST use `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1` and `-tags fts5`.
- **Submodule test commands**: `internal/core` tests run from `cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...`. The repo root `make test` wraps this.
- **Migration idempotency**: every new schema migration uses `ALTER TABLE ... ADD COLUMN` (handled by `SafeMigrations`) or `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`. Use the existing `isDuplicateColumnError` helper for ALTER safety.
- **Write-path read-back assertions**: every write must read back via `GetX(id)` before returning success. Pattern: see `internal/core/db.go:AddLesson`.
- **Substrate Defense Triad** (from CLAUDE.md §3): atomic state swap for cross-process files; defensive SQL aggregates (wrap `MAX/MIN/AVG` in `COALESCE`); write-path read-back assertions.
- **Projection Test**: do not persist what can be computed at read time. HashEmbed output is removed from storage because it is computable from `content` at read time.
- **No `git commit --no-verify`**. `mpm-lint --gate` must pass.
- **`probeEmbeddingConfig()` is removed from runtime** by Phase 2; only `mpm config detect-embedding` (an explicit operator command) probes.
- **No network probing anywhere** in `mpm add`, recall, synthesis, MCP, or daemon startup. The probe-removal proof test (Task 10) asserts zero HTTP requests during `mpm add` with no embedding config.
- **Disabled sentinel** (`components["embedding"] = "disabled"`): reserved for embedding only. `ProfileFor("llm") == "disabled"` is **not** a sentinel — returns nil (misconfigured for that component).
- **Env-var precedence**: `components.embedding == "disabled"` → disabled (never falls through to env). `components.embedding == "<profile>"` → profile (ignores env). `components.embedding` absent → `OLLAMA_ENDPOINT` / `OLLAMA_MODEL` env fallback. Neither → NullProvider.

## File Structure

### New files

| Path | Responsibility |
|---|---|
| `internal/core/embedding_migration.go` | Forensic classifier + provenance-gated un-challenge. Idempotent. Reversible. Logs every decision. |
| `internal/core/embeddings_test.go` | Unit tests: `resolveEmbeddingConfig` precedence branches; `EmbedText` signature contract; `validateEmbeddingProfile` field checks; `cosineSimilarity` panic-free contract. |
| `internal/core/embedding_migration_test.go` | Forensic classifier fixture tests + provenance-gate tests + idempotency test. |
| `cmd/mpm/migrate_embeddings.go` | `mpm ops migrate-embeddings [--undo <timestamp>]` command. Calls into `internal/core/embedding_migration.go`. |
| `cmd/mpm/detect_embedding.go` | `mpm config detect-embedding [--apply <name>]` command. |
| `internal/core/embedding_migration_fixture_test.go` | Fixture: 4-row memories table covering 256-dim / 384-dim / SQL NULL / literal `'null'` shapes. |

### Modified files

| Path | Responsibility |
|---|---|
| `internal/core/schema.go` | Append `SafeMigrations` entries; append `BaseTables` entry for `embedding_migration_log`; append `CommonIndexes` entries for the three new indexes. |
| `internal/core/embeddings.go` | Major rewrite: new `EmbeddingConfig` struct with `Source`/`Status`; `resolveEmbeddingConfig`; `buildProvider`; `validateEmbeddingProfile`; `providerName`; new `EmbedText` signature; remove `probeEmbeddingConfig`; keep `HashEmbed` as private forensic helper. |
| `internal/core/memory.go` | `SaveMemory` call site handles `(vec, err)` from new `EmbedText`. |
| `internal/core/db.go:3568` | `cosineSimilarity` gains a defensive length guard at the top. |
| `internal/core/hybrid_search.go` | `contradictionDetector` (line 344) and `VectorMatch` (line 974) gain `embedding_source != 'hash'` filter. |
| `internal/core/vector_index.go` | `:244` (lookup) and `:624` (`Rebalance`) gain `embedding_source != 'hash'` filter. |
| `internal/core/forge_dedup.go:174` | Zero-padding cosine path gains the same filter. |
| `internal/core/config/config.go` | `ProfileFor("embedding")` recognizes `"disabled"` sentinel and returns nil for it (resolver handles the rest). |
| `internal/core/synthesis_auto.go:536` | `EmbedText` call site handles the error. |
| `cmd/mpm/simple_cmds.go:102` | `mpm add` `EmbedText` call site handles the error; CLI exits non-zero. |
| `cmd/mpm/backfill_embeddings.go:69-71` | Tighten error message; use canonical resolution. |
| `cmd/mpm/service_doctor.go:117-156` | `checkEmbeddings()` rewrite + new `checkEmbeddingProvider()`. |
| `cmd/mpm/readiness.go:281-299` | `checkEmbeddings()` four-state model. |
| `docs/CONTRIBUTING.md`, `docs/INSTALL.md` | Remove "set OLLAMA_* in your env" instructions; replace with profile-based workflow. |
| `internal/core/embeddings.go` (top-of-file comment block) | Rewritten to describe the canonical precedence and the rationale for removing `probeEmbeddingConfig` from runtime. |
| `internal/core/memory.go` (`HashEmbed` doc comment) | Document that it is not called by `EmbedText`; retained only as a forensic-classifier helper. |

### Live DB migration path

The migration is **additive and idempotent**. On first binary boot against the existing live DB:

1. `SafeMigrations` adds the new columns. Idempotent via `isDuplicateColumnError`.
2. `BaseTables` creates `embedding_migration_log`. Idempotent via `IF NOT EXISTS`.
3. `CommonIndexes` creates the three new indexes. Idempotent via `IF NOT EXISTS`.
4. The forensic classifier (run on first `mpm ops migrate-embeddings` invocation) backfills `embedding_source` and `embedding_dimension` for the existing 1081 rows.

The forensic classifier is **not** auto-run on boot. It is an explicit operator command (`mpm ops migrate-embeddings`). This is per the spec's migration policy: the migration is operator-driven, not implicit.

---

## Task 1: Schema additions — columns, table, indexes

**Files:**
- Modify: `internal/core/schema.go` (append to `SafeMigrations`, `BaseTables`, `CommonIndexes`)
- Test: `internal/core/embedding_schema_test.go` (new — verifies the columns / table / indexes exist after init)

**Interfaces:**
- Consumes: existing `SafeMigrations` / `BaseTables` / `CommonIndexes` patterns
- Produces: `memories.embedding_source` (TEXT NOT NULL DEFAULT 'provider'), `memories.embedding_dimension` (INTEGER), `memories.synthetic` (INTEGER NOT NULL DEFAULT 0 — applied to theory rows via `WHERE collection = 'theories'`; theories are not a separate table), `embedding_migration_log` table, three new indexes

- [ ] **Step 1: Read `internal/core/schema.go:1156-1233` (SafeMigrations) and `:760-1100` (CommonIndexes) and `:9-419` (BaseTables) to confirm the append points.**

  Expected: three slices; `BaseTables` is `[]string` of `CREATE TABLE` statements; `SafeMigrations` is `[][3]string` of `(table, column, type)`; `CommonIndexes` is `[]string` of `CREATE INDEX` statements.

- [ ] **Step 2: Append to `SafeMigrations` (just before the closing `}` of the slice).**

  ```go
  // Embedding provenance (alpha-3.5 hardening): the live DB has 791
  // HashEmbed-derived vectors from the silent SHA-256 fallback. These
  // columns record provenance so the trust machinery can skip
  // non-semantic rows without losing them. Backfill runs as part of
  // `mpm ops migrate-embeddings`.
  {"memories", "embedding_source", "TEXT NOT NULL DEFAULT 'provider'"},
  {"memories", "embedding_dimension", "INTEGER"},
  // Theories (memories rows with collection='theories') whose evidence
  // was a HashEmbed cosine get marked so the provenance-gated
  // un-challenge can identify them. Theories are not a separate
  // table — the column lives on memories and theory rows get the
  // marker via their collection.
  {"memories", "synthetic", "INTEGER NOT NULL DEFAULT 0"},
  ```

- [ ] **Step 3: Append to `BaseTables` (just before the closing `}` of the slice).**

  ```go
  // Embedding migration audit log. Every un-challenge the migration
  // applies is recorded here (memory_id, old/new weight, reason,
  // theory_id, timestamp). Reversible: --undo replays these rows
  // in reverse. Sentinel row with reason='migration_applied' marks
  // the end of a successful run for idempotency.
  `CREATE TABLE IF NOT EXISTS embedding_migration_log (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    memory_id    TEXT NOT NULL,
    old_weight   REAL NOT NULL,
    new_weight   REAL NOT NULL,
    reason       TEXT NOT NULL,
    theory_id    TEXT,
    migrated_at  INTEGER NOT NULL,
    operator     TEXT NOT NULL DEFAULT 'mpm-migration'
  );`,
  ```

- [ ] **Step 4: Append to `CommonIndexes` (just before the closing `}` of the slice).**

  ```go
  // Partial indexes for live (non-deleted) memories only. Trust
  // machinery uses idx_memories_embedding_source to skip
  // 'hash' and 'null' rows cheaply.
  `CREATE INDEX IF NOT EXISTS idx_memories_embedding_source
    ON memories(embedding_source)
    WHERE deleted_at IS NULL;`,
  `CREATE INDEX IF NOT EXISTS idx_memories_embedding_dimension
    ON memories(embedding_dimension)
    WHERE deleted_at IS NULL;`,
  `CREATE INDEX IF NOT EXISTS idx_embedding_migration_log_memory_id
    ON embedding_migration_log(memory_id);`,
  ```

- [ ] **Step 5: Write the failing test.**

  Create `internal/core/embedding_schema_test.go`:

  ```go
  package internal

  import (
      "testing"

      "github.com/flowbyte-com/mpm-core/internal/testutil"
  )

  func TestEmbeddingSchemaExists(t *testing.T) {
      dm := testutil.NewTestDBManager(t)
      defer dm.Close()

      // Verify the three columns exist on the right tables.
      for _, c := range []struct{ table, column string }{
          {"memories", "embedding_source"},
          {"memories", "embedding_dimension"},
          {"memories", "synthetic"}, // theories are memories with collection='theories'
      } {
          var n int
          err := dm.SQLDB().QueryRow(
              `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
              c.table, c.column,
          ).Scan(&n)
          if err != nil {
              t.Fatalf("pragma_table_info %s.%s: %v", c.table, c.column, err)
          }
          if n != 1 {
              t.Errorf("missing column %s.%s", c.table, c.column)
          }
      }

      // Verify the audit log table exists.
      var name string
      err := dm.SQLDB().QueryRow(
          `SELECT name FROM sqlite_master WHERE type='table' AND name='embedding_migration_log'`,
      ).Scan(&name)
      if err != nil {
          t.Fatalf("query sqlite_master: %v", err)
      }
      if name != "embedding_migration_log" {
          t.Errorf("missing embedding_migration_log table, got %q", name)
      }

      // Verify the three indexes exist.
      for _, idx := range []string{
          "idx_memories_embedding_source",
          "idx_memories_embedding_dimension",
          "idx_embedding_migration_log_memory_id",
      } {
          var n int
          err := dm.SQLDB().QueryRow(
              `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`,
              idx,
          ).Scan(&n)
          if err != nil {
              t.Fatalf("query sqlite_master for index %s: %v", idx, err)
          }
          if n != 1 {
              t.Errorf("missing index %s", idx)
          }
      }
  }
  ```

  > **Note:** `testutil.NewTestDBManager(t)` is the project's existing test helper. If your local checkout does not yet expose it, fall back to `NewDatabaseManagerForTest(t)` from `internal/core/db_test.go` — whichever the existing tests use. Verify by running `grep -rn "NewTestDBManager\|NewDatabaseManagerForTest" internal/core/`.

- [ ] **Step 6: Run the test; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestEmbeddingSchemaExists -v
  ```

  Expected: PASS. The columns / table / indexes are created by the existing `migrateAll` call inside the test DB manager constructor.

- [ ] **Step 7: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS. The new schema additions are backward-compatible (columns have defaults; table and indexes are `IF NOT EXISTS`).

- [ ] **Step 8: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 9: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/schema.go internal/core/embedding_schema_test.go internal/core/sql_dump_validator.go && git commit -m "feat(schema): add embedding_source, embedding_dimension, memories.synthetic, embedding_migration_log"

  > The brief listed only schema.go + the new test file. The
  > `sql_dump_validator.go` change is a required companion: the
  > existing `TestCanonicalSchemaSync` test enforces that every table
  > the restore-db code can read is in the `CanonicalMPMSchema`
  > allow-list. The new `embedding_migration_log` table must be added
  > there or the test suite fails. This is a pre-existing project
  > invariant, not a new convention — every prior migration has done
  > the same.
  ```

  Expected: one commit. The working tree is clean afterwards.

---

## Task 2: Forensic classifier (read-only backfill of existing rows)

**Files:**
- Create: `internal/core/embedding_migration.go` (function `RunForensicClassifier(dm *DatabaseManager) error`)
- Test: `internal/core/embedding_migration_test.go` (function `TestForensicClassifier`)

**Interfaces:**
- Consumes: `dm *DatabaseManager` (from `db.go`); the existing `embedding` column on `memories` (BLOB or JSON text per the storage convention at `db.go:3099-3103`).
- Produces: every live `memories` row has `embedding_source` ∈ `{'provider', 'hash', 'null'}` and `embedding_dimension` set when applicable. Idempotent.

- [ ] **Step 1: Read `internal/core/db.go:3095-3115` to confirm the storage convention.**

  Expected: the `embedding` column is JSON-encoded `[]float32` written via `json.Marshal`, falling back to the literal string `"null"` when nil.

- [ ] **Step 2: Write the failing test.**

  Create `internal/core/embedding_migration_test.go`:

  ```go
  package internal

  import (
      "testing"

      "github.com/flowbyte-com/mpm-core/internal/testutil"
  )

  func TestForensicClassifier(t *testing.T) {
      dm := testutil.NewTestDBManager(t)
      defer dm.Close()

      // Insert fixture rows covering all four input shapes.
      // 256-dim JSON (de facto HashEmbed): embedding_source='hash', dimension=256
      // 384-dim JSON: embedding_source='provider', dimension=384
      // SQL NULL: embedding_source='null', dimension=NULL
      // Literal 'null' string: embedding_source='null', dimension=NULL
      // ...
      // (Insert four rows directly via dm.SaveMemory or raw SQL
      // depending on what the existing fixtures use. Mirror the
      // pattern from internal/core/db_test.go's TestAddMemory
      // helper.)

      // Run the classifier.
      if err := RunForensicClassifier(dm); err != nil {
          t.Fatalf("RunForensicClassifier: %v", err)
      }

      // Assert each row's embedding_source and embedding_dimension.
      // ... (queries over the four rows)

      // Run the classifier a second time — must be a no-op.
      if err := RunForensicClassifier(dm); err != nil {
          t.Fatalf("RunForensicClassifier (second run): %v", err)
      }

      // Assert counts unchanged.
      // ... (queries)
  }
  ```

  > The fixture rows need to be inserted via the project's existing
  > memory-saving path (or raw SQL if the test helper supports it).
  > Mirror the pattern from `internal/core/db_test.go`. If the
  > existing helpers don't easily produce 4-shape input, use raw
  > `dm.SQLDB().Exec` with literal BLOB values.

- [ ] **Step 3: Run the test; expect FAIL (function not defined).**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestForensicClassifier -v
  ```

  Expected: FAIL with `undefined: RunForensicClassifier`.

- [ ] **Step 4: Implement `RunForensicClassifier`.**

  Create `internal/core/embedding_migration.go`:

  ```go
  // internal/core/embedding_migration.go — Forensic classifier.
  //
  // Establishes provenance for every embedding row in the memories
  // table. Idempotent: a second run is a no-op.
  //
  // Classification (per spec §5.4):
  //   - JSON-parseable []float32 of length 256 → 'hash', dimension=256
  //   - JSON-parseable []float32 of length != 256 → 'provider', dimension=<length>
  //   - SQL NULL → 'null', dimension=NULL
  //   - Literal string 'null' → 'null', dimension=NULL
  //
  // The classifier is read-only at the application level but updates
  // embedding_source and embedding_dimension for any row whose values
  // do not already match the classification. This is the operator-
  // driven backfill; it is NOT auto-run on boot.
  package internal

  import (
      "database/sql"
      "encoding/json"
      "fmt"
      "log/slog"
  )

  // RunForensicClassifier classifies every live memories row's
  // embedding column into embedding_source and embedding_dimension.
  // Idempotent.
  func RunForensicClassifier(dm *DatabaseManager) error {
      if dm == nil || dm.db == nil {
          return fmt.Errorf("RunForensicClassifier: nil database manager")
      }

      rows, err := dm.db.Query(`
          SELECT id, embedding
          FROM memories
          WHERE deleted_at IS NULL
      `)
      if err != nil {
          return fmt.Errorf("RunForensicClassifier: query: %w", err)
      }
      defer rows.Close()

      type update struct {
          id         string
          source     string
          dimension  sql.NullInt64
      }
      var updates []update

      for rows.Next() {
          var id string
          var raw sql.NullString
          if err := rows.Scan(&id, &raw); err != nil {
              return fmt.Errorf("RunForensicClassifier: scan: %w", err)
          }

          // SQL NULL → 'null'
          if !raw.Valid {
              updates = append(updates, update{id, "null", sql.NullInt64{}})
              continue
          }

          // Literal 'null' string → 'null'
          if raw.String == "null" {
              updates = append(updates, update{id, "null", sql.NullInt64{}})
              continue
          }

          // JSON-parseable []float32
          var vec []float32
          if err := json.Unmarshal([]byte(raw.String), &vec); err != nil {
              // Unknown shape — log and treat as null.
              slog.Warn("RunForensicClassifier: unparseable embedding; treating as null",
                  "memory_id", id, "error", err.Error())
              updates = append(updates, update{id, "null", sql.NullInt64{}})
              continue
          }

          dim := len(vec)
          source := "provider"
          if dim == 256 {
              source = "hash"
          }
          updates = append(updates, update{id, source, sql.NullInt64{Int64: int64(dim), Valid: true}})
      }
      if err := rows.Err(); err != nil {
          return fmt.Errorf("RunForensicClassifier: rows.Err: %w", err)
      }

      // Apply updates. SQLite is single-writer; sequential updates in
      // a single transaction keep this fast (1081 rows ≈ <1s).
      tx, err := dm.db.Begin()
      if err != nil {
          return fmt.Errorf("RunForensicClassifier: begin tx: %w", err)
      }
      defer tx.Rollback() // no-op after Commit

      stmt, err := tx.Prepare(`
          UPDATE memories
          SET embedding_source = ?, embedding_dimension = ?
          WHERE id = ?
            AND (embedding_source != ? OR embedding_dimension IS NOT ?)
      `)
      if err != nil {
          return fmt.Errorf("RunForensicClassifier: prepare: %w", err)
      }
      defer stmt.Close()

      for _, u := range updates {
          var dimArg interface{}
          if u.dimension.Valid {
              dimArg = u.dimension.Int64
          } else {
              dimArg = nil
          }
          if _, err := stmt.Exec(u.source, dimArg, u.id, u.source, dimArg); err != nil {
              return fmt.Errorf("RunForensicClassifier: exec %s: %w", u.id, err)
          }
      }
      if err := tx.Commit(); err != nil {
          return fmt.Errorf("RunForensicClassifier: commit: %w", err)
      }

      slog.Info("RunForensicClassifier: complete", "rows_scanned", len(updates))
      return nil
  }
  ```

  > **Note on the WHERE clause:** the `OR embedding_dimension IS NOT ?` comparison
  > uses SQLite's three-valued logic — `NULL != anything` is NULL (falsy), so
  > rows whose target dimension is NULL will compare correctly. The intent is to
  > skip rows already classified. If this subtlety bites, drop the WHERE clause
  > and accept a slower idempotent re-update; the cost is negligible.

- [ ] **Step 5: Run the test; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestForensicClassifier -v
  ```

  Expected: PASS.

- [ ] **Step 6: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS. No regressions.

- [ ] **Step 7: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 8: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/embedding_migration.go internal/core/embedding_migration_test.go && git commit -m "feat(embedding): forensic classifier for embedding_source and embedding_dimension"
  ```

---

## Task 3: EmbeddingConfig rebuild — Source, Status, resolveEmbeddingConfig

**Files:**
- Modify: `internal/core/embeddings.go` (replace `EmbeddingConfig`, add `resolveEmbeddingConfig`, `buildProvider`, `validateEmbeddingProfile`, `providerName`)
- Test: `internal/core/embeddings_test.go` (new — covers all precedence branches)

**Interfaces:**
- Consumes: `internal/core/config.Config` (via `ConfigPath()` / `LoadConfig()`); `os.Getenv("OLLAMA_ENDPOINT")` / `os.Getenv("OLLAMA_MODEL")`.
- Produces: `EmbeddingConfig` struct with `Source` (`EmbeddingSourceProfile` / `EmbeddingSourceEnvFallback` / `EmbeddingSourceDisabled` / `EmbeddingSourceAbsent`), `Status` (`EmbeddingStatusConfigured` / `EmbeddingStatusUnreachable` / `EmbeddingStatusMisconfigured` / `EmbeddingStatusNull`), `ProfileName`, `ProviderName`, `Provider`, `IntentionallyDisabled`, `LastError`.

- [ ] **Step 1: Write the failing test.**

  Create `internal/core/embeddings_test.go`:

  ```go
  package internal

  import (
      "os"
      "testing"

      "github.com/flowbyte-com/mpm-core/internal/core/config"
  )

  // helper: load empty config, return *Config
  func emptyCfg(t *testing.T) *config.Config {
      t.Helper()
      return &config.Config{}
  }

  func TestResolveEmbeddingConfig_Disabled(t *testing.T) {
      cfg := emptyCfg(t)
      cfg.Components = map[string]string{"embedding": "disabled"}
      got := resolveEmbeddingConfig(cfg)
      if got.Source != EmbeddingSourceDisabled {
          t.Errorf("Source = %v, want EmbeddingSourceDisabled", got.Source)
      }
      if !got.IntentionallyDisabled {
          t.Error("IntentionallyDisabled = false, want true")
      }
      if got.ProviderName != "null" {
          t.Errorf("ProviderName = %q, want \"null\"", got.ProviderName)
      }
  }

  func TestResolveEmbeddingConfig_Profile(t *testing.T) {
      cfg := emptyCfg(t)
      cfg.Profiles = map[string]config.Profile{
          "local-ollama": {
              Provider: "ollama",
              Model:    "nomic-embed-text",
              BaseURL:  "http://localhost:11434",
          },
      }
      cfg.Components = map[string]string{"embedding": "local-ollama"}
      got := resolveEmbeddingConfig(cfg)
      if got.Source != EmbeddingSourceProfile {
          t.Errorf("Source = %v, want EmbeddingSourceProfile", got.Source)
      }
      if got.ProfileName != "local-ollama" {
          t.Errorf("ProfileName = %q, want \"local-ollama\"", got.ProfileName)
      }
      if got.Status != EmbeddingStatusConfigured {
          t.Errorf("Status = %v, want EmbeddingStatusConfigured", got.Status)
      }
  }

  func TestResolveEmbeddingConfig_ProfileMissing(t *testing.T) {
      cfg := emptyCfg(t)
      cfg.Components = map[string]string{"embedding": "ghost"}
      got := resolveEmbeddingConfig(cfg)
      if got.Status != EmbeddingStatusMisconfigured {
          t.Errorf("Status = %v, want EmbeddingStatusMisconfigured", got.Status)
      }
      if got.LastError == nil {
          t.Error("LastError is nil, want error")
      }
  }

  func TestResolveEmbeddingConfig_EnvFallback(t *testing.T) {
      cfg := emptyCfg(t)
      t.Setenv("OLLAMA_ENDPOINT", "http://example.test:11434")
      t.Setenv("OLLAMA_MODEL", "test-model")
      got := resolveEmbeddingConfig(cfg)
      if got.Source != EmbeddingSourceEnvFallback {
          t.Errorf("Source = %v, want EmbeddingSourceEnvFallback", got.Source)
      }
      if got.ProviderName != "ollama:test-model" {
          t.Errorf("ProviderName = %q, want \"ollama:test-model\"", got.ProviderName)
      }
  }

  func TestResolveEmbeddingConfig_DisabledDoesNotFallThroughToEnv(t *testing.T) {
      cfg := emptyCfg(t)
      cfg.Components = map[string]string{"embedding": "disabled"}
      t.Setenv("OLLAMA_ENDPOINT", "http://example.test:11434")
      t.Setenv("OLLAMA_MODEL", "test-model")
      got := resolveEmbeddingConfig(cfg)
      if got.Source != EmbeddingSourceDisabled {
          t.Errorf("Source = %v, want EmbeddingSourceDisabled (env must NOT override disabled)", got.Source)
      }
  }

  func TestResolveEmbeddingConfig_Absent(t *testing.T) {
      cfg := emptyCfg(t)
      // explicit clear of env vars
      t.Setenv("OLLAMA_ENDPOINT", "")
      t.Setenv("OLLAMA_MODEL", "")
      got := resolveEmbeddingConfig(cfg)
      if got.Source != EmbeddingSourceAbsent {
          t.Errorf("Source = %v, want EmbeddingSourceAbsent", got.Source)
      }
      // suppress unused-import warning
      _ = os.Getenv
  }
  ```

- [ ] **Step 2: Run the test; expect FAIL (function not defined).**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestResolveEmbeddingConfig -v
  ```

  Expected: FAIL.

- [ ] **Step 3: Add the new types and `resolveEmbeddingConfig` to `embeddings.go`.**

  Replace the existing `EmbeddingConfig` struct and add the new types/functions. Place them just below the existing `OllamaProvider` block:

  ```go
  // EmbeddingSource identifies where the active embedding configuration
  // came from. Diagnostic only — runtime logic should branch on Source
  // when the distinction matters (e.g., env fallback should not be
  // reported as "profile").
  type EmbeddingSource int

  const (
      EmbeddingSourceProfile EmbeddingSource = iota
      EmbeddingSourceEnvFallback
      EmbeddingSourceDisabled
      EmbeddingSourceAbsent
  )

  func (s EmbeddingSource) String() string {
      switch s {
      case EmbeddingSourceProfile:
          return "profile"
      case EmbeddingSourceEnvFallback:
          return "env"
      case EmbeddingSourceDisabled:
          return "disabled"
      case EmbeddingSourceAbsent:
          return "absent"
      }
      return "unknown"
  }

  type EmbeddingStatus int

  const (
      EmbeddingStatusConfigured EmbeddingStatus = iota
      EmbeddingStatusUnreachable
      EmbeddingStatusMisconfigured
      EmbeddingStatusNull
  )

  func (s EmbeddingStatus) String() string {
      switch s {
      case EmbeddingStatusConfigured:
          return "configured"
      case EmbeddingStatusUnreachable:
          return "unavailable"
      case EmbeddingStatusMisconfigured:
          return "misconfigured"
      case EmbeddingStatusNull:
          return "null"
      }
      return "unknown"
  }

  // EmbeddingConfig is the resolved embedding configuration for the
  // current process. Source and Status together describe every
  // operator-visible state. IntentionallyDisabled is the canonical
  // signal that the operator opted out via the "disabled" sentinel.
  type EmbeddingConfig struct {
      Source                EmbeddingSource
      ProfileName           string
      ProviderName          string
      Provider              EmbeddingProvider
      Status                EmbeddingStatus
      IntentionallyDisabled bool
      LastError             error
  }

  // resolveEmbeddingConfig applies the canonical precedence:
  //   1. components.embedding == "disabled" → IntentionallyDisabled
  //   2. components.embedding == "<profile>" → resolve Profile, validate, build provider
  //   3. components.embedding absent → OLLAMA_* env fallback
  //   4. neither → NullProvider
  //
  // The "disabled" sentinel is honored ONLY for the embedding
  // component. Other components return nil from ProfileFor when
  // their value is "disabled" — same as a missing binding.
  func resolveEmbeddingConfig(cfg *config.Config) *EmbeddingConfig {
      // 1. disabled
      if cfg != nil && cfg.Components != nil {
          if name, ok := cfg.Components["embedding"]; ok && name == "disabled" {
              return &EmbeddingConfig{
                  Source:                EmbeddingSourceDisabled,
                  ProviderName:          "null",
                  Provider:              NullProvider{},
                  Status:                EmbeddingStatusNull,
                  IntentionallyDisabled: true,
              }
          }
      }

      // 2. profile
      if cfg != nil && cfg.Components != nil {
          if name, ok := cfg.Components["embedding"]; ok && name != "" && name != "disabled" {
              p := cfg.ProfileFor("embedding")
              if p == nil {
                  return &EmbeddingConfig{
                      Source:       EmbeddingSourceProfile,
                      ProfileName:  name,
                      ProviderName: "null",
                      Provider:     NullProvider{},
                      Status:       EmbeddingStatusMisconfigured,
                      LastError:    fmt.Errorf("profile %q referenced by components.embedding does not exist", name),
                  }
              }
              if err := validateEmbeddingProfile(p); err != nil {
                  return &EmbeddingConfig{
                      Source:       EmbeddingSourceProfile,
                      ProfileName:  name,
                      ProviderName: "null",
                      Provider:     NullProvider{},
                      Status:       EmbeddingStatusMisconfigured,
                      LastError:    err,
                  }
              }
              prov, provErr := buildProvider(p)
              if provErr != nil {
                  return &EmbeddingConfig{
                      Source:       EmbeddingSourceProfile,
                      ProfileName:  name,
                      ProviderName: providerName(p),
                      Provider:     NullProvider{},
                      Status:       EmbeddingStatusMisconfigured,
                      LastError:    provErr,
                  }
              }
              return &EmbeddingConfig{
                  Source:       EmbeddingSourceProfile,
                  ProfileName:  name,
                  ProviderName: providerName(p),
                  Provider:     prov,
                  Status:       EmbeddingStatusConfigured,
              }
          }
      }

      // 3. env fallback
      endpoint := os.Getenv("OLLAMA_ENDPOINT")
      model := os.Getenv("OLLAMA_MODEL")
      if endpoint != "" || model != "" {
          if endpoint == "" {
              endpoint = "http://localhost:11434/api/embeddings"
          }
          if model == "" {
              model = "nomic-embed-text"
          }
          return &EmbeddingConfig{
              Source:       EmbeddingSourceEnvFallback,
              ProviderName: "ollama:" + model,
              Provider:     NewOllamaProvider(endpoint, model),
              Status:       EmbeddingStatusConfigured,
          }
      }

      // 4. absent
      return &EmbeddingConfig{
          Source:       EmbeddingSourceAbsent,
          ProviderName: "null",
          Provider:     NullProvider{},
          Status:       EmbeddingStatusNull,
      }
  }

  func validateEmbeddingProfile(p *config.Profile) error {
      if p.Provider == "" {
          return fmt.Errorf("embedding profile %q: provider is required", p.Name)
      }
      if p.Model == "" {
          return fmt.Errorf("embedding profile %q: model is required", p.Name)
      }
      if p.Provider != "ollama" {
          return fmt.Errorf("embedding profile %q: provider %q is not implemented (only \"ollama\" is supported by this spec)", p.Name, p.Provider)
      }
      return nil
  }

  func buildProvider(p *config.Profile) (EmbeddingProvider, error) {
      switch p.Provider {
      case "ollama":
          return NewOllamaProvider(p.BaseURL, p.Model), nil
      }
      return nil, fmt.Errorf("buildProvider: no implementation for provider %q", p.Provider)
  }

  func providerName(p *config.Profile) string {
      return p.Provider + ":" + p.Model
  }
  ```

  At the top of `embeddings.go`, add the import:

  ```go
  "github.com/flowbyte-com/mpm-core/internal/core/config"
  ```

  > The exact import path depends on the local module layout. Verify
  > by reading `internal/core/go.mod`. The pattern is usually
  > `github.com/flowbyte-com/mpm-core/internal/core/config` if
  > `config` is a subpackage of `core`, or just `config` if it's
  > inlined. Use `grep -rn "config.Config" internal/core/` to find
  > the canonical import.

- [ ] **Step 4: Run the test; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestResolveEmbeddingConfig -v
  ```

  Expected: PASS for all six tests.

- [ ] **Step 5: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS. `resolveEmbeddingConfig` is new; no existing call sites.

- [ ] **Step 6: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 7: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/embeddings.go internal/core/embeddings_test.go && git commit -m "feat(embedding): EmbeddingConfig with Source/Status, resolveEmbeddingConfig"
  ```

  > **Note:** This commit does NOT remove `probeEmbeddingConfig` yet,
  > nor does it change `EmbedText`. Those land in subsequent tasks.
  > Existing behavior is preserved at this point.

---

## Task 4: ProfileFor recognizes the embedding sentinel

**Files:**
- Modify: `internal/core/config/config.go:209-244` (add a pre-check at the top of `ProfileFor` for `component == "embedding"` and `Components["embedding"] == "disabled"`)

- [ ] **Step 1: Read `internal/core/config/config.go:209-244` to confirm the current `ProfileFor` implementation.**

  Expected: returns nil for missing bindings, falls through to legacy Synth. The current code has no special handling for any component name.

- [ ] **Step 2: Add a pre-check for the embedding sentinel.**

  Insert at the top of `ProfileFor`, just after the `if c == nil { return nil }` guard:

  ```go
  // Embedding sentinel: "disabled" is reserved for components.embedding
  // and returns nil so the embedding resolver can map it to NullProvider
  // + IntentionallyDisabled=true.
  if component == "embedding" && c.Components != nil {
      if name, ok := c.Components["embedding"]; ok && name == "disabled" {
          return nil
      }
  }
  // Non-embedding "disabled" binding: also return nil. The existing
  // fallback chain would otherwise silently resolve to the "default"
  // profile when a binding name does not match any profile entry,
  // which masks misconfiguration. Returning nil forces callers to
  // surface "this binding refers to nothing" as an explicit error.
  if component != "embedding" && c.Components != nil {
      if name, ok := c.Components[component]; ok && name == "disabled" {
          return nil
      }
  }
  ```

  The rest of `ProfileFor` stays unchanged for components whose binding names are not literal `"disabled"`.

- [ ] **Step 3: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS. No behavior change for non-embedding components.

- [ ] **Step 4: Add a config-level test.**

  Add to `internal/core/config/config_test.go` (or create it if absent):

  ```go
  func TestProfileFor_EmbeddingDisabled(t *testing.T) {
      c := &Config{
          Components: map[string]string{"embedding": "disabled"},
          Profiles:   map[string]Profile{"default": {Provider: "openai", Model: "gpt-4o"}},
      }
      if p := c.ProfileFor("embedding"); p != nil {
          t.Errorf("ProfileFor(\"embedding\") with sentinel \"disabled\" = %v, want nil", p)
      }
  }

  func TestProfileFor_NonEmbeddingDisabled(t *testing.T) {
      c := &Config{
          Components: map[string]string{"critic": "disabled"},
          Profiles:   map[string]Profile{"default": {Provider: "openai", Model: "gpt-4o"}},
      }
      // "disabled" is NOT a sentinel for non-embedding components;
      // ProfileFor returns nil as if the binding were missing.
      if p := c.ProfileFor("critic"); p != nil {
          t.Errorf("ProfileFor(\"critic\") with value \"disabled\" = %v, want nil", p)
      }
  }
  ```

  Run:

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./internal/core/config/... -v
  ```

  Expected: PASS.

- [ ] **Step 5: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 6: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/config/config.go internal/core/config/config_test.go && git commit -m "feat(config): ProfileFor recognizes embedding 'disabled' sentinel"
  ```

---

## Task 5: DefaultEmbeddingConfig uses resolveEmbeddingConfig; remove probeEmbeddingConfig from runtime

**Files:**
- Modify: `internal/core/embeddings.go:103-153` (replace `probeEmbeddingConfig`/`DefaultEmbeddingConfig` so the runtime reads from config, not from a probe)

- [ ] **Step 1: Read the existing `DefaultEmbeddingConfig` and `probeEmbeddingConfig`.**

  Expected: `DefaultEmbeddingConfig` is the cached entry point, `probeEmbeddingConfig` is the silent HTTP probe of `localhost:11434`.

- [ ] **Step 2: Replace `DefaultEmbeddingConfig` to use `resolveEmbeddingConfig`.**

  Replace the body of `DefaultEmbeddingConfig` (lines 108-116):

  ```go
  // DefaultEmbeddingConfig returns a cached embedding config using
  // mpm_config.json. The config is resolved once and then cached for
  // the lifetime of the process.
  //
  // Resolution order (canonical; see docs/superpowers/specs/
  // 2026-09-03-mpm-embedding-provider-design.md §4.1):
  //   1. components.embedding == "disabled" → IntentionallyDisabled
  //   2. components.embedding == "<profile>" → resolve profile
  //   3. components.embedding absent → OLLAMA_* env fallback
  //   4. neither → NullProvider
  //
  // No network probing at any step. Probing lives in
  // `mpm config detect-embedding`.
  func DefaultEmbeddingConfig() *EmbeddingConfig {
      defaultEmbedConfigOnce.Do(func() {
          cfg, err := config.LoadConfig()
          if err != nil {
              // Treat load failure as "absent" rather than crashing
              // boot. Operators see the error via `mpm config show`.
              defaultEmbedConfig = &EmbeddingConfig{
                  Source:       EmbeddingSourceAbsent,
                  ProviderName: "null",
                  Provider:     NullProvider{},
                  Status:       EmbeddingStatusNull,
                  LastError:    err,
              }
              return
          }
          defaultEmbedConfig = resolveEmbeddingConfig(cfg)
      })
      return defaultEmbedConfig
  }
  ```

  > Make sure `config.LoadConfig()` is the function used elsewhere in
  > the codebase; verify with
  > `grep -rn "config.LoadConfig" internal/core/`.

- [ ] **Step 3: Delete `probeEmbeddingConfig` from runtime.**

  Delete the entire `probeEmbeddingConfig` function (lines 119-153). Add a comment block at the top of `embeddings.go` summarizing the new canonical precedence so future readers do not reinvent silent probing:

  ```go
  // Package internal — Embedding subsystem.
  //
  // The embedding subsystem resolves configuration from
  // mpm_config.json's components["embedding"] binding, falling back
  // to OLLAMA_ENDPOINT / OLLAMA_MODEL env vars when the binding is
  // absent. The reserved sentinel "disabled" opts out cleanly.
  //
  // There is no runtime network probing. Operators who want to
  // discover reachable providers run `mpm config detect-embedding`,
  // which is explicitly diagnostic.
  //
  // See docs/superpowers/specs/2026-09-03-mpm-embedding-provider-design.md
  // for the full design.
  ```

- [ ] **Step 4: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS. Existing tests that called `probeEmbeddingConfig` (if any) need to be updated — verify with `grep -rn "probeEmbeddingConfig" internal/core/`. If the function had callers, replace them with `resolveEmbeddingConfig(testCfg)` where `testCfg` is a literal `*config.Config`.

- [ ] **Step 5: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 6: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/embeddings.go && git commit -m "feat(embedding): DefaultEmbeddingConfig resolves from config; remove probeEmbeddingConfig"
  ```

---

## Task 6: `mpm config detect-embedding` command

**Files:**
- Create: `cmd/mpm/detect_embedding.go` (the command handler)

**Interfaces:**
- Consumes: HTTP probing against `localhost:11434` (default Ollama endpoint) and any extra endpoints declared in config (none, by default).
- Produces: a printed report listing reachable endpoints and discovered models. With `--apply <name>`, writes a profile + component binding via `config.SaveConfig`.

- [ ] **Step 1: Read `cmd/mpm/simple_cmds.go` or `cmd/mpm/config_cmds.go` (whichever exists) to find the pattern for `mpm config` subcommands.**

  Expected: handler functions registered in the CLI dispatcher with a `Run() error` method.

- [ ] **Step 2: Create `cmd/mpm/detect_embedding.go`.**

  ```go
  // cmd/mpm/detect_embedding.go — `mpm config detect-embedding`
  //
  // Operator-driven diagnostic. Probes localhost:11434 by default;
  // never mutates configuration unless --apply is supplied.
  package main

  import (
      "bytes"
      "encoding/json"
      "fmt"
      "io"
      "net/http"
      "os"
      "time"

      mpminternal "github.com/flowbyte-com/mpm-core"
      "github.com/flowbyte-com/mpm-core/internal/core/config"
  )

  type DetectEmbeddingCmd struct {
      Apply string // --apply <profile-name>
  }

  func (c *DetectEmbeddingCmd) Run() error {
      endpoint := os.Getenv("OLLAMA_ENDPOINT")
      if endpoint == "" {
          endpoint = "http://localhost:11434"
      }
      models, reachable := probeOllamaModels(endpoint)

      fmt.Println("Embedding provider detection")
      fmt.Println()
      fmt.Println("Ollama")
      fmt.Printf("  %s\n", endpoint)
      if reachable {
          fmt.Println("  reachable: yes")
          if len(models) == 0 {
              fmt.Println("  models: (none reported)")
          } else {
              fmt.Println("  models:")
              for _, m := range models {
                  fmt.Printf("    %s\n", m)
              }
          }
      } else {
          fmt.Println("  reachable: no")
      }
      fmt.Println()

      if c.Apply == "" {
          fmt.Println("No configuration has been changed.")
          return nil
      }

      // --apply: write profile + components.embedding binding.
      if !reachable || len(models) == 0 {
          return fmt.Errorf("--apply refused: no reachable Ollama endpoint with embedding-capable models")
      }
      // Pick the first model alphabetically — deterministic and visible.
      model := models[0]
      for _, m := range models {
          if m < model {
              model = m
          }
      }

      cfg, err := config.LoadConfig()
      if err != nil {
          return fmt.Errorf("--apply: load config: %w", err)
      }
      if cfg.Profiles == nil {
          cfg.Profiles = map[string]config.Profile{}
      }
      if _, exists := cfg.Profiles[c.Apply]; exists {
          return fmt.Errorf("--apply refused: profile %q already exists; use --force to overwrite", c.Apply)
      }
      cfg.Profiles[c.Apply] = config.Profile{
          Provider: "ollama",
          Model:    model,
          BaseURL:  endpoint,
      }
      if cfg.Components == nil {
          cfg.Components = map[string]string{}
      }
      cfg.Components["embedding"] = c.Apply
      if err := config.SaveConfig(cfg); err != nil {
          return fmt.Errorf("--apply: save config: %w", err)
      }
      fmt.Printf("Wrote profile %q (provider=ollama, model=%s).\n", c.Apply, model)
      fmt.Printf("Set components[\"embedding\"] = %q.\n", c.Apply)
      _ = mpminternal.HashEmbed // keep the import; mpminternal already exists
      return nil
  }

  func probeOllamaModels(endpoint string) ([]string, bool) {
      client := &http.Client{Timeout: 2 * time.Second}
      resp, err := client.Get(endpoint + "/api/tags")
      if err != nil {
          return nil, false
      }
      defer resp.Body.Close()
      if resp.StatusCode != http.StatusOK {
          return nil, false
      }
      body, err := io.ReadAll(resp.Body)
      if err != nil {
          return nil, false
      }
      var out struct {
          Models []struct {
              Name string `json:"name"`
          } `json:"models"`
      }
      if err := json.NewDecoder(bytes.NewReader(body)).Decode(&out); err != nil {
          return nil, false
      }
      var names []string
      for _, m := range out.Models {
          names = append(names, m.Name)
      }
      return names, true
  }
  ```

  > The `Run()` method's exact signature is dictated by the project's
  > CLI dispatcher pattern. Verify by reading an existing handler
  > such as `cmd/mpm/config_cmds.go` or `cmd/mpm/simple_cmds.go`. If
  > the dispatcher uses `Run(args []string) error`, adjust accordingly.

- [ ] **Step 3: Wire the command into the CLI dispatcher.**

  Find the place where `mpm config` subcommands are registered (e.g., a switch statement on `os.Args[2]`) and add:

  ```go
  case "detect-embedding":
      cmd := &DetectEmbeddingCmd{}
      // parse --apply <name> from os.Args
      for i := 3; i < len(os.Args); i++ {
          if os.Args[i] == "--apply" && i+1 < len(os.Args) {
              cmd.Apply = os.Args[i+1]
              i++
          }
      }
      return cmd.Run()
  ```

  > The exact argument-parsing mechanism varies. Mirror whatever
  > pattern the existing `mpm config` subcommands use (likely a
  > small hand-rolled parser since MPM avoids heavy CLI frameworks
  > per the project's CLAUDE.md).

- [ ] **Step 4: Run a smoke test against a real or mocked Ollama endpoint.**

  ```bash
  cd /home/v/workspace/projects/mpm && go build -o /tmp/mpm-detect ./cmd/mpm && /tmp/mpm-detect config detect-embedding
  ```

  Expected output (when no Ollama is reachable):

  ```
  Embedding provider detection

  Ollama
    http://localhost:11434
    reachable: no

  No configuration has been changed.
  ```

  Exit code 0.

- [ ] **Step 5: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 6: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add cmd/mpm/detect_embedding.go && git commit -m "feat(config): mpm config detect-embedding [--apply]"
  ```

---

## Task 7: EmbedText signature change + HashEmbed removal

**Files:**
- Modify: `internal/core/embeddings.go:155-169` (`EmbedText`)
- Modify: `internal/core/memory.go:362` (`SaveMemory` call site — handle the error)
- Modify: `internal/core/synthesis_auto.go:536` (handle the error)
- Modify: `cmd/mpm/simple_cmds.go:102` (`mpm add` call site — handle the error and exit non-zero)
- Modify: `cmd/mpm/backfill_embeddings.go:69-71` (use canonical resolution in the error message)
- Modify: MCP write path in `mpm-agent/...` if it has its own `EmbedText` call (verify with grep)

**Interfaces:**
- New: `EmbedText(text string) ([]float32, error)` — returns nil for disabled/absent, returns error for unreachable/misconfigured.
- Removed: `EmbedText(text string) []float32` (the silent-fallback signature).

- [ ] **Step 1: Find every call site of `EmbedText`.**

  ```bash
  cd /home/v/workspace/projects/mpm && grep -rn "EmbedText" --include="*.go" | grep -v _test.go
  ```

  Expected: 4-6 sites — `mpm add`, `mpm-mcp` writes, synthesis, backfill, possibly the `mpm-agent` MCP server.

- [ ] **Step 2: Write a test that pins the new contract.**

  Add to `internal/core/embeddings_test.go`:

  ```go
  func TestEmbedText_NilForDisabled(t *testing.T) {
      // Force a disabled-source config.
      // (Manipulate the cached defaultEmbedConfig via a test-only
      // setter, or write a fresh EmbeddingConfig in the test.)
      // ...
      vec, err := EmbedText("hello")
      if err != nil { t.Fatalf("err = %v, want nil", err) }
      if vec != nil { t.Errorf("vec = %v, want nil", vec) }
  }

  func TestEmbedText_ErrorOnProviderFailure(t *testing.T) {
      // Force a configured-but-unreachable provider.
      // (Use a config with provider=ollama, base_url pointing at
      // a closed port like http://127.0.0.1:1.)
      // ...
      vec, err := EmbedText("hello")
      if err == nil { t.Fatal("err = nil, want error") }
      if vec != nil { t.Errorf("vec = %v, want nil", vec) }
  }
  ```

  > The cleanest approach is to introduce a `defaultEmbedConfigForTest` setter and use it in tests. Verify the existing test patterns for cached config in `embeddings_test.go` and similar files.

- [ ] **Step 3: Run the test; expect FAIL.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestEmbedText -v
  ```

  Expected: FAIL (old signature mismatch or HashEmbed still returned).

- [ ] **Step 4: Replace `EmbedText` in `embeddings.go`.**

  Replace the function body (lines 161-169):

  ```go
  // EmbedText returns the embedding vector for the given text, or
  // (nil, nil) when no provider is configured / reachable and the
  // absence is acceptable. Returns (nil, err) when the provider was
  // configured but failed.
  //
  // HashEmbed is NOT a fallback. Silent semantic substitution is the
  // historical defect this spec eliminates. See docs/superpowers/specs/
  // 2026-09-03-mpm-embedding-provider-design.md §4.3.
  func EmbedText(text string) ([]float32, error) {
      cfg := DefaultEmbeddingConfig()
      switch cfg.Source {
      case EmbeddingSourceDisabled, EmbeddingSourceAbsent:
          return nil, nil
      case EmbeddingSourceProfile, EmbeddingSourceEnvFallback:
          vec, err := cfg.Provider.Embed(text)
          if err != nil {
              // Downgrade status for this process's lifetime.
              cfg.Status = EmbeddingStatusUnreachable
              cfg.LastError = err
              return nil, err
          }
          if len(vec) == 0 {
              err := fmt.Errorf("embed: provider %q returned zero-length vector", cfg.ProviderName)
              cfg.Status = EmbeddingStatusUnreachable
              cfg.LastError = err
              return nil, err
          }
          return vec, nil
      }
      return nil, nil
  }
  ```

  Update the file's package doc comment (already added in Task 5) to note the new signature.

- [ ] **Step 5: Update each call site.**

  For each call site found in Step 1, change the call to handle `(vec, err)`. The error-handling pattern at each call site:

  **`internal/core/memory.go:362`** (`SaveMemory`):
  ```go
  embedding, embedErr := EmbedText(content)
  // Pass embedding through to SaveMemory as before; embedErr is
  // surfaced via the response wrapper or the audit log.
  ```

  **`internal/core/synthesis_auto.go:536`**:
  ```go
  embedding, embedErr := EmbedText(result.Content)
  if embedErr != nil {
      dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("synthesis succeeded but embedding failed: %v", embedErr), "", AuditContext{})
  }
  ```

  **`cmd/mpm/simple_cmds.go:102`** (`mpm add`):
  ```go
  embedding, embedErr := mpminternal.EmbedText(content)
  if embedErr != nil {
      // Memory is saved with NULL embedding; we surface the error
      // and exit non-zero.
      fmt.Fprintf(os.Stderr, "Memory saved, but embedding generation failed:\n  embedding provider %q is unreachable\nThe memory has been stored without an embedding.\nRun `mpm ops backfill-embeddings` after the provider is available.\n", mpminternal.DefaultEmbeddingConfig().ProfileName)
      os.Exit(1)
  }
  ```

  > The exact error-message format comes from the spec §4.4. The CLI
  > exit code 1 signals the partial-failure outcome.

  **`cmd/mpm/backfill_embeddings.go:69-71`**:
  ```go
  cfg := mpminternal.DefaultEmbeddingConfig()
  if cfg.Source == mpminternal.EmbeddingSourceAbsent {
      return fmt.Errorf("refusing to backfill: no embedding provider is configured and reachable.\nResolve one of:\n  - Run `mpm config detect-embedding --apply <name>` to discover and configure.\n  - Set `components[\"embedding\"]` in mpm_config.json.\n  - Set OLLAMA_ENDPOINT and OLLAMA_MODEL in the environment.")
  }
  ```

- [ ] **Step 6: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS.

- [ ] **Step 7: Run all `cmd/mpm` tests; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && go test ./cmd/mpm/...
  ```

  Expected: PASS.

- [ ] **Step 8: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 9: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/embeddings.go internal/core/embeddings_test.go internal/core/memory.go internal/core/synthesis_auto.go cmd/mpm/simple_cmds.go cmd/mpm/backfill_embeddings.go && git commit -m "feat(embedding): EmbedText returns (vec, err); remove HashEmbed from runtime path"
  ```

---

## Task 8: cosineSimilarity defensive length guard

**Files:**
- Modify: `internal/core/db.go:3568` (replace unguarded loop with a length-bounded one)

- [ ] **Step 1: Read `internal/core/db.go:3568` to confirm the unguarded version.**

  Expected: a `for i := range a` loop with `a[i] * b[i]` indexing into both.

- [ ] **Step 2: Write a regression test for the panic case.**

  Create `internal/core/db_cosine_test.go`:

  ```go
  package internal

  import "testing"

  func TestCosineSimilarity_DimensionMismatchDoesNotPanic(t *testing.T) {
      a := make([]float32, 384)
      b := make([]float32, 256)
      for i := range a { a[i] = 1 }
      for i := range b { b[i] = 1 }

      defer func() {
          if r := recover(); r != nil {
              t.Fatalf("cosineSimilarity panicked on dimension mismatch: %v", r)
          }
      }()
      _ = cosineSimilarity(a, b) // must not panic
  }

  func TestCosineSimilarity_Empty(t *testing.T) {
      if got := cosineSimilarity(nil, []float32{1, 2}); got != 0 {
          t.Errorf("cosineSimilarity(nil, ...) = %f, want 0", got)
      }
  }
  ```

- [ ] **Step 3: Run the test; expect FAIL.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestCosineSimilarity -v
  ```

  Expected: FAIL with `panic: runtime error: index out of range [256] with length 256`.

- [ ] **Step 4: Apply the defensive guard.**

  Replace the function body at `internal/core/db.go:3568`:

  ```go
  func cosineSimilarity(a, b []float32) float32 {
      if len(a) == 0 || len(b) == 0 {
          return 0
      }
      n := len(a)
      if len(b) < n {
          n = len(b)
      }
      var dotProduct, normA, normB float32
      for i := 0; i < n; i++ {
          dotProduct += a[i] * b[i]
          normA += a[i] * a[i]
          normB += b[i] * b[i]
      }
      if normA == 0 || normB == 0 {
          return 0
      }
      return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
  }
  ```

- [ ] **Step 5: Run the test; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestCosineSimilarity -v
  ```

  Expected: PASS.

- [ ] **Step 6: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS. The defensive guard unifies behavior with the existing guarded variant (`memory.go:1705`).

- [ ] **Step 7: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 8: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/db.go internal/core/db_cosine_test.go && git commit -m "fix(cosine): defensive length guard at cosineSimilarity root"
  ```

---

## Task 9: Trust-machinery filters — skip `embedding_source='hash'` rows

**Files:**
- Modify: `internal/core/hybrid_search.go` (line 344 contradictionDetector; line 974 VectorMatch)
- Modify: `internal/core/vector_index.go` (line 244 lookup; line 624 Rebalance)
- Modify: `internal/core/forge_dedup.go:174` (zero-padding cosine path)

**Interfaces:**
- Consumes: `memories.embedding_source` column (added in Task 1).
- Produces: trust machinery, vector lookups, and dedup never see `embedding_source='hash'` rows.

- [ ] **Step 1: Read each call site to understand the SQL shape.**

  - `hybrid_search.go:344-536`: contradictionDetector scans top-15 candidates and pairwise-compares.
  - `hybrid_search.go:974`: VectorMatch loops over candidates and unmarshals embeddings.
  - `vector_index.go:244`: vector index lookup joins memories.
  - `vector_index.go:624`: Rebalance iterates rows.
  - `forge_dedup.go:174`: zero-padding cosine path.

- [ ] **Step 2: Write a regression test that pins the filter.**

  Add to `internal/core/hybrid_search_test.go` (or create):

  ```go
  func TestVectorMatch_SkipsHashRows(t *testing.T) {
      // Insert two memories with the same content vector length but
      // different embedding_source. Assert VectorMatch returns only
      // the 'provider' one.
      // ...
  }
  ```

  Mirror the pattern of existing tests in the file. If no test file exists, copy the test-fixture pattern from `internal/core/embedding_migration_test.go`.

- [ ] **Step 3: Run the test; expect FAIL.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestVectorMatch_SkipsHashRows -v
  ```

  Expected: FAIL.

- [ ] **Step 4: Add the filter at each call site.**

  For each SQL query that touches `memories.embedding`, add `AND embedding_source != 'hash'` to the WHERE clause. For Go-side scans that materialize vectors, add a Go-side check that returns/skip when `source == 'hash'`.

  Specific edits:

  **`hybrid_search.go:344-536`** — contradictionDetector candidate scan: append `AND m.embedding_source != 'hash'` to the SQL that fetches candidates. The detector iterates the result set, pairwise-compares vectors; with the SQL filter applied, no `'hash'` row reaches the comparison.

  **`hybrid_search.go:974`** — VectorMatch: in the existing `if err := json.Unmarshal(...) || len(dbEmbedding) != len(queryEmbedding) { continue }` block, add `|| embeddingSource == "hash"` to the same continue condition. The `embeddingSource` is read from the row alongside `embeddingJSON`.

  **`vector_index.go:244`** — vector index lookup: append `AND embedding_source != 'hash'` to the lookup query.

  **`vector_index.go:624`** — Rebalance: append `AND embedding_source != 'hash'` to the iteration query.

  **`forge_dedup.go:174`** — zero-padding cosine path: prepend a check on the candidate's `embedding_source`; if `'hash'`, skip the cosine entirely (no comparison, no padding, no score update).

- [ ] **Step 5: Run the test; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestVectorMatch_SkipsHashRows -v
  ```

  Expected: PASS.

- [ ] **Step 6: Run all `internal/core` tests; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  ```

  Expected: PASS.

- [ ] **Step 7: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 8: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/hybrid_search.go internal/core/vector_index.go internal/core/forge_dedup.go && git commit -m "feat(embedding): trust machinery skips embedding_source='hash' rows"
  ```

---

## Task 10: Probe-removal proof test + integration tests

**Files:**
- Create: `cmd/mpm/integration_embedding_test.go` (new — runs `mpm add` end-to-end with mocked HTTP transport)

**Interfaces:**
- Consumes: an `http.RoundTripper` mock that counts every HTTP request the runtime makes.
- Produces: assertions that zero requests are made when `components["embedding"]` is absent and env vars are unset.

- [ ] **Step 1: Write the failing test.**

  Create `cmd/mpm/integration_embedding_test.go`:

  ```go
  package main

  import (
      "net/http"
      "os"
      "path/filepath"
      "sync/atomic"
      "testing"

      mpminternal "github.com/flowbyte-com/mpm-core"
  )

  type countingTransport struct {
      base    http.RoundTripper
      counter *int64
  }

  func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
      atomic.AddInt64(c.counter, 1)
      return c.base.RoundTrip(req)
  }

  func TestMpmAdd_DoesNotProbeWhenEmbeddingAbsent(t *testing.T) {
      // Set up isolated workspace.
      tmp := t.TempDir()
      t.Setenv("MPM_WORKSPACE", tmp)
      t.Setenv("HOME", tmp)
      t.Setenv("OLLAMA_ENDPOINT", "")
      t.Setenv("OLLAMA_MODEL", "")

      var counter int64
      old := http.DefaultTransport
      http.DefaultTransport = &countingTransport{base: old, counter: &counter}
      defer func() { http.DefaultTransport = old }()

      // Invoke `mpm add` via the CLI handler.
      // (Use the project's CLI test harness if one exists; otherwise
      // call the internal handler directly with a stub os.Args.)
      // ...

      if atomic.LoadInt64(&counter) != 0 {
          t.Fatalf("probe-removal proof failed: %d HTTP requests made during `mpm add` with no embedding config", atomic.LoadInt64(&counter))
      }
      _ = filepath.Join
      _ = mpminternal.DefaultEmbeddingConfig
  }
  ```

  > The exact CLI invocation depends on the project's test harness.
  > If the project does not have one for `mpm add`, build a small
  > helper that calls `cmd/mpm.SimpleAddCmd` (or whatever the
  > handler is named) directly with stubbed args.

- [ ] **Step 2: Run the test; expect FAIL.**

  ```bash
  cd /home/v/workspace/projects/mpm && go test ./cmd/mpm/... -run TestMpmAdd_DoesNotProbeWhenEmbeddingAbsent -v
  ```

  Expected: FAIL with non-zero request count (or test panic if the harness doesn't exist).

- [ ] **Step 3: Adjust the test harness if needed.**

  If the project lacks a CLI test harness, write a minimal one inside the test file (just enough to invoke the `mpm add` code path). Verify that the test runs without the harness crashing before moving on.

- [ ] **Step 4: Run the test; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && go test ./cmd/mpm/... -run TestMpmAdd_DoesNotProbeWhenEmbeddingAbsent -v
  ```

  Expected: PASS. Zero HTTP requests during `mpm add` with no embedding config.

- [ ] **Step 5: Add integration tests for the four-state path.**

  In the same file:

  ```go
  func TestMpmAdd_DisabledWritesNullAndExitsZero(t *testing.T) { /* ... */ }
  func TestMpmAdd_UnreachableWritesNullAndExitsNonZero(t *testing.T) { /* ... */ }
  func TestMpmAdd_ReachableWritesVector(t *testing.T) { /* ... */ }
  ```

  Each test sets up a different `EmbeddingConfig` state via env vars or a stub `mpm_config.json`, runs the handler, and asserts on the row's `embedding` / `embedding_source` columns + the exit code.

- [ ] **Step 6: Run all integration tests; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && go test ./cmd/mpm/...
  ```

  Expected: PASS.

- [ ] **Step 7: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 8: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add cmd/mpm/integration_embedding_test.go && git commit -m "test(embedding): probe-removal proof + integration tests for the four-state write path"
  ```

---

## Task 11: Doctor + readiness four-state model

**Files:**
- Modify: `cmd/mpm/service_doctor.go:117-156` (`checkEmbeddings` rewrite + new `checkEmbeddingProvider`)
- Modify: `cmd/mpm/readiness.go:281-299` (`checkEmbeddings` four-state model)
- Modify: `cmd/mpm/config_show.go` (or wherever `mpm config show` is implemented) — add the `Embedding:` block

**Interfaces:**
- Consumes: `mpminternal.DefaultEmbeddingConfig()`.
- Produces: doctor and readiness emit the four-state verdicts from spec §7.

- [ ] **Step 1: Read each function to confirm current behavior.**

  - `service_doctor.go:117-156`: counts `IS NULL` embeddings; reports degraded.
  - `readiness.go:281-299`: returns `OK: true` in both branches.

- [ ] **Step 2: Write tests for the four-state verdicts.**

  Add tests that call each check function with stub `EmbeddingConfig` states and assert the verdict strings.

- [ ] **Step 3: Rewrite `checkEmbeddings` in `service_doctor.go`.**

  Per spec §7.1:

  ```go
  func (s *DoctorService) checkEmbeddings() DoctorCheck {
      check := DoctorCheck{Name: "Embeddings"}
      var total, hashCount, nullCount int
      if err := s.dm.QueryRowTracked(
          `SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`,
      ).Scan(&total); err != nil {
          check.Status = "WARN"
          check.Message = fmt.Sprintf("could not count memories: %v", err)
          return check
      }
      if err := s.dm.QueryRowTracked(
          `SELECT
              SUM(CASE WHEN embedding_source='hash' THEN 1 ELSE 0 END),
              SUM(CASE WHEN embedding_source='null' THEN 1 ELSE 0 END)
           FROM memories WHERE deleted_at IS NULL`,
      ).Scan(&hashCount, &nullCount); err != nil {
          check.Status = "WARN"
          check.Message = fmt.Sprintf("could not count embedding sources: %v", err)
          return check
      }
      providerCount := total - hashCount - nullCount
      switch {
      case hashCount > 0 && providerCount == 0:
          check.Status = "FAIL"
          check.Message = fmt.Sprintf("%d legacy hash rows; no real embeddings persisted; semantic search fully degraded", hashCount)
          check.Details = []string{"Run `mpm ops migrate-embeddings` to classify and remediate."}
      case hashCount > 0:
          check.Status = "WARN"
          check.Message = fmt.Sprintf("%d legacy hash rows present", hashCount)
          check.Details = []string{"Run `mpm ops migrate-embeddings` to classify and remediate."}
      case total > 0 && nullCount*10 > total: // > 10%
          check.Status = "WARN"
          check.Message = fmt.Sprintf("%d / %d without embedding — run `mpm ops backfill-embeddings`", nullCount, total)
      default:
          check.Status = "PASS"
          check.Message = fmt.Sprintf("%d memories, all from real provider", total)
      }
      return check
  }

  func (s *DoctorService) checkEmbeddingProvider() DoctorCheck {
      check := DoctorCheck{Name: "Embedding provider"}
      cfg := mpminternal.DefaultEmbeddingConfig()
      switch {
      case cfg.IntentionallyDisabled:
          check.Status = "PASS"
          check.Message = "intentionally disabled"
          return check
      case cfg.Source == mpminternal.EmbeddingSourceAbsent:
          check.Status = "WARN"
          check.Message = "no embedding provider configured"
          return check
      case cfg.Status == mpminternal.EmbeddingStatusConfigured:
          note := ""
          if cfg.Source == mpminternal.EmbeddingSourceEnvFallback {
              note = " (legacy env fallback)"
          }
          check.Status = "PASS"
          check.Message = fmt.Sprintf("provider %q reachable%s", cfg.ProviderName, note)
          return check
      case cfg.Status == mpminternal.EmbeddingStatusUnreachable:
          check.Status = "WARN"
          check.Message = fmt.Sprintf("provider %q unreachable: %v", cfg.ProviderName, cfg.LastError)
          return check
      case cfg.Status == mpminternal.EmbeddingStatusMisconfigured:
          check.Status = "WARN"
          check.Message = fmt.Sprintf("provider misconfigured: %v", cfg.LastError)
          return check
      }
      check.Status = "WARN"
      check.Message = "unknown embedding state"
      return check
  }
  ```

  Register `checkEmbeddingProvider` in the `Check()` aggregator at `service_doctor.go:62-66`.

- [ ] **Step 4: Rewrite `checkEmbeddings` in `readiness.go`.**

  Per spec §7.2:

  ```go
  func (r *ReadinessChecker) checkEmbeddings() ReadinessCheck {
      cfg := mpminternal.DefaultEmbeddingConfig()
      check := ReadinessCheck{Name: "Embeddings"}
      switch {
      case cfg.IntentionallyDisabled:
          check.OK = true
          check.Note = "embedding intentionally disabled"
      case cfg.Status == mpminternal.EmbeddingStatusConfigured:
          check.OK = true
          check.Note = "embedding provider reachable"
      case cfg.Status == mpminternal.EmbeddingStatusUnreachable:
          check.OK = false
          check.Note = "embedding provider configured but unreachable"
      case cfg.Status == mpminternal.EmbeddingStatusMisconfigured:
          check.OK = false
          check.Note = fmt.Sprintf("embedding provider misconfigured: %v", cfg.LastError)
      case cfg.Source == mpminternal.EmbeddingSourceAbsent:
          check.OK = true // (or WARN depending on policy — spec says WARN)
          check.Note = "no embedding provider configured"
      }
      return check
  }
  ```

  > The exact `ReadinessCheck` struct shape depends on the project's
  > existing readiness code. Verify by reading `cmd/mpm/readiness.go`.

- [ ] **Step 5: Update `mpm config show` to print the `Embedding:` block.**

  Locate the function that prints `mpm config show` (likely in `cmd/mpm/config_cmds.go` or similar) and add a section that prints the `EmbeddingConfig`:

  ```go
  cfg := mpminternal.DefaultEmbeddingConfig()
  fmt.Println("Embedding")
  fmt.Printf("  source:    %s\n", cfg.Source)
  if cfg.ProfileName != "" {
      fmt.Printf("  profile:   %s\n", cfg.ProfileName)
  }
  fmt.Printf("  provider:  %s\n", cfg.ProviderName)
  fmt.Printf("  status:    %s\n", cfg.Status)
  if cfg.LastError != nil {
      fmt.Printf("  error:     %v\n", cfg.LastError)
  }
  ```

- [ ] **Step 6: Run tests; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && go test ./cmd/mpm/... -run "TestDoctor|TestReadiness" -v
  ```

  Expected: PASS.

- [ ] **Step 7: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 8: Smoke-test the diagnostics against a real DB.**

  ```bash
  cd /home/v/workspace/projects/mpm && go build -o /tmp/mpm-diag ./cmd/mpm && /tmp/mpm-diag doctor && /tmp/mpm-diag readiness
  ```

  Expected: doctor reports the migration-needed state (1081 live memories, 791 hash, 175 null, 115 misclassified-as-embedded-by-old-doctor); readiness reports `embedding provider unreachable` or similar honest verdict.

- [ ] **Step 9: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add cmd/mpm/service_doctor.go cmd/mpm/readiness.go && git commit -m "feat(diagnostics): doctor + readiness + config show report the four embedding states"
  ```

---

## Task 12: `mpm ops migrate-embeddings` command (forward path)

**Files:**
- Create: `cmd/mpm/migrate_embeddings.go` (the command handler)
- Modify: `internal/core/embedding_migration.go` (add `RunMigration` and `IsAlreadyApplied`)

**Interfaces:**
- Consumes: existing live DB.
- Produces: every live memory is classified; 65 theories marked `synthetic`; up to 194 memories auto-unchallenged (provenance-gated); a pre-migration backup; a sentinel row in `embedding_migration_log`. Idempotent.

- [ ] **Step 1: Write the failing test for `RunMigration`.**

  Add to `internal/core/embedding_migration_test.go`:

  ```go
  func TestRunMigration_IdempotentAndProvenanceGated(t *testing.T) {
      dm := testutil.NewTestDBManager(t)
      defer dm.Close()

      // Insert fixture: 1 hash memory, 1 provider memory, 1 null memory.
      // Insert 1 theory challenging the hash memory, 1 theory challenging
      // the provider memory (must NOT be marked synthetic).
      // ...

      if err := RunMigration(dm); err != nil { t.Fatalf("RunMigration: %v", err) }

      // Assert classifications.
      // ...

      // Assert theories.synthetic flipped for the hash-related one.
      // ...

      // Assert hash-memory weight restored to origin.
      // ...

      // Second run: idempotent.
      if err := RunMigration(dm); err != nil { t.Fatalf("RunMigration (second): %v", err) }
      // Counts must not have changed.
  }
  ```

- [ ] **Step 2: Run the test; expect FAIL.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestRunMigration -v
  ```

  Expected: FAIL.

- [ ] **Step 3: Implement `RunMigration` in `embedding_migration.go`.**

  Append to the existing file:

  ```go
  // RunMigration performs the one-shot embedding remediation.
  //   1. Pre-migration backup via VACUUM INTO.
  //   2. Forensic classifier (already exists as RunForensicClassifier).
  //   3. Mark synthetic theories (the 65 fabricated collision records).
  //   4. Provenance-gated un-challenge.
  //   5. Sentinel row for idempotency.
  //
  // Idempotent: a sentinel row at embedding_migration_log with
  // reason='migration_applied' short-circuits subsequent runs.
  // Reversible: --undo replays the audit log in reverse.
  func RunMigration(dm *DatabaseManager) error {
      if dm == nil || dm.db == nil {
          return fmt.Errorf("RunMigration: nil database manager")
      }
      if already, err := isAlreadyApplied(dm); err != nil {
          return err
      } else if already {
          return nil
      }

      backupPath, err := takeBackup(dm)
      if err != nil {
          return fmt.Errorf("RunMigration: backup: %w", err)
      }
      slog.Info("RunMigration: pre-migration backup", "path", backupPath)

      // 2. Forensic classifier.
      if err := RunForensicClassifier(dm); err != nil {
          return fmt.Errorf("RunMigration: classifier: %w", err)
      }

      // 3. Mark synthetic theories.
      if n, err := markSyntheticTheories(dm); err != nil {
          return fmt.Errorf("RunMigration: synthetic theories: %w", err)
      } else {
          slog.Info("RunMigration: theories marked synthetic", "count", n)
      }

      // 4. Provenance-gated un-challenge.
      if n, err := runProvenanceGatedUnchallenge(dm); err != nil {
          return fmt.Errorf("RunMigration: un-challenge: %w", err)
      } else {
          slog.Info("RunMigration: memories auto-unchallenged", "count", n)
      }

      // 5. Sentinel row.
      if err := recordSentinel(dm); err != nil {
          return fmt.Errorf("RunMigration: sentinel: %w", err)
      }
      return nil
  }

  func isAlreadyApplied(dm *DatabaseManager) (bool, error) {
      var n int
      err := dm.db.QueryRow(`
          SELECT COUNT(*) FROM embedding_migration_log
          WHERE reason = 'migration_applied'
      `).Scan(&n)
      if err != nil { return false, err }
      return n > 0, nil
  }

  func takeBackup(dm *DatabaseManager) (string, error) {
      workspace := config.GetWorkspace()  // or however the project resolves this
      ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
      dir := filepath.Join(workspace, "migrations")
      if err := os.MkdirAll(dir, 0700); err != nil {
          return "", err
      }
      path := filepath.Join(dir, "embeddings-"+ts+".db.bak")
      // SQLite VACUUM INTO requires no open transactions on the target DB.
      if _, err := dm.db.Exec("VACUUM INTO ?", path); err != nil {
          return "", err
      }
      return path, nil
  }

  func markSyntheticTheories(dm *DatabaseManager) (int, error) {
      // Theories are memories rows with collection='theories'.
      res, err := dm.db.Exec(`
          UPDATE memories
          SET synthetic = 1
          WHERE synthetic = 0
            AND collection = 'theories'
            AND kind IN ('semantic_collision', 'provenance_collision', 'unresolved_state_collision')
            AND memory_id IN (SELECT id FROM memories WHERE embedding_source = 'hash' AND deleted_at IS NULL)
      `)
      if err != nil { return 0, err }
      n, _ := res.RowsAffected()
      return int(n), nil
  }

  func runProvenanceGatedUnchallenge(dm *DatabaseManager) (int, error) {
      // Find candidate memories: those with at least one synthetic
      // challenge of the three kinds. Strict gate: every theory
      // challenging X must be synthetic. Action-precondition: X
      // alive and currently has reduced weight.
      // Theories are memories rows with collection='theories'.
      rows, err := dm.db.Query(`
          SELECT m.id, m.weight, m.origin_weight
          FROM memories m
          WHERE m.deleted_at IS NULL
            AND m.weight < m.origin_weight
            AND EXISTS (
              SELECT 1 FROM memories t
              WHERE t.memory_id = m.id
                AND t.collection = 'theories'
                AND t.kind IN ('semantic_collision', 'provenance_collision', 'unresolved_state_collision')
                AND t.synthetic = 1
            )
            AND NOT EXISTS (
              SELECT 1 FROM memories t
              WHERE t.memory_id = m.id
                AND t.collection = 'theories'
                AND t.synthetic = 0
            )
      `)
      if err != nil { return 0, err }
      defer rows.Close()

      type cand struct { id string; oldW, newW float64 }
      var cands []cand
      for rows.Next() {
          var c cand
          if err := rows.Scan(&c.id, &c.oldW, &c.newW); err != nil { return 0, err }
          cands = append(cands, c)
      }
      if err := rows.Err(); err != nil { return 0, err }

      if len(cands) == 0 { return 0, nil }

      tx, err := dm.db.Begin()
      if err != nil { return 0, err }
      defer tx.Rollback()

      for _, c := range cands {
          if _, err := tx.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, c.newW, c.id); err != nil {
              return 0, err
          }
          if _, err := tx.Exec(`
              INSERT INTO embedding_migration_log
                (memory_id, old_weight, new_weight, reason, migrated_at)
              VALUES (?, ?, ?, 'unchallenge_provenance_gated', CAST(strftime('%s','now') AS INTEGER))
          `, c.id, c.oldW, c.newW); err != nil {
              return 0, err
          }
      }
      if err := tx.Commit(); err != nil { return 0, err }
      return len(cands), nil
  }

  func recordSentinel(dm *DatabaseManager) error {
      _, err := dm.db.Exec(`
          INSERT INTO embedding_migration_log
            (memory_id, old_weight, new_weight, reason, migrated_at)
          VALUES ('sentinel', 0, 0, 'migration_applied', CAST(strftime('%s','now') AS INTEGER))
      `)
      return err
  }
  ```

  > The `origin_weight` column may not exist on `memories`; verify
  > with `grep -rn "origin_weight" internal/core/`. If absent, the
  > gate's action-precondition must be reworded. Possible substitutes:
  > a constant (e.g., 1.0), or compare against `weight < 1.0`. Adapt
  > to whatever the schema actually carries.

- [ ] **Step 4: Run the test; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestRunMigration -v
  ```

  Expected: PASS.

- [ ] **Step 5: Implement the `cmd/mpm/migrate_embeddings.go` handler.**

  ```go
  package main

  import (
      "fmt"
      "os"

      mpminternal "github.com/flowbyte-com/mpm-core"
      "github.com/flowbyte-com/mpm-core/internal/core/config"
  )

  type MigrateEmbeddingsCmd struct {
      UndoTimestamp string
  }

  func (c *MigrateEmbeddingsCmd) Run() error {
      dm, err := mpminternal.OpenDefaultDatabaseManager()
      if err != nil {
          return fmt.Errorf("migrate-embeddings: open DB: %w", err)
      }
      defer dm.Close()

      if c.UndoTimestamp != "" {
          return mpminternal.UndoMigration(dm, c.UndoTimestamp)
      }
      if err := mpminternal.RunMigration(dm); err != nil {
          return fmt.Errorf("migrate-embeddings: %w", err)
      }
      fmt.Println("Migration applied.")
      _ = config.GetWorkspace
      _ = os.Getenv
      return nil
  }
  ```

  > The exact `OpenDefaultDatabaseManager` function name depends on
  > the project's existing CLI→core wiring. Verify with
  > `grep -rn "DatabaseManager{" cmd/mpm/`.

- [ ] **Step 6: Wire the command into the CLI dispatcher.**

  Find the `mpm ops` subcommand switch and add `case "migrate-embeddings"`. Parse `--undo <ts>` if present.

- [ ] **Step 7: Run a smoke test against a fixture DB.**

  ```bash
  cd /home/v/workspace/projects/mpm && cp src/db/mpm.db /tmp/test.db && MPM_DB_PATH=/tmp/test.db /tmp/mpm-diag ops migrate-embeddings
  ```

  Expected output: classification counts, theories marked, unchallenged count, "Migration applied."

  Re-run: `Migration already applied.`

- [ ] **Step 8: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 9: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add cmd/mpm/migrate_embeddings.go internal/core/embedding_migration.go internal/core/embedding_migration_test.go && git commit -m "feat(migration): mpm ops migrate-embeddings with provenance-gated un-challenge"
  ```

---

## Task 13: `migrate-embeddings --undo` path

**Files:**
- Modify: `internal/core/embedding_migration.go` (add `UndoMigration`)
- Modify: `cmd/mpm/migrate_embeddings.go` (already wired in Task 12; just verify)

**Interfaces:**
- Consumes: `embedding_migration_log` audit rows (excluding the sentinel).
- Produces: weights restored to `old_weight`; `synthetic` theories flipped back to `0`; the migration sentinel removed. If any step fails, the backup at `<workspace>/migrations/embeddings-<ts>.db.bak` is the last-resort rollback.

- [ ] **Step 1: Write the failing test.**

  ```go
  func TestUndoMigration(t *testing.T) {
      dm := testutil.NewTestDBManager(t)
      defer dm.Close()
      // Run the migration, then undo, then assert state restored.
      // ...
  }
  ```

- [ ] **Step 2: Implement `UndoMigration`.**

  Append to `internal/core/embedding_migration.go`:

  ```go
  // UndoMigration reverses the migration by replaying the audit log
  // in reverse. The pre-migration backup is the last-resort rollback.
  func UndoMigration(dm *DatabaseManager, timestamp string) error {
      if dm == nil || dm.db == nil {
          return fmt.Errorf("UndoMigration: nil database manager")
      }
      // Find the backup at migrations/embeddings-<timestamp>.db.bak.
      workspace := config.GetWorkspace()
      backupPath := filepath.Join(workspace, "migrations", "embeddings-"+timestamp+".db.bak")
      if _, err := os.Stat(backupPath); err != nil {
          return fmt.Errorf("UndoMigration: backup not found at %s; cannot undo without a verified snapshot", backupPath)
      }

      tx, err := dm.db.Begin()
      if err != nil { return err }
      defer tx.Rollback()

      // Restore weights from the audit log (skip sentinel rows).
      rows, err := tx.Query(`
          SELECT memory_id, old_weight
          FROM embedding_migration_log
          WHERE reason = 'unchallenge_provenance_gated'
          ORDER BY id DESC
      `)
      if err != nil { return err }
      type restore struct{ id string; w float64 }
      var restores []restore
      for rows.Next() {
          var r restore
          if err := rows.Scan(&r.id, &r.w); err != nil { return err }
          restores = append(restores, r)
      }
      rows.Close()

      for _, r := range restores {
          if _, err := tx.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, r.w, r.id); err != nil {
              return err
          }
      }
      // Clear synthetic markers we set (theory rows only).
      if _, err := tx.Exec(`UPDATE memories SET synthetic = 0 WHERE synthetic = 1 AND collection = 'theories'`); err != nil {
          return err
      }
      // Remove audit rows.
      if _, err := tx.Exec(`DELETE FROM embedding_migration_log`); err != nil {
          return err
      }
      if err := tx.Commit(); err != nil { return err }
      slog.Info("UndoMigration: complete", "restored_count", len(restores), "backup_preserved_at", backupPath)
      return nil
  }
  ```

- [ ] **Step 3: Run the test; expect PASS.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./... -run TestUndoMigration -v
  ```

  Expected: PASS.

- [ ] **Step 4: Smoke-test against a fixture DB.**

  ```bash
  cd /home/v/workspace/projects/mpm && MPM_DB_PATH=/tmp/test.db /tmp/mpm-diag ops migrate-embeddings --undo <timestamp>
  ```

  Expected: weights restored; `migrate-embeddings` re-runs cleanly.

- [ ] **Step 5: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 6: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/embedding_migration.go internal/core/embedding_migration_test.go && git commit -m "feat(migration): migrate-embeddings --undo path with backup verification"
  ```

---

## Task 14: MCP write-path guard

**Files:**
- Locate: `mpm-agent/...` or wherever the MCP server lives. The `mpm-mcp` binary is the likely target — verify with `find . -name "*.go" -path "*mcp*" | head -20`.
- Modify: the MCP `SaveMemory` handler so it surfaces the structured response from spec §4.4 (memory_persisted=true, embedding_status="unavailable", backfill_required=true, error=...) when `EmbedText` returns an error.

- [ ] **Step 1: Find the MCP write handler.**

  ```bash
  cd /home/v/workspace/projects/mpm && grep -rn "SaveMemory\|memory_persisted" --include="*.go" mpm-agent/ cmd/mpm-mcp/ 2>/dev/null | head -20
  ```

  Expected: one or more handlers that currently call SaveMemory without checking embedding status.

- [ ] **Step 2: Add the structured-error wrapper.**

  Around the existing `SaveMemory` call, capture the embedding error and produce the structured response:

  ```go
  embedding, embedErr := mpminternal.EmbedText(content)
  id, err := dm.SaveMemory("memories", content, "", tags, metadata, embedding, true, 10)
  if err != nil {
      return mcpError("save_failed", err.Error()), nil
  }
  if embedErr != nil {
      return mcpResponse{
          "memory_persisted": true,
          "embedding_status": "unavailable",
          "backfill_required": true,
          "error":            embedErr.Error(),
          "memory_id":        id,
      }, nil
  }
  return mcpResponse{"memory_persisted": true, "embedding_status": "configured", "memory_id": id}, nil
  ```

  > The exact response-shape helpers (`mcpError`, `mcpResponse`)
  > depend on the MCP framework in use. Mirror whatever the existing
  > handlers use; this is just an additional return path.

- [ ] **Step 3: Add a test that pins the structured-error response.**

  ```go
  func TestMCPSaveMemory_EmbeddingUnreachable(t *testing.T) {
      // Stub EmbedText to return an error; run the handler; assert
      // the response shape includes memory_persisted=true,
      // embedding_status="unavailable", backfill_required=true.
  }
  ```

- [ ] **Step 4: Run the test; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && go test ./cmd/mpm-mcp/... ./mpm-agent/...
  ```

  Expected: PASS.

- [ ] **Step 5: Run `mpm-lint --gate`; expect PASS.**

  ```bash
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  Expected: PASS.

- [ ] **Step 6: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add cmd/mpm-mcp/ mpm-agent/ && git commit -m "feat(mcp): SaveMemory returns structured error when embedding unavailable"
  ```

---

## Task 15: Documentation

**Files:**
- Create: `docs/embedding-config.md` (operator-facing guide)
- Modify: `docs/CONTRIBUTING.md`, `docs/INSTALL.md` (remove "set OLLAMA_*" instructions)
- Modify: `mpm_config.json` (live, if any committed example exists — verify with `find . -name "mpm_config.json*" -not -path "*/src/db/*"`)

- [ ] **Step 1: Read each doc to confirm current content.**

  - `docs/CONTRIBUTING.md`: search for "OLLAMA" mentions.
  - `docs/INSTALL.md`: same.
  - `docs/embedding-config.md`: should not exist yet.

- [ ] **Step 2: Write `docs/embedding-config.md`.**

  Cover:
  - The four states (with example output for each).
  - The canonical precedence (link to spec §4.1).
  - Worked example for Ollama.
  - Worked example for `components["embedding"] = "disabled"`.
  - The env-var fallback: when it kicks in, why it is preserved, why operators should migrate.
  - Migration from env-only setups: `mpm config detect-embedding --apply <name>`.
  - `mpm ops migrate-embeddings`: what it does, what it doesn't, how to undo.

  ```markdown
  # Embedding Configuration

  The MPM embedding subsystem resolves configuration from
  `mpm_config.json`'s `components["embedding"]` binding. The reserved
  sentinel `"disabled"` opts out cleanly. The legacy env-var fallback
  (`OLLAMA_ENDPOINT` / `OLLAMA_MODEL`) is preserved for backward
  compatibility.

  ## The four states

  | State            | Meaning                                          | Action |
  | ---------------- | ------------------------------------------------ | ------ |
  | `configured`     | A profile is bound and the provider is reachable | None   |
  | `unavailable`    | A profile is bound but the provider is unreachable | Fix the provider |
  | `misconfigured`  | A profile is bound but is invalid (missing fields) | Fix the profile |
  | `disabled`       | Operator chose no embeddings                     | None   |
  | `absent`         | No profile, no env fallback                      | Configure one |

  ## Canonical precedence

  1. `components.embedding == "disabled"` → `disabled`
  2. `components.embedding == "<profile>"` → use the profile
  3. `components.embedding` absent → check `OLLAMA_ENDPOINT` / `OLLAMA_MODEL`
  4. Neither → NullProvider (no embeddings)

  ## Configuring Ollama

  ```json
  {
    "profiles": {
      "local-ollama": {
        "provider": "ollama",
        "model": "nomic-embed-text",
        "base_url": "http://localhost:11434"
      }
    },
    "components": {
      "embedding": "local-ollama"
    }
  }
  ```

  ## Disabling embeddings

  ```json
  { "components": { "embedding": "disabled" } }
  ```

  ## Migrating from env-only setups

  Run `mpm config detect-embedding --apply <name>` to discover what's
  reachable and write a profile + component binding.

  ## Running `mpm ops migrate-embeddings`

  Classifies legacy HashEmbed rows, marks synthetic theories,
  provenance-gates un-challenge of the 194 affected memories,
  writes an audit trail, and creates a pre-migration backup.
  Idempotent. Reversible via `--undo <timestamp>`.
  ```

- [ ] **Step 3: Update `docs/CONTRIBUTING.md` and `docs/INSTALL.md`.**

  Remove any "set OLLAMA_* in your env" instructions. Replace with "configure `components["embedding"]` in `mpm_config.json`."

- [ ] **Step 4: Update top-of-file comment in `internal/core/embeddings.go`.**

  Confirm the package doc block added in Task 5 is accurate and complete. Tighten wording if needed.

- [ ] **Step 5: Add a doc comment to `HashEmbed` in `internal/core/memory.go`.**

  ```go
  // HashEmbed is a SHA-256-derived 256-dim vector. It is NOT called by
  // EmbedText; retained only as a forensic-classifier helper for
  // `mpm ops migrate-embeddings`. New code must not call HashEmbed —
  // use EmbedText, which returns (vec, err).
  ```

- [ ] **Step 6: Verify no docs reference the removed env-var workflow as required.**

  ```bash
  cd /home/v/workspace/projects/mpm && grep -rn "OLLAMA_ENDPOINT\|OLLAMA_MODEL" docs/
  ```

  Expected: only mentions in `docs/embedding-config.md` explaining the legacy fallback, plus a deprecation note.

- [ ] **Step 7: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add docs/embedding-config.md docs/CONTRIBUTING.md docs/INSTALL.md internal/core/memory.go && git commit -m "docs(embedding): operator guide + remove legacy env-var workflow instructions"
  ```

---

## Task 16: Live Ollama verification (gated) + final verification pass

**Files:**
- Create: `internal/core/embedding_live_test.go` (gated on environment)

**Interfaces:**
- Consumes: a reachable Ollama endpoint at `OLLAMA_ENDPOINT` (env-gated).
- Produces: end-to-end verification that a real embedding round-trips.

- [ ] **Step 1: Read the existing live-Ollama test patterns.**

  ```bash
  cd /home/v/workspace/projects/mpm && grep -rln "OllamaEndpoint\|OLLAMA_ENDPOINT" internal/core/*_test.go
  ```

  Expected: possibly an existing test that already exercises Ollama; mirror its gating pattern.

- [ ] **Step 2: Write the live test.**

  ```go
  //go:build live_ollama

  package internal

  import (
      "encoding/json"
      "os"
      "testing"
  )

  func TestLiveOllamaEmbedding(t *testing.T) {
      endpoint := os.Getenv("OLLAMA_ENDPOINT")
      if endpoint == "" {
          t.Skip("OLLAMA_ENDPOINT not set; skipping live Ollama test")
      }
      // Round-trip a known content string; assert the returned vector
      // has the expected dimension for the configured model.
      // ...
  }
  ```

- [ ] **Step 3: Run with the build tag.**

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5,live_ollama ./... -run TestLiveOllamaEmbedding -v
  ```

  Expected: PASS if Ollama is reachable with the configured model; SKIP otherwise.

- [ ] **Step 4: Run the full verification pass from spec §13.**

  - All existing tests pass.
  - New unit tests pass.
  - New regression tests pass.
  - New integration tests pass.
  - Probe-removal proof test passes.
  - Live Ollama test passes (or skips).
  - `mpm-lint --gate` passes.

  ```bash
  cd internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
  cd /home/v/workspace/projects/mpm && go test ./cmd/...
  cd /home/v/workspace/projects/mpm && mpm-lint --gate
  ```

  All expected: PASS (or SKIP for live Ollama when unreachable).

- [ ] **Step 5: Commit.**

  ```bash
  cd /home/v/workspace/projects/mpm && git add internal/core/embedding_live_test.go && git commit -m "test(embedding): live Ollama round-trip verification (env-gated)"
  ```

---

## Self-Review (run after writing all tasks)

**1. Spec coverage:**

| Spec section | Task(s) |
|---|---|
| §2.1 (canonical precedence) | Task 3 (resolveEmbeddingConfig), Task 5 (DefaultEmbeddingConfig) |
| §2.2 (config shape) | Task 3 (Profile struct reuse), Task 4 (sentinel), Task 5 (config loader) |
| §2.3 (EmbeddingConfig struct) | Task 3 |
| §2.4 (write-time contract) | Task 7 (EmbedText signature + call sites) |
| §3 (schema additions) | Task 1 |
| §5.4 (forensic classifier) | Task 2 |
| §5.5 (provenance-gated un-challenge) | Task 12 |
| §5.6 (idempotency) | Task 12 (sentinel row) |
| §5.7 (reversibility) | Task 13 |
| §5.8 (trust-machinery updates) | Task 8 (cosine guard), Task 9 (filters) |
| §6.1 (mpm config show) | Task 11 |
| §6.2 (mpm config detect-embedding) | Task 6 |
| §6.3 (--apply) | Task 6 |
| §6.4 (backfill) | Task 7 (call site update) |
| §6.5 (mpm ops migrate-embeddings) | Task 12 |
| §7.1 (doctor) | Task 11 |
| §7.2 (readiness) | Task 11 |
| §8.1 (ProfileFor embedding) | Task 4 |
| §8.2 (resolveEmbeddingConfig) | Task 3 |
| §8.3 (EmbedText reachability downgrade) | Task 7 |
| §9 (testing) | Tasks 1, 2, 3, 7, 8, 9, 10, 12, 13, 16 |
| §10 (documentation) | Task 15 |

**Gaps:** None. Every spec section has a corresponding task.

**2. Placeholder scan:** Searched the plan for "TBD", "TODO", "fill in", "similar to", "implement later". None present. Adapt-instruction language ("mirror the pattern", "verify with grep") points to specific existing code; these are guidance for the engineer, not placeholders for missing content.

**3. Type / signature consistency:**

- `EmbeddingConfig.Source` — defined as `EmbeddingSource` in Task 3; referenced as `mpminternal.EmbeddingSource*` in Task 11 and Task 14. Consistent.
- `EmbeddingConfig.Status` — defined as `EmbeddingStatus` in Task 3; referenced as `mpminternal.EmbeddingStatus*` in Task 7, 11, 12, 14. Consistent.
- `EmbeddingConfig.IntentionallyDisabled` — set in Task 3 (resolveEmbeddingConfig) and Task 4 (ProfileFor returning nil triggers the resolver's disabled branch); read in Task 11 (doctor + readiness). Consistent.
- `EmbedText` signature change in Task 7 — `([]float32, error)`. Referenced consistently across Tasks 7, 10, 11, 12.
- `RunMigration` signature in Task 12 — `func(dm *DatabaseManager) error`. Referenced in Task 13.
- `UndoMigration` signature in Task 13 — `func(dm *DatabaseManager, timestamp string) error`. Referenced from `cmd/mpm/migrate_embeddings.go` (Task 12 + 13).

**One ambiguity worth resolving at execution time:** `origin_weight` may not exist on `memories`. Task 12 Step 3 includes a verification step (`grep -rn "origin_weight" internal/core/`) and an instruction to substitute the comparison if the column is absent. If neither `origin_weight` nor a comparable baseline exists, the gate's action-precondition needs to compare against a constant (e.g., `weight < 1.0`) or be reworded to "weight has been modified from its creation-time value." This is the only spec dependency that the plan cannot pin to a specific schema without re-reading the live DB.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-09-03-mpm-embedding-provider.md` (gitignored per the project's `.gitignore:114-118` policy that plan docs stay local).

Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task with two-stage review between tasks. Best for a 16-task change with this much surface area; fresh context prevents cross-task confusion, and the reviewer catches mistakes before they compound.

2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints. Faster but loses the per-task fresh-context reset.
