package opencode

// subprocess_test.go — the bounded subprocess lifecycle (B1) and the
// credential-safe read errors (B2), driven with synthetic frameworks.
//
// The descendant here is a REAL child process of the fake framework that
// inherits its output pipe, which is exactly the shape that made Wait
// block: `exec.CommandContext` kills only the direct child, so a
// descendant holding the pipe used to keep the run alive until IT exited.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// descendantToken is distinctive enough that a /proc cmdline scan can find
// the exact process, and long enough that a CI hiccup cannot confuse it.
const descendantToken = "sleep 97"

// secretSentinel is an unfamiliar credential-shaped value: nothing in the
// codebase knows this name, so a leak of ANY captured text would carry it.
const (
	secretName  = "OAUTH_TOKEN"
	secretValue = "super-secret-sentinel-value"
)

// descendantAlive scans /proc for a process whose cmdline contains the
// token. Linux only; the test skips the check elsewhere.
func descendantAlive(t *testing.T, token string) bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("descendant accounting is proven on Linux")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("read /proc: %v", err)
	}
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ReplaceAll(string(raw), "\x00", " "), token) {
			return true
		}
	}
	return false
}

// isPID reports whether the directory name is a numeric process id.
func isPID(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// waitForDescendantGone polls until the descendant is gone.
func waitForDescendantGone(t *testing.T, token string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !descendantAlive(t, token) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a descendant (%q) survived the terminated run", token)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// timeoutFramework writes a synthetic framework whose `run` spawns a
// descendant that inherits the output pipe and outlives the run, then
// sleeps past any reasonable deadline.
func timeoutFramework(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*)
    echo '[{"id":"mpm-worker","mode":"primary","permissions":[]},{"id":"mpm-reviewer","mode":"primary","permissions":[]}]'
    exit 0;;
  *"session.list"*)
    echo '{"data":[]}'; exit 0;;
  *"session.message.list"*)
    echo '` + secretName + `=` + secretValue + `' >&2; exit 1;;
  *"run"*)
    ` + descendantToken + ` &
    echo "started" > "` + marker + `"
    sleep 30
    exit 0;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// secretFramework writes a synthetic framework whose `debug agents` and
// `session.message.list` fail after printing an unfamiliar credential.
func secretFramework(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*)
    echo '` + secretName + `=` + secretValue + `' >&2; exit 1;;
  *"session.message.list"*)
    echo '` + secretName + `=` + secretValue + `' >&2; exit 1;;
  *"run"*) echo '` + secretName + `=` + secretValue + `' >&2; exit 1;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestLaunchDeadlineTerminatesTheProcessGroupIsB1 proves a worker run that
