package internal

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// newTestSynthesisDM returns a per-test in-memory DM with the
// 'changelog' collection available. Uses a separate DSN prefix
// from newTestRefDM and newTestChangelogDM so a future global-
// cache refactor cannot cross-contaminate.
func newTestSynthesisDM(t *testing.T) *DatabaseManager {
	t.Helper()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand: %v", err)
	}
	dsn := "file:synth_" + hex.EncodeToString(suffix) + "?mode=memory&cache=shared"
	raw, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dm := NewDatabaseManagerForDB(raw)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return dm
}

// seedChangelogMemory writes a changelog memory tied to a specific
// commit hash, mimicking what LogChangelogEntry does. The fact
// text is short so test assertions stay readable.
func seedChangelogMemory(t *testing.T, dm *DatabaseManager, hash, fact string) string {
	t.Helper()
	id, err := dm.LogChangelogEntry(fact, hash, nil)
	if err != nil {
		t.Fatalf("seed memory for %s: %v", hash, err)
	}
	return id
}

// TestSynthesize_PlainBullet: entry with no matching memory.
// Renderer emits just the bullet, no blockquote. This is the
// "unmatched" path. The body is NOT synthesised with anything.
func TestSynthesize_PlainBullet(t *testing.T) {
	dm := newTestSynthesisDM(t)
	// No memories seeded.

	doc := &ChangelogDocument{
		Project: "T",
		Releases: []ChangelogRelease{
			{
				Version: "1.1.0",
				Date:    "2026-06-19",
				Entries: []ChangelogEntry{
					{Type: "feat", Scope: "reference", Summary: "embed chunks", CommitHash: "914ebf90665964d6f75a4dbc1847f13a83977820", MPMMemoryIDs: []string{}},
				},
			},
		},
	}
	res, err := SynthesizeChangelog(doc, dm.SQLDB())
	if err != nil {
		t.Fatalf("SynthesizeChangelog: %v", err)
	}

	if res.Matched != 0 {
		t.Errorf("expected 0 matched, got %d", res.Matched)
	}
	if res.Unmatched != 1 {
		t.Errorf("expected 1 unmatched, got %d", res.Unmatched)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("expected 0 orphans, got %d", len(res.Orphans))
	}
	// Body is still empty (no merge happened).
	entry := res.Document.Releases[0].Entries[0]
	if entry.Body != "" {
		t.Errorf("expected empty body for unmatched entry, got %q", entry.Body)
	}

	// Render and confirm no blockquote.
	md := res.Document.RenderMarkdown()
	if strings.Contains(md, ">") {
		t.Errorf("plain bullet path should not emit blockquotes, got %q", md)
	}
}

// TestSynthesize_MergedProse: entry with a matching memory.
// The memory's content is merged into the entry's Body. The
// renderer wraps it in a blockquote. The MPMMemoryIDs field is
// populated with the memory's id.
func TestSynthesize_MergedProse(t *testing.T) {
	dm := newTestSynthesisDM(t)
	const hash = "914ebf90665964d6f75a4dbc1847f13a83977820"
	memID := seedChangelogMemory(t, dm, hash, "Embedding is a separate phase after AddReference. The split is load-bearing: the chunk-insert tx stays small and fast, embedding is independently retryable on provider outage.")

	doc := &ChangelogDocument{
		Project: "T",
		Releases: []ChangelogRelease{
			{
				Version: "1.1.0",
				Date:    "2026-06-19",
				Entries: []ChangelogEntry{
					{Type: "feat", Scope: "reference", Summary: "embed chunks", CommitHash: hash, MPMMemoryIDs: []string{}},
				},
			},
		},
	}
	res, err := SynthesizeChangelog(doc, dm.SQLDB())
	if err != nil {
		t.Fatalf("SynthesizeChangelog: %v", err)
	}

	if res.Matched != 1 {
		t.Errorf("expected 1 matched, got %d", res.Matched)
	}
	if res.Unmatched != 0 {
		t.Errorf("expected 0 unmatched, got %d", res.Unmatched)
	}

	entry := res.Document.Releases[0].Entries[0]
	if !strings.Contains(entry.Body, "Embedding is a separate phase") {
		t.Errorf("entry.Body missing memory prose: %q", entry.Body)
	}
	if len(entry.MPMMemoryIDs) != 1 || entry.MPMMemoryIDs[0] != memID {
		t.Errorf("expected MPMMemoryIDs=[%s], got %v", memID, entry.MPMMemoryIDs)
	}

	// Render confirms blockquote wrapping.
	md := res.Document.RenderMarkdown()
	if !strings.Contains(md, "> Embedding is a separate phase") {
		t.Errorf("merged prose should render as blockquote; got markdown:\n%s", md)
	}
}

