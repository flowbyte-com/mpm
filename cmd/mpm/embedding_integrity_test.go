package main

import (
	"fmt"
	"strings"
	"testing"
)

// Regression tests for the embedding COVERAGE / INTEGRITY split.
//
// The defect these pin: checkEmbeddings measures coverage against a
// ">10% nulls" tolerance and reports on the `embedding_source` LABEL
// alone. It never opened a vector. So a database where every row is
// labelled `provider` but the payloads are empty, truncated, non-finite
// or of mixed dimensions reported PASS.
//
// Every test here uses an isolated in-memory DatabaseManager; nothing
// touches the operator's live database.

// newIntegrityFixture builds a doctor service over a fresh isolated DB.
// Uses the same in-package fixture as doctor_embedding_test.go, so these
// tests never touch the operator's live database.
func newIntegrityFixture(t *testing.T) *DoctorService {
	t.Helper()
	return NewDoctorService(newTestDMForCmd(t))
}

// idFor builds a deterministic, syntactically valid row id per test.
func idFor(tag string, i int) string {
	return fmt.Sprintf("%s-%04d-0000-0000-000000000000", tag, i)
}

func TestEmbeddingIntegrity_CleanStoreIsPass(t *testing.T) {
	svc := newIntegrityFixture(t)

	for i := 0; i < 5; i++ {
		if _, err := svc.dm.ExecTracked(
			`INSERT INTO memories (id, collection, content, embedding, created_at, embedding_source, embedding_dimension)
			 VALUES (?, 'test', ?, ?, ?, 'provider', 384)`,
			0, idFor("clean", i), "content", `[0.1,0.2,0.3]`, int64(1700000000+i),
		); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}

	check := svc.checkEmbeddingIntegrity()
	if check.Status != "PASS" {
		t.Fatalf("clean store: got %s (%s) details=%v, want PASS",
			check.Status, check.Message, check.Details)
	}
}