// blows its deadline returns bounded — even with a descendant holding the
// output pipe — and that the descendant does NOT outlive the terminated
// run.
func TestLaunchDeadlineTerminatesTheProcessGroupIsB1(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "launch.txt")
	bin := timeoutFramework(t, marker)

	rt, err := NewRuntime(Config{
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
	_, launchErr := rt.Launch(context.Background(), LaunchRequest{
		SessionID: "ses_b1_worker",
		Agent:     "mpm-worker",
		Workspace: workspace,
		Title:     "b1",
		Prompt:    "hang",
	})
	elapsed := time.Since(start)
	if launchErr == nil {
		t.Fatal("a run past its deadline succeeded")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the launch returned after %s; the deadline did not bound it", elapsed)
	}
	var le *LaunchError
	if !errors.As(launchErr, &le) || le.RunFailure != LaunchDeadlineExceeded {
		t.Fatalf("the launch error is %v, want %q", launchErr, LaunchDeadlineExceeded)
	}
	waitForDescendantGone(t, descendantToken)
}

// TestReviewDeadlineTerminatesTheProcessGroupIsB1 proves the reviewer path
// has the same bounding.
func TestReviewDeadlineTerminatesTheProcessGroupIsB1(t *testing.T) {
	repo := mustGitRepo(t)
	marker := filepath.Join(t.TempDir(), "review.txt")
	bin := timeoutFramework(t, marker)

	rt, err := NewRuntime(Config{
		Root:          t.TempDir(),
		OpenCodeBin:   bin,
		Model:         "provider/test-model",
		RepoPath:      repo,
		AllowedAgents: []string{"mpm-reviewer"},
		Timeout:       500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, reviewErr := rt.Review(context.Background(), ReviewRequest{
		SessionID: "ses_b1_review",
		Brief:     "hang",
		Title:     "b1",
	})
	elapsed := time.Since(start)
	if reviewErr == nil {
		t.Fatal("a review past its deadline succeeded")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the review returned after %s; the deadline did not bound it", elapsed)
	}
	if !errors.Is(reviewErr, ErrReviewIncomplete) {
		t.Fatalf("the review error is %v, want ErrReviewIncomplete", reviewErr)
	}
	waitForDescendantGone(t, descendantToken)
}

// TestLaunchRespectsCallerCancellationIsB1 proves an earlier caller
// cancellation terminates the run and the group, without waiting for the
// configured deadline.
func TestLaunchRespectsCallerCancellationIsB1(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "launch.txt")
	bin := timeoutFramework(t, marker)

	rt, err := NewRuntime(Config{
		Root:          t.TempDir(),
		OpenCodeBin:   bin,
		Model:         "provider/test-model",
		AllowedAgents: []string{"mpm-worker"},
		Timeout:       5 * time.Minute, // the deadline is NOT what stops it
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = rt.Launch(ctx, LaunchRequest{
			SessionID: "ses_b1_cancel",
			Agent:     "mpm-worker",
			Workspace: workspace,
			Title:     "b1 cancel",
			Prompt:    "hang",
		})
	}()
	// Wait until the framework is actually running, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the framework never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the launch did not return after the caller cancelled")
	}
	waitForDescendantGone(t, descendantToken)
}

// TestReadCommandsAreBoundedIsB1 proves the read-only commands carry their
// own deadline and respect caller cancellation.
func TestReadCommandsAreBoundedIsB1(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*) sleep 30; exit 0;;
  *"run"*) exit 0;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(Config{
		Root:          t.TempDir(),
		OpenCodeBin:   bin,
		AllowedAgents: []string{"mpm-worker"},
		ReadTimeout:   500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	verifyErr := rt.VerifyAgent(context.Background(), "mpm-worker")
	elapsed := time.Since(start)
	if verifyErr == nil {
		t.Fatal("a read command past its deadline was accepted")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the read returned after %s; the deadline did not bound it", elapsed)
	}
	var rce *readCommandError
	if !errors.As(verifyErr, &rce) {
		t.Fatalf("the read error is %T, want a bounded read-command error", verifyErr)
	}
}

// TestReadErrorsNeverCarryCapturedOutputIsB2 is the adversarial B2 test:
// the framework prints an unfamiliar credential to stderr and fails; the
// returned error must carry the classification and a digest, and must NOT
// carry the value — through the error, its Unwrap chain, or the wrapper.
func TestReadErrorsNeverCarryCapturedOutputIsB2(t *testing.T) {
	bin := secretFramework(t)
	rt, err := NewRuntime(Config{
		Root:          t.TempDir(),
		OpenCodeBin:   bin,
		AllowedAgents: []string{"mpm-worker", "mpm-reviewer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	leaked := func(err error) bool {
		if err == nil {
			return false
		}
		return strings.Contains(err.Error(), secretValue) ||
			strings.Contains(err.Error(), secretName)
	}
	if err := rt.VerifyAgent(context.Background(), "mpm-worker"); err == nil {
		t.Fatal("a failing read command was accepted")
	} else if leaked(err) {
		t.Fatalf("VerifyAgent leaked captured output: %v", err)
	} else if !strings.Contains(err.Error(), "sha256:") {
		t.Fatalf("the read error carries no bounded-output digest: %v", err)
	}
	if _, err := rt.Inspect(context.Background(), "ses_any"); err == nil {
		t.Fatal("a failing session list was accepted")
	} else if leaked(err) {
		t.Fatalf("Inspect leaked captured output: %v", err)
	}
	if _, err := rt.Review(context.Background(), ReviewRequest{
		SessionID: "ses_b2",
		Brief:     "review",
		Title:     "b2",
	}); err == nil {
		t.Fatal("a failing message stream was accepted")
	} else if leaked(err) {
		t.Fatalf("Review leaked captured output: %v", err)
	}
}

// TestSuccessfulOrdinaryRunIsNotTerminatedIsB1 proves the normal path is
// untouched: a run that completes while a framework descendant lingers
// still succeeds (bounded by WaitDelay), and nothing is signalled.
func TestSuccessfulOrdinaryRunIsNotTerminatedIsB1(t *testing.T) {
	workspace := t.TempDir()
	sessionFile := filepath.Join(t.TempDir(), "session")
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*)
    echo '[{"id":"mpm-worker","mode":"primary","permissions":[]}]'; exit 0;;
  *"session.list"*)
    sess=$(cat "` + sessionFile + `" 2>/dev/null)
    echo '{"data":[{"id":"'"$sess"'","agent":"mpm-worker","model":{"id":"test-model","providerID":"provider"},"outcome":"succeeded","location":{"directory":"` + workspace + `"},"permissions":[]}]}'
    exit 0;;
  *"run"*)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--session" ]; then echo "$a" > "` + sessionFile + `"; fi
      prev="$a"
    done
    sleep 2 &          # a descendant that OUTLIVES the run
    echo "ran"
    exit 0;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(Config{
		Root:          t.TempDir(),
		OpenCodeBin:   bin,
		Model:         "provider/test-model",
		AllowedAgents: []string{"mpm-worker"},
		Timeout:       time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, launchErr := rt.Launch(context.Background(), LaunchRequest{
		SessionID: "ses_b1_normal",
		Agent:     "mpm-worker",
		Workspace: workspace,
		Title:     "normal",
		Prompt:    "work",
	})
	elapsed := time.Since(start)
	if launchErr != nil {
		t.Fatalf("an ordinary run failed: %v", launchErr)
	}
	if !res.InferredOK || res.State == nil || res.State.Outcome != "succeeded" {
		t.Fatalf("the ordinary run is %+v", res)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("a normal run whose descendant held the pipe returned after %s", elapsed)
	}
}

// mustGitRepo creates a disposable one-commit repository.
func mustGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "--quiet", "-m", "initial")
	return repo
}
