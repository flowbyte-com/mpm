// onboard_after_state_defects_test.go — issue #63.
//
// The after-state report carries a banner that says the sections below
// describe the state as it is NOW. Four defects made that banner false,
// or made the report fail to explain itself.
//
//  1. The pre-change NotVerified/NotAvailable entries were carried
//     forward. Dedup is on exact string equality, so an entry whose
//     text changed between the two collections — an embedding model
//     identity, for instance — appears TWICE under a banner that claims
//     to describe the current state.
//
//  2. The "cannot re-read the configuration" error was recorded in the
//     PRE-change report, and the merge does not carry that report's
//     error list forward. The error that explains an empty plan was
//     discarded with it.
//
//  3. A failed re-collection was silent: the report kept the pre-change
//     rows with no marker at all.
//
//  4. "nothing was committed" was claimed for a run that HAD committed
//     the configuration — a wizard run that rewrote byte-identical
//     content — contradicting the wizard's own "✓ Configuration saved".
//
// These tests pin each defect. They are pure: no terminal, no database,
// no network, and they never touch the operator's real workspace.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestMergeOnboardAfterState_DoesNotCarryPreChangeNotVerifiedForward
// pins defect 1.
//
// The merge takes the collected rows from the AFTER report because they
// describe the configuration as it is. NotVerified and NotAvailable are
// collected rows too — the split that keeps the step's own findings
// apart was applied to Attention/Skipped/Errors and not to these two.
func TestMergeOnboardAfterState_DoesNotCarryPreChangeNotVerifiedForward(t *testing.T) {
	const beforeEntry = "embedding model identity of already-stored memory vectors: " +
		"120 vectors, 0 labelled (ollama:nomic-embed-text)"
	const afterEntry = "embedding model identity of already-stored memory vectors: " +
		"120 vectors, 0 labelled (ollama:nomic-embed-text-v2)"

	before := onboardReport{
		NotVerified:  []string{beforeEntry},
		NotAvailable: []string{"model configuration: available, but no host was named"},
	}
	after := onboardReport{
		NotVerified:  []string{afterEntry},
		NotAvailable: []string{"host installation: available, but no host was named"},
	}

	merged := mergeOnboardAfterState(after, before)

	for _, e := range merged.NotVerified {
		if e == beforeEntry {
			t.Errorf("the after-state report carries the PRE-CHANGE not-verified entry %q; "+
				"the entry it replaced differs in text, so exact-equality dedup does not "+
				"remove it and the resolved problem is reported as an open one", beforeEntry)
		}
	}
	if len(merged.NotVerified) != 1 || merged.NotVerified[0] != afterEntry {
		t.Errorf("not-verified entries = %v, want exactly the after-state entry %q",
			merged.NotVerified, afterEntry)
	}

	for _, e := range merged.NotAvailable {
		if e == "model configuration: available, but no host was named" {
			t.Errorf("the after-state report carries the PRE-CHANGE not-available entry %q", e)
		}
	}
	if len(merged.NotAvailable) != 1 ||
		merged.NotAvailable[0] != "host installation: available, but no host was named" {
		t.Errorf("not-available entries = %v, want exactly the after-state entry",
			merged.NotAvailable)
	}
}

// TestMergeOnboardAfterState_KeepsTheAfterStateNotVerified pins the
// other half of defect 1: the after-state entries must still be there.
//
// A fix that simply drops both lists would satisfy the first test and
// lose the information the operator needs.
func TestMergeOnboardAfterState_KeepsTheAfterStateNotVerified(t *testing.T) {
	const afterEntry = "live reachability and authentication of the configured endpoints"

	merged := mergeOnboardAfterState(
		onboardReport{NotVerified: []string{afterEntry}},
		onboardReport{NotVerified: []string{"a stale entry"}})

	if len(merged.NotVerified) != 1 || merged.NotVerified[0] != afterEntry {
		t.Errorf("not-verified entries = %v, want exactly the after-state entry %q",
			merged.NotVerified, afterEntry)
	}
}

// TestReloadConfig_KeepsTheReReadErrorAcrossTheAfterStateMerge pins
// defect 2.
//
// reloadConfig records a re-read failure in the report it is handed —
// the PRE-change report. The merge takes the step's own errors, not the
// pre-change report's, so the error that explains an empty post-run
// plan was dropped on the floor.
func TestReloadConfig_KeepsTheReReadErrorAcrossTheAfterStateMerge(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("HOME", t.TempDir())

	// A configuration file that cannot be parsed: the re-read fails
	// with a real error rather than with "no such file".
	if err := os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(config.ConfigPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt configuration: %v", err)
	}

	rep := onboardReport{ModelStep: &modelStepOutcome{}}
	reloadConfig(&rep)

	if len(rep.Errors) == 0 {
		t.Fatal("the re-read failure was not recorded in the report")
	}
	if len(rep.ModelStep.Errors) == 0 {
		t.Fatalf("the re-read failure was not recorded where the merge keeps it; "+
			"the merge takes the step's own errors, not the pre-change report's: %v",
			rep.ModelStep.Errors)
	}

	merged := mergeOnboardAfterState(onboardReport{Errors: []string{}}, rep)
	if len(merged.Errors) == 0 {
		t.Fatalf("the after-state report dropped the re-read failure entirely: %v", merged.Errors)
	}
	if !strings.Contains(merged.Errors[0], "cannot re-read") {
		t.Errorf("the after-state report does not carry the re-read failure; errors = %v",
			merged.Errors)
	}
}

