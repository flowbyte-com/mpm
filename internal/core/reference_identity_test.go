// reference_identity_test.go — pins the source-identity and idempotency
// contract for reference ingestion.
//
// The failure this file exists to prevent, reproduced from a real
// documentation bootstrap: one Markdown file imported four times through
// four spellings of its own path (bare name from inside the directory,
// `docs/…` from the parent, `./docs/../docs/…`, and the absolute path)
// became four reference documents with four chunk sets and four
// embedding passes. Retrieval then competed against its own copies and
// "27 documents" was really 27 documents plus their accidental aliases.
package internal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeTestDoc creates a file under dir and returns its path.
func writeTestDoc(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const menusMarkdown = `# WordPress Menus

Navigation menus are registered by the theme and rendered in a template.

## wp_nav_menu()

Renders a navigation menu for a theme location.

` + "```php" + `
wp_nav_menu( array( 'theme_location' => 'primary' ) );
` + "```" + `

## register_nav_menus()

Registers the locations a theme offers to the menu administration screen.
`

// referenceDocCount returns the number of stored documents.
func referenceDocCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM reference_docs`).Scan(&n); err != nil {
		t.Fatalf("count reference_docs: %v", err)
	}
	return n
}

// TestReferenceIngest_SameFileDifferentSpellingsIsOneDocument is the
// primary regression: every equivalent spelling of one path must resolve
// to one document.
func TestReferenceIngest_SameFileDifferentSpellingsIsOneDocument(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	writeTestDoc(t, filepath.Join(corpus, "docs"), "wordpress-menus.md", menusMarkdown)

	absolute := filepath.Join(corpus, "docs", "wordpress-menus.md")

	// 1. Bare name, from inside the directory.
	t.Chdir(filepath.Join(corpus, "docs"))
	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: "wordpress-menus.md"})
	if err != nil {
		t.Fatalf("ingest bare name: %v", err)
	}
	if res.Status != ReferenceStatusImported {
		t.Fatalf("first ingest status = %q, want %q", res.Status, ReferenceStatusImported)
	}
	firstID := res.ID

	// 2. Relative path with a directory prefix, from the parent.
	t.Chdir(corpus)
	res, err = dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: "docs/wordpress-menus.md"})
	if err != nil {
		t.Fatalf("ingest relative path: %v", err)
	}
	if res.Status != ReferenceStatusUnchanged {
		t.Errorf("relative-path re-import status = %q, want %q (same file, same bytes)", res.Status, ReferenceStatusUnchanged)
	}
	if res.ID != firstID {
		t.Errorf("relative-path re-import id = %q, want %q", res.ID, firstID)
	}

	// 3. `..`-containing path.
	res, err = dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: "./docs/../docs/wordpress-menus.md"})
	if err != nil {
		t.Fatalf("ingest dot-dot path: %v", err)
	}
	if res.Status != ReferenceStatusUnchanged || res.ID != firstID {
		t.Errorf("dot-dot re-import = (%q, %s), want (%q, %s)", res.Status, res.ID, ReferenceStatusUnchanged, firstID)
	}

	// 4. Absolute path.
	res, err = dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: absolute})
	if err != nil {
		t.Fatalf("ingest absolute path: %v", err)
	}
	if res.Status != ReferenceStatusUnchanged || res.ID != firstID {
		t.Errorf("absolute re-import = (%q, %s), want (%q, %s)", res.Status, res.ID, ReferenceStatusUnchanged, firstID)
	}

	if n := referenceDocCount(t, dm); n != 1 {
		t.Errorf("stored %d documents for one file; want 1", n)
	}
	if res.Source != absolute {
		t.Errorf("canonical source = %q, want the absolute path %q", res.Source, absolute)
	}
}

// TestReferenceIngest_SymlinkAliasIsOneDocument pins the documented
// symlink behaviour: a symlink is an alias of its target, not a second
// document.
func TestReferenceIngest_SymlinkAliasIsOneDocument(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	target := writeTestDoc(t, filepath.Join(corpus, "real"), "guide.md", menusMarkdown)
	linkDir := filepath.Join(corpus, "link")
	if err := os.Symlink(filepath.Join(corpus, "real"), linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	first, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: target})
	if err != nil {
		t.Fatalf("ingest target: %v", err)
	}
	viaLink, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: filepath.Join(linkDir, "guide.md")})
	if err != nil {
		t.Fatalf("ingest via symlink: %v", err)
	}
	if viaLink.ID != first.ID {
		t.Errorf("symlink alias produced document %s; want the existing %s", viaLink.ID, first.ID)
	}
	if n := referenceDocCount(t, dm); n != 1 {
		t.Errorf("stored %d documents for a file and its symlink; want 1", n)
	}
}

// TestReferenceIngest_UnchangedReimportPreservesChunkRows pins the
// no-op path: same bytes mean no chunk writes, no id churn, no
// re-embedding.
func TestReferenceIngest_UnchangedReimportPreservesChunkRows(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, corpus, "menus.md", menusMarkdown)

	first, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	before := readReferenceChunkRows(t, dm, first.ID)
	if len(before) == 0 {
		t.Fatal("first ingest stored no chunks")
	}

	second, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.Status != ReferenceStatusUnchanged {
		t.Fatalf("status = %q, want %q", second.Status, ReferenceStatusUnchanged)
	}
	if second.ChunksAdded != 0 || second.ChunksRemoved != 0 {
		t.Errorf("unchanged re-import reported %d added / %d removed chunks; want 0/0",
			second.ChunksAdded, second.ChunksRemoved)
	}
	after := readReferenceChunkRows(t, dm, first.ID)
	if len(after) != len(before) {
		t.Fatalf("chunk count changed on unchanged re-import: %d → %d", len(before), len(after))
	}
	for id, hash := range before {
		if after[id] != hash {
			t.Errorf("chunk %s content hash changed on unchanged re-import", id)
		}
	}
}

// TestReferenceIngest_ChangedContentReusesUnchangedChunks pins the diff:
// editing one section updates the document in place, reuses the chunks
// that did not change, and leaves no orphan rows.
func TestReferenceIngest_ChangedContentReusesUnchangedChunks(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, corpus, "menus.md", menusMarkdown)

	first, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	before := readReferenceChunkRows(t, dm, first.ID)

	edited := strings.Replace(menusMarkdown,
		"Registers the locations a theme offers to the menu administration screen.",
		"Registers the locations a theme offers, and must be called in after_setup_theme.",
		1)
	writeTestDoc(t, corpus, "menus.md", edited)

	second, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("re-ingest edited file: %v", err)
	}
	if second.Status != ReferenceStatusUpdated {
		t.Fatalf("status = %q, want %q", second.Status, ReferenceStatusUpdated)
	}
	if second.ID != first.ID {
		t.Errorf("document id changed on update: %s → %s", first.ID, second.ID)
	}
	if second.ChunksReused == 0 {
		t.Errorf("no chunks were reused after a single-section edit (reused=%d added=%d removed=%d)",
			second.ChunksReused, second.ChunksAdded, second.ChunksRemoved)
	}
	after := readReferenceChunkRows(t, dm, first.ID)
	for id := range after {
		if _, ok := before[id]; ok {
			continue
		}
		// A new chunk id is legitimate only if it is one of the chunks
		// this call reported as added.
		if second.ChunksAdded == 0 {
			t.Errorf("chunk %s appeared without being reported as added", id)
		}
	}
	// No orphans: every chunk row belongs to the current document and the
	// stored total matches the rows.
	var storedTotal, rowCount int
	if err := dm.SQLDB().QueryRow(`SELECT total_chunks FROM reference_docs WHERE id = ?`, first.ID).Scan(&storedTotal); err != nil {
		t.Fatalf("read total_chunks: %v", err)
	}
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM reference_chunks WHERE doc_id = ?`, first.ID).Scan(&rowCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if storedTotal != rowCount {
		t.Errorf("total_chunks = %d but %d chunk rows exist (orphans or a lost write)", storedTotal, rowCount)
	}
}

