package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// The fixture's temporary roots
// ---------------------------------------------------------------------

// TestFixtureRootCleanupRemovesOnlyItsOwnRoot proves the cleanup
// removes the roots the fixture created and nothing else.
//
// The dangerous shape is a cleanup that sweeps the temporary directory
// for anything resembling an MPM temporary directory: one day it
// removes an operator's directory. Cleanup removes a path it created,
// and it PROVES it with the sentinel.
func TestFixtureRootCleanupRemovesOnlyItsOwnRoot(t *testing.T) {
	work := t.TempDir()

	root, err := newFixtureRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, fixtureRootSentinel)); err != nil {
		t.Fatalf("the fixture root carries no ownership sentinel: %v", err)
	}
	if err := removeFixtureRoot(root); err != nil {
		t.Fatalf("the fixture root was not removed: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the fixture root still exists after cleanup: %v", err)
	}

	// A directory that LOOKS like a fixture root but was not created by
	// the fixture must survive.
	lookalike := filepath.Join(work, fixtureRootPrefix+"lookalike")
	if err := os.MkdirAll(lookalike, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(lookalike, "operator-file.txt")
	if err := os.WriteFile(marker, []byte("mine\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := removeFixtureRoot(lookalike); err == nil {
		t.Error("cleanup accepted a directory it cannot prove it owns")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup removed a directory it did not create: %v", err)
	}
}

// TestFixtureRootCleanupAfterASetupFailure proves a fixture whose
// preparation FAILED leaves nothing behind.
//
// A partially prepared fixture is exactly the scratch that
// accumulates: the leak this work removes started with a root that was
// created and then abandoned.
func TestFixtureRootCleanupAfterASetupFailure(t *testing.T) {
	for _, stage := range []string{"root", "copy"} {
		t.Run(stage, func(t *testing.T) {
			work := t.TempDir()
			devSourceFailurePoint = stage
			defer func() { devSourceFailurePoint = "" }()

			registeredBefore := registeredFixtureRootCount()
			if _, err := prepareDevSourceIn(work); err == nil {
				t.Fatal("the injected setup failure did not fail the setup")
			}
			entries, err := os.ReadDir(work)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				names := []string{}
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("a failed setup left scratch behind: %v", names)
			}
			// And the failed setup's root is not left registered for a
			// later cleanup. The SHARED fixture's root may be
			// registered — it is cleaned when the test process ends —
			// so the count is compared, not asserted to be zero.
			if got := registeredFixtureRootCount(); got != registeredBefore {
				t.Errorf("a failed setup left %d root(s) registered for cleanup", got-registeredBefore)
			}
		})
	}
}

// TestRepeatedFixtureRunsDoNotAccumulateScratch proves the fixture's
// roots do not pile up when the suite runs repeatedly.
//
// The accumulation the acceptance exercise saw was exactly this: a
// host that ran the suite over and over kept every scratch root.
func TestRepeatedFixtureRunsDoNotAccumulateScratch(t *testing.T) {
	work := t.TempDir()
	for i := 0; i < 5; i++ {
		root, err := newFixtureRoot(work)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("run %d: the fixture root was not created: %v", i, err)
		}
		if err := removeFixtureRoot(root); err != nil {
			t.Fatalf("run %d: the fixture root was not removed: %v", i, err)
		}
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("repeated fixture runs accumulated %d scratch root(s)", len(entries))
	}
}

// ---------------------------------------------------------------------
// The fixture's copy boundary
// ---------------------------------------------------------------------

