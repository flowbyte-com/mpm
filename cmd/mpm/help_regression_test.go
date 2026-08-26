package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelp_NeverExecutesHandlers is the Stage 7 D5-residual regression test.
//
// Bug: the Stage 6 fix intercepted the rewritten "help" token only at
// args[1] (the first post-command position). parseFlags rewrites -h/--help
// ANYWHERE in argv into the literal token "help", and main.go runs its own
// parseFlags pass before router.Execute — so for handlers that parse
// positional args manually and ignore trailing tokens, `--help` still
// reached the handler as data:
//
//	mpm rm <id> --help            → deleted the memory
//	mpm lesson shred <id> --help  → hard-deleted the lesson
//	mpm challenge <id> --help     → created a theory (mutation)
//	mpm snooze <id> --help        → bumped weight + last_accessed_at
//
// Invariant: a help request must NEVER execute the handler. The command
// must print help/usage and exit 0 with no state change.
func TestHelp_NeverExecutesHandlers(t *testing.T) {
	binPath := filepath.Join("..", "..", "bin", "mpm")
	if _, err := os.Stat(binPath); err != nil {
		t.Skip("bin/mpm not built; run make build first")
	}

	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	run := func(args ...string) (string, int) {
		cmd := exec.Command(binPath, args...)
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	// Seed a victim memory.
	out, _ := run("add", "help-regression-victim")
	if !strings.Contains(out, "Added memory") {
		t.Fatalf("seed add failed: %s", out)
	}
	victimID := strings.Fields(out)[2]

	// Each case: command that used to mutate on --help.
	cases := []struct {
		name string
		args []string
	}{
		{"rm id --help", []string{"rm", victimID, "--help"}},
		{"rm -h id", []string{"rm", "-h", victimID}},
		{"snooze id --help", []string{"snooze", victimID, "--help"}},
		{"reinforce id --help", []string{"reinforce", victimID, "--help"}},
		{"challenge id --help", []string{"challenge", victimID, "--help"}},
		{"shred id --help", []string{"shred", victimID, "--help"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(tc.args...)
			if code != 0 {
				t.Errorf("--help must exit 0, got %d (out=%q)", code, out)
			}
			if !strings.Contains(out, "Usage") && !strings.Contains(out, "usage") &&
				!strings.Contains(out, "help") && !strings.Contains(out, "Run") {
				t.Errorf("expected help output, got %q", out)
			}

			// The load-bearing assertion: victim must still be live in the DB.
			qout, _ := run("show", victimID)
			if strings.Contains(qout, "not found") || qout == "" {
				t.Fatalf("handler EXECUTED despite --help: victim %s gone/hidden\nshow output: %q", victimID, qout)
			}
			dbPath := filepath.Join(ws, "src", "db", "mpm.db")
			got := querySQLite(t, dbPath,
				`SELECT deleted_at IS NOT NULL FROM memories WHERE id = ?`, victimID)
			if got == "1" {
				t.Fatalf("victim %s was soft-deleted by a --help invocation", victimID)
			}
		})
	}
}

// TestHelp_LiteralWordRemainsData guards against over-blocking: a literal
// "help" argument that did NOT come from a help flag stays handler data.
func TestHelp_LiteralWordRemainsData(t *testing.T) {
	binPath := filepath.Join("..", "..", "bin", "mpm")
	if _, err := os.Stat(binPath); err != nil {
		t.Skip("bin/mpm not built; run make build first")
	}
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	run := func(args ...string) string {
		cmd := exec.Command(binPath, args...)
		out, err := cmd.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "Added") {
			t.Fatalf("run %v: %v (%s)", args, err, out)
		}
		return string(out)
	}

	run("add", "help me remember this literal word test")
	out := run("ls")
	if !strings.Contains(out, "help me remember this") {
		t.Fatalf("literal 'help' inside content was mangled: %s", out)
	}
}

// querySQLite is a minimal helper for direct persistence assertions.
func querySQLite(t *testing.T, dbPath, query string, args ...interface{}) string {
	t.Helper()
	cmdArgs := []string{"-c", `
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
r = c.execute(sys.argv[2], sys.argv[3:]).fetchone()
print("" if r is None else r[0])
`, dbPath, query}
	for _, a := range args {
		cmdArgs = append(cmdArgs, a.(string))
	}
	out, err := exec.Command("python3", cmdArgs...).Output()
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return strings.TrimSpace(string(out))
}
