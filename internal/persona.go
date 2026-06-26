package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mpm/internal/config"

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
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
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

// GetActive returns the active persona name from the active file
func (pm *PersonaManager) GetActive() (string, error) {
	data, err := os.ReadFile(pm.ActiveFile)
	if err != nil {
		return "", err
	}

	type activeState struct {
		Persona string   `json:"persona"`
		Modes   []string `json:"modes"`
		Updated string   `json:"updated"`
	}
	var active activeState
	if err := json.Unmarshal(data, &active); err != nil {
		active = activeState{}
	}

	return active.Persona, nil
}

// SetActive updates the active persona
// If the name is not a valid persona, it is set to empty string (system falls back to default).
// Also writes config/current_persona so detectActiveContext() (cmd/mpm/handlers.go)
// injects the same value into memory metadata. The "auto" sentinel is skipped —
// it's a feature flag, not a real persona.
func (pm *PersonaManager) SetActive(personaName string) error {
	data, err := os.ReadFile(pm.ActiveFile)
	if err != nil {
		return err
	}

	type activeState struct {
		Persona string   `json:"persona"`
		Modes   []string `json:"modes"`
		Updated string   `json:"updated"`
	}
	var active activeState
	if err := json.Unmarshal(data, &active); err != nil {
		active = activeState{}
	}

	// Only set if valid, otherwise leave empty (triggers default fallback)
	if personaName == "" || pm.Validate(personaName) {
		active.Persona = personaName
	} else {
		active.Persona = ""
	}
	active.Updated = time.Now().Format(time.RFC3339)

	newData, err := json.MarshalIndent(active, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(pm.ActiveFile, newData, 0644); err != nil {
		return err
	}

	// Mirror to config/current_persona so memory metadata injection sees the
	// same value the user just selected. Skip the "auto" sentinel.
	if active.Persona != "" && active.Persona != "auto" {
		path := filepath.Join(filepath.Dir(pm.ActiveFile), "config", "current_persona")
		_ = os.MkdirAll(filepath.Dir(path), 0755)
		_ = os.WriteFile(path, []byte(active.Persona), 0644)
	} else {
		path := filepath.Join(filepath.Dir(pm.ActiveFile), "config", "current_persona")
		_ = os.Remove(path)
	}
	return nil
}

// RemoveAll removes all persona .md files (use with caution)
func (pm *PersonaManager) RemoveAll() (int, error) {
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
