package scheduler

// Scheduler subprocess binary resolution.
//
// The scheduler used to exec a bare "mpm" and let PATH decide which binary
// ran. These tests pin the replacement contract: the installed binary, named
// explicitly, is the one that executes — and a configured-but-broken MPM_BIN
// fails loudly instead of quietly falling back to whatever PATH offers.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFakeMPM builds a stand-in `mpm` that records how it was invoked. It
// writes its own argv and the MPM_BIN it saw to a marker file, so a test can
// assert which binary actually ran rather than inferring it from a side
// effect.
func writeFakeMPM(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$0\" > \"$MARKER\"\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake mpm %s: %v", path, err)
	}
	return path
}

// hostilePathDir returns a directory containing an `mpm` that would win a
// PATH lookup. Tests put it first on PATH to prove it is not selected.
func hostilePathDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFakeMPM(t, dir, "mpm")
	return dir
}

func TestResolveMPMBin_UsesConfiguredPath(t *testing.T) {
	bin := writeFakeMPM(t, t.TempDir(), "mpm")
	t.Setenv(MPMBinEnv, bin)

	got, err := resolveMPMBin()
	if err != nil {
		t.Fatalf("resolveMPMBin: %v", err)
	}
	if got != bin {
		t.Fatalf("resolveMPMBin() = %q, want the configured %q", got, bin)
	}
}

// TestResolveMPMBin_IgnoresEarlierPathEntry is the PATH-shadow case: a
// perfectly executable `mpm` sits earlier on PATH. With MPM_BIN set, it must
// not be selected.
func TestResolveMPMBin_IgnoresEarlierPathEntry(t *testing.T) {
	bin := writeFakeMPM(t, t.TempDir(), "mpm")
	t.Setenv(MPMBinEnv, bin)
	t.Setenv("PATH", hostilePathDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := resolveMPMBin()
	if err != nil {
		t.Fatalf("resolveMPMBin: %v", err)
	}
	if got != bin {
		t.Fatalf("resolveMPMBin() = %q; a PATH `mpm` was selected over the configured %q", got, bin)
	}
	if got == "mpm" {
		t.Fatal("resolveMPMBin() returned a bare name; PATH would decide the binary")
	}
}

// TestResolveMPMBin_ConfiguredButMissingFails is the "do not silently
// degrade" case. Falling back to PATH here would let a missing installed
// binary be papered over by an unrelated one.
func TestResolveMPMBin_ConfiguredButMissingFails(t *testing.T) {
	t.Setenv(MPMBinEnv, filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("PATH", hostilePathDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := resolveMPMBin()
	if err == nil {
		t.Fatalf("resolveMPMBin() = %q, want an error; a missing MPM_BIN must not fall back to PATH", got)
	}
	if !strings.Contains(err.Error(), MPMBinEnv) {
		t.Errorf("error does not name %s, so an operator cannot tell what is misconfigured: %v", MPMBinEnv, err)
	}
}

func TestResolveMPMBin_ConfiguredButNotExecutableFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mpm")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(MPMBinEnv, bin)

	if _, err := resolveMPMBin(); err == nil {
		t.Fatal("resolveMPMBin() accepted a non-executable MPM_BIN")
	}
}

func TestResolveMPMBin_ConfiguredDirectoryFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MPMBinEnv, dir)

	if _, err := resolveMPMBin(); err == nil {
		t.Fatal("resolveMPMBin() accepted a directory as MPM_BIN")
	}
}

func TestResolveMPMBin_ConfiguredBareNameIsRejected(t *testing.T) {
	// A bare name would reintroduce the PATH lookup this exists to remove.
	t.Setenv(MPMBinEnv, "mpm")

	got, err := resolveMPMBin()
	if err == nil {
		t.Fatalf("resolveMPMBin() = %q, want an error for a non-path MPM_BIN", got)
	}
}

// TestResolveMPMBin_UnsetFallsBackToBareName documents the ad-hoc/test
// contract explicitly rather than leaving it as an accident. The installed
// scheduler never reaches this branch: the unit template sets MPM_BIN.
func TestResolveMPMBin_UnsetFallsBackToBareName(t *testing.T) {
	t.Setenv(MPMBinEnv, "")

	got, err := resolveMPMBin()
	if err != nil {
		t.Fatalf("resolveMPMBin: %v", err)
	}
	if got != "mpm" {
		t.Fatalf("resolveMPMBin() = %q, want %q for an ad-hoc launch", got, "mpm")
	}
}

