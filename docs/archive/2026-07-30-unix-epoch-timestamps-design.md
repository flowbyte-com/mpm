# Unify All Timestamp Columns on Unix-Epoch Seconds — 2026-07-30

## Goal

Migrate every remaining `DATETIME DEFAULT CURRENT_TIMESTAMP` (TEXT ISO 8601) column in MPM's SQLite schema to `INTEGER` Unix-epoch seconds. Brings the schema to a single, consistent representation for time values and closes the cross-column comparison bug surfaced in the 2026-07-23 audit (`CURRENT_TIMESTAMP < integer` is always TRUE in SQLite's manifest typing, causing premature shredding).

## Background

MPM is partially migrated. Integer unix-epoch columns already include `memories.deleted_at`, `memories.last_synthesized_at`, `scheduled_wakes.target_time`, `scheduled_wakes.fired_at`, `evidence.created_at`, `evidence.expires_at`, `confidence_history.computed_at`, `schema_migrations.applied_at`, `synth_runs.first_run_at`, `synth_runs.last_run_at`. The precedent `MigrateDeletedAtToUnixEpoch` (`internal/core/migration_deleted_at.go`) already backfills legacy TEXT rows to INTEGER using a sentinel row in `schema_migrations`.

What remains is ~33 columns across 19 tables that still store `DATETIME DEFAULT CURRENT_TIMESTAMP`. These mix with the INTEGER columns in queries like `ORDER BY created_at DESC` and `WHERE updated_at < ?`, which produce subtle correctness bugs when SQLite's manifest typing compares integer against text.

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Scope | All 33 DATETIME columns across 19 tables | User directive; consistent end state |
| Resolution | Seconds (INTEGER) | Matches `deleted_at_unified_v1` precedent and existing integer columns |
| Go struct fields | `int64` (non-nullable) / `*int64` (nullable) | Cleaner JSON marshaling than `sql.NullInt64`; native NULL scan support; idiomatic Go |
| Display boundary | `FormatUnixSeconds` / `FormatOptionalUnixSeconds` helpers | Centralize RFC3339 formatting at CLI/MCP boundary; storage type-change isolated |
| CLI input | Accept both RFC3339 string AND unix-epoch integer | Backward compatible; documented in `--help` |
| Migration shape | One function, one sentinel (`timestamps_unified_v1`) | Mirrors `deleted_at_unified_v1` audit style; one transaction; ~50 lines |
| Tie-breaker for same-second ties | Add `rowid DESC` (or natural PK like `version DESC`) to ORDER BYs that lost fractional precision | Prevents non-deterministic sort order |
| MCP JSON schemas | Tools that accept `--as-of`-style args get `type: ["string", "integer"]` | Prevents MCP client from rejecting int before Go parses it |
| `memory_revisions.created_at` | Included in migration (drops fractional precision) | SQLite `strftime('%s', ...)` cleanly truncates fractional part; no exception needed |

## Scope — Columns Migrated

| Table | Columns |
|---|---|
| `sessions` | `created_at` |
| `topics` | `created_at`, `updated_at` |
| `topic_memberships` | `created_at` |
| `memories` | `created_at`, `updated_at`, `last_accessed_at`, `expires_at` |
| `system_config` | `updated_at` |
| `retrieval_metadata` | `created_at`, `updated_at`, `last_retrieved_at` |
| `external_db_cursors` | `updated_at` |
| `reference_docs` | `created_at`, `last_indexed` |
| `system_audit_log` | `created_at` |
| `audit_cluster_proposals` | `created_at`, `updated_at`, `first_seen`, `last_seen`, `snooze_until` |
| `session_handoffs` | `created_at`, `ended_at`, `read_at` |
| `scheduled_wakes` | `created_at` |
| `scheduled_tasks` | `created_at`, `updated_at`, `last_run_at`, `next_run_at` |
| `ephemeral_scratchpad` | `created_at`, `updated_at`, `decay_at` |
| `vector_clusters` | `updated_at` |
| `vector_assignments` | `updated_at` |
| `reference_interactions` | `created_at` |
| `admission_log` | `created_at` |
| `memory_revisions` | `created_at` (drops `STRFTIME('%Y-%m-%d %H:%M:%f','NOW')` format) |

