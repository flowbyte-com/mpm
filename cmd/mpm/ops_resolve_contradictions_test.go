// ops_resolve_contradictions_test.go — CLI-level coverage for the issue #10
// adjudication gate.
//
// The behaviour that matters for #10 is not only in internal/core: it is in
// what this command TELLS the operator. A row refused for want of
// adjudication has a known next step, and the two ways to get that wrong are
// to hide the row (silently discard a pending candidate) or to bury it (print
// one error line per row, so a 100-row candidate queue drowns any real
// failure). Both are tested here.
//
// captureStdout lives in f8_f10_regression_test.go; these tests are in the
// same package and reuse it rather than adding a second copy.
package main

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestParseAdjudicationFlag_PinsTheMechanism covers the parsing contract of
// --adjudicate=ID:REASON.
//
// The reason is required because it is the audit trail: an "adjudicated"
// marker with nothing recorded against it says only that something happened,
// which does not distinguish a considered decision from a reflex. The id must
// be a queue row id, which is why "mem-abc123:..." is rejected — passing a
// memory id here is the most likely operator mistake.
func TestParseAdjudicationFlag_PinsTheMechanism(t *testing.T) {
	t.Run("accepts id and reason", func(t *testing.T) {
		got, err := parseAdjudicationFlag("42:mem-a claims X, mem-b claims not-X")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.queueID != 42 {
			t.Errorf("queueID = %d, want 42", got.queueID)
		}
		if got.reason != "mem-a claims X, mem-b claims not-X" {
			t.Errorf("reason = %q", got.reason)
		}
	})

	t.Run("splits on the first colon so a reason may contain colons", func(t *testing.T) {
		got, err := parseAdjudicationFlag("7:mem-a asserts X; mem-b: asserts not-X")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.queueID != 7 {
			t.Errorf("queueID = %d, want 7", got.queueID)
		}
		if got.reason != "mem-a asserts X; mem-b: asserts not-X" {
			t.Errorf("reason = %q — a reason containing a colon must survive intact", got.reason)
		}
	})

	t.Run("trims whitespace around the id", func(t *testing.T) {
		got, err := parseAdjudicationFlag("  9  :because they disagree")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.queueID != 9 {
			t.Errorf("queueID = %d, want 9", got.queueID)
		}
	})

	rejects := []struct {
		name  string
		value string
	}{
		{"missing colon entirely", "42"},
		{"blank reason", "42:   "},
		{"empty value", ""},
		{"memory id instead of queue row id", "mem-abc123:they conflict"},
		{"zero id", "0:reason"},
		{"negative id", "-3:reason"},
		{"non-numeric id", "abc:reason"},
	}
	for _, tc := range rejects {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, err := parseAdjudicationFlag(tc.value); err == nil {
				t.Errorf("parseAdjudicationFlag(%q) accepted an invalid value; "+
					"the gate must be deliberate, not best-effort", tc.value)
			}
		})
	}
}

// TestPrintResolveDecisions_ReportsPendingRowsAccurately is requirement 6 of
// issue #10 at the CLI surface: a refused row must be reported accurately
// rather than silently discarded.
//
// It asserts both halves of that. The row must be VISIBLE — id, the
// detected_by that failed the check, why it was not applied, and the exact
// command that would change that. And it must not be dressed up as a
// resolution: the operator must be able to see at a glance that nothing was
// applied to it.
func TestPrintResolveDecisions_ReportsPendingRowsAccurately(t *testing.T) {
	pending := mpminternal.ResolutionDecision{
		Adjudicated:      false,
		VerificationNote: "unverified similarity candidate: a high cosine score shows two memories are related, not that their claims conflict.",
	}
	adjudicated := mpminternal.ResolutionDecision{
		ResolutionTag: "RESOLVE",
		LoserID:       "mem-weak",
		WinnerID:      "mem-strong",
		Margin:        0.42,
		LoserScore:    mpminternal.ProvenanceScore{Score: 0.11, Confidence: 0.1, AgeDays: 400},
		WinnerScore:   mpminternal.ProvenanceScore{Score: 0.89, Confidence: 0.95, AgeDays: 2},
		Rationale:     "adjudicated conflict, decisive margin",
	}

	outcomes := []resolveOutcome{
		{row: map[string]interface{}{
			"id": "42", "memory_id_a": "mem-a", "memory_id_b": "mem-b",
			"detected_by": "similarity-scan",
		}, dec: pending, pending: true},
		{row: map[string]interface{}{
			"id": "43", "memory_id_a": "mem-weak", "memory_id_b": "mem-strong",
			"detected_by": "similarity-scan|adjudicated:read both, claims conflict",
		}, dec: adjudicated},
	}

	out := captureStdout(t, func() {
		printResolveDecisions(outcomes, true, 1)
	})

	// The pending row is visible, with the evidence an operator needs to act.
	mustContain := []string{
		"NEEDS ADJUDICATION",
		"queue_id=42",
		"mem-a",
		"mem-b",
		"similarity-scan",
		"unverified similarity candidate",
		"--adjudicate=42:",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("pending row not reported accurately: output is missing %q\n---\n%s", want, out)
		}
	}

	// The summary must say what did and did not happen. "1 applied" and "1
	// needs adjudication" are different states and the operator has to see
	// both.
	if !strings.Contains(out, "✓ Applied 1 resolutions") {
		t.Errorf("summary should report the one applied resolution:\n%s", out)
	}
	if !strings.Contains(out, "⏸ 1 row(s) need adjudication — no memory weight was changed for them.") {
		t.Errorf("summary should report the refused row and state that nothing changed for it:\n%s", out)
	}

	// The refused row must not be presented as a resolution. This is the
	// failure mode #10 describes: a candidate that reads like a finding.
	pendingIdx := strings.Index(out, "queue_id=42")
	appliedIdx := strings.Index(out, "queue_id=43")
	if pendingIdx < 0 || appliedIdx < 0 {
		t.Fatalf("expected both rows in output:\n%s", out)
	}
	pendingBlock := out[pendingIdx:appliedIdx]
	if strings.Contains(pendingBlock, "verdict:") {
		t.Errorf("a row awaiting adjudication was rendered with a verdict:\n%s", pendingBlock)
	}
	if strings.Contains(pendingBlock, "slash") {
		t.Errorf("a row awaiting adjudication was rendered as a slash:\n%s", pendingBlock)
	}
}

// TestPrintResolveDecisions_DryRunStillReportsPending: suppressing the
// decision in dry-run would hide the row from the operator who is trying to
// find out what is in the queue. The gate covers apply, not visibility.
func TestPrintResolveDecisions_DryRunStillReportsPending(t *testing.T) {
	outcomes := []resolveOutcome{
		{row: map[string]interface{}{
			"id": "7", "memory_id_a": "mem-a", "memory_id_b": "mem-b",
			"detected_by": "similarity-scan",
		}, dec: mpminternal.ResolutionDecision{VerificationNote: "no provenance"}, pending: true},
	}

	out := captureStdout(t, func() {
		printResolveDecisions(outcomes, false, 1)
	})

	if !strings.Contains(out, "NEEDS ADJUDICATION") || !strings.Contains(out, "queue_id=7") {
		t.Errorf("dry-run must still surface unadjudicated rows:\n%s", out)
	}
	if strings.Contains(out, "Applied") {
		t.Errorf("dry-run output must not claim anything was applied:\n%s", out)
	}
}
