// embedding_model_compatibility_test.go — pins that memory embeddings are
// only ever compared against vectors from the same model.
//
// The gap these cover:
//
//   - memories had embedding_source and embedding_dimension but NO
//     embedding_model, so model identity for a memory vector was
//     unrecoverable from stored data (all 149 live rows carried NULL);
//   - VectorSearch loaded every non-NULL embedding with no filter at all
//     (db.go:4234), so a store holding two models' vectors scored them as
//     if they shared a vector space.
//
// Dimension equality is NOT a sufficient guard and must not be mistaken for
// one. Two models can share a dimension while producing incomparable
// spaces; the pre-existing `len(dbEmbedding) != len(queryEmbedding)` check
// passes in exactly that case and cosineSimilarity then returns a
// plausible-looking number in [-1,1] that describes nothing. Worse, a high
// score on that number is load-bearing: HybridSearch challenges — and
// therefore MUTATES — a memory at cosine >= 0.85.
package internal

import (
	"encoding/json"
	"math"
	"testing"
)

// activeFingerprintCache is process-global (see activeEmbeddingDimMu in
// reference_db.go). Reading CurrentEmbeddingFingerprint() populates it via a
// sample embed, which arms the embedding_model filter for EVERY later test in
// this package. Tests that seed unfingerprinted rows — including several
// pre-existing VectorMatch tests — then see an empty corpus and fail.
//
// So any test that reads the fingerprint must restore the cache on exit. It is
// cleared rather than restored to a previous value because there is no getter
// for the cached state, and the correct post-test state is "not yet sampled",
// which is what a fresh process would have.
func useActiveFingerprint(t *testing.T) ReferenceEmbeddingFingerprint {
	t.Helper()
	ResetActiveEmbeddingDimensionForTest()
	t.Cleanup(ResetActiveEmbeddingDimensionForTest)
	fp := CurrentEmbeddingFingerprint()
	if fp.Model == "" || fp.Dimension == 0 {
		t.Skip("no embedding provider configured in this test environment")
	}
	return fp
}

// sameDimensionDifferentModelVectors builds two vectors that are BOTH
// 384-dimensional and BOTH individually well-formed, but which come from
// unrelated models. A dimension check cannot distinguish them; only the
// model fingerprint can.
func sameDimensionDifferentModelVectors() (a, b []float32) {
	a = make([]float32, 384)
	b = make([]float32, 384)
	for i := range a {
		a[i] = float32(math.Sin(float64(i) * 0.1))
		// A different, unrelated space: rotated and offset so the cosine
		// is high-but-wrong rather than obviously zero.
		b[i] = float32(math.Cos(float64(i)*0.1)) + 0.5
	}
	return a, b
}

// TestDimensionMatchIsNotModelCompatibility is the load-bearing claim.
// If this ever passes trivially (i.e. dimension alone rejected the pair),
// the test would stop proving what it is named for.
func TestDimensionMatchIsNotModelCompatibility(t *testing.T) {
	a, b := sameDimensionDifferentModelVectors()
	if len(a) != len(b) {
		t.Fatalf("precondition: vectors must share a dimension, got %d and %d", len(a), len(b))
	}
	// validEmbeddingPair accepts them: dimension matches and both are finite.
	if !validEmbeddingPair(a, b) {
		t.Fatal("precondition: a same-dimension finite pair should pass validEmbeddingPair")
	}
	// And the cosine is a plausible-looking number in [-1,1]...
	sim := cosineSimilarity(a, b)
	if sim < -1 || sim > 1 {
		t.Errorf("cosine %.4f outside [-1,1]", sim)
	}
	// ...which is exactly why it cannot be trusted without a model match.
	// This test documents the hazard; ModelFingerprintMismatchIsRejected
	// below is what actually prevents it.
}

// TestMemoryEmbeddingModelColumnExists guards the schema migration: the
// fingerprint is what every other guarantee here rests on.
func TestMemoryEmbeddingModelColumnExists(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	cols, err := dm.db.Query("PRAGMA table_info(memories)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer cols.Close()
	found := false
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "embedding_model" {
			found = true
		}
	}
	if !found {
		t.Error("memories has no embedding_model column — model identity for a " +
			"memory vector is unrecoverable from stored data")
	}
}

// TestReferenceChunksHasEmbeddingModelParity — memories must not be the
// only tier carrying a fingerprint; reference_chunks has had one since
// 2026-09-28 and this pins the mirror.
func TestReferenceChunksHasEmbeddingModelParity(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	cols, err := dm.db.Query("PRAGMA table_info(reference_chunks)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer cols.Close()
	found := false
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "embedding_model" {
			found = true
		}
	}
	if !found {
		t.Error("reference_chunks lost its embedding_model column")
	}
}

