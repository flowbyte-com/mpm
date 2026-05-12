package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the application configuration
type Config struct {
	Workspace      string `json:"workspace,omitempty"`
	MemoryDir      string   `json:"memory_dir,omitempty"`      // Legacy singular — use MemoryDirs
	MemoryDirs     []string `json:"memory_dirs,omitempty"`
	SessionsDir    string   `json:"sessions_dir,omitempty"`    // Legacy singular — use SessionsDirs
	SessionsDirs   []string `json:"sessions_dirs,omitempty"`
	ExternalDbs    []ExternalDB `json:"external_dbs,omitempty"`
	OpenClawDBPath string `json:"openclaw_db_path,omitempty"` // Source DB for ingest (default: ~/.openclaw/memory/main.sqlite)
	WebToken       string `json:"web_token,omitempty"`         // Optional bearer token for web UI auth
	Synth          *SynthConfig `json:"synth,omitempty"`
}

// ExternalDB describes an external SQLite database to poll for memories.
type ExternalDB struct {
	Path            string `json:"path"`
	Label           string `json:"label"`
	IntervalSeconds int    `json:"interval_seconds"` // default 30
}

// GetMemoryDirs returns configured memory directories.
// Uses MemoryDirs (array) if set, falls back to [MemoryDir] (singular) for backward compat.
func (c *Config) GetMemoryDirs() []string {
	if len(c.MemoryDirs) > 0 {
		return c.MemoryDirs
	}
	if c.MemoryDir != "" {
		return []string{c.MemoryDir}
	}
	return nil
}

// GetSessionsDirs returns configured session directories.
// Uses SessionsDirs (array) if set, falls back to [SessionsDir] (singular) for backward compat.
func (c *Config) GetSessionsDirs() []string {
	if len(c.SessionsDirs) > 0 {
		return c.SessionsDirs
	}
	if c.SessionsDir != "" {
		return []string{c.SessionsDir}
	}
	return nil
}

// GetExternalDbs returns configured external DBs.
func (c *Config) GetExternalDbs() []ExternalDB {
	return c.ExternalDbs
}

// SynthConfig holds LLM settings for the synth command.
// All fields are optional — missing fields fall back to env vars or defaults.
type SynthConfig struct {
	Model        string `json:"model"`          // e.g. "MiniMax-M2.7", "llama3", "gpt-4o"
	APIKey      string `json:"api_key"`        // defaults to MINIMAX_API_KEY env var
	BaseURL     string `json:"base_url"`       // e.g. "http://localhost:11434/v1"
	MaxTokens   int    `json:"max_tokens"`    // default 1024
	TimeoutSecs int    `json:"timeout_seconds"` // default 300
}

// ConfigPath returns the path to the MPM config file
func ConfigPath() string {
	workspace := GetWorkspace()
	// Priority 1: workspace/mpm_config.json
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
// All MPM runtime data resides within workspace/flowbyte/mpm/ (src/, mode/, persona/)
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
	// 1. Check environment variable (CLI flag or env var)
	if workspace := os.Getenv("MPM_WORKSPACE"); workspace != "" {
		return workspace
	}

	// 2. Resolve relative to executable location
	execPath, err := os.Executable()
	if err == nil {
		// Get the directory containing the binary
		execDir := filepath.Dir(execPath)

		// Check if we're in a "bin/" directory (standard layout)
		if filepath.Base(execDir) == "bin" {
			parent := filepath.Dir(execDir)   // e.g. .../flowbyte/mpm or .../projects
			parentBase := filepath.Base(parent)
			// If parent is projects/ or workspace/, workspace is the grandparent
			if parentBase == "projects" || parentBase == "workspace" {
				return filepath.Dir(parent)
			}
			// Parent is the project root (e.g. flowbyte/mpm or symai).
			// Workspace IS the project root, not the grandparent.
			if parentBase == "mpm" || parentBase == "symai" || parentBase == "desp" {
				return parent
			}
			// Generic project dir — workspace is the grandparent
			return filepath.Dir(parent)
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

		// If we're in projects/, return parent
		if parentBase == "projects" {
			return filepath.Dir(parent)
		}

		// Fallback: return current working directory
		cwd, _ := os.Getwd()

		// For system-installed binaries (e.g. /usr/local/bin/mpm), the walk-up
		// from /usr/local/bin finds nothing useful. Check if the OpenClaw
		// workspace pattern exists: ~/.openclaw/workspace/projects/mpm
		if home := os.Getenv("HOME"); home != "" {
			openclawWS := filepath.Join(home, ".openclaw", "workspace", "projects", "mpm")
			dbPath := filepath.Join(openclawWS, "src", "db", "mpm.db")
			if _, err := os.Stat(dbPath); err == nil {
				return openclawWS
			}
		}

		return cwd
	}

	// Fallback: use current working directory
	cwd, _ := os.Getwd()
	return cwd
}

// GetMemoryPath returns the memory directory path for MPM's internal database.
// Priority: 1) Config file memory_dir(s), 2) MPM internal fallback (mpm/src/db)
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

// GetOpenClawDBPath returns the OpenClaw source DB path for ingest.
// Priority: 1) Config file openclaw_db_path, 2) Default ~/.openclaw/memory/main.sqlite
func GetOpenClawDBPath() string {
	if config, err := LoadConfig(); err == nil && config.OpenClawDBPath != "" {
		return ResolveEnvPath(config.OpenClawDBPath)
	}
	// Default: ~/.openclaw/memory/main.sqlite
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".openclaw", "memory", "main.sqlite")
}

// GetMPMDir returns the absolute path to the MPM directory
// All MPM runtime data (mode, persona, toxicphrases, src/db) lives here
func GetMPMDir() string {
	// Use executable-relative resolution to find mpm directory
	execPath, err := os.Executable()
	if err == nil {
		execDir := filepath.Dir(execPath)

		// Case: binary is at mpm/bin/mpm
		if filepath.Base(execDir) == "bin" {
			parent := filepath.Dir(execDir) // mpm/
			if filepath.Base(parent) == "mpm" {
				return parent
			}
		}

		// Case: binary is at mpm/ (running from source, e.g. ./mpm)
		if filepath.Base(execDir) == "mpm" {
			return execDir
		}

		// Case: binary is in PATH or elsewhere — walk up to find mpm/
		for dir := execDir; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if filepath.Base(dir) == "mpm" {
				return dir
			}
		}

		// Case: binary is in PATH or elsewhere — walk up to find mpm/
		workspace := GetWorkspace()
		// If workspace is already the mpm directory, use it directly
		if filepath.Base(workspace) == "mpm" {
			return workspace
		}
		// Otherwise, mpm is a subdirectory of workspace
		mpmDir := filepath.Join(workspace, "mpm")
		if _, err := os.Stat(mpmDir); err == nil {
			return mpmDir
		}
		// Fallback: workspace itself is the mpm directory
		return workspace
	}

	// Fallback: use workspace as the mpm directory
	return GetWorkspace()
}

// GetPersonaPath constructs the full path to the persona configurations directory
func GetPersonaPath() string {
	return filepath.Join(GetMPMDir(), "persona")
}

// GetModePath constructs the full path to the mode configurations directory
func GetModePath() string {
	return filepath.Join(GetMPMDir(), "mode")
}

// GetToxicPhrasesPath constructs the full path to the toxic phrases file
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
