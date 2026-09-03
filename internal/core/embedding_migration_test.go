// embedding_migration_test.go — forensic classifier for embedding_source
// and embedding_dimension.
package internal

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestForensicClassifier(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Build the four fixture embeddings as JSON strings.
	vec256 := make([]float32, 256)
	for i := range vec256 {
		vec256[i] = float32(i) / 255.0
	}
	emb256, _ := json.Marshal(vec256)

	vec384 := make([]float32, 384)
	for i := range vec384 {
		vec384[i] = float32(i) / 255.0
	}
	emb384, _ := json.Marshal(vec384)

	// Insert four rows covering all classification cases.
	// The classifier is idempotent; rows already have the CORRECT
	// embedding_source so a second run is a no-op.
	rows := []struct {
		id              string
		embeddingSource string // already-set value — second run must be no-op
		embJSON         string
		embDim          interface{} // nil = SQL NULL column
	}{
		{
			id:              "hash-256",
			embeddingSource: "hash",
			embJSON:         string(emb256),
			embDim:          256,
		},
		{
			id:              "provider-384",
			embeddingSource: "provider",
			embJSON:         string(emb384),
			embDim:          384,
		},
		{
			id:              "sql-null",
			embeddingSource: "null",
			embJSON:         "", // SQL NULL — omit from VALUES list
			embDim:          nil,
		},
		{
			id:              "literal-null",
			embeddingSource: "null",
			embJSON:         `"null"`, // literal "null" string stored in embedding
			embDim:          nil,
		},
	}

	for _, r := range rows {
		if r.embJSON == "" {
			// SQL NULL embedding
			_, err := dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension)
				VALUES (?, 'memories', ?, NULL, ?, NULL)`,
				r.id, fmt.Sprintf("content for %s", r.id), r.embeddingSource)
			if err != nil {
				t.Fatalf("insert %s: %v", r.id, err)
			}
		} else {
			_, err := dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension)
				VALUES (?, 'memories', ?, ?, ?, ?)`,
				r.id, fmt.Sprintf("content for %s", r.id), r.embJSON, r.embeddingSource, r.embDim)
			if err != nil {
				t.Fatalf("insert %s: %v", r.id, err)
			}
		}
	}

	// Run the classifier.
	if err := RunForensicClassifier(dm); err != nil {
		t.Fatalf("RunForensicClassifier: %v", err)
	}

	// Verify each row's embedding_source and embedding_dimension.
	type expected struct {
		source string
		dim    interface{} // nil = NULL
	}
	exp := map[string]expected{
		"hash-256":       {source: "hash", dim: 256},
		"provider-384":   {source: "provider", dim: 384},
		"sql-null":       {source: "null", dim: nil},
		"literal-null":   {source: "null", dim: nil},
	}

	for id, e := range exp {
		var gotSource string
		var gotDim *int64
		err := dm.SQLDB().QueryRow(`
			SELECT embedding_source, embedding_dimension FROM memories WHERE id = ?`, id,
		).Scan(&gotSource, &gotDim)
		if err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if gotSource != e.source {
			t.Errorf("%s: embedding_source=%q, want %q", id, gotSource, e.source)
		}
		if e.dim == nil {
			if gotDim != nil {
				t.Errorf("%s: embedding_dimension=%v, want NULL", id, *gotDim)
			}
		} else {
			if gotDim == nil {
				t.Errorf("%s: embedding_dimension=NULL, want %v", id, e.dim)
			} else if *gotDim != int64(e.dim.(int)) {
				t.Errorf("%s: embedding_dimension=%d, want %v", id, *gotDim, e.dim)
			}
		}
	}

	// Run the classifier a second time — must be a no-op.
	if err := RunForensicClassifier(dm); err != nil {
		t.Fatalf("RunForensicClassifier (second run): %v", err)
	}

	// Assert counts of each embedding_source are unchanged.
	for id, e := range exp {
		var count int
		err := dm.SQLDB().QueryRow(`
			SELECT COUNT(*) FROM memories WHERE id = ? AND embedding_source = ? AND embedding_dimension IS NOT DISTINCT FROM ?`,
			id, e.source, e.dim,
		).Scan(&count)
		if err != nil {
			t.Fatalf("count check %s: %v", id, err)
		}
		if count != 1 {
			t.Errorf("%s: second-run: count=%d, want 1 (idempotency broken)", id, count)
		}
	}
}
