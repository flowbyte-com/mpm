// release_pass_20260914_dashboard_hints_test.go — Regression
// coverage for the dashboard's "absent component" hints
// (release-pass brief cases Y, Z).
//
// 2026-09-14 release-pass: when a configurable dashboard
// component is absent/degraded, the dashboard hint must
// include the canonical command used to configure or repair
// it. The hint is intentionally short; the detailed
// capability/degradation explanation lives in Doctor/help.
//
// LLM hint: must include `mpm config` (or equivalent).
// Embedding hint: must include `mpm config detect-embedding`
// (or equivalent).

package main

import (
	"path/filepath"
	"strings"
	"testing"

	stdlibexec "os/exec"
)

// TestDashboard_AbsentLLMHintsCanonicalConfigCommand (case Y).
// With no LLM profile configured, the dashboard's LLM row
// hint must include the canonical command `mpm config` so the
// operator has the next action inline.
func TestDashboard_AbsentLLMHintsCanonicalConfigCommand(t *testing.T) {
	bin := buildDashboardBin(t)
	ws := t.TempDir()
	t.Setenv("QUIET", "1")

	dashOut := mustRunMpmBin(t, bin, ws)

	if !strings.Contains(dashOut, "LLM provider") {
		t.Fatalf("dashboard must surface LLM provider row; got:\n%s", dashOut)
	}
	// The hint row follows the LLM provider row, indented with
	// the canonical "→" arrow. The hint MUST include the
	// canonical config command.
	if !strings.Contains(dashOut, "mpm config") {
		t.Errorf("dashboard LLM hint must include canonical `mpm config` command; got:\n%s", dashOut)
	}
}

// TestDashboard_AbsentEmbeddingHintsCanonicalConfigCommand
// (case Z). With no embedding profile configured, the
// dashboard's Embedding row hint must include the canonical
// `mpm config` command (Custom + protocol manual path).
// 2026-09-14 final-simplification: detect-embedding is retired
// from the public CLI; the hint must NOT mention it.
func TestDashboard_AbsentEmbeddingHintsCanonicalConfigCommand(t *testing.T) {
	bin := buildDashboardBin(t)
	ws := t.TempDir()
	t.Setenv("QUIET", "1")

	dashOut := mustRunMpmBin(t, bin, ws)

	if !strings.Contains(dashOut, "Embedding model") {
		t.Fatalf("dashboard must surface Embedding model row; got:\n%s", dashOut)
	}
	if !strings.Contains(dashOut, "mpm config") {
		t.Errorf("dashboard Embedding hint must include canonical `mpm config` command; got:\n%s", dashOut)
	}
	if strings.Contains(dashOut, "detect-embedding") {
		t.Errorf("dashboard Embedding hint must NOT mention the retired detect-embedding command; got:\n%s", dashOut)
	}
}

// --- helpers ---

func buildDashboardBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func mustRunMpmBin(t *testing.T, bin, ws string) string {
	t.Helper()
	cmd := stdlibexec.Command(bin)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath(), "QUIET=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = err // dashboard may exit non-zero on attention items.
	}
	return stripLogNoise(string(out))
}