// TestApplyAfterState_RecordsACollectionFailureAndMarksTheReportStale
// pins defect 3.
//
// A run that changed the configuration and then failed to re-read it
// must not present the PRE-change rows as if nothing had happened, and
// must not present them as an after-state report either — they are
// neither. The failure is recorded and the report is marked stale.
func TestApplyAfterState_RecordsACollectionFailureAndMarksTheReportStale(t *testing.T) {
	rep := onboardReport{ModelStep: &modelStepOutcome{Saved: true}}

	out := applyAfterState(onboardOptions{}, rep, func(onboardOptions) (onboardReport, error) {
		return onboardReport{}, errors.New("the database is locked")
	})

	if !out.Stale {
		t.Error("a failed re-collection did not mark the report stale")
	}
	if out.AfterState {
		t.Error("a failed re-collection is presented as an after-state report; " +
			"the rows in it do not describe the current state")
	}
	if len(out.Errors) == 0 {
		t.Fatal("a failed re-collection was recorded nowhere")
	}
	if !strings.Contains(out.Errors[0], "cannot re-read the configuration state") {
		t.Errorf("the recorded failure does not say what went wrong: %v", out.Errors)
	}
	if !strings.Contains(out.Errors[0], "the database is locked") {
		t.Errorf("the recorded failure does not carry the underlying cause: %v", out.Errors)
	}
}

// TestApplyAfterState_MergesASuccessfulCollection is the control for
// the test above: a successful re-collection must still produce an
// after-state report, and must not be marked stale.
func TestApplyAfterState_MergesASuccessfulCollection(t *testing.T) {
	rep := onboardReport{ModelStep: &modelStepOutcome{Saved: true}}

	out := applyAfterState(onboardOptions{}, rep, func(onboardOptions) (onboardReport, error) {
		return onboardReport{NotVerified: []string{"an after-state entry"}}, nil
	})

	if !out.AfterState {
		t.Error("a successful re-collection did not produce an after-state report")
	}
	if out.Stale {
		t.Error("a successful re-collection marked the report stale")
	}
	if len(out.NotVerified) != 1 || out.NotVerified[0] != "an after-state entry" {
		t.Errorf("not-verified entries = %v, want the after-state entry", out.NotVerified)
	}
}

// TestRenderOnboardReport_MarksAStaleReport pins the human-facing
// half of defect 3: the operator must be able to see that the rows
// below describe the state as it was found.
func TestRenderOnboardReport_MarksAStaleReport(t *testing.T) {
	out := captureStdout(t, func() {
		renderOnboardReport(onboardReport{Stale: true})
	})
	if !strings.Contains(out, "could not be re-read") {
		t.Errorf("a stale report is not marked as stale:\n%s", out)
	}
	if strings.Contains(out, "After this run") {
		t.Errorf("a stale report claims to describe the state as it is now:\n%s", out)
	}
}

// TestRenderModelStep_DoesNotClaimNothingWasCommittedWhenTheWizard
// Committed pins defect 4.
//
// The wizard prints "✓ Configuration saved to …" the moment it writes.
// A report that says "nothing was committed" about the same run
// contradicts it in the same terminal.
func TestRenderModelStep_DoesNotClaimNothingWasCommittedWhenTheWizardCommitted(t *testing.T) {
	out := captureStdout(t, func() {
		renderModelStep(onboardReport{ModelStep: &modelStepOutcome{
			Reason: "approved", Applied: true, Saved: false, Committed: true,
		}})
	})
	if strings.Contains(out, "nothing was committed") {
		t.Errorf("a run that wrote the configuration is reported as having committed nothing:\n%s", out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("a run that wrote byte-identical content does not say the configuration is unchanged:\n%s", out)
	}
}

// TestRenderModelStep_ClaimsNothingWasCommittedWhenTheWizardDidNot
// is the control for the test above.
//
// "Nothing was committed" is the right thing to say when the operator
// DECLINED the final save. The fix must not make the report vague in
// that case.
func TestRenderModelStep_ClaimsNothingWasCommittedWhenTheWizardDidNot(t *testing.T) {
	out := captureStdout(t, func() {
		renderModelStep(onboardReport{ModelStep: &modelStepOutcome{
			Reason: "approved", Applied: true, Saved: false, Committed: false,
		}})
	})
	if !strings.Contains(out, "nothing was committed") {
		t.Errorf("a run that committed nothing is not reported as such:\n%s", out)
	}
}

// TestWizard_ReportsWhetherItCommittedTheConfiguration pins the
// mechanism defect 4 rests on.
//
// The wizard's exit code is 0 whether it committed the configuration
// or the operator declined the final "Save?", so it cannot answer the
// question the report has to answer.
func TestWizard_ReportsWhetherItCommittedTheConfiguration(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	_, committed := runWizardInPTY(t, c, "y\n")
	if !committed {
		t.Error("the wizard did not report committing the configuration after the operator approved the save")
	}
}

func TestWizard_ReportsNotCommittingWhenTheOperatorDeclines(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	_, committed := runWizardInPTY(t, c, "\x04") // Ctrl-D: the terminal's EOF character.
	if committed {
		t.Error("the wizard reported committing the configuration after the operator aborted")
	}
}
