package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultMiniBotConfig(t *testing.T) {
	cfg := DefaultMiniBotConfig()

	if cfg.Identity.Name != "mini-bot" {
		t.Errorf("expected Identity.Name 'mini-bot', got '%s'", cfg.Identity.Name)
	}

	if cfg.Synth.Model != "MiniMax-M2.7" {
		t.Errorf("expected Synth.Model 'MiniMax-M2.7', got '%s'", cfg.Synth.Model)
	}

	if cfg.Synth.BaseURL != "https://api.minimax.io/anthropic" {
		t.Errorf("expected Synth.BaseURL 'https://api.minimax.io/anthropic', got '%s'", cfg.Synth.BaseURL)
	}

	if cfg.SelfImprove.AnchorThreshold != 3 {
		t.Errorf("expected SelfImprove.AnchorThreshold 3, got %d", cfg.SelfImprove.AnchorThreshold)
	}

	if cfg.SelfImprove.Enabled != true {
		t.Error("expected SelfImprove.Enabled to be true")
	}
}

func TestLoadMiniBotConfig(t *testing.T) {
	// Create a temp config file
	dir := t.TempDir()
	configPath := filepath.Join(dir, "mini-bot-config.json")

	configJSON := `{
		"identity": {
			"name": "test-bot",
			"version": "0.1.0",
			"hot_reload": true
		},
		"synth": {
			"model": "custom-model",
			"api_key": "test-key",
			"base_url": "https://custom.api.com",
			"max_tokens": 8192,
			"timeout_seconds": 600
		},
		"telegram": {
			"bot_token": "test-token",
			"polling": true,
			"allowed_users": [12345]
		},
		"mcp": {
			"token": "mcp-token",
			"socket_path": "/tmp/test.sock"
		},
		"self_improve": {
			"enabled": false,
			"anchor_threshold": 5,
			"lesson_complexity_threshold": 10,
			"identity_patch_approval_required": false
		},
		"paths": {
			"db": "test.db",
			"identity": "test-identity.md",
			"mcp_socket": "test.sock"
		}
	}`

	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadMiniBotConfig(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Identity.Name != "test-bot" {
		t.Errorf("expected Identity.Name 'test-bot', got '%s'", cfg.Identity.Name)
	}

	if cfg.Identity.Version != "0.1.0" {
		t.Errorf("expected Identity.Version '0.1.0', got '%s'", cfg.Identity.Version)
	}

	if cfg.Identity.HotReload != true {
		t.Error("expected Identity.HotReload to be true")
	}

	if cfg.Synth.Model != "custom-model" {
		t.Errorf("expected Synth.Model 'custom-model', got '%s'", cfg.Synth.Model)
	}

	if cfg.Synth.APIKey != "test-key" {
		t.Errorf("expected Synth.APIKey 'test-key', got '%s'", cfg.Synth.APIKey)
	}

	if cfg.Synth.MaxTokens != 8192 {
		t.Errorf("expected Synth.MaxTokens 8192, got %d", cfg.Synth.MaxTokens)
	}

	if cfg.SelfImprove.Enabled != false {
		t.Error("expected SelfImprove.Enabled to be false")
	}

	if cfg.SelfImprove.AnchorThreshold != 5 {
		t.Errorf("expected SelfImprove.AnchorThreshold 5, got %d", cfg.SelfImprove.AnchorThreshold)
	}

	if len(cfg.Telegram.AllowedUsers) != 1 || cfg.Telegram.AllowedUsers[0] != 12345 {
		t.Errorf("expected Telegram.AllowedUsers [12345], got %v", cfg.Telegram.AllowedUsers)
	}

	if cfg.Paths.DB != "test.db" {
		t.Errorf("expected Paths.DB 'test.db', got '%s'", cfg.Paths.DB)
	}
}

func TestLoadMiniBotConfigDefaults(t *testing.T) {
	// Create a minimal config file to test defaults
	dir := t.TempDir()
	configPath := filepath.Join(dir, "mini-bot-config.json")

	// Minimal config with only some fields set
	configJSON := `{
		"identity": {
			"name": "minimal-bot"
		}
	}`

	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadMiniBotConfig(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that explicit values are preserved
	if cfg.Identity.Name != "minimal-bot" {
		t.Errorf("expected Identity.Name 'minimal-bot', got '%s'", cfg.Identity.Name)
	}

	// Check that defaults are applied
	if cfg.Synth.Model != "MiniMax-M2.7" {
		t.Errorf("expected default Synth.Model 'MiniMax-M2.7', got '%s'", cfg.Synth.Model)
	}

	if cfg.Synth.BaseURL != "https://api.minimax.io/anthropic" {
		t.Errorf("expected default Synth.BaseURL 'https://api.minimax.io/anthropic', got '%s'", cfg.Synth.BaseURL)
	}

	if cfg.SelfImprove.AnchorThreshold != 3 {
		t.Errorf("expected default SelfImprove.AnchorThreshold 3, got %d", cfg.SelfImprove.AnchorThreshold)
	}
}

func TestLoadMiniBotConfigEnvOverride(t *testing.T) {
	// Create a minimal config
	dir := t.TempDir()
	configPath := filepath.Join(dir, "mini-bot-config.json")

	configJSON := `{
		"identity": {
			"name": "env-test-bot"
		}
	}`

	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Set environment variables
	os.Setenv("MINIMAX_API_KEY", "env-api-key")
	os.Setenv("MPM_API_TOKEN", "env-mcp-token")
	defer func() {
		os.Unsetenv("MINIMAX_API_KEY")
		os.Unsetenv("MPM_API_TOKEN")
	}()

	cfg, err := LoadMiniBotConfig(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Synth.APIKey != "env-api-key" {
		t.Errorf("expected Synth.APIKey 'env-api-key' from env, got '%s'", cfg.Synth.APIKey)
	}

	if cfg.MCP.Token != "env-mcp-token" {
		t.Errorf("expected MCP.Token 'env-mcp-token' from env, got '%s'", cfg.MCP.Token)
	}
}

func TestLoadMiniBotConfigNotFound(t *testing.T) {
	_, err := LoadMiniBotConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Error("expected error for non-existent config file")
	}
}

func TestLoadMiniBotConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "mini-bot-config.json")

	if err := os.WriteFile(configPath, []byte("invalid json"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadMiniBotConfig(configPath)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestMiniBotConfigJSONRoundTrip(t *testing.T) {
	cfg := DefaultMiniBotConfig()

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}

	var decoded MiniBotConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	if decoded.Identity.Name != cfg.Identity.Name {
		t.Errorf("round-trip failed: expected Name '%s', got '%s'", cfg.Identity.Name, decoded.Identity.Name)
	}

	if decoded.Synth.Model != cfg.Synth.Model {
		t.Errorf("round-trip failed: expected Model '%s', got '%s'", cfg.Synth.Model, decoded.Synth.Model)
	}
}
