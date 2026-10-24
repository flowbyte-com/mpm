// onboard_after_state_test.go — HAL-9001 Finding 4.
//
// The onboarding report used to be collected BEFORE the guided steps
// ran and rendered afterwards. The operator therefore read a report
// about a configuration that the run itself had just replaced: the
// Models section said "no generative profile configured" directly
// above a report that had just configured one.
//
// The correction is to re-collect the report after a step that
// changed the configuration, and to mark the result as an after-state
// so the operator knows which moment it describes.
//
// These tests are pure: they exercise the merge and the rendering
// without a terminal, without a database and without a network.
package main

import (
	"os"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestMergeOnboardAfterState_DescribesTheStateAsItIsNow pins the core
// correction: the collected rows come from the AFTER report.
func TestMergeOnboardAfterState_DescribesTheStateAsItIsNow(t *testing.T) {
	before := onboardReport{
		Models: []onboardRow{{
			Subject: "Generative · memory",
			State:   onboardStateAttention,
			Detail:  "no profile configured",
		}},
	}
	after := onboardReport{
		Models: []onboardRow{{
			Subject: "Generative · memory",
			State:   onboardStateOperational,
			Detail:  "resolves to profile default",
		}},
	}

	merged := mergeOnboardAfterState(after, before)

	if len(merged.Models) != 1 {
		t.Fatalf("merged report has %d model rows, want 1: %+v", len(merged.Models), merged.Models)
	}
	if merged.Models[0].State != onboardStateOperational {
		t.Errorf("the merged report still describes the state as it was FOUND; row: %+v", merged.Models[0])
	}
	if merged.Models[0].Detail != "resolves to profile default" {
		t.Errorf("the merged report does not carry the after-state detail; row: %+v", merged.Models[0])
	}
}

// TestMergeOnboardAfterState_DropsResolvedProblems pins that the
// after-state report does not carry forward a problem the run fixed.
//
// The collected attention entries describe the configuration as it was
// found. After a run that resolved them, keeping them would report a
// closed problem as an open one.
func TestMergeOnboardAfterState_DropsResolvedProblems(t *testing.T) {
	const resolved = "configuration could not be loaded: no such file"

	before := onboardReport{
		Attention: []string{resolved},
		ModelStep: &modelStepOutcome{
			Attention: []string{"model configuration is incomplete: embedding model profile does still"},
		},
	}
	after := onboardReport{Attention: []string{}}

	merged := mergeOnboardAfterState(after, before)

	for _, a := range merged.Attention {
		if a == resolved {
			t.Errorf("the after-state report carries a problem the run had resolved: %q", a)
		}
	}
	found := false
	for _, a := range merged.Attention {
		if strings.HasPrefix(a, "model configuration is incomplete") {
			found = true
		}
	}
	if !found {
		t.Errorf("the step's own attention entry was lost from the after-state report: %v", merged.Attention)
	}
}

// TestMergeOnboardAfterState_KeepsTheStepOutcome pins that the
// after-state report still says what the run did.
//
// Re-collecting the rows must not throw away the plan, the step
// record or the step's own findings — those are the account of the
// run, and the re-collected rows are the account of the result.
func TestMergeOnboardAfterState_KeepsTheStepOutcome(t *testing.T) {
	plan := &modelStepPlan{Generative: modelStateConfigured, Embedding: modelStateMissing}
	step := &modelStepOutcome{
		Reason:  "approved: delegating to the existing `mpm config` wizard",
		Applied: true,
		Saved:   true,
		After:   plan,
		Skipped: []string{"a step-level skip"},
	}
	before := onboardReport{
		ModelPlan: plan,
		ModelStep: step,
		Plan:      []string{"collect a generative model profile"},
	}
	after := onboardReport{
		Skipped: []string{"a collected skip"},
	}

	merged := mergeOnboardAfterState(after, before)

	if merged.ModelStep != step {
		t.Errorf("the after-state report lost the step outcome: %+v", merged.ModelStep)
	}
	if merged.ModelPlan != plan {
		t.Errorf("the after-state report lost the model plan: %+v", merged.ModelPlan)
	}
	if len(merged.Plan) != 1 || merged.Plan[0] != "collect a generative model profile" {
		t.Errorf("the after-state report lost the plan: %v", merged.Plan)
	}
	if len(merged.Skipped) != 2 {
		t.Errorf("the after-state report has %d skipped entries, want 2 (one collected, one step-level): %v",
			len(merged.Skipped), merged.Skipped)
	}
}

// TestMergeOnboardAfterState_MarksTheReport pins that the report says
// which moment it describes.
func TestMergeOnboardAfterState_MarksTheReport(t *testing.T) {
	merged := mergeOnboardAfterState(onboardReport{}, onboardReport{})
	if !merged.AfterState {
		t.Error("the merged report is not marked as an after-state")
	}
}

// TestRenderOnboardReport_MarksTheAfterState pins the human-facing
// half of the correction: an after-state report must be unmistakably
// labelled, and an ordinary report must not claim to be one.
func TestRenderOnboardReport_MarksTheAfterState(t *testing.T) {
	after := captureStdout(t, func() {
		renderOnboardReport(onboardReport{AfterState: true})
	})
	if !strings.Contains(after, "After this run") {
		t.Errorf("the after-state report is not labelled as such:\n%s", after)
	}

	plain := captureStdout(t, func() {
		renderOnboardReport(onboardReport{})
	})
	if strings.Contains(plain, "After this run") {
		t.Errorf("a report that describes the state as found is labelled as an after-state:\n%s", plain)
	}
}

// TestRenderModelStep_DistinguishesSavedFromUnchanged pins the
// distinction the operator needs most.
//
// The wizard exits 0 whether it committed the configuration or the
// operator declined the final "Save?", so "the wizard ran and exited
// 0" says nothing about whether anything was written.
func TestRenderModelStep_DistinguishesSavedFromUnchanged(t *testing.T) {
	saved := captureStdout(t, func() {
		renderModelStep(onboardReport{ModelStep: &modelStepOutcome{
			Reason: "approved", Applied: true, Saved: true,
		}})
	})
	if !strings.Contains(saved, "was saved") {
		t.Errorf("a saved configuration is not reported as saved:\n%s", saved)
	}

	unchanged := captureStdout(t, func() {
		renderModelStep(onboardReport{ModelStep: &modelStepOutcome{
			Reason: "approved", Applied: true, Saved: false,
		}})
	})
	if !strings.Contains(unchanged, "unchanged") {
		t.Errorf("a run that committed nothing is not reported as such:\n%s", unchanged)
	}
	if strings.Contains(unchanged, "was saved") {
		t.Errorf("a run that committed nothing is reported as saved:\n%s", unchanged)
	}
}

// TestConfigSnapshot_DetectsAChangedConfiguration pins the mechanism
// that tells "the wizard ran" from "the configuration changed".
func TestConfigSnapshot_DetectsAChangedConfiguration(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	before := takeConfigSnapshot()
	if before.exists {
		t.Fatalf("a fresh workspace should have no configuration file; found %s", config.ConfigPath())
	}

	if err := os.WriteFile(config.ConfigPath(), []byte(`{"profiles":{}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	created := takeConfigSnapshot()
	if !before.changed(created) {
		t.Error("creating the configuration file was not detected as a change")
	}

	if created.changed(takeConfigSnapshot()) {
		t.Error("an unchanged configuration was reported as changed")
	}

	if err := os.WriteFile(config.ConfigPath(), []byte(`{"profiles":{"default":{}}}`), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if !created.changed(takeConfigSnapshot()) {
		t.Error("a rewritten configuration was not detected as a change")
	}
}
