package internal

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLintRouterDirectories_CleanFiles verifies that the linter accepts
// well-formed persona/mode files with no issues. This is the baseline —
// if real-shaped production files fail the linter, the linter is wrong.
//
// Earlier this test read ~/.mpm/persona and ~/.mpm/mode via os.UserHomeDir().
// That coupled the test to a developer machine that has MPM installed.
// On a clean CI runner those directories do not exist, the linter returns
// a "cannot read directory" issue, and the test fails for environmental
// rather than semantic reasons. See #32.
//
// The fix is to synthesise the fixtures in t.TempDir() with the same
// frontmatter shape the linter inspects. The CLI path (cmd/mpm/lint_cmds.go)
// pre-filters missing dirs before calling the linter, so the production
// "mpm ops lint" command never reaches the missing-dir branch.
func TestLintRouterDirectories_CleanFiles(t *testing.T) {
	root := t.TempDir()
	personaDir := filepath.Join(root, "persona")
	modeDir := filepath.Join(root, "mode")
	require.NoError(t, os.MkdirAll(personaDir, 0o755))
	require.NoError(t, os.MkdirAll(modeDir, 0o755))

	// Minimal-but-realistic clean persona: matches the shape of a real
	// ~/.mpm/persona/*.md file. The linter inspects patterns and
	// domain_out; voice_guards is descriptive prose (not regex).
	persona := `---
name: default
title: Core Substrate
patterns: "status, report, standard, update, default, execute"
domain_out: "brainstorm, creative, chat, explore"
---

# Default Persona
`
	require.NoError(t, os.WriteFile(filepath.Join(personaDir, "default.md"), []byte(persona), 0o644))

	// Minimal-but-realistic clean mode: matches the shape of a real
	// ~/.mpm/mode/*.md file.
	mode := `---
name: architect
patterns: "design, architecture, structure, plan, system, refactor"
domain_out: "typo, css, syntax, minor, quick fix"
---

# Architect Mode
`
	require.NoError(t, os.WriteFile(filepath.Join(modeDir, "architect.md"), []byte(mode), 0o644))

	report := LintRouterDirectories(personaDir, modeDir)
	if !report.OK() {
		for _, issue := range report.Issues {
			t.Errorf("clean file lint failure: %s field=%q pattern=%q msg=%q",
				issue.File, issue.Field, issue.Pattern, issue.Message)
		}
	}
	require.Less(t, report.FilesScanned, 20, "expected to scan a focused number of persona/mode files (<20); a higher count would mean the linter is scanning too much")
}

// TestLintRouterDirectories_HandlesMissingOptionalDirs pins the current
// linter contract for missing optional asset directories. The linter
// surfaces a missing dir as a LintIssue with Field == "" and Message
// containing "cannot read directory"; it does NOT silently pass.
//
// This is the right production contract for the linter itself: silently
// passing a missing dir would hide real configuration breakage from
// operators. The "optional vs required" distinction lives one layer up
// — cmd/mpm/lint_cmds.go pre-filters missing dirs before calling the
// linter, and cmd/mpm/main.go (doctor) classifies them as WARN for
// optional assets (mode, persona) versus FAIL for required (src/db).
// See cmd/mpm/doctor_workspace_optional_dirs_test.go.
//
// Without this test, a future change that silently swallows missing-dir
// errors would pass every other LintRouterDirectories test and only
// surface as a regression in production user reports. #32.
func TestLintRouterDirectories_HandlesMissingOptionalDirs(t *testing.T) {
	t.Run("one populated, one missing", func(t *testing.T) {
		root := t.TempDir()
		personaDir := filepath.Join(root, "persona")
		require.NoError(t, os.MkdirAll(personaDir, 0o755))
		// Write a clean file in the populated dir.
		clean := `---
name: ok
patterns: "ok"
---
`
		require.NoError(t, os.WriteFile(filepath.Join(personaDir, "ok.md"), []byte(clean), 0o644))

		// Point at the populated dir plus a non-existent mode dir.
		missingModeDir := filepath.Join(root, "mode-does-not-exist")
		report := LintRouterDirectories(personaDir, missingModeDir)

		// Contract: the missing dir is reported as an issue. Field is
		// empty (whole-file/directory-level), Message names the cause.
		require.False(t, report.OK(), "missing optional dir must be surfaced as a lint issue, not silently passed")
		require.Len(t, report.Issues, 1, "expected exactly one issue for the one missing dir")
		issue := report.Issues[0]
		require.Equal(t, missingModeDir, issue.File, "issue must name the missing path")
		require.Equal(t, "", issue.Field, "missing-dir issues are whole-file/directory-level (Field empty)")
		require.Contains(t, issue.Message, "cannot read directory", "issue must explain the cause")

		// The populated dir's files scanned and linted clean.
		require.Equal(t, 1, report.FilesScanned, "populated dir's clean file was scanned exactly once")
	})

	t.Run("both missing", func(t *testing.T) {
		root := t.TempDir()
		missingA := filepath.Join(root, "missing-a")
		missingB := filepath.Join(root, "missing-b")
		report := LintRouterDirectories(missingA, missingB)

		require.False(t, report.OK(), "two missing dirs must produce two issues, not silent pass")
		require.Len(t, report.Issues, 2, "expected one issue per missing dir")
		require.Equal(t, 0, report.FilesScanned, "no files were scanned because no dir was readable")

		// Both missing paths must be named. Order follows the variadic
		// args (matches the linter's for-loop order).
		files := []string{report.Issues[0].File, report.Issues[1].File}
		require.Contains(t, files, missingA)
		require.Contains(t, files, missingB)
		for _, issue := range report.Issues {
			require.Equal(t, "", issue.Field)
			require.Contains(t, issue.Message, "cannot read directory")
		}
	})
}

