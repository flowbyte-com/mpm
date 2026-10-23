// call_io_lifecycle_test.go — the lifecycle of the shared mpm binary
// the IO tests exec.
//
// The binary is built once per package run and shared by every test
// that needs it, so the PACKAGE owns its directory: the directory is
// recorded before anything is built into it, and removed when the
// package's tests are done with it.
//
// That directory used to survive every run of this package, leaving
// ~22 MB behind each time.
//
// The removal itself happens in the package's TestMain, after
// `m.Run()` returns. A test running INSIDE the package cannot observe
// that, so the residue this file is about is measured from outside the
// package — see the external post-process check in the PR report.
//
// A PRIVATE directory — one a single test creates for itself — is a
// different case. That directory belongs to the test, so it is removed
// by a t.Cleanup registered the moment it exists. Removing it at the
// end of the test body instead leaks it whenever the body is abandoned:
// t.Fatal() calls runtime.Goexit, which unwinds the test goroutine —
// deferred functions and registered cleanups still run, but the body
// never reaches a trailing removal.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// callIOEarlyFailureEnv makes the re-executed test binary take the
	// early-failure path instead of the parent path.
	callIOEarlyFailureEnv = "MPM_CALL_IO_EARLY_FAILURE"

	// callIOEarlyFailurePathEnv carries the path of the file the child
	// writes the directory it created to, so that the parent can check
	// that directory after the child has exited.
	callIOEarlyFailurePathEnv = "MPM_CALL_IO_EARLY_FAILURE_PATH"
)

// newPrivateCallIOBinDir creates a build directory owned by a single
// test and guarantees its removal.
//
// The removal is registered as a t.Cleanup the moment the directory
// exists, not as a statement at the end of the test body, so a test
// that ends early with t.Fatal() still removes what it created.
//
// The package pointer is restored to the value it had before, so the
// package teardown keeps owning the SHARED binary's directory.
//
// The removal is performed on the captured path and never through
// removeCallIOBinDir(): by the time the cleanup runs the package
// pointer has already been restored, and removeCallIOBinDir() would
// then remove the shared directory instead of the private one.
func newPrivateCallIOBinDir(t *testing.T) string {
	t.Helper()

	saved := callIOBinDir
	dir, err := newCallIOBinDir()
	if err != nil {
		callIOBinDir = saved
		t.Fatalf("create the private build directory: %v", err)
	}

	t.Cleanup(func() {
		// Restore first, so the package teardown keeps owning the
		// shared directory even when this cleanup runs after an early
		// failure.
		callIOBinDir = saved
		if err := removeBuildDir(dir); err != nil {
			t.Errorf("remove the private build directory %q: %v", dir, err)
		}
		// The shared directory must survive: a private test may never
		// remove the binary the rest of the package still execs.
		if saved != "" {
			if _, err := os.Stat(saved); err != nil {
				t.Errorf("the shared build directory %q was removed by a private test: %v",
					saved, err)
			}
		}
	})

	return dir
}

// TestCallIO_TheSharedBinaryLivesInsideTheRecordedDirectory proves
// every shared binary lives in a directory the package teardown knows
// about.
//
// If a future change wrote it somewhere else, the teardown would leave
// it behind and the residue would return.
func TestCallIO_TheSharedBinaryLivesInsideTheRecordedDirectory(t *testing.T) {
	if mpmBin == "" {
		t.Skip("the shared mpm binary was not built (see TestMain)")
	}
	if callIOBinDir == "" {
		t.Fatal("the shared build directory was not recorded, so the package teardown cannot remove it")
	}
	if _, err := os.Stat(callIOBinDir); err != nil {
		t.Fatalf("the recorded build directory does not exist: %v", err)
	}
	if got := filepath.Dir(mpmBin); got != callIOBinDir {
		t.Errorf("the shared binary lives in %q, not in the recorded build directory %q",
			got, callIOBinDir)
	}
	if base := filepath.Base(callIOBinDir); !strings.HasPrefix(base, "mpm-call-io-bin-") {
		t.Errorf("the recorded build directory %q does not carry the mpm-call-io-bin- prefix", base)
	}

	// The binary must still be there while the tests are running: it
	// is shared by the whole package, so no individual test may
	// remove it. This assertion is what would fail if the cleanup
	// were ever moved into a per-test cleanup.
	if _, err := os.Stat(mpmBin); err != nil {
		t.Errorf("the shared binary %q is not present during the test run: %v", mpmBin, err)
	}
}