**Defaults:** Every migrated column with `DEFAULT CURRENT_TIMESTAMP` becomes `DEFAULT (CAST(strftime('%s','now') AS INTEGER))`. `memory_revisions.created_at` default changes from `STRFTIME('%Y-%m-%d %H:%M:%f','NOW')` to the same `CAST(strftime('%s','now') AS INTEGER)` form.

**Out of scope:** `active.json` `Updated` field, scratchpad JSON files, working-context JSON, `raw_memories` (already INTEGER/REAL with own conventions), `memories.promoted_at` (already REAL). `parseClusterSnoozeUntil` (`internal/core/cluster_proposals.go:395`) gains unix-epoch int support for symmetry with `--as-of`.

## Migration Function

**File:** `internal/core/migration_timestamps.go` (new, paired with `migration_deleted_at.go`).

**Pattern** (mirrors `MigrateDeletedAtToUnixEpoch` at `internal/core/migration_deleted_at.go:24-59`):

```go
func MigrateAllTimestampsToUnixEpoch(tx *sql.Tx) error
```

**Steps:**

1. **Sentinel check** — bail if `schema_migrations` already has row `id = 'timestamps_unified_v1'`.
2. **Per-column UPDATE** — for each entry in a hardcoded `[]timestampColumn{table, column, nullable}` list, run:
   ```sql
   UPDATE <table> SET <col> = CAST(strftime('%s', <col>) AS INTEGER)
   WHERE <col> IS NOT NULL AND typeof(<col>) = 'text'
   ```
   The `typeof() = 'text'` guard is the idempotency mechanism — skips already-integer rows.
3. **Record sentinel** — `INSERT INTO schema_migrations (id, applied_at) VALUES ('timestamps_unified_v1', ?)`.

**Invocation** — wired into `DatabaseManager.init` immediately after `MigrateDeletedAtToUnixEpoch(tx)`. Both wrap in the same init transaction.

## Go Struct Field Changes

| Struct | Field changes |
|---|---|
| `Memory` | `CreatedAt int64`, `UpdatedAt int64`, `LastAccessedAt *int64`, `ExpiresAt *int64` |
| `Session` | `CreatedAt int64` |
| `Topic` | `CreatedAt int64`, `UpdatedAt int64` |
| `TopicMembership` | `CreatedAt int64` |
| `SystemConfig` | `UpdatedAt int64` |
| `RetrievalMetadata` | `CreatedAt int64`, `UpdatedAt int64`, `LastRetrievedAt *int64` |
| `ExternalDBCursor` | `UpdatedAt int64` |
| `ReferenceDoc` | `CreatedAt int64` |
| `AuditEntry` | `CreatedAt int64` |
| `AuditClusterProposal` | `CreatedAt int64`, `UpdatedAt int64`, `FirstSeen int64`, `LastSeen int64`, `SnoozeUntil *int64` |
| `Handoff` | `CreatedAt int64`, `EndedAt int64`, `ReadAt *int64` |
| `Wake` | `CreatedAt int64` |
| `ScheduledTask` | `CreatedAt int64`, `UpdatedAt int64`, `LastRunAt *int64`, `NextRunAt int64` |
| `ScratchpadRow` | `CreatedAt int64`, `UpdatedAt int64`, `DecayAt int64` |
| `VectorCluster` | `UpdatedAt int64` |
| `VectorAssignment` | `UpdatedAt int64` |
| `ReferenceInteraction` | `CreatedAt int64` |
| `AdmissionLogRow` | `CreatedAt int64` |
| `MemoryRevision` | `CreatedAt int64` |

**Helper file:** `internal/core/format_time.go`

