// db_path_resolution_test.go — the D4 contract at the EXECUTABLE level.
//
// THE DEFECT THIS PINS
//
// The CLI ignored MPM_DB_PATH entirely and always opened the workspace's
// own database. That is defensible on its own — but it meant an
// operator who set MPM_DB_PATH and then ran the CLI was told nothing
// about the fact that the CLI was not using it. The operator's writes
// went to the workspace's database while the environment claimed
// another one, and nothing said so.
//
// The scheduler and the critic had the opposite defect: they honoured
// MPM_DB_PATH by DERIVING a workspace from it with
// Dir(Dir(Dir(path))), so a path without the canonical
// <root>/src/db/mpm.db shape was silently rewritten into a different
// database.
//
// THE CONTRACT (D4)
//
// MPM_WORKSPACE is the canonical workspace selector. MPM_DB_PATH may
// only RESTATE the workspace's own database. Anything else is an
// explicit conflict error that names both paths.
//
// This test is executable-level on purpose: the resolver's unit tests
// live in internal/core/config, but the defect lived in the WIRING —
// a binary that never called the resolver. A resolver that is correct
// and unused is not a fix.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCLI builds the mpm binary under test.
func buildCLI(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	bin := filepath.Join(t.TempDir(), "mpm")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build mpm: %v", err)
	}
	return bin
}

// TestD4_DivergentEnvOverrideIsRefused pins the headline case for the
// CLI: an MPM_DB_PATH that names a database other than the workspace's
// own must be refused, not ignored.
func TestD4_DivergentEnvOverrideIsRefused(t *testing.T) {
	bin := buildCLI(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")

	cmd := exec.Command(bin, "ls")
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+bad)
	out, err := cmd.CombinedOutput()

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
	// It must also name the database the workspace actually owns, so
	// the operator can see the disagreement rather than infer it.
	canonical := filepath.Join(ws, "src", "db", "mpm.db")
	if !strings.Contains(msg, canonical) {
		t.Errorf("the refusal must name the workspace's own database %q; got:\n%s", canonical, msg)
	}
}

// TestD4_ConsistentEnvironmentIsAccepted is the control: the live
// installation's exact configuration must NOT be refused.
func TestD4_ConsistentEnvironmentIsAccepted(t *testing.T) {
	bin := buildCLI(t)

	ws := t.TempDir()
	canonical := filepath.Join(ws, "src", "db", "mpm.db")

	cmd := exec.Command(bin, "ls")
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+canonical)
	out, _ := cmd.CombinedOutput()

	if strings.Contains(string(out), "database path conflict") {
		t.Fatalf("a consistent MPM_DB_PATH must be accepted; got:\n%s", out)
	}
}

// TestD4_VersionSurfaceStaysFreeOfTheGate pins that the version
// interception still runs BEFORE the gate: `mpm --version` must answer
// even when the configuration and the database selectors are both
// broken, because it is the command an operator runs when something is
// wrong.
func TestD4_VersionSurfaceStaysFreeOfTheGate(t *testing.T) {
	bin := buildCLI(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")
	// A configuration file that does not unmarshal.
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(`{"components": {"memory": true}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "--version")
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+bad)
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Fatalf("--version must succeed regardless of the configuration; got %v:\n%s", err, out)
	}
	msg := string(out)
	if !strings.Contains(msg, "commit:") {
		t.Errorf("--version must report the embedded build identity; got:\n%s", msg)
	}
	if strings.Contains(msg, "database path conflict") {
		t.Errorf("--version must not be gated on the database target; got:\n%s", msg)
	}
}