// TestCallIO_TheSharedBinaryStillProducesMachineCleanOutput proves the
// stdout/stderr contract of the shared binary is unchanged.
//
// The IO regression tests in call_io_test.go cover the mpm_memory call
// path; this one covers the binary itself — it must still run from its
// recorded directory, report on stdout, and keep stderr free of
// operational noise. A build that stopped producing a usable binary
// would otherwise show up only as a silent skip.
func TestCallIO_TheSharedBinaryStillProducesMachineCleanOutput(t *testing.T) {
	if mpmBin == "" {
		t.Skip("the shared mpm binary was not built (see TestMain)")
	}

	cmd := exec.Command(mpmBin, "--version")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"MPM_SCHEDULER_DISABLED=1",
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("the shared binary did not run: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if stdout.Len() == 0 {
		t.Error("the shared binary reported nothing on stdout")
	}
	if stderr.Len() != 0 {
		t.Errorf("the shared binary wrote to stderr:\n%s", stderr.String())
	}
}

// TestCallIO_TheDirectoryIsRecordedBeforeTheBuild proves the recording
// happens BEFORE anything is built into the directory.
//
// A directory recorded only after a successful build leaks precisely in
// the case that needs cleaning up most: a build that failed halfway.
func TestCallIO_TheDirectoryIsRecordedBeforeTheBuild(t *testing.T) {
	// The private directory is removed by the helper's cleanup, and
	// the package pointer is restored to the shared directory when
	// this test ends — whatever ends it.
	dir := newPrivateCallIOBinDir(t)
	if callIOBinDir != dir {
		t.Fatalf("the build directory was not recorded: recorded %q, created %q",
			callIOBinDir, dir)
	}

	// A build that fails must not discard the recording.
	if _, err := buildCallIOBinary("./cmd/this-package-does-not-exist",
		filepath.Join(dir, "mpm")); err == nil {
		t.Error("building a package that does not exist must fail")
	}
	if callIOBinDir != dir {
		t.Errorf("a failed build discarded the recorded directory: %q != %q",
			callIOBinDir, dir)
	}

	// And the package teardown removes it.
	if err := removeCallIOBinDir(); err != nil {
		t.Fatalf("remove the recorded build directory: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the recorded build directory %q still exists after the teardown", dir)
	}
}

// TestCallIO_APartialBuildStillLeavesARemovableRecordedDirectory
// proves a build that failed HALFWAY leaves a directory the teardown
// can still remove.
//
// A partial build is the case that matters: the directory exists, holds
// a half-written artefact, and the run is about to fail. If the
// recording were conditional on the build's success, exactly this
// directory would be the one left behind.
func TestCallIO_APartialBuildStillLeavesARemovableRecordedDirectory(t *testing.T) {
	dir := newPrivateCallIOBinDir(t)

	// The artefact a half-finished build would leave behind.
	if err := os.WriteFile(filepath.Join(dir, "mpm"), []byte("partial"), 0o755); err != nil {
		t.Fatalf("write the partial artefact: %v", err)
	}
	if callIOBinDir != dir {
		t.Fatalf("the build directory was not recorded: recorded %q, created %q",
			callIOBinDir, dir)
	}

	if err := removeCallIOBinDir(); err != nil {
		t.Fatalf("remove the recorded build directory: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the recorded build directory %q still exists after the teardown", dir)
	}
}

