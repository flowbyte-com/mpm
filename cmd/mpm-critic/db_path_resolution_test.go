// db_path_resolution_test.go — the D4 contract at the EXECUTABLE level.
//
// THE DEFECT THIS PINS
//
// mpm-critic derived the workspace from an explicit -db / MPM_DB_PATH
// with Dir(Dir(Dir(path))). The derivation ASSUMED that the path had
// the canonical <root>/src/db/mpm.db shape and never checked it:
//
//	-db /tmp/other.db     →  workspace "/"  →  DB "/src/db/mpm.db"
//	MPM_DB_PATH=/tmp/x.db  →  workspace "/"  →  DB "/src/db/mpm.db"
//
// The critic then AUDITED that other database — reporting findings
// about a substrate the operator never asked about, and (before the
// read-only adjudication) writing to it.
//
// THE CONTRACT (D4)
//
// MPM_WORKSPACE is the canonical workspace selector. MPM_DB_PATH may
// only RESTATE the workspace's own database. Anything else is an
// explicit conflict error that names both paths.
//
// The critic is the binary that most needs this: it is spawned by the
// scheduler with an inherited environment, so it is the surface where a
// divergent selector does the most damage least visibly.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCritic builds the mpm-critic binary under test.
func buildCritic(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	bin := filepath.Join(t.TempDir(), "mpm-critic")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build mpm-critic: %v", err)
	}
	return bin
}

// TestD4_ExplicitNonCanonicalDBPathIsRefused pins the headline case.
func TestD4_ExplicitNonCanonicalDBPathIsRefused(t *testing.T) {
	bin := buildCritic(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")

	cmd := exec.Command(bin, "-db", bad)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH=")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("a -db without the canonical shape must fail; it exited 0:\n%s", out)
	}
	msg := string(out)
	if !strings.Contains(msg, "database path conflict") {
		t.Errorf("the refusal must say it is a database path conflict; got:\n%s", msg)
	}
	if !strings.Contains(msg, bad) {
		t.Errorf("the refusal must name the rejected path %q; got:\n%s", bad, msg)
	}
	if _, statErr := os.Stat("/src/db/mpm.db"); statErr == nil {
		t.Errorf("D4 REGRESSION: /src/db/mpm.db was created — the old derivation still ran")
	}
}

// TestD4_DivergentEnvOverrideIsRefused pins the environment half.
func TestD4_DivergentEnvOverrideIsRefused(t *testing.T) {
	bin := buildCritic(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")

	cmd := exec.Command(bin)
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
}
