package opencode

// native_review_test.go — the reviewer path with no containment at all.
//
// The reviewer runs under the framework's own definition, reads a
// disposable snapshot of the exact revision, and the HOST captures the
// findings. No plugin, no sandbox_exec, no sidecar, no namespace.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReviewRunsWithoutContainment proves Review works without the plugin
// path, the reviewer credential or any sandbox: a synthetic framework
// answers the message stream and the host derives the evidence.
func TestReviewRunsWithoutContainment(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("commit", "--quiet", "-m", "initial")
	head := strings.TrimSpace(mustOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))

	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "review.txt")
	sessionFile := filepath.Join(t.TempDir(), "session")
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*)
    echo '[{"id":"mpm-reviewer","mode":"primary","permissions":[]}]'; exit 0;;
  *"session.message.list"*)
    echo '{"data":[{"type":"assistant","content":[{"type":"text","text":"the review found nothing"}]}]}'; exit 0;;
  *"session.list"*)
    sess=$(cat "` + sessionFile + `" 2>/dev/null)
    echo '{"data":[{"id":"'"$sess"'","agent":"mpm-reviewer","model":{"id":"test-model","providerID":"provider"},"outcome":"succeeded","location":{"directory":"` + repo + `"},"permissions":[]}]}'
    exit 0;;
  *"run"*)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--session" ]; then echo "$a" > "` + sessionFile + `"; fi
      prev="$a"
    done
    { echo "cwd=$PWD"; echo "probe=$MPM_NATIVE_PROBE"; } > "` + marker + `"
    exit 0;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MPM_NATIVE_PROBE", "ambient-environment")
	rt, err := NewRuntime(Config{
		Root:          dir,
		OpenCodeBin:   bin,
		Model:         "provider/test-model",
		RepoPath:      repo,
		AllowedAgents: []string{"mpm-reviewer"},
		Timeout:       30 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the reviewer runtime: %v", err)
	}
	res, err := rt.Review(context.Background(), ReviewRequest{
		SessionID:        "ses_native_review",
		Brief:            "review the change",
		Title:            "native review",
		ExpectedRevision: head,
	})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if res.Outcome != "succeeded" || res.ReviewedRevision != head {
		t.Fatalf("review result: %+v", res)
	}
	if !strings.Contains(res.Findings, "the review found nothing") {
		t.Fatalf("findings were not captured: %q", res.Findings)
	}
	if _, err := os.Stat(res.EvidencePath); err != nil {
		t.Fatalf("the host did not write the evidence: %v", err)
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the synthetic reviewer never ran: %v", err)
	}
	// The reviewer ran in the exact-revision SNAPSHOT, with the ambient
	// environment (no manufactured isolation).
	if !strings.Contains(string(body), "probe=ambient-environment") {
		t.Fatalf("the reviewer did not receive the ambient environment:\n%s", body)
	}
	if strings.Contains(string(body), "cwd="+repo+"\n") {
		t.Fatalf("the reviewer ran in the operator's checkout, not the snapshot:\n%s", body)
	}
	// The snapshot is a DISPOSABLE copy: it lives under the runtime root.
	if !strings.Contains(string(body), "cwd="+filepath.Join(dir, "snapshot")) {
		t.Fatalf("the reviewer did not run in the pinned snapshot:\n%s", body)
	}
}

// mustOutput runs a command and returns its stdout.
func mustOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out)
}

// TestRuntimeNeedsNoContainmentPrerequisites proves the runtime constructs
// with only a state directory and a binary: no MPM binary for a relay, no
// sidecar socket, no reviewer credential, no plugin path.
func TestRuntimeNeedsNoContainmentPrerequisites(t *testing.T) {
	rt, err := NewRuntime(Config{
		Root:          t.TempDir(),
		OpenCodeBin:   "/bin/true",
		RepoPath:      t.TempDir(),
		AllowedAgents: []string{"mpm-worker", "mpm-reviewer"},
	})
	if err != nil {
		t.Fatalf("the runtime demanded a containment prerequisite: %v", err)
	}
	if _, err := os.Stat(rt.Config().Root); err != nil {
		t.Fatalf("the state directory was not created: %v", err)
	}
}