// TestReferenceIngest_IdenticalContentDifferentSourcesStaySeparate pins
// the rule that content hashes are NOT identity. Two publishers may
// legitimately ship the same words; merging them would conflate their
// provenance.
func TestReferenceIngest_IdenticalContentDifferentSourcesStaySeparate(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	a := writeTestDoc(t, filepath.Join(corpus, "vendor-a"), "reference.md", menusMarkdown)
	b := writeTestDoc(t, filepath.Join(corpus, "vendor-b"), "reference.md", menusMarkdown)

	resA, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: a})
	if err != nil {
		t.Fatalf("ingest a: %v", err)
	}
	resB, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: b})
	if err != nil {
		t.Fatalf("ingest b: %v", err)
	}
	if resA.ID == resB.ID {
		t.Fatalf("two different sources with identical bytes collapsed into one document (%s)", resA.ID)
	}
	if resB.Status != ReferenceStatusImported {
		t.Errorf("second distinct source status = %q, want %q", resB.Status, ReferenceStatusImported)
	}
	if n := referenceDocCount(t, dm); n != 2 {
		t.Errorf("stored %d documents; want 2 distinct sources", n)
	}
}

// TestReferenceIngest_LegacyRelativePathIsRecognised covers a row written
// by an older binary, which stored the caller's string verbatim.
func TestReferenceIngest_LegacyRelativePathIsRecognised(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, filepath.Join(corpus, "docs"), "wordpress-menus.md", menusMarkdown)

	// Simulate the legacy row: relative path, old random id, same content.
	legacyID := "legacy00000000ab"
	insertLegacyReferenceDoc(t, dm, legacyID, "docs/wordpress-menus.md", menusMarkdown)

	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("ingest over legacy row: %v", err)
	}
	if res.ID != legacyID {
		t.Errorf("ingest created a new document %s instead of recognising legacy %s", res.ID, legacyID)
	}
	if n := referenceDocCount(t, dm); n != 1 {
		t.Errorf("stored %d documents; want the single legacy row", n)
	}
	// The legacy identity is upgraded in place so the next import takes
	// the exact-match path.
	var stored string
	if err := dm.SQLDB().QueryRow(`SELECT file_path FROM reference_docs WHERE id = ?`, legacyID).Scan(&stored); err != nil {
		t.Fatalf("read file_path: %v", err)
	}
	if stored != path {
		t.Errorf("legacy file_path = %q, want the canonical %q", stored, path)
	}
}

