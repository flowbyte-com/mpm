// db_path_resolution_test.go — the D4 contract at the EXECUTABLE level.
//
// THE DEFECT THIS PINS
//
// mpm-scheduler (and mpm-critic) derived the workspace from an explicit
// --db / MPM_DB_PATH with Dir(Dir(Dir(path))). The derivation ASSUMED
// that the path had the canonical <root>/src/db/mpm.db shape and never
// checked it:
//
//	--db /tmp/other.db     →  workspace "/"  →  DB "/src/db/mpm.db"
//	MPM_DB_PATH=/tmp/x.db  →  workspace "/"  →  DB "/src/db/mpm.db"
//
// The operator asked for one database and got another. Nothing said so,
// at startup or ever afterwards, and every write landed in a database
// they did not name.
//
// THE CONTRACT
//
// A database path without the canonical shape is REFUSED, explicitly,
// before the process opens anything. The refusal names the path that
// was rejected.
//
// These tests are executable-level on purpose: the unit tests in
// internal/core/config cover the resolver, but the defect lived in the
// WIRING — a binary that never called the resolver. A resolver that is
// correct and unused is not a fix.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildScheduler builds the mpm-scheduler binary under test.
func buildScheduler(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	bin := filepath.Join(t.TempDir(), "mpm-scheduler")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build mpm-scheduler: %v", err)
	}
	return bin
}

// runBounded runs the scheduler and waits for it to exit — but not
// forever.
//
// The scheduler is a DAEMON. Without the gate it would start its loop
// and run until something killed it, so a test that waits for the
// process to exit would hang until the whole package's test timeout:
// ten minutes, and a panic that reads like an infrastructure problem
// rather than a missing refusal.
//
// So the wait is BOUNDED. A process that is still alive when the bound
// expires is killed, and the caller is told it did not exit — which is
// what "the gate did not fire" actually looks like from the outside.
func runBounded(t *testing.T, cmd *exec.Cmd, limit time.Duration) (string, error, bool) {
	t.Helper()
	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mpm-scheduler: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err, true
	case <-time.After(limit):
		_ = cmd.Process.Kill()
		<-done
		return buf.String(), nil, false
	}
}

// TestD4_ExplicitNonCanonicalDBPathIsRefused pins the headline case:
// `--db <path>` without the canonical shape must fail, loudly, and must
// not create anything.
func TestD4_ExplicitNonCanonicalDBPathIsRefused(t *testing.T) {
	bin := buildScheduler(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")

	cmd := exec.Command(bin, "--db", bad)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH=")
	out, err, exited := runBounded(t, cmd, 60*time.Second)

	if !exited {
		t.Fatalf("the scheduler did not refuse: it kept running, so the gate is missing\n%s", out)
	}
	if err == nil {
		t.Fatalf("a --db without the canonical shape must fail; it exited 0:\n%s", out)
	}
	msg := string(out)
	if !strings.Contains(msg, "database path conflict") {
		t.Errorf("the refusal must say it is a database path conflict; got:\n%s", msg)
	}
	if !strings.Contains(msg, bad) {
		t.Errorf("the refusal must name the rejected path %q; got:\n%s", bad, msg)
	}

	// Nothing may have been created: the refusal happens before the
	// filesystem is touched.
	if _, statErr := os.Stat(bad); statErr == nil {
		t.Errorf("the rejected path %q must not have been created", bad)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "src", "db", "mpm.db")); statErr == nil {
		t.Errorf("no database may have been created in the workspace")
	}
	// And above all: the pathological "/src/db/mpm.db" the old
	// derivation used to produce must NOT exist.
	if _, statErr := os.Stat("/src/db/mpm.db"); statErr == nil {
		t.Errorf("D4 REGRESSION: /src/db/mpm.db was created — the old derivation still ran")
	}
}

// TestD4_DivergentEnvOverrideIsRefused pins the environment half of the
// contract: MPM_DB_PATH that names a database other than the workspace's
// own must be a conflict, not a selection.
func TestD4_DivergentEnvOverrideIsRefused(t *testing.T) {
	bin := buildScheduler(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+bad)
	out, err, exited := runBounded(t, cmd, 60*time.Second)

	if !exited {
		t.Fatalf("the scheduler did not refuse: it kept running, so the gate is missing\n%s", out)
	}
	if err == nil {
		t.Fatalf("a divergent MPM_DB_PATH must fail; it exited 0:\n%s", out)
	}
	msg := string(out)
	if !strings.Contains(msg, "database path conflict") {
		t.Errorf("the refusal must say it is a database path conflict; got:\n%s", msg)
	}
	if !strings.Contains(msg, bad) {
		t.Errorf("the refusal must name the divergent path %q; got:\n%s", bad, msg)
	}
}

// TestD4_ConsistentEnvironmentIsAccepted is the control: the live
// installation's exact configuration (MPM_WORKSPACE and MPM_DB_PATH
// pointing at the same workspace) must NOT be refused.
//
// The scheduler does not exit on its own, so the test starts it, waits
// for the startup line — which reports the RESOLVED workspace and
// database — and then stops it.
func TestD4_ConsistentEnvironmentIsAccepted(t *testing.T) {
	bin := buildScheduler(t)

	ws := t.TempDir()
	canonical := filepath.Join(ws, "src", "db", "mpm.db")

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+canonical)

	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mpm-scheduler: %v", err)
	}
	started := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "mpm-scheduler starting") {
			started = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	msg := buf.String()
	if !started {
		t.Fatalf("the scheduler never reported that it started; got:\n%s", msg)
	}
	if strings.Contains(msg, "database path conflict") {
		t.Fatalf("a consistent MPM_DB_PATH must be accepted; got:\n%s", msg)
	}
	if !strings.Contains(msg, "workspace="+ws) {
		t.Errorf("the startup line must report the resolved workspace %q; got:\n%s", ws, msg)
	}
	if !strings.Contains(msg, "db="+canonical) {
		t.Errorf("the startup line must report the resolved database %q; got:\n%s", canonical, msg)
	}
}

// syncBuffer is a bytes.Buffer that is safe to read while a child
// process is writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
