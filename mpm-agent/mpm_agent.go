// mpm-agent — MPM companion agent
// MIT License
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

// ============================================================================
// Config & Paths
// ============================================================================

// Config from mpm_config.json (synth section)
type Config struct {
	Model        string `json:"model"`
	APIKey      string `json:"api_key"`
	BaseURL     string `json:"base_url"`
	MaxTokens   int    `json:"max_tokens"`
	TimeoutSecs int    `json:"timeout_seconds"`
}

// MPM paths — resolved same way as MPM itself
var (
	configPath = resolvePath("mpm_config.json", "mpm_config.json")
	dbPath     = resolvePath("src/db/mpm.db", "src/db/mpm.db")
)

// resolvePath resolves a path: MPM_WORKSPACE env var → executable-relative → CWD
func resolvePath(envKey, defaultRel string) string {
	if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
		return ws + "/flowbyte/mpm/" + defaultRel
	}
	exec, err := os.Executable()
	if err == nil {
		dir := exec
		for i := 0; i < 4; i++ {
			dir = dir[:len(dir)-len("/"+trimDir(dir))]
			if len(dir) == 0 {
				break
			}
			if trimDir(dir) == "mpm" {
				return dir + "/" + defaultRel
			}
		}
	}
	cwd, _ := os.Getwd()
	return cwd + "/" + defaultRel
}

func trimDir(p string) string {
	i := len(p) - 1
	for i > 0 && p[i] == '/' {
		i--
	}
	j := i
	for j > 0 && p[j] != '/' {
		j--
	}
	return p[j+1 : i+1]
}

// LoadConfig reads mpm_config.json and returns the synth config
func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read config at %s: %w", configPath, err)
	}
	var raw struct {
		Synth *Config `json:"synth"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Synth == nil {
		return &Config{}, nil // all defaults
	}
	// Fallbacks from env
	if raw.Synth.APIKey == "" {
		raw.Synth.APIKey = os.Getenv("MINIMAX_API_KEY")
	}
	if raw.Synth.BaseURL == "" {
		if os.Getenv("MINIMAX_BASE_URL") != "" {
			raw.Synth.BaseURL = os.Getenv("MINIMAX_BASE_URL")
		} else {
			raw.Synth.BaseURL = "https://api.minimax.io/anthropic"
		}
	}
	if raw.Synth.Model == "" {
		raw.Synth.Model = "MiniMax-M2.7"
	}
	if raw.Synth.MaxTokens == 0 {
		raw.Synth.MaxTokens = 1024
	}
	if raw.Synth.TimeoutSecs == 0 {
		raw.Synth.TimeoutSecs = 300
	}
	return raw.Synth, nil
}

// ============================================================================
// Database
// ============================================================================

// OpenDB opens the MPM database (read-write for tool execution).
func OpenDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open mpm.db at %s: %w", dbPath, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot connect to mpm.db at %s: %w", dbPath, err)
	}
	return db, nil
}

func main() {
	fmt.Println("mpm-agent v0.1.0 — not yet implemented")
	os.Exit(0)
}
