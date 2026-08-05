package capability

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"testing"
)

func TestCosineSimilarity_IdenticalVectors(t *testing.T) {
	v := []float32{1, 2, 3, 4}
	got := cosineSimilarity(v, v)
	if math.Abs(float64(got-1.0)) > 1e-5 {
		t.Errorf("identical vectors should give 1.0, got %f", got)
	}
}

func TestCosineSimilarity_OrthogonalVectors(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	got := cosineSimilarity(a, b)
	if math.Abs(float64(got)) > 1e-5 {
		t.Errorf("orthogonal vectors should give 0, got %f", got)
	}
}

func TestCosineSimilarity_OppositeVectors(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{-1, 0, 0}
	got := cosineSimilarity(a, b)
	if math.Abs(float64(got-(-1.0))) > 1e-5 {
		t.Errorf("opposite vectors should give -1.0, got %f", got)
	}
}

func TestCosineSimilarity_ZeroVectors(t *testing.T) {
	if got := cosineSimilarity(nil, []float32{1, 2}); got != 0 {
		t.Errorf("nil first vector should give 0, got %f", got)
	}
	if got := cosineSimilarity([]float32{1, 2}, nil); got != 0 {
		t.Errorf("nil second vector should give 0, got %f", got)
	}
	if got := cosineSimilarity([]float32{0, 0}, []float32{0, 0}); got != 0 {
		t.Errorf("zero vectors should give 0, got %f", got)
	}
}

func TestCosineSimilarity_DifferentLengths(t *testing.T) {
	// Shorter vector's overlap is what matters; the rest is
	// ignored. Conservative — underestimates true similarity.
	a := []float32{1, 0, 0, 99} // 4-dim
	b := []float32{1, 0, 0}     // 3-dim
	got := cosineSimilarity(a, b)
	if math.Abs(float64(got-1.0)) > 1e-5 {
		t.Errorf("3-dim overlap of identical prefix should give 1.0, got %f", got)
	}
}

func TestCosineSimilarity_Realistic(t *testing.T) {
	// Two purpose statements that say roughly the same thing
	// should score above 0.85. Two that differ should score
	// below 0.7.
	similar1 := []float32{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8}
	similar2 := []float32{0.11, 0.21, 0.31, 0.41, 0.51, 0.61, 0.71, 0.81}
	different := []float32{-0.7, 0.3, -0.2, 0.9, -0.5, 0.1, 0.8, -0.4}

	if sim := cosineSimilarity(similar1, similar2); sim < 0.99 {
		t.Errorf("near-identical should score >= 0.99, got %f", sim)
	}
	if sim := cosineSimilarity(similar1, different); sim > 0.5 {
		t.Errorf("orthogonal-mix should score <= 0.5, got %f", sim)
	}
}

func TestFakeDedup_ReturnsCanned(t *testing.T) {
	fake := &FakeDedup{
		Result: DedupResult{Matched: true, MatchedID: "cap_x", MatchedName: "x", Similarity: 0.95},
	}
	res, err := fake.Check(context.Background(), "purpose", []string{"skip_me"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Matched || res.MatchedID != "cap_x" {
		t.Errorf("result wrong: %+v", res)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fake.Calls))
	}
	if fake.Calls[0].Purpose != "purpose" {
		t.Errorf("recorded purpose wrong: %q", fake.Calls[0].Purpose)
	}
	if len(fake.Calls[0].SkipIDs) != 1 || fake.Calls[0].SkipIDs[0] != "skip_me" {
		t.Errorf("recorded skip list wrong: %v", fake.Calls[0].SkipIDs)
	}
}

