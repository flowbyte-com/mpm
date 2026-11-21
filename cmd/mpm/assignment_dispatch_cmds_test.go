// assignment_dispatch_cmds_test.go — the operator's own path to the
// dispatcher.
//
// # WHAT IS UNDER TEST
//
// The two things an operator must be able to SEE about the dispatcher:
//
//   - whether it is on (the switch), and
//   - what it would do (the status).
//
// And the one thing they must not be able to do by accident: turn it on
// without saying so.
//
// # WHY THE CLI IS THE ONLY DOOR
//
// `dispatch` starts a contained runtime process and spends real model
// budget. It is not an agent-facing action, so the boundary test in
// internal/core/tools pins that too.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssignmentCLIDispatchIsOffWithoutAConfigurationFile proves the
// default.
//
// There is no configuration file, so the dispatcher is off, and the verb
// says so. It does not read a row, does not build a runtime and does
// not dispatch: an operator can run it to confirm the switch without
// any risk.
func TestAssignmentCLIDispatchIsOffWithoutAConfigurationFile(t *testing.T) {
	assignmentCLIFixture(t)

	code, out, errOut := runAssignmentCLI(t, "assignment", "dispatch")
	if code != 0 {
		t.Fatalf("the dispatch verb exited with %d: %s", code, errOut)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("the dispatch verb does not say the dispatcher is disabled:\n%s", out)
	}
	if !strings.Contains(out, dispatchReasonDisabled) {
		t.Errorf("the dispatch verb does not carry the disabled reason code:\n%s", out)
	}
}

// dispatchReasonDisabled is the reason code the disabled path reports.
//
// It is written out here rather than imported so the CLI test pins the
// STRING an operator sees, not the constant behind it.
const dispatchReasonDisabled = "dispatch_disabled"

// TestAssignmentCLIDispatchStatusWorksWhileDisabled proves the status
// is a READ.
//
// It works when the dispatcher is off — which is exactly when an
// operator wants to see what turning it on would do — and it writes
// nothing.
func TestAssignmentCLIDispatchStatusWorksWhileDisabled(t *testing.T) {
	dm := assignmentCLIFixture(t)
	_ = dm

	code, out, errOut := runAssignmentCLI(t, "assignment", "dispatch-status", "--json")
	if code != 0 {
		t.Fatalf("the status verb exited with %d: %s", code, errOut)
	}
	var status struct {
		Enabled     bool       `json:"enabled"`
		Pending     []struct{} `json:"pending"`
		Blocked     []struct{} `json:"blocked"`
		Recoverable []struct{} `json:"recoverable"`
		Budgets     []struct{} `json:"budgets"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("the status is not valid JSON: %v\n%s", err, out)
	}
	if status.Enabled {
		t.Error("the status says the dispatcher is enabled, with no configuration file")
	}
	for _, key := range []string{"pending", "blocked", "recoverable", "budgets"} {
		if !strings.Contains(out, "\""+key+"\"") {
			t.Errorf("the status has no %q bucket:\n%s", key, out)
		}
	}
}

// TestAssignmentCLIDispatchRefusesABrokenConfigurationFile proves the
// loud direction.
//
// A file the operator wrote and that cannot be understood is an ERROR.
// It is not a reason to dispatch and it is not a reason to guess that
// the dispatcher is off.
func TestAssignmentCLIDispatchRefusesABrokenConfigurationFile(t *testing.T) {
	assignmentCLIFixture(t)

	path := filepath.Join(t.TempDir(), "dispatch.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runAssignmentCLI(t, "assignment", "dispatch", "--dispatch-config", path)
	if code == 0 {
		t.Fatal("a broken configuration file was accepted")
	}
	if !strings.Contains(errOut, path) {
		t.Errorf("the refusal does not name the broken file: %s", errOut)
	}
}

// TestAssignmentCLIDispatchRefusesAnIncompleteEnabledConfiguration
// proves that "enabled" is not enough.
//
// A configuration that turns the dispatcher on but leaves a limit
// unstated is refused: a dispatcher that invents its own limits is worse
// than no dispatcher.
func TestAssignmentCLIDispatchRefusesAnIncompleteEnabledConfiguration(t *testing.T) {
	assignmentCLIFixture(t)

	path := filepath.Join(t.TempDir(), "dispatch.json")
	if err := os.WriteFile(path, []byte(`{"enabled": true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runAssignmentCLI(t, "assignment", "dispatch", "--dispatch-config", path)
	if code == 0 {
		t.Fatal("an enabled configuration with no limits was accepted")
	}
	if !strings.Contains(errOut, "dispatch") {
		t.Errorf("the refusal does not name the dispatcher: %s", errOut)
	}
}
