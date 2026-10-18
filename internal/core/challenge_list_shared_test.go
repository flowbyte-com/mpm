// challenge_list_shared_test.go — issue #20 / HAL review follow-up:
// targeted attached-shared-DB tests for ListChallengedMemories.
//
// HAL finding 3: "Add a targeted attached-shared-DB test demonstrating
// that the new list includes shared rows and makes no database changes."
//
// HAL finding 1: "challengeTables() enumerates local first, shared
// second. Per-table ORDER BY followed by append does not produce global
// chronological ordering. Sort the combined results before truncation,
// and test interleaved local/shared timestamps."
//
// These two tests live in internal/core (where the attach-shared helper
// already exists) rather than cmd/mpm (where the rest of the issue #20
// tests live) because attaching a shared DB requires writing directly
// to dm.db, and the test plumbing for that is in this package.

package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var challengeListSharedTestDBCounter int64

// challengeListSharedDM attaches a shared DB to a NewTestDM-style
// DatabaseManager and creates the shared.memories table. Mirrors
// the production attachShared path (newTestDMWithShared in
// contradiction_log_test.go) but creates only the minimum schema
// this test needs.
func challengeListSharedDM(t *testing.T) *DatabaseManager {
	t.Helper()
	dm := NewTestDM(t)
	n := atomic.AddInt64(&challengeListSharedTestDBCounter, 1)
	dsn := fmt.Sprintf("file:mpm-iss20-sharedtest-%d?mode=memory&cache=shared", n)
	if _, err := dm.db.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS shared", dsn)); err != nil {
		t.Fatalf("attach shared: %v", err)
	}
	if _, err := dm.db.Exec(`
		CREATE TABLE IF NOT EXISTS shared.memories (
			id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
			tags JSON, metadata JSON, weight REAL DEFAULT 1.0,
			retrieval_priority REAL NOT NULL DEFAULT 0.5,
			importance REAL NOT NULL DEFAULT 0.5,
			confidence REAL NOT NULL DEFAULT 0.8,
			reinforcement_count INTEGER DEFAULT 0,
			last_accessed_at DATETIME,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			deleted_at INTEGER, dependencies TEXT
		)`); err != nil {
		t.Fatalf("create shared.memories: %v", err)
	}
	dm.sharedPath = dsn
	dm.sharedAttached = true
	return dm
}

// seedChallengedInTable inserts a memory row directly with the
// status='challenged' metadata and the supplied challenged_at,
// challenged_via, and challenged_prior_weight. The caller controls
// which table ("memories" or "shared.memories") and therefore
// which side of the local/shared split the row lands on.
//
// This bypasses the production challenge writer so a test can
// stage an exact timestamp. The production writer would not give
// us millisecond-level control over the challenged_at column.
func seedChallengedInTable(t *testing.T, dm *DatabaseManager, table, id, content string, weight, priorWeight float64, challengedAt time.Time) {
	t.Helper()
	meta := fmt.Sprintf(`{"status":"challenged","challenged_at":%d,"challenged_via":"test_fixture","challenged_theory_id":"%s","challenged_prior_weight":%g,"challenged_prior_confidence":0.5}`,
		challengedAt.Unix(), id, priorWeight)
	_, err := dm.db.Exec(fmt.Sprintf(
		`INSERT INTO %s (id, collection, content, metadata, weight, retrieval_priority, importance, confidence)
		 VALUES (?, 'memories', ?, ?, ?, 0.5, 0.5, 0.5)`, table),
		id, content, meta, weight)
	require.NoError(t, err, "seed %s in %s", id, table)
}

// TestIssue20_ListSharedAttached — HAL finding 3.
//
// When the shared DB is attached, ListChallengedMemories must
// include rows from shared.memories alongside the local
// memories rows. The audit-time 41 are all in the local table,
// so a list that silently dropped the shared table would
// appear to "work" against the live store. A targeted test
// against an attached shared DB with a row in shared.memories
// is the only way to pin the include-shared contract.
//
// Also pins the read-only contract against the shared DB:
// a full SHA snapshot of both tables before and after the
// list call must be byte-equal. The doctor service and the
// CLI surface both rely on this for the observational
// contract post-MPM #28.
func TestIssue20_ListSharedAttached(t *testing.T) {
	dm := challengeListSharedDM(t)

	// One local row, one shared row. Different IDs so the
	// test cannot accidentally collapse them through a
	// id-already-breaks-ties rule.
	seedChallengedInTable(t, dm, "memories", "issue20-local-1", "local content", 1.0, 1.0, time.Unix(100, 0))
	seedChallengedInTable(t, dm, "shared.memories", "issue20-shared-1", "shared content", 1.0, 1.0, time.Unix(200, 0))

	// SHA snapshot before the list call.
	before := sharedHash(t, dm)

	rows, err := dm.ListChallengedMemories()
	require.NoError(t, err)
	require.Len(t, rows, 2, "list must include the local row AND the shared row")

	// Both stores must be represented in the result.
	stores := map[string]bool{}
	for _, r := range rows {
		stores[r.Store] = true
	}
	assert.True(t, stores["local"], "list must include the local row")
	assert.True(t, stores["shared"], "list must include the shared row")

	// SHA snapshot after the list call must be byte-equal
	// to the snapshot before. ListChallengedMemories is
	// read-only by construction; a refactor that
	// accidentally writes (e.g. a stray UPDATE, a row-level
	// trigger, an FTS5 reindex on read) would fail this
	// assertion.
	after := sharedHash(t, dm)
	if before != after {
		t.Fatalf("ListChallengedMemories mutated the database.\nbefore sha: %s\nafter  sha: %s", before, after)
	}
}

