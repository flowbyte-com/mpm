// drill_harness_test.go — pin the synthetic harness contract.
//
// Why two tests (compliant + non-compliant): the matrix's whole point
// is to differentiate passing and failing frameworks. The synthetic
// harness is the ground-truth emitter; a regression here means the
// matrix can't tell PASS from FAIL.

package internal

import (
	"context"
	"testing"
)

func TestSyntheticHarness_Compliant_EmitsRequiredSequence(t *testing.T) {
	h := NewSyntheticHarness()
	drill := DrillSpec{
		ID:        "test",
		Framework: "synthetic",
		Expect: DrillExpect{
			ToolsRequired: []string{"mpm_context", "mpm_lessons"},
			Sequence: []DrillStep{
				{Tool: "mpm_context", Action: "read_wake_context"},
				{Tool: "mpm_lessons", Action: "save"},
			},
		},
	}
	calls, sessID, err := h.Run(context.Background(), drill, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].ToolName != "mpm_context" || calls[0].Action != "read_wake_context" {
		t.Fatalf("first call wrong: %+v", calls[0])
	}
	if calls[1].ToolName != "mpm_lessons" || calls[1].Action != "save" {
		t.Fatalf("second call wrong: %+v", calls[1])
	}
	if sessID == "" {
		t.Fatal("session id must be a non-empty UUID")
	}
}

func TestSyntheticHarness_NonCompliant_SkipsFirstStep(t *testing.T) {
	h := NewSyntheticHarness()
	drill := DrillSpec{
		ID:        "test",
		Framework: "synthetic",
		Expect: DrillExpect{
			ToolsRequired: []string{"mpm_context", "mpm_lessons"},
			Sequence: []DrillStep{
				{Tool: "mpm_context", Action: "read_wake_context"},
				{Tool: "mpm_lessons", Action: "save"},
			},
		},
	}
	calls, _, err := h.Run(context.Background(), drill, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("non-compliant should still emit at least one call")
	}
	// Non-compliant drops the first recall-style step entirely — this
	// is the deliberate FAIL pattern that the scorer's sequence check
	// turns into a verdict. The drill_engine self-test in
	// drill_score_test.go exercises the FAIL path end-to-end.
	for _, c := range calls {
		if c.ToolName == "mpm_context" && c.Action == "read_wake_context" {
			t.Fatalf("non-compliant must skip read_wake_context; got call %+v", c)
		}
	}
	// The remaining steps still fire — non-compliance is about ORDER
	// (sequence-step omission), not about silencing the rest.
	sawLessons := false
	for _, c := range calls {
		if c.ToolName == "mpm_lessons" && c.Action == "save" {
			sawLessons = true
		}
	}
	if !sawLessons {
		t.Fatalf("non-compliant should still emit the lesson-save call: %+v", calls)
	}
}
