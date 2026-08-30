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
//
// Steps is parsed into a generic shape first so that the validation
// layer can distinguish "step[N] was a YAML mapping with no call
// field" from "step[N] was a YAML mapping with an empty call field"
// from "step[N] was a non-mapping scalar". yaml.v3 silently coerces
// the first two cases to SkillStep{Call:"", ArgsFrom:""}, which the
// auditor (alpha-4.1.2) flagged as silent corruption. The dedicated
// validation step in validateSkillFrontmatterAndScan (skill_db.go)
// closes that hole.
type SkillFrontmatter struct {
	Name        string             `yaml:"name"`
	Description string             `yaml:"description"`
	WhenToUse   string             `yaml:"when_to_use"`
	Domain      string             `yaml:"domain"`
	Version     string             `yaml:"version"`
	Constraints []string           `yaml:"constraints"`
	Steps       []SkillStep        `yaml:"steps"`
	StepNodes   []SkillStepNodeRaw `yaml:"-"` // populated by ParseSkillFrontmatter for validation; never persisted
}

// SkillStep is one ordered step in a skill's procedure.
type SkillStep struct {
	Call     string `yaml:"call"`
	ArgsFrom string `yaml:"args_from,omitempty"`
}

// SkillStepNodeRaw preserves the raw YAML shape of each step entry
// so the validator can detect silently-coerced shapes (empty mappings,
// scalar entries that yaml.v3 accepts but that have no `call` field).
//
// Fields:
//
//   - Kind       — yaml.ScalarNode, yaml.MappingNode, etc.
//   - CallRaw    — the raw call value as yaml.v3 saw it. Empty string
//     with Kind==MappingNode means the mapping had no `call` key.
//     Empty string with Kind==ScalarNode means the entry was a string
//     or other scalar that yaml.v3 silently coerced.
//   - CallSet    — true iff the YAML mapping explicitly contained a
//     `call` key (even if the value was empty).
//   - ArgSet     — true iff the YAML mapping explicitly contained an
//     `args_from` key.
//
// SkillStep.Call is computed from CallRaw and CallSet; CallSet=false
// forces Call="" even if CallRaw happens to be non-empty.
type SkillStepNodeRaw struct {
	Kind    yaml.Kind
	CallRaw string
	CallSet bool
	ArgSet  bool
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
	// Capture raw step shape for validation. See SkillStepNodeRaw
	// for why we need the raw view: yaml.v3 silently coerces empty
	// mappings to SkillStep{Call:"", ArgsFrom:""}, which the auditor
	// (alpha-4.1.2) flagged as silent corruption. We replay the YAML
	// into a generic node tree here so the validator can see whether
	// each step was a mapping with a call key, a mapping without one,
	// or a scalar that yaml.v3 coerced away.
	if err := captureRawStepNodes([]byte(yamlBlock), &fm); err != nil {
		// Non-fatal — fall through to the typed parse. The validator
		// will catch the obvious shapes; capture errors are diagnostic.
		_ = err
	}
	if fm.Name == "" {
		return SkillFrontmatter{}, "", fmt.Errorf("frontmatter missing required field: name")
	}
	if fm.Version == "" {
		return SkillFrontmatter{}, "", fmt.Errorf("frontmatter missing required field: version")
	}
	return fm, body, nil
}

// captureRawStepNodes replays the YAML block into a node tree and
// extracts the raw shape of every entry in the steps: list. Populates
// fm.StepNodes in document order so the validator can distinguish
// "mapping without a call key" from "mapping with empty call" from
// "scalar entry".
func captureRawStepNodes(yamlBlock []byte, fm *SkillFrontmatter) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(yamlBlock, &doc); err != nil {
		return err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	// Walk the mapping at top level looking for `steps`.
	var stepsNode *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		val := root.Content[i+1]
		if key.Value == "steps" && val.Kind == yaml.SequenceNode {
			stepsNode = val
			break
		}
	}
	if stepsNode == nil {
		fm.StepNodes = nil
		return nil
	}
	fm.StepNodes = make([]SkillStepNodeRaw, len(stepsNode.Content))
	for i, entry := range stepsNode.Content {
		raw := SkillStepNodeRaw{Kind: entry.Kind}
		switch entry.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(entry.Content); j += 2 {
				k := entry.Content[j]
				v := entry.Content[j+1]
				switch k.Value {
				case "call":
					raw.CallSet = true
					if v.Kind == yaml.ScalarNode {
						raw.CallRaw = v.Value
					}
				case "args_from":
					raw.ArgSet = true
				}
			}
		case yaml.ScalarNode:
			raw.CallRaw = entry.Value
		}
		fm.StepNodes[i] = raw
	}
	return nil
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
	// Reject `-v` in skill names — it would collide with the
	// `skill:<name>-v<version>` id format and ParseNameAndVersionFromID's
	// strings.Index(rest, "-v") lookup would mis-split. Force authors to
	// pick names that round-trip through the id parser unambiguously.
	if strings.Contains(name, "-v") {
		return fmt.Errorf("invalid skill name %q: must not contain %q (collides with id format)", name, "-v")
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
