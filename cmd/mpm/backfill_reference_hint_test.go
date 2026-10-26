// backfill_reference_hint_test.go — the backfill summary must not read as
// "everything is embedded" when it only covered memories.
//
// Observed in the round-5 acceptance run: a two-document corpus (28 chunks)
// was imported with no embedding provider, so every chunk was pending.
// `mpm ops backfill-embeddings` — the command the import warning pointed at
// — printed
//
//	Memory backfill complete: 5 succeeded, 0 failed
//
// and left all 25 reference chunks without vectors. The command was correct
// for its documented default scope (memories); the message implied a
// completeness it did not have, and the warning pointed at the wrong
// invocation.
//
// The import warning now names `--include references`. These tests pin the
// second half: when the default scope leaves reference chunks unembedded,
// the summary says so and names the flag.
package main

import (
	"strings"
	"testing"
)

// TestReferenceBackfillHint_SilentWhenNothingIsPending pins that the note is
// absent on a clean corpus.
func TestReferenceBackfillHint_SilentWhenNothingIsPending(t *testing.T) {
	dm := newTestDMForCmd(t)
	if hint := referenceBackfillHint(dm.SQLDB()); hint != "" {
		t.Errorf("note printed on an empty corpus: %q", hint)
	}
}

// TestReferenceBackfillHint_NamesTheReferenceScope pins the note's content
// when a reference chunk has no vector.
func TestReferenceBackfillHint_NamesTheReferenceScope(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if _, err := db.Exec(`
		INSERT INTO reference_docs
		(id, title, file_path, source_type, tags, content, content_hash, total_chunks, last_indexed)
		VALUES ('ref-hint-1', 'Hint doc', '/tmp/hint.md', 'markdown', '[]', 'body', 'hash-1', 1, '0')`); err != nil {
		t.Fatalf("seed reference doc: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO reference_chunks
		(id, doc_id, chunk_index, content, source_path, embedding)
		VALUES ('ref-hint-1-c0', 'ref-hint-1', 0, 'pending body', '/tmp/hint.md', NULL)`); err != nil {
		t.Fatalf("seed reference chunk: %v", err)
	}

	hint := referenceBackfillHint(db)
	if hint == "" {
		t.Fatal("no note was printed even though a reference chunk has no vector")
	}
	if !strings.Contains(hint, "--include references") {
		t.Errorf("note does not name the reference scope: %q", hint)
	}

	// Once the chunk carries a vector the note stops: it reports state, not
	// history.
	if _, err := db.Exec(`UPDATE reference_chunks SET embedding = '[0.1]' WHERE id = 'ref-hint-1-c0'`); err != nil {
		t.Fatalf("set embedding: %v", err)
	}
	if hint := referenceBackfillHint(db); hint != "" {
		t.Errorf("note still printed after the chunk was embedded: %q", hint)
	}
}
