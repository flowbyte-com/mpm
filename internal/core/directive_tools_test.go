// directive_tools_test.go — pins the scope-filtering contract for
// ReadDirectivesForFramework. See docs/architecture/directives.md §4.
package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
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