// TestCallIO_AnEarlyFailureStillRemovesThePrivateDirectory proves the
// private directory is removed even when the test that created it ends
// early with t.Fatal().
//
// A test that removes its directory at the end of its body leaks that
// directory whenever the body is abandoned — and t.Fatal() abandons it
// by design. The pointer restoration still happens, so the package
// teardown no longer knows about the directory: it is left behind with
// nobody left to remove it.
//
// The failure has to happen in a separate process: a failing subtest
// fails its parent, so the parent could not assert anything. The child
// is this same test binary, re-executed with the early-failure path
// enabled. It creates the directory through the same helper as every
// other private directory and then fails on purpose.
func TestCallIO_AnEarlyFailureStillRemovesThePrivateDirectory(t *testing.T) {
	if os.Getenv(callIOEarlyFailureEnv) == "1" {
		// The child process: create the directory, record where it is,
		// then fail before reaching any removal of its own.
		dir := newPrivateCallIOBinDir(t)
		if pathFile := os.Getenv(callIOEarlyFailurePathEnv); pathFile != "" {
			if err := os.WriteFile(pathFile, []byte(dir), 0o600); err != nil {
				t.Fatalf("record the created directory: %v", err)
			}
		}
		t.Fatalf("intentional early failure: the private directory %q must still be removed", dir)
	}

	// The parent process: run the child and check what it left behind.
	pathFile := filepath.Join(t.TempDir(), "created-dir.txt")

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestCallIO_AnEarlyFailureStillRemovesThePrivateDirectory$",
		"-test.timeout=20m")
	// The child is this same test binary — an MPM-capable binary — so it
	// must not inherit an environment that could resolve the operator's
	// MPM state. MPM_WORKSPACE is pinned to a temporary directory, which
	// closes both resolution paths: config.GetMPMDir's $HOME/.mpm
	// fallback and mpmcli.ResolveWorkspace's "." fallback. os/exec
	// dedups Env with the last entry winning, so this overrides an
	// ambient MPM_WORKSPACE rather than racing it.
	//
	// The rest of the ambient environment is inherited deliberately: the
	// child's own TestMain runs `go build`, and a hermetic environment
	// without GOPATH/GOMODCACHE/GOCACHE would relocate the Go module
	// cache into a temporary HOME, turning a one-second build into a
	// full module re-download.
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+t.TempDir(),
		callIOEarlyFailureEnv+"=1",
		callIOEarlyFailurePathEnv+"="+pathFile,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatalf("the child test was supposed to fail early, but it succeeded:\nstdout=%s\nstderr=%s",
			stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "intentional early failure") &&
		!strings.Contains(stderr.String(), "intentional early failure") {
		t.Fatalf("the child did not take the early-failure path:\nstdout=%s\nstderr=%s",
			stdout.String(), stderr.String())
	}

	recorded, readErr := os.ReadFile(pathFile)
	if readErr != nil {
		t.Fatalf("the child did not record the directory it created: %v\nstdout=%s\nstderr=%s",
			readErr, stdout.String(), stderr.String())
	}
	dir := strings.TrimSpace(string(recorded))
	if dir == "" {
		t.Fatal("the child recorded an empty directory path")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("the private directory %q survived the child's early failure (stat error: %v)",
			dir, statErr)
	}
}

// TestCallIO_ATeardownFailureCannotLeaveThePackageGreen proves the
// package teardown turns a cleanup failure into a FAILING package
// result.
//
// A swallowed cleanup failure is how a temporary filesystem fills up
// slowly: the directory holds a ~22 MB binary and nothing says so. The
// original exit code is preserved when the tests already failed.
func TestCallIO_ATeardownFailureCannotLeaveThePackageGreen(t *testing.T) {
	if got := teardownFailureCode(0); got == 0 {
		t.Error("a cleanup failure must turn a green package result into a failing one")
	}
	if got := teardownFailureCode(3); got != 3 {
		t.Errorf("an already-failing package must keep its exit code, got %d", got)
	}

	// A removal that cannot succeed must REPORT that.
	//
	// A read-only directory is the portable way to make removal fail
	// for an unprivileged user. The root user ignores the permission
	// bits, so the assertion is skipped there rather than made
	// vacuous.
	if os.Geteuid() == 0 {
		t.Log("running as root; the removal-failure path cannot be provoked this way")
		return
	}
	// The prefix is deliberately NOT "mpm-call-io-bin-": this
	// directory is deliberately hard to remove, and a residue check
	// must not mistake it for the leak it is guarding against.
	locked, err := os.MkdirTemp("", "mpm-call-io-removal-locked-")
	if err != nil {
		t.Fatalf("create the locked directory: %v", err)
	}
	// The removal is registered the moment the directory exists, so it
	// happens even if this test ends early, and it reports its errors.
	t.Cleanup(func() {
		// The directory is made read-only below; it has to be writable
		// again before it can be removed.
		if err := os.Chmod(locked, 0o700); err != nil {
			t.Errorf("make the locked directory writable again: %v", err)
		}
		if err := os.RemoveAll(locked); err != nil {
			t.Errorf("remove the locked directory %q: %v", locked, err)
		}
	})
	// The directory holds a file: removing an EMPTY read-only directory
	// succeeds, because the parent's write permission is what governs
	// the removal of the directory entry itself.
	if err := os.WriteFile(filepath.Join(locked, "mpm"), []byte("binary"), 0o755); err != nil {
		t.Fatalf("write the marker: %v", err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("make the directory read-only: %v", err)
	}

	saved := callIOBinDir
	t.Cleanup(func() { callIOBinDir = saved })
	callIOBinDir = locked
	if err := removeCallIOBinDir(); err == nil {
		t.Error("removing a read-only directory must report a failure, not swallow it")
	}
}