```go
// FormatUnixSeconds returns RFC3339-formatted UTC string for an int64 epoch.
func FormatUnixSeconds(sec int64) string {
    return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// FormatOptionalUnixSeconds returns RFC3339 or empty string if nil.
func FormatOptionalUnixSeconds(sec *int64) string {
    if sec == nil {
        return ""
    }
    return time.Unix(*sec, 0).UTC().Format(time.RFC3339)
}
```

## SQL Query Updates

**Category 1 — `datetime('now', ...)` → `CAST(strftime('%s','now', ...) AS INTEGER)`:**

| Location | Current pattern |
|---|---|
| `internal/core/audit.go:163` | `WHERE created_at >= datetime('now', ?)` |
| `internal/core/audit.go:248` | `DELETE FROM system_audit_log WHERE created_at < datetime('now', ?)` |
| `internal/core/handoff.go:323` | `WHERE created_at < datetime('now', '-' \|\| ? \|\| ' days')` |
| `internal/core/wake_context.go:530` | `WHERE created_at >= datetime('now', '-7 days')` |
| `internal/core/wake_context.go:735` | `AND created_at >= datetime('now', '-30 days')` |

**Category 2 — Boundary parameter sites** (caller passes RFC3339 string → must become `int64`):

| Function | Location | Caller change |
|---|---|---|
| `PruneMemoriesBefore(before time.Time)` | `internal/core/web_db.go:1313-1314` | `before.Unix()` |
| `GetMemoriesByDateRange(fromDate, toDate string)` | `internal/core/db.go:2522, 2679-2681` | parse both to `int64` at boundary |
| `GetMemoryRevisionAtTime(memoryID, asOf time.Time)` | `internal/core/db.go:3057, 3099-3108` | `asOf.Unix()`; `time.Parse` cascade at lines 3064-3093 collapses because `memories.created_at` is now INTEGER |
| `RecallMemories` FTS5 / LIKE paths | `cmd/mpm/recall.go:622-678` | parse `--since`/`--until`/`--as-of` to `int64` at boundary |

**Category 3 — INSERT sites** that write `time.Now().UTC().Format(time.RFC3339)` to a migrated column → `time.Now().Unix()`:

| Location | Column written |
|---|---|
| `internal/core/web_db.go:909` | `reference_interactions.created_at` |
| `internal/core/admission_db.go:120` | `admission_log.created_at` |
| `internal/core/compact.go:261, 330` | lesson metadata (verify each) |
| `cmd/mpm/handlers_epistemology.go:155, 317` | scratchpad JSON stamps (verify field) |
| `cmd/mpm/handlers_challenge.go:32` | challenge timestamps (verify field) |
| `cmd/mpm/simple_cmds.go:739` | verify field |

**Tie-breaker additions:** Queries that lost fractional-second precision add `, rowid DESC` (or natural PK like `version DESC`) to ORDER BYs to keep ordering deterministic:

- `internal/core/db.go:3105` already orders by `version DESC` — no change needed.
- Any new `ORDER BY created_at` discovered during implementation gets `, rowid DESC` appended.

## MCP JSON Schema Updates

Tools that accept timestamp flags must update their JSON Schema definitions from `type: "string"` to `type: ["string", "integer"]` so MCP clients don't reject the integer form before Go parses it. Affected tools: any that accept `--as-of`, `--since`, `--until`, or `snooze_until`-style inputs.

## CLI Input Parsing

Single helper in `internal/core/parse_time.go`:

```go
// ParseTimestampArg accepts RFC3339 string OR unix-epoch integer.
// Used by --as-of, --since, --until, and snooze_until flags.
func ParseTimestampArg(s string) (int64, error) {
    if n, err := strconv.ParseInt(s, 10, 64); err == nil {
        return n, nil
    }
    if t, err := time.Parse(time.RFC3339, s); err == nil {
        return t.Unix(), nil
    }
    return 0, fmt.Errorf("invalid timestamp: %q (want unix-epoch integer or RFC3339)", s)
}
```

## Testing Strategy