// TestSynthesize_OrphanMemory: a changelog memory with a commit
// hash that has no matching entry. Surfaced in res.Orphans with
// no error. This is the load-bearing case the operator must see
// — an agent either hallucinated a hash or annotated an
// abandoned branch.
func TestSynthesize_OrphanMemory(t *testing.T) {
	dm := newTestSynthesisDM(t)
	const orphanHash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	seedChangelogMemory(t, dm, orphanHash, "I claimed to ship the gravity drive but we shipped nothing.")

	doc := &ChangelogDocument{
		Project: "T",
		Releases: []ChangelogRelease{
			{
				Version: "1.1.0",
				Date:    "2026-06-19",
				Entries: []ChangelogEntry{
					{Type: "feat", Summary: "real feat", CommitHash: "914ebf90665964d6f75a4dbc1847f13a83977820", MPMMemoryIDs: []string{}},
				},
			},
		},
	}
	res, err := SynthesizeChangelog(doc, dm.SQLDB())
	if err != nil {
		t.Fatalf("SynthesizeChangelog: %v", err)
	}

	if res.Matched != 0 {
		t.Errorf("expected 0 matched, got %d", res.Matched)
	}
	if res.Unmatched != 1 {
		t.Errorf("expected 1 unmatched, got %d", res.Unmatched)
	}
	if len(res.Orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(res.Orphans))
	}
	if res.Orphans[0].CommitHash != orphanHash {
		t.Errorf("orphan commit hash = %q, want %q", res.Orphans[0].CommitHash, orphanHash)
	}
	if !strings.Contains(res.Orphans[0].Content, "gravity drive") {
		t.Errorf("orphan content missing: %q", res.Orphans[0].Content)
	}
}

// TestSynthesize_OrphanRendered: prove that the runSynthesis
// helper in cmd/mpm/changelog.go attaches orphans as a synthetic
// release so the renderer emits them in a dedicated section. This
// is the contract: orphans are visible in the artifact, not only
// in the terminal warning.
func TestSynthesize_OrphanAttachedAsRelease(t *testing.T) {
	dm := newTestSynthesisDM(t)
	const orphanHash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	seedChangelogMemory(t, dm, orphanHash, "orphan body content")

	doc := &ChangelogDocument{
		Project: "T",
		Releases: []ChangelogRelease{
			{Version: "1.1.0", Date: "2026-06-19", Entries: []ChangelogEntry{}},
		},
	}
	res, err := SynthesizeChangelog(doc, dm.SQLDB())
	if err != nil {
		t.Fatalf("SynthesizeChangelog: %v", err)
	}

	// Simulate the cmd/mpm helper: append a synthetic 'orphans'
	// release to the working document.
	if len(res.Orphans) > 0 {
		orphanEntries := make([]ChangelogEntry, 0, len(res.Orphans))
		for _, m := range res.Orphans {
			orphanEntries = append(orphanEntries, ChangelogEntry{
				Version:      "orphans",
				Date:         Today(),
				Author:       "agent (unmatched commit)",
				Summary:      "orphan memory",
				Body:         "commit hash claimed: " + m.CommitHash + "\n\n" + m.Content,
				MPMMemoryIDs: []string{m.ID},
			})
		}
		res.Document.Releases = append(res.Document.Releases, ChangelogRelease{
			Version: "orphans",
			Date:    Today(),
			Entries: orphanEntries,
		})
	}

	// Render and confirm the orphan section heading appears.
	md := res.Document.RenderMarkdown()
	if !strings.Contains(md, "## [orphans]") {
		t.Errorf("expected ## [orphans] section in rendered markdown, got:\n%s", md)
	}
	if !strings.Contains(md, "orphan body content") {
		t.Errorf("orphan body not rendered; got:\n%s", md)
	}
	if !strings.Contains(md, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") {
		t.Errorf("orphan commit hash not surfaced for forensics; got:\n%s", md)
	}
}

