package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDirectives_AllReadPathsAgree locks in the unified read-path contract
// for prime directives. Three identifiers can mark a row as a directive:
//
//   1. collection = 'directives'           — set by save_to_memory
//   2. is_prime_directive = 1              — legacy column, web/CLI paths
//   3. metadata / tags LIKE '%is_prime_directive%' — older web UI heuristic
//
// Each read path used to query by exactly one identifier, so a directive
// ingested via path A was invisible to readers on paths B and C. This
// regression test asserts that all three paths return the same set when
// given the same database state — split-brain would fail the assertion.
func TestDirectives_AllReadPathsAgree(t *testing.T) {
	dm := newTestDM(t)

	// Three directive-style memories, each marked by a different identifier.
	// Plus one regular memory that must NOT appear in any path.
	rows := []struct {
		id      string
		coll    string
		content string
		prime   int
		tags    string
		meta    string
	}{
		{"legacy", "memories", "Legacy column directive", 1, "[]", `{"is_prime_directive":1}`},
		{"collection", "directives", "Collection-only directive", 0, "[]", `{}`},
		{"both", "directives", "Both identifiers set", 1, "[]", `{"is_prime_directive":1}`},
		{"regular", "memories", "Just a memory — should be excluded", 0, "[]", `{}`},
	}
	for _, r := range rows {
		_, err := dm.ExecTracked(`
			INSERT INTO memories (id, collection, content, is_prime_directive, tags, metadata)
			VALUES (?, ?, ?, ?, ?, ?)
		`, 0, r.id, r.coll, r.content, r.prime, r.tags, r.meta)
		require.NoError(t, err)
	}

	want := []string{"legacy", "collection", "both"}

	// Path 1: MCP read_directives (the agent-facing surface).
	got, err := dm.ReadDirectives()
	require.NoError(t, err)
	assert.Equal(t, want, idsOf(got), "MCP ReadDirectives must include all three identifier paths")

	// Path 2: web UI QueryMemories(primeOnly=true).
	webRows, err := dm.QueryMemories("", true, 100, 0)
	require.NoError(t, err)
	assert.Equal(t, want, idsOf(webRows), "Web QueryMemories(primeOnly) must include all three identifier paths")

	// Path 3: the mpm-agent query shape — assert it agrees on count. (mpm-agent
	// is a separate Go module so we cannot import its retrieveDirectives
	// directly; instead we run its exact WHERE clause against the same DB.)
	var mpmAgentCount int
	row := dm.QueryRowTracked(`
		SELECT COUNT(*) FROM memories
		WHERE (metadata LIKE '%is_prime_directive%' OR collection = 'directives')
		  AND deleted_at IS NULL
	`)
	require.NoError(t, row.Scan(&mpmAgentCount))
	assert.Equal(t, len(want), mpmAgentCount, "mpm-agent retrieveDirectives must include all three identifier paths")

	// Counts must match across every path — the parity guarantee.
	assert.Equal(t, len(want), len(got), "MCP count must match expected")
	assert.Equal(t, len(want), len(webRows), "Web count must match MCP")
	assert.Equal(t, len(got), len(webRows), "MCP and Web counts must agree")
}

func idsOf(rows []map[string]interface{}) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if id, ok := r["id"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}