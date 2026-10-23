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
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	// The shared pointer is saved and restored: the real directory
	// holds the binary the other tests in this package still need.
	saved := callIOBinDir
	defer func() { callIOBinDir = saved }()

	dir, err := newCallIOBinDir()
	if err != nil {
		t.Fatalf("create the shared build directory: %v", err)
	}
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
	saved := callIOBinDir
	defer func() { callIOBinDir = saved }()

	dir, err := newCallIOBinDir()
	if err != nil {
		t.Fatalf("create the shared build directory: %v", err)
	}
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
	defer func() {
		os.Chmod(locked, 0o700)
		os.RemoveAll(locked)
	}()
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
	defer func() { callIOBinDir = saved }()
	callIOBinDir = locked
	if err := removeCallIOBinDir(); err == nil {
		t.Error("removing a read-only directory must report a failure, not swallow it")
	}
}
