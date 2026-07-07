package internal

import (
	"strings"
	"testing"
)

// newTestChangelogDM returns a per-test in-memory DM. The DSN strategy
// (unique shared-cache name per test) is now centralized in
// internal/testhelpers.go::NewTestDM, so cross-contamination between
// reference / changelog / synthesis test families cannot happen —
// each call gets a unique counter-derived name.
func newTestChangelogDM(t *testing.T) *DatabaseManager {
	t.Helper()
	return NewTestDM(t)
}

func TestLogChangelogEntry_HappyPath(t *testing.T) {
	dm := newTestChangelogDM(t)

	id, err := dm.LogChangelogEntry(
		"This commit shipped the unified write surface.",
		"abc1234567890abcdef1234567890abcdef12345",
		[]string{"reference", "architecture"},
	)
	if err != nil {
		t.Fatalf("LogChangelogEntry: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty memory id")
	}

	// The memory should be retrievable by tag. The synthesis engine
	// will use this lookup.
	row := dm.SQLDB().QueryRow(
		`SELECT content, tags, collection FROM memories WHERE id = ?`, id)
	var content, tags, collection string
	if err := row.Scan(&content, &tags, &collection); err != nil {
		t.Fatalf("scan memory: %v", err)
	}

	if collection != "changelog" {
		t.Errorf("collection = %q, want %q", collection, "changelog")
	}
	if !strings.Contains(content, "abc1234567890abcdef1234567890abcdef12345") {
		t.Errorf("content missing commit hash header: %q", content)
	}
	if !strings.Contains(content, "This commit shipped the unified write surface.") {
		t.Errorf("content missing fact body: %q", content)
	}
	// Tags must include #changelog and #commit:<hash>.
	if !strings.Contains(tags, ChangelogTag) {
		t.Errorf("tags missing %q: %q", ChangelogTag, tags)
	}
	if !strings.Contains(tags, CommitTagPrefix+"abc1234567890abcdef1234567890abcdef12345") {
		t.Errorf("tags missing commit tag: %q", tags)
	}
	if !strings.Contains(tags, "reference") {
		t.Errorf("tags missing caller's extra tag: %q", tags)
	}
}

// TestLogChangelogEntry_RejectsEmptyCommit: the strict retrospective
// contract. A call without a commit_hash must be refused so the
// synthesis engine's join key is never orphaned.
func TestLogChangelogEntry_RejectsEmptyCommit(t *testing.T) {
	dm := newTestChangelogDM(t)
	_, err := dm.LogChangelogEntry("orphan entry", "", nil)
	if err == nil {
		t.Fatal("expected error for empty commit_hash, got nil")
	}
	if !strings.Contains(err.Error(), "commit_hash is required") {
		t.Errorf("error should explain why commit_hash is required, got: %v", err)
	}
}

// TestLogChangelogEntry_RejectsMalformedCommit: a short hash or
// non-hex string would create an ambiguous join. The synthesis
// engine could not tell which commit the memory references. Reject
// at write time.
func TestLogChangelogEntry_RejectsMalformedCommit(t *testing.T) {
	dm := newTestChangelogDM(t)

	cases := []string{
		"abc1234",                  // too short
		"abc1234567890abcdef1234567890abcdef1234z", // non-hex char
		"abc1234567890abcdef1234567890abcdef1234567", // too long
		"not-a-hash",               // not even hex
		"main",                     // ref name, not a hash
	}
	for _, c := range cases {
		_, err := dm.LogChangelogEntry("body", c, nil)
		if err == nil {
			t.Errorf("expected error for commit_hash=%q, got nil", c)
		}
	}
}

// TestLogChangelogEntry_RejectsEmptyFact: an empty fact is a
// silent no-op. Reject it so the memory table never carries rows
// with no human-meaningful content.
func TestLogChangelogEntry_RejectsEmptyFact(t *testing.T) {
	dm := newTestChangelogDM(t)
	_, err := dm.LogChangelogEntry("", "abc1234567890abcdef1234567890abcdef12345", nil)
	if err == nil {
		t.Fatal("expected error for empty fact, got nil")
	}
}

