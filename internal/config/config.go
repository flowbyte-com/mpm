package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the application configuration
type Config struct {
	Workspace   string `json:"workspace,omitempty"`
	MemoryDir   string `json:"memory_dir,omitempty"`
	SessionsDir string `json:"sessions_dir,omitempty"`
}

// ConfigPath returns the path to the MPM config file
func ConfigPath() string {
	workspace := GetWorkspace()
	// Priority 1: projects/mpm/mpm_config.json (new structure)
	projectsPath := filepath.Join(workspace, "projects", "mpm", "mpm_config.json")
	if _, err := os.Stat(projectsPath); err == nil {
		return projectsPath
	}
	// Priority 2: workspace/mpm_config.json (legacy)
	return filepath.Join(workspace, "mpm_config.json")
}

// LoadConfig loads the MPM configuration from file
func LoadConfig() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return defaults if config doesn't exist
			return &Config{}, nil
		}
		return nil, err
	}
	
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// SaveConfig saves the MPM configuration to file
func SaveConfig(config *Config) error {
	path := ConfigPath()
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// MPMDataDir is the subdirectory where all MPM runtime data resides
const MPMDataDir = "mpm"

// GetWorkspace determines the base workspace directory using a cascading priority system:
// 1. CLI Flag (--workspace) via environment variable (os.Getenv)
// 2. Environment Variable (os.Getenv)
// 3. Executable Relative (os.Executable() + symai parent)
// 4. Current Working Directory (os.Getwd)
//
// This enables portable installations - the same binary can work from any directory.
// All MPM runtime data resides within workspace/projects/mpm/ (src/, mode/, persona/)
// Recommended structure:
//   /workspace/         ← User-configurable (workspace root)
//   └── symai/          ← Project folder
//       └── projects/   ← MPM Go binary
//           ├── src/db/         ← SQLite databases (mpm.db - consolidated)
//           │   ├── mpm.db ← Main database
//           │   ├── init.sql     ← Initialization script
//           │   └── schema.sql   ← Database schema
//           ├── mode/           ← Mode configurations (JSON files)
//           ├── persona/        ← Persona configurations (JSON files)
//           ├── toxicphrases.txt ← Cognitive firewall file
//           └── src/            ← Source code (hidden from end users)
func GetWorkspace() string {
	// 1. Check CLI flag via environment variable (set by CLI)
	if workspace := os.Getenv("MPM_WORKSPACE"); workspace != "" {
		return workspace
	}

	// 2. Check environment variable (fallback)
	if envWorkspace := os.Getenv("MPM_WORKSPACE"); envWorkspace != "" {
		return envWorkspace
	}

	// 3. Resolve relative to executable location
	execPath, err := os.Executable()
	if err == nil {
		// Get the directory containing the binary
		execDir := filepath.Dir(execPath)
		
		// Check if we're in a "bin/" directory (standard layout)
		if filepath.Base(execDir) == "bin" {
			// bin/ -> parent directory
			parent := filepath.Dir(execDir)
			// If parent is projects/, return its parent (symai)
			if filepath.Base(parent) == "projects" {
				return filepath.Dir(parent)
			}
			return parent
		}
		
		// If we're already in symai/, return it
		base := filepath.Base(execDir)
		if base == "symai" {
			return execDir
		}
		
		// If we're in projects/, return parent (symai)
		if base == "projects" {
			return execDir
		}
		
		// Fallback: look for parent symai directory
		parent := filepath.Dir(execDir)
		parentBase := filepath.Base(parent)
		if parentBase == "symai" {
			return parent
		}
		
		// If we're in projects/mpm/, return parent (symai)
		if parentBase == "projects" {
			return filepath.Dir(parent)
		}
		
		// Fallback: return current working directory
		cwd, _ := os.Getwd()
		return cwd
	}

	// Fallback: use current working directory
	cwd, _ := os.Getwd()
	return cwd
}

// GetDBPath returns the database directory path (legacy).
// Deprecated: Use GetMemoryPath() for the consolidated database.
func GetDBPath(filename string) string {
	workspace := GetWorkspace()
	dbDir := filepath.Join(workspace, "src", "db")
	os.MkdirAll(dbDir, 0755)
	return filepath.Join(dbDir, filename+".db")
}


// GetMemoryPath returns the memory directory path for MPM's internal database.
// Priority: 1) Config file memory_dir, 2) MPM internal fallback (mpm/src/db)
// NOTE: The memory_dir from config is for the WATCH DAEMON to process OpenClaw files.
//       The MPM database (mpm.db) ALWAYS lives at mpm/src/db/mpm.db.
func GetMemoryPath() string {
	if config, err := LoadConfig(); err == nil && config.MemoryDir != "" {
		return ResolveEnvPath(config.MemoryDir)
	}
	// Legacy fallback: mpm/src/db (only used if no config)
	return filepath.Join(GetMPMDir(), "src", "db")
}

// GetMirrorPath constructs the full path to the mirror JSONL file
// Always located at mpm/src/db/mirror.jsonl (internal storage)
func GetMirrorPath() string {
	mpmDir := GetMPMDir()
	return filepath.Join(mpmDir, "src", "db", "mirror.jsonl")
}

// GetSessionsPath returns the sessions directory path.
// Priority: 1) Config file sessions_dir, 2) Default ~/.openclaw/agents/main/sessions
func GetSessionsPath() string {
	if config, err := LoadConfig(); err == nil && config.SessionsDir != "" {
		return ResolveEnvPath(config.SessionsDir)
	}
	// Default: ~/.openclaw/agents/main/sessions
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".openclaw", "agents", "main", "sessions")
}

