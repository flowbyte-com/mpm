package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mpm/internal/config"
)

// Persona represents a persona configuration
type Persona struct {
	SymID       string                 `json:"sym_id"`
	Title       string                 `json:"title"`
	Name        string                 `json:"name"`
	Version     string                 `json:"version"`
	Description string                 `json:"description"`
	Style       interface{}            `json:"style,omitempty"` // Accept string or object
	Voice       interface{}            `json:"voice,omitempty"` // Accept string or object
	Knowledge   map[string]interface{} `json:"knowledge,omitempty"`
	JSONData    string                 `json:"-"`
}

// PersonaManager handles persona operations (JSON-only mode for lean binaries)
type PersonaManager struct {
	JSONDir     string
	ActiveFile  string
}

// NewPersonaManager creates a new persona manager (JSON-only mode)
// Updated for new path structure: workspace/persona/ (pristine root)
func NewPersonaManager(basePath string) *PersonaManager {
	if basePath == "" {
		// Use config.GetWorkspace() for portable installations
		basePath = config.GetWorkspace()
	}

	return &PersonaManager{
		JSONDir:    filepath.Join(basePath, "persona"),
		ActiveFile: filepath.Join(basePath, "active.json"),
	}
}

// InitDB initializes the persona database (no-op for JSON-only mode)
func (pm *PersonaManager) InitDB() error {
	os.MkdirAll(pm.JSONDir, 0755)
	return nil
}

// Compile loads all personas from JSON files
func (pm *PersonaManager) Compile() (int, error) {
	files, err := os.ReadDir(pm.JSONDir)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		jsonPath := filepath.Join(pm.JSONDir, file.Name())
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			continue
		}

		var p Persona
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}

		// Validate required fields
		if p.SymID == "" {
			continue
		}

		count++
	}

	return count, nil
}

// Get retrieves a persona by name from JSON file
func (pm *PersonaManager) Get(symID string) (*Persona, error) {
	jsonPath := filepath.Join(pm.JSONDir, symID+".json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}

	var p Persona
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}

	// Validate required fields
	if p.SymID != symID && p.Name != symID {
		return nil, fmt.Errorf("persona ID mismatch")
	}

	return &p, nil
}

// GetActive returns the active persona name from the active file
func (pm *PersonaManager) GetActive() (string, error) {
	data, err := os.ReadFile(pm.ActiveFile)
	if err != nil {
		return "", err
	}

	var active struct {
		Persona string `json:"persona"`
	}
	if err := json.Unmarshal(data, &active); err != nil {
		return "", err
	}

	return active.Persona, nil
}

// SetActive updates the active persona
func (pm *PersonaManager) SetActive(personaName string) error {
	data, err := os.ReadFile(pm.ActiveFile)
	if err != nil {
		return err
	}

	var active struct {
		Persona string `json:"persona"`
		Modes   []string `json:"modes"`
		Updated string `json:"updated"`
	}
	if err := json.Unmarshal(data, &active); err != nil {
		return err
	}

	active.Persona = personaName
	active.Updated = time.Now().Format(time.RFC3339)

	newData, err := json.MarshalIndent(active, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(pm.ActiveFile, newData, 0644)
}

// GetActivePersona returns the active persona name (alias for GetActive)
func (pm *PersonaManager) GetActivePersona() (string, error) {
	return pm.GetActive()
}

// SetActivePersona updates the active persona (alias for SetActive)
func (pm *PersonaManager) SetActivePersona(personaName string) error {
	return pm.SetActive(personaName)
}

// List returns all personas (read from JSON files directly)
func (pm *PersonaManager) List() ([]*Persona, error) {
	files, err := os.ReadDir(pm.JSONDir)
	if err != nil {
		return nil, err
	}

	var personas []*Persona
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		jsonPath := filepath.Join(pm.JSONDir, file.Name())
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			continue
		}

		var p Persona
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}

		if p.SymID != "" {
			personas = append(personas, &p)
		}
	}

	return personas, nil
}

// CompileAll compiles all personas (no-op for JSON-only mode)
func (pm *PersonaManager) CompileAll() error {
	return nil
}

// RemoveAll removes all persona JSON files (secure delete)
func (pm *PersonaManager) RemoveAll() (int, error) {
	files, err := os.ReadDir(pm.JSONDir)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		jsonPath := filepath.Join(pm.JSONDir, file.Name())
		if err := os.Remove(jsonPath); err != nil {
			continue
		}

		count++
	}

	// Clear the active file
	pm.SetActive("")

	return count, nil
}