func TestFakeDedup_ReturnsCannedError(t *testing.T) {
	fake := &FakeDedup{Error: context.DeadlineExceeded}
	_, err := fake.Check(context.Background(), "purpose", nil)
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestFakeDedup_SkipListIsolation(t *testing.T) {
	// The Forge should not see the caller's slice mutated; the
	// fake copies it on entry. This guards against the Forge
	// caching a slice that the caller reuses.
	fake := &FakeDedup{}
	skip := []string{"a", "b", "c"}
	_, _ = fake.Check(context.Background(), "p", skip)
	// Mutate the caller's slice; the recorded copy must be intact.
	skip[0] = "MUTATED"
	if fake.Calls[0].SkipIDs[0] != "a" {
		t.Errorf("fake recorded slice was not isolated: %v", fake.Calls[0].SkipIDs)
	}
}

func TestDefaultDedup_NilStore(t *testing.T) {
	d := NewDefaultDedup(nil, nil, 0)
	_, err := d.Check(context.Background(), "purpose", nil)
	if err == nil {
		t.Error("expected error with nil store")
	}
}

func TestDefaultDedup_EmptyPurpose(t *testing.T) {
	store := &Store{} // we never get to query because purpose check fails first
	d := NewDefaultDedup(store, nil, 0)
	_, err := d.Check(context.Background(), "", nil)
	if err == nil {
		t.Error("expected error with empty purpose")
	}
}

func TestDefaultDedup_EmptyEmbedderReturnsNoMatch(t *testing.T) {
	// An embedder that returns nil simulates a failed embed.
	// The dedup must not panic; it must report no match (better
	// to over-accept than to block on a broken embed service).
	store := &Store{} // ListNonRetiredEmbeddings will fail; but we test
	// the early-return path with a non-empty purpose and nil embed
	// result.
	d := NewDefaultDedup(store, func(string) []float32 { return nil }, 0.92)
	res, err := d.Check(context.Background(), "purpose", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Matched {
		t.Errorf("nil embedder should not match, got %+v", res)
	}
}

func TestDefaultDedup_SkipsEmptyEmbeddings(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Seed two capabilities: one with no embedding, one with a
	// near-identical embedding. The empty-embedding row must be
	// skipped; the populated row should match.
	seedCapabilityWithEmbedding(t, db, "cap_empty", "no_embed", []float32{})
	seedCapabilityWithEmbedding(t, db, "cap_match", "matcher", []float32{0.1, 0.2, 0.3, 0.4})

	// Use a controlled embedder that returns a vector very
	// similar to cap_match's stored embedding.
	embedder := func(_ string) []float32 {
		return []float32{0.1, 0.2, 0.3, 0.4}
	}
	d := NewDefaultDedup(store, embedder, 0.92)

	res, err := d.Check(context.Background(), "any purpose", nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Matched {
		t.Fatalf("expected match against cap_match, got no match")
	}
	if res.MatchedID != "cap_match" {
		t.Errorf("matched wrong id: %q", res.MatchedID)
	}
	if res.Similarity < 0.99 {
		t.Errorf("similarity should be ~1.0, got %f", res.Similarity)
	}
}

func TestDefaultDedup_SkipsIDsInSkipList(t *testing.T) {
	store, db, _ := newTestStore(t)

	seedCapabilityWithEmbedding(t, db, "cap_a", "alpha", []float32{1, 0, 0})
	seedCapabilityWithEmbedding(t, db, "cap_b", "beta", []float32{0, 1, 0})

	// Embedder returns (1, 0, 0) — matches cap_a perfectly.
	embedder := func(_ string) []float32 { return []float32{1, 0, 0} }
	d := NewDefaultDedup(store, embedder, 0.92)

	// Without skip: matches cap_a.
	res, _ := d.Check(context.Background(), "p", nil)
	if res.MatchedID != "cap_a" {
		t.Errorf("expected cap_a, got %q", res.MatchedID)
	}

	// With skip list containing cap_a: no match (cap_b is
	// orthogonal; cap_a is excluded).
	res, _ = d.Check(context.Background(), "p", []string{"cap_a"})
	if res.Matched {
		t.Errorf("expected no match with cap_a in skip list, got: %+v", res)
	}
}

func TestDefaultDedup_BelowThresholdIsNotAMatch(t *testing.T) {
	store, db, _ := newTestStore(t)

	seedCapabilityWithEmbedding(t, db, "cap_x", "x", []float32{1, 0, 0})

	// Orthogonal embedder — similarity 0, well below 0.92.
	embedder := func(_ string) []float32 { return []float32{0, 1, 0} }
	d := NewDefaultDedup(store, embedder, 0.92)

	res, err := d.Check(context.Background(), "p", nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Matched {
		t.Errorf("orthogonal vector should not match, got: %+v", res)
	}
}

func TestDefaultDedup_IgnoresRetiredAndRolledBack(t *testing.T) {
	store, db, _ := newTestStore(t)

	// A retired capability with a near-identical embedding
	// should be excluded from the dedup pool. So should a
	// rolled-back one. The agent is free to propose a new
	// capability with the same purpose as one we've already
	// marked dead.
	seedCapabilityWithEmbedding(t, db, "cap_dead", "dead", []float32{1, 0, 0})
	seedCapabilityWithEmbedding(t, db, "cap_rolled", "rolled", []float32{1, 0, 0})
	if _, err := db.Exec(`UPDATE capabilities SET state='retired' WHERE id='cap_dead'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE capabilities SET state='rolled_back' WHERE id='cap_rolled'`); err != nil {
		t.Fatal(err)
	}

	embedder := func(_ string) []float32 { return []float32{1, 0, 0} }
	d := NewDefaultDedup(store, embedder, 0.92)

	res, _ := d.Check(context.Background(), "p", nil)
	if res.Matched {
		t.Errorf("retired/rolled_back capabilities should be ignored, got: %+v", res)
	}
}

func TestDefaultDedup_HandlesCorruptEmbedding(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Insert a row whose embedding is "not json" — simulates a
	// pre-migration or corrupt row. The dedup must skip it
	// (not crash, not block the proposal).
	seedCapabilityWithRawEmbedding(t, db, "cap_corrupt", "corrupt", []byte("not-json{"))
	seedCapabilityWithEmbedding(t, db, "cap_good", "good", []float32{1, 0, 0})

	embedder := func(_ string) []float32 { return []float32{1, 0, 0} }
	d := NewDefaultDedup(store, embedder, 0.92)

	res, err := d.Check(context.Background(), "p", nil)
	if err != nil {
		t.Fatalf("Check should not error on corrupt row, got: %v", err)
	}
	if !res.Matched || res.MatchedID != "cap_good" {
		t.Errorf("expected match against cap_good (corrupt row skipped), got: %+v", res)
	}
}

// TestJSONRoundTripEmbedding checks that a []float32 round-trips
// through json.Marshal/Unmarshal with no precision loss at the
// magnitudes we use (HashEmbed scales to 0..1; Ollama returns
// normalised vectors). Drift would be a false-negative risk in
// the cosine comparison.
func TestJSONRoundTripEmbedding(t *testing.T) {
	src := []float32{0.1, -0.2, 0.3, -0.4, 0.5}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var dst []float32
	if err := json.Unmarshal(raw, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dst) != len(src) {
		t.Fatalf("length drift: %d -> %d", len(src), len(dst))
	}
	for i := range src {
		if math.Abs(float64(src[i]-dst[i])) > 1e-6 {
			t.Errorf("index %d: %f -> %f", i, src[i], dst[i])
		}
	}
}

// =============================================================================
// test helpers for integration tests
// =============================================================================

// seedCapabilityWithEmbedding inserts a capability row with a
// controlled embedding. Used by the dedup integration tests.
func seedCapabilityWithEmbedding(t *testing.T, db *sql.DB, id, name string, emb []float32) {
	t.Helper()
	raw, err := json.Marshal(emb)
	if err != nil {
		t.Fatalf("marshal embedding: %v", err)
	}
	seedCapabilityWithRawEmbedding(t, db, id, name, raw)
}

// seedCapabilityWithRawEmbedding inserts a capability row with
// raw embedding bytes. Used to test corrupt-embedding handling.
func seedCapabilityWithRawEmbedding(t *testing.T, db *sql.DB, id, name string, emb []byte) {
	t.Helper()
	q := `INSERT INTO capabilities
	      (id, name, purpose, source_code, source_language, source_hash,
	       state, execution_domain, state_changed_at,
	       author_agent, created_from_id,
	       probation_required_success_count, probation_max_failure_rate,
	       tags, created_at, updated_at, metadata, embedding)
	      VALUES (?, ?, 'test', 'echo', 'bash', 'deadbeef',
	              'active', 'sandbox', 1700000000,
	              '', NULL,
	              5, 0.10,
	              '[]', 1700000000, 1700000000, '{}', ?)`
	if _, err := db.Exec(q, id, name, string(emb)); err != nil {
		t.Fatalf("seedCapabilityWithRawEmbedding: %v", err)
	}
}