// ── Handler-level: prove the resolved binary is the one executed ──────────

// wakeHandler is the shape every scheduler wake executor shares, so one
// table can drive whichever handler is under test.
type wakeHandler func(context.Context, Wake) error

// runHandlerWithFakeBin points MPM_BIN at a fake binary, forces PATH to a
// hostile `mpm`, runs the handler, and returns the marker the fake wrote.
func runHandlerWithFakeBin(t *testing.T, h wakeHandler) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	bin := writeFakeMPM(t, dir, "configured-mpm")
	t.Setenv(MPMBinEnv, bin)
	t.Setenv("MARKER", marker)
	t.Setenv("PATH", hostilePathDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := h(context.Background(), Wake{}); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("fake binary never ran: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestGCHandler_InvokesConfiguredBinary(t *testing.T) {
	got := runHandlerWithFakeBin(t, GCHandler)
	if !strings.Contains(got, "configured-mpm") {
		t.Fatalf("GCHandler ran %q, want the MPM_BIN-configured binary", got)
	}
}

func TestBroadcastHandler_InvokesConfiguredBinary(t *testing.T) {
	got := runHandlerWithFakeBin(t, BroadcastHandler)
	if !strings.Contains(got, "configured-mpm") {
		t.Fatalf("BroadcastHandler ran %q, want the MPM_BIN-configured binary", got)
	}
}

// TestHandlers_FailWhenConfiguredBinaryMissing proves the failure surfaces
// as an error from the handler, before any exec is attempted.
func TestHandlers_FailWhenConfiguredBinaryMissing(t *testing.T) {
	t.Setenv(MPMBinEnv, filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PATH", hostilePathDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

	for name, h := range map[string]wakeHandler{
		"GCHandler":        GCHandler,
		"BroadcastHandler": BroadcastHandler,
	} {
		t.Run(name, func(t *testing.T) {
			err := h(context.Background(), Wake{})
			if err == nil {
				t.Fatalf("%s succeeded with a missing MPM_BIN; it must not fall back to PATH", name)
			}
			if !strings.Contains(err.Error(), MPMBinEnv) {
				t.Errorf("%s error does not name %s: %v", name, MPMBinEnv, err)
			}
		})
	}
}

// TestGCHandler_CancellationPropagates pins that switching to an explicit
// binary did not lose the F-3 property: the subprocess still dies with the
// context.
func TestGCHandler_CancellationPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	// A binary that signals it started, then blocks.
	//
	// `exec` matters: without it the shell forks a `sleep` grandchild, and
	// the grandchild inherits the stdout pipe that CombinedOutput reads. The
	// kill reaches the shell, but the read stays blocked until the sleep
	// exits — so the test would measure the fake's plumbing rather than
	// cancellation. exec makes the blocking process the one exec'd, which
	// is the shape the real (Go) binary has.
	script := "#!/bin/sh\ntouch \"" + marker + "\"\nexec sleep 30\n"
	bin := filepath.Join(dir, "slow-mpm")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(MPMBinEnv, bin)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- GCHandler(ctx, Wake{}) }()

	select {
	case err := <-done:
		// The subprocess is killed by the context; GCHandler surfaces the
		// non-zero exit. Either outcome is fine — what matters is that it
		// returned promptly rather than waiting out the sleep.
		if err != nil && !strings.Contains(err.Error(), "signal") {
			t.Logf("GCHandler returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("GCHandler did not return after cancellation; the subprocess outlived its context")
	}
}

// TestDefaultDBPath_IsWorkspaceRelativeNotCwd pins that the scheduler's DB
// default follows the workspace and never the working directory. The old
// implementation probed cwd for src/db/mpm.db and used it if present, which
// is the ghost-DB shape.
func TestDefaultDBPath_IsWorkspaceRelativeNotCwd(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	// A cwd that contains a perfectly plausible src/db/mpm.db. The default
	// must still point into the workspace.
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "src", "db"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "src", "db", "mpm.db"), []byte("decoy"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(wd) }()

	got := defaultDBPath()
	want := filepath.Join(ws, "src", "db", "mpm.db")
	if got != want {
		t.Fatalf("defaultDBPath() = %q, want %q (it followed the cwd)", got, want)
	}
}

func TestDefaultDBPath_ExplicitEnvWins(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv("MPM_DB_PATH", "/tmp/explicit-mpm.db")

	if got := defaultDBPath(); got != "/tmp/explicit-mpm.db" {
		t.Fatalf("defaultDBPath() = %q, want the explicit MPM_DB_PATH", got)
	}
}