// TestEmbeddingFingerprintNamesTheModel — a dimension-only "fingerprint"
// is not a fingerprint. CurrentEmbeddingFingerprint must carry the model
// identity, not merely a length.
func TestEmbeddingFingerprintNamesTheModel(t *testing.T) {
	ResetActiveEmbeddingDimensionForTest()
	t.Cleanup(ResetActiveEmbeddingDimensionForTest)
	fp := CurrentEmbeddingFingerprint()
	t.Logf("active fingerprint: model=%q dimension=%d", fp.Model, fp.Dimension)
	if fp.Model == "" && fp.Dimension != 0 {
		t.Errorf("dimension %d recorded with no model identity — that is the "+
			"dimension-only check this column exists to replace", fp.Dimension)
	}
	// When no provider is configured the fingerprint must be empty, not a
	// plausible-looking default that would match garbage.
	if fp.Model == "" && fp.Dimension != 0 {
		t.Error("empty model with non-zero dimension must not happen")
	}
}

// TestVectorSearchExcludesForeignModelRows is the behavioural guarantee.
//
// Two memories hold same-dimension vectors stamped with different models.
// Only the one matching the active provider may be returned. Without the
// filter both are returned and the foreign vector's meaningless cosine
// competes in the ranking.
func TestVectorSearchExcludesForeignModelRows(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	fp := useActiveFingerprint(t)

	mine := make([]float32, fp.Dimension)
	for i := range mine {
		mine[i] = float32(i%7) / 7.0
	}
	foreign := make([]float32, fp.Dimension)
	for i := range foreign {
		foreign[i] = float32((i*13)%11) / 11.0
	}

	mineJSON, _ := json.Marshal(mine)
	foreignJSON, _ := json.Marshal(foreign)

	insert := func(id, content, vecJSON, model string) {
		_, err := dm.db.Exec(`
			INSERT INTO memories (id, collection, content, embedding, embedding_source,
			                      embedding_dimension, embedding_model)
			VALUES (?, 'memories', ?, ?, 'provider', ?, ?)`,
			id, content, vecJSON, fp.Dimension, model)
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("aaa11111", "same-model memory", string(mineJSON), fp.Model)
	insert("bbb22222", "foreign-model memory", string(foreignJSON), fp.Model+"-not-the-active-one")

	res, err := dm.VectorSearch("memories", mine, 10)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	for _, r := range res {
		if id, _ := r["id"].(string); id == "bbb22222" {
			t.Errorf("VectorSearch returned a memory embedded by a different model "+
				"(%q vs active %q); its cosine describes nothing and a high score "+
				"triggers ChallengeMemory", id, fp.Model)
		}
	}
	var sawMine bool
	for _, r := range res {
		if id, _ := r["id"].(string); id == "aaa11111" {
			sawMine = true
		}
	}
	if !sawMine {
		t.Error("VectorSearch excluded the memory embedded by the ACTIVE model — " +
			"the filter is over-restrictive")
	}
}

// TestVectorSearchExcludesUnfingerprintedRows — a vector with NULL
// embedding_model predates the column. Its model cannot be proven, so it
// must not compete with vectors that carry a verified fingerprint.
func TestVectorSearchExcludesUnfingerprintedRows(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	fp := useActiveFingerprint(t)

	vec := make([]float32, fp.Dimension)
	for i := range vec {
		vec[i] = float32(i%5) / 5.0
	}
	vecJSON, _ := json.Marshal(vec)

	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension)
		VALUES (?, 'memories', ?, ?, 'provider', ?)`, "ccc33333", "legacy unfingerprinted", string(vecJSON), fp.Dimension)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := dm.VectorSearch("memories", vec, 10)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	for _, r := range res {
		if id, _ := r["id"].(string); id == "ccc33333" {
			t.Error("VectorSearch returned a memory with NULL embedding_model; " +
				"an unrecorded model cannot be proven comparable to the active one")
		}
	}
}

// TestVectorMatchExcludesForeignModelRows covers the ACTUAL semantic path.
//
// `mpm recall --semantic` reaches memory vectors through
// VectorMatch (hybrid_search.go), not through VectorSearch. Filtering only
// VectorSearch would leave the real path unprotected while the unit test
// still passed — which is exactly the gap this test closes. Both the IVF
// fast path and its brute-force fallback are exercised, because the two
// must not disagree about which rows are comparable.
func TestVectorMatchExcludesForeignModelRows(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	fp := useActiveFingerprint(t)

	mine := make([]float32, fp.Dimension)
	foreign := make([]float32, fp.Dimension)
	for i := range mine {
		mine[i] = float32(i%7) / 7.0
		foreign[i] = float32((i*13)%11) / 11.0
	}
	mineJSON, _ := json.Marshal(mine)
	foreignJSON, _ := json.Marshal(foreign)

	ins := func(id, content, vec, model string) {
		_, err := dm.db.Exec(`
			INSERT INTO memories (id, collection, content, embedding, embedding_source,
			                      embedding_dimension, embedding_model)
			VALUES (?, 'memories', ?, ?, 'provider', ?, ?)`,
			id, content, vec, fp.Dimension, model)
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	ins("ddd44444", "same-model memory", string(mineJSON), fp.Model)
	ins("eee55555", "foreign-model memory", string(foreignJSON), fp.Model+"-not-active")

	matches, err := dm.VectorMatch("memories", mine, 10, "")
	if err != nil {
		t.Fatalf("VectorMatch: %v", err)
	}
	for _, m := range matches {
		if m.ID == "eee55555" {
			t.Error("VectorMatch returned a memory embedded by a different model; " +
				"its cosine is computed over an unrelated vector space and a score " +
				">= 0.85 triggers ChallengeMemory")
		}
	}
	var sawMine bool
	for _, m := range matches {
		if m.ID == "ddd44444" {
			sawMine = true
		}
	}
	if !sawMine {
		t.Error("VectorMatch excluded the memory embedded by the ACTIVE model")
	}
}

// TestVectorMatchExcludesUnlabelledRows is the migration's operational
// consequence, pinned deliberately.
//
// On the live database every pre-migration memory row has a NULL
// embedding_model, so this filter excludes all of them and semantic
// retrieval returns nothing until a re-embed. That is correct — an
// unrecorded model cannot be proven compatible — but it is a large,
// silent behaviour change if discovered by surprise, so it is pinned here
// and surfaced as a WARN with a remedy in VectorMatch itself.
func TestVectorMatchExcludesUnlabelledRows(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	fp := useActiveFingerprint(t)

	vec := make([]float32, fp.Dimension)
	for i := range vec {
		vec[i] = float32(i%5) / 5.0
	}
	vecJSON, _ := json.Marshal(vec)

	if _, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension)
		VALUES (?, 'memories', ?, ?, 'provider', ?)`, "fff66666", "unlabelled", string(vecJSON), fp.Dimension); err != nil {
		t.Fatalf("insert: %v", err)
	}

	matches, err := dm.VectorMatch("memories", vec, 10, "")
	if err != nil {
		t.Fatalf("VectorMatch: %v", err)
	}
	for _, m := range matches {
		if m.ID == "fff66666" {
			t.Error("VectorMatch returned an unlabelled memory; its model cannot " +
				"be proven compatible with the active provider")
		}
	}
}

