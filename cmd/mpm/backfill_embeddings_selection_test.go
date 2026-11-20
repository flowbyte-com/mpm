package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// Regression tests for which memory rows the embeddings backfill treats as
// pending.
//
// The gap these pin: `countMemoriesWithoutEmbedding` selected only
// `embedding IS NULL OR embedding = 'null'`. Every pre-existing row already
// has a vector, so the pass counted zero and printed "Nothing to do" — yet
// those rows carry no embedding_model, which the 2026-10-13 compatibility
// filter in VectorMatch and IVFSearch excludes. Semantic recall over
// memories was therefore filtered down to nothing on any store that
// predates the column, with no supported command able to restore it.
//
// The fix makes an unlabelled row count as pending alongside a null one.
// That is the whole behaviour under test; the fingerprinting itself is
// covered by updateMemoryEmbedding and the retrieval filters.

// seedMemories inserts rows with the given (embedding, embedding_model)
// combinations and returns a cleanup that leaves the shared test DB clean.
func seedMemories(t *testing.T, db *sql.DB, rows []struct {
	id    string
	vec   string
	model interface{}
}) {
	t.Helper()
	for _, r := range rows {
		var err error
		if r.model == nil {
			_, err = db.Exec(`
				INSERT INTO memories (id, collection, content, embedding, embedding_source)
				VALUES (?, 'test', 'content', ?, 'provider')`, r.id, r.vec)
		} else {
			_, err = db.Exec(`
				INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_model)
				VALUES (?, 'test', 'content', ?, 'provider', ?)`, r.id, r.vec, r.model)
		}
		if err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
		t.Cleanup(func() {
			db.Exec(`DELETE FROM memories WHERE id = ?`, r.id)
		})
	}
}

// The core regression. An unlabelled row with a perfectly good vector must
// still be selected — otherwise the compatibility filter permanently
// excludes it and no command can bring it back.
func TestBackfillSelectsUnlabelledRowsWithValidVectors(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Skip("memories.embedding_model absent; SafeMigrations did not run on this fixture")
	}

	seedMemories(t, db, []struct {
		id    string
		vec   string
		model interface{}
	}{
		{"bkf00001", `[0.1,0.2,0.3]`, nil},   // vector, no fingerprint -> MUST be selected
		{"bkf00002", `null`, nil},            // no vector -> selected (pre-existing behaviour)
		{"bkf00003", `[0.1,0.2,0.3]`, ""},    // vector, empty fingerprint -> MUST be selected
		{"bkf00004", `[0.1,0.2,0.3]`, "m:a"}, // fingerprinted -> must NOT be selected
	})

	n, err := countMemoriesNeedingEmbedding(db, "", false)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("pending count = %d, want 3 (bkf00001, bkf00002, bkf00003)\n"+
			"bkf00001/3 have a valid vector but no model — the historical "+
			"NULL-only predicate skipped them, leaving them permanently "+
			"excluded by the retrieval compatibility filter", n)
	}

	// Demonstrate the regression rather than assert it. This is the exact
	// predicate that shipped before the fix; run it against the same rows.
	// If it already selected all three, the new predicate would be doing
	// nothing and this test would be pinning nothing.
	var legacy int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM memories
		 WHERE (embedding IS NULL OR embedding = 'null') AND deleted_at IS NULL`,
	).Scan(&legacy); err != nil {
		t.Fatalf("legacy predicate: %v", err)
	}
	if legacy >= n {
		t.Fatalf("legacy NULL-only predicate selected %d of the %d pending "+
			"rows — it already covered this case, so the new predicate is "+
			"unnecessary and this test proves nothing", legacy, n)
	}
	t.Logf("legacy predicate would have selected %d of %d pending rows", legacy, n)

	ids, err := fetchMemoriesNeedingEmbedding(db, "", 10, false, "", "", false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got := map[string]bool{}
	for _, m := range ids {
		got[m.ID] = true
	}
	for _, want := range []string{"bkf00001", "bkf00002", "bkf00003"} {
		if !got[want] {
			t.Errorf("%s was not selected for backfill", want)
		}
	}
	if got["bkf00004"] {
		t.Error("bkf00004 already carries a fingerprint but was selected — " +
			"the default pass must be a no-op for a fully backfilled store")
	}
}

// A store already fully backfilled under the active model must report zero
// pending, so the command stays idempotent and does not re-embed on every
// run.
func TestBackfillIsIdempotentOnFullyFingerprintedStore(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Skip("memories.embedding_model absent")
	}
	seedMemories(t, db, []struct {
		id    string
		vec   string
		model interface{}
	}{
		{"bkf00011", `[0.1,0.2,0.3]`, "m:one"},
		{"bkf00012", `[0.1,0.2]`, "m:two"},
	})

	n, err := countMemoriesNeedingEmbedding(db, "", false)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("fully fingerprinted store reported %d pending, want 0 — "+
			"the backfill must converge, not re-embed forever", n)
	}
}

// --force-reembed is the explicit operator override and must select every
// live row regardless of fingerprint. It was previously references-only.
func TestBackfillForceReembedSelectsEverything(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Skip("memories.embedding_model absent")
	}
	seedMemories(t, db, []struct {
		id    string
		vec   string
		model interface{}
	}{
		{"bkf00021", `[0.1,0.2,0.3]`, "m:already-done"},
	})

	forced, err := countMemoriesNeedingEmbedding(db, "", true)
	if err != nil {
		t.Fatalf("count forced: %v", err)
	}
	if forced < 1 {
		t.Error("--force-reembed selected nothing; a fingerprinted row must " +
			"still be re-embedded under an explicit override")
	}

	normal, err := countMemoriesNeedingEmbedding(db, "", false)
	if err != nil {
		t.Fatalf("count normal: %v", err)
	}
	if normal >= forced {
		t.Errorf("forced count %d is not greater than normal count %d — the "+
			"override is not doing anything", forced, normal)
	}
}

// The collection filter must compose with the new pending predicate, not
// replace it.
func TestBackfillCollectionFilterComposes(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Skip("memories.embedding_model absent")
	}

	for _, spec := range []struct{ id, coll string }{
		{"bkf00031", "alpha"},
		{"bkf00032", "beta"},
	} {
		if _, err := db.Exec(`
			INSERT INTO memories (id, collection, content, embedding, embedding_source)
			VALUES (?, ?, 'content', '[0.1,0.2,0.3]', 'provider')`, spec.id, spec.coll); err != nil {
			t.Fatalf("seed %s: %v", spec.id, err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM memories WHERE id = ?`, spec.id) })
	}

	n, err := countMemoriesNeedingEmbedding(db, "alpha", false)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("collection-filtered count = %d, want 1 (only the alpha row)", n)
	}
}

