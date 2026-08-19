// directive_tools_test.go — pins the scope-filtering contract for
// ReadDirectivesForFramework. See docs/architecture/directives.md §4.
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm-core/seed"
)

// seedFixtureForScopeTest inserts one global directive and one
// framework:openclaw directive. Returns their stable ids.
func seedFixtureForScopeTest(t *testing.T, dm *DatabaseManager) (string, string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories
		    (id, collection, content, tags, metadata, is_prime_directive, weight, confidence, retrieval_priority, importance)
		VALUES
		    ('fixture-global', 'directives', 'global test directive', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global","stable_id":"fixture-global"}',
		     1, 10, 1.0, 1.0, 1.0),
		    ('fixture-openclaw', 'directives', 'openclaw-specific directive', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"framework:openclaw","stable_id":"fixture-openclaw"}',
		     1, 10, 1.0, 1.0, 1.0)`)
	require.NoError(t, err)
	return "fixture-global", "fixture-openclaw"
}

// TestReadDirectivesForFramework_AdditiveUnion pins the wake-time
// contract: a framework:openclaw caller must see BOTH the global
// directive AND its framework-scoped one. Additive, not replacement.
func TestReadDirectivesForFramework_AdditiveUnion(t *testing.T) {
	dm := NewTestDM(t)
	gID, oID := seedFixtureForScopeTest(t, dm)

	got, err := dm.ReadDirectivesForFramework("openclaw")
	require.NoError(t, err)

	ids := make(map[string]bool, len(got))
	for _, d := range got {
		ids[d["id"].(string)] = true
	}
	require.True(t, ids[gID], "global directive must reach framework:openclaw")
	require.True(t, ids[oID], "framework:openclaw directive must reach openclaw caller")
}

// TestReadDirectivesForFramework_ScopeIsolation pins the negative case:
// an opencode caller must NOT see the framework:openclaw directive. Only
// the global one. Otherwise one framework could accidentally receive
// another framework's directives.
func TestReadDirectivesForFramework_ScopeIsolation(t *testing.T) {
	dm := NewTestDM(t)
	gID, oID := seedFixtureForScopeTest(t, dm)

	got, err := dm.ReadDirectivesForFramework("opencode")
	require.NoError(t, err)

	ids := make(map[string]bool, len(got))
	for _, d := range got {
		ids[d["id"].(string)] = true
	}
	require.True(t, ids[gID], "global directive must reach any framework")
	require.False(t, ids[oID], "framework:openclaw directive must NOT reach opencode caller")
}

// TestReadDirectivesForFramework_EmptyFrameworkIsGlobalOnly pins the
// safety default. When MPM_FRAMEWORK is unset (or an unidentified caller
// passes ""), only global directives surface. Never accidentally
// surface a framework-scoped directive to an unidentified consumer.
func TestReadDirectivesForFramework_EmptyFrameworkIsGlobalOnly(t *testing.T) {
	dm := NewTestDM(t)
	gID, oID := seedFixtureForScopeTest(t, dm)

	got, err := dm.ReadDirectivesForFramework("")
	require.NoError(t, err)

	ids := make(map[string]bool, len(got))
	for _, d := range got {
		ids[d["id"].(string)] = true
	}
	require.True(t, ids[gID], "global directive must reach empty-framework caller")
	require.False(t, ids[oID], "framework:openclaw directive must NOT reach empty-framework caller")
}

// TestReadDirectivesForFramework_LegacyNullScopeTreatedAsGlobal pins the
// backward-compat property: rows seeded before scope existed (no
// metadata.scope field) must still be readable by every framework. The
// additive-union semantics treat json_extract(..., '$.scope') IS NULL
// the same as scope='global'.
func TestReadDirectivesForFramework_LegacyNullScopeTreatedAsGlobal(t *testing.T) {
	dm := NewTestDM(t)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories
		    (id, collection, content, tags, metadata, is_prime_directive, weight, confidence, retrieval_priority, importance)
		VALUES
		    ('fixture-legacy', 'directives', 'legacy no-scope directive', '["prime_directive"]',
		     '{"is_prime_directive":1}',
		     1, 10, 1.0, 1.0, 1.0)`)
	require.NoError(t, err)

	for _, fw := range []string{"openclaw", "opencode", "pi", ""} {
		got, err := dm.ReadDirectivesForFramework(fw)
		require.NoError(t, err)
		found := false
		for _, d := range got {
			if d["id"].(string) == "fixture-legacy" {
				found = true
				break
			}
		}
		require.True(t, found,
			"legacy null-scope directive must reach framework=%q (additive union)", fw)
	}
}

