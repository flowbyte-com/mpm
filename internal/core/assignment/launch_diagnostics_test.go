package assignment

// R2 regressions: launch failures keep a safe classification, never raw
// output, and remain AMBIGUOUS when native state is not observed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

func TestExecuteAmbiguousEvidenceKeepsExitCodeAndOutput(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	const marker = "SYNTHETIC-OUTPUT-MARKER-B1"
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		result:  &oc.LaunchResult{ExitCode: 7, Output: marker},
		launchErr: &oc.LaunchError{
			RunFailure:       oc.LaunchExitFailure,
			ExitCode:         7,
			InspectFailure:   oc.InspectUnavailable,
			InferenceUnknown: true,
		},
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LaunchState != SessionAmbiguous {
		t.Fatalf("launch state = %q, want ambiguous", res.LaunchState)
	}
	if res.Evidence.ExitCode != 7 {
		t.Fatalf("B1: evidence exit_code = %d, want 7", res.Evidence.ExitCode)
	}
	if res.Evidence.OutputBytes != len(marker) {
		t.Fatalf("B1: evidence output_bytes = %d, want %d", res.Evidence.OutputBytes, len(marker))
	}
	sum := sha256.Sum256([]byte(marker))
	wantDigest := hex.EncodeToString(sum[:])
	if res.Evidence.OutputSHA256 != wantDigest {
		t.Fatalf("B1: evidence digest = %q, want %q", res.Evidence.OutputSHA256, wantDigest)
	}
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"exit_code":7`, `"output_bytes":` + fmt.Sprint(len(marker)), wantDigest} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("operator JSON lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), marker) {
		t.Fatalf("raw output reached the operator JSON:\n%s", body)
	}
	last := lastAttempt(t, store, id)
	if last.LaunchState != SessionAmbiguous || !strings.Contains(last.LastError, "exit 7") {
		t.Fatalf("persisted attempt is inconsistent: %+v", last)
	}
	if strings.Contains(last.LastError, marker) {
		t.Fatalf("raw output reached the persisted attempt: %q", last.LastError)
	}
	t.Logf("evidence: exit=%d bytes=%d sha=%s reason=%q", res.Evidence.ExitCode, res.Evidence.OutputBytes, res.Evidence.OutputSHA256, res.Evidence.Reason)
}

func TestExecuteAmbiguousEvidenceFallsBackToLaunchError(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	const digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		launchErr: &oc.LaunchError{
			RunFailure:       oc.LaunchExitFailure,
			ExitCode:         9,
			InspectFailure:   oc.InspectUnavailable,
			InferenceUnknown: true,
			OutputBytes:      42,
			OutputSHA256:     digest,
		},
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Evidence.ExitCode != 9 || res.Evidence.OutputBytes != 42 ||
		res.Evidence.OutputSHA256 != digest {
		t.Fatalf("fallback evidence = %+v", res.Evidence)
	}
	body, _ := json.Marshal(res)
	if !strings.Contains(string(body), `"exit_code":9`) ||
		!strings.Contains(string(body), `"output_bytes":42`) ||
		!strings.Contains(string(body), digest) {
		t.Fatalf("operator JSON lacks fallback evidence:\n%s", body)
	}
}

func TestExecuteAmbiguousEvidenceKeepsGenuineZero(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		result:  &oc.LaunchResult{ExitCode: 0, Output: ""},
		launchErr: &oc.LaunchError{
			InspectFailure:   oc.InspectUnavailable,
			InferenceUnknown: true,
			OutputBytes:      0,
		},
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.LaunchState != SessionAmbiguous {
		t.Fatalf("state = %q, want ambiguous", res.LaunchState)
	}
	if res.Evidence.ExitCode != 0 || res.Evidence.OutputBytes != 0 {
		t.Fatalf("genuine zero evidence = %+v", res.Evidence)
	}
	if res.Evidence.OutputSHA256 != "" {
		t.Fatalf("empty capture invented a digest: %q", res.Evidence.OutputSHA256)
	}
}

func TestExecuteSuccessfulCommandFailedInspectionIsAmbiguous(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	const output = "ok"
	rt := &fakeRuntime{
		allowed: map[string]bool{"mpm-worker": true},
		result:  &oc.LaunchResult{ExitCode: 0, Output: output},
		launchErr: &oc.LaunchError{
			InspectFailure:   oc.InspectUnavailable,
			InferenceUnknown: true,
		},
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.LaunchState != SessionAmbiguous {
		t.Fatalf("state = %q, want ambiguous", res.LaunchState)
	}
	if res.Evidence.ExitCode != 0 || res.Evidence.OutputBytes != len(output) {
		t.Fatalf("evidence = %+v", res.Evidence)
	}
	sum := sha256.Sum256([]byte(output))
	if res.Evidence.OutputSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q", res.Evidence.OutputSHA256)
	}
}

func TestExecuteAmbiguousEvidenceDoesNotInventUnknownValues(t *testing.T) {
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
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Evidence.OutputSHA256 != "" || res.Evidence.OutputBytes != 0 {
		t.Fatalf("invented output evidence = %+v", res.Evidence)
	}
	if !strings.Contains(res.Evidence.Reason, "could not start") {
		t.Fatalf("reason does not name the known failure: %q", res.Evidence.Reason)
	}
}
