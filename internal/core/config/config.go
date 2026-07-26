package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the application configuration
type Config struct {
	Workspace      string            `json:"workspace,omitempty"`
	MemoryDir      string            `json:"memory_dir,omitempty"` // Legacy singular — use MemoryDirs
	MemoryDirs     []string          `json:"memory_dirs,omitempty"`
	SessionsDir    string            `json:"sessions_dir,omitempty"` // Legacy singular — use SessionsDirs
	SessionsDirs   []string          `json:"sessions_dirs,omitempty"`
	ExternalDbs    []ExternalDB      `json:"external_dbs,omitempty"`
	OpenClawDBPath string            `json:"openclaw_db_path,omitempty"` // Source DB for ingest (default: ~/.openclaw/memory/main.sqlite)
	Synth          *SynthConfig      `json:"synth,omitempty"`
	Aliases        map[string]string `json:"aliases,omitempty"` // CLI command aliases: "mem" → "recall --collection memories"
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

// SynthVendor describes a single vendor in the fallback chain.
type SynthVendor struct {
	Name       string `json:"name"`        // "minimax", "openai", "ollama"
	APIKey     string `json:"api_key"`     // vendor-specific key (overrides synth-level)
	BaseURL    string `json:"base_url"`    // e.g. "https://api.minimax.io/anthropic/v1"
	Model      string `json:"model"`       // model name for this vendor
	TimeoutSec int    `json:"timeout_sec"` // per-vendor timeout (0 = use default)
}

// SynthConfig holds LLM settings for the synth command.
// Vendors field defines the ordered fallback chain. If empty, MiniMax is used alone.
type SynthConfig struct {
	Model       string        `json:"model"`    // e.g. "MiniMax-M2.7", "gpt-4o", "llama3"
	APIKey      string        `json:"api_key"`  // primary API key
	BaseURL     string        `json:"base_url"` // e.g. "http://localhost:11434/v1"
	MaxTokens   int           `json:"max_tokens"`
	TimeoutSecs int           `json:"timeout_seconds"`
	Vendors     []SynthVendor `json:"vendors,omitempty"` // ordered fallback chain
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

// MPMDataDir is the subdirectory where all MPM runtime data resides
const MPMDataDir = "mpm"

// GetWorkspace determines the base workspace directory using a cascading priority system:
// 1. CLI Flag (--workspace) via environment variable (os.Getenv)
// 2. $HOME/.mpm — canonical default for a personal memory substrate
// 3. Current Working Directory (os.Getwd) — absolute last resort; the prototype DB
//    is created in cwd if missing, which is the ghost-DB shape; surface loudly.
//
// This enables portable installations - the same binary can work from any directory.
// All MPM runtime data resides within workspace/flowbyte/mpm/ (src/, mode/, persona/)
// Recommended structure:
//
//	/workspace/         ← User-configurable (workspace root)
//	└── symai/          ← Project folder
//	    └── projects/   ← MPM Go binary
//	        ├── src/db/         ← SQLite databases (mpm.db - consolidated)
//	        │   ├── mpm.db ← Main database
//	        │   ├── init.sql     ← Initialization script
//	        │   └── schema.sql   ← Database schema
//	        ├── mode/           ← Mode configurations (JSON files)
//	        ├── persona/        ← Persona configurations (JSON files)
//	        ├── toxicphrases.txt ← Cognitive firewall file
//	        └── src/            ← Source code (hidden from end users)
func GetWorkspace() string {
	// 1. Check environment variable (CLI flag or env var)
	if workspace := os.Getenv("MPM_WORKSPACE"); workspace != "" {
		return workspace
	}

	// 2. Default to $HOME/.mpm. Stricter than the previous fallback chain
	// (which probed CWD and the binary's exec path). The ghost-DB
	// incident of 2026-07-21 (lesson 59fe3f8ff3e1549e) showed that CWD
	// probing silently creates bogus DBs in unrelated dirs; the binary
	// walk-up also brittle (catches unrelated projects with similar
	// names). Home is the canonical default for a personal memory
	// substrate. Tests like TestGetWorkspace_DefaultsToHomeMpm pin this.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".mpm")
	}

	// 3. Absolute last resort (no $HOME available). CWD is the only
	// fallback left, but the prototype DB is created in cwd if missing —
	// which is the ghost-DB shape. Surface loudly: do not silently
	// create a DB here.
	cwd, _ := os.Getwd()
	return cwd
}

// GetMirrorPath constructs the full path to the mirror JSONL file
// Always located at mpm/src/db/mirror.jsonl (internal storage)
func GetMirrorPath() string {
	mpmDir := GetMPMDir()
	return filepath.Join(mpmDir, "src", "db", "mirror.jsonl")
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

// GetMPMDir returns the absolute path to the MPM data directory.
//
// Resolution order:
//  1. MPM_WORKSPACE environment variable (explicit override; required
//     for all production deployments)
//
// The legacy ~/.mpm fallback was removed when MPM migrated to
// system-level paths (/var/lib/mpm) on 2026-07-18. Silently writing
// to the operator's home directory caused permission errors under
// systemd User=v where the unit ran before login or lacked write
// access to /home. With the Lazy-Start Architecture in place
// (AGENTS.md Session Startup step 2 starts the daemon post-`/home`
// mount), that permission concern is no longer a hazard — `$HOME`
// is always available when `GetMPMDir` runs from an agent wake,
// and for non-agent callers the systemd-user `ExecStartPre`
// delay-start drop-in keeps the daemon from running before the
// mount is up. Falling back to CWD was the wrong trade: a daemon
// launched from /var/log or /tmp would initialize the SQLite data
// plane there, silently losing the entire procedural memory layer
// on reboot. `$HOME/.mpm` is the only sane default for a
// persistent background service.
func GetMPMDir() string {
	if envPath := os.Getenv("MPM_WORKSPACE"); envPath != "" {
		os.MkdirAll(envPath, 0755)
		return envPath
	}

	// os.UserHomeDir() handles the cross-platform nuances (sudo,
	// containers, init systems where $HOME may be stripped or
	// malformed). Cwd is the ultimate failsafe if even HOME is
	// unreachable — it keeps the daemon from crashing on a truly
	// broken environment, but the user will see the misconfig on
	// next invocation.
	home, err := os.UserHomeDir()
	if err != nil {
		cwd, _ := os.Getwd()
		return cwd
	}

	defaultPath := filepath.Join(home, ".mpm")
	os.MkdirAll(defaultPath, 0755)
	return defaultPath
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
