// embedding_wording_test.go — Regression coverage for the
// 2026-09-14 release-pass embedding-as-optional semantics.
//
// Embedding absence is informational (not a defect). The Doctor
// check uses the neutral "INFO" status (rendered as `○`) so the
// tally does not increment warning counts. Only configured-but-
// unreachable and configured-but-misconfigured states remain WARN.

package main

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestEmbedding_AbsentIsInformational asserts that the Doctor
// Embedding provider check returns INFO (not WARN) when the
// embedding source is absent. The rendered marker must be the
// neutral `○`, not `✓` (which would imply a successful check).
func TestEmbedding_AbsentIsInformational(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source:       mpminternal.EmbeddingSourceAbsent,
		ProviderName: "null",
		Status:       mpminternal.EmbeddingStatusNull,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	check := svc.checkEmbeddingProvider()
	if check.Status != "INFO" {
		t.Fatalf("absent embedding must be INFO; got %q (msg: %q)", check.Status, check.Message)
	}
	if check.Message != "not configured · optional" {
		t.Fatalf("absent embedding message: got %q, want %q", check.Message, "not configured · optional")
	}
	// Details list should describe the verified degradation
	// (lexical and structured retrieval remain available).
	if len(check.Details) == 0 {
		t.Fatalf("absent embedding should carry a degradation hint in Details")
	}
}

// TestEmbedding_AbsentDoesNotIncrementWarnings asserts that
// the doctor tally for an absent-embedding state does NOT
// increment warning or fail counts. This pins the release-pass
// invariant: embedding absence alone is not a defect.
func TestEmbedding_AbsentDoesNotIncrementWarnings(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source: mpminternal.EmbeddingSourceAbsent,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	report, err := svc.Check()
	if err != nil {
		t.Fatalf("Check() returned error: %v", err)
	}
	if report.Warnings != 0 {
		t.Fatalf("absent embedding must not increment warnings; got %d (informational should be the only non-pass count)", report.Warnings)
	}
	if report.Failed != 0 {
		t.Fatalf("absent embedding must not increment failures; got %d", report.Failed)
	}
	// Informational should be at least 1 (the absent embedding check).
	if report.Informational < 1 {
		t.Fatalf("absent embedding must increment Informational count; got %d", report.Informational)
	}
}

// TestEmbedding_DegradationTextVerified asserts the degradation
// hint surfaced by Doctor / dashboard does NOT make broad claims
// (e.g. "lexical and structured retrieval remain available")
// unless verified. The verified-fallback text is pinned here so
// any drift is caught at test time.
func TestEmbedding_DegradationTextVerified(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source: mpminternal.EmbeddingSourceAbsent,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	check := svc.checkEmbeddingProvider()
	if len(check.Details) == 0 {
		t.Fatalf("expected at least one Detail line; got none")
	}
	hint := strings.Join(check.Details, " ")
	// The pinned hint must mention the verified fallback capabilities.
	// Embedding absence loses semantic / vector retrieval; lexical
	// (FTS5) and structured (SQL) retrieval remain available — both
	// are present in the substrate (see internal/core/memory.go).
	if !strings.Contains(hint, "semantic") || !strings.Contains(hint, "lexical") {
		t.Fatalf("degradation hint must mention what is lost (semantic) and what remains (lexical); got %q", hint)
	}
}

// TestEmbedding_ConfiguredRendersModelDotProvider asserts that a
// configured embedding surfaces the canonical `<model> · <Provider>`
// reformat, NOT the misleading `model=<host>:<model>` form (where
// the host name is mistaken for the model name) and NOT the legacy
// `provider "<host>:<model>" reachable` form (which mixes the wire
// shape into the user-facing string).
//
// 2026-09-14 release-pass: presentation reformats the canonical
// `<host>:<model>` wire shape into the human-facing
// `<model> · <Provider>` form across doctor and readiness. The
// underlying wire shape is unchanged.
func TestEmbedding_ConfiguredRendersModelDotProvider(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source:       mpminternal.EmbeddingSourceProfile,
		ProfileName:  "local-ollama",
		ProviderName: "ollama:nomic-embed-text",
		Status:       mpminternal.EmbeddingStatusConfigured,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	// Doctor: check message must be the human-facing
	// `<model> · <Provider>` form, not the wire shape.
	check := svc.checkEmbeddingProvider()
	if check.Status != "PASS" {
		t.Fatalf("configured embedding check status = %q; want PASS (msg: %q)", check.Status, check.Message)
	}
	if check.Message != "nomic-embed-text · Ollama" {
		t.Fatalf("configured embedding message = %q; want %q", check.Message, "nomic-embed-text · Ollama")
	}

	// Readiness: detail must use the same `<model> · <Provider>`
	// form. OK=true (configured + reachable is informational-ready).
	item := checkEmbeddings()
	if !item.OK {
		t.Fatalf("configured+reachable readiness OK = false; want true (detail: %q)", item.Detail)
	}
	if item.Detail != "nomic-embed-text · Ollama" {
		t.Fatalf("configured readiness detail = %q; want %q", item.Detail, "nomic-embed-text · Ollama")
	}
}

// TestEmbedding_AbsentRowLabel pins the canonical row label across
// doctor and readiness surfaces. The label was renamed from
// "Embedding provider" → "Embedding model" for the 2026-09-14
// release-pass so the user-facing terminology distinguishes the
// model identity from the provider identity (the two are reported
// together in the configured form).
func TestEmbedding_AbsentRowLabel(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source: mpminternal.EmbeddingSourceAbsent,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	// Doctor: row label must be "Embedding model".
	check := svc.checkEmbeddingProvider()
	if check.Name != "Embedding model" {
		t.Fatalf("doctor embedding row name = %q; want %q", check.Name, "Embedding model")
	}

	// Readiness: row label must be "Embedding model".
	item := checkEmbeddings()
	if item.Name != "Embedding model" {
		t.Fatalf("readiness embedding row name = %q; want %q", item.Name, "Embedding model")
	}
}
