// help_hygiene_test.go — Regression coverage for the 2026-09-14
// release-pass help-text hygiene sweep.
//
// User-facing help text must NOT contain:
//   - Round / T-number audit/test identifiers
//   - "discoverable facade over mpm call ..." implementation language
//   - "internally the substrate's existing scratchpad APIs are used"
//     or similar implementation archaeology
//   - "v1 evidence types (single source of truth: internal/core/...)"
//     source-tree pointers
//   - dated "fixed on YYYY-MM-DD" / "2026-09-10 fix" commentary
//   - Wave N markers in section-help bodies
//
// The product documentation must describe current supported
// syntax only — no implementation plumbing, no test archaeology.

package main

import (
	"strings"
	"testing"
)

// TestEvidenceHelp_NoArchaeology asserts `mpm evidence --help`
// does not contain source-tree pointers or "single source of truth"
// framing.
func TestEvidenceHelp_NoArchaeology(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "evidence", "--help")
	out := string(stdout)
	for _, phrase := range []string{
		"single source of truth",
		"internal/core/",
		"v1 evidence types",
	} {
		if strings.Contains(out, phrase) {
			t.Fatalf("`mpm evidence --help` must not contain archaeology %q.\nGot:\n%s", phrase, out)
		}
	}
}

// TestWorkItemHelp_NoRoundNoFacadeNoInternal asserts `mpm work item --help`
// does not contain Round/T-number, "facade over mpm call mpm_work", or
// "internally the substrate's ..." implementation language.
func TestWorkItemHelp_NoRoundNoFacadeNoInternal(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "work", "item", "--help")
	out := string(stdout)
	for _, phrase := range []string{
		"Round ",
		"F6-",
		"T54b",
		"T20-1",
		"discoverable facade",
		"internally the substrate",
	} {
		if strings.Contains(out, phrase) {
			t.Fatalf("`mpm work item --help` must not contain archaeology %q.\nGot:\n%s", phrase, out)
		}
	}
}

// TestWorkHelp_NoFacadeNoInternal asserts `mpm work --help` does not
// contain "discoverable facade" or "internally the substrate's
// existing scratchpad APIs" implementation language. The
// standalone-CLI session-identity section is preserved (genuinely
// useful operator guidance), so the test allows that section.
func TestWorkHelp_NoFacadeNoInternal(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "work", "--help")
	out := string(stdout)
	for _, phrase := range []string{
		"discoverable facade",
		"internally the substrate",
		"scratchpad APIs",
	} {
		if strings.Contains(out, phrase) {
			t.Fatalf("`mpm work --help` must not contain archaeology %q.\nGot:\n%s", phrase, out)
		}
	}
}

// TestTopicHelp_NoDatedFixNotes asserts `mpm topic --help` does not
// contain "2026-09-10 fix — ..." dated commentary.
func TestTopicHelp_NoDatedFixNotes(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "topic", "--help")
	out := string(stdout)
	for _, phrase := range []string{
		"2026-09-10 fix",
		"fix — --name",
		"no longer creates a topic literally",
	} {
		if strings.Contains(out, phrase) {
			t.Fatalf("`mpm topic --help` must not contain archaeology %q.\nGot:\n%s", phrase, out)
		}
	}
}

// TestHelpSections_NoWaveMarkers asserts that `mpm help <section>`
// and `mpm help --all` output does not contain Wave N+ markers
// (e.g. "Wave 2 surface", "Wave 1 commit").
func TestHelpSections_NoWaveMarkers(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	for _, args := range [][]string{
		{"help", "--all"},
		{"help", "knowledge"},
		{"help", "runtime"},
		{"help", "maintenance"},
		{"help", "reflection"},
		{"help", "work"},
		{"help", "explain"},
	} {
		stdout, _, _ := runMpmParity(t, bin, workspace, args...)
		out := string(stdout)
		if strings.Contains(out, "Wave ") {
			t.Fatalf("`mpm %s` output must not contain Wave markers.\nGot:\n%s", strings.Join(args, " "), out)
		}
	}
}
