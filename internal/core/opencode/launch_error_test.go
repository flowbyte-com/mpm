package opencode

// R2 unit regressions: failures keep a safe classification, exit code,
// inspection class and output digest — never raw output text.

import (
	"errors"
	"strings"
	"testing"
)

func TestNewLaunchErrorClassifiesWithoutRawOutput(t *testing.T) {
	const marker = "SYNTHETIC-SECRET-MARKER"
	res := &LaunchResult{Output: marker}
	le := newLaunchError(errors.New("exit status 7"), true, 7, errors.New("inspect failed"), res)
	if le.RunFailure != LaunchExitFailure || le.ExitCode != 7 {
		t.Fatalf("run classification = %q/%d", le.RunFailure, le.ExitCode)
	}
	if le.InspectFailure != InspectUnavailable {
		t.Fatalf("inspect classification = %q", le.InspectFailure)
	}
	if le.StateObserved || !le.InferenceUnknown {
		t.Fatalf("state observed=%v inference unknown=%v", le.StateObserved, le.InferenceUnknown)
	}
	if le.OutputBytes != len(marker) || le.OutputSHA256 == "" {
		t.Fatalf("output evidence = %d/%q", le.OutputBytes, le.OutputSHA256)
	}
	msg := le.Error()
	if strings.Contains(msg, marker) {
		t.Fatalf("raw output reached the diagnostic: %q", msg)
	}
	for _, want := range []string{"exit 7", "could not be inspected", "inference is unknown", "bounded output"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic %q lacks %q", msg, want)
		}
	}
}

func TestNewLaunchErrorStartFailureAndNotFound(t *testing.T) {
	le := newLaunchError(errors.New("exec: no such file"), false, 0, ErrSessionNotFound, &LaunchResult{})
	if le.RunFailure != LaunchStartFailed || le.InspectFailure != InspectNotFound {
		t.Fatalf("classification = %q/%q", le.RunFailure, le.InspectFailure)
	}
	msg := le.Error()
	if !strings.Contains(msg, "could not start") || !strings.Contains(msg, "was not found") {
		t.Fatalf("diagnostic = %q", msg)
	}
}

func TestNewLaunchErrorInspectionOnly(t *testing.T) {
	le := newLaunchError(nil, true, 0, errors.New("inspect failed"), &LaunchResult{})
	if le.RunFailure != "" {
		t.Fatalf("a successful command was classified as %q", le.RunFailure)
	}
	if le.InspectFailure != InspectUnavailable || !le.InferenceUnknown {
		t.Fatalf("inspection = %q unknown=%v", le.InspectFailure, le.InferenceUnknown)
	}
	if strings.Contains(le.Error(), "exited unsuccessfully") {
		t.Fatalf("a successful command was misclassified: %q", le.Error())
	}
}

func TestNewLaunchErrorObservedStateWithoutOutcome(t *testing.T) {
	res := &LaunchResult{State: &SessionState{}}
	le := newLaunchError(nil, true, 0, errors.New("inspect failed"), res)
	if !le.StateObserved || !le.InferenceUnknown {
		t.Fatalf("state observed=%v inference unknown=%v", le.StateObserved, le.InferenceUnknown)
	}
	if !strings.Contains(le.Error(), "no terminal outcome") {
		t.Fatalf("diagnostic = %q", le.Error())
	}
}
