package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/config"

	"gopkg.in/yaml.v3"
)

// Persona represents a persona configuration from .md file
type Persona struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Version     string `yaml:"version"`
	Status      string `yaml:"status"`
	Description string `yaml:"description,omitempty"`
	Creature    string `yaml:"creature,omitempty"`
	Vibe        string `yaml:"vibe,omitempty"`
	Voice       string `yaml:"voice,omitempty"`
	Emoji       string `yaml:"emoji,omitempty"`
	Content     string `yaml:"-"` // Markdown body after frontmatter
}

// PersonaManager handles persona operations from .md files
type PersonaManager struct {
	Dir        string
	ActiveFile string
}

// NewPersonaManager creates a new persona manager
func NewPersonaManager(basePath string) *PersonaManager {
	if basePath == "" {
		basePath = config.GetMPMDir()
	}
	return &PersonaManager{
		Dir:        filepath.Join(basePath, "persona"),
		ActiveFile: filepath.Join(basePath, "active.json"),
	}
}

// parsePersonaFile reads a .md file and parses YAML frontmatter + markdown body
func parsePersonaFile(path string) (*Persona, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	var frontmatter string
	var body string

	if strings.HasPrefix(content, "---") {
		parts := strings.SplitN(content[3:], "---", 2)
		if len(parts) == 2 {
			frontmatter = strings.TrimSpace(parts[0])
			body = strings.TrimSpace(parts[1])
		}
	}

	var p Persona
	if frontmatter != "" {
		if err := yaml.Unmarshal([]byte(frontmatter), &p); err != nil {
			return nil, fmt.Errorf("invalid frontmatter in %s: %w", path, err)
		}
	}
	if err := validatePersona(&p, path); err != nil {
		return nil, err
	}
	p.Content = body

	return &p, nil
}

// validatePersona checks that a parsed persona has the minimum required
// fields. Mirrors validateMode: a malformed persona must fail at load
// time, not when an agent tries to use it.
//
// Required: name OR title. name is the canonical identifier (used in
// active.json and mpm persona <name>). Names share the same character
// class as modes.
func validatePersona(p *Persona, path string) error {
	if p.Name == "" && p.Title == "" {
		return fmt.Errorf("persona file %s has no name and no title; one is required", path)
	}
	if p.Name != "" {
		for _, r := range p.Name {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				return fmt.Errorf("persona file %s has invalid name %q: must be [a-z0-9_-]+", path, p.Name)
			}
		}
	}
	if p.Version != "" && !((p.Version[0] >= '0' && p.Version[0] <= '9')) {
		return fmt.Errorf("persona file %s has invalid version %q: must start with a digit", path, p.Version)
	}
	return nil
}

// Get retrieves a persona by name from .md file
func (pm *PersonaManager) Get(name string) (*Persona, error) {
	mdPath := filepath.Join(pm.Dir, name+".md")
	return parsePersonaFile(mdPath)
}

// List returns all personas from .md files
func (pm *PersonaManager) List() ([]*Persona, error) {
	entries, err := os.ReadDir(pm.Dir)
	if err != nil {
		return nil, err
	}

	var personas []*Persona
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Eligibility gate: must be a Markdown definition file. README.md
		// and other documentation entries are rejected unconditionally here
		// so a README carrying valid frontmatter (e.g. `name: README`)
		// cannot become a selectable persona.
		if !IsDefinitionFile(entry.Name()) {
			continue
		}
		p, err := parsePersonaFile(filepath.Join(pm.Dir, entry.Name()))
		if err != nil {
			continue
		}
		if p.Name == "" && p.Title == "" {
			continue
		}
		personas = append(personas, p)
	}
	return personas, nil
}

// Validate returns true if a persona .md file exists for the given name
func (pm *PersonaManager) Validate(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(pm.Dir, name+".md"))
	return err == nil
}

// GetActive returns the active persona name from the active file.
// Returns "" if the file is missing or unreadable.
//
// v spec 2026-09-11: callers that need to distinguish "explicit clear"
// from "absent" should consult ActiveState.IsPersonaExplicitClear /
// IsPersonaAbsent. The plain string return cannot represent that
// distinction.
func (pm *PersonaManager) GetActive() (string, error) {
	active, err := LoadActiveJSON()
	if err != nil {
		return "", err
	}
	return active.PersonaString(), nil
}

// GetActiveState returns the full ActiveState from the canonical
// active.json. New code should prefer this over GetActive so callers
// can distinguish explicit-clear from absent. The legacy mirror file
// (config/current_persona) is NOT consulted.
func (pm *PersonaManager) GetActiveState() (*ActiveState, error) {
	return LoadActiveJSON()
}

// SetActive updates the active persona via the canonical ActiveState.
// Pointer semantics:
//
//	personaName == ""  → explicit clear (writes "" into active.json)
//	personaName == "x" → explicit selection (writes "x")
//	personaName invalid → does NOT touch active.json (returns error)
//
// The pointer-aware encoding preserves the user's intent across reads.
// Also writes config/current_persona (legacy mirror) for back-compat
// with third-party readers; that file is NOT consulted by any MPM
// read path.
func (pm *PersonaManager) SetActive(personaName string) error {
	if personaName != "" && !pm.Validate(personaName) {
		return fmt.Errorf("persona not found: %s", personaName)
	}

	active, err := LoadActiveJSON()
	if err != nil {
		return err
	}
	// Pointer assignment preserves intent: nil=absent, &""=explicit clear,
	// &"name"=explicit selection.
	p := personaName
	active.Persona = &p
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := SaveActiveJSON(active); err != nil {
		return err
	}

	// Mirror to config/current_persona for legacy readers. Skip the "auto"
	// sentinel — it's a feature flag, not a real persona.
	if personaName != "" && personaName != "auto" {
		path := filepath.Join(filepath.Dir(pm.ActiveFile), "config", "current_persona")
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		_ = os.WriteFile(path, []byte(personaName), 0644)
	} else {
		path := filepath.Join(filepath.Dir(pm.ActiveFile), "config", "current_persona")
		_ = os.Remove(path)
	}
	return nil
}

// RemoveAll removes all persona .md files (use with caution)
//
// Mirrors ModeManager.RemoveAll: under the canonical layout the MPM
// checkout is the workspace root, so `persona/` is git-tracked source
// at the runtime root and must not be bulk-deleted. A non-Git
// workspace keeps the original behaviour.
func (pm *PersonaManager) RemoveAll() (int, error) {
	if isInsideGitWorktree(pm.Dir) {
		return 0, fmt.Errorf(
			"refusing to shred personas from a Git worktree (%s): these files are repository-owned source, not runtime state; remove them with `git rm` if that is really intended",
			pm.Dir)
	}
	entries, err := os.ReadDir(pm.Dir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if err := os.Remove(filepath.Join(pm.Dir, entry.Name())); err == nil {
			count++
		}
	}
	return count, nil
}
