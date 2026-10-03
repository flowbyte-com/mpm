package scheduler

// state_isolation_test.go — hermeticity of the scheduler heartbeat file.
//
// # The defect
//
// run/scheduler.state is the cross-process health bridge between the
// daemon and the CLI's emitSchedulerHealthWarning. persistState resolved
// its target through StateFilePath() — i.e. from the AMBIENT environment —
// on every tick, rather than from the Scheduler it was writing for.
//
// A test scheduler is not special-cased anywhere: newTestScheduler
// builds a &Scheduler{...} literal, so running the tick loop in a test
// overwrote the operator's live heartbeat with a fresh one carrying the
// test's own pid and tick count. The CLI reads that file to decide
// whether the real daemon is alive; a test run made a healthy daemon
// look as though it had been replaced seconds earlier, and — worse — a
// test that ran after the real daemon exited would keep the file
// advancing, masking a genuinely dead scheduler.
//
// The <target>.tmp sibling is part of the same artifact: it is created
// and renamed away on every successful tick.
//
// # The invariant these tests pin
//
// A Scheduler writes its heartbeat to the path it captured at
// construction. New() captures StateFilePath(), so a daemon under the
// systemd unit writes exactly the path it always did. A struct-literal
// Scheduler with no captured path keeps the lazy fallback.

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// sentinelHomeWithWorkspace installs a throwaway HOME containing a
// ~/.mpm/run tree and, separately, pins MPM_WORKSPACE to a second
// throwaway root. Two distinct roots make an escape observable: a leak
// that ignores the pin lands in the sentinel home, a leak that merely
// hops to the pinned root is caught by the pinned-root assertions.
func sentinelHomeWithWorkspace(t *testing.T) (ambient, pinned string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".mpm", "run"), 0700); err != nil {
		t.Fatalf("mkdir sentinel install: %v", err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("MPM_WORKSPACE")
	ambient = filepath.Join(home, ".mpm")

	pinned = t.TempDir()
	t.Setenv("MPM_WORKSPACE", pinned)
	return ambient, pinned
}

func assertNoHeartbeat(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"scheduler.state", "scheduler.state.tmp"} {
		p := filepath.Join(root, "run", name)
		if st, err := os.Stat(p); err == nil {
			t.Errorf("ESCAPED WRITE: %s was created at %s (%d bytes)", name, p, st.Size())
		} else if !os.IsNotExist(err) {
			t.Errorf("unexpected error stat %s: %v", p, err)
		}
	}
}

// openSchedulerDB returns a closed-over db the tests below never use for
// anything but construction; persistState does no I/O against it.
func openSchedulerDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "unused.db"))
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestSchedulerState_TestSchedulerWritesOnlyItsOwnPath is the core
// regression: the shared test constructor's heartbeat must land inside
// its own t.TempDir and nowhere else.
func TestSchedulerState_TestSchedulerWritesOnlyItsOwnPath(t *testing.T) {
	ambient, pinned := sentinelHomeWithWorkspace(t)

	s := newTestScheduler(t)
	s.persistState()

	// The escape assertions come first so a failure names the file that
	// actually leaked, rather than the one that went missing.
	assertNoHeartbeat(t, ambient)
	assertNoHeartbeat(t, pinned)
	if _, err := os.Stat(s.statePath); err != nil {
		t.Fatalf("test scheduler did not write its heartbeat at %s: %v", s.statePath, err)
	}

	// The .tmp sibling is renamed away on success — it must not survive.
	if _, err := os.Stat(s.statePath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("leftover %s.tmp after a successful write (err=%v)", s.statePath, err)
	}
}

// TestSchedulerState_DispatchSchedulerWritesOnlyItsOwnPath covers the
// second struct-literal constructor in this package.
func TestSchedulerState_DispatchSchedulerWritesOnlyItsOwnPath(t *testing.T) {
	ambient, pinned := sentinelHomeWithWorkspace(t)

	s := newDispatchTestScheduler(t)
	s.persistState()

	// The escape assertions come first so a failure names the file that
	// actually leaked, rather than the one that went missing.
	assertNoHeartbeat(t, ambient)
	assertNoHeartbeat(t, pinned)
	if _, err := os.Stat(s.statePath); err != nil {
		t.Fatalf("dispatch scheduler did not write its heartbeat at %s: %v", s.statePath, err)
	}
}

// TestSchedulerState_PersistedContentUnchanged pins the file's meaning:
// the wire format, the pid, and the "ok" status are untouched by the
// ownership change.
func TestSchedulerState_PersistedContentUnchanged(t *testing.T) {
	_, _ = sentinelHomeWithWorkspace(t)

	s := newTestScheduler(t)
	s.tickCount = 7
	s.processStartedUnix = time.Now().Unix()
	s.persistState()

	raw, err := os.ReadFile(s.statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var got schedulerState
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if got.TickCount != 7 {
		t.Errorf("tick_count = %d, want 7", got.TickCount)
	}
	if got.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", got.PID, os.Getpid())
	}
	if got.LastStatus != "ok" {
		t.Errorf("last_status = %q, want %q", got.LastStatus, "ok")
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want empty", got.LastError)
	}
	if got.LastTickUnix == 0 {
		t.Error("last_tick_unix was not stamped")
	}
}

// TestSchedulerState_NewCapturesAmbientPath is the production-parity
// half. New() must capture the canonical path so a daemon is unaffected
// by the change.
func TestSchedulerState_NewCapturesAmbientPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MPM_WORKSPACE", filepath.Join(home, ".mpm"))
	if err := os.MkdirAll(filepath.Join(home, ".mpm", "run"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	want := filepath.Join(home, ".mpm", "run", "scheduler.state")
	if got := StateFilePath(); got != want {
		t.Fatalf("StateFilePath() = %q, want %q", got, want)
	}

	s, err := New(openSchedulerDB(t), slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.statePath != want {
		t.Errorf("New() captured statePath = %q, want %q", s.statePath, want)
	}

	s.persistState()
	if _, err := os.Stat(want); err != nil {
		t.Errorf("New()-built scheduler did not write the canonical heartbeat: %v", err)
	}
}

// TestSchedulerState_LiteralSchedulerKeepsLazyFallback pins the
// backward-compatibility branch: a Scheduler with no captured path still
// resolves StateFilePath() at write time, so no existing struct-literal
// construction site silently stops persisting.
func TestSchedulerState_LiteralSchedulerKeepsLazyFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MPM_WORKSPACE", filepath.Join(home, ".mpm"))

	s := &Scheduler{log: slog.Default()}
	if got := s.stateTarget(); got != StateFilePath() {
		t.Errorf("stateTarget() with no captured path = %q, want %q", got, StateFilePath())
	}
}