// TestReferenceIngest_AmbiguousLegacyDuplicatesAreRefused pins the
// no-write path: when several existing rows already resolve to the same
// identity, importing again must report the duplicates instead of
// creating another one.
func TestReferenceIngest_AmbiguousLegacyDuplicatesAreRefused(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, filepath.Join(corpus, "docs"), "wordpress-menus.md", menusMarkdown)

	insertLegacyReferenceDoc(t, dm, "legacy0000000001", "wordpress-menus.md", menusMarkdown)
	insertLegacyReferenceDoc(t, dm, "legacy0000000002", "docs/wordpress-menus.md", menusMarkdown)

	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("ingest with ambiguous duplicates: %v", err)
	}
	if res.Status != ReferenceStatusDuplicate {
		t.Fatalf("status = %q, want %q", res.Status, ReferenceStatusDuplicate)
	}
	if len(res.DuplicateIDs) != 2 {
		t.Errorf("reported %d duplicate ids, want 2: %v", len(res.DuplicateIDs), res.DuplicateIDs)
	}
	if n := referenceDocCount(t, dm); n != 2 {
		t.Errorf("stored %d documents; want the 2 pre-existing rows and no new one", n)
	}
	if len(res.Warnings) == 0 {
		t.Error("duplicate outcome carried no warning explaining the repair path")
	}
}

