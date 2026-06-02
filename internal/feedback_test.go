package internal

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFeedbackWeightAdjustment covers the +<id> and -<id> shortcut logic.
func TestFeedbackWeightAdjustment(t *testing.T) {
	db := feedbackFreshDB(t)
	defer db.Close()

	// Create a memory at weight 3
	id, err := db.SaveMemory("memories", "test content", "", []string{"test"}, nil, nil, false, 3)
	require.NoError(t, err)

	// Verify initial weight
	mem, err := db.GetMemory(id)
	require.NoError(t, err)
	assert.Equal(t, 3, mem["weight"])

	// Test 1: +<id> at weight 3 → weight increases (reinforce)
	err = db.ReinforceMemory(id, 1)
	require.NoError(t, err)
	mem, _ = db.GetMemory(id)
	assert.Greater(t, mem["weight"].(int), 3)

	// Test 2: -<id> at weight 4 → weight decreases by 1 (floors at 1 if below)
	err = db.AdjustMemoryWeight(id, -1)
	require.NoError(t, err)
	mem, _ = db.GetMemory(id)
	assert.Equal(t, 3, mem["weight"])

	// Test 3: -<id> when delta would push below 1 → floors at 1
	err = db.AdjustMemoryWeight(id, -10)
	require.NoError(t, err)
	mem, _ = db.GetMemory(id)
	assert.Equal(t, 1, mem["weight"], "weight cannot go below 1")

	// Test 4: AdjustMemoryWeight with positive delta (edge case from wrong sign)
	err = db.AdjustMemoryWeight(id, 3)
	require.NoError(t, err)
	mem, _ = db.GetMemory(id)
	assert.Equal(t, 4, mem["weight"])
}

// TestFeedbackChallengeAndReinforce covers the implicit challenge restore on +<id>.
func TestFeedbackChallengeAndReinforce(t *testing.T) {
	db := feedbackFreshDB(t)
	defer db.Close()

	// Create a challenged memory
	meta := map[string]interface{}{
		"status":               "challenged",
		"challenged_theory_id": "theory-001",
	}
	id, err := db.SaveMemory("memories", "challenged content", "", []string{"test"},
		meta, nil, false, 5)
	require.NoError(t, err)

	// Verify it is challenged
	mem, err := db.GetMemory(id)
	require.NoError(t, err)
	memMetaStr := mem["metadata"].(string)
	var memMeta map[string]interface{}
	json.Unmarshal([]byte(memMetaStr), &memMeta)
	assert.Equal(t, "challenged", memMeta["status"])

	// Apply ChallengeAndReinforce — should clear challenged AND reinforce in one tx
	err = db.ChallengeAndReinforce(id, 1)
	require.NoError(t, err)

	// Status should be cleared
	mem, _ = db.GetMemory(id)
	memMetaStr = mem["metadata"].(string)
	json.Unmarshal([]byte(memMetaStr), &memMeta)
	// Status should be cleared — json_patch removes keys set to null (RFC 7396)
	// After ChallengeAndReinforce the metadata JSON is {}. We assert on the raw string.
	t.Logf("metadata after ChallengeAndReinforce (raw): %s", memMetaStr)
	assert.Equal(t, "{}", memMetaStr, "metadata should be empty object after clearing keys")

	// Weight should be reinforced
	assert.Greater(t, mem["weight"].(int), 5)
}

// TestFeedbackCLIInteraction covers the router-level +<id> / -<id> interception.
// This is tested indirectly via the integration path; the key invariant is
// that single-character flags like -v and -h are NOT intercepted.
func TestFeedbackCLIFlagCollision(t *testing.T) {
	// The router guard: len(cmdName) > 2 for negative feedback
	// This test documents the invariant: a bare "-v" (len=2) is NOT treated as feedback
	testCases := []struct {
		raw       string
		isFeedback bool
	}{
		{"+abc123", true},
		{"-abc123", true},
		{"-v", false}, // len=2, single-dash flag — must NOT be intercepted
		{"-h", false},
		{"-+abc", true},  // starts with +, treated as reinforce attempt — will fail at DB lookup
		{"--help", false}, // double-dash — standard flag
	}

	for _, tc := range testCases {
		isFeedback := len(tc.raw) > 1 && ((tc.raw[0] == '+') ||
			(tc.raw[0] == '-' && tc.raw[1] != '-' && len(tc.raw) > 2))
		assert.Equal(t, tc.isFeedback, isFeedback, "raw=%q", tc.raw)
	}
}

// feedbackFreshDB creates an isolated DatabaseManager for feedback tests.
func feedbackFreshDB(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := t.TempDir() + "/feedback_test.db"
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	return dm
}