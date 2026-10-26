// reference_batch_test.go — manifest ingestion.
//
// The corpus this sprint had to import was 27 documents. Doing that one
// `reference add` at a time meant the agent had to reconcile the results
// by hand, and a failure halfway through looked the same as a page that
// had already been imported. These tests pin the batch contract:
//
//   - per-document status, so a failure is attributable;
//   - one failure does not stop the rest;
//   - the batch is NOT reported as successful when anything failed or was
//     ambiguous;
//   - re-running the same manifest is idempotent and does not re-embed.
package internal

import (
	"context"
	"path/filepath"
	"testing"
)

const restAuthMarkdown = `# Authentication

## Cookie authentication

Cookie authentication is the standard method included with WordPress.

## Application passwords

Application passwords are a separate credential for the REST API.
`

// TestIngestReferenceManifest_MixedOutcomes pins per-document status for a
// corpus with a duplicate entry, a changed entry and a failing entry.
func TestIngestReferenceManifest_MixedOutcomes(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	menus := writeTestDoc(t, filepath.Join(corpus, "docs"), "menus.md", menusMarkdown)
	auth := writeTestDoc(t, filepath.Join(corpus, "docs"), "auth.md", restAuthMarkdown)

	manifest := &ReferenceManifest{Documents: []ReferenceManifestEntry{
		{Path: menus, Tags: []string{"wordpress"}, Reason: "AgentShell development"},
		{Path: auth, Tags: []string{"wordpress", "rest"}},
		// Same file as the first entry, different spelling.
		{Path: filepath.Join(corpus, "docs", "..", "docs", "menus.md")},
		// Missing file.
		{Path: filepath.Join(corpus, "docs", "does-not-exist.md")},
		// A URL that must be refused by the SSRF boundary in production
		// configuration (the batch path uses no test seam).
		{URL: "http://169.254.169.254/latest/meta-data/"},
	}}

	res, err := dm.IngestReferenceManifest(context.Background(), manifest)
	if err != nil {
		t.Fatalf("batch ingest: %v", err)
	}
	if res.Total != 5 {
		t.Fatalf("total = %d, want 5", res.Total)
	}
	if res.Imported != 2 {
		t.Errorf("imported = %d, want 2", res.Imported)
	}
	if res.Unchanged != 1 {
		t.Errorf("unchanged = %d, want 1 (the duplicate spelling)", res.Unchanged)
	}
	if res.Failed != 2 {
		t.Errorf("failed = %d, want 2 (missing file + refused URL)", res.Failed)
	}
	if res.OK {
		t.Error("batch reported success despite 2 failures")
	}

	// Statuses are per-document and in manifest order.
	want := []string{"imported", "imported", "unchanged", "failed", "failed"}
	for i, w := range want {
		if res.Documents[i].Status != w {
			t.Errorf("documents[%d].status = %q, want %q (%s)", i, res.Documents[i].Status, w, res.Documents[i].Error)
		}
	}
	if res.Documents[3].Error == "" {
		t.Error("failed entry carried no error message")
	}
	if res.Documents[0].Result == nil || res.Documents[0].Result.ID == "" {
		t.Error("imported entry carried no document id")
	}

	// Only the two distinct files exist.
	if n := referenceDocCount(t, dm); n != 2 {
		t.Errorf("stored %d documents, want 2", n)
	}
}

// TestIngestReferenceManifest_RetryIsIdempotent pins the resume path: the
// second run of the same manifest writes nothing.
func TestIngestReferenceManifest_RetryIsIdempotent(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	menus := writeTestDoc(t, corpus, "menus.md", menusMarkdown)
	manifest := &ReferenceManifest{Documents: []ReferenceManifestEntry{
		{Path: menus, Tags: []string{"docs"}},
	}}

	first, err := dm.IngestReferenceManifest(context.Background(), manifest)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if first.Imported != 1 || !first.OK {
		t.Fatalf("first batch = %+v", first)
	}
	chunksBefore := readReferenceChunkRows(t, dm, first.Documents[0].Result.ID)

	second, err := dm.IngestReferenceManifest(context.Background(), manifest)
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if !second.OK {
		t.Errorf("idempotent retry reported not-ok: %+v", second)
	}
	if second.Unchanged != 1 || second.Imported != 0 {
		t.Errorf("retry statuses = imported %d / unchanged %d, want 0/1", second.Imported, second.Unchanged)
	}
	if second.Documents[0].Result.ChunksAdded != 0 {
		t.Errorf("retry added %d chunks; want 0", second.Documents[0].Result.ChunksAdded)
	}
	chunksAfter := readReferenceChunkRows(t, dm, first.Documents[0].Result.ID)
	if len(chunksAfter) != len(chunksBefore) {
		t.Errorf("chunk rows changed on idempotent retry: %d → %d", len(chunksBefore), len(chunksAfter))
	}
}

// TestIngestReferenceManifest_ChangedContentUpdatesInPlace pins that a
// re-run after editing one document updates it rather than adding one.
func TestIngestReferenceManifest_ChangedContentUpdatesInPlace(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, corpus, "auth.md", restAuthMarkdown)
	manifest := &ReferenceManifest{Documents: []ReferenceManifestEntry{{Path: path}}}

	first, err := dm.IngestReferenceManifest(context.Background(), manifest)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	id := first.Documents[0].Result.ID

	writeTestDoc(t, corpus, "auth.md", restAuthMarkdown+"\n## JWT\n\nA third-party JWT plugin can authenticate requests.\n")
	second, err := dm.IngestReferenceManifest(context.Background(), manifest)
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if second.Updated != 1 {
		t.Errorf("updated = %d, want 1: %+v", second.Updated, second.Documents[0])
	}
	if second.Documents[0].Result.ID != id {
		t.Errorf("document id changed on update: %s → %s", id, second.Documents[0].Result.ID)
	}
	if n := referenceDocCount(t, dm); n != 1 {
		t.Errorf("stored %d documents after update, want 1", n)
	}
}

// TestParseReferenceManifest_ValidationIsUpFront pins that a malformed
// entry is rejected before anything is fetched or written.
func TestParseReferenceManifest_ValidationIsUpFront(t *testing.T) {
	cases := map[string]string{
		"empty":          ``,
		"no documents":   `{"documents": []}`,
		"not json":       `documents: nope`,
		"both sources":   `{"documents":[{"path":"a.md","url":"https://example.org"}]}`,
		"no source":      `{"documents":[{"tags":["x"]}]}`,
		"bad chunk size": `{"documents":[{"path":"a.md","chunk_size":10}]}`,
	}
	for name, body := range cases {
		if _, err := ParseReferenceManifest([]byte(body)); err == nil {
			t.Errorf("%s: manifest was accepted", name)
		}
	}

	// A bare array is accepted: it is the obvious first guess.
	m, err := ParseReferenceManifest([]byte(`[{"path":"a.md"},{"url":"https://example.org/x"}]`))
	if err != nil {
		t.Fatalf("bare array rejected: %v", err)
	}
	if len(m.Documents) != 2 {
		t.Errorf("bare array produced %d documents, want 2", len(m.Documents))
	}
}