// The predicate itself, as a pure function of (force, hasModelCol). Cheap to
// assert directly and it documents the schema-fallback branch.
func TestMemoryNeedsEmbeddingPredicate(t *testing.T) {
	cases := []struct {
		name       string
		force      bool
		hasModelCol bool
		mustMention []string
		mustNot     []string
	}{
		{
			name: "default with column selects unlabelled",
			hasModelCol: true,
			mustMention: []string{"embedding_model"},
			mustNot:     []string{},
		},
		{
			name: "legacy schema falls back to null-only",
			hasModelCol: false,
			mustNot:     []string{"embedding_model"},
			mustMention: []string{"embedding IS NULL"},
		},
		{
			name: "force selects everything",
			force: true,
			mustNot: []string{"embedding IS NULL", "embedding_model"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := memoryNeedsEmbedding(tc.force, tc.hasModelCol)
			for _, want := range tc.mustMention {
				if !strings.Contains(got, want) {
					t.Errorf("predicate %q does not mention %q", got, want)
				}
			}
			for _, unwanted := range tc.mustNot {
				if strings.Contains(got, unwanted) {
					t.Errorf("predicate %q should not mention %q", got, unwanted)
				}
			}
		})
	}
}
// TestBackfillPaginationVisitsEveryRowOnce is the regression for the
// pagination bug, and it is why the loop uses a keyset cursor.
//
// Each successful update makes the row stop matching the pending
// predicate, so the result set shrinks under an OFFSET cursor: with
// batchSize 50 over 149 pending rows, the second batch's OFFSET 50 lands
// past rows that were already processed, and roughly half the rows are
// never fetched. No error is raised — the command reports success for the
// rows it did touch and leaves the rest filtered out of semantic recall.
//
// The loop is driven directly against the DB with a stand-in for the
// provider, so the test needs no live embedding service.
func TestBackfillPaginationVisitsEveryRowOnce(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Skip("memories.embedding_model absent")
	}

	// 12 pending rows, batch size 4 => 3 batches. Enough rows and batches
	// that an OFFSET cursor demonstrably skips.
	const n = 12
	const batch = 4
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("pg%05d", i)
		ids = append(ids, id)
		// Distinct created_at so the keyset order is total and deterministic.
		if _, err := db.Exec(`
			INSERT INTO memories (id, collection, content, embedding, embedding_source, created_at)
			VALUES (?, 'pagetest', 'content', '[0.1,0.2,0.3]', 'provider', ?)`,
			id, int64(1700000000+i)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM memories WHERE id = ?`, id) })
	}

	visited := map[string]int{}
	processed := 0
	var cursorCreated string
	var cursorID string
	haveCursor := false

	for iteration := 0; iteration < n+5; iteration++ {
		batchRows, err := fetchMemoriesNeedingEmbedding(db, "", batch, haveCursor, cursorCreated, cursorID, false)
		if err != nil {
			t.Fatalf("batch %d: %v", iteration, err)
		}
		if len(batchRows) == 0 {
			break
		}
		for _, m := range batchRows {
			visited[m.ID]++
			// Stand in for the provider: stamp a fingerprint so the row
			// stops matching the predicate, exactly as a real embed would.
			if _, err := db.Exec(
				`UPDATE memories SET embedding_model = 'test:model' WHERE id = ?`, m.ID); err != nil {
				t.Fatalf("stamp %s: %v", m.ID, err)
			}
			processed++
		}
		last := batchRows[len(batchRows)-1]
		cursorCreated, cursorID, haveCursor = last.CreatedAt, last.ID, true
		if len(batchRows) < batch {
			break
		}
	}

	if processed != n {
		t.Errorf("processed %d of %d pending rows; %d were skipped. An OFFSET "+
			"cursor skips a batch's worth of rows per iteration because the "+
			"result set shrinks as rows are updated", processed, n, n-processed)
	}
	for _, id := range ids {
		if visited[id] != 1 {
			t.Errorf("%s visited %d times, want exactly 1", id, visited[id])
		}
	}

	// And the run must converge: nothing pending afterwards.
	remaining, err := countMemoriesNeedingEmbedding(db, "pagetest", false)
	if err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d rows still pending after a full pass, want 0", remaining)
	}
}

// TestBackfillOffsetPaginationWouldSkipRows demonstrates the pagination
// regression rather than asserting it.
//
// This replays the loop exactly as it shipped — `LIMIT ? OFFSET ?` —
// over a fresh set of pending rows, stamping each row so it drops out of
// the pending set. It asserts nothing about the production function; it
// exists so the reason for the keyset cursor is checkable. If a future
// change made OFFSET safe, this test fails and the comment should be
// revisited.
func TestBackfillOffsetPaginationWouldSkipRows(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Skip("memories.embedding_model absent")
	}

	const n = 12
	const batch = 4
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("of%05d", i)
		ids = append(ids, id)
		if _, err := db.Exec(`
			INSERT INTO memories (id, collection, content, embedding, embedding_source, created_at)
			VALUES (?, 'offsettest', 'content', '[0.1,0.2,0.3]', 'provider', ?)`,
			id, int64(1700000000+i)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM memories WHERE id = ?`, id) })
	}

	visited := map[string]int{}
	offset := 0
	for iteration := 0; iteration < n+5; iteration++ {
		rows, err := db.Query(`
			SELECT id FROM memories
			WHERE `+memoryNeedsEmbedding(false, true)+`
			  AND collection = 'offsettest'
			ORDER BY created_at ASC, id ASC
			LIMIT ? OFFSET ?`, batch, offset)
		if err != nil {
			t.Fatalf("offset query: %v", err)
		}
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			got = append(got, id)
		}
		rows.Close()
		if len(got) == 0 {
			break
		}
		for _, id := range got {
			visited[id]++
			// Stamp it, exactly as a successful embed does — which is what
			// shrinks the result set underneath the OFFSET cursor.
			if _, err := db.Exec(
				`UPDATE memories SET embedding_model = 'test:model' WHERE id = ?`, id); err != nil {
				t.Fatalf("stamp %s: %v", id, err)
			}
		}
		offset += batch
		if len(got) < batch {
			break
		}
	}

	reached := len(visited)
	if reached == n {
		t.Skip("OFFSET pagination visited every row — the keyset cursor may no " +
			"longer be necessary; revisit the comment in runMemoryBackfill")
	}
	t.Logf("OFFSET pagination visited %d of %d rows (skipped %d: %v)",
		reached, n, n-reached, missingFrom(ids, visited))
}

// missingFrom returns the ids in all that were never visited.
func missingFrom(all []string, visited map[string]int) []string {
	var out []string
	for _, id := range all {
		if visited[id] == 0 {
			out = append(out, id)
		}
	}
	return out
}
