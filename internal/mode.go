package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mpm/internal/config"
)

// Mode represents a mode configuration
type Mode struct {
	SymID        string      `json:"sym_id"`
	Title        string      `json:"title"`
	Name         string      `json:"name"`
	Version      string      `json:"version"`
	Description  string      `json:"description"`
	Purpose      string      `json:"purpose"`
	Patterns     string      `json:"patterns"`
	Checklist    string      `json:"checklist"`
	AntiPatterns interface{} `json:"anti_patterns"` // Accept string or array
	Tools        interface{} `json:"tools"`        // Accept string or array
	JSONData     string      `json:"-"`
}

// ModeManager handles mode operations (JSON-only mode for lean binaries)
type ModeManager struct {
	JSONDir     string
	ActiveFile  string
}

// NewModeManager creates a new mode manager (JSON-only mode)
// Updated for new path structure: workspace/mode/ (pristine root)
func NewModeManager(basePath string) *ModeManager {
	if basePath == "" {
		// Use config.GetMPMDir() so modes are found at mpm/mode/
		basePath = config.GetMPMDir()
	}

	return &ModeManager{
		JSONDir:    filepath.Join(basePath, "mode"),
		ActiveFile: filepath.Join(basePath, "active.json"),
	}
}

// InitDB initializes the mode database (no-op for JSON-only mode)
func (mm *ModeManager) InitDB() error {
	os.MkdirAll(mm.JSONDir, 0755)
	return nil
}

// Compile loads all modes from JSON files
func (mm *ModeManager) Compile() (int, error) {
	files, err := os.ReadDir(mm.JSONDir)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		jsonPath := filepath.Join(mm.JSONDir, file.Name())
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			continue
		}

		var m Mode
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}

		// Validate required fields
		if m.SymID == "" {
			continue
		}

		count++
	}

	return count, nil
}

// Get retrieves a mode by name from JSON file
func (mm *ModeManager) Get(name string) (*Mode, error) {
	jsonPath := filepath.Join(mm.JSONDir, name+".json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}

	var m Mode
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	return &m, nil
}

// GetActive returns the active modes from the active file
func (mm *ModeManager) GetActive() ([]string, error) {
	data, err := os.ReadFile(mm.ActiveFile)
	if err != nil {
		return nil, err
	}

	var active struct {
		Modes []string `json:"modes"`
	}
	if err := json.Unmarshal(data, &active); err != nil {
		return nil, err
	}

	return active.Modes, nil
}

// SetActive updates the active modes
// Invalid mode names are silently removed from the list.
func (mm *ModeManager) SetActive(modes []string) error {
	data, err := os.ReadFile(mm.ActiveFile)
	if err != nil {
		return err
	}

	var active struct {
		Persona string   `json:"persona"`
		Modes   []string `json:"modes"`
		Updated string   `json:"updated"`
	}
	if err := json.Unmarshal(data, &active); err != nil {
		return err
	}

	// Filter to only valid modes
	valid := make([]string, 0, len(modes))
	for _, m := range modes {
		if mm.Validate(m) {
			valid = append(valid, m)
		}
	}
	active.Modes = valid
	active.Updated = time.Now().Format(time.RFC3339)

	newData, err := json.MarshalIndent(active, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := mm.ActiveFile + ".tmp"
	if err := os.WriteFile(tmpPath, newData, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, mm.ActiveFile)
}

// GetActiveModes returns the active modes (alias for GetActive)
func (mm *ModeManager) GetActiveModes() ([]string, error) {
	return mm.GetActive()
}

// Validate returns true if a mode JSON file exists for the given name.
func (mm *ModeManager) Validate(name string) bool {
	if name == "" {
		return false
	}
	jsonPath := filepath.Join(mm.JSONDir, name+".json")
	_, err := os.Stat(jsonPath)
	return err == nil
}

// SetActiveModes updates the active modes (alias for SetActive)
func (mm *ModeManager) SetActiveModes(modes []string) error {
	return mm.SetActive(modes)
}

// List returns all modes (read from JSON files directly)
func (mm *ModeManager) List() ([]*Mode, error) {
	files, err := os.ReadDir(mm.JSONDir)
	if err != nil {
		return nil, err
	}

	var modes []*Mode
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		jsonPath := filepath.Join(mm.JSONDir, file.Name())
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			continue
		}

		var m Mode
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}

		if m.SymID != "" {
			modes = append(modes, &m)
		}
	}

	return modes, nil
}

// AddMode adds a mode to the active list
func (mm *ModeManager) AddMode(name string) error {
	modes, _ := mm.GetActive()
	for _, m := range modes {
		if m == name {
			return nil // already exists
		}
	}
	modes = append(modes, name)
	return mm.SetActive(modes)
}

// RemoveMode removes a mode from the active list
func (mm *ModeManager) RemoveMode(name string) error {
	modes, _ := mm.GetActive()
	var newModes []string
	for _, m := range modes {
		if m != name {
			newModes = append(newModes, m)
		}
	}
	return mm.SetActive(newModes)
}

// ClearModes clears all modes
func (mm *ModeManager) ClearModes() error {
	return mm.SetActive([]string{})
}

// RemoveAll removes all mode JSON files (secure delete)
func (mm *ModeManager) RemoveAll() (int, error) {
	files, err := os.ReadDir(mm.JSONDir)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		jsonPath := filepath.Join(mm.JSONDir, file.Name())
		if err := os.Remove(jsonPath); err != nil {
			continue
		}

		count++
	}

	// Clear the active file
	mm.ClearModes()

	return count, nil
}

// CompileAll compiles all modes (no-op for JSON-only mode)
func (mm *ModeManager) CompileAll() error {
	return nil
}
