# Audit Gap Closure — 2026-07-23

## Goal

Close all six gaps identified in `/home/v/workspace/projects/mpm/audit.md` (dated 2026-07-23), delivered as three PRs grouped by risk profile.

## Background

The 2026-07-23 comprehensive audit scored MPM 90/100, flagging six residual findings from prior reviews. Two are trivial fixes; two are doc/test improvements; one is a timestamp-format unification spanning 13 write sites and 1 comparison site; one is tooling. The prior security audit (2026-07-07) and hostile audit (2026-06-03) both shipped fixes referenced in this design.

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Scope | All 6 gaps in priority order | User directive |
| Timestamp format | Unify on `INTEGER` Unix epoch (matching `expires_at`) | Existing precedent; integer comparisons avoid SQLite lexicographic gotchas |
| Existing-row backfill | Yes, in the migration | Don't strand production rows in the old format |
| PR grouping | 3 PRs by risk profile | Clean review surface, easy rollback |
| NewSession consolidation | Document as intentional | Low risk; refactor risk outweighs WAL contention benefit |
| golangci-lint | Minimal config (advisory) | Discover noise floor without breaking build |

## PR 1 — Trivial Fixes & Doc Comments (low risk)

### Changes

**1.1** `internal/scheduler/scheduler_test.go` — extend `testSchema` (lines 33–47) to include the `scheduled_tasks` table. Audit finding 2. The exact schema must mirror the one installed by `DatabaseManager.initUnifiedSchema()` in the main module so the scheduler's poll loop doesn't emit `"no such table: scheduled_tasks"` errors during tests.

**1.2** `cmd/mpm/handlers_backup.go` — replace `defer db.Close()` (lines 149, 213) with a new helper. Audit finding 3.

Add to `cmd/mpm/handlers.go`:

```go
// closeSQLDB closes a raw *sql.DB and logs any error. Mirrors closeDB for
// DatabaseManager (LOW-03 fix). Used by handlers that open transient
// connections outside the singleton (backup/restore-db/wal-flush).
func closeSQLDB(db *sql.DB) {
    if db == nil {
        return
    }
    if err := db.Close(); err != nil {
        slog.Warn("sql.DB close error", "error", err.Error())
    }
}
```

Then in `handlers_backup.go`: replace both `defer db.Close()` calls with `defer closeSQLDB(db)`.

**1.3** `internal/core/artifact_table.go` — expand the existing doc comment to call out the invariant explicitly:

```go
// ArtifactTable maps an artifact type to its underlying SQLite table name.
//
// INVARIANT: this function only ever returns one of two hardcoded strings:
// "memories" or "lessons". It is NOT user-controllable. Callers pass the
// return value into fmt.Sprintf("UPDATE %s SET ...", table) at
// evidence_store.go (190, 260, 318, 409, 633, 827), so any change to add
// a new artifact type MUST be reflected here AND validated against the
// canonical schema allow-list in canonical_dump.go.
func ArtifactTable(artifactType string) string {
    ...
}
```

**1.4** `internal/core/db.go` — `NewSession()` (lines 750–784): add a comment to the function header making the intent explicit:

```go
// NewSession opens an independent connection to the same database file.
//
// Background workers (synthesis, lifecycle, critic) need an isolated
// connection so their long-running queries don't block the primary
// session's hot path. WAL mode + busy_timeout=5000 keep contention
// bounded — see audit.md (2026-07-23) finding 4 for the rationale.
//
// This pattern is allowed by sqlopen_owner_test.go's whitelist.
```

### Testing

No new tests required. The test schema change in 1.1 *is* the test improvement (eliminates noise). The `closeSQLDB` helper is trivial; existing handler tests cover the close path.

### Verification

`make test` must pass with race detector enabled. Static-analysis tests `TestScannerCoverage_AllMemoriesWritersScanContent` and `TestSqlOpenOwnership` must remain green.

## PR 2 — Timestamp Unification (medium risk — schema migration)

### Changes

**2.1 Schema** — `internal/core/schema.go`:
- Change `BaseTables` so the `deleted_at` column is declared `INTEGER` (cosmetic for fresh DBs — SQLite is dynamically typed).
- Add a new one-shot migration table `schema_migrations(id TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)` to `BaseTables`.

