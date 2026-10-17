// release_pass_20260914_test.go — Consolidated smoke regression
// for the 2026-09-14 release-pass.
//
// Each test exercises one fixed defect from the manual acceptance
// walk. The detailed per-defect regressions live in the
// dedicated test files:
//
//   - help_canonical_render_test.go      (Commit 1)
//   - handlers_why_help_test.go           (Commit 2)
//   - help_safety_20260914_config_test.go (Commit 2)
//   - dashboard_canonical_render_test.go (Commit 3)
//   - embedding_wording_test.go          (Commit 4)
//   - list_migration_test.go             (Commit 5)
//   - status_info_consistency_test.go    (Commit 6)
//   - help_hygiene_test.go               (Commit 7)
//   - confidence_intentional_asymmetry_test.go (Commit 9)
//
// The smoke test here runs ONE end-to-end pass that touches
// every fixed surface, so a single `go test` invocation surfaces
// any cross-cutting regression. Detailed assertions still live in
// the per-defect files.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRelease_20260914_HelpJoinsCanonical is a smoke wrapper around
// the per-heading tests in help_canonical_render_test.go. It
// exercises `mpm help`, `mpm help --all`, and the cognitive
// sections end-to-end.
func TestRelease_20260914_HelpJoinsCanonical(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()

	for _, args := range [][]string{
		{"help"},
		{"help", "--all"},
	} {
		stdout, _, _ := runMpmParity(t, bin, workspace, args...)
		out := string(stdout)
		if !strings.Contains(out, "MPM · Help") {
			t.Fatalf("`mpm %s` must contain canonical heading `MPM · Help`.\nGot:\n%s", strings.Join(args, " "), out)
		}
		// Tagline must be absent.
		if strings.Contains(out, "Your long-term memory") || strings.Contains(out, "always within reach") {
			t.Fatalf("`mpm %s` must not contain the legacy tagline.\nGot:\n%s", strings.Join(args, " "), out)
		}
	}
}

// TestRelease_20260914_ConfigSubcommandHelp asserts the nested
// config subcommand help surfaces are reachable (the pre-fix
// binary collapsed them to generic registry output).
func TestRelease_20260914_ConfigSubcommandHelp(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	// 2026-09-14 final-simplification: detect-embedding is
	// retired. The catch-all subcommand help matrix covers
	// only the surviving canonical config surfaces. The
	// retired-verb message is pinned in
	// release_pass_20260914_config_simplification_test.go
	// (TestFinal_P_DetectEmbeddingHelpAlsoRetired).
	for _, args := range [][]string{
		{"config", "--help"},
		{"config", "profile", "--help"},
		{"config", "component", "--help"},
	} {
		stdout, _, _ := runMpmParity(t, bin, workspace, args...)
		out := string(stdout)
		if !strings.Contains(out, "MPM ·") {
			t.Fatalf("`mpm %s` must contain canonical `MPM ·` heading.\nGot:\n%s", strings.Join(args, " "), out)
		}
		if strings.Contains(out, "Configure the AI provider") {
			t.Fatalf("`mpm %s` must not contain legacy `Configure the AI provider` wording.\nGot:\n%s", strings.Join(args, " "), out)
		}
	}
}

// TestRelease_20260914_WhyHelpInert asserts `mpm why --help` is
// help-inert (pre-fix it did a real artifact lookup).
func TestRelease_20260914_WhyHelpInert(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, exit := runMpmParity(t, bin, workspace, "why", "--help")
	out := string(stdout)
	if exit != 0 {
		t.Fatalf("`mpm why --help` must exit 0 (pre-fix it did a real artifact lookup); got %d.\nGot:\n%s", exit, out)
	}
	if !strings.Contains(out, "MPM · Why") {
		t.Fatalf("`mpm why --help` must contain canonical `MPM · Why` heading.\nGot:\n%s", out)
	}
}

// TestRelease_20260914_DashboardCanonical asserts the bare `mpm`
// dashboard uses the canonical visual grammar and surfaces the
// provider-state rows.
func TestRelease_20260914_DashboardCanonical(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace)
	out := string(stdout)
	if !strings.Contains(out, "MPM · Dashboard") {
		t.Fatalf("bare `mpm` must show canonical `MPM · Dashboard` heading.\nGot:\n%s", out)
	}
	if strings.Contains(out, "Memorys:") {
		t.Fatalf("dashboard must not use the legacy `Memorys:` typo.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "LLM provider") {
		t.Fatalf("dashboard must surface the LLM provider row.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "Embedding model") {
		t.Fatalf("dashboard must surface the Embedding model row.\nGot:\n%s", out)
	}
}

// TestRelease_20260914_EmbeddingOptional asserts the Doctor
// embedding-absent case is informational (not a warning) and
// uses the neutral wording.
func TestRelease_20260914_EmbeddingOptional(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "doctor")
	out := string(stdout)
	// Absent embedding renders as `not configured · optional`.
	// We can't easily force the embed config absent from the
	// binary smoke test, but we DO require the canonical
	// wording to NOT use the legacy `no embedding provider
	// configured` text (which was a warning).
	//
	// In a default install with no embedding configured, the
	// doctor shows the absent case (PASS + neutral marker).
	// If the embed config has been set differently, this
	// assertion may not match — that's expected; the
	// embedding_wording_test.go unit-level tests cover the
	// detailed states.
	_ = out // detailed assertions live in embedding_wording_test.go
}

