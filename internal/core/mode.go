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

// Mode represents a mode configuration from .md file
type Mode struct {
	Name               string  `yaml:"name"`
	Title              string  `yaml:"title"`
	Version            string  `yaml:"version"`
	Status             string  `yaml:"status"`
	Purpose            string  `yaml:"purpose,omitempty"`
	Description        string  `yaml:"description,omitempty"`
	Patterns           string  `yaml:"patterns,omitempty"`
	Checklist          string  `yaml:"checklist,omitempty"`
	AntiPatterns       string  `yaml:"anti_patterns,omitempty"`
	Tools              string  `yaml:"tools,omitempty"`
	RetrievalLimit     int     `yaml:"retrieval_limit"`
	RetrievalThreshold float64 `yaml:"retrieval_threshold"`
	Content            string  `yaml:"-"` // Markdown body after frontmatter
}

// ModeManager handles mode operations from .md files
type ModeManager struct {
	Dir        string
	ActiveFile string
}

// NewModeManager creates a new mode manager
func NewModeManager(basePath string) *ModeManager {
	if basePath == "" {
		basePath = config.GetMPMDir()
	}
	return &ModeManager{
		Dir:        filepath.Join(basePath, "mode"),
		ActiveFile: filepath.Join(basePath, "active.json"),
	}
}

// parseModeFile reads a .md file and parses YAML frontmatter + markdown body
func parseModeFile(path string) (*Mode, error) {
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

	var m Mode
	if frontmatter != "" {
		if err := yaml.Unmarshal([]byte(frontmatter), &m); err != nil {
			return nil, fmt.Errorf("invalid frontmatter in %s: %w", path, err)
		}
	}
	if err := validateMode(&m, path); err != nil {
		return nil, err
	}
	if m.RetrievalLimit <= 0 {
		m.RetrievalLimit = 5
	}
	if m.RetrievalThreshold == 0 {
		m.RetrievalThreshold = -1.0
	}
	m.Content = body

	return &m, nil
}

// validateMode checks that a parsed mode has the minimum required fields.
// Failure here means a malformed mode file will not silently land in the
// routing engine — operators see the error at load time, not when an
// agent tries to use the mode hours later.
//
// Required: name OR title must be present. name is preferred because it's
// how callers reference the mode (mpm mode <name>). The pattern list and
// anti-patterns are optional (some modes exist purely as persona anchors).
func validateMode(m *Mode, path string) error {
	if m.Name == "" && m.Title == "" {
		return fmt.Errorf("mode file %s has no name and no title; one is required", path)
	}
	if m.Name != "" {
		// Name must be a valid identifier: lowercase letters, digits, dashes,
		// underscores. Reject whitespace, slashes, control chars, etc.
		for _, r := range m.Name {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				return fmt.Errorf("mode file %s has invalid name %q: must be [a-z0-9_-]+", path, m.Name)
			}
		}
	}
	if m.Version != "" {
		// Version is loose-validated: must start with a digit. We don't
		// enforce semver strictly; that's a community convention.
		if !((m.Version[0] >= '0' && m.Version[0] <= '9')) {
			return fmt.Errorf("mode file %s has invalid version %q: must start with a digit", path, m.Version)
		}
	}
	return nil
}

// Get retrieves a mode by name from .md file
func (mm *ModeManager) Get(name string) (*Mode, error) {
	mdPath := filepath.Join(mm.Dir, name+".md")
	return parseModeFile(mdPath)
}

// List returns all modes from .md files
func (mm *ModeManager) List() ([]*Mode, error) {
	entries, err := os.ReadDir(mm.Dir)
	if err != nil {
		return nil, err
	}

	var modes []*Mode
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Eligibility gate: must be a Markdown definition file. README.md
		// and other documentation entries are rejected unconditionally here
		// so a README carrying valid frontmatter (e.g. `name: README`)
		// cannot become a selectable mode.
		if !IsDefinitionFile(entry.Name()) {
			continue
		}
		m, err := parseModeFile(filepath.Join(mm.Dir, entry.Name()))
		if err != nil {
			continue
		}
		if m.Name == "" && m.Title == "" {
			continue
		}
		modes = append(modes, m)
	}
	return modes, nil
}

