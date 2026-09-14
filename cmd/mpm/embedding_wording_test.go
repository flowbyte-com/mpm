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