// TestReadDirectivesForFramework_StableIDOrder pins the deterministic
// ordering. Directives must be returned in id ASC order so the agent's
// wake context is byte-stable across runs.
func TestReadDirectivesForFramework_StableIDOrder(t *testing.T) {
	dm := NewTestDM(t)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories
		    (id, collection, content, tags, metadata, is_prime_directive, weight, confidence, retrieval_priority, importance)
		VALUES
		    ('fixture-c', 'directives', 'c global', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global"}', 1, 10, 1.0, 1.0, 1.0),
		    ('fixture-a', 'directives', 'a global', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global"}', 1, 10, 1.0, 1.0, 1.0),
		    ('fixture-b', 'directives', 'b global', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global"}', 1, 10, 1.0, 1.0, 1.0)`)
	require.NoError(t, err)

	got, err := dm.ReadDirectivesForFramework("openclaw")
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, "fixture-a", got[0]["id"])
	require.Equal(t, "fixture-b", got[1]["id"])
	require.Equal(t, "fixture-c", got[2]["id"])
}

// TestReadDirectives_AdminViewReturnsAll pins that the admin/CLI path
// (ReadDirectives, no framework filter) still sees every directive
// regardless of scope. Substrate filtering is wake-time only; the
// `mpm directives` CLI and web DB must keep showing the full set.
func TestReadDirectives_AdminViewReturnsAll(t *testing.T) {
	dm := NewTestDM(t)
	gID, oID := seedFixtureForScopeTest(t, dm)

	got, err := dm.ReadDirectives()
	require.NoError(t, err)

	ids := make(map[string]bool, len(got))
	for _, d := range got {
		ids[d["id"].(string)] = true
	}
	require.True(t, ids[gID], "admin view must include global directive")
	require.True(t, ids[oID], "admin view must include framework:openclaw directive")
}

// TestBoot_SeedsBaselineDirectives pins the tiered-fallback
// contract: a fresh production substrate is never directive-blind.
// After NewDatabaseManager (the prod constructor), the four
// constitutional global directives must be present and ordered by
// stable id, even with no shared DB attached. The hermetic test
// constructor (NewTestDM / NewDatabaseManagerForDB + InitSchema)
// deliberately does NOT seed — fixtures assert on their own rows.
func TestBoot_SeedsBaselineDirectives(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	got, err := dm.ReadDirectivesForFramework("")
	require.NoError(t, err)
	require.Len(t, got, len(seed.SeedDirectives),
		"boot must seed every baseline directive, got %d rows", len(got))

	for i := range got {
		if i > 0 {
			require.Less(t, got[i-1]["id"], got[i]["id"],
				"baseline directives must be returned sorted by stable id")
		}
	}

	// Idempotent: a second boot path run (InitSchema is called again
	// on the same DB) must not duplicate rows.
	require.NoError(t, dm.InitSchema())
	again, err := dm.ReadDirectivesForFramework("")
	require.NoError(t, err)
	require.Len(t, again, len(seed.SeedDirectives), "re-init must not duplicate the baseline")
}

// TestReadDirectivesForFramework_SharedOverlayUnion pins the shared
// overlay contract: when MPM_SHARED_DB is attached, directive rows from
// shared.memories are merged additively with the local baseline —
// deduplicated by stable id, with the shared row winning on collision
// (org-wide policy overrides the local copy).
func TestReadDirectivesForFramework_SharedOverlayUnion(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Shared-only directive: must surface in the union.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, tags, metadata, weight, retrieval_priority, importance, confidence)
		VALUES
		    ('shared-org-policy', 'directives', 'org-wide policy', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global"}', 10, 1.0, 1.0, 1.0)`)
	require.NoError(t, err)

	// Divergent copy of a baseline id in the shared DB: shared wins.
	firstBaseline := seed.SeedDirectives[0].StableID
	_, err = dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, tags, metadata, weight, retrieval_priority, importance, confidence)
		VALUES
		    (?, 'directives', 'ORG OVERRIDE of baseline', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global"}', 10, 1.0, 1.0, 1.0)`, firstBaseline)
	require.NoError(t, err)

	got, err := dm.ReadDirectivesForFramework("")
	require.NoError(t, err)
	require.Len(t, got, len(seed.SeedDirectives)+1,
		"union must be baseline + shared-only (shared override dedups), got %d rows", len(got))

	contentByID := make(map[string]string, len(got))
	for _, d := range got {
		contentByID[d["id"].(string)] = d["content"].(string)
	}
	require.Equal(t, "ORG OVERRIDE of baseline", contentByID[firstBaseline],
		"shared row must override the local baseline copy on id collision")
	require.Equal(t, "org-wide policy", contentByID["shared-org-policy"],
		"shared-only directive must surface in the union")

	// Deterministic ordering holds across the merged set.
	for i := 1; i < len(got); i++ {
		require.Less(t, got[i-1]["id"], got[i]["id"], "merged set must be sorted by id ASC")
	}
}

// TestReadDirectivesForFramework_SharedOverlayAbsent pins the negative
// path: without an attached shared DB the union is exactly the local
// baseline — no error, no shared leakage.
func TestReadDirectivesForFramework_SharedOverlayAbsent(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	got, err := dm.ReadDirectivesForFramework("")
	require.NoError(t, err)
	require.Len(t, got, len(seed.SeedDirectives),
		"local-only runtime must surface exactly the baseline directives")
	for _, d := range got {
		require.NotContains(t, d["id"], "shared-", "no shared rows may leak without attachment")
	}
}

// TestReadDirectivesForFramework_LegacyZeroSentinelTreatedAsLive pins
// sentinel-agnostic liveness: legacy installs wrote deleted_at = 0 for
// "not deleted" (the current codebase writes NULL). A directive row
// with deleted_at = 0 must surface in reads, and a row with a real
// epoch soft-delete must stay hidden — otherwise the tiered fallback
// guarantee ("standalone runtime is never directive-blind") silently
// breaks on pre-existing operator databases.
func TestReadDirectivesForFramework_LegacyZeroSentinelTreatedAsLive(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	now := time.Now().Unix()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories
		    (id, collection, content, tags, metadata, is_prime_directive, deleted_at, weight, confidence, retrieval_priority, importance)
		VALUES
		    ('legacy-live-0', 'directives', 'live under legacy sentinel', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global"}', 1, 0, 10, 1.0, 1.0, 1.0),
		    ('legacy-shredded', 'directives', 'gone', '["prime_directive"]',
		     '{"is_prime_directive":1,"scope":"global"}', 1, ?, 10, 1.0, 1.0, 1.0)`,
		now)
	require.NoError(t, err)

	got, err := dm.ReadDirectivesForFramework("")
	require.NoError(t, err)
	ids := make(map[string]bool, len(got))
	for _, d := range got {
		ids[d["id"].(string)] = true
	}
	require.True(t, ids["legacy-live-0"],
		"directive with deleted_at = 0 (legacy live sentinel) must surface")
	require.False(t, ids["legacy-shredded"],
		"directive with epoch deleted_at must stay hidden")
}