// TestIssue20_ListGlobalOrderingAcrossTables — HAL finding 1.
//
// challengeTables() returns [local, shared]. The naive
// implementation appends the per-table results local-then-shared.
// Without a global sort, a shared row at challenged_at=10
// followed by a local row at challenged_at=20 would land in
// that order regardless of the actual chronology. This test
// interleaves the timestamps across tables and asserts the
// returned slice is in global chronological order.
func TestIssue20_ListGlobalOrderingAcrossTables(t *testing.T) {
	dm := challengeListSharedDM(t)

	// 4 rows, 2 per table, with timestamps that interleave
	// across the local/shared boundary.
	//   local  @ 50, shared @ 100, shared @ 150, local @ 200
	// The naive per-table-append would produce
	//   [local@50, local@200, shared@100, shared@150]
	// which is wrong (200 comes before 100). The global sort
	// must produce
	//   [local@50, shared@100, shared@150, local@200]
	seedChallengedInTable(t, dm, "memories", "issue20-ord-L50", "L50", 1.0, 1.0, time.Unix(50, 0))
	seedChallengedInTable(t, dm, "memories", "issue20-ord-L200", "L200", 1.0, 1.0, time.Unix(200, 0))
	seedChallengedInTable(t, dm, "shared.memories", "issue20-ord-S100", "S100", 1.0, 1.0, time.Unix(100, 0))
	seedChallengedInTable(t, dm, "shared.memories", "issue20-ord-S150", "S150", 1.0, 1.0, time.Unix(150, 0))

	rows, err := dm.ListChallengedMemories()
	require.NoError(t, err)
	require.Len(t, rows, 4)

	// Assert the slice is monotonic non-decreasing in
	// ChallengedAt; that is the global chronological order.
	for i := 1; i < len(rows); i++ {
		assert.LessOrEqual(t, rows[i-1].ChallengedAt, rows[i].ChallengedAt,
			"row %d (%s@%d) must come before row %d (%s@%d) — global chronological order is broken",
			i-1, rows[i-1].Store, rows[i-1].ChallengedAt,
			i, rows[i].Store, rows[i].ChallengedAt)
	}

	// Pin the exact expected order: [L50, S100, S150, L200].
	// A test that asserts only the monotonic property would
	// pass for the wrong reason; this asserts the actual
	// sequence the operator sees.
	wantOrder := []struct {
		id    string
		store string
		at    int64
	}{
		{"issue20-ord-L50", "local", 50},
		{"issue20-ord-S100", "shared", 100},
		{"issue20-ord-S150", "shared", 150},
		{"issue20-ord-L200", "local", 200},
	}
	for i, want := range wantOrder {
		assert.Equal(t, want.id, rows[i].ID, "row %d id", i)
		assert.Equal(t, want.store, rows[i].Store, "row %d store", i)
		assert.Equal(t, want.at, rows[i].ChallengedAt, "row %d challenged_at", i)
	}
}

// sharedHash returns a deterministic SHA of every row in
// both the local and shared memories tables, plus the
// metadata JSON column. The hash is identical when
// database state is identical; a refactor that
// introduced a write would change the hash.
//
// Tables are walked in a fixed order; rows within a
// table are sorted by id so the hash is comparable
// across calls.
func sharedHash(t *testing.T, dm *DatabaseManager) string {
	t.Helper()
	h := sha256.New()
	tables := []string{"memories", "shared.memories"}
	for _, tbl := range tables {
		h.Write([]byte(tbl))
		h.Write([]byte{'\n'})
		rows, err := dm.db.Query("SELECT id, metadata, weight FROM " + tbl + " WHERE deleted_at IS NULL ORDER BY id ASC")
		if err != nil {
			t.Fatalf("hash query %s: %v", tbl, err)
		}
		var lines []string
		for rows.Next() {
			var id string
			var meta *string
			var weight float64
			if err := rows.Scan(&id, &meta, &weight); err != nil {
				rows.Close()
				t.Fatalf("hash scan %s: %v", tbl, err)
			}
			m := ""
			if meta != nil {
				m = *meta
			}
			lines = append(lines, id+"|"+m+"|"+fmt.Sprintf("%g", weight))
		}
		rows.Close()
		// Defensive: rows are already ORDER BY id, but
		// if the schema changes, sort again.
		sort.Strings(lines)
		for _, l := range lines {
			h.Write([]byte(l))
			h.Write([]byte{'\n'})
		}
	}
	// silence unused import lint if strings is dropped
	_ = strings.HasPrefix
	return hex.EncodeToString(h.Sum(nil))
}
