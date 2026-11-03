package update

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// The fixture's cleanup failures
// ---------------------------------------------------------------------
//
// A cleanup that cannot release its scratch is a DEFECT, and a defect
// that reports nothing is a defect that is found the next time the
// host runs out of space.
//
// The two tests below pin the requirement from both ends: the
// diagnostic names the root that survived, and the RUN fails when its
// own resources could not be cleaned up.

// cleanupProbeEnv makes the test below run in its "child" mode: the
// mode in which the process deliberately leaves a root it cannot clean
// up.
const cleanupProbeEnv = "MPM_UPDATE_FIXTURE_CLEANUP_PROBE"

// cleanupProbePathEnv names the file the child writes the path of the
// root it could not clean up to.
const cleanupProbePathEnv = "MPM_UPDATE_FIXTURE_CLEANUP_PROBE_PATH"

// TestCleanupFailureIsReported proves a failed cleanup is reported.
//
// The failure is made by registering a root this process does NOT own:
// the cleanup refuses to remove it, because the ownership proof is
// missing. That refusal is a cleanup failure, and it must be visible:
// reported as a failure, diagnosed with the root's path, and fatal to
// the run.
func TestCleanupFailureIsReported(t *testing.T) {
	work := t.TempDir()
	foreign := filepath.Join(work, fixtureRootPrefix+"not-ours")
	if err := os.MkdirAll(foreign, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(foreign, "operator-file.txt")
	if err := os.WriteFile(marker, []byte("mine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// The shared fixture's root must not be removed by a test of the
	// cleanup path, so the registry is taken over for the duration of
	// the test and restored afterwards.
	fixtureRoots.mu.Lock()
	saved := fixtureRoots.paths
	fixtureRoots.paths = nil
	fixtureRoots.mu.Unlock()
	defer func() {
		fixtureRoots.mu.Lock()
		fixtureRoots.paths = saved
		fixtureRoots.mu.Unlock()
	}()
	registerFixtureRoot(foreign)

	var diag bytes.Buffer
	restore := captureFixtureCleanupOutput(&diag)
	failures := cleanupFixtureRoots()
	restore()

	if len(failures) == 0 {
		t.Fatal("a failed cleanup was not reported")
	}
	if !strings.Contains(diag.String(), foreign) {
		t.Errorf("the diagnostic does not name the fixture root %s: %q", foreign, diag.String())
	}
	if got := cleanupExitCode(0, failures); got == 0 {
		t.Error("a run whose cleanup failed was reported as a success")
	}
	if got := cleanupExitCode(1, failures); got != 1 {
		t.Errorf("cleanupExitCode(1, failures) = %d, want 1", got)
	}
	if got := cleanupExitCode(0, nil); got != 0 {
		t.Errorf("cleanupExitCode(0, nil) = %d, want 0", got)
	}
	// No destructive fallback: the path the cleanup could not prove it
	// owns is still there.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the failed cleanup deleted a path it did not own: %v", err)
	}
}

// TestCleanupFailureFailsTheTestRun proves the failure is fatal to
// the RUN, not only to a helper.
//
// The test re-executes this test binary as a child. The child
// deliberately leaves a root it cannot clean up; the parent's
// assertion is on the CHILD's exit code and output, which is what a
// CI run sees.
func TestCleanupFailureFailsTheTestRun(t *testing.T) {
	if os.Getenv(cleanupProbeEnv) == "1" {
		runCleanupFailureProbe(t)
		return
	}

	probeDir := t.TempDir()
	pathFile := filepath.Join(probeDir, "root-path")

	cmd := exec.Command(os.Args[0], "-test.run=^TestCleanupFailureFailsTheTestRun$", "-test.v") //nolint:gosec
	cmd.Env = append(os.Environ(), cleanupProbeEnv+"=1", cleanupProbePathEnv+"="+pathFile)
	out, err := cmd.CombinedOutput()

	// The child's scratch is removed whether the assertions below pass
	// or FAIL: a regression test that leaves scratch behind when it
	// fails is the same defect this suite is about.
	rootBytes, rerr := os.ReadFile(pathFile)
	root := ""
	if rerr == nil {
		root = strings.TrimSpace(string(rootBytes))
	}
	if root != "" {
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(root)) })
	}

	if err == nil {
		t.Fatalf("the run reported success although its cleanup failed:\n%s", out)
	}
	if !strings.Contains(string(out), "could not be cleaned up") {
		t.Errorf("the run failed without saying that its cleanup failed:\n%s", out)
	}
	if rerr != nil {
		t.Fatalf("the child did not report the root it could not clean up: %v\n%s", rerr, out)
	}
	if root == "" {
		t.Fatalf("the child reported an empty root path:\n%s", out)
	}
	if !strings.Contains(string(out), root) {
		t.Errorf("the diagnostic does not name the fixture root %s:\n%s", root, out)
	}
	// The child could not remove the root, so the root is still there:
	// the cleanup did not fall back to deleting what it could not
	// prove it owns.
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the failed cleanup deleted a path it did not own: %v", err)
	}
}

// runCleanupFailureProbe is the child half of
// TestCleanupFailureFailsTheTestRun.
//
// It registers a root the cleanup cannot remove — one that carries no
// ownership sentinel — and returns. The cleanup that runs when the
// test binary exits is the thing under test.
func runCleanupFailureProbe(t *testing.T) {
	work, err := os.MkdirTemp("", "mpm-update-cleanup-probe-")
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(work, fixtureRootPrefix+"not-ours")
	if err := os.MkdirAll(foreign, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "operator-file.txt"), []byte("mine\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if pathFile := os.Getenv(cleanupProbePathEnv); pathFile != "" {
		if err := os.WriteFile(pathFile, []byte(foreign), 0644); err != nil {
			t.Fatal(err)
		}
	}
	registerFixtureRoot(foreign)
}