**Test 1 — Migration function** (`migration_timestamps_test.go`)
- Table-driven across all 33 column entries.
- Per-column fixtures: TEXT (RFC3339), TEXT (RFC3339 with `Z` suffix from `time.Now().UTC().Format(time.RFC3339)`), INTEGER, NULL.
- Assert TEXT→INTEGER conversion, INTEGER unchanged, NULL unchanged, sentinel set exactly once.
- Re-run, assert idempotency.
- The `Z`-suffix fixture proves no local-timezone offset is applied during conversion.

**Test 2 — Schema fingerprint** (`schema_fingerprint_test.go`, new)
- Every column in the migration list returns `typeof() == 'integer'` after migration.

**Test 3 — Backward-compat smoke** (in `migration_timestamps_test.go`)
- Hand-built "legacy" DB with old DATETIME TEXT DDL, populated with RFC3339 timestamps.
- After migration, assert:
  - `web_db.go:QueryMemories` returns rows with `CreatedAt int64` matching `strftime('%s', seeded_rfc3339)`.
  - `db.go:GetMemoryRevisionAtTime(memoryID, asOf)` returns the right revision when `asOf.Unix()` is passed.
  - `recall.go` `--as-of` accepts both RFC3339 and int64 with identical results.

**Test 4 — Format helper** (`format_time_test.go`)
- `FormatUnixSeconds(0)` → `"1970-01-01T00:00:00Z"`
- `FormatUnixSeconds(1785421960)` → `"2026-07-30T14:32:40Z"`
- `FormatOptionalUnixSeconds(nil)` → `""`
- `FormatOptionalUnixSeconds(&zero)` → `"1970-01-01T00:00:00Z"`

**Test 5 — Existing test fixture updates**
- Grep across `*_test.go` for explicit RFC3339 INSERT values feeding migrated columns.
- Update each to `time.Now().Unix()` or hardcoded `int64`.
- Update each `Scan(...)` call expecting `time.Time` to expect `int64` / `*int64`.

**Test 6 — Write-path AST coverage** (Go AST walker, required)
- Mirrors `TestScannerCoverage_AllMemoriesWritersScanContent` precedent.
- Asserts no Go source constructs `time.Now().UTC().Format(time.RFC3339)` and feeds the result into a migrated column.
- Maintains a hardcoded whitelist (`map[string]bool` with inline justifications) for known-safe sites: `active.json`, scratchpad JSON, working-context JSON.
- Defends against SQLite manifest typing silently storing strings in INTEGER columns.

**Test 7 — End-to-end smoke** (`scripts/smoke_timestamps.sh`)
- CLI: isolated MPM_WORKSPACE, ingest a memory, read it back via `mpm recall --as-of`. Assert CLI output is RFC3339-formatted AND underlying DB column is INTEGER.
- MCP check: invoke `mpm call recall` and assert the JSON output produces RFC3339 strings via `jq -e '.[] | .created_at | match("^[0-9]{4}-[0-9]{2}-[0-9]{2}T")'`.

## Implementation Order

Work on feature branch `feat/unix-epoch-timestamps` (created from main).

**Development phase** — separate commits for own sanity:

| Step | Commit | Behavior if cherry-picked alone |
|---|---|---|
| 1 | Schema + migration + Test 1 + Test 2 | Runtime crash (DB int, Go time.Time) |
| 2 | Struct fields + format helpers | Compile passes, runtime scan fails |
| 3 | Test fixture updates | Runtime OK on writes, fails on SQL comparisons |
| 4 | SQL comparisons + boundary functions + MCP JSON Schemas | Full app works, but legacy DB un-migrated breaks |
| 5 | AST coverage + smoke script | Defensive tests only — safe to cherry-pick |
| 6 | Docs (CLAUDE.md, README) | Safe to cherry-pick |
| 7 | Final verification (`make test` both modules, `smoke_shared.sh`, `smoke_timestamps.sh`) | No commit |

**Merge phase** — once Steps 1-5 are green:

- `git rebase -i main` to **squash commits 1-4 into a single atomic commit**.
- Commits 5 (AST coverage) and 6 (docs) stay separate.
- Result: `main` never sees a state where schema is migrated but Go structs or SQL queries haven't caught up. `git bisect` stays clean.