**2.2 Backfill migration** — `internal/core/migration_deleted_at.go` (new file):

```go
package internal

import (
    "database/sql"
    "fmt"
)

// MigrateDeletedAtToUnixEpoch converts any existing TEXT deleted_at values
// to INTEGER Unix epoch, matching the expires_at convention. Idempotent
// via the schema_migrations sentinel.
//
// Must run BEFORE any handler executes. Caller (DatabaseManager.init)
// wraps in a transaction:
//
//   tx.Begin()
//   MigrateDeletedAtToUnixEpoch(tx)
//   tx.Commit()
func MigrateDeletedAtToUnixEpoch(tx *sql.Tx) error {
    // 1. Check sentinel
    var applied int
    err := tx.QueryRow(
        `SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
        "deleted_at_unified_v1",
    ).Scan(&applied)
    if err != nil {
        return fmt.Errorf("check sentinel: %w", err)
    }
    if applied > 0 {
        return nil // already applied
    }

    // 2. Convert TEXT rows to INTEGER Unix epoch
    if _, err := tx.Exec(`
        UPDATE memories
        SET deleted_at = CAST(strftime('%s', deleted_at) AS INTEGER)
        WHERE deleted_at IS NOT NULL
          AND typeof(deleted_at) = 'text'
    `); err != nil {
        return fmt.Errorf("backfill deleted_at: %w", err)
    }

    // 3. Record sentinel
    if _, err := tx.Exec(
        `INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
        "deleted_at_unified_v1",
        time.Now().Unix(),
    ); err != nil {
        return fmt.Errorf("record sentinel: %w", err)
    }
    return nil
}
```

**2.3 Update write sites** — change all 13 sites from `deleted_at = CURRENT_TIMESTAMP` (or its `STRFTIME` variants) to `deleted_at = strftime('%s','now')`:

| File | Line | Old | New |
|---|---|---|---|
| `cmd/mpm/handlers_gc.go` | 312 | `deleted_at = CURRENT_TIMESTAMP` | `deleted_at = strftime('%s','now')` |
| `cmd/mpm/handlers_gc.go` | 317 | `deleted_at = CURRENT_TIMESTAMP` | `deleted_at = strftime('%s','now')` |
| `cmd/mpm/simple_cmds.go` | 279 | `deleted_at = CURRENT_TIMESTAMP` | `deleted_at = strftime('%s','now')` |
| `internal/core/memory.go` | 1242 | `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')` | `strftime('%s','now')` |
| `internal/core/memory.go` | 1264 | `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')` | `strftime('%s','now')` |
| `internal/core/memory.go` | 1386 | `CURRENT_TIMESTAMP` | `strftime('%s','now')` |
| `internal/core/memory.go` | 1981 | `CURRENT_TIMESTAMP` | `strftime('%s','now')` |
| `internal/core/memory.go` | 2184 | `CURRENT_TIMESTAMP` | `strftime('%s','now')` |
| `internal/core/memory.go` | 2288 | `CURRENT_TIMESTAMP` | `strftime('%s','now')` |
| `internal/core/memory.go` | 2722 | `CURRENT_TIMESTAMP` | `strftime('%s','now')` |
| `internal/core/db.go` | 3119 | `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')` | `strftime('%s','now')` |
| `internal/core/synthesis_auto.go` | 312 | `CURRENT_TIMESTAMP` | `strftime('%s','now')` |
| `internal/core/synthesis_auto.go` | 314 | `CURRENT_TIMESTAMP` | `strftime('%s','now')` |

**2.4 Update comparison site** — `cmd/mpm/handlers_gc.go:162`:

```sql
-- Old (text vs text, lexicographic):
AND deleted_at < datetime('now', '-30 days')

-- New (integer vs integer, numeric):
AND deleted_at < strftime('%s','now', '-30 days')
```

**2.5 Update test schemas** — `internal/critic/critic_test.go:33` and `internal/core/contradiction_log_test.go:58`: change `deleted_at TEXT` → `deleted_at INTEGER` for parity with the live schema.

**2.6 Update doc references** — `internal/critic/hunts.go:46` and any other docstring that calls out the format. Add a note to `internal/core/web_db.go` next to the existing `MemoryExpireClause` comment block, documenting that `deleted_at` follows the same convention.

### Tests (PR 2)

`internal/core/migration_deleted_at_test.go` (new file):

```go
func TestMigrateDeletedAtToUnixEpoch(t *testing.T) {
    db := setupTestDB(t)
    seedRow(t, db, "text-row", "2026-07-23 14:32:40") // text format
    seedRow(t, db, "int-row",  1721743500)             // already correct
    seedRow(t, db, "null-row", nil)                    // never deleted

    if err := MigrateDeletedAtToUnixEpoch(txFor(db)); err != nil {
        t.Fatalf("migrate: %v", err)
    }

    // NULL row: untouched
    assertNull(t, db, "null-row")

    // Text row: converted. Expected value computed dynamically from the
    // same SQL the migration runs, so the test isn't tied to a hardcoded
    // epoch constant that could drift across timezones or platforms.
    expected := queryInt64(t, db, `SELECT CAST(strftime('%s', '2026-07-23 14:32:40') AS INTEGER)`)
    assertEquals(t, db, "text-row", expected)

    // Integer row: untouched (typeof guard worked)
    assertEquals(t, db, "int-row", int64(1721743500))
    assertType(t, db, "int-row", "integer")

    // Idempotent: second call is a no-op
    if err := MigrateDeletedAtToUnixEpoch(txFor(db)); err != nil {
        t.Fatalf("re-migrate: %v", err)
    }
    assertSentinelApplied(t, db, "deleted_at_unified_v1")

    // Comparison site works post-migration
    rows := query(t, db, `SELECT id FROM memories WHERE deleted_at < strftime('%s','now', '-30 days')`)
    assertContains(t, rows, "text-row")  // was old enough
    assertContains(t, rows, "int-row")   // was old enough
    assertNotContains(t, rows, "null-row")
}
```

### Risks & Rollback (PR 2)

**Forward risks** (covered in audit review):
- Wrong backfill math for fractional-second text rows — verified `strftime('%s', text)` handles `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')` format correctly.
- Comparison site mismatch during mid-deploy — mitigated because `MigrateDeletedAtToUnixEpoch` runs synchronously inside `initUnifiedSchema()` before the router is reachable.

**Reverse migration (rollback)** — CRITICAL:

Naively reverting the Go binary without first reconverting integer rows to text is **destructive**. In SQLite, `INTEGER < TEXT` is always TRUE regardless of value, so the GC sweep would immediately shred every soft-deleted row that was migrated to integer.

The rollback procedure is:

1. **First**, with the PR 2 binary still running, execute the reverse migration:
   ```sql
   UPDATE memories
   SET deleted_at = datetime(deleted_at, 'unixepoch')
   WHERE typeof(deleted_at) = 'integer';
   ```
2. **Then** revert the Go binary. Old code now sees TEXT rows and the original `deleted_at < datetime('now', '-30 days')` comparison works correctly.

Document this in `cmd/mpm/handlers_gc.go` next to the comparison site as a permanent warning comment, and add a one-liner to the `release-notes/` directory so operators see it on rollback.

## PR 3 — Tooling (low risk, separate from code)

### Changes

**3.1** Add `.golangci.yml` at repo root:

```yaml
linters:
  disable-all: true
  enable:
    - errcheck      # deferred Close() error handling
    - govet         # standard go vet checks
    - ineffassign   # unused assignments
    - unused        # dead code
    - staticcheck   # extended static analysis

run:
  timeout: 5m
  tests: true
```

**3.2** Add a `lint` target to `Makefile`:

```makefile
lint:
	golangci-lint run ./...
```

**3.3** Document advisory-only status in `CONTRIBUTING.md` (or `README.md` if CONTRIBUTING doesn't exist). CI does NOT run lint yet — discover the noise floor first.

### Tests (PR 3)

No new tests. The deliverable is the config file and the noise-floor report captured in the PR description.

### Verification

`make lint` must complete without panics. Document any findings in the PR description.

## Open Items

None. All six audit gaps are addressed by these three PRs.
