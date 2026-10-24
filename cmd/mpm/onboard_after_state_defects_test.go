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
