// embedding_partial_migration_test.go — pin the diagnostics for a
// PARTIALLY migrated embedding store (Hermes review, priority 3).
//
// # The defect these tests pin
//
// Retrieval intentionally accepts only vectors attributable to the
// active embedding model. That safety rule is correct: two models can
// share a dimension while producing incomparable vector spaces, and a
// cosine over unrelated spaces returns a plausible number that can even
// trigger a memory challenge. Unlabelled (pre-migration) rows are
// excluded for the same reason.
//
// The gap was observability. `VectorMatch` warned only when ZERO rows
// were labelled, so a half-migrated database — some rows compatible,
// the rest silently excluded — ranked a subset of the corpus with no
// signal at all. `mpm doctor` reported unlabelled rows but not the
// split against the ACTIVE model, and it never said which retrieval
// path would actually run.
//
// These tests pin the doctor surface: compatible / incompatible /
// unlabelled counts against the active fingerprint, an explicit
// retrieval mode (IVF vs brute force) and actionable remediation.
package main

import (
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// fixedModelProvider is a provider stub whose only job is to give the
// active-embedding fingerprint a stable, test-chosen model name. It
// never performs inference.
type fixedModelProvider struct{ name string }

func (p fixedModelProvider) Embed(string) ([]float32, error) { return nil, nil }
func (p fixedModelProvider) Name() string                    { return p.name }

// withActiveEmbeddingModel installs a test embedding config carrying
// the given model name and restores the previous config afterwards.
func withActiveEmbeddingModel(t *testing.T, model string) {
	t.Helper()
	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Provider:     fixedModelProvider{name: model},
		ProviderName: model,
	})
	t.Cleanup(func() {
		if prev != nil {
			mpminternal.SetEmbedConfigForTest(prev)
			return
		}
		mpminternal.ResetEmbedConfigForTest()
	})
}

// seedEmbeddedMemory inserts one live memory row with the given
// fingerprint (empty string = unlabelled). body lets a caller seed a
// vectorless row.
func seedEmbeddedMemory(t *testing.T, svc *DoctorService, tag string, i int, body, model string) {
	t.Helper()
	var modelArg interface{}
	if model != "" {
		modelArg = model
	}
	if _, err := svc.dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, embedding, created_at,
		                       embedding_source, embedding_dimension, embedding_model)
		 VALUES (?, 'memories', ?, ?, ?, 'provider', 384, ?)`,
		0, idFor(tag, i), fmt.Sprintf("content %s %d", tag, i), body,
		int64(1700000000+i), modelArg,
	); err != nil {
		t.Fatalf("seed %s-%d: %v", tag, i, err)
	}
}

// TestEmbeddingIntegrity_PartialMigrationWarnsWithSplit pins the core
// observability fix: a store with compatible rows AND excluded rows is
// WARNed, with the exact split reported.
func TestEmbeddingIntegrity_PartialMigrationWarnsWithSplit(t *testing.T) {
	svc := newIntegrityFixture(t)
	withActiveEmbeddingModel(t, "model-a")

	// 3 compatible, 2 embedded by another model, 1 real vector with no
	// fingerprint, 1 row with no vector at all (the literal null body —
	// it belongs to the coverage/integrity disclosure, not this split).
	for i := 0; i < 3; i++ {
		seedEmbeddedMemory(t, svc, "compatible", i, `[0.1,0.2,0.3]`, "model-a")
	}
	for i := 0; i < 2; i++ {
		seedEmbeddedMemory(t, svc, "othermodel", i, `[0.1,0.2,0.3]`, "model-b")
	}
	seedEmbeddedMemory(t, svc, "unlabelled", 0, `[0.1,0.2,0.3]`, "")
	seedEmbeddedMemory(t, svc, "nullbody", 0, `null`, "")

	check := svc.checkEmbeddingIntegrity()
	joined := strings.Join(check.Details, "\n")

	if check.Status != "WARN" {
		t.Errorf("partial migration: status = %s (%s), want WARN", check.Status, check.Message)
	}
	for _, want := range []string{"3 of 6", "2 were embedded by a different model", "1 carry no fingerprint", "model-a", "backfill-embeddings"} {
		if !strings.Contains(joined, want) {
			t.Errorf("details missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(check.Message, "3 of 6") {
		t.Errorf("message does not state the excluded share: %q", check.Message)
	}

	// The retrieval mode must be stated, and with no clusters it is the
	// brute-force fallback with a remediation pointer.
	if !strings.Contains(joined, "brute force") || !strings.Contains(joined, "mpm ops rebalance") {
		t.Errorf("details must report the actual retrieval mode and its remedy:\n%s", joined)
	}
}

// TestEmbeddingIntegrity_FullyCompatibleStorePasses guards against
// over-warning: a fully migrated, fully compatible store must still
// PASS.
func TestEmbeddingIntegrity_FullyCompatibleStorePasses(t *testing.T) {
	svc := newIntegrityFixture(t)
	withActiveEmbeddingModel(t, "model-a")

	for i := 0; i < 3; i++ {
		seedEmbeddedMemory(t, svc, "compatible", i, `[0.1,0.2,0.3]`, "model-a")
	}

	check := svc.checkEmbeddingIntegrity()
	if check.Status != "PASS" {
		t.Errorf("fully compatible store: status = %s (%s), want PASS; details:\n%s",
			check.Status, check.Message, strings.Join(check.Details, "\n"))
	}
}

// TestEmbeddingIntegrity_ReportsIVFModeWhenClustersExist pins the
// retrieval-mode report on the other side: once clusters exist, the
// check must say IVF rather than brute force.
func TestEmbeddingIntegrity_ReportsIVFModeWhenClustersExist(t *testing.T) {
	svc := newIntegrityFixture(t)
	withActiveEmbeddingModel(t, "model-a")

	seedEmbeddedMemory(t, svc, "compatible", 0, `[0.1,0.2,0.3]`, "model-a")
	if _, err := svc.dm.ExecTracked(
		`INSERT INTO vector_clusters (cluster_id, centroid, n_vectors, variance)
		 VALUES (1, ?, 1, 0.0)`, 0, []byte{0, 0, 128, 63},
	); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	check := svc.checkEmbeddingIntegrity()
	joined := strings.Join(check.Details, "\n")
	if !strings.Contains(joined, "IVF (1 clusters") {
		t.Errorf("details do not report IVF mode with one cluster:\n%s", joined)
	}
	if strings.Contains(joined, "IVF index holds no clusters") {
		t.Errorf("details still claim the IVF index is empty:\n%s", joined)
	}
	if !strings.Contains(joined, "scan cap") {
		t.Errorf("details do not report the vector scan cap:\n%s", joined)
	}
}

// TestEmbeddingIntegrity_NoActiveModelIsExplicit pins the honest
// wording when no embedding model is configured: no stored vector can
// be attributed to the query model, and the remedy is configuration,
// not silent comparison.
func TestEmbeddingIntegrity_NoActiveModelIsExplicit(t *testing.T) {
	svc := newIntegrityFixture(t)
	withActiveEmbeddingModel(t, "") // null provider

	seedEmbeddedMemory(t, svc, "labelled", 0, `[0.1,0.2,0.3]`, "model-a")

	check := svc.checkEmbeddingIntegrity()
	joined := strings.Join(check.Details, "\n")
	if !strings.Contains(joined, "No embedding model is configured") {
		t.Errorf("details must state that no embedding model is configured:\n%s", joined)
	}
	if !strings.Contains(joined, "mpm ops backfill-embeddings") {
		t.Errorf("details must name the remediation:\n%s", joined)
	}
}
