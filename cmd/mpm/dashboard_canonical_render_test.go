// dashboard_canonical_render_test.go — Regression coverage for the
// 2026-09-14 release-pass migration of the bare `mpm` dashboard
// (`PrintQuicklinks`) to the canonical visual grammar.
//
// The dashboard surface must:
//   1. Use the `MPM · Dashboard` heading (canonical amber/yellow bold).
//   2. Render `Memories` (not the legacy `Memorys` typo).
//   3. NOT infer user history from the absence of a session/handoff.
//   4. Distinguish LLM provider from Embedding model configuration,
//      resolved via the canonical helpers (not raw c.Synth fields).
//   5. Render `Embedding model` as `not configured · optional`
//      when absent (informational, not a defect).
//   6. Counts use the canonical query/source for each scope.

package main

import (
	"strings"
	"testing"
)

// TestDashboard_UsesExactHeading asserts the bare `mpm` dashboard
// opens with `MPM · Dashboard`.
func TestDashboard_UsesExactHeading(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace)
	out := string(stdout)
	if !strings.Contains(out, "MPM · Dashboard") {
		t.Fatalf("bare `mpm` must show canonical heading `MPM · Dashboard`.\nGot:\n%s", out)
	}
}

// TestDashboard_MemoriesNotMemorys asserts the dashboard uses
// `Memories` (not the legacy `Memorys` typo).
func TestDashboard_MemoriesNotMemorys(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace)
	out := string(stdout)
	if !strings.Contains(out, "Memories") {
		t.Fatalf("dashboard must contain `Memories` (canonical spelling).\nGot:\n%s", out)
	}
	if strings.Contains(out, "Memorys:") {
		t.Fatalf("dashboard must NOT contain legacy `Memorys:` typo.\nGot:\n%s", out)
	}
}

// TestDashboard_NoFirstRunInference asserts the dashboard does not
// infer user history from the absence of a session/handoff. The
// pre-fix wording was `(no prior sessions — this is your first run)`,
// which falsely implies a fresh install; the post-fix wording is
// neutral (`No previous handoff/session available.`).
func TestDashboard_NoFirstRunInference(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace)
	out := string(stdout)
	if strings.Contains(out, "this is your first run") {
		t.Fatalf("dashboard must not infer `first run` from absence of handoff.\nGot:\n%s", out)
	}
}

// TestDashboard_ProviderStateLines asserts the dashboard renders
// explicit `LLM provider` and `Embedding model` rows (the canonical
// terminology). The pre-fix wording conflated both under
// `AI provider`.
func TestDashboard_ProviderStateLines(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace)
	out := string(stdout)
	if !strings.Contains(out, "LLM provider") {
		t.Fatalf("dashboard must contain canonical `LLM provider` row.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "Embedding model") {
		t.Fatalf("dashboard must contain canonical `Embedding model` row.\nGot:\n%s", out)
	}
}

// TestDashboard_NoWelcomeHeader asserts the legacy
// `Welcome to MPM — no AI provider configured` welcome header is
// removed; provider state is rendered as a row in the readiness
// output instead.
func TestDashboard_NoWelcomeHeader(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace)
	out := string(stdout)
	if strings.Contains(out, "Welcome to MPM") {
		t.Fatalf("dashboard must not contain the legacy `Welcome to MPM — no AI provider configured` header.\nGot:\n%s", out)
	}
}
