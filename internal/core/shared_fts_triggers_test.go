package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSharedFTS_TriggersKeepIndexSynced proves the structural invariant
// promised by the shared_memories_ai/_ad/_au trigger set:
//   "If a row is in shared.memories, it is searchable in shared.memories_fts."
//
// This is the regression net for the operator-workflow gap exposed by
// the 2026-07-07 smoke test (hybrid_search_federated_test.go's
// TestFederated_HybridSearchMemoriesScope was passing in the unit
// suite but the live binary returned 0 hits under scope=all when a
// fresh shared DB was seeded via a separate connection).
//
// We deliberately DO NOT manually backfill the FTS table. If the
// triggers work, the index is updated by the time INSERT returns.
//
// Requires the build tag `sqlite_fts5` to enable FTS5 in the
// production driver. The smoke script (scripts/smoke_shared.sh) is
// the integration counterpart for this test.
func TestSharedFTS_TriggersKeepIndexSynced(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Sanity: confirm the FTS5 module is actually available. The
	// trigger set is only useful if FTS5 is compiled in.
	var hasFTS5 int
	err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM pragma_module_list WHERE name = 'fts5'
	`).Scan(&hasFTS5)
	if err != nil {
		t.Skipf("pragma_module_list unavailable: %v (likely an FTS5-disabled build)", err)
	}
	if hasFTS5 == 0 {
		t.Skip("FTS5 module not enabled in this build (need -tags sqlite_fts5)")
	}

	// Confirm triggers installed.
	var triggerCount int
	err = dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.sqlite_master
		WHERE type = 'trigger' AND name LIKE 'shared_memories_%'
	`).Scan(&triggerCount)
	require.NoError(t, err)
	require.Equal(t, 4, triggerCount,
		"expected 4 shared_memories triggers (_ai, _ad, _au, _au_content); got %d", triggerCount)

	// Insert a row through mpm's own connection (the same path
	// record_global_rule uses). The trigger should auto-populate FTS.
	_, err = dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, tags, weight, is_global, deleted_at)
		VALUES ('fts-trigger-001', 'rules', 'FTS trigger regression: API keys are secrets', '[]', 10, 1, NULL)
	`)
	require.NoError(t, err)

	// FTS must now contain the row, WITHOUT any manual backfill.
	var ftsCount int
	err = dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.memories_fts
		WHERE memories_fts MATCH 'secrets'
	`).Scan(&ftsCount)
	require.NoError(t, err)
	require.Equal(t, 1, ftsCount,
		"FTS index should auto-populate on INSERT (regression for the 2026-07-07 footgun)")

	// DELETE should remove the FTS row.
	_, err = dm.db.Exec(`DELETE FROM shared.memories WHERE id = 'fts-trigger-001'`)
	require.NoError(t, err)

	err = dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.memories_fts
		WHERE memories_fts MATCH 'secrets'
	`).Scan(&ftsCount)
	require.NoError(t, err)
	require.Equal(t, 0, ftsCount, "DELETE should remove the FTS entry")

	// UPDATE of content should re-index (delete-old + insert-new).
	_, err = dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, tags, weight, is_global, deleted_at)
		VALUES ('fts-trigger-002', 'rules', 'original token xyzzy', '[]', 10, 1, NULL)
	`)
	require.NoError(t, err)

	_, err = dm.db.Exec(`
		UPDATE shared.memories SET content = 'updated token plugh' WHERE id = 'fts-trigger-002'
	`)
	require.NoError(t, err)

	err = dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.memories_fts
		WHERE memories_fts MATCH 'xyzzy'
	`).Scan(&ftsCount)
	require.NoError(t, err)
	require.Equal(t, 0, ftsCount, "old content should be gone from FTS after UPDATE")

	err = dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.memories_fts
		WHERE memories_fts MATCH 'plugh'
	`).Scan(&ftsCount)
	require.NoError(t, err)
	require.Equal(t, 1, ftsCount, "new content should be in FTS after UPDATE")

	// Soft-delete (deleted_at set) should remove the FTS row.
	_, err = dm.db.Exec(`
		UPDATE shared.memories SET deleted_at = '2026-07-07' WHERE id = 'fts-trigger-002'
	`)
	require.NoError(t, err)
	err = dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.memories_fts
		WHERE memories_fts MATCH 'plugh'
	`).Scan(&ftsCount)
	require.NoError(t, err)
	require.Equal(t, 0, ftsCount, "soft-delete should remove the FTS entry")
}

// TestSharedFTS_FederatedScopeAll_TriggersOnly verifies the end-to-end
// claim: an INSERT through dm.db.Exec (the same path record_global_rule
// uses) makes the row immediately queryable via HybridSearchMemories
// scope=all, WITHOUT any manual FTS backfill. This is the assertion
// the smoke script's step [3/6] was trying to make.
func TestSharedFTS_FederatedScopeAll_TriggersOnly(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Skip on FTS5-disabled builds.
	var hasFTS5 int
	_ = dm.db.QueryRow(`SELECT COUNT(*) FROM pragma_module_list WHERE name = 'fts5'`).Scan(&hasFTS5)
	if hasFTS5 == 0 {
		t.Skip("FTS5 module not enabled in this build (need -tags sqlite_fts5)")
	}

	// Seed local + shared rows through mpm's own connection.
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, weight, deleted_at)
		VALUES ('local-trigger-ref', 'memories', 'house architectural memory decisions', '[]', 5, NULL)
	`)
	require.NoError(t, err)

	_, err = dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, tags, weight, is_global, deleted_at)
		VALUES ('shared-trigger-ref', 'rules', 'house architectural memory house rule', '[]', 10, 1, NULL)
	`)
	require.NoError(t, err)

	// NO manual FTS backfill. The triggers should have done it.
	items, err := dm.HybridSearchMemories("house architectural", "", 10, "all")
	require.NoError(t, err)
	require.NotEmpty(t, items, "scope=all should return hits after a normal INSERT")

	var foundShared, foundLocal bool
	for _, it := range items {
		origin, _ := it["origin"].(string)
		if strings.Contains(stringFromMap(it["id"]), "shared-trigger") {
			foundShared = origin == "shared"
		}
		if strings.Contains(stringFromMap(it["id"]), "local-trigger") {
			foundLocal = origin == "local"
		}
	}
	require.True(t, foundShared,
		"shared row must appear in scope=all results without manual FTS backfill (got %d items)", len(items))
	require.True(t, foundLocal,
		"local row must appear in scope=all results without manual FTS backfill (got %d items)", len(items))
}

// stringFromMap safely extracts a string from a map[string]interface{}
// (the items returned by HybridSearchMemories are map types and id may
// be a string OR a fmt.Stringer depending on the code path).
func stringFromMap(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return ""
}
