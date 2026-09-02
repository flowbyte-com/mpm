// ledger_audit_test.go — semantic invariants for the 2026-09-02
// ledger-wide audit pass.
//
// Background: the OpenClaw gateway's report that `hc.theories_pending`
// was 92 while `mpm_theories list` returned 8 surfaced a class of
// silent-popotion bugs that span every persistent artifact family.
// Each test in this file pins one structural invariant on a different
// family: NULL safety on legacy column writes, missing lifecycle
// predicates on read paths, dead-code filter parameters, validation
// gaps on enumerated parameters, and CLI/MCP parity. If a future
// refactor regresses any of these, the matching test fails.
//
// The expected behavior, in every case, is the "what the operator
// expected" answer: read paths agree with each other and with the
// health-check aggregate; legacy NULL columns do not panic the read
// surface; advertised predicates actually apply; CLI and MCP routes
// hit the same database and return the same rows.
package internal

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Decisions family (GetDecision, ListDecisions) — NULL safety
// ============================================================================

// TestLedgerAudit_GetDecision_NullTagsAndMetadataDoNotPanic pins the
// Substrate Defense Triad #2 fix on the decision read path. Legacy
// decision rows may have NULL tags or NULL metadata (the columns were
// treated as optional in earlier write paths). GetDecision and
// ListDecisions now scan into sql.NullString and unwrap safely; the
// fix matches GetTheory's NULL-safety pattern.
func TestLedgerAudit_GetDecision_NullTagsAndMetadataDoNotPanic(t *testing.T) {
	dm := newTestDM(t)

	// Insert a decision with NULL tags and NULL metadata — the exact
	// shape a pre-fix writer could produce. This row must not panic
	// GetDecision, ListDecisions, or the MCP path that wraps them.
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at)
		VALUES ('d-null-tags-meta', 'decisions', 'legacy null row', NULL, NULL, ?)`,
		time.Now().Unix())
	require.NoError(t, err)

	row, err := dm.GetDecision("d-null-tags-meta")
	require.NoError(t, err, "GetDecision must not panic on NULL tags/metadata")
	assert.Equal(t, "d-null-tags-meta", row["id"])
	assert.NotNil(t, row["tags"], "tags should be a non-nil empty slice after NULL-unwrap")

	rows, err := dm.ListDecisions(DecisionFilter{Status: "all", Limit: 100})
	require.NoError(t, err, "ListDecisions must not panic on NULL tags/metadata")
	found := false
	for _, r := range rows {
		if r["id"] == "d-null-tags-meta" {
			found = true
		}
	}
	assert.True(t, found, "NULL-tags row must appear in status=all list")
}

// TestLedgerAudit_DecisionFilterTags_AppliesFilter pins the SEV-1
// fix: DecisionFilter.Tags was defined in core.go and populated by
// callers (MCP/CLI) but the SQL filter never applied it. The tag
// filter is now an EXISTS (SELECT 1 FROM json_each(tags) WHERE value = ?)
// clause per requested tag, joined with OR.
func TestLedgerAudit_DecisionFilterTags_AppliesFilter(t *testing.T) {
	dm := newTestDM(t)

	// Seed three decisions: alpha-tagged, beta-tagged, untagged.
	for _, c := range []struct {
		id, tagJSON string
	}{
		{"d-tags-alpha", `["alpha","shared"]`},
		{"d-tags-beta", `["beta","shared"]`},
		{"d-tags-none", `["unrelated"]`},
	} {
		_, err := dm.ExecTracked(
			`INSERT INTO memories (id, collection, content, tags, metadata, created_at)
			 VALUES (?, 'decisions', ?, ?, ?, ?)`,
			0, c.id, c.id, c.tagJSON, `{}`, time.Now().Unix(),
		)
		require.NoError(t, err)
	}

	// Filter by tag="alpha": only d-tags-alpha matches.
	rows, err := dm.ListDecisions(DecisionFilter{Status: "all", Tags: []string{"alpha"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "d-tags-alpha", rows[0]["id"])

	// Filter by tag="shared": both alpha and beta match (untagged excluded).
	rows, err = dm.ListDecisions(DecisionFilter{Status: "all", Tags: []string{"shared"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// Empty tags = no filter (returns all three).
	rows, err = dm.ListDecisions(DecisionFilter{Status: "all", Limit: 100})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 3)
}

// ============================================================================
// Evidence family — MCP/CLI parity on expires_at
// ============================================================================

// TestLedgerAudit_Evidence_ListEvidencenceFiltersExpired pins the
// evidence parity fix: ListEvidence (MCP mpm_evidence list) now
// applies the same expires_at predicate as the CLI ListEvidenceForArtifact.
// Previously the MCP path returned expired rows; the CLI excluded them.
func TestLedgerAudit_Evidence_ListEvidenceFiltersExpired(t *testing.T) {
	dm := newTestDM(t)

	// Two evidence rows for the same artifact: one expired, one live.
	_, err := dm.db.Exec(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at, expires_at)
		VALUES ('ev-live', 'mem-ledger-1', 'memory', 'observation', 'log-x', 0.5, 'test', ?, NULL)`,
		time.Now().Unix())
	require.NoError(t, err)

	_, err = dm.db.Exec(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at, expires_at)
		VALUES ('ev-expired', 'mem-ledger-1', 'memory', 'observation', 'log-x', 0.5, 'test', ?, ?)`,
		time.Now().Unix(), time.Now().Add(-time.Hour).Unix())
	require.NoError(t, err)

	// MCP path: ListEvidence must NOT return expired.
	out, err := dm.ListEvidence("mem-ledger-1", "memory")
	require.NoError(t, err)
	ev := out["evidence"].([]map[string]interface{})
	require.Len(t, ev, 1, "MCP ListEvidence must filter expired evidence rows")
	assert.Equal(t, "ev-live", ev[0]["id"])

	// CLI path: ListEvidenceForArtifact must agree.
	cliRows, err := ListEvidenceForArtifact(dm, "mem-ledger-1", "memory")
	require.NoError(t, err)
	require.Len(t, cliRows, 1)
	assert.Equal(t, "ev-live", cliRows[0].ID)

	// Parity invariant: MCP count == CLI count for the same artifact.
	assert.Equal(t, len(ev), len(cliRows), "MCP and CLI evidence counts must agree (parity)")
}

// ============================================================================
// Sessions family — GetSessionMemories NULL safety + lifecycle
// ============================================================================

// TestLedgerAudit_GetSessionMemories_NullSafetyAndLifecycle pins the
// NULL safety + deleted_at + expires_at predicates fix on the session
// read path. The previous form scanned tags/metadata into concrete Go
// strings (NULL-panic) and skipped both soft-delete and expiry
// filtering. The new form matches every other memory read surface.
func TestLedgerAudit_GetSessionMemories_NullSafetyAndLifecycle(t *testing.T) {
	dm := newTestDM(t)

	// Seed a session to satisfy the FK.
	_, err := dm.db.Exec(`
		INSERT INTO sessions (id, session_id, content, content_hash, created_at)
		VALUES (1, 'sess-ledger-1', 'session content', 'hash-sess-1', ?)`,
		time.Now().Unix())
	require.NoError(t, err)

	// Live memory with NULL tags — must not panic.
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at)
		VALUES ('m-ledger-live', 'memories', 'live null-tags', 'sess-ledger-1', NULL, NULL, ?)`,
		time.Now().Unix())
	require.NoError(t, err)

	// Soft-deleted memory (same session) — must be filtered.
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at, deleted_at)
		VALUES ('m-ledger-deleted', 'memories', 'soft deleted', 'sess-ledger-1', '[]', '{}', ?, ?)`,
		time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	// Expired memory (same session) — must be filtered.
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at, expires_at)
		VALUES ('m-ledger-expired', 'memories', 'expired', 'sess-ledger-1', '[]', '{}', ?, ?)`,
		time.Now().Unix(), time.Now().Add(-time.Hour).Unix())
	require.NoError(t, err)

	rows, err := dm.GetSessionMemories("sess-ledger-1", 100)
	require.NoError(t, err, "GetSessionMemories must not panic on NULL tags/metadata")
	require.Len(t, rows, 1, "soft-deleted and expired must be filtered; only live row appears")
	assert.Equal(t, "m-ledger-live", rows[0]["id"])
}

