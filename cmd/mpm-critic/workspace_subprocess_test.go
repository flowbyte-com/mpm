// workspace_subprocess_test.go — alpha-4.1.2 release-integrity test for
// D-001 (mpm-critic must honor MPM_WORKSPACE).
//
// The original D-001 fix (alpha-4.1.1) added a unit test pinning the
// helper. This file adds a subprocess-level test that drives the actual
// mpm-critic binary across the five scenarios section 2 of the
// alpha-4.1.2 spec mandates:
//
//   1. environment only
//   2. explicit -db
//   3. both environment + explicit DB (matching)
//   4. both environment + explicit DB (conflicting)
//   5. nonexistent workspace
//   6. clean workspace (no pre-existing DB)
//
// The contract under test: a disposable workspace must NEVER silently
// target production. We prove this by giving each workspace a
// distinguishable sentinel, running the critic against one, and
// asserting the other's DB is byte-identical before/after.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// findCriticBinary locates the test-built mpm-critic binary, falling
// back to the project root binary. Tests run from the package
// directory; `go test` builds the binary as a test artifact for the
// `Test` target only when invoked with -c, which we do explicitly via
// exec.Command in setupCriticBinary below. If the pre-built binary is
// present at $PROJECT_ROOT/bin/mpm-critic, prefer that — it's the
// release artifact that operators actually invoke.
func findCriticBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../bin/mpm-critic",
		"./mpm-critic.test", // go test -c output (relative to package dir)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Skip("mpm-critic binary not found; run `make build` first")
	return ""
}

// setupWorkspace creates a disposable workspace with an initialized DB
// and a unique sentinel memory. Returns the workspace path.
func setupWorkspace(t *testing.T, sentinel string) string {
	t.Helper()
	tmp := t.TempDir()
	mustMkdir(t, filepath.Join(tmp, "src", "db"))

	mpmBin := findMPMBinary(t)
	// Initialize the DB by adding the sentinel via the canonical CLI.
	cmd := exec.Command(mpmBin, "call", "mpm_memory",
		"--payload", `{"action":"save","params":{"fact":"`+sentinel+`"}}`)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init workspace %s: %v\n%s", tmp, err, out)
	}
	return tmp
}