// TestVectorMatchPartialBackfillStillRetrieves pins the mixed state an
// operator actually passes through: some rows re-embedded under the active
// model, the rest still unlabelled.
//
// The compatibility filter must exclude only the unlabelled half. If it
// dropped both halves, retrieval would silently shrink toward nothing as
// the backfill progressed — worst exactly when the operator is watching to
// confirm the backfill is working. This also guards the WARN in
// VectorMatch: it is gated on `labelled == 0` precisely so a partially
// backfilled store does not emit "every embedded memory is excluded",
// which is false here and would teach the operator to ignore it.
func TestVectorMatchPartialBackfillStillRetrieves(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	fp := useActiveFingerprint(t)

	vec := make([]float32, fp.Dimension)
	for i := range vec {
		vec[i] = float32(i%5) / 5.0
	}
	vecJSON, _ := json.Marshal(vec)

	if _, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension)
		VALUES (?, 'memories', ?, ?, 'provider', ?)`, "aaa11111", "still unlabelled", string(vecJSON), fp.Dimension); err != nil {
		t.Fatalf("insert unlabelled: %v", err)
	}
	if _, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, embedding, embedding_source,
		                      embedding_dimension, embedding_model)
		VALUES (?, 'memories', ?, ?, 'provider', ?, ?)`,
		"bbb22222", "already re-embedded", string(vecJSON), fp.Dimension, fp.Model); err != nil {
		t.Fatalf("insert labelled: %v", err)
	}

	matches, err := dm.VectorMatch("memories", vec, 10, "")
	if err != nil {
		t.Fatalf("VectorMatch: %v", err)
	}
	var sawLabelled bool
	for _, m := range matches {
		switch m.ID {
		case "bbb22222":
			sawLabelled = true
		case "aaa11111":
			t.Error("unlabelled row returned from a partially backfilled store")
		}
	}
	if !sawLabelled {
		t.Error("partially backfilled store returned nothing — the filter excluded " +
			"the labelled row too, so retrieval degrades as the backfill progresses")
	}
}