**Squashed commit message:**

```
feat(core): unify all timestamp columns on unix-epoch seconds

Closes the gap between integer timestamp columns (deleted_at,
scheduled_wakes, evidence, confidence_history, synth_runs) and the
~33 DATETIME TEXT columns that still used CURRENT_TIMESTAMP. SQLite's
manifest typing makes "INTEGER < TEXT" always TRUE regardless of value,
which caused subtle bugs in cross-column comparisons (the deleted_at
incident the 2026-07-23 audit flagged).

Migrates 19 tables across 33 columns to INTEGER seconds. Adds the
timestamps_unified_v1 sentinel migration alongside deleted_at_unified_v1.
Resolves to int64 (non-nullable) or *int64 (nullable) in Go structs;
FormatUnixSeconds / FormatOptionalUnixSeconds centralize RFC3339
formatting at display boundaries. CLI inputs accept both RFC3339 and
unix-epoch integers. Schema fingerprint test + AST walker guard
against regression.
```

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Migration silently corrupts rows with non-standard datetime formats | Low | Medium | Test 1 covers bare-date and fractional formats. `strftime` returns NULL on unparseable strings → row keeps original TEXT value, sentinel still set. Best-effort, no data loss. |
| `time.Parse` cascade in `GetMemoryRevisionAtTime` (`db.go:3064-3093`) collapses but breaks a code path that depends on the cascade | Low | Medium | Step 4 explicitly updates the function. Test 3 covers it. |
| Hidden consumer (mpm-agent, third-party script) reads `memories.created_at` as RFC3339 string and breaks when seeing int64 | Medium | Low-High | Step 7 checks `scripts/smoke_shared.sh` and exercises `mpm call` paths. mpm-agent lives in a separate Go module — its own test suite catches breakage. README documents the change. |
| New write path added between Steps 1-4 bypasses the format helper | Low | Medium | Step 5's AST walker catches it at test time. Whitelist documented in CLAUDE.md. |
| `deleted_at_unified_v1` and `timestamps_unified_v1` migrations interact badly on a partially-migrated DB | Low | Low | Both wrapped in the same `DatabaseManager.init` transaction. Either both run or neither. |
| `CAST(strftime('%s','now') AS INTEGER)` default produces a different value than CURRENT_TIMESTAMP for the same wall-clock moment | Low | None | Both resolve to whole-second resolution. A row inserted at HH:MM:SS.999 with new default gets truncated to HH:MM:SS — matches the documented second-resolution contract. |
| MCP client rejects int arg before Go parses it | Low | Medium | Step 4 updates JSON Schemas to `type: ["string", "integer"]`. Test 7's MCP check verifies. |
| Same-second ties on `ORDER BY created_at` produce non-deterministic ordering | Low | Low-Medium | Tie-breaker additions: `rowid DESC` for new ORDER BYs; `version DESC` already covers `memory_revisions`. |

## Rollback Plan

The migration is **not reversible via SQL**. There is no automatic way to flip INTEGER columns back to DATETIME TEXT without losing the format.

1. **Recommended:** `git revert <squash-sha>` on main + restore database from pre-migration `mpm backup-db` snapshot. Clean rollback.
2. **Forward-fix** if revert is impossible in production: the atomic migration is correct; adding a strftime-coercing view layer is preferable to reverting.

**Operational guardrail:** Document in changelog that operators must take a `mpm backup-db` snapshot before installing the release containing this migration.

## Success Criteria

- All 7 test suites (Test 1-7) pass.
- `make test` green on repo root AND `internal/core/`.
- A populated dev DB migrates cleanly with zero row loss.
- A fresh DB creates rows with INTEGER timestamps by default.
- `scripts/smoke_shared.sh` and `scripts/smoke_timestamps.sh` both green.
- No false positives from the AST walker.
- Manual verification: `mpm recall --as-of 2026-07-30T00:00:00Z` AND `mpm recall --as-of 1785421960` return the same memories.
- CLAUDE.md updated, README updated, changelog entry written.