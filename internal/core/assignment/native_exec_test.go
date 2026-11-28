package assignment

// native_exec_test.go — the FULL lifecycle through the REAL runtime.
//
// The runtime under test is `opencode.Runtime`, driven by a synthetic agent
// framework, not a fake implementing the interface. There is no bubblewrap,
// no user namespace, no network namespace, no sidecar and no relay: MPM
// invokes the framework as an ordinary child process.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// nativeFramework writes a synthetic agent framework for the worker path.
//
// It answers the debug/agents and session.list reads the adapter uses and
// records each launch in marker. When outcome is empty, session.list
// returns NO session, which is how a launch that left no record behaves.
func nativeFramework(t *testing.T, workspace, outcome, agent, model, marker string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	sessionFile := filepath.Join(dir, "session")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*)
    echo '[{"id":"mpm-worker","mode":"primary","permissions":[]},{"id":"mpm-reviewer","mode":"primary","permissions":[]}]'
    exit 0;;
  *"session.list"*)
    sess=$(cat "` + sessionFile + `" 2>/dev/null)
    if [ -z "` + outcome + `" ] || [ -z "$sess" ]; then echo '{"data":[]}'; exit 0; fi
    echo '{"data":[{"id":"'"$sess"'","agent":"` + agent + `","model":{"id":"` + model + `","providerID":"provider"},"outcome":"` + outcome + `","location":{"directory":"` + workspace + `"},"permissions":[]}]}'
    exit 0;;
  *"run"*)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--session" ]; then echo "$a" > "` + sessionFile + `"; fi
      prev="$a"
    done
    echo "ran" > "` + marker + `"
    echo "synthetic launch output that must never be copied into a diagnostic"
    exit 0;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// nativeWorkerRuntime builds the real runtime MPM now uses.
func nativeWorkerRuntime(t *testing.T, bin, root, model string) *oc.Runtime {
	t.Helper()
	rt, err := oc.NewRuntime(oc.Config{
		Root:          root,
		OpenCodeBin:   bin,
		Model:         model,
		AllowedAgents: []string{"mpm-worker"},
	})
	if err != nil {
		t.Fatalf("build the runtime: %v", err)
	}
	return rt
}

// TestNativeExecutionPreservesTheLifecycle proves a launch drives the
// assignment from in_progress to completed through the same attempt,
// evidence and ledger rules as before the containment removal.
func TestNativeExecutionPreservesTheLifecycle(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	mustDo(t, func() (*Assignment, error) {
		return store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor})
	})
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "launch.txt")
	bin := nativeFramework(t, workspace, "succeeded", "mpm-worker", "test-model", marker)

	rt := nativeWorkerRuntime(t, bin, t.TempDir(), "provider/test-model")
	res, err := store.Execute(context.Background(), rt, ExecuteParams{
		ID:                 id,
		Epoch:              epoch,
		Actor:              actor,
		Agent:              "mpm-worker",
		Workspace:          workspace,
		ExpectedRuntimeDir: workspace,
		WorkspaceID:        "ws",
		Model:              "provider/test-model",
		Title:              "native run",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Attempt == nil || res.Attempt.LaunchState != SessionCompleted {
		t.Fatalf("the attempt is %+v, want completed", res.Attempt)
	}
	if res.RuntimeOutcome != "succeeded" {
		t.Fatalf("runtime outcome %q, want succeeded", res.RuntimeOutcome)
	}
	if res.Assignment == nil || res.Assignment.State != StateCompleted {
		t.Fatalf("the assignment is %+v, want completed", res.Assignment)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the framework never ran: %v", err)
	}
	events := eventTypes(t, store, id)
	if len(events) < 2 || events[len(events)-1] != EventCompleted {
		t.Fatalf("the ledger ends %v, want a completed event", events)
	}
}

// TestNativeRuntimeFailurePreservesAmbiguityAndDiagnostics proves a launch
// that leaves no trustworthy state is AMBIGUOUS with the structured R2
// diagnostics, and the assignment is not silently completed.
func TestNativeRuntimeFailurePreservesAmbiguityAndDiagnostics(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	mustDo(t, func() (*Assignment, error) {
		return store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor})
	})
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "launch.txt")
	bin := nativeFramework(t, workspace, "", "mpm-worker", "test-model", marker)

	rt := nativeWorkerRuntime(t, bin, t.TempDir(), "provider/test-model")
	res, err := store.Execute(context.Background(), rt, ExecuteParams{
		ID:                 id,
		Epoch:              epoch,
		Actor:              actor,
		Agent:              "mpm-worker",
		Workspace:          workspace,
		ExpectedRuntimeDir: workspace,
		WorkspaceID:        "ws",
		Model:              "provider/test-model",
		Title:              "native run",
	})
	if err != nil {
		t.Fatalf("the adapter returned a transport error instead of recording ambiguity: %v", err)
	}
	if res.Attempt == nil || res.Attempt.LaunchState != SessionAmbiguous {
		t.Fatalf("the attempt is %+v, want ambiguous", res.Attempt)
	}
	if res.Assignment == nil || res.Assignment.State != StateInProgress {
		t.Fatalf("the assignment moved to %+v; a failed launch must not complete it", res.Assignment)
	}
	if res.Evidence.Reason == "" {
		t.Fatalf("the failure carries no structured diagnostic: %+v", res.Evidence)
	}
	if strings.Contains(res.Evidence.Reason, "synthetic launch output") {
		t.Fatalf("the diagnostic carries raw output: %q", res.Evidence.Reason)
	}
	if res.Evidence.OutputSHA256 == "" {
		t.Fatalf("the diagnostic has no bounded-output digest: %+v", res.Evidence)
	}
}

