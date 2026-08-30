// skill_step_validation_test.go — alpha-4.1.2 D-004 regression test.
//
// Bug: a step YAML entry like `- call: ""` or `- args_from: "x"`
// parses cleanly into a SkillStep{Call:"", ArgsFrom:""} (yaml.v3 silently
// accepts empty mappings). The runtime then renders the skill with
// empty step rows that look present-but-broken: list_skills and
// read_skill show N steps, but executing the skill has nothing to call.
//
// Audit (alpha-4.1.2): the auditor reported this exact silent corruption
// as a release-blocking defect.
//
// Fix (alpha-4.1.2): validateSkillFrontmatterAndScan runs after
// ParseSkillFrontmatter and rejects:
//
//   - any step with empty Call (the unusable invocation),
//   - any step list that contains a YAML scalar (string/int) — yaml.v3
//     already returns an error for those, but the validation step makes
//     the rejection explicit at the application boundary,
//   - any step where Call is non-empty but whitespace-only.
//
// Empty Call is rejected with `step[N].call must be a non-empty
// invocation target` so the author can locate the bad row.
//
// This file pins the validation contract across the canonical shapes:
// valid object step, empty call (REJECTED), whitespace-only call
// (REJECTED), missing call (REJECTED), mixed valid/invalid steps
// (REJECTED at the first invalid row index).

package internal

import (
	"strings"
	"testing"
)

// TestSkillStep_ValidObject_PassesValidation pins the canonical happy
// path: a properly-shaped step with both Call and ArgsFrom survives
// validation with zero errors.
func TestSkillStep_ValidObject_PassesValidation(t *testing.T) {
	content := `---
name: agentshell
version: 1.0.0
steps:
  - call: agentshell_get_config
    args_from: mpm_wake
  - call: agentshell_set_css_var
---
body
`
	s, warns, errs, err := validateSkillFrontmatterAndScan(content)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if s == nil || len(s.Steps) != 2 {
		t.Fatalf("steps = %v, want 2", s)
	}
	if s.Steps[0].Call != "agentshell_get_config" || s.Steps[0].ArgsFrom != "mpm_wake" {
		t.Errorf("step[0] = %+v, want call=agentshell_get_config args_from=mpm_wake", s.Steps[0])
	}
	_ = warns // warnings are advisory
}

// TestSkillStep_EmptyCall_Rejected pins the auditor's reported scenario:
// a step with `- call: ""` must be rejected with a useful error
// identifying the offending row index.
func TestSkillStep_EmptyCall_Rejected(t *testing.T) {
	content := `---
name: agentshell
version: 1.0.0
steps:
  - call: agentshell_get_config
  - call: ""
---
body
`
	_, _, errs, err := validateSkillFrontmatterAndScan(content)
	if err != nil {
		t.Fatalf("unexpected outer error (yaml.v3 may already reject some shapes): %v", err)
	}
	if len(errs) == 0 {
		t.Fatalf("expected rejection for empty call, got nil errors")
	}
	// Must identify the bad row index.
	found := false
	for _, e := range errs {
		if strings.Contains(e, "step[1]") && strings.Contains(e, "call") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("error must name the offending row (got %v)", errs)
	}
}

// TestSkillStep_WhitespaceOnlyCall_Rejected pins that a step whose
// Call is whitespace-only is also unusable — the runtime would try to
// invoke "  " as a tool name and fail confusingly downstream.
func TestSkillStep_WhitespaceOnlyCall_Rejected(t *testing.T) {
	content := `---
name: agentshell
version: 1.0.0
steps:
  - call: "   "
---
body
`
	_, _, errs, err := validateSkillFrontmatterAndScan(content)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if len(errs) == 0 {
		t.Fatalf("expected rejection for whitespace-only call, got nil errors")
	}
}

