package dispatch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The shared SOURCE REPOSITORY for the dispatcher's tests.
//
// The production provisioner creates a real Git worktree, so the tests
// that run the dispatcher exercise the real provisioner too. One
// repository is created for the test process; each assignment gets its
// own worktree under its own test's workspace root.
var (
	testRepoOnce sync.Once
	testRepoDir  string
	testRepoBase string
	testRepoErr  error
)

// testRepository returns the process-wide source repository and its base
// revision.
func testRepository(t *testing.T) (string, string) {
	t.Helper()
	testRepoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mpm-dispatch-repo-")
		if err != nil {
			testRepoErr = fmt.Errorf("create the test repository: %w", err)
			return
		}
		testRepoDir = dir
		run := func(args ...string) error {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		}
		for _, step := range [][]string{
			{"init", "-q"},
			{"config", "user.email", "dispatch-test@example.invalid"},
			{"config", "user.name", "Dispatch Test"},
		} {
			if err := run(step...); err != nil {
				testRepoErr = err
				return
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "README.md"),
			[]byte("# the source repository\n"), 0o600); err != nil {
			testRepoErr = err
			return
		}
		if err := run("add", "README.md"); err != nil {
			testRepoErr = err
			return
		}
		if err := run("commit", "-q", "-m", "the base revision"); err != nil {
			testRepoErr = err
			return
		}
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			testRepoErr = fmt.Errorf("read the test repository's HEAD: %w", err)
			return
		}
		testRepoBase = strings.TrimSpace(string(out))
	})
	if testRepoErr != nil {
		t.Fatal(testRepoErr)
	}
	return testRepoDir, testRepoBase
}
