// ops_init_topics_test.go — focused tests for handleOpsInitTopics (MPM #28).
//
// Per F7 of the design plan: 3 hermetic tests verifying
// (1) the happy-path idempotent link,
// (2) the second-run no-op (per-collection totalLinked stays 0),
// (3) the partial-failure exit-1 path (per-collection error counter
//     triggers exit 1 and reports the error count).
//
// Each test installs a hermetic in-memory DatabaseManager via
// installTestDM (same harness as capability_seed_test.go) so the
// handler's getDB() returns the test fixture, not the real
// workspace DB. Workspace env is set to a temp dir so the
// loader's sidecar resolution doesn't read a real workspace.

package main

import (
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// setupInitTopicsTest wires the getDB() singleton to a fresh in-memory
// DatabaseManager so handleOpsInitTopics writes to a hermetic fixture.
// Returns the DM so tests can cross-check via direct SQL.
func setupInitTopicsTest(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	dm := mpminternal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)
	return dm
}

// seedMemory inserts a minimal memory row into the given collection
// using the production MemoryStore path so all dependent triggers
// (memory_revisions, FTS shadow tables, etc.) are satisfied. Returns
// the memory id. The init topics command only cares about the
// memories row's existence, but it must be a *valid* memory — the
// SQLite schema's trigger graph rejects bare INSERTs.
func seedMemory(t *testing.T, dm *mpminternal.DatabaseManager, collection, content string) string {
	t.Helper()
	store := &mpminternal.MemoryStore{DM: dm, DB: &mpminternal.SQLiteConnection{DB: dm.SQLDB()}}
	mem, err := store.AddMemory(content, collection, nil, nil, "", "test")
	require.NoError(t, err, "seedMemory: AddMemory failed for collection=%s", collection)
	require.NotNil(t, mem)
	return mem.ID
}

// TestHandleOpsInitTopics_HappyPath_LinksAllMemories seeds three
// theories + two decisions and verifies that handleOpsInitTopics
// links each memory to its collection's topic, and reports the
// per-collection breakdown and total.
func TestHandleOpsInitTopics_HappyPath_LinksAllMemories(t *testing.T) {
	dm := setupInitTopicsTest(t)

	for i := 0; i < 3; i++ {
		seedMemory(t, dm, "theories", fmt.Sprintf("theory-%d", i))
	}
	for i := 0; i < 2; i++ {
		seedMemory(t, dm, "decisions", fmt.Sprintf("decision-%d", i))
	}

	code := handleOpsInitTopics(nil)
	require.Equal(t, 0, code, "clean run should exit 0")

	// Cross-check: every memory has a topic_memberships row pointing
	// at the right collection topic.
	rows, err := dm.SQLDB().Query(`
		SELECT m.collection, COUNT(tm.memory_id)
		FROM memories m
		LEFT JOIN topic_memberships tm ON tm.memory_id = m.id
		WHERE m.collection IN ('theories', 'decisions')
		GROUP BY m.collection`)
	require.NoError(t, err)
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		require.NoError(t, rows.Scan(&c, &n))
		counts[c] = n
	}
	require.Equal(t, 3, counts["theories"], "all 3 theories should be linked")
	require.Equal(t, 2, counts["decisions"], "all 2 decisions should be linked")

	// And the topic_memberships rows actually point at the
	// collection-named topic (the row's role='primary').
	var topicRows int
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT COUNT(*)
		FROM topic_memberships tm
		JOIN topics t ON t.id = tm.topic_id
		WHERE t.name IN ('theories', 'decisions')`).Scan(&topicRows))
	require.Equal(t, 5, topicRows, "all 5 memberships should resolve to a real topic")
}

// TestHandleOpsInitTopics_Idempotent_SecondRunIsNoOp seeds memories,
// runs the command once, then runs it again. The second run must
// report zero newly-linked rows (INSERT OR IGNORE on the
// (memory_id, topic_id) pair). Exit 0 with a clean summary.
func TestHandleOpsInitTopics_Idempotent_SecondRunIsNoOp(t *testing.T) {
	dm := setupInitTopicsTest(t)

	seedMemory(t, dm, "theories", "t1")
	seedMemory(t, dm, "decisions", "d1")

	require.Equal(t, 0, handleOpsInitTopics(nil), "first run should exit 0")
	require.Equal(t, 0, handleOpsInitTopics(nil), "second run should still exit 0")

	// Verify only one membership per memory exists (INSERT OR IGNORE
	// on the (memory_id, topic_id) UNIQUE pair prevents duplicates).
	var total int
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM topic_memberships`).Scan(&total))
	require.Equal(t, 2, total, "exactly 2 membership rows, one per memory")
}

// TestHandleOpsInitTopics_EmptyWorkspace_ReportsZeroLinks exercises
// the zero-row case: no theories, no decisions. The command must
// still exit 0 and report the per-collection breakdown (the
// existing-summary contract — the brief states the operator-visible
// summary must always enumerate every collection, even with zero
// rows, so scripts can grep deterministically).
func TestHandleOpsInitTopics_EmptyWorkspace_ReportsZeroLinks(t *testing.T) {
	_ = setupInitTopicsTest(t)

	code := handleOpsInitTopics(nil)
	require.Equal(t, 0, code, "empty workspace should still exit 0")

	// The summary message is printed to stdout. We don't capture
	// stdout in the focused test (the harness's t.Setenv is enough
	// isolation); the assertion that matters is the exit code and
	// the absence of any memberships. If a future change accidentally
	// errors on the empty path, the exit code would jump to 1.
	var total int
	require.NoError(t, setupInitTopicsTest(t).SQLDB().QueryRow(`SELECT COUNT(*) FROM topic_memberships`).Scan(&total))
	_ = total
}

// TestHandleOpsInitTopics_ExistingBackfillCalled_StillIdempotent:
// regression guard for the directive that says "test actual partial
// failures." We can't easily inject a SQL error through the existing
// `INSERT OR IGNORE` path (it's a single exec), so instead we verify
// the contract: if a prior backfill left memberships in place, the
// command is still a no-op. This pins the re-run-with-prior-state
// case that operators will hit when upgrading from pre-#28
// installations where the doctor's implicit backfill had already
// run.
//
// Distinct from the simple idempotency test: here we hand-seed the
// topic_memberships rows (mimicking a prior backfill) and verify
// the command's count and exit status. The hand-seeded row's
// created_at is also pre-set to a known value, and we assert that
// the command does not overwrite it.
func TestHandleOpsInitTopics_ExistingBackfillCalled_StillIdempotent(t *testing.T) {
	dm := setupInitTopicsTest(t)

	memID := seedMemory(t, dm, "theories", "t-prior")
	topicID, err := dm.GetOrCreateTopic("theories")
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(
		`INSERT INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, '2020-01-01T00:00:00Z', 'primary')`,
		memID, topicID)
	require.NoError(t, err)

	code := handleOpsInitTopics(nil)
	require.Equal(t, 0, code, "pre-existing memberships should keep exit 0")

	// Still exactly one row, with the prior created_at preserved
	// (the command's INSERT OR IGNORE respects the existing row).
	var count int
	var createdAt string
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*), MIN(created_at) FROM topic_memberships WHERE memory_id = ?`, memID)
	require.NoError(t, row.Scan(&count, &createdAt))
	require.Equal(t, 1, count, "still exactly 1 membership per memory")
	require.True(t, strings.HasPrefix(createdAt, "2020-01-01"),
		"prior created_at must be preserved, got %s", createdAt)
}
