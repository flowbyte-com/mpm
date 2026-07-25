package internal

import (
	"strings"
	"testing"
)

func TestParseSkillFrontmatter_Valid(t *testing.T) {
	body := `---
name: agentshell
description: Use when working with the AgentShell theme.
when_to_use: agentshell, AgentShell, MCP tools
domain: wordpress
version: 2.0.0
constraints:
  - never edit header.php
  - always call get_config
steps:
  - call: agentshell_get_config
  - call: agentshell_set_css_var
---
# AgentShell Skill

Full markdown body here.`

	fm, body, err := ParseSkillFrontmatter(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fm.Name != "agentshell" {
		t.Errorf("name = %q, want agentshell", fm.Name)
	}
	if fm.Version != "2.0.0" {
		t.Errorf("version = %q, want 2.0.0", fm.Version)
	}
	if fm.Domain != "wordpress" {
		t.Errorf("domain = %q, want wordpress", fm.Domain)
	}
	if len(fm.Constraints) != 2 {
		t.Errorf("constraints len = %d, want 2", len(fm.Constraints))
	}
	if len(fm.Steps) != 2 {
		t.Errorf("steps len = %d, want 2", len(fm.Steps))
	}
	if !strings.Contains(body, "# AgentShell Skill") {
		t.Errorf("body should contain heading, got %q", body)
	}
}

func TestParseSkillFrontmatter_NoFrontmatter(t *testing.T) {
	body := "# Just markdown"
	_, _, err := ParseSkillFrontmatter(body)
	if err == nil {
		t.Fatal("expected error when frontmatter missing")
	}
}

func TestParseSkillFrontmatter_Malformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"unterminated", "---\nname: x\nno closing fence"},
		{"missing name", "---\ndescription: x\n---\nbody"},
		{"missing version", "---\nname: x\n---\nbody"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseSkillFrontmatter(tc.in)
			if err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestSkillIDForNameAndVersion(t *testing.T) {
	id, err := SkillIDForNameAndVersion("agentshell", "2.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "skill:agentshell-v2.0.0" {
		t.Errorf("got %q, want skill:agentshell-v2.0.0", id)
	}
}

func TestSkillIDForNameAndVersion_RejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name    string
		version string
	}{
		{"", "1.0.0"},
		{"agentshell", ""},
		{"agent:shell", "1.0.0"},
		{"agent shell", "1.0.0"},
		{"agentshell", "1:0.0"},
		{"agentshell", "1.0.0\nattack"},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.version, func(t *testing.T) {
			if _, err := SkillIDForNameAndVersion(tc.name, tc.version); err == nil {
				t.Errorf("expected error for name %q and version %q", tc.name, tc.version)
			}
		})
	}
}

func TestParseNameAndVersionFromID(t *testing.T) {
	name, ver, err := ParseNameAndVersionFromID("skill:agentshell-v2.0.0")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if name != "agentshell" || ver != "2.0.0" {
		t.Errorf("got (%q, %q), want (agentshell, 2.0.0)", name, ver)
	}
}

func TestParseNameAndVersionFromID_RejectsInvalidInputs(t *testing.T) {
	ids := []string{
		"skill:-v1.0.0",
		"skill:agentshell-v",
		"skill:agent shell-v1.0.0",
		"skill:agentshell-v1:0.0",
	}

	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if _, _, err := ParseNameAndVersionFromID(id); err == nil {
				t.Errorf("expected error for id %q", id)
			}
		})
	}
}