// fixtureRepo creates a small git repository that stands in for a
// checkout, and returns its path.
//
// It is a REAL repository: the copy's boundary is the tracked file set,
// and a directory that is not a checkout has no bounded file set.
func fixtureRepo(t *testing.T, dir string) string {
	t.Helper()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"README.md":              "# fixture\n",
		"cmd/mpm/main.go":        "package main\n",
		"agent_installation/x/a": "alpha\n",
	}
	for name, content := range files {
		full := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// A build artefact that must NOT travel: the compiled payload is
	// the build's output, not the source's. It is gitignored, exactly
	// as it is in the real checkout.
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("dist/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "agent_installation", "x", "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "agent_installation", "x", "dist", "index.v2.js"), []byte("compiled\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// An untracked file: the copy's boundary is the TRACKED file set,
	// which is what a fresh clone carries. It is created AFTER the
	// commit, so it is genuinely untracked.
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "fixture"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...) //nolint:gosec
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("local\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// countFiles counts the regular files under dir, stopping after limit.
func countFiles(t *testing.T, dir string, limit int) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.Mode().IsRegular() {
			n++
			if n >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return n
}

// runBoundedCopy runs a fixture copy and STOPS it if it writes more
// than maxFiles.
//
// The stop is a tripwire: the destination is replaced by a regular
// file, so the copy's next mkdir fails and it returns. That keeps a
// runaway copy bounded — it cannot fill the filesystem — while still
// showing that it did not finish.
//
// A copy that finishes within the cap is a copy that was bounded.
func runBoundedCopy(t *testing.T, copy func() error, dst string, maxFiles int, timeout time.Duration) (err error, files int, runaway bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- copy() }()

	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err = <-done:
			return err, countFiles(t, dst, maxFiles+1), false
		case <-ticker.C:
			if countFiles(t, dst, maxFiles+1) > maxFiles {
				// Tripwire: make the destination impossible to write.
				_ = os.RemoveAll(dst)
				_ = os.WriteFile(dst, []byte("tripwire\n"), 0644)
				select {
				case err = <-done:
					return err, maxFiles + 1, true
				case <-time.After(10 * time.Second):
					return err, maxFiles + 1, true
				}
			}
		case <-deadline:
			return err, countFiles(t, dst, maxFiles+1), true
		}
	}
}

// TestFixtureCopyWithTheDestinationOutsideTheSource proves the normal
// shape: the destination lives elsewhere, and the copy carries the
// source's files and nothing else.
func TestFixtureCopyWithTheDestinationOutsideTheSource(t *testing.T) {
	work := t.TempDir()
	repo := fixtureRepo(t, work)
	dst := filepath.Join(work, "bundle", "checkout")

	if err := copySourceTreeFrom(repo, dst); err != nil {
		t.Fatalf("the fixture copy failed: %v", err)
	}
	for _, want := range []string{"README.md", "cmd/mpm/main.go", "agent_installation/x/a"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(want))); err != nil {
			t.Errorf("the copy does not carry %s: %v", want, err)
		}
	}
	// The build's output must not travel: a fixture that carried it
	// would let a broken builder pass.
	if _, err := os.Stat(filepath.Join(dst, "agent_installation", "x", "dist", "index.v2.js")); err == nil {
		t.Error("the copy carried the compiled output")
	}
	// Nor an untracked file: the boundary is the tracked file set,
	// which is what a fresh clone carries.
	if _, err := os.Stat(filepath.Join(dst, "untracked.txt")); err == nil {
		t.Error("the copy carried an untracked file")
	}
	// The repository's own metadata must not travel.
	if _, err := os.Lstat(filepath.Join(dst, ".git")); err == nil {
		t.Error("the copy carried the repository's git metadata")
	}
}

