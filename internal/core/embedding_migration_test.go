// embedding_migration_test.go — forensic classifier for embedding_source
// and embedding_dimension, plus RunMigration integration test.
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

// TestRunMigration_IdempotentAndProvenanceGated exercises the full RunMigration
// orchestrator: forensic classifier, synthetic-theory marker, provenance-gated
// un-challenge, and idempotent sentinel. The setup has three memories
// (hash-sourced, provider-sourced, null-sourced) and two theories: one
// challenging the hash memory (must become synthetic) and one challenging
// the provider memory (must NOT become synthetic). Only the hash memory's
// weight should be restored (all its challenges are synthetic).
//
// NOTE: the memories.kind column does not exist in the current schema.
// markSyntheticTheories uses source_id (not memory_id — theories reference
// the challenged memory via source_id) and has no kind filter. This matches
// the spec intent: mark all theories whose challenged-memory was hash-embedded.
// The provenance-gated gate uses weight<1.0 as the action-precondition
// heuristic (origin_weight column does not exist).
func TestRunMigration_IdempotentAndProvenanceGated(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Build the 256-dim hash embedding (HashEmbed convention: JSON []float32 len=256).
	vec256 := make([]float32, 256)
	for i := range vec256 {
		vec256[i] = float32(i) / 255.0
	}
	emb256, _ := json.Marshal(vec256)

	// Build a 384-dim provider embedding (non-256 → provider source).
	vec384 := make([]float32, 384)
	for i := range vec384 {
		vec384[i] = float32(i) / 255.0
	}
	emb384, _ := json.Marshal(vec384)

	// Insert three fixture memories covering all embedding_source cases.
	// weight=0.5 for hash-mem (challenged, < 1.0) to test un-challenge.
	// weight=1.0 for prov-mem and null-mem (unchallenged).
	fixtures := []struct {
		id     string
		coll   string
		content string
		emb    interface{} // string = JSON text, nil = SQL NULL
		embSrc string
		embDim interface{}
		weight float64
	}{
		{
			id: "hash-mem", coll: "memories", content: "hash-sourced memory",
			emb: string(emb256), embSrc: "hash", embDim: 256, weight: 0.5,
		},
		{
			id: "prov-mem", coll: "memories", content: "provider-sourced memory",
			emb: string(emb384), embSrc: "provider", embDim: 384, weight: 1.0,
		},
		{
			id: "null-mem", coll: "memories", content: "null-embedded memory",
			emb: nil, embSrc: "null", embDim: nil, weight: 1.0,
		},
	}

	for _, f := range fixtures {
		if f.emb == nil {
			_, err := dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension, weight, synthetic)
				VALUES (?, ?, ?, NULL, ?, ?, ?, 0)`,
				f.id, f.coll, f.content, f.embSrc, f.embDim, f.weight)
			if err != nil {
				t.Fatalf("insert %s: %v", f.id, err)
			}
		} else {
			_, err := dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension, weight, synthetic)
				VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
				f.id, f.coll, f.content, f.emb, f.embSrc, f.embDim, f.weight)
			if err != nil {
				t.Fatalf("insert %s: %v", f.id, err)
			}
		}
	}

	// Insert two theories:
	// - hash-theory challenges hash-mem → must become synthetic
	// - prov-theory challenges prov-mem → must NOT become synthetic
	//
	// Theories (collection='theories') reference the challenged memory via
	// metadata['challenged_memory_id'] — NOT via source_id. This matches
	// production theory creation in epistemology_tools.go:54-60. Storing
	// the reference in source_id would test the wrong schema.
	theories := []struct {
		id      string
		challengedID string // stored in metadata['challenged_memory_id']
		coll    string
		content string
		weight  float64
	}{
		{
			id: "hash-theory", challengedID: "hash-mem", coll: "theories",
			content: "CHALLENGED_MEMORY_ID: hash-mem\nEVIDENCE: theoretical challenge\nCHALLENGED_AT_NANO: 0\nORIGINAL_CONTENT: hash-sourced memory",
			weight: 1.0,
		},
		{
			id: "prov-theory", challengedID: "prov-mem", coll: "theories",
			content: "CHALLENGED_MEMORY_ID: prov-mem\nEVIDENCE: theoretical challenge\nCHALLENGED_AT_NANO: 0\nORIGINAL_CONTENT: provider-sourced memory",
			weight: 1.0,
		},
	}

	for _, th := range theories {
		// Mirror production: challenged_memory_id goes into metadata JSON,
		// source_id stays NULL.
		metaJSON, _ := json.Marshal(map[string]interface{}{
			"status":               "pending",
			"challenged_memory_id": th.challengedID,
			"evidence":             "theoretical challenge",
		})
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, weight, synthetic)
			VALUES (?, ?, ?, ?, ?, 0)`,
			th.id, th.coll, th.content, string(metaJSON), th.weight)
		if err != nil {
			t.Fatalf("insert theory %s: %v", th.id, err)
		}
	}

	// Run the migration.
	if err := RunMigration(dm); err != nil {
		t.Fatalf("RunMigration: %v", err)
	}

	// ── Assert embedding_source classifications ──────────────────────────
	for _, f := range fixtures {
		var gotSrc string
		var gotDim *int64
		err := dm.SQLDB().QueryRow(`
			SELECT embedding_source, embedding_dimension FROM memories WHERE id = ?`, f.id,
		).Scan(&gotSrc, &gotDim)
		if err != nil {
			t.Fatalf("query classification %s: %v", f.id, err)
		}
		if gotSrc != f.embSrc {
			t.Errorf("%s: embedding_source=%q, want %q", f.id, gotSrc, f.embSrc)
		}
		if f.embDim == nil {
			if gotDim != nil {
				t.Errorf("%s: embedding_dimension=%v, want NULL", f.id, *gotDim)
			}
		} else {
			if gotDim == nil {
				t.Errorf("%s: embedding_dimension=NULL, want %v", f.id, f.embDim)
			} else if *gotDim != int64(f.embDim.(int)) {
				t.Errorf("%s: embedding_dimension=%d, want %v", f.id, *gotDim, f.embDim)
			}
		}
	}

	// ── Assert synthetic markers on theories ────────────────────────────
	// hash-theory → synthetic=1 (challenges a hash-embedded memory)
	// prov-theory → synthetic=0 (challenges a provider-embedded memory)
	wantSynthetic := map[string]int{
		"hash-theory": 1,
		"prov-theory":  0,
	}
	for thID, want := range wantSynthetic {
		var got int
		err := dm.SQLDB().QueryRow(`SELECT synthetic FROM memories WHERE id = ?`, thID).Scan(&got)
		if err != nil {
			t.Fatalf("query synthetic %s: %v", thID, err)
		}
		if got != want {
			t.Errorf("%s: synthetic=%d, want %d", thID, got, want)
		}
	}

	// ── Assert provenance-gated un-challenge ─────────────────────────────
	// hash-mem: all challenges are synthetic → weight restored to 1.0
	// prov-mem: has a non-synthetic challenge → weight unchanged
	// null-mem: no theories → weight unchanged
	wantWeight := map[string]float64{
		"hash-mem": 1.0, // was 0.5, restored because all challenges are synthetic
		"prov-mem": 1.0, // was 1.0, unchanged because prov-theory is NOT synthetic
		"null-mem": 1.0, // was 1.0, unchanged (no theories)
	}
	for memID, want := range wantWeight {
		var got float64
		err := dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, memID).Scan(&got)
		if err != nil {
			t.Fatalf("query weight %s: %v", memID, err)
		}
		if got != want {
			t.Errorf("%s: weight=%f, want %f", memID, got, want)
		}
	}

	// ── Assert sentinel row for idempotency ─────────────────────────────
	var sentinelCount int
	err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM embedding_migration_log WHERE reason = 'migration_applied'`,
	).Scan(&sentinelCount)
	if err != nil {
		t.Fatalf("query sentinel: %v", err)
	}
	if sentinelCount != 1 {
		t.Errorf("sentinel count=%d, want 1", sentinelCount)
	}

	// ── Assert un-challenge log rows were recorded ──────────────────────
	var unchallengeCount int
	err = dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM embedding_migration_log WHERE reason = 'unchallenge_provenance_gated'`,
	).Scan(&unchallengeCount)
	if err != nil {
		t.Fatalf("query unchallenge log: %v", err)
	}
	if unchallengeCount != 1 {
		t.Errorf("unchallenge log count=%d, want 1 (only hash-mem)", unchallengeCount)
	}

	// ── Second run: must be idempotent (no changes) ─────────────────────
	// Capture weights before second run.
	weightsBefore := make(map[string]float64)
	for _, f := range fixtures {
		var w float64
		dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, f.id).Scan(&w)
		weightsBefore[f.id] = w
	}
	syntheticBefore := make(map[string]int)
	for thID := range wantSynthetic {
		var s int
		dm.SQLDB().QueryRow(`SELECT synthetic FROM memories WHERE id = ?`, thID).Scan(&s)
		syntheticBefore[thID] = s
	}
	sentinelBefore := sentinelCount

	if err := RunMigration(dm); err != nil {
		t.Fatalf("RunMigration (second): %v", err)
	}

	// Weights must be unchanged.
	for _, f := range fixtures {
		var w float64
		dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, f.id).Scan(&w)
		if w != weightsBefore[f.id] {
			t.Errorf("%s: second-run weight=%f, want %f (idempotency broken)",
				f.id, w, weightsBefore[f.id])
		}
	}
	// Synthetics must be unchanged.
	for thID := range wantSynthetic {
		var s int
		dm.SQLDB().QueryRow(`SELECT synthetic FROM memories WHERE id = ?`, thID).Scan(&s)
		if s != syntheticBefore[thID] {
			t.Errorf("%s: second-run synthetic=%d, want %d (idempotency broken)",
				thID, s, syntheticBefore[thID])
		}
	}
	// Sentinel count must still be 1 (no duplicate sentinel).
	var sentinelAfter int
	dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM embedding_migration_log WHERE reason = 'migration_applied'`).Scan(&sentinelAfter)
	if sentinelAfter != sentinelBefore {
		t.Errorf("second-run sentinel count=%d, want %d (idempotency broken)", sentinelAfter, sentinelBefore)
	}
}