// Validate returns true if a mode .md file exists for the given name
func (mm *ModeManager) Validate(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(mm.Dir, name+".md"))
	return err == nil
}

// GetActive returns the active modes from the canonical active.json.
// Returns nil if the file is missing or unreadable.
//
// v spec 2026-09-11: callers that need to distinguish "explicit clear"
// from "absent" should consult ActiveState.IsModesExplicitClear /
// IsModesAbsent. The plain slice return cannot represent that
// distinction.
func (mm *ModeManager) GetActive() ([]string, error) {
	active, err := LoadActiveJSON()
	if err != nil {
		return nil, err
	}
	return active.ModesSlice(), nil
}

// GetActiveState returns the full ActiveState from the canonical
// active.json. New code should prefer this over GetActive so callers
// can distinguish explicit-clear from absent.
func (mm *ModeManager) GetActiveState() (*ActiveState, error) {
	return LoadActiveJSON()
}

// SetActive updates the active modes via the canonical ActiveState.
// Pointer semantics:
//
//	modes == nil OR len==0  → explicit clear (writes [] into active.json)
//	modes == […]            → explicit selection (writes the slice)
//
// Invalid mode names are silently removed from the input list.
// Also writes the first non-"auto" mode to config/current_mode for
// back-compat with legacy readers. The mirror file is NOT consulted
// by any MPM read path.
func (mm *ModeManager) SetActive(modes []string) error {
	// Filter to only valid modes. Empty input (nil or []) is honoured as
	// explicit clear — no validation needed.
	valid := make([]string, 0, len(modes))
	for _, m := range modes {
		if mm.Validate(m) {
			valid = append(valid, m)
		}
	}

	active, err := LoadActiveJSON()
	if err != nil {
		return err
	}
	// Pointer assignment preserves intent: nil=absent, &[]=explicit clear,
	// &[…]=explicit selection.
	active.Modes = &valid
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := SaveActiveJSON(active); err != nil {
		return err
	}

	// Mirror the first non-"auto" mode to config/current_mode for legacy
	// readers. The "auto" sentinel is a feature flag, not a real mode.
	writeConfigCurrentMode(mm.ActiveFile, valid)
	return nil
}

// writeConfigCurrentMode writes the first real mode name to
// <mpm-dir>/config/current_mode. The "auto" sentinel is skipped — it's a
// feature flag, not a real mode, and shouldn't be injected as the active mode.
func writeConfigCurrentMode(activeJSONPath string, modes []string) {
	mpmDir := filepath.Dir(activeJSONPath)
	for _, m := range modes {
		if m != "" && m != "auto" {
			path := filepath.Join(mpmDir, "config", "current_mode")
			_ = os.MkdirAll(filepath.Dir(path), 0700)
			_ = os.WriteFile(path, []byte(m), 0644)
			return
		}
	}
	// No real mode selected — clear the file.
	path := filepath.Join(mpmDir, "config", "current_mode")
	_ = os.Remove(path)
}

// GetActiveModes returns the active modes (alias for GetActive)
func (mm *ModeManager) GetActiveModes() ([]string, error) {
	return mm.GetActive()
}

// SetActiveModes updates the active modes (alias for SetActive)
func (mm *ModeManager) SetActiveModes(modes []string) error {
	return mm.SetActive(modes)
}

// AddMode writes a new mode .md file (stub — mode files are managed externally)
func (mm *ModeManager) AddMode(name string) error {
	return fmt.Errorf("mpm does not create mode files; manage them directly in %s", mm.Dir)
}

// RemoveMode removes a mode .md file
func (mm *ModeManager) RemoveMode(name string) error {
	mdPath := filepath.Join(mm.Dir, name+".md")
	return os.Remove(mdPath)
}

// ClearModes sets active modes to empty
func (mm *ModeManager) ClearModes() error {
	return mm.SetActive(nil)
}

// RemoveAll removes all mode files (use with caution)
func (mm *ModeManager) RemoveAll() (int, error) {
	entries, err := os.ReadDir(mm.Dir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if err := os.Remove(filepath.Join(mm.Dir, entry.Name())); err == nil {
			count++
		}
	}
	return count, nil
}
