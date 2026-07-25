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
	"unicode"

	"gopkg.in/yaml.v3"
)

// SkillFrontmatter is the parsed YAML frontmatter of a skill. Fields
// are populated from the frontmatter block; non-frontmatter is the
// body markdown.
type SkillFrontmatter struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	WhenToUse   string      `yaml:"when_to_use"`
	Domain      string      `yaml:"domain"`
	Version     string      `yaml:"version"`
	Constraints []string    `yaml:"constraints"`
	Steps       []SkillStep `yaml:"steps"`
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
	// Like mode and persona parsing, this treats the first delimiter as closing.
	idx := strings.Index(rest, "\n"+fence)
	if idx < 0 {
		return SkillFrontmatter{}, "", fmt.Errorf("frontmatter unterminated")
	}
	yamlBlock := rest[:idx]
	after := rest[idx+len("\n"+fence):]
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
func SkillIDForNameAndVersion(name, version string) (string, error) {
	if err := validateSkillNameAndVersion(name, version); err != nil {
		return "", err
	}
	return fmt.Sprintf("skill:%s-v%s", name, version), nil
}

func validateSkillNameAndVersion(name, version string) error {
	if name == "" {
		return fmt.Errorf("skill name must not be empty")
	}
	if version == "" {
		return fmt.Errorf("skill version must not be empty")
	}
	if strings.ContainsRune(name, ':') || strings.IndexFunc(name, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid skill name %q: must not contain colons or whitespace", name)
	}
	if strings.ContainsRune(version, ':') || strings.IndexFunc(version, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid skill version %q: must not contain colons or whitespace", version)
	}
	return nil
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
	name, version := rest[:idx], rest[idx+2:]
	if err := validateSkillNameAndVersion(name, version); err != nil {
		return "", "", fmt.Errorf("invalid skill id %q: %w", id, err)
	}
	return name, version, nil
}

// Skill is the read-side view of a skill row. Body is the markdown
// after the frontmatter is stripped; Frontmatter is the parsed YAML.
type Skill struct {
	ID          string
	Collection  string
	Tags        []string
	Metadata    map[string]interface{}
	IsGlobal    bool
	IsLatest    bool
	Weight      int
	CreatedAt   string
	Name        string
	Version     string
	WhenToUse   string
	Domain      string
	Constraints []string
	Steps       []SkillStep
	Frontmatter SkillFrontmatter
	Body        string
	ContentHash string // Populated by SaveSkill and projected from metadata.content_hash by ReadSkill.
}

// SkillSummary is the lightweight projection used by list_skills and
// the wake-context <available_skills> block. Avoids pulling the full
// markdown body for inventory queries.
type SkillSummary struct {
	ID        string
	Name      string
	Version   string
	WhenToUse string
	IsGlobal  bool
	Weight    int
}
