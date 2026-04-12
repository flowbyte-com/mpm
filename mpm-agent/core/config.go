package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MiniBotConfig is the top-level config for mini-bot.
type MiniBotConfig struct {
	Identity    IdentityConfig       `json:"identity"`
	Synth      SynthConfig          `json:"synth"`
	Telegram   TelegramConfig       `json:"telegram"`
	MCP        MCPConfig            `json:"mcp"`
	SelfImprove SelfImproveConfig   `json:"self_improve"`
	Paths      PathsConfig          `json:"paths"`
	Profiles   map[string][]string  `json:"profiles"`
}

type IdentityConfig struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	HotReload  bool   `json:"hot_reload"`
}

type SynthConfig struct {
	Model       string `json:"model"`
	APIKey      string `json:"api_key"`
	BaseURL     string `json:"base_url"`
	MaxTokens   int    `json:"max_tokens"`
	TimeoutSecs int    `json:"timeout_seconds"`
}

type TelegramConfig struct {
	BotToken     string  `json:"bot_token"`
	Polling      bool    `json:"polling"`
	AllowedUsers []int64 `json:"allowed_users"`
}

type MCPConfig struct {
	Token      string `json:"token"`
	SocketPath string `json:"socket_path"`
}

type SelfImproveConfig struct {
	Enabled                      bool `json:"enabled"`
	AnchorThreshold              int  `json:"anchor_threshold"`
	LessonComplexityThreshold    int  `json:"lesson_complexity_threshold"`
	IdentityPatchApprovalRequired bool `json:"identity_patch_approval_required"`
}

type PathsConfig struct {
	DB        string `json:"db"`
	Identity  string `json:"identity"`
	MCPSocket string `json:"mcp_socket"`
}

// DefaultMiniBotConfig returns the default config.
func DefaultMiniBotConfig() *MiniBotConfig {
	return &MiniBotConfig{
		Identity: IdentityConfig{Name: "mini-bot", Version: "1.0"},
		Synth: SynthConfig{
			Model: "MiniMax-M2.7",
			BaseURL: "https://api.minimax.io/anthropic",
			MaxTokens: 4096,
			TimeoutSecs: 300,
		},
		SelfImprove: SelfImproveConfig{
			Enabled: true,
			AnchorThreshold: 3,
			LessonComplexityThreshold: 7,
			IdentityPatchApprovalRequired: true,
		},
		Paths: PathsConfig{
			DB: "mini-bot.db",
			Identity: "IDENTITY.md",
			MCPSocket: "mini-bot-mcp.sock",
		},
		Profiles: map[string][]string{
			"standard": {
				"read_file", "write_file", "ReadFileSemantic", "ReadFileCompare",
				"WebSynthesize", "jq",
			},
		},
	}
}

// LoadMiniBotConfig reads and parses mini-bot-config.json.
func LoadMiniBotConfig(configPath string) (*MiniBotConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config at %s: %w", configPath, err)
	}
	var cfg MiniBotConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	defaults := DefaultMiniBotConfig()
	if cfg.Synth.Model == "" {
		cfg.Synth.Model = defaults.Synth.Model
	}
	if cfg.Synth.BaseURL == "" {
		cfg.Synth.BaseURL = defaults.Synth.BaseURL
	}
	if cfg.Synth.MaxTokens == 0 {
		cfg.Synth.MaxTokens = defaults.Synth.MaxTokens
	}
	if cfg.Synth.TimeoutSecs == 0 {
		cfg.Synth.TimeoutSecs = defaults.Synth.TimeoutSecs
	}
	if cfg.SelfImprove.AnchorThreshold == 0 {
		cfg.SelfImprove.AnchorThreshold = defaults.SelfImprove.AnchorThreshold
	}
	if cfg.SelfImprove.LessonComplexityThreshold == 0 {
		cfg.SelfImprove.LessonComplexityThreshold = defaults.SelfImprove.LessonComplexityThreshold
	}

	// Environment variable overrides
	if cfg.Synth.APIKey == "" {
		if envKey := os.Getenv("MINIMAX_API_KEY"); envKey != "" {
			cfg.Synth.APIKey = envKey
		}
	}
	if cfg.MCP.Token == "" {
		if envToken := os.Getenv("MPM_API_TOKEN"); envToken != "" {
			cfg.MCP.Token = envToken
		}
	}

	return &cfg, nil
}

// GetConfigPath returns the path to mini-bot-config.json.
func GetConfigPath() string {
	binaryDir := GetBinaryDir()
	if binaryDir != "" {
		path := filepath.Join(binaryDir, "mini-bot-config.json")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "mini-bot-config.json"
}