// TestFixtureCopyWithTheDestinationInsideTheSource proves the copy
// stays bounded when the destination lives INSIDE the source.
//
// This is the TMPDIR-inside-the-checkout case: the destination is
// created inside the tree being copied, so a copy that follows the
// filesystem instead of a file set copies its OWN output, over and
// over, until the filesystem is full.
//
// The copy is run under a tripwire (see runBoundedCopy), so the test
// is safe against an implementation that does recurse: it stops the
// runaway instead of filling the disk.
func TestFixtureCopyWithTheDestinationInsideTheSource(t *testing.T) {
	work := t.TempDir()
	repo := fixtureRepo(t, work)
	// The destination is inside the repository, and the repository is
	// the source: the exact shape of TMPDIR inside the checkout. The
	// TMPDIR directory itself exists beforehand — that is what an
	// operator's TMPDIR inside a checkout looks like, and it is what
	// puts the destination inside the tree being walked.
	if err := os.MkdirAll(filepath.Join(repo, "tmp"), 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(repo, "tmp", "mpm-devsource-x", "checkout")

	// copySourceTree derives the source from the working directory,
	// so the working directory is the fixture's "internal/update".
	t.Chdir(filepath.Join(repo, "cmd", "mpm"))
	err, files, runaway := runBoundedCopy(t, func() error { return copySourceTree(dst) }, dst, 64, 30*time.Second)
	if runaway {
		t.Fatalf("the fixture copy did not stay bounded with the destination inside the source (it wrote more than 64 files before being stopped): %v", err)
	}
	if err != nil {
		t.Fatalf("the fixture copy failed: %v", err)
	}
	if files > 64 {
		t.Fatalf("the fixture copy wrote %d files; a bounded copy writes the source's files only", files)
	}
	for _, want := range []string{"README.md", "cmd/mpm/main.go", "agent_installation/x/a"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(want))); err != nil {
			t.Errorf("the copy does not carry %s: %v", want, err)
		}
	}
	// Nothing under the destination may have been copied into itself.
	if n := countFiles(t, filepath.Join(dst, "tmp"), 1000); n > 0 {
		t.Errorf("the copy descended into its own destination (%d file(s) below it)", n)
	}
}

// TestFixtureCopyWithANestedDestinationInsideTheSource proves the
// bound holds for a destination several levels inside the source.
func TestFixtureCopyWithANestedDestinationInsideTheSource(t *testing.T) {
	work := t.TempDir()
	repo := fixtureRepo(t, work)
	// The nested destination's parent exists beforehand, exactly as a
	// TMPDIR inside the checkout does.
	if err := os.MkdirAll(filepath.Join(repo, "a", "b", "c"), 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(repo, "a", "b", "c", "mpm-devsource-x", "checkout")

	t.Chdir(filepath.Join(repo, "cmd", "mpm"))
	err, files, runaway := runBoundedCopy(t, func() error { return copySourceTree(dst) }, dst, 64, 30*time.Second)
	if runaway {
		t.Fatalf("the fixture copy did not stay bounded with a nested destination inside the source: %v", err)
	}
	if err != nil {
		t.Fatalf("the fixture copy failed: %v", err)
	}
	if files > 64 {
		t.Fatalf("the fixture copy wrote %d files; a bounded copy writes the source's files only", files)
	}
}

// TestFixtureCopyFromALinkedWorktree proves a linked worktree — whose
// ".git" is a FILE pointing at the real repository — is copied
// without its git metadata, and that the real repository is untouched.
//
// A copied ".git" file would make the fixture's `git init`
// REINITIALIZE THE REAL REPOSITORY, and the fixture's commits would
// land in the developer's own checkout.
func TestFixtureCopyFromALinkedWorktree(t *testing.T) {
	work := t.TempDir()
	repo := fixtureRepo(t, work)
	worktree := filepath.Join(work, "linked-worktree")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "--quiet", worktree, "-b", "fixture-branch") //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this git cannot create a linked worktree: %v\n%s", err, out)
	}
	// The linked worktree's .git is a FILE, not a directory.
	gitPath := filepath.Join(worktree, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		t.Fatalf("the linked worktree's .git is a directory; this host does not produce the linked-worktree shape")
	}

	headBefore := gitOutput(t, repo, "rev-parse", "HEAD")
	commitsBefore := gitOutput(t, repo, "rev-list", "--count", "HEAD")

	dst := filepath.Join(work, "bundle", "checkout")
	if err := copySourceTreeFrom(worktree, dst); err != nil {
		t.Fatalf("the fixture copy of a linked worktree failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, ".git")); err == nil {
		t.Error("the copy carried the linked worktree's .git file")
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); err != nil {
		t.Errorf("the copy does not carry the worktree's files: %v", err)
	}

	// The real repository must be exactly as it was.
	if got := gitOutput(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("the original repository's HEAD changed: %q -> %q", headBefore, got)
	}
	if got := gitOutput(t, repo, "rev-list", "--count", "HEAD"); got != commitsBefore {
		t.Errorf("the original repository gained commits: %s -> %s", commitsBefore, got)
	}
}