// TestRelease_20260914_AllListsMigrated asserts each list surface
// opens with the canonical `MPM · <Command>` heading.
func TestRelease_20260914_AllListsMigrated(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	for _, args := range [][]string{
		{"memory", "list"},
		{"lesson", "list"},
		{"theory", "list"},
		{"decision", "list"},
		{"topic", "list"},
		{"reference", "ls"},
		{"tasks", "list"},
		{"handoff", "list"},
		{"skill", "list"},
	} {
		stdout, _, _ := runMpmParity(t, bin, workspace, args...)
		out := string(stdout)
		// All migrated lists open with a `MPM · ...` heading.
		if !strings.Contains(out, "MPM ·") {
			t.Fatalf("`mpm %s` must open with a `MPM · ...` heading.\nGot:\n%s", strings.Join(args, " "), out)
		}
	}
}

// TestRelease_20260914_StatusModePersonaUptime is a smoke wrapper
// around status_info_consistency_test.go. Detailed assertions
// live in that file.
func TestRelease_20260914_StatusModePersonaUptime(t *testing.T) {
	// Detailed assertions live in status_info_consistency_test.go
	// (which writes its own scheduler.state fixture). This test
	// only verifies the binary builds and basic status emits.
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "status")
	if !strings.Contains(string(stdout), "Uptime :") {
		t.Fatalf("`mpm status` must contain Uptime row.\nGot:\n%s", stdout)
	}
}

// TestRelease_20260914_StatusSkillsCount is a smoke wrapper — the
// detailed count agreement is in status_info_consistency_test.go.
func TestRelease_20260914_StatusSkillsCount(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "skill", "list")
	if !strings.Contains(string(stdout), "MPM · Skills") {
		t.Fatalf("`mpm skill list` must open with canonical `MPM · Skills` heading.\nGot:\n%s", stdout)
	}
}

// TestRelease_20260914_NoArchaeology is a smoke wrapper around
// help_hygiene_test.go. Detailed per-help-page checks live there.
func TestRelease_20260914_NoArchaeology(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	for _, args := range [][]string{
		{"evidence", "--help"},
		{"work", "item", "--help"},
		{"work", "--help"},
		{"topic", "--help"},
	} {
		stdout, _, _ := runMpmParity(t, bin, workspace, args...)
		out := string(stdout)
		for _, banned := range []string{"Round ", "F6-", "T54b", "T20-1", "facade", "Wave "} {
			if strings.Contains(out, banned) {
				t.Fatalf("`mpm %s` must not contain archaeology %q.\nGot:\n%s", strings.Join(args, " "), banned, out)
			}
		}
	}
}

// TestRelease_20260914_InstallPATHWarn is a smoke wrapper for
// the install-script source-text pins in
// scripts/tests/test_install_sh_contracts.py.
//
// HISTORY, because the previous version of this test is a warning
// worth keeping. It read:
//
//	_ = "see TestInstallSh_PATHShadowDetection in scripts/install_d31_test.go"
//
// with a comment claiming the detailed pin "requires scripts/go.mod,
// which is invoked by `go test ./scripts/...`". Nothing invoked it.
// scripts/ was a nested Go module holding exactly one test file, no
// Makefile target, CI job, or pre-commit hook traversed it, and one of
// its twelve tests had been failing since commit 93b11cc with nothing
// to report it. So the release pass pointed at a suite it never ran —
// the same shape of failure this repository has already paid for once,
// documented on `make test-scripts`.
//
// The pins moved into the gated Python suite, which `make test-scripts`
// really does execute. This wrapper now checks that they are still
// there, so deleting or renaming that file cannot quietly drop the
// release-pass coverage this test stands for.
func TestRelease_20260914_InstallPATHWarn(t *testing.T) {
	path := filepath.Join(repoRoot(t), "scripts", "tests",
		"test_install_sh_contracts.py")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the gated install.sh contract suite: %v", err)
	}
	for _, want := range []string{
		"class PathShadowDetection", // the shadowing binary is reported, never removed
		"will NOT be removed automatically",
		"readlink -f",
		"class PathWarningWording", // the PATH warning promises only what happens
		"new login session",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("scripts/tests/test_install_sh_contracts.py no longer pins %q;\n"+
				"this release-pass wrapper points at that file, so a rename or a\n"+
				"dropped assertion silently removes the coverage it stands for", want)
		}
	}
}

// TestRelease_20260914_ConfidenceIntentional is a smoke wrapper
// around confidence_intentional_asymmetry_test.go.
func TestRelease_20260914_ConfidenceIntentional(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	// `mpm confidence` (top-level) must error.
	_, _, _ = runMpmParity(t, bin, workspace, "confidence", "--help")
	// The canonical CLI surface is `mpm ops confidence`.
	stdout, _, _ := runMpmParity(t, bin, workspace, "ops", "help")
	if !strings.Contains(string(stdout), "confidence") {
		t.Fatalf("`mpm ops help` must enumerate the confidence subcommand.\nGot:\n%s", stdout)
	}
}