// TestLogChangelogEntry_DedupesChangelogTag: if the caller already
// added "changelog" as a tag, we must not duplicate it. Duplicates
// would bloat the tag index and could confuse the synthesis
// engine's lookup-by-tag query.
func TestLogChangelogEntry_DedupesChangelogTag(t *testing.T) {
	dm := newTestChangelogDM(t)

	id, err := dm.LogChangelogEntry(
		"body",
		"abc1234567890abcdef1234567890abcdef12345",
		[]string{"CHANGELOG", "reference"}, // mixed case + duplicate
	)
	if err != nil {
		t.Fatalf("LogChangelogEntry: %v", err)
	}

	row := dm.SQLDB().QueryRow(`SELECT tags FROM memories WHERE id = ?`, id)
	var tags string
	if err := row.Scan(&tags); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Count occurrences of "changelog" (case-insensitive) — should be 1.
	lower := strings.ToLower(tags)
	if strings.Count(lower, "changelog") != 1 {
		t.Errorf("expected exactly 1 occurrence of 'changelog' tag, got %d in %q", strings.Count(lower, "changelog"), tags)
	}
}

// TestLogChangelogEntry_NilDM: defensive — a nil receiver must
// return an error, not panic. The MCP handler can then surface a
// meaningful message to the agent.
func TestLogChangelogEntry_NilDM(t *testing.T) {
	var dm *DatabaseManager
	_, err := dm.LogChangelogEntry("body", "abc1234567890abcdef1234567890abcdef12345", nil)
	if err == nil {
		t.Fatal("expected error for nil DatabaseManager, got nil")
	}
}

// TestLogChangelogEntry_JoinKeyShape: a load-bearing assertion
// that the tags generated by LogChangelogEntry are exactly the
// shape the future synthesis engine will look up. If this test
// fails, the synthesis engine's contract is broken.
func TestLogChangelogEntry_JoinKeyShape(t *testing.T) {
	dm := newTestChangelogDM(t)

	const hash = "0123456789abcdef0123456789abcdef01234567"
	id, err := dm.LogChangelogEntry("body", hash, nil)
	if err != nil {
		t.Fatalf("LogChangelogEntry: %v", err)
	}

	row := dm.SQLDB().QueryRow(`SELECT tags FROM memories WHERE id = ?`, id)
	var tags string
	if err := row.Scan(&tags); err != nil {
		t.Fatalf("scan: %v", err)
	}

	wantTag := CommitTagPrefix + hash // "commit:0123..." (lowercase per spec)
	if !strings.Contains(tags, wantTag) {
		t.Errorf("expected join tag %q in %q", wantTag, tags)
	}
	// And the #changelog tag, which is the synthesis engine's filter.
	if !strings.Contains(tags, ChangelogTag) {
		t.Errorf("expected #changelog tag in %q", tags)
	}
}

// TestLogChangelogEntry_SynthesisEngineLookup: end-to-end smoke
// test. Write three changelog memories and one unrelated memory,
// then run the same query the future synthesis engine will run.
// If the lookup returns the right rows, the contract is wired.
func TestLogChangelogEntry_SynthesisEngineLookup(t *testing.T) {
	dm := newTestChangelogDM(t)

	hashes := []string{
		"0123456789abcdef0123456789abcdef01234567",
		"abcdef0123456789abcdef0123456789abcdef01",
		"fedcba9876543210fedcba9876543210fedcba98",
	}
	for _, h := range hashes {
		if _, err := dm.LogChangelogEntry("body for "+h[:7], h, []string{"smoke"}); err != nil {
			t.Fatalf("LogChangelogEntry: %v", err)
		}
	}
	// One unrelated memory that should NOT be returned.
	_, _, err := dm.SaveMemoryWithContext(
		"unrelated memory",
		"memories",
		[]string{"not-changelog"},
		0.5,
		"24h",
		ActiveContext{},
	)
	if err != nil {
		t.Fatalf("SaveMemoryWithContext: %v", err)
	}

	// This is the synthesis engine's lookup: every memory in the
	// changelog collection. Tag-based filters could refine further
	// (e.g., only those with commit:<hash> in tags) but the
	// collection is the primary scope.
	rows, err := dm.SQLDB().Query(
		`SELECT tags FROM memories WHERE collection = 'changelog' ORDER BY created_at`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var tags string
		if err := rows.Scan(&tags); err != nil {
			t.Fatalf("scan: %v", err)
		}
		count++
		if !strings.Contains(tags, ChangelogTag) {
			t.Errorf("row %d missing #changelog tag: %q", count, tags)
		}
	}
	if count != 3 {
		t.Errorf("expected 3 changelog memories, got %d", count)
	}
}