// TestFixtureSetupLeavesTheOriginalRepositoryUntouched proves the
// fixture never writes into the repository it copies from.
//
// The fixture builds a DISPOSABLE repository: the developer's own
// checkout must be byte-identical afterwards, including its HEAD, its
// commit count and its working-tree state.
func TestFixtureSetupLeavesTheOriginalRepositoryUntouched(t *testing.T) {
	work := t.TempDir()
	repo := fixtureRepo(t, work)

	headBefore := gitOutput(t, repo, "rev-parse", "HEAD")
	commitsBefore := gitOutput(t, repo, "rev-list", "--count", "HEAD")
	statusBefore := gitOutput(t, repo, "status", "--porcelain")

	dst := filepath.Join(work, "bundle", "checkout")
	if err := copySourceTreeFrom(repo, dst); err != nil {
		t.Fatalf("the fixture copy failed: %v", err)
	}
	if err := initFixtureRepo(dst); err != nil {
		t.Fatalf("the fixture repository could not be initialised: %v", err)
	}
	// And the disposable repository is the fixture's OWN: its git dir
	// lives inside it.
	if got := gitOutput(t, dst, "rev-parse", "--absolute-git-dir"); !pathWithinDir(dst, got) {
		t.Errorf("the fixture repository's git dir is %s, which is outside %s", got, dst)
	}

	if got := gitOutput(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("the original repository's HEAD changed: %q -> %q", headBefore, got)
	}
	if got := gitOutput(t, repo, "rev-list", "--count", "HEAD"); got != commitsBefore {
		t.Errorf("the original repository gained commits: %s -> %s", commitsBefore, got)
	}
	if got := gitOutput(t, repo, "status", "--porcelain"); got != statusBefore {
		t.Errorf("the original repository's working tree changed:\n--- before ---\n%s\n--- after ---\n%s", statusBefore, got)
	}
}

// TestFixtureWritesNothingOutsideItsDisposableRoot proves the fixture
// touches nothing outside the root it owns.
func TestFixtureWritesNothingOutsideItsDisposableRoot(t *testing.T) {
	work := t.TempDir()
	repo := fixtureRepo(t, work)

	// A file the operator owns, beside the fixture's roots.
	outside := filepath.Join(work, "operator.txt")
	if err := os.WriteFile(outside, []byte("operator state\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}

	root, err := newFixtureRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "checkout")
	if err := copySourceTreeFrom(repo, dst); err != nil {
		t.Fatalf("the fixture copy failed: %v", err)
	}
	if err := initFixtureRepo(dst); err != nil {
		t.Fatalf("the fixture repository could not be initialised: %v", err)
	}
	if err := removeFixtureRoot(root); err != nil {
		t.Fatalf("the fixture root was not removed: %v", err)
	}

	after, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("the fixture removed a file it did not create: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("the fixture modified a file outside its root:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	// Only the operator's own file is left in the work directory.
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "operator.txt" && e.Name() != "repo" {
			t.Errorf("the fixture left %q behind", e.Name())
		}
	}
}

// TestFixtureRepoRefusesToInitialiseForeignGitMetadata proves the
// initialiser refuses a directory that already carries git metadata.
//
// A linked worktree's ".git" is a FILE pointing at another
// repository. Initialising beside it would reinitialise THAT
// repository, and the fixture's commits would land in the developer's
// checkout.
func TestFixtureRepoRefusesToInitialiseForeignGitMetadata(t *testing.T) {
	work := t.TempDir()
	repo := fixtureRepo(t, work)
	linked := filepath.Join(work, "linked-worktree")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "--quiet", linked, "-b", "fixture-branch") //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this git cannot create a linked worktree: %v\n%s", err, out)
	}
	headBefore := gitOutput(t, repo, "rev-parse", "HEAD")

	err := initFixtureRepo(linked)
	if err == nil {
		t.Fatal("the initialiser accepted a directory carrying foreign git metadata")
	}
	if !strings.Contains(err.Error(), "git metadata") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if got := gitOutput(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("the original repository's HEAD changed: %q -> %q", headBefore, got)
	}
}

// gitOutput runs a git command in dir and returns its trimmed output,
// or "" when the command fails.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("<error: %v: %s>", err, out)
	}
	return strings.TrimSpace(string(out))
}