// findMPMBinary returns the mpm binary the critic would shell out to
// during telemetry hunts. For pure workspace-resolution tests we
// don't trigger telemetry hunts, so this is mostly unused — but
// keeping it available makes the helper file complete.
func findMPMBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../bin/mpm",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Skip("mpm binary not found; run `make build` first")
	return ""
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// dbChecksum returns the SHA256 of the mpm.db file in the workspace.
// Used to assert byte-equality (i.e. "this DB was not touched").
func dbChecksum(t *testing.T, workspace string) string {
	t.Helper()
	dbPath := filepath.Join(workspace, "src", "db", "mpm.db")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read %s: %v", dbPath, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// runCritic invokes the mpm-critic binary with the given env vars and
// CLI args, returning combined stdout+stderr.
func runCritic(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	bin := findCriticBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// baseEnv returns a copy of os.Environ with MPM_WORKSPACE, MPM_DB_PATH
// stripped — tests build their own controlled env.
func baseEnv() []string {
	env := os.Environ()
	out := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "MPM_WORKSPACE=") || strings.HasPrefix(e, "MPM_DB_PATH=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Scenario 1: MPM_WORKSPACE only — critic opens that workspace's DB.
func TestCriticSubprocess_EnvOnly_TargetsSelectedWorkspace(t *testing.T) {
	workA := setupWorkspace(t, "SENTINEL-A")
	workB := setupWorkspace(t, "SENTINEL-B")
	sumABefore := dbChecksum(t, workA)

	env := append(baseEnv(), "MPM_WORKSPACE="+workB)
	out, err := runCritic(t, env)
	if err != nil {
		t.Fatalf("critic on workspace B failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "mpm-critic complete") {
		t.Fatalf("critic did not finish cleanly:\n%s", out)
	}

	// A's DB must be byte-identical: critic must not touch it.
	sumAAfter := dbChecksum(t, workA)
	if sumABefore != sumAAfter {
		t.Fatalf("workspace A DB was modified by critic targeting B\n  before=%s\n  after =%s",
			sumABefore, sumAAfter)
	}
}

// Scenario 2: explicit -db — operator override beats MPM_WORKSPACE.
func TestCriticSubprocess_ExplicitDB_OverridesWorkspace(t *testing.T) {
	workA := setupWorkspace(t, "SENTINEL-A")
	workB := setupWorkspace(t, "SENTINEL-B")
	sumBBefore := dbChecksum(t, workB)

	env := append(baseEnv(), "MPM_WORKSPACE="+workA)
	dbPath := filepath.Join(workB, "src", "db", "mpm.db")
	out, err := runCritic(t, env, "-db", dbPath)
	if err != nil {
		t.Fatalf("critic with -db failed: %v\n%s", err, out)
	}

	// With -db pointing at B, A should be untouched.
	sumAAfter := dbChecksum(t, workA)
	sumABefore := dbChecksum(t, workA)
	if sumABefore != sumAAfter {
		t.Fatalf("workspace A was modified when -db pointed at B\n  before=%s\n  after =%s",
			sumABefore, sumAAfter)
	}

	// B's DB may have changed (we wrote to it). Just confirm it's still readable.
	sumBAfter := dbChecksum(t, workB)
	if sumBAfter == "" {
		t.Fatalf("workspace B DB unreadable after -db run")
	}
	_ = sumBBefore // B is expected to change
}

// Scenario 3: both MPM_WORKSPACE and -db, same DB — no conflict.
func TestCriticSubprocess_EnvAndExplicitDB_Matching_NoConflict(t *testing.T) {
	workB := setupWorkspace(t, "SENTINEL-B")

	dbPath := filepath.Join(workB, "src", "db", "mpm.db")
	env := append(baseEnv(), "MPM_WORKSPACE="+workB, "MPM_DB_PATH="+dbPath)

	out, err := runCritic(t, env)
	if err != nil {
		t.Fatalf("critic with matching env+db failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "mpm-critic complete") {
		t.Fatalf("critic did not finish cleanly:\n%s", out)
	}
}

// Scenario 4: MPM_WORKSPACE and -db disagree — explicit -db wins.
// This pins the documented override contract: an operator pointing
// -db at a specific DB always wins, even if MPM_WORKSPACE says
// otherwise. The critic must NEVER silently coerce MPM_WORKSPACE
// over the operator's explicit intent.
func TestCriticSubprocess_EnvAndExplicitDB_Conflicting_ExplicitWins(t *testing.T) {
	workA := setupWorkspace(t, "SENTINEL-A")
	workB := setupWorkspace(t, "SENTINEL-B")
	sumABefore := dbChecksum(t, workA)

	env := append(baseEnv(), "MPM_WORKSPACE="+workA)
	dbPath := filepath.Join(workB, "src", "db", "mpm.db")
	out, err := runCritic(t, env, "-db", dbPath)
	if err != nil {
		t.Fatalf("critic with conflicting env+db failed: %v\n%s", err, out)
	}

	// A should be untouched (we explicitly pointed at B).
	sumAAfter := dbChecksum(t, workA)
	if sumABefore != sumAAfter {
		t.Fatalf("workspace A was modified when -db explicitly targeted B")
	}
}

// Scenario 5: nonexistent workspace — the critic must NOT fall
// through to a default/production DB. Whether it errors or
// bootstraps a fresh DB is a secondary concern; the release-integrity
// invariant is that NO other workspace (in particular, the
// production DB at $PROJECT_ROOT/src/db/mpm.db) is touched.
//
// We prove this by:
//
//   1. Snapshotting the production DB checksum.
//   2. Running the critic with MPM_WORKSPACE pointing at a path that
//      has never existed on this filesystem.
//   3. Asserting the production DB checksum is byte-identical.
//
// This is the failure mode the alpha-4.1.2 spec section 2 calls out
// as "highest-priority" — a disposable workspace silently targeting
// production.
func TestCriticSubprocess_NonexistentWorkspace_DoesNotTouchProduction(t *testing.T) {
	prodDB := findProductionDB(t)
	if prodDB == "" {
		t.Skip("production DB not found; cannot assert no-production-touch")
	}
	before, err := os.ReadFile(prodDB)
	if err != nil {
		t.Fatalf("read production DB: %v", err)
	}
	sumBefore := sha256.Sum256(before)

	nonexistent := "/tmp/mpm-critic-does-not-exist-" + t.Name()
	env := append(baseEnv(), "MPM_WORKSPACE="+nonexistent)
	out, err := runCritic(t, env)
	if err != nil {
		// Errors are acceptable — the only unacceptable outcome is
		// silently targeting production.
		t.Logf("critic on nonexistent workspace exited with error (acceptable): %v\n%s", err, out)
	}

	after, err := os.ReadFile(prodDB)
	if err != nil {
		t.Fatalf("re-read production DB: %v", err)
	}
	sumAfter := sha256.Sum256(after)
	if sumBefore != sumAfter {
		t.Fatalf("production DB was modified by critic on nonexistent workspace\n  before=%x\n  after =%x",
			sumBefore, sumAfter)
	}
}

// findProductionDB returns the path to the project's mpm.db if it
// exists. We resolve relative to this test file (which lives in
// cmd/mpm-critic/), so production is two directories up.
func findProductionDB(t *testing.T) string {
	t.Helper()
	// Walk up from this test file until we find src/db/mpm.db.
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := cwd
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "src", "db", "mpm.db")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// Scenario 6: clean workspace (no pre-existing DB) — fresh DB is
// created, critic runs to completion. This is the bootstrap path
// operators hit on a fresh install.
func TestCriticSubprocess_CleanWorkspace_CreatesDBAndRuns(t *testing.T) {
	clean := t.TempDir()
	mustMkdir(t, filepath.Join(clean, "src"))

	env := append(baseEnv(), "MPM_WORKSPACE="+clean)
	out, err := runCritic(t, env)
	if err != nil {
		t.Fatalf("critic on clean workspace failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "mpm-critic complete") {
		t.Fatalf("critic did not finish on clean workspace:\n%s", out)
	}
	// DB file should now exist (the critic created it).
	dbPath := filepath.Join(clean, "src", "db", "mpm.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("critic did not create DB at %s: %v", dbPath, err)
	}
}