// A null vector must be surfaced by the INTEGRITY check regardless of how
// small it is. This is the specific case the brief calls out: a null
// vector must not be classified as healthy merely because it falls below
// a statistical tolerance. One null out of 200 rows is far below any
// percentage tolerance and must still WARN here.
func TestEmbeddingIntegrity_SingleNullStillWarns(t *testing.T) {
	svc := newIntegrityFixture(t)

	for i := 0; i < 199; i++ {
		if _, err := svc.dm.ExecTracked(
			`INSERT INTO memories (id, collection, content, embedding, created_at, embedding_source, embedding_dimension)
			 VALUES (?, 'test', ?, ?, ?, 'provider', 384)`,
			0, idFor("bulk", i), "content", `[0.1,0.2,0.3]`, int64(1700000000+i),
		); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
	// One null row, 0.5% of the store — below any tolerance.
	if _, err := svc.dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, embedding, created_at, embedding_source)
		 VALUES (?, 'test', ?, 'null', ?, 'null')`,
		0, idFor("null", 0), "content", int64(1700000000),
	); err != nil {
		t.Fatalf("seed null memory: %v", err)
	}

	// The COVERAGE check tolerates it (0.5% < 10%)...
	coverage := svc.checkEmbeddings()
	// ...but INTEGRITY must not call it healthy.
	integrity := svc.checkEmbeddingIntegrity()

	if integrity.Status == "PASS" {
		t.Errorf("integrity reported PASS with a null vector present: %+v", integrity)
	}
	if integrity.Status != "WARN" {
		t.Errorf("integrity: got %s, want WARN", integrity.Status)
	}
	if !mentionsAny(integrity.Details, "no vector", "null") {
		t.Errorf("integrity details do not mention the null vector: %v", integrity.Details)
	}
	// Coverage must NOT have failed — that is the deliberate asymmetry.
	if coverage.Status == "FAIL" {
		t.Errorf("coverage unexpectedly FAILed on 0.5%% nulls: %s", coverage.Message)
	}
}

// Mixed dimensions are the integrity failure that matters most: they
// make cosine similarity across the store meaningless while every row
// still looks provider-generated.
func TestEmbeddingIntegrity_MixedDimensionsFail(t *testing.T) {
	svc := newIntegrityFixture(t)

	for i, dim := range []int{3, 3, 5} {
		vec := "[0.1,0.2,0.3]"
		if dim == 5 {
			vec = "[0.1,0.2,0.3,0.4,0.5]"
		}
		if _, err := svc.dm.ExecTracked(
			`INSERT INTO memories (id, collection, content, embedding, created_at, embedding_source, embedding_dimension)
			 VALUES (?, 'test', ?, ?, ?, 'provider', ?)`,
			0, idFor("mixed", i), "content", vec, int64(1700000000+i), dim,
		); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}

	check := svc.checkEmbeddingIntegrity()
	if check.Status != "FAIL" {
		t.Fatalf("mixed dimensions: got %s (%s), want FAIL", check.Status, check.Message)
	}
	if !mentionsAny(check.Details, "Mixed vector dimensions") {
		t.Errorf("details do not surface the dimension split: %v", check.Details)
	}
	// Crucially: the coverage check still calls this healthy, because it
	// only counts labels. That gap is the reason this check exists.
	if svc.checkEmbeddings().Status == "FAIL" {
		t.Errorf("coverage unexpectedly FAILed — it cannot see dimensions, " +
			"which is precisely the point of the new check")
	}
}

func TestEmbeddingIntegrity_MalformedPayloadFails(t *testing.T) {
	svc := newIntegrityFixture(t)

	cases := map[string]string{
		"truncated":      `[0.1,0.2,`,
		"not_an_array":   `{"vector":[0.1,0.2]}`,
		"empty_array":    `[]`,
		"non_numeric":    `["a","b"]`,
		"string_literal": `"hello"`,
	}
	i := 0
	for name, body := range cases {
		if _, err := svc.dm.ExecTracked(
			`INSERT INTO memories (id, collection, content, embedding, created_at, embedding_source, embedding_dimension)
			 VALUES (?, 'test', ?, ?, ?, 'provider', 384)`,
			0, idFor("malformed-"+name, 0), "content", body, int64(1700000000+i),
		); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		i++
	}

	check := svc.checkEmbeddingIntegrity()
	if check.Status != "FAIL" {
		t.Fatalf("malformed payloads: got %s (%s), want FAIL", check.Status, check.Message)
	}
	// Coverage is blind to all of these — every row claims 'provider'.
	if svc.checkEmbeddings().Status == "FAIL" {
		t.Errorf("coverage unexpectedly FAILed on malformed-but-provider-labelled rows")
	}
}

// Non-finite components: JSON cannot encode NaN, so these cannot arise
// through the normal write path — but the check must still catch them if
// a row is ever written by another route. (db.go:3664 ignores the
// json.Marshal error, which would itself produce "null".)
func TestEmbeddingIntegrity_NonFiniteIsDetected(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"nan", `NaN`},
		{"inf", `[Infinity]`},
		{"neg_inf", `[-Infinity]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dim, malformed, empty, nonFinite := classifyEmbeddingPayload(tc.body)
			if !(malformed || nonFinite || empty) {
				t.Errorf("classifyEmbeddingPayload(%q) returned dim=%d and flagged "+
					"nothing; a non-finite payload must never validate cleanly",
					tc.body, dim)
			}
		})
	}

	// And the well-formed control still passes.
	dim, malformed, empty, nonFinite := classifyEmbeddingPayload(`[0.1,0.2,0.3]`)
	if dim != 3 || malformed || empty || nonFinite {
		t.Errorf("well-formed vector misclassified: dim=%d malformed=%v empty=%v nonFinite=%v",
			dim, malformed, empty, nonFinite)
	}
}

func TestEmbeddingIntegrity_ClassifyNullAndEmpty(t *testing.T) {
	// A literal "null" is an ABSENT vector, not a malformed one.
	dim, malformed, empty, nonFinite := classifyEmbeddingPayload("null")
	if dim != 0 || malformed || empty || nonFinite {
		t.Errorf("\"null\": dim=%d malformed=%v empty=%v nonFinite=%v — want all zero/false",
			dim, malformed, empty, nonFinite)
	}
	// Same for an empty/NULL body.
	for _, body := range []string{"", "   "} {
		if _, m, e, n := classifyEmbeddingPayload(body); m || e || n {
			t.Errorf("empty body %q flagged malformed=%v empty=%v nonFinite=%v", body, m, e, n)
		}
	}
	// A decodable but zero-length vector IS empty.
	if _, _, empty, _ := classifyEmbeddingPayload(`[]`); !empty {
		t.Error("`[]` should classify as empty")
	}
}