// TestNativeExecutionIsStillFenced proves the claim/epoch fence is
// unchanged: a stale epoch is refused BEFORE the framework runs.
func TestNativeExecutionIsStillFenced(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "launch.txt")
	bin := nativeFramework(t, workspace, "succeeded", "mpm-worker", "test-model", marker)

	rt := nativeWorkerRuntime(t, bin, t.TempDir(), "provider/test-model")
	_, err := store.Execute(context.Background(), rt, ExecuteParams{
		ID:                 id,
		Epoch:              epoch + 7, // not the held epoch
		Actor:              actor,
		Agent:              "mpm-worker",
		Workspace:          workspace,
		ExpectedRuntimeDir: workspace,
		WorkspaceID:        "ws",
		Model:              "provider/test-model",
		Title:              "stale run",
	})
	if err == nil {
		t.Fatal("a stale epoch was accepted")
	}
	if !errors.Is(err, ErrStaleClaim) && !errors.Is(err, ErrConstraint) {
		t.Fatalf("the refusal is %v, want a claim/epoch refusal", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the framework ran despite the stale epoch")
	}
}

// TestTimeoutRecordsAmbiguityAndTerminatesTheRun proves a run that blows
// its deadline is recorded as AMBIGUOUS (the framework may have done work),
// the assignment stays in_progress, and the surviving descendant is
// terminated rather than left holding the adapter's output pipe.
func TestTimeoutRecordsAmbiguityAndTerminatesTheRun(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	mustDo(t, func() (*Assignment, error) {
		return store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor})
	})
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "launch.txt")

	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	sessionFile := filepath.Join(dir, "session")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*)
    echo '[{"id":"mpm-worker","mode":"primary","permissions":[]}]'; exit 0;;
  *"session.list"*) echo '{"data":[]}'; exit 0;;
  *"run"*)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--session" ]; then echo "$a" > "` + sessionFile + `"; fi
      prev="$a"
    done
    sleep 97 &
    echo "started" > "` + marker + `"
    sleep 30
    exit 0;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := oc.NewRuntime(oc.Config{
		Root:          t.TempDir(),
		OpenCodeBin:   bin,
		Model:         "provider/test-model",
		AllowedAgents: []string{"mpm-worker"},
		Timeout:       500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, execErr := store.Execute(context.Background(), rt, ExecuteParams{
		ID:                 id,
		Epoch:              epoch,
		Actor:              actor,
		Agent:              "mpm-worker",
		Workspace:          workspace,
		ExpectedRuntimeDir: workspace,
		WorkspaceID:        "ws",
		Model:              "provider/test-model",
		Title:              "timeout run",
	})
	elapsed := time.Since(start)
	if execErr != nil {
		t.Fatalf("a timed-out run was refused instead of recorded: %v", execErr)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the run returned after %s; the deadline did not bound it", elapsed)
	}
	if res.Attempt == nil || res.Attempt.LaunchState != SessionAmbiguous {
		t.Fatalf("the attempt is %+v, want ambiguous", res.Attempt)
	}
	if res.Assignment == nil || res.Assignment.State != StateInProgress {
		t.Fatalf("the assignment moved to %+v; an ambiguous run must not complete it", res.Assignment)
	}
	if res.Evidence.Reason == "" || !strings.Contains(res.Evidence.Reason, "deadline") {
		t.Fatalf("the diagnostic does not name the deadline: %+v", res.Evidence)
	}
	// THE DESCENDANT IS GONE: the terminated group cannot outlive the
	// accounting.
	waitForDescendantGone(t)
}

// waitForDescendantGone polls /proc until the test's descendant process is
// gone (Linux only; the check skips elsewhere).
func waitForDescendantGone(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("descendant accounting is proven on Linux")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !descendantSleeping(t) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a descendant of the terminated run is still alive")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// descendantSleeping reports whether a `sleep 97` process is alive.
func descendantSleeping(t *testing.T) bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("read /proc: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "" || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ReplaceAll(string(raw), "\x00", " "), "sleep 97") {
			return true
		}
	}
	return false
}
