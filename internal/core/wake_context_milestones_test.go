// wake_context_milestones_test.go — pins the recentMilestones contract.
//
// The wake-context narrative-arc surface has three load-bearing
// invariants, all encoded here:
//
//  1. Tag anchor: only memories tagged type:milestone-* are surfaced.
//     A regular memory in the 30d window MUST NOT appear.
//  2. Window: 30 days. Memories older than that MUST NOT appear.
//  3. Limit: at most `limit` rows (5 in production, configurable in
//     tests for boundary coverage).
//
// Plus the GatherWakeContext integration: the RecentMilestones block
// reaches the wake payload only when milestones exist.

package internal

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedMilestone inserts one row into memories with the given tags and
// created_at. created_at is the raw ISO-8601 string so the test can
// simulate an ancient milestone without sleeping for 30 days.
func seedMilestone(t *testing.T, dm *DatabaseManager, content string, tags []string, createdAt string) string {
	t.Helper()
	tagsJSON, err := json.Marshal(tags)
	require.NoError(t, err)
	id := GenerateID()
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, content, tags, collection, created_at, updated_at, weight)
		VALUES (?, ?, ?, 'memories', ?, ?, 0.5)`,
		id, content, string(tagsJSON), createdAt, createdAt,
	)
	require.NoError(t, err)
	return id
}

// TestRecentMilestones_OnlyAnchoredRows surfaces the tag-anchor
// invariant — only memories with the type:milestone-* tag appear.
func TestRecentMilestones_OnlyAnchoredRows(t *testing.T) {
	dm := newTestDMForWake(t)

	now := time.Now().UTC().Format(time.RFC3339)
	seedMilestone(t, dm, "milestone: shipped feature X", []string{"type:milestone-shipped"}, now)
	seedMilestone(t, dm, "regular memory", []string{"general-knowledge"}, now)
	seedMilestone(t, dm, "another regular memory", []string{"session:abc"}, now)
	seedMilestone(t, dm, "milestone: learned Y", []string{"type:milestone-insight"}, now)

	got := dm.recentMilestones(5)
	if len(got) != 2 {
		t.Fatalf("expected 2 milestones (anchored), got %d: %+v", len(got), got)
	}
	for i, m := range got {
		if !containsMS(m.Content, "milestone:") {
			t.Errorf("got[%d] content %q is not a milestone", i, m.Content)
		}
	}
}

// TestRecentMilestones_Only30DayWindow pins the rolling window.
// Simulating an ancient milestone is done by setting created_at
// directly; the function uses that column literally.
func TestRecentMilestones_Only30DayWindow(t *testing.T) {
	dm := newTestDMForWake(t)

	now := time.Now().UTC()
	veryOld := now.AddDate(0, 0, -45).Format(time.RFC3339) // 45 days ago
	edge := now.AddDate(0, 0, -29).Format(time.RFC3339)    // 29 days — inside window
	today := now.Format(time.RFC3339)

	seedMilestone(t, dm, "ancient: pre-window", []string{"type:milestone-shipped"}, veryOld)
	seedMilestone(t, dm, "edge case: 29d old", []string{"type:milestone-shipped"}, edge)
	seedMilestone(t, dm, "today: fresh", []string{"type:milestone-insight"}, today)

	got := dm.recentMilestones(5)
	if len(got) != 2 {
		t.Fatalf("expected 2 milestones in 30d window (edge+today), got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if m.Content == "ancient: pre-window" {
			t.Errorf("ancient milestone leaked into 30d window: %+v", m)
		}
	}
}

// TestRecentMilestones_Limit pins the cap. 7 candidates, limit 5 → 5
// returned, newest-first ordering.
func TestRecentMilestones_Limit(t *testing.T) {
	dm := newTestDMForWake(t)

	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		ts := now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		seedMilestone(t, dm, "milestone #"+string(rune('A'+i)), []string{"type:milestone-shipped"}, ts)
	}

	got := dm.recentMilestones(5)
	if len(got) != 5 {
		t.Fatalf("expected exactly 5 (limit), got %d", len(got))
	}
	if !containsMS(got[0].Content, "#G") {
		t.Errorf("newest milestone expected to be '#G' (most recent insert), got %q", got[0].Content)
	}
	if !containsMS(got[4].Content, "#C") {
		t.Errorf("oldest retained milestone expected to be '#C' (4th insert), got %q", got[4].Content)
	}
}

// TestRecentMilestones_DoesNotMisclassifyPrefixes pins the
// false-positive guard. The LIKE pattern includes the trailing JSON
// string-close anchor so a memory whose *content* mentions the
// "type:milestone-" substring but has no such tag MUST NOT match.
// Tags are JSON-encoded, content is not; the LIKE on tags cannot
// accidentally match content.
func TestRecentMilestones_DoesNotMisclassifyPrefixes(t *testing.T) {
	dm := newTestDMForWake(t)
	now := time.Now().UTC().Format(time.RFC3339)

	// Memory whose content mentions the milestone prefix but carries
	// no actual type:milestone-* tag. Must NOT appear in the listing.
	tagsTrap, _ := json.Marshal([]string{"general-knowledge", "trick"})
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, content, tags, collection, created_at, weight)
		VALUES ('trap-id', 'a meta-note that mentions type:milestone- but is not a milestone', ?, 'memories', ?, 0.5)`,
		string(tagsTrap), now,
	)
	require.NoError(t, err)

	// And a real milestone to make sure the query still works.
	seedMilestone(t, dm, "real milestone", []string{"type:milestone-shipped"}, now)

	got := dm.recentMilestones(5)
	if len(got) != 1 {
		t.Fatalf("expected 1 milestone (the trap filtered out), got %d: %+v", len(got), got)
	}
	if got[0].Content != "real milestone" {
		t.Errorf("expected 'real milestone', got %q", got[0].Content)
	}
}

// TestGatherWakeContext_IncludesRecentMilestones is the integration
// test that pins the contract at the public surface: a committed
// milestone shows up in the wake payload under `recent_milestones`.
func TestGatherWakeContext_IncludesRecentMilestones(t *testing.T) {
	dm := newTestDMForWake(t)
	now := time.Now().UTC().Format(time.RFC3339)
	seedMilestone(t, dm, "Test milestone for wake context", []string{"type:milestone-shipped"}, now)
	seedMilestone(t, dm, "Test insight for wake context", []string{"type:milestone-insight"}, now)

	data, err := dm.GatherWakeContext()
	require.NoError(t, err)
	if len(data.RecentMilestones) != 2 {
		t.Fatalf("expected 2 milestones in wake context, got %d", len(data.RecentMilestones))
	}
	// Defensive symmetry check: the envelope decision was 5+5, so
	// recent_memories is capped at 5 from the gather. Confirm the
	// budget change. If this breaks, the budget envelope drifted.
	if cap := len(data.RecentMemories); cap > 5 {
		t.Errorf("Recent Memories exceeded budget: got %d, want <=5", cap)
	}
}

// containsMS is a tiny helper that mirrors strings.Contains without
// pulling in another import. Named containsMS to avoid a symbol
// collision with `contains` in shared_write_test.go in the same package.
func containsMS(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
