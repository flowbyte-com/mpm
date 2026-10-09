package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Git provenance must never be inferred from ambient process state.
//
// Before the fail-closed rule, the snapshot helper walked a chain of
// ambient candidates — the MPM workspace, then the process cwd — and returned
// the first that happened to be inside a Git worktree. Because a `git`
// evidence row classifies as AUDIT, recording one promoted
// DeriveWorkVerification to `partial`. The observable consequence was that
// simply running MPM from inside ANY unrelated repository asserted, on that
// repository's behalf, that an arbitrary work item had been observed.
//
// Each test below uses repos with DISTINCT commit messages and DISTINCT
// filenames. That matters: an assertion that merely checks "some HEAD was
// recorded" would still pass while the wrong repository supplied it. Asserting
// that repo A's marker is present and repo B's is absent makes false
// attribution directly observable rather than inferred.
//
// These tests pin PRODUCT behaviour through the real work lifecycle. The
// snapshot helper itself (CaptureGitSnapshot and its private core) had zero
// production callers and has been removed; the guards below never called it.

// newTestRepo creates a Git repository whose content is uniquely identifiable.
//
// markerFile is written and committed, and markerCommit is the commit
// message, so both the repository's HEAD and its changed-file list can be
// attributed to this repo and to no other in the test.
func newTestRepo(t *testing.T, markerFile, markerCommit string) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	// t.TempDir() can hand back a path under a symlinked ancestor (/tmp is a
	// symlink on some systems), and Git reports the resolved physical path
	// as toplevel. Resolve once so path comparisons below are exact.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@example.invalid")
	gitRun(t, dir, "config", "user.name", "MPM Test")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, markerFile), []byte(markerCommit), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", markerCommit)
	return dir
}

// newDirtyTestRepo creates a repo with one committed marker file plus one
// uncommitted working-tree change, so both HEAD and ChangedFiles are
// distinguishable from any other repo.
func newDirtyTestRepo(t *testing.T, markerFile, markerCommit, dirtyFile string) string {
	t.Helper()
	dir := newTestRepo(t, markerFile, markerCommit)
	if err := os.WriteFile(filepath.Join(dir, dirtyFile), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	return dir
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func repoHead(t *testing.T, dir string) string {
	t.Helper()
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// E. Without an explicit repository, no Git evidence row is written.
//
// This test previously called dm.recordGitEvidenceForWork. That method is
// gone: it was a guaranteed no-op, so calling it asserted only that the
// no-op did nothing. What actually matters is the PRODUCT behaviour — that
// MPM's work lifecycle writes no `git` evidence row when the work has no
// repository identity — so this test now drives the real path (add +
// complete) instead of the removed plumbing. The invariant, not the old
// function name, is what is pinned here.
func TestWorkLifecycle_NoExplicitRepoWritesNoGitEvidence(t *testing.T) {
	// Run from a real repository so a cwd-based implementation would have
	// something to wrongly attribute.
	t.Chdir(newTestRepo(t, "e-marker.txt", "e commit"))

	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Work with no repository identity", "Body", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Drive the production lifecycle path the removed method used to hang
	// off. If any of these transitions ever re-admit ambient Git capture,
	// this is where it shows.
	if _, err := dm.CompleteWorkWithContext(w.ID, "Done", ActiveContext{}); err != nil {
		t.Fatalf("CompleteWorkWithContext: %v", err)
	}

	evidence, err := ListEvidenceForArtifact(dm, w.ID, "work")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	for _, e := range evidence {
		if e.SourceGroup == "git" {
			t.Errorf("recorded a git evidence row (%q) with no explicit repository", e.Notes)
		}
	}
}

// F. Absence of Git evidence does not break work completion.
//
// The no-op must not become a functional regression: the item still reaches
// its terminal status through the normal path.
func TestWork_CompletionSucceedsWithoutGitEvidence(t *testing.T) {
	t.Chdir(newTestRepo(t, "f-marker.txt", "f commit"))

	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Complete without git evidence", "Body", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	completed, err := dm.CompleteWorkWithContext(w.ID, "Done", ActiveContext{})
	if err != nil {
		t.Fatalf("CompleteWorkWithContext: %v", err)
	}
	if completed.Status != WorkStatusDone {
		t.Errorf("Status = %v, want done", completed.Status)
	}
	if completed.Verification != WorkVerificationUnverified {
		t.Errorf("Verification = %v, want unverified (absence of evidence is not a failure)", completed.Verification)
	}
}

// G. Verification cannot be promoted by unrelated cwd Git state.
//
// This is the harm the old chain caused: a `git` row classifies as AUDIT,
// and audit-without-outcome derives `partial`.
//
// The removed recordGitEvidenceForWork used to be the thing under test; the
// assertion below is stronger without it. Instead of asking "does the no-op
// avoid recording?", this asks "does the real lifecycle avoid the false
// promotion?" — run the production path under an unrelated Git cwd and
// require that verification stays unverified.
func TestDeriveWorkVerification_NotPromotedByUnrelatedCwdGitState(t *testing.T) {
	t.Chdir(newTestRepo(t, "g-marker.txt", "g commit"))

	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Work item under an unrelated repo", "Body", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Production path: complete the item, which internally re-derives
	// verification. If ambient Git capture were ever reinstated, the
	// fabricated `git` row would land here and flip the result to partial.
	if _, err := dm.CompleteWorkWithContext(w.ID, "Done", ActiveContext{}); err != nil {
		t.Fatalf("CompleteWorkWithContext: %v", err)
	}

	derived, err := dm.DeriveWorkVerification(w.ID)
	if err != nil {
		t.Fatalf("DeriveWorkVerification: %v", err)
	}
	if derived == WorkVerificationPartial {
		t.Errorf("Verification = partial; the unrelated cwd's git state was admitted as evidence")
	}
	if derived != WorkVerificationUnverified {
		t.Errorf("Verification = %v, want unverified", derived)
	}
}

// H. An unrelated repository's HEAD and filenames must not surface anywhere
// in the work's recorded state, across a full completion flow.
func TestWork_UnrelatedRepoStateNeverAppearsInEvidence(t *testing.T) {
	unrelated := newDirtyTestRepo(t, "h-marker.txt", "h commit", "h-dirty.txt")
	unrelatedHead := repoHead(t, unrelated)
	t.Chdir(unrelated)

	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Isolated work item", "Body", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	if _, err := dm.CompleteWorkWithContext(w.ID, "Done", ActiveContext{}); err != nil {
		t.Fatalf("CompleteWorkWithContext: %v", err)
	}

	evidence, err := ListEvidenceForArtifact(dm, w.ID, "work")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	for _, e := range evidence {
		if strings.Contains(e.Notes, unrelatedHead) {
			t.Errorf("evidence notes %q contain the unrelated repo's HEAD %s", e.Notes, unrelatedHead)
		}
		for _, marker := range []string{"h-marker.txt", "h-dirty.txt"} {
			if strings.Contains(e.Notes, marker) {
				t.Errorf("evidence notes %q contain the unrelated repo's file %q", e.Notes, marker)
			}
		}
	}

	// And the same must hold for the work item as a whole.
	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	blob := reloaded.Title + reloaded.Content
	if strings.Contains(blob, unrelatedHead) || strings.Contains(blob, "h-dirty.txt") {
		t.Errorf("work content absorbed the unrelated repo's state: %q", blob)
	}
}
