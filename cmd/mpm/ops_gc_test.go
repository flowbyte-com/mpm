package main

// Regression test for the GC cooldown bug.
//
// The GC atomic lock lives in system_config under the key 'last_gc_at'.
// Before the upsert fix, the lock query was a plain UPDATE that returned
// 0 rowsAffected on a fresh DB (no row existed) — the engine misread that
// as "another GC is on cooldown" and silently aborted the maintenance loop.
//
// The fix changes the query to INSERT ... ON CONFLICT(key) DO UPDATE.
// These tests pin the four behavioural guarantees:
//
//   1. First run on a fresh DB claims the lock (1 row).
//   2. A second run within the cooldown window does NOT claim (0 rows).
//   3. A run after the cooldown window claims (1 row).
//   4. An operator who DELETEd the row (or any other row-loss event)
//      gets self-healing on the next run (1 row).
//
// We test the SQL directly via dm.SQLDB().Exec rather than going through
// the handleOpsMaintain function — the query is the contract; the wrapper
// is just argument parsing and output formatting.

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"mpm/internal"

	"github.com/stretchr/testify/require"
)

// gcClaimSlot is the actual SQL used by the GC cooldown lock, kept verbatim
// from cmd/mpm/handlers.go. If you change it there, change it here. The
// test is a contract for the query, not the wrapper.
func gcClaimSlot(t *testing.T, db *sql.DB, ts string, maxAgeHours int) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('last_gc_at', ?, '')
		ON CONFLICT(key) DO UPDATE SET
		  raw_json = excluded.raw_json,
		  updated_at = CURRENT_TIMESTAMP
		WHERE (
		  system_config.raw_json IS NULL
		  OR json_extract(system_config.raw_json, '$.updated_at') IS NULL
		  OR datetime(json_extract(system_config.raw_json, '$.updated_at')) < datetime('now', '-' || ? || ' hours')
		)
	`, ts, strconv.Itoa(maxAgeHours))
	require.NoError(t, err)
	rows, err := res.RowsAffected()
	require.NoError(t, err)
	return rows
}

func newTestDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	return internal.NewTestDM(t)
}

func TestGCLock_FreshDB_ClaimsSlot(t *testing.T) {
	dm := newTestDM(t)
	// Fresh DB has no last_gc_at row. The upsert must INSERT and report
	// rowsAffected=1 — this is the original bug (was 0).
	ts := `{"updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	rows := gcClaimSlot(t, dm.SQLDB(), ts, 24)
	require.Equal(t, int64(1), rows, "fresh DB should claim GC slot on first run")
}

func TestGCLock_WithinCooldown_Skips(t *testing.T) {
	dm := newTestDM(t)
	ts := `{"updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	// First run claims.
	require.Equal(t, int64(1), gcClaimSlot(t, dm.SQLDB(), ts, 24))
	// Second run within the 24h cooldown must NOT claim.
	rows := gcClaimSlot(t, dm.SQLDB(), ts, 24)
	require.Equal(t, int64(0), rows, "second run within cooldown should be skipped")
}

func TestGCLock_AfterCooldown_Claims(t *testing.T) {
	dm := newTestDM(t)
	// Seed with a timestamp 48h ago.
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	oldJSON := `{"updated_at":"` + old + `"}`
	require.Equal(t, int64(1), gcClaimSlot(t, dm.SQLDB(), oldJSON, 24))

	// Run again with maxAge=24h. The old timestamp is older than that,
	// so the slot should be claimable.
	fresh := `{"updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	rows := gcClaimSlot(t, dm.SQLDB(), fresh, 24)
	require.Equal(t, int64(1), rows, "run after cooldown expiry should claim")
}

func TestGCLock_OperatorDeletedRow_SelfHeals(t *testing.T) {
	dm := newTestDM(t)
	// Seed once.
	ts := `{"updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	require.Equal(t, int64(1), gcClaimSlot(t, dm.SQLDB(), ts, 24))

	// Operator (or bug) deletes the row.
	_, err := dm.SQLDB().Exec(`DELETE FROM system_config WHERE key = 'last_gc_at'`)
	require.NoError(t, err)

	// Next run must self-heal — this is the resilience property that the
	// schema-bootstrap option would NOT provide. Without the upsert, the
	// lock query would silently start failing again.
	rows := gcClaimSlot(t, dm.SQLDB(), ts, 24)
	require.Equal(t, int64(1), rows, "deleted row should self-heal on next run")
}

func TestGCLock_EmptyRawJSON_ClaimsOnFirstRun(t *testing.T) {
	// Edge case: someone INSERTed the row with raw_json='{}' (no timestamp
	// field). The WHERE clause explicitly checks for NULL on the extracted
	// updated_at, so the upsert claims the slot — same semantic as a fresh
	// DB. Without the explicit IS NULL check, json_extract on a missing key
	// returns NULL, and `NULL < <anything>` is NULL (falsy), short-circuiting
	// the WHERE clause and silently no-oping the GC cooldown.
	dm := newTestDM(t)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash) VALUES ('last_gc_at', '{}', '')
	`)
	require.NoError(t, err)

	ts := `{"updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	rows := gcClaimSlot(t, dm.SQLDB(), ts, 24)
	require.Equal(t, int64(1), rows, "empty JSON object treated as 'no timestamp available' — should claim")
}
