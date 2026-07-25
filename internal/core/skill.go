// skill.go — Skill type, frontmatter parser, and id helpers.
//
// A skill is a memory row with collection='skills'. The frontmatter
// lives inside the row's content column (single source of truth; FTS5
// searches the whole document). These helpers parse the frontmatter
// on read and construct skill IDs from (name, version) pairs.

package internal

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillFrontmatter is the parsed YAML frontmatter of a skill. Fields
// are populated from the frontmatter block; non-frontmatter is the
// body markdown.
type SkillFrontmatter struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	WhenToUse   string            `yaml:"when_to_use"`
	Domain      string            `yaml:"domain"`
	Version     string            `yaml:"version"`
	Constraints []string          `yaml:"constraints"`
	Steps       []SkillStep       `yaml:"steps"`
	Extra       map[string]string `yaml:",inline"`
}

// SkillStep is one ordered step in a skill's procedure.
type SkillStep struct {
	Call     string `yaml:"call"`
	ArgsFrom string `yaml:"args_from,omitempty"`
}

// ParseSkillFrontmatter extracts the frontmatter and body from a skill
// content string. Returns an error if the frontmatter is missing,
// unterminated, or lacks required fields (name, version).
func ParseSkillFrontmatter(content string) (SkillFrontmatter, string, error) {
	const fence = "---"
	if !strings.HasPrefix(content, fence+"\n") {
		return SkillFrontmatter{}, "", fmt.Errorf("frontmatter missing: must start with %q", fence)
	}
	rest := strings.TrimPrefix(content, fence+"\n")
	idx := strings.Index(rest, "\n"+fence)
	if idx < 0 {
		return SkillFrontmatter{}, "", fmt.Errorf("frontmatter unterminated")
	}
	yamlBlock := rest[:idx]
	after := rest[idx+len(fence):]
	body := strings.TrimPrefix(after, "\n")

	var fm SkillFrontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return SkillFrontmatter{}, "", fmt.Errorf("parse frontmatter: %w", err)
	}
	if fm.Name == "" {
		return SkillFrontmatter{}, "", fmt.Errorf("frontmatter missing required field: name")
	}
	if fm.Version == "" {
		return SkillFrontmatter{}, "", fmt.Errorf("frontmatter missing required field: version")
	}
	return fm, body, nil
}

// SkillIDForNameAndVersion builds the canonical id for a skill row.
// Format: skill:<name>-v<semver>
func SkillIDForNameAndVersion(name, version string) string {
	return fmt.Sprintf("skill:%s-v%s", name, version)
}

// ParseNameAndVersionFromID extracts the (name, version) pair from a
// skill id. Returns an error if the id doesn't match the expected
// format.
func ParseNameAndVersionFromID(id string) (string, string, error) {
	const prefix = "skill:"
	if !strings.HasPrefix(id, prefix) {
		return "", "", fmt.Errorf("id %q does not start with %q", id, prefix)
	}
	rest := id[len(prefix):]
	idx := strings.Index(rest, "-v")
	if idx < 0 {
		return "", "", fmt.Errorf("id %q missing -v<version> suffix", id)
	}
	return rest[:idx], rest[idx+2:], nil
}
