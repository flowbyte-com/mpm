package assignment

// R2 regressions: launch failures keep a safe classification, never raw
// output, and remain AMBIGUOUS when native state is not observed.

import (
	"context"
	"errors"
	"strings"
	"testing"

	oc "github.com/flowbyte-com/mpm-core/opencode"
)

func lastAttempt(t *testing.T, store *Store, id string) *SessionAttempt {
	t.Helper()
	attempts, err := store.AttemptsFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	return attempts[0]
}

func TestExecuteRecordsStructuredLaunchFailure(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	ws := t.TempDir()
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		launchErr: &oc.LaunchError{
			RunFailure:       oc.LaunchExitFailure,
			ExitCode:         7,
			InspectFailure:   oc.InspectUnavailable,
			StateObserved:    false,
			InferenceUnknown: true,
			OutputBytes:      19,
			OutputSHA256:     strings.Repeat("a", 64),
		},
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("the adapter did not record the ambiguity: %v", err)
	}
	if res.LaunchState != SessionAmbiguous {
		t.Fatalf("launch state = %q, want ambiguous", res.LaunchState)
	}
	last := lastAttempt(t, store, id).LastError
	for _, want := range []string{"launch incomplete", "exit 7", "could not be inspected", "inference is unknown"} {
		if !strings.Contains(last, want) {
			t.Errorf("last_error %q does not carry %q", last, want)
		}
	}
	if strings.Contains(last, "the runtime produced no session state") {
		t.Errorf("the generic message replaced the structured diagnostic: %q", last)
	}
}

func TestExecuteRecordsStartFailureAndSessionNotFound(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		launchErr: &oc.LaunchError{
			RunFailure:       oc.LaunchStartFailed,
			InspectFailure:   oc.InspectNotFound,
			InferenceUnknown: true,
		},
	}
	if _, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	last := lastAttempt(t, store, id).LastError
	if !strings.Contains(last, "could not start") || !strings.Contains(last, "was not found") {
		t.Fatalf("last_error = %q, want start and not-found classifications", last)
	}
}

func TestExecuteRecordsInspectionFailureWithoutCommandFailure(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		launchErr: &oc.LaunchError{
			InspectFailure:   oc.InspectUnavailable,
			InferenceUnknown: true,
		},
	}
	if _, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	last := lastAttempt(t, store, id).LastError
	if !strings.Contains(last, "could not be inspected") {
		t.Fatalf("last_error = %q", last)
	}
	if strings.Contains(last, "exited unsuccessfully") {
		t.Fatalf("a successful command was misclassified: %q", last)
	}
}

func TestExecuteDoesNotPersistRawLaunchOutput(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	const marker = "SYNTHETIC-SECRET-MARKER"
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		result:  &oc.LaunchResult{Output: marker},
		launchErr: &oc.LaunchError{
			RunFailure:       oc.LaunchExitFailure,
			ExitCode:         1,
			InspectFailure:   oc.InspectUnavailable,
			InferenceUnknown: true,
		},
	}
	if _, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	last := lastAttempt(t, store, id).LastError
	if strings.Contains(last, marker) {
		t.Fatalf("raw output reached the durable diagnostic: %q", last)
	}
}

func TestExecuteCommandFailureWithTerminalStateStillRecords(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	ws := t.TempDir()
	const marker = "SYNTHETIC-SECRET-MARKER"
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		result: &oc.LaunchResult{
			ExitCode: 7,
			Output:   marker,
			State:    okState(ws, "permitted-model", "mpm-worker"),
		},
		launchErr: errors.New("exit status 7"),
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("a terminal state should still be recorded: %v", err)
	}
	if res.LaunchState != SessionCompleted {
		t.Fatalf("launch state = %q, want completed", res.LaunchState)
	}
	last := lastAttempt(t, store, id)
	if strings.Contains(last.LastError, marker) {
		t.Fatalf("raw output reached the durable diagnostic: %q", last.LastError)
	}
}