// Legacy memory embeddings have no recoverable model identity. The check
// must say so explicitly rather than implying the vectors are attributable.
func TestEmbeddingIntegrity_ReportsUnrecoverableMemoryFingerprint(t *testing.T) {
	svc := newIntegrityFixture(t)
	if _, err := svc.dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, embedding, created_at, embedding_source, embedding_dimension)
		 VALUES (?, 'test', ?, ?, ?, 'provider', 384)`,
		0, idFor("legacy", 0), "content", `[0.1,0.2,0.3]`, int64(1700000000),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	check := svc.checkEmbeddingIntegrity()
	joined := strings.Join(check.Details, "\n")
	if !strings.Contains(joined, "NOT recoverable") {
		t.Errorf("expected an explicit statement that memory-vector model identity is "+
			"unrecoverable, got:\n%s", joined)
	}
	if !strings.Contains(joined, "no `embedding_model` column") &&
		!strings.Contains(joined, "embedding_model column") {
		t.Errorf("expected the reason (missing embedding_model column), got:\n%s", joined)
	}
}

// Provider-agnostic: the check must not name a vendor or recommend a
// model, per the v0.1-final decision to remove catalogues and the probe.
func TestEmbeddingIntegrity_IsProviderAgnostic(t *testing.T) {
	svc := newIntegrityFixture(t)
	if _, err := svc.dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, embedding, created_at, embedding_source, embedding_dimension)
		 VALUES (?, 'test', ?, ?, ?, 'provider', 384)`,
		0, idFor("agnostic", 0), "content", `[0.1,0.2,0.3]`, int64(1700000000),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	check := svc.checkEmbeddingIntegrity()
	text := strings.ToLower(check.Message + "\n" + strings.Join(check.Details, "\n"))
	for _, banned := range []string{
		"ollama", "openai", "anthropic", "minilm", "nomic", "bge",
		"gte-", "e5-", "qwen", "sentence-transformers", "we recommend",
		"recommended model",
	} {
		if strings.Contains(text, banned) {
			t.Errorf("integrity check names provider/model %q — it must stay "+
				"provider-agnostic:\n%s", banned, text)
		}
	}
}

// Reference chunks DO carry a fingerprint. A split across two models
// means references were embedded by different generations.
func TestEmbeddingIntegrity_ReferenceFingerprintSplitFails(t *testing.T) {
	svc := newIntegrityFixture(t)
	docID := idFor("refdoc", 0)
	if _, err := svc.dm.ExecTracked(
		`INSERT INTO reference_docs (id, title) VALUES (?, ?)`,
		0, docID, "t", "s",
	); err != nil {
		t.Skipf("reference_docs schema unavailable: %v", err)
	}

	for i, model := range []string{"model-a", "model-b"} {
		if _, err := svc.dm.ExecTracked(
			`INSERT INTO reference_chunks (id, doc_id, chunk_index, content, embedding, embedding_model, embedding_dimension)
			 VALUES (?, ?, ?, ?, ?, ?, 384)`,
			0, idFor("chunk", i), docID, i, "chunk text", `[0.1,0.2,0.3]`, model,
		); err != nil {
			t.Skipf("reference_chunks schema unavailable: %v", err)
		}
	}

	check := svc.checkEmbeddingIntegrity()
	if check.Status != "FAIL" {
		t.Fatalf("reference fingerprint split: got %s, want FAIL", check.Status)
	}
	if !mentionsAny(check.Details, "distinct embedding models") {
		t.Errorf("details do not surface the model split: %v", check.Details)
	}
}

// An empty store is INFO, not FAIL — absence of an optional feature must
// not make the substrate look unhealthy (same contract as the existing
// EmbeddingProvider check).
func TestEmbeddingIntegrity_EmptyStoreIsInformational(t *testing.T) {
	svc := newIntegrityFixture(t)
	check := svc.checkEmbeddingIntegrity()
	if check.Status != "INFO" {
		t.Fatalf("empty store: got %s (%s), want INFO", check.Status, check.Message)
	}
}

// The new check must be registered so it actually runs in `mpm doctor`.
func TestDoctorRunsEmbeddingIntegrityCheck(t *testing.T) {
	svc := newIntegrityFixture(t)
	report, _ := svc.Check()

	found := false
	for _, c := range report.Checks {
		if c.Name == "Embedding integrity" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(report.Checks))
		for _, c := range report.Checks {
			names = append(names, c.Name)
		}
		t.Fatalf("doctor does not run the Embedding integrity check; ran: %v", names)
	}
}

// mentionsAny reports whether any needle appears in haystack.
func mentionsAny(haystack []string, needles ...string) bool {
	joined := strings.ToLower(strings.Join(haystack, "\n"))
	for _, n := range needles {
		if strings.Contains(joined, strings.ToLower(n)) {
			return true
		}
	}
	return false
}
