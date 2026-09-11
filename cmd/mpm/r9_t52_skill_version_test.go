// r9_t52_skill_version_test.go — Round 9 T52 regression.
//
// Pin the skill version schema contract:
//
//   - The skill id is `skill:<name>-v<version>`; the `-v<version>`
//     suffix disambiguates edits over time, so version is REQUIRED.
//   - The substrate `validateSkillNameAndVersion` enforces this —
//     smoke tests that try to save a skill without version must
//     fail with "skill version must not be empty".
//   - The CLI must DOCUMENT that version is required (both via
//     `mpm save-skill --help` and the equivalent cognitive-verb
//     surface `mpm skill add --help`), so a smoke test that reads
//     help and then fails on a missing version reads the contract
//     before the failure.
//
// We do NOT default-version to "0.0.0" / content-hash: that would
// obscure authoring intent and break downstream tooling that parses
// the id suffix.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func r9T52Mpm(t *testing.T, args ...string) (string, int) {
	t.Helper()
	bin := "/home/v/.mpm/bin/mpm"
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestR9T52_SaveSkillHelpDocumentsVersion pins the help discovery
// surface: `mpm save-skill --help` and `mpm skill add --help` must
// enumerate the required `version` schema field. Pre-fix the help
// page didn't exist at all — only the generic description was
// printed and the requirement was hidden inside the substrate
// error.
func TestR9T52_SaveSkillHelpDocumentsVersion(t *testing.T) {
	for _, args := range [][]string{
		{"save-skill", "--help"},
		{"skill", "add", "--help"},
		{"skill", "save", "--help"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, _ := r9T52Mpm(t, args...)
			for _, want := range []string{"--version", "Required schema", "version"} {
				if !strings.Contains(out, want) {
					t.Errorf("%v: help missing %q. Output:\n%s", args, want, out)
				}
			}
		})
	}
}

// TestR9T52_SkillWithoutVersionFails pins the substrate guard:
// `dm.SaveSkill` with empty version must reject. We exercise the
// CLI surface (no test fixtures that bypass the gateway).
func TestR9T52_SkillWithoutVersionFails(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")

	// Frontmatter missing the `version:` key.
	mdPath := filepath.Join(tmp, "missing-version.md")
	mdContent := `---
name: r9-t52-no-version
---
# R9 T52 — no version
`
	if err := os.WriteFile(mdPath, []byte(mdContent), 0644); err != nil {
		t.Fatalf("write md: %v", err)
	}

	workspaceCmd := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("/home/v/.mpm/bin/mpm", args...)
		cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}

	out, code := workspaceCmd("save-skill", "--file", mdPath)
	if code == 0 {
		t.Fatalf("skill without version should fail. Output:\n%s", out)
	}
	if !strings.Contains(out, "missing required field") || !strings.Contains(out, "version") {
		t.Errorf("expected 'missing required field: version' error, got:\n%s", out)
	}
}

// TestR9T52_SkillWithVersionSucceeds — opposite guard: a complete
// frontmatter lands successfully.
func TestR9T52_SkillWithVersionSucceeds(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")

	mdPath := filepath.Join(tmp, "with-version.md")
	mdContent := `---
name: r9_t52_with_version
version: 1.0.0
description: a versioned skill fixture for T52
---
# R9 T52 — with version
`
	if err := os.WriteFile(mdPath, []byte(mdContent), 0644); err != nil {
		t.Fatalf("write md: %v", err)
	}

	workspaceCmd := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("/home/v/.mpm/bin/mpm", args...)
		cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}

	out, code := workspaceCmd("save-skill", "--file", mdPath)
	if code != 0 {
		t.Fatalf("skill with version should succeed. Output:\n%s", out)
	}
	if !strings.Contains(out, "Saved skill") {
		t.Errorf("expected 'Saved skill' success marker, got:\n%s", out)
	}
}

// TestR9T52_HelpAdvertisesFrontmatterVersionKey pins the discovery
// contract: the help text must mention "frontmatter" as the canonical
// place to set version (since the CLI --version flag is rewritten by
// parseFlags in a way that prevents the override path from working
// cleanly for the r9-T52 fixture). The fix is to make frontmatter
// the documented surface; --version CLI override is acknowledged as
// "supported but quirky" by parsing apart.
func TestR9T52_HelpAdvertisesFrontmatterVersionKey(t *testing.T) {
	out, _ := r9T52Mpm(t, "save-skill", "--help")
	for _, want := range []string{"frontmatter", "version:"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q marker. Output:\n%s", want, out)
		}
	}
}