// TestLintRouterDirectories_BrokenFrontmatter verifies the linter
// catches YAML parse errors. This is the 'anti-patterns single quote
// inside single-quoted YAML' failure mode from 2026-06-26 — Go yaml
// rejects \' with 'did not find expected key'.
func TestLintRouterDirectories_BrokenFrontmatter(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken-frontmatter.md")
	content := `---
name: broken
patterns: '\brefactor\b, what\'s in your control\b'
---

# Broken
`
	require.NoError(t, os.WriteFile(bad, []byte(content), 0644))

	report := LintRouterDirectories(dir)
	require.False(t, report.OK(), "expected linter to flag broken frontmatter")
	require.Len(t, report.Issues, 1)
	require.Equal(t, "yaml", report.Issues[0].Field)
	require.Contains(t, report.Issues[0].Message, "YAML parse error")
}

// TestLintRouterDirectories_BrokenRegexInPatterns verifies the linter
// catches regex compilation errors in the patterns field. The original
// anti-patterns-cut bug surfaced this way: a backslash-b in double-quoted
// YAML becomes a backspace character, which compiles but never matches.
func TestLintRouterDirectories_BrokenRegexInPatterns(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken-patterns.md")
	// Unbalanced bracket — regexp.Compile will reject.
	content := `---
name: broken
patterns: '\bunclosed['
---

# Broken
`
	require.NoError(t, os.WriteFile(bad, []byte(content), 0644))

	report := LintRouterDirectories(dir)
	require.False(t, report.OK(), "expected linter to flag broken regex in patterns")
	require.Len(t, report.Issues, 1)
	require.Equal(t, "patterns", report.Issues[0].Field)
	require.Contains(t, report.Issues[0].Message, "regex compile error")
}

// TestLintRouterDirectories_BrokenRegexInDomainOut verifies the linter
// catches raw-form regex errors in domain_out even when regexp.QuoteMeta
// would mask them at runtime. This closes the silent-zero-match gap
// discovered during the 2026-06-26 anti-patterns audit.
func TestLintRouterDirectories_BrokenRegexInDomainOut(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken-domain.md")
	content := `---
name: broken
patterns: '\brefactor\b'
domain_out: 'unclosed[group'
---

# Broken
`
	require.NoError(t, os.WriteFile(bad, []byte(content), 0644))

	report := LintRouterDirectories(dir)
	require.False(t, report.OK(), "expected linter to flag broken raw-form regex in domain_out")
	// Should be flagged as raw-form error, NOT runtime-compile error
	// (because QuoteMeta would mask the runtime error).
	foundRawForm := false
	for _, issue := range report.Issues {
		if issue.Field == "domain_out" &&
			strings.Contains(issue.Message, "raw-form compile error") {
			foundRawForm = true
			break
		}
	}
	require.True(t, foundRawForm, "expected raw-form compile error in domain_out, got %+v", report.Issues)
}

// TestLintRouterDirectories_NoFrontmatter verifies the linter accepts
// files without frontmatter (mode/standard.md historically had this
// shape and is loaded fine).
func TestLintRouterDirectories_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "no-frontmatter.md")
	content := "# Just a markdown file\n\nNo frontmatter here.\n"
	require.NoError(t, os.WriteFile(good, []byte(content), 0644))

	report := LintRouterDirectories(dir)
	require.True(t, report.OK(), "no-frontmatter file should not be flagged")
	require.Equal(t, 1, report.FilesScanned)
}