// TestSkillStep_EmptyMap_Rejected pins that `- {}` (an entirely empty
// mapping) is rejected. yaml.v3 currently parses this as a silent
// SkillStep{Call:"", ArgsFrom:""} — the exact corruption the auditor
// reported.
func TestSkillStep_EmptyMap_Rejected(t *testing.T) {
	content := `---
name: agentshell
version: 1.0.0
steps:
  - call: real
  - {}
---
body
`
	_, _, errs, err := validateSkillFrontmatterAndScan(content)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if len(errs) == 0 {
		t.Fatalf("expected rejection for empty step map, got nil errors")
	}
	// Must identify step[1].
	found := false
	for _, e := range errs {
		if strings.Contains(e, "step[1]") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("error must name step[1] as the offender (got %v)", errs)
	}
}

// TestSkillStep_StringEntry_Rejected pins that a YAML scalar step
// (yaml.v3 already errors, but the application-level rejection makes
// the boundary explicit and produces a useful message rather than the
// raw yaml.v3 unmarshal error).
func TestSkillStep_StringEntry_Rejected(t *testing.T) {
	content := `---
name: agentshell
version: 1.0.0
steps:
  - "just a description, not a step"
---
body
`
	_, _, errs, err := validateSkillFrontmatterAndScan(content)
	// Either path is acceptable: yaml.v3 errors out at the parse stage
	// (returned as err), or our validator catches it after. Both are
	// rejections; the spec cares about the outcome, not which layer.
	if err == nil && len(errs) == 0 {
		t.Fatalf("expected rejection for string step, got no error and no validation errors")
	}
	if err != nil {
		// Verify the parse error names the step field.
		if !strings.Contains(err.Error(), "step") && !strings.Contains(err.Error(), "yaml") {
			t.Errorf("error should mention step or yaml, got: %v", err)
		}
	}
}

// TestSkillStep_MissingCall_Rejected pins that `- args_from: "x"` (no
// call field at all) is rejected. yaml.v3 silently parses this as
// SkillStep{Call:"", ArgsFrom:"x"} — a step that looks like it has
// a binding but no target.
func TestSkillStep_MissingCall_Rejected(t *testing.T) {
	content := `---
name: agentshell
version: 1.0.0
steps:
  - args_from: mpm_wake
---
body
`
	_, _, errs, err := validateSkillFrontmatterAndScan(content)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if len(errs) == 0 {
		t.Fatalf("expected rejection for missing call, got nil errors")
	}
	// Must mention 'call' so the author knows what's missing.
	found := false
	for _, e := range errs {
		if strings.Contains(e, "call") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("error must mention 'call' field, got %v", errs)
	}
}

// TestSkillStep_NoSteps_PassesValidation pins that a skill with zero
// steps is allowed — some skills are pure guidance, no procedure.
func TestSkillStep_NoSteps_PassesValidation(t *testing.T) {
	content := `---
name: agentshell
version: 1.0.0
description: guidance only, no procedure
---
body
`
	s, _, errs, err := validateSkillFrontmatterAndScan(content)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if len(errs) > 0 {
		t.Fatalf("unexpected errors for skill without steps: %v", errs)
	}
	if s == nil {
		t.Fatalf("skill should not be nil")
	}
	if len(s.Steps) != 0 {
		t.Errorf("steps = %d, want 0", len(s.Steps))
	}
}

// TestSkillStep_ValidSkillSave_RoundTrips confirms the end-to-end
// SaveSkill path: a well-formed skill with valid steps persists and
// reads back identically. This pins that validation didn't introduce
// a regression on the happy path.
func TestSkillStep_ValidSkillSave_RoundTrips(t *testing.T) {
	dm := newTestDM(t)
	content := `---
name: agentshell
version: 1.0.0
description: round-trip pin
steps:
  - call: agentshell_get_config
  - call: agentshell_set_css_var
    args_from: mpm_wake
---
body
`
	id, err := dm.SaveSkill("agentshell", "1.0.0", content, "", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}
	if id == "" {
		t.Fatalf("SaveSkill returned empty id")
	}
	got, err := dm.ReadSkill(id, "")
	if err != nil {
		t.Fatalf("ReadSkill(%s): %v", id, err)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("read back %d steps, want 2", len(got.Steps))
	}
	if got.Steps[1].Call != "agentshell_set_css_var" || got.Steps[1].ArgsFrom != "mpm_wake" {
		t.Errorf("read-back step[1] = %+v, want call=agentshell_set_css_var args_from=mpm_wake", got.Steps[1])
	}
}