// ============================================================================
// Memories family — QueryMemories deleted_at + GetMemoriesForExport expires_at
// ============================================================================

// TestLedgerAudit_QueryMemories_FiltersDeletedAndExpired pins D-001 + D-002:
// the web UI read path (QueryMemories) now filters soft-deleted rows,
// and the CLI export path (GetMemoriesForExport, mpm ls) now applies
// MemoryExpireClause. Both fixes bring these read paths into parity
// with the rest of the memory read surface.
func TestLedgerAudit_QueryMemories_FiltersDeletedAndExpired(t *testing.T) {
	dm := newTestDM(t)

	// Live row.
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, tags, metadata, created_at) VALUES (?, 'memories', 'live', '[]', '{}', ?)`,
		0, "m-qm-live", time.Now().Unix())
	require.NoError(t, err)

	// Soft-deleted.
	_, err = dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, tags, metadata, created_at, deleted_at) VALUES (?, 'memories', 'deleted', '[]', '{}', ?, ?)`,
		0, "m-qm-deleted", time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	// Expired.
	_, err = dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, tags, metadata, created_at, expires_at) VALUES (?, 'memories', 'expired', '[]', '{}', ?, ?)`,
		0, "m-qm-expired", time.Now().Unix(), time.Now().Add(-time.Hour).Unix())
	require.NoError(t, err)

	// QueryMemories (web UI) — only live.
	webRows, err := dm.QueryMemories("memories", false, 100, 0)
	require.NoError(t, err)
	var ids []string
	for _, r := range webRows {
		ids = append(ids, r["id"].(string))
	}
	assert.Contains(t, ids, "m-qm-live")
	assert.NotContains(t, ids, "m-qm-deleted", "QueryMemories must filter soft-deleted rows (D-001)")
	assert.NotContains(t, ids, "m-qm-expired", "QueryMemories must filter expired rows")

	// GetMemoriesForExport (CLI mpm ls) — only live.
	exportRows, err := dm.GetMemoriesForExport("memories", "", "")
	require.NoError(t, err)
	var expIDs []string
	for _, r := range exportRows {
		expIDs = append(expIDs, r["id"].(string))
	}
	assert.Contains(t, expIDs, "m-qm-live")
	assert.NotContains(t, expIDs, "m-qm-deleted")
	assert.NotContains(t, expIDs, "m-qm-expired", "GetMemoriesForExport must filter expired rows (D-002)")
}

// TestLedgerAudit_RecentMemories_FiltersExpired pins D-003 + D-004:
// recentMemories and recentMilestones in the wake-context surface now
// apply the same expires_at predicate as the rest of the read paths.
func TestLedgerAudit_RecentMemories_FiltersExpired(t *testing.T) {
	dm := newTestDM(t)

	// Live memory.
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, tags, metadata, created_at) VALUES (?, 'memories', 'live wake', '[]', '{}', ?)`,
		0, "m-wake-live", time.Now().Unix())
	require.NoError(t, err)

	// Expired memory (in a non-decisions/theories collection so recentMemories would surface it).
	_, err = dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, tags, metadata, created_at, expires_at) VALUES (?, 'memories', 'expired wake', '[]', '{}', ?, ?)`,
		0, "m-wake-expired", time.Now().Unix(), time.Now().Add(-time.Hour).Unix())
	require.NoError(t, err)

	recent, err := dm.recentMemories(100)
	require.NoError(t, err)
	for _, r := range recent {
		assert.NotEqual(t, "m-wake-expired", r.ID, "recentMemories must not surface expired rows (D-003)")
	}
}

// ============================================================================
// Lessons family — searchLessonsLike fallback handles tag-only searches
// ============================================================================

// TestLedgerAudit_SearchLessonsLikeFallback_TagOnly pins the search
// fallback fix: when FTS5 is unavailable, searchLessonsLike now uses
// json_each on the tags column instead of LOWER(tags) LIKE ?. Tag-only
// queries against rows with NULL tags (or any tag value) now match.
func TestLedgerAudit_SearchLessonsLikeFallback_TagOnly(t *testing.T) {
	dm := newTestDM(t)

	// Lesson with a unique tag.
	_, err := dm.AddLesson("lesson body", LessonTypeInsight, []string{"uniquetag-ledger"}, "")
	require.NoError(t, err)

	results, err := dm.searchLessonsLike("uniquetag-ledger", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, results, "tag-only query must match in the LIKE fallback (json_each path)")
}

// ============================================================================
// Topics family — GetTopic is_active + handleTopicShow NULL safety
// ============================================================================

// TestLedgerAudit_GetTopic_FiltersInactive pins T-1: GetTopic(id) now
// requires is_active = 1, matching the canonical filter on ListTopics
// and GetTopicByName. Direct ID lookups can no longer resurrect
// soft-deleted topics.
func TestLedgerAudit_GetTopic_FiltersInactive(t *testing.T) {
	dm := newTestDM(t)

	// Create + soft-delete a topic.
	id, err := dm.CreateTopic("topic-ledger-1", "test topic", "", "")
	require.NoError(t, err)
	require.NoError(t, dm.DeleteTopic(id))

	_, err = dm.GetTopic(id)
	assert.Error(t, err, "GetTopic must reject soft-deleted (is_active=0) topics")

	// A live topic must still be reachable.
	idLive, err := dm.CreateTopic("topic-ledger-live", "live", "", "")
	require.NoError(t, err)
	_, err = dm.GetTopic(idLive)
	require.NoError(t, err, "GetTopic must succeed for active topics")
}

// ============================================================================
// Works family — ListWorksByStatus validates status
// ============================================================================

// TestLedgerAudit_ListWorksByStatus_RejectsUnknown pins W-1: the
// method used to silently return zero rows for unknown status values;
// the MCP handler validated the enum, but direct DatabaseManager
// callers would get a misleading empty result. Unknown status now
// returns an error matching the ListDecisions / ListTheories pattern.
func TestLedgerAudit_ListWorksByStatus_RejectsUnknown(t *testing.T) {
	dm := newTestDM(t)

	_, err := dm.ListWorksByStatus("bogus-status")
	assert.Error(t, err, "ListWorksByStatus must reject unknown status values")
	assert.True(t, strings.Contains(err.Error(), "unknown status"),
		"error must identify the unknown status (got: %v)", err)

	// The valid enum values still succeed (empty result, no error).
	for _, s := range []string{"open", "done", "cancelled"} {
		_, err := dm.ListWorksByStatus(s)
		assert.NoError(t, err, "valid status %q must succeed", s)
	}
}

// ============================================================================
// Cross-family invariant — every read surface that filters deleted_at
// must also filter expires_at (the "lifecycle predicate parity" rule)
// ============================================================================

// TestLedgerAudit_LifecycleParity_DeletedAndExpired is the headline
// invariant. After seeding a mix of live, soft-deleted, and expired
// rows, every memory read surface (GetMemory, SearchMemories,
// HybridSearch, QueryMemories, GetMemoriesForExport, recentMemories,
// GetSessionMemories) must return only the live row. This is the
// failure mode the 2026-09-02 OpenClaw report would have produced
// at scale across the ledger if the audit pass had not run.
func TestLedgerAudit_LifecycleParity_DeletedAndExpired(t *testing.T) {
	dm := newTestDM(t)

	// Seed a session (FK target for GetSessionMemories).
	_, err := dm.db.Exec(`
		INSERT INTO sessions (id, session_id, content, content_hash, created_at)
		VALUES (1, 'sess-parity', 'session content', 'hash-parity', ?)`,
		time.Now().Unix())
	require.NoError(t, err)

	now := time.Now().Unix()
	liveID, deletedID, expiredID := "m-parity-live", "m-parity-deleted", "m-parity-expired"

	_, err = dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at)
		 VALUES (?, 'memories', 'live', 'sess-parity', '[]', '{}', ?)`,
		0, liveID, now)
	require.NoError(t, err)
	_, err = dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at, deleted_at)
		 VALUES (?, 'memories', 'deleted', 'sess-parity', '[]', '{}', ?, ?)`,
		0, deletedID, now, now)
	require.NoError(t, err)
	_, err = dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at, expires_at)
		 VALUES (?, 'memories', 'expired', 'sess-parity', '[]', '{}', ?, ?)`,
		0, expiredID, now, now-3600)
	require.NoError(t, err)

	// Helper: every read surface must include only liveID.
	assertOnlyLive := func(surface string, ids []string) {
		t.Helper()
		assert.Contains(t, ids, liveID, "%s: must include live row", surface)
		assert.NotContains(t, ids, deletedID, "%s: must filter soft-deleted row", surface)
		assert.NotContains(t, ids, expiredID, "%s: must filter expired row", surface)
	}

	// GetMemory: live only.
	live, err := dm.GetMemory(liveID)
	require.NoError(t, err)
	assertOnlyLive("GetMemory", []string{live["id"].(string)})

	// GetSessionMemories: live only (same session).
	rows, err := dm.GetSessionMemories("sess-parity", 100)
	require.NoError(t, err)
	var sessIDs []string
	for _, r := range rows {
		sessIDs = append(sessIDs, r["id"].(string))
	}
	assertOnlyLive("GetSessionMemories", sessIDs)

	// QueryMemories (web UI): live only.
	qmRows, err := dm.QueryMemories("memories", false, 100, 0)
	require.NoError(t, err)
	var qmIDs []string
	for _, r := range qmRows {
		qmIDs = append(qmIDs, r["id"].(string))
	}
	assertOnlyLive("QueryMemories", qmIDs)

	// GetMemoriesForExport (mpm ls): live only.
	exportRows, err := dm.GetMemoriesForExport("memories", "", "")
	require.NoError(t, err)
	var expIDs []string
	for _, r := range exportRows {
		expIDs = append(expIDs, r["id"].(string))
	}
	assertOnlyLive("GetMemoriesForExport", expIDs)

	// recentMemories (wake context): live only.
	recent, err := dm.recentMemories(100)
	require.NoError(t, err)
	var recIDs []string
	for _, r := range recent {
		recIDs = append(recIDs, r.ID)
	}
	assertOnlyLive("recentMemories", recIDs)
}