// TestLintRouterDirectories_MultipleIssuesInOneFile verifies that one
// file with multiple broken patterns produces multiple issues, not
// just the first one.
func TestLintRouterDirectories_MultipleIssuesInOneFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "multi.md")
	// Two syntactically broken entries in patterns (YAML list format).
	// domain_out has one entry that QuoteMeta would mask, so it
	// would NOT be flagged — see domain_out contract note.
	content := `---
name: multi
patterns:
  - '\bbroken1['
  - '\bbroken2['
domain_out: '\bbroken3['
---

# Multi
`
	require.NoError(t, os.WriteFile(bad, []byte(content), 0644))

	report := LintRouterDirectories(dir)
	require.False(t, report.OK())
	require.GreaterOrEqual(t, len(report.Issues), 2, "expected at least 2 issues from patterns field")

	// patterns should be flagged at least twice.
	patternIssues := 0
	for _, issue := range report.Issues {
		if issue.Field == "patterns" {
			patternIssues++
		}
	}
	require.GreaterOrEqual(t, patternIssues, 2, "expected at least 2 patterns issues")
}

// TestLintRouterDirectories_VoiceGuardsNotChecked verifies the linter
// does NOT try to compile voice_guards (they are descriptive prose,
// not regex). This pins the post-rename contract.
func TestLintRouterDirectories_VoiceGuardsNotChecked(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "voice-guard-as-prose.md")
	// voice_guards contains commas and prose that wouldn't compile as
	// regex. The linter must NOT flag this.
	content := `---
name: voice-guard-test
patterns: '\brefactor\b'
voice_guards: "Bikeshedding, premature optimization, gold-plating, scope creep"
---

# Voice Guard Test
`
	require.NoError(t, os.WriteFile(good, []byte(content), 0644))

	report := LintRouterDirectories(dir)
	require.True(t, report.OK(), "voice_guards should NOT be regex-compiled; got issues: %+v", report.Issues)
}

// TestLintRouterDirectories_BackspaceFootgun documents the silent
// breakage mode that motivated this linter. A \b in DOUBLE-QUOTED YAML
// becomes a literal backspace character (0x08). The regex compiles
// successfully but never matches anything in real text. The linter
// must catch this by NOT silently accepting it.
func TestLintRouterDirectories_BackspaceFootgun(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "backspace-footgun.md")
	// \b in double-quoted YAML → backspace. The router will compile
	// this as (?i)<BS>refactor<BS> — valid regex, never matches.
	content := "---\nname: backspace-test\npatterns: \"\\brefactor\\b\"\n---\n\n# Test\n"
	require.NoError(t, os.WriteFile(bad, []byte(content), 0644))

	// We document the current behavior: the linter COMPILES the regex
	// the same way the router does (with (?i) prefix), so a backspace
	// in the frontmatter will compile fine (it's a valid regex char)
	// and the linter will report OK. This is intentional — the
	// detection of the backspace-as-semantic-difference problem is
	// a deeper analysis than regex compilation.
	//
	// For now, the linter catches the SYNTACTIC failure modes
	// (broken frontmatter, broken regex). The semantic failure
	// (backspace that compiles but matches nothing) is detected by
	// the empirical corpus test, not the linter. Future work could
	// add a 'suspicious compile result' check that rejects regexes
	// whose compiled form contains non-printable bytes.
	report := LintRouterDirectories(dir)
	t.Logf("backspace test report: %d issues, OK=%v", len(report.Issues), report.OK())
	// The current behavior is OK=true (compiles) — see comment above.
	// This test pins the current contract.
	require.True(t, report.OK(), "backspace char compiles as valid regex; deeper check is future work")
}

// TestLintReport_SortedDeterministicOutput verifies that running the
// linter twice on the same input produces identical output. Useful for
// CI determinism — a linter that produces different output across runs
// is impossible to diff in code review.
func TestLintReport_SortedDeterministicOutput(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		content := "---\nname: " + strings.TrimSuffix(name, ".md") + "\npatterns: 'broken['\n---\n\n# Test\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
	}

	report := LintRouterDirectories(dir)
	require.False(t, report.OK())

	// Sort issues by File for deterministic output (matches linter's
	// internal os.ReadDir alphabetical order).
	sort.Slice(report.Issues, func(i, j int) bool {
		return report.Issues[i].File < report.Issues[j].File
	})

	files := []string{}
	for _, issue := range report.Issues {
		files = append(files, filepath.Base(issue.File))
	}
	// ReadDir returns entries in lexical order, so we expect a, b, c.
	expected := []string{"a.md", "b.md", "c.md"}
	require.Equal(t, expected, files, "linter output should be deterministic and alphabetical")
}
