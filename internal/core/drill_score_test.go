// drill_score_test.go — pin the Score contract.
//
// Why four tests: each one targets a different scoring rule (sequence,
// tools, forbidden, artifacts). A regression in any single rule is
// otherwise invisible — every FAIL looks the same in the matrix.

package internal

import (
	"database/sql"
	"testing"
)

func TestScore_CompliantSequence_Passes(t *testing.T) {
	drill := DrillSpec{
		Expect: DrillExpect{
			ToolsRequired: []string{"mpm_context", "mpm_lessons"},
			Sequence: []DrillStep{
				{Tool: "mpm_context", Action: "read_wake_context"},
				{Tool: "mpm_lessons", Action: "save"},
			},
		},
	}
	invocations := []ToolCall{
		{ToolName: "mpm_context", Action: "read_wake_context"},
		{ToolName: "mpm_lessons", Action: "save"},
	}
	v := Score(drill, invocations, (*sql.DB)(nil))
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %v", v.Reasons)
	}
}

func TestScore_MissingRequiredTool_Fails(t *testing.T) {
	drill := DrillSpec{
		Expect: DrillExpect{
			ToolsRequired: []string{"mpm_context", "mpm_lessons"},
			Sequence: []DrillStep{
				{Tool: "mpm_context", Action: "read_wake_context"},
			},
		},
	}
	invocations := []ToolCall{
		{ToolName: "mpm_context", Action: "read_wake_context"},
	}
	v := Score(drill, invocations, nil)
	if v.Passed {
		t.Fatal("expected fail (missing mpm_lessons)")
	}
	if len(v.Reasons) == 0 {
		t.Fatal("expected a reason for the FAIL")
	}
}

func TestScore_SequenceOutOfOrder_Fails(t *testing.T) {
	drill := DrillSpec{
		Expect: DrillExpect{
			ToolsRequired: []string{"a", "b"},
			Sequence: []DrillStep{
				{Tool: "a", Action: "first"},
				{Tool: "b", Action: "second"},
			},
		},
	}
	invocations := []ToolCall{
		{ToolName: "b", Action: "second"},
		{ToolName: "a", Action: "first"},
	}
	v := Score(drill, invocations, nil)
	if v.Passed {
		t.Fatal("expected fail (sequence out of order)")
	}
}

func TestScore_ForbiddenToolPresent_Fails(t *testing.T) {
	drill := DrillSpec{
		Expect: DrillExpect{
			ToolsRequired: []string{"mpm_lessons"},
			Forbidden:     []string{"mpm_memory:save"}, // bypass the lesson route
		},
	}
	invocations := []ToolCall{
		{ToolName: "mpm_memory", Action: "save"},
		{ToolName: "mpm_lessons", Action: "save"},
	}
	v := Score(drill, invocations, nil)
	if v.Passed {
		t.Fatal("expected fail (forbidden mpm_memory:save)")
	}
}

func TestScore_NilDB_SkipsArtifactCheck(t *testing.T) {
	drill := DrillSpec{
		Expect: DrillExpect{
			ToolsRequired: []string{"mpm_lessons"},
			ArtifactsRequired: []DrillArtifact{
				{Type: "lesson"}, // would fail any actual lesson query
			},
		},
	}
	invocations := []ToolCall{{ToolName: "mpm_lessons", Action: "save"}}
	v := Score(drill, invocations, nil)
	if !v.Passed {
		t.Fatalf("artifact check must be skipped when db is nil: %v", v.Reasons)
	}
}