// GetMPMDir returns the absolute path to the projects/mpm directory
// All MPM runtime data (mode, persona, toxicphrases, src/db) lives here
func GetMPMDir() string {
	// Use executable-relative resolution to find projects/mpm/
	execPath, err := os.Executable()
	if err == nil {
		execDir := filepath.Dir(execPath)

		// Case: binary is at projects/mpm/bin/mpm
		if filepath.Base(execDir) == "bin" {
			parent := filepath.Dir(execDir) // projects/mpm/
			if filepath.Base(parent) == "mpm" {
				return parent
			}
		}

		// Case: binary is at projects/mpm/ (running from source)
		if filepath.Base(execDir) == "mpm" {
			return execDir
		}

		// Case: binary is in PATH or elsewhere — walk up to find projects/mpm/
		for dir := execDir; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if filepath.Base(dir) == "mpm" && filepath.Base(filepath.Dir(dir)) == "projects" {
				return dir
			}
		}

		// Case: already in projects/mpm/ (development)
		mpmDir := filepath.Join(GetWorkspace(), "projects", "mpm")
		if _, err := os.Stat(mpmDir); err == nil {
			return mpmDir
		}
	}

	// Fallback: resolve from workspace (always use GetWorkspace to avoid /projects/mpm
	// when the walk reaches filesystem root /)
	return filepath.Join(GetWorkspace(), "projects", "mpm")
}

// GetPersonaPath constructs the full path to the persona configurations directory
// All persona files live inside projects/mpm/persona/
func GetPersonaPath() string {
	return filepath.Join(GetMPMDir(), "persona")
}

// GetModePath constructs the full path to the mode configurations directory
// All mode files live inside projects/mpm/mode/
func GetModePath() string {
	return filepath.Join(GetMPMDir(), "mode")
}

// GetToxicPhrasesPath constructs the full path to the toxic phrases file
// Located at projects/mpm/toxicphrases.txt
func GetToxicPhrasesPath() string {
	return filepath.Join(GetMPMDir(), "toxicphrases.txt")
}

// ResolveEnvPath replaces ~ with the home directory and handles relative paths
// Relative paths (no leading /) are resolved relative to the workspace root
func ResolveEnvPath(path string) string {
	// Handle ~ expansion
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	
	// Handle relative paths (no leading /)
	if !filepath.IsAbs(path) {
		// Resolve relative to workspace root
		return filepath.Join(GetWorkspace(), path)
	}
	
	// Absolute path - return as-is
	return path
}

// ResolveEnvPathDebug is like ResolveEnvPath but with debug output
func ResolveEnvPathDebug(path string) string {
	return ResolveEnvPath(path)
}