// TestFindReferenceDuplicates_ReportsGroupsReadOnly pins the diagnostic:
// it groups the historical duplicates, marks a survivor and changes
// nothing.
func TestFindReferenceDuplicates_ReportsGroupsReadOnly(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	writeTestDoc(t, filepath.Join(corpus, "docs"), "menus.md", menusMarkdown)

	insertLegacyReferenceDoc(t, dm, "dupe000000000a1", "menus.md", menusMarkdown)
	insertLegacyReferenceDoc(t, dm, "dupe000000000a2", "docs/menus.md", menusMarkdown)
	insertLegacyReferenceDoc(t, dm, "solo00000000a03", filepath.Join(corpus, "other.md"), "# Other\n\nDifferent document.\n")

	groups, err := FindReferenceDuplicates(dm.SQLDB())
	if err != nil {
		t.Fatalf("FindReferenceDuplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("found %d duplicate groups, want 1: %+v", len(groups), groups)
	}
	if len(groups[0].Docs) != 2 {
		t.Errorf("group has %d members, want 2", len(groups[0].Docs))
	}
	preferred := 0
	for _, d := range groups[0].Docs {
		if d.Preferred {
			preferred++
		}
	}
	if preferred != 1 {
		t.Errorf("%d members marked preferred; want exactly 1", preferred)
	}
	if n := referenceDocCount(t, dm); n != 3 {
		t.Errorf("diagnostic changed the library: %d documents, want 3", n)
	}
}

// TestReferenceIngest_ConcurrentSameSourceIsOneDocument pins the
// concurrency story: the document id is derived from the canonical
// identity, so two ingests of one new file converge on the same row.
func TestReferenceIngest_ConcurrentSameSourceIsOneDocument(t *testing.T) {
	// File-backed, not the shared-cache in-memory fixture: concurrent
	// writers need WAL + busy_timeout, which is what production uses.
	workspace := t.TempDir()
	dm, err := NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	corpus := t.TempDir()
	path := writeTestDoc(t, corpus, "menus.md", menusMarkdown)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ingest failed: %v", err)
	}
	if n := referenceDocCount(t, dm); n != 1 {
		t.Errorf("concurrent imports of one file stored %d documents; want 1", n)
	}
	var chunkRows int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM reference_chunks`).Scan(&chunkRows); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	var total int
	if err := dm.SQLDB().QueryRow(`SELECT COALESCE(SUM(total_chunks),0) FROM reference_docs`).Scan(&total); err != nil {
		t.Fatalf("sum total_chunks: %v", err)
	}
	if chunkRows != total {
		t.Errorf("%d chunk rows but total_chunks sums to %d (duplicate chunk sets)", chunkRows, total)
	}
}

// TestReferenceIngest_MutuallyExclusiveSources pins that a request naming
// two different sources is refused instead of guessed.
func TestReferenceIngest_MutuallyExclusiveSources(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, corpus, "menus.md", menusMarkdown)

	if _, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{
		FilePath: path,
		URL:      "https://example.org/docs",
	}); err == nil {
		t.Fatal("a path plus a URL was accepted; want a conflict error")
	}
	if _, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{}); err == nil {
		t.Fatal("an empty request was accepted; want a source-required error")
	}
	// A URL passed as a bare source is classified as a URL, not a path.
	if IsURLSource("https://example.org/x") != true {
		t.Error("https source not recognised as a URL")
	}
	if IsURLSource("http:notes.md") != false {
		t.Error("a local file named http:notes.md was treated as a URL")
	}
}

// TestChunkReferenceDocument_SectionsAndChunkIDs pins that Markdown
// chunks carry the heading they live under and that chunk ids stay
// deterministic.
func TestChunkReferenceDocument_SectionsAndChunkIDs(t *testing.T) {
	chunks, err := chunkReferenceDocument(menusMarkdown, "markdown", 512)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks; want one per section at least", len(chunks))
	}
	var sections []string
	for _, c := range chunks {
		sections = append(sections, c.Section)
		if c.Index < 0 {
			t.Errorf("negative chunk index")
		}
	}
	joined := strings.Join(sections, "|")
	for _, want := range []string{"WordPress Menus", "wp_nav_menu()", "register_nav_menus()"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no chunk carried section %q; got %q", want, joined)
		}
	}
	first, _ := chunkReferenceDocument(menusMarkdown, "markdown", 512)
	second, _ := chunkReferenceDocument(menusMarkdown, "markdown", 512)
	if len(first) != len(second) {
		t.Fatalf("chunking is not deterministic: %d vs %d chunks", len(first), len(second))
	}
	for i := range first {
		if first[i].Content != second[i].Content || first[i].Section != second[i].Section {
			t.Fatalf("chunk %d differs between identical runs", i)
		}
	}
}

// TestReferenceDocIDForIdentity_IsStable pins the deterministic id that
// the concurrency guarantee rests on.
func TestReferenceDocIDForIdentity_IsStable(t *testing.T) {
	a := SourceIdentity{Kind: ReferenceSourceFile, Canonical: "/corpus/docs/x.md"}
	b := SourceIdentity{Kind: ReferenceSourceFile, Canonical: "/corpus/docs/x.md"}
	c := SourceIdentity{Kind: ReferenceSourceFile, Canonical: "/corpus/docs/y.md"}
	d := SourceIdentity{Kind: ReferenceSourceURL, Canonical: "/corpus/docs/x.md"}

	if ReferenceDocIDForIdentity(a) != ReferenceDocIDForIdentity(b) {
		t.Fatal("same identity produced different ids")
	}
	if ReferenceDocIDForIdentity(a) == ReferenceDocIDForIdentity(c) {
		t.Fatal("different identities produced the same id")
	}
	if ReferenceDocIDForIdentity(a) == ReferenceDocIDForIdentity(d) {
		t.Fatal("a file and a URL with the same string produced the same id")
	}
	if got := len(ReferenceDocIDForIdentity(a)); got != 16 {
		t.Errorf("id length = %d, want 16 (matching GenerateID)", got)
	}
}

// readReferenceChunkRows returns chunk id → content hash for a doc.
func readReferenceChunkRows(t *testing.T, dm *DatabaseManager, docID string) map[string]string {
	t.Helper()
	rows, err := dm.SQLDB().Query(`SELECT id, COALESCE(content_hash,'') FROM reference_chunks WHERE doc_id = ?`, docID)
	if err != nil {
		t.Fatalf("read chunk rows: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			t.Fatalf("scan chunk row: %v", err)
		}
		out[id] = hash
	}
	return out
}

// insertLegacyReferenceDoc writes a reference row the way a pre-identity
// binary did: caller-supplied path verbatim, random id, no source_kind.
func insertLegacyReferenceDoc(t *testing.T, dm *DatabaseManager, id, sourcePath, content string) {
	t.Helper()
	hash := HashContent(content)
	chunks, err := ChunkByTokens(content, 512)
	if err != nil {
		t.Fatalf("chunk legacy doc: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO reference_docs
		(id, title, file_path, source_type, tags, content, content_hash,
		 import_reason, total_chunks, last_indexed, created_at)
		VALUES (?, ?, ?, 'markdown', '[]', ?, ?, '', ?, '0', '0')`,
		id, strings.TrimSuffix(filepath.Base(sourcePath), ".md"), sourcePath, content, hash, len(chunks),
	); err != nil {
		t.Fatalf("insert legacy doc: %v", err)
	}
	for i, c := range chunks {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO reference_chunks (id, doc_id, chunk_index, section, content, source_path, content_hash)
			VALUES (?, ?, ?, '', ?, ?, ?)`,
			fmt.Sprintf("%s-c%d", id, i), id, i, c.Content, sourcePath, ComputeChunkHash(c.Content),
		); err != nil {
			t.Fatalf("insert legacy chunk: %v", err)
		}
	}
}
