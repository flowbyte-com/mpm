package internal

// Regression test for the wake topic filter drift.
//
// Before the unification, two paths produced `RecentTopics` for wake
// context:
//   1. internal/wake_context.go — `dm.recentTopicNames(5)` queried the
//      topics table directly with a structural filter.
//   2. cmd/mpm/handlers.go — `handleWake` derived topics from per-memory
//      topic_memberships with NO structural filter.
//
// The CLI path silently leaked structural topics. Live DB had 27
// memberships on `decisions` and 10 on `theories`; any recent memory
// linked to them would surface those structural topics in the wake
// output.
//
// The fix unifies both paths behind `GetRecentUserTopics(limit int)`.
// These tests pin the structural-topic exclusion contract.

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "github.com/mattn/go-sqlite3"
	"database/sql"
)

// newTestDMForWake mirrors the pattern in call_evidence_test.go: a fresh
// sqlite3 file in t.TempDir() + InitSchema. Used by wake-context tests so
// the test never touches the workspace database.
func newTestDMForWake(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "wake-test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	t.Cleanup(func() { dm.Close() })
	return dm
}

// seedTopic inserts a topic row with the given name, description, and
// tags. The IsStructural filter inspects (description empty AND tags empty),
// so this helper lets us pin both branches cleanly.
func seedTopic(t *testing.T, dm *DatabaseManager, name, description, tags string) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO topics (id, name, description, tags, is_active)
		VALUES (?, ?, ?, ?, 1)
	`, "t-"+name, name, description, tags)
	require.NoError(t, err)
}

// seedTopicAt inserts a topic with an explicit created_at so ordering
// tests don't race the schema's second-resolution default.
func seedTopicAt(t *testing.T, dm *DatabaseManager, name, description, tags, createdAt string) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO topics (id, name, description, tags, is_active, created_at)
		VALUES (?, ?, ?, ?, 1, ?)
	`, "t-"+name, name, description, tags, createdAt)
	require.NoError(t, err)
}

func TestGetRecentUserTopics_ExcludesStructuralByPattern(t *testing.T) {
	dm := newTestDMForWake(t)
	// Empty description + empty tags = structural pattern. Should be filtered
	// even though the name isn't in the allowlist — the pattern filter is
	// the primary test, the allowlist is defense-in-depth.
	seedTopic(t, dm, "auto-anchor-1", "", "[]")
	seedTopic(t, dm, "auto-anchor-2", "{}", "[]")
	// NULL tags (not the literal string "null") — a row that was inserted
	// with a NULL tags column. Verifies the IS NULL branch of the filter.
	_, err := dm.db.Exec(`
		INSERT INTO topics (id, name, description, tags, is_active)
		VALUES (?, ?, '', NULL, 1)
	`, "t-auto-anchor-3", "auto-anchor-3")
	require.NoError(t, err)

	got := dm.GetRecentUserTopics(10)
	for _, name := range got {
		require.NotContains(t, []string{"auto-anchor-1", "auto-anchor-2", "auto-anchor-3"}, name,
			"structural pattern (empty desc + empty tags) must be filtered")
	}
}

func TestGetRecentUserTopics_ExcludesKnownStructuralByName(t *testing.T) {
	dm := newTestDMForWake(t)
	// Belt-and-suspenders: even if a future operator seeds the structural
	// topics with a non-empty description, the name allowlist still filters
	// them. This is the case the heuristic would have missed.
	seedTopic(t, dm, "decisions", "All decisions", "[]")
	seedTopic(t, dm, "theories", "All theories", "[]")

	got := dm.GetRecentUserTopics(10)
	for _, name := range got {
		require.NotContains(t, []string{"decisions", "theories"}, name,
			"name allowlist must filter known structural topics")
	}
}

func TestGetRecentUserTopics_IncludesUserTopics(t *testing.T) {
	dm := newTestDMForWake(t)
	seedTopic(t, dm, "decisions", "{}", "[]")   // structural (filtered)
	seedTopic(t, dm, "theories", "{}", "[]")    // structural (filtered)
	seedTopic(t, dm, "Interface Decoupling", "How to separate concerns", "[]")
	seedTopic(t, dm, "Concurrency Patterns", "Mutexes and channels", "[]")
	seedTopic(t, dm, "Decisions Catalog", "User-created catalog of decisions", "[]")

	got := dm.GetRecentUserTopics(10)
	require.ElementsMatch(t,
		[]string{"Interface Decoupling", "Concurrency Patterns", "Decisions Catalog"},
		got,
		"user topics with real descriptions must be returned; structural anchors must not")
}

func TestGetRecentUserTopics_RespectsLimit(t *testing.T) {
	dm := newTestDMForWake(t)
	for i := 0; i < 7; i++ {
		seedTopic(t, dm, "user-topic-"+string(rune('a'+i)), "desc", "[]")
	}
	got := dm.GetRecentUserTopics(3)
	require.Len(t, got, 3, "limit must be respected")
}

func TestGetRecentUserTopics_OrderedNewestFirst(t *testing.T) {
	// topic_memberships / created_at ordering: topics are returned in
	// created_at DESC. Schema default is second-resolution, so we set
	// explicit timestamps to avoid test flakiness.
	dm := newTestDMForWake(t)
	seedTopicAt(t, dm, "user-topic-a", "first", "[]", "2026-06-24 10:00:00")
	seedTopicAt(t, dm, "user-topic-b", "second", "[]", "2026-06-24 11:00:00")
	seedTopicAt(t, dm, "user-topic-c", "third", "[]", "2026-06-24 12:00:00")
	got := dm.GetRecentUserTopics(3)
	require.Equal(t, []string{"user-topic-c", "user-topic-b", "user-topic-a"}, got,
		"newest topic must come first")
}
