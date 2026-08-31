// w007_challenge_banner_parity_test.go — surface-parity regression for the
// challenge banner (W-007, 2026-08-31).
//
// Contract locked in by this test:
//   1. handleShowMemory returns `is_challenged: true` and a populated
//      `banner` field for a memory whose metadata.status == "challenged".
//   2. The memory's stored `content` is NOT mutated — the banner is a
//      top-level sibling field, not a prefix injected into the body.
//   3. handleMpmResolve's memory branch advertises the same
//      `is_challenged` / `banner` pair so callers using mpm:// URIs see
//      the same challenge signal as callers using the direct show path.
//   4. Non-challenged memories surface `is_challenged: false` and an
//      empty banner — the banner pair is always present, never missing.
package tools

import (
	"encoding/json"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedChallengedMemoryForBanner writes a memory then patches its
// metadata to carry `status: "challenged"`. This mirrors what
// mpm_challenge does at runtime without pulling the entire challenge
// workflow into this regression test.
func seedChallengedMemoryForBanner(t *testing.T, dm *mpminternal.DatabaseManager, content string) string {
	t.Helper()
	id, err := dm.SaveMemory("memories", content, "", []string{"w007"}, nil, nil, false, 5.0)
	require.NoError(t, err)

	meta := map[string]interface{}{
		"status":                  "challenged",
		"challenged_prior_weight": 5.0,
		"challenged_theory_id":    "theory-w007-fixture",
	}
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`UPDATE memories SET metadata = ? WHERE id = ?`, string(metaJSON), id)
	require.NoError(t, err)
	return id
}

// TestW007_ShowMemory_SurfacesChallengeBanner is the canonical
// parity assertion. handleShowMemory must advertise the challenge
// status without mutating the stored content.
func TestW007_ShowMemory_SurfacesChallengeBanner(t *testing.T) {
	dm := newTestDMForTools(t)
	originalContent := "deploy to prod via canary — verified 2026-08-30"
	id := seedChallengedMemoryForBanner(t, dm, originalContent)

	out, err := handleShowMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"id": id,
	})
	require.NoError(t, err)
	resp, ok := out.(map[string]interface{})
	require.True(t, ok)

	// Banner pair present.
	assert.Equal(t, true, resp["is_challenged"], "is_challenged must be true for challenged memory")
	banner, _ := resp["banner"].(string)
	assert.NotEmpty(t, banner, "banner must be non-empty when is_challenged is true")

	// Content NOT mutated.
	content, _ := resp["content"].(string)
	assert.Equal(t, originalContent, content, "stored content must remain untouched; banner is a sibling field, not a prefix")
}

// TestW007_ShowMemory_NonChallenged_HasEmptyBanner ensures the banner
// pair is always present (never missing) so callers can branch on the
// field without nil-checking.
func TestW007_ShowMemory_NonChallenged_HasEmptyBanner(t *testing.T) {
	dm := newTestDMForTools(t)
	id, err := dm.SaveMemory("memories", "stable baseline fact", "", []string{"w007"}, nil, nil, false, 5.0)
	require.NoError(t, err)

	out, err := handleShowMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"id": id,
	})
	require.NoError(t, err)
	resp, ok := out.(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, false, resp["is_challenged"])
	banner, _ := resp["banner"].(string)
	assert.Equal(t, "", banner, "banner must be empty string (not missing) for non-challenged memories")
}

// TestW007_MpmResolve_MemoryBranch_SurfacesBanner exercises the URI
// resolution path. A challenge banner advertised on show but missing on
// resolve would be a contract asymmetry — this locks in parity.
func TestW007_MpmResolve_MemoryBranch_SurfacesBanner(t *testing.T) {
	dm := newTestDMForTools(t)
	originalContent := "rate limit is 100 req/min per partner"
	id := seedChallengedMemoryForBanner(t, dm, originalContent)

	out, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri":       "mpm://memory/" + id,
		"max_bytes": 1024,
	})
	require.NoError(t, err)
	resp, ok := out.(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, true, resp["is_challenged"], "resolve must advertise is_challenged with show parity")
	banner, _ := resp["banner"].(string)
	assert.NotEmpty(t, banner, "resolve must advertise a banner with show parity")

	// Content NOT mutated.
	content, _ := resp["content"].(string)
	assert.Equal(t, originalContent, content, "resolve must not mutate the stored content; banner is a sibling field")
}
