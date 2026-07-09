// pidfile_test.go — unit tests for the mpm-mcp single-instance lock.
//
// The pidfile logic is small but carries a critical invariant: at most
// one mpm-mcp process may hold the lock at a time. These tests cover
// the four lifecycle transitions:
//
//   1. Cold start, no pidfile         → acquire succeeds (canonical).
//   2. Cold start, valid live pidfile → acquire returns ErrOrphan.
//   3. Cold start, stale pidfile      → acquire succeeds (takeover).
//   4. Cold start, corrupt pidfile    → acquire succeeds (takeover).
//   5. Release by current owner       → file is removed.
//   6. Release by non-owner           → file is untouched.

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakePidfile writes a pidfile pointing at `pid` with the given
// version string. Used to set up "another live mpm-mcp" scenarios.
func writeFakePidfile(t *testing.T, path string, pid int, version string) {
	t.Helper()
	payload := PidfilePayload{
		PID:       pid,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Version:   version,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestAcquirePidfile_ColdStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-mcp.pid")

	if err := AcquirePidfile(path); err != nil {
		t.Fatalf("cold-start acquire should succeed, got: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	got, err := readPidfile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Errorf("pidfile.pid = %d, want %d", got.PID, os.Getpid())
	}
	if got.Version != pidfileVersion {
		t.Errorf("pidfile.version = %q, want %q", got.Version, pidfileVersion)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.StartedAt); err != nil {
		t.Errorf("pidfile.started_at = %q is not RFC3339Nano: %v", got.StartedAt, err)
	}
}

func TestAcquirePidfile_OrphanRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-mcp.pid")

	// Fork a long-lived helper whose argv[0] is "mpm-mcp" so
	// isLiveMcpProcess treats it as a real instance.
	helper := startFakeMcpHelper(t)
	t.Cleanup(func() { helper.stop() })

	writeFakePidfile(t, path, helper.pid, pidfileVersion)

	err := AcquirePidfile(path)
	if err == nil {
		os.Remove(path)
		t.Fatal("acquire against live holder should fail, got nil")
	}
	if !errors.Is(err, ErrOrphan) {
		t.Errorf("acquire error = %v, want ErrOrphan", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(helper.pid)) {
		t.Errorf("error should reference holder pid %d, got: %v", helper.pid, err)
	}
}

func TestAcquirePidfile_StaleTakesOver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-mcp.pid")

	// A pid that is definitely not alive. PIDs above 2^22 are
	// not allocated on Linux by default.
	const deadPID = 0x7ffffff0
	writeFakePidfile(t, path, deadPID, pidfileVersion)

	if err := AcquirePidfile(path); err != nil {
		t.Fatalf("stale pidfile should be taken over, got: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	got, err := readPidfile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Errorf("after takeover, pidfile.pid = %d, want %d", got.PID, os.Getpid())
	}
}

func TestAcquirePidfile_CorruptTakesOver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-mcp.pid")

	if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("seed corrupt pidfile: %v", err)
	}

	if err := AcquirePidfile(path); err != nil {
		t.Fatalf("corrupt pidfile should be taken over, got: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	got, err := readPidfile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Errorf("after corrupt takeover, pidfile.pid = %d, want %d", got.PID, os.Getpid())
	}
}

func TestAcquirePidfile_VersionMismatchTakesOver(t *testing.T) {
	// A pidfile from an older build points at a dead pid. The
	// version field differs but liveness is the only decision
	// criterion — refusing to start based on version would be a
	// self-lockout waiting to happen.
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-mcp.pid")

	writeFakePidfile(t, path, 0x7ffffff1, "mpm-mcp-pidfile/0.9")

	if err := AcquirePidfile(path); err != nil {
		t.Fatalf("version-mismatched stale pidfile should be taken over, got: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
}

func TestReleasePidfile_OwnerRemoves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-mcp.pid")

	if err := AcquirePidfile(path); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := ReleasePidfile(path); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after release, file should be gone; stat err = %v", err)
	}
}

func TestReleasePidfile_NonOwnerLeavesAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-mcp.pid")

	// Simulate "another instance's pidfile" by writing a payload
	// whose PID is not ours.
	writeFakePidfile(t, path, os.Getpid()+100000, pidfileVersion)

	if err := ReleasePidfile(path); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("non-owner release should not remove the file; stat err = %v", err)
	}
	os.Remove(path)
}

func TestReleasePidfile_MissingIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.pid")

	if err := ReleasePidfile(path); err != nil {
		t.Errorf("release on missing file should be nil, got: %v", err)
	}
}

func TestPidfilePath_RootedAtMPMDir(t *testing.T) {
	got := PidfilePath()
	if !filepath.IsAbs(got) {
		t.Errorf("PidfilePath() = %q, want absolute", got)
	}
	if filepath.Base(got) != "mpm-mcp.pid" {
		t.Errorf("PidfilePath() base = %q, want mpm-mcp.pid", filepath.Base(got))
	}
}

// ── helper process ─────────────────────────────────────────────────────

type fakeMcpHelper struct {
	cmd  *exec.Cmd
	pid  int
	stop func()
}

// startFakeMcpHelper forks a long-lived process whose comm is
// "mpm-mcp" so isLiveMcpProcess treats it as a real instance.
//
// Approach: create a temp symlink named "mpm-mcp" pointing at the
// platform's `sleep` binary, then exec it. Linux sets /proc/<pid>/comm
// from the basename of the executable path, so the kernel sees
// comm="mpm-mcp" even though the real binary is sleep(1). This is
// more reliable than `exec -a NAME` (which only changes argv[0] and
// is ignored by /proc/<pid>/comm on most kernels).
func startFakeMcpHelper(t *testing.T) *fakeMcpHelper {
	t.Helper()
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	linkPath := filepath.Join(t.TempDir(), "mpm-mcp")
	if err := os.Symlink(sleepPath, linkPath); err != nil {
		t.Skipf("cannot create comm-spoof symlink: %v", err)
	}
	cmd := exec.Command(linkPath, "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot fork helper: %v", err)
	}
	return &fakeMcpHelper{
		cmd: cmd,
		pid: cmd.Process.Pid,
		stop: func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		},
	}
}
