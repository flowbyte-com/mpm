package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelpFlag_NeverMutates is the D5 regression test (2026-08-25).
//
// Bug: parseFlags rewrote --help/-h into the literal argument "help", which
// then fell through as DATA into command handlers:
//   - `mpm prune --help` performed a real prune (destructive)
//   - `mpm gc --help`    ran a decay sweep
//   - `mpm show --help`  looked up memory "help"
//   - `mpm reinforce --help` errored on id "help"
//   - `mpm recall --help` searched for the word "help"
//
// Invariant: `mpm <command> --help` (and -h) must print help, exit 0, and
// perform NO mutation.
func TestHelpFlag_NeverMutates(t *testing.T) {
	binPath := requireBuiltCLI(t)

	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	env := append(os.Environ(), "MPM_WORKSPACE="+workspace)

	run := func(args ...string) (string, int) {
		cmd := exec.Command(binPath, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	sqliteQuery := func(query string) string {
		dbPath := filepath.Join(workspace, "src", "db", "mpm.db")
		cmd := exec.Command("sqlite3", dbPath, query)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("sqlite3 %q: %v", query, err)
		}
		return strings.TrimSpace(string(out))
	}

	helpCases := []struct {
		name string
		args []string
	}{
		{"prune --help", []string{"prune", "--help"}},
		{"prune -h", []string{"prune", "-h"}},
		{"gc --help", []string{"gc", "--help"}},
		{"show --help", []string{"show", "--help"}},
		{"reinforce --help", []string{"reinforce", "--help"}},
		{"recall --help", []string{"recall", "--help"}},
	}
	for _, tc := range helpCases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(tc.args...)
			if code != 0 {
				t.Errorf("%s exited %d; help must exit 0\noutput: %s", tc.name, code, out)
			}
			if !strings.Contains(out, "Usage:") {
				t.Errorf("%s output missing Usage line:\n%s", tc.name, out)
			}
			if strings.Contains(out, "Memory not found") || strings.Contains(strings.ToLower(out), "no memories found") {
				t.Errorf("%s treated the help flag as data:\n%s", tc.name, out)
			}
		})
	}

	// Destructive commands: a prune-able row must SURVIVE `prune --help`
	// and `gc --help`.
	run("add", "--ttl", "24h", "d5 expiry probe")
	// Force the row expired so any real prune/gc pass would delete it.
	dbPath := filepath.Join(workspace, "src", "db", "mpm.db")
	sq := exec.Command("sqlite3", dbPath,
		`UPDATE memories SET expires_at = strftime('%s','now') - 50 WHERE content='d5 expiry probe';`)
	if sqOut, err := sq.CombinedOutput(); err != nil {
		t.Fatalf("expire setup: %v (%s)", err, sqOut)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"prune --help no-mutation", []string{"prune", "--help"}},
		{"gc --help no-mutation", []string{"gc", "--help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, code := run(tc.args...); code != 0 {
				t.Fatalf("%s exited %d", tc.name, code)
			}
			if got := sqliteQuery(`SELECT count(*) FROM memories WHERE content='d5 expiry probe';`); got != "1" {
				t.Errorf("%s mutated state: probe rows remaining = %s, want 1", tc.name, got)
			}
		})
	}

	// Sanity: a REAL prune still deletes the expired row (the fix must not
	// have neutered the operation itself).
	if _, code := run("prune"); code != 0 {
		t.Fatalf("real prune exited %d", code)
	}
	if got := sqliteQuery(`SELECT count(*) FROM memories WHERE content='d5 expiry probe';`); got != "0" {
		t.Errorf("real prune did not remove expired row; remaining = %s", got)
	}
}