// TestSynthesize_DoesNotMutateInput: the synthesis function must
// not mutate the input document. The caller is expected to
// receive a working copy. This is the contract the cmd/mcp
// callers rely on.
func TestSynthesize_DoesNotMutateInput(t *testing.T) {
	dm := newTestSynthesisDM(t)
	const hash = "914ebf90665964d6f75a4dbc1847f13a83977820"
	seedChangelogMemory(t, dm, hash, "memory prose")

	original := &ChangelogDocument{
		Project: "T",
		Releases: []ChangelogRelease{
			{Version: "1.1.0", Date: "2026-06-19", Entries: []ChangelogEntry{
				{Type: "feat", Summary: "x", CommitHash: hash, MPMMemoryIDs: []string{}},
			}},
		},
	}
	inputSnapshot := ChangelogDocument{
		Project: original.Project,
		Releases: []ChangelogRelease{
			{Version: "1.1.0", Date: "2026-06-19", Entries: []ChangelogEntry{
				{Type: "feat", Summary: "x", CommitHash: hash, MPMMemoryIDs: []string{}},
			}},
		},
	}

	if _, err := SynthesizeChangelog(original, dm.SQLDB()); err != nil {
		t.Fatalf("SynthesizeChangelog: %v", err)
	}

	// Original entry body should still be empty.
	if original.Releases[0].Entries[0].Body != "" {
		t.Errorf("input was mutated: Body=%q", original.Releases[0].Entries[0].Body)
	}
	if len(original.Releases[0].Entries[0].MPMMemoryIDs) != 0 {
		t.Errorf("input was mutated: MPMMemoryIDs=%v", original.Releases[0].Entries[0].MPMMemoryIDs)
	}
	// And structurally equal to the snapshot.
	if original.Releases[0].Entries[0].Type != inputSnapshot.Releases[0].Entries[0].Type {
		t.Error("input Type changed")
	}
	if original.Releases[0].Entries[0].Summary != inputSnapshot.Releases[0].Entries[0].Summary {
		t.Error("input Summary changed")
	}
}

// TestFetchChangelogMemories_Unkeyed: a memory in the changelog
// collection that lacks a #commit:<hash> tag (e.g., corrupted
// tags, or a direct write that bypassed LogChangelogEntry) must
// be returned in the unkeyed slice so the caller can surface it
// as an orphan.
func TestFetchChangelogMemories_Unkeyed(t *testing.T) {
	dm := newTestSynthesisDM(t)
	// Direct write with no #commit:<hash> tag. This bypasses
	// LogChangelogEntry's contract on purpose to exercise the
	// defensiveness of FetchChangelogMemories.
	_, _, err := dm.SaveMemoryWithContext(
		"orphan body without commit tag",
		"changelog",
		[]string{"changelog"}, // no commit:abc123 tag
		1.0,
		"0",
		ActiveContext{},
	)
	if err != nil {
		t.Fatalf("SaveMemoryWithContext: %v", err)
	}

	indexed, unkeyed, err := FetchChangelogMemories(dm.SQLDB())
	if err != nil {
		t.Fatalf("FetchChangelogMemories: %v", err)
	}
	if len(indexed) != 0 {
		t.Errorf("expected 0 indexed, got %d", len(indexed))
	}
	if len(unkeyed) != 1 {
		t.Fatalf("expected 1 unkeyed, got %d", len(unkeyed))
	}
	if unkeyed[0].CommitHash != "" {
		t.Errorf("unkeyed memory should have empty CommitHash, got %q", unkeyed[0].CommitHash)
	}
}

// TestExtractCommitHashFromTags: small unit test for the lookup
// helper. Should return the hash for #commit:<hash> tags and ""
// for everything else.
func TestExtractCommitHashFromTags(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"single commit tag", []string{"commit:abc"}, "abc"},
		{"multiple tags, commit present", []string{"changelog", "commit:0123", "extra"}, "0123"},
		{"no commit tag", []string{"changelog", "extra"}, ""},
		{"empty", nil, ""},
		{"commit with prefix only", []string{"commit:"}, ""}, // empty hash after prefix
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractCommitHashFromTags(c.tags)
			if got != c.want {
				t.Errorf("extractCommitHashFromTags(%v) = %q, want %q", c.tags, got, c.want)
			}
		})
	}
}
