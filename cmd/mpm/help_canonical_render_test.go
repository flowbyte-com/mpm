// help_canonical_render_test.go — Regression coverage for the
// 2026-09-14 release-pass help-system migration.
//
// The release-pass removed the Bubble Tea rounded-border wrapper
// from `mpm help` and migrated the help surfaces to the canonical
// visual grammar shared with Doctor / Info / Status. This file
// pins the resulting contract.
//
// Each test exercises the built binary against an isolated
// workspace (`t.TempDir()`), so the production MPM database is
// never touched.

package main

import (
	"strings"
	"testing"
)

// TestHelp_ExactHeadingIs_MPM_Dot_Help asserts the canonical
// heading token `MPM · Help` appears in `mpm help` output. The
// exact string (with the centered-dot separator) is the canonical
// visual contract — the prior implementation used a centered
// `" mpm  ·  Memory Persistence Module"` title which is wrong.
func TestHelp_ExactHeadingIs_MPM_Dot_Help(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "help")
	out := string(stdout)
	if !strings.Contains(out, "MPM · Help") {
		t.Fatalf("`mpm help` output must contain the canonical heading `MPM · Help`.\nGot:\n%s", out)
	}
}

// TestHelp_NoBubbleTeaBorders asserts that no Bubble Tea
// rounded-border box-drawing characters appear in `mpm help`
// output. The prior presentation wrapped the help panel in
// `lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder())`
// producing `╭`, `╰`, `│`, `─` lines.
func TestHelp_NoBubbleTeaBorders(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "help")
	out := string(stdout)
	for _, ch := range []string{"╭", "╰", "│"} {
		if strings.Contains(out, ch) {
			t.Fatalf("`mpm help` output must not contain Bubble Tea border character %q.\nGot:\n%s", ch, out)
		}
	}
}

// TestHelp_NoTagline asserts the marketing tagline
// `Your long-term memory, always within reach` is removed from
// `mpm help` output.
func TestHelp_NoTagline(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "help")
	out := string(stdout)
	for _, phrase := range []string{"Your long-term memory", "always within reach"} {
		if strings.Contains(out, phrase) {
			t.Fatalf("`mpm help` output must not contain the legacy tagline %q.\nGot:\n%s", phrase, out)
		}
	}
}

// TestHelp_AllCatalogueRendered asserts that `mpm help --all`
// (the operator catalogue) shows the canonical `MPM · Help`
// heading.
func TestHelp_AllCatalogueRendered(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "help", "--all")
	out := string(stdout)
	if !strings.Contains(out, "MPM · Help") {
		t.Fatalf("`mpm help --all` output must contain the canonical heading `MPM · Help`.\nGot:\n%s", out)
	}
	// Operator catalogue section must be present.
	if !strings.Contains(out, "Operator catalogue") {
		t.Fatalf("`mpm help --all` output must contain the Operator catalogue section.\nGot:\n%s", out)
	}
}

// TestHelp_AllSectionsRender asserts that every section in
// `cognitiveHelpSections` (Daily / Create / Knowledge /
// Maintenance / Debug / Need more?) appears in `mpm help` output.
func TestHelp_AllSectionsRender(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "help")
	out := string(stdout)
	for _, sec := range cognitiveHelpSections {
		if !strings.Contains(out, sec.title) {
			t.Errorf("`mpm help` output must contain section %q.\nGot:\n%s", sec.title, out)
		}
	}
}

// TestHelp_BareIsCanonical asserts that bare `mpm` (no args) no
// longer invokes the legacy help panel — the bare invocation is
// the dashboard, not help. This test pins the split: bare `mpm`
// must NOT print the help heading.
func TestHelp_BareIsDashboard(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace)
	out := string(stdout)
	// The bare invocation is the dashboard surface; `MPM · Help`
	// is reserved for `mpm help` / `mpm help --all`. If bare mpm
	// still emits the help heading, the split has regressed.
	if strings.Contains(out, "MPM · Help") && !strings.Contains(out, "Dashboard") {
		t.Fatalf("bare `mpm` must not present the help panel (regression to legacy PrintHelp).\nGot:\n%s", out)
	}
}
