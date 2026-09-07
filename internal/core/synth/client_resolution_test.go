package synth

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfigFile is a test helper that writes a JSON config to disk.
// The config package's LoadConfig reads from MPM_WORKSPACE/mpm_config.json.
func writeConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config %s: %v", path, err)
	}
}

// TestNewSynthClient_ProfilesDefaultCanonical pins the launch-default
// resolution: a config with Profiles["default"] populates the SynthClient
// from the canonical profile. The legacy top-level `synth` block is no
// longer required.
func TestNewSynthClient_ProfilesDefaultCanonical(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	writeConfigFile(t, cfgFile, `{
  "profiles": {
    "default": {
      "provider": "minimax",
      "model": "TestModel-X",
      "base_url": "https://api.test.example/anthropic/v1",
      "api_key": "profile-key",
      "max_tokens": 2048,
      "timeout_seconds": 120
    }
  }
}`)

	sc := NewSynthClient()
	if sc.Model != "TestModel-X" {
		t.Errorf("expected Model from Profiles[default], got %q", sc.Model)
	}
	if sc.BaseURL != "https://api.test.example/anthropic/v1" {
		t.Errorf("expected BaseURL from Profiles[default], got %q", sc.BaseURL)
	}
	if sc.APIKey != "profile-key" {
		t.Errorf("expected APIKey from Profiles[default], got %q", sc.APIKey)
	}
	if sc.MaxTokens != 2048 {
		t.Errorf("expected MaxTokens=2048 from Profiles[default], got %d", sc.MaxTokens)
	}
	if sc.Timeout.Seconds() != 120 {
		t.Errorf("expected Timeout=120s from Profiles[default], got %v", sc.Timeout)
	}
}

// TestNewSynthClient_ComponentsBindingWins pins the routing layer:
// Components["synth"] = "<profile>" routes synthesis through that profile.
// Default is implicit via the Profiles["default"] fallback chain.
func TestNewSynthClient_ComponentsBindingWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	writeConfigFile(t, cfgFile, `{
  "components": {
    "synth": "fast"
  },
  "profiles": {
    "default": {"provider": "minimax", "model": "DefaultModel", "api_key": "default-key"},
    "fast":    {"provider": "minimax", "model": "FastModel",    "api_key": "fast-key"}
  }
}`)

	sc := NewSynthClient()
	if sc.Model != "FastModel" {
		t.Errorf("expected Model from Profiles[fast] via Components[synth] binding, got %q", sc.Model)
	}
	if sc.APIKey != "fast-key" {
		t.Errorf("expected APIKey from Profiles[fast] via binding, got %q", sc.APIKey)
	}
}

// TestNewSynthClient_LegacySynthFallback pins the migration path:
// pre-profiles configs with only a top-level `synth` block continue
// to work via the legacy migration fallback.
func TestNewSynthClient_LegacySynthFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	writeConfigFile(t, cfgFile, `{
  "synth": {
    "model": "LegacyModel",
    "api_key": "legacy-key",
    "base_url": "https://api.legacy.example/anthropic/v1",
    "max_tokens": 512,
    "timeout_seconds": 60
  }
}`)

	sc := NewSynthClient()
	if sc.Model != "LegacyModel" {
		t.Errorf("expected Model from legacy synth block, got %q", sc.Model)
	}
	if sc.APIKey != "legacy-key" {
		t.Errorf("expected APIKey from legacy synth block, got %q", sc.APIKey)
	}
	if sc.MaxTokens != 512 {
		t.Errorf("expected MaxTokens=512 from legacy synth block, got %d", sc.MaxTokens)
	}
}

// TestNewSynthClient_MixedConfigPrefersProfile pins the precedence rule:
// when both Profiles["default"] and a legacy Synth block are present,
// the canonical profile wins. The legacy block is silently ignored.
func TestNewSynthClient_MixedConfigPrefersProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	writeConfigFile(t, cfgFile, `{
  "profiles": {
    "default": {"provider": "minimax", "model": "ModernModel", "api_key": "modern-key"}
  },
  "synth": {
    "model": "LegacyModel",
    "api_key": "legacy-key",
    "base_url": "https://api.legacy.example/anthropic/v1"
  }
}`)

	sc := NewSynthClient()
	if sc.Model != "ModernModel" {
		t.Errorf("expected ModernModel (canonical), got %q", sc.Model)
	}
	if sc.APIKey != "modern-key" {
		t.Errorf("expected modern-key (canonical), got %q", sc.APIKey)
	}
}

// TestNewSynthClient_EnvFallbackForAPIKey pins the env-var fallback
// chain when no config supplies an API key.
func TestNewSynthClient_EnvFallbackForAPIKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)
	t.Setenv("MINIMAX_API_KEY", "env-key-test")

	cfgFile := filepath.Join(dir, "mpm_config.json")
	writeConfigFile(t, cfgFile, `{
  "profiles": {
    "default": {"provider": "minimax", "model": "TestModel"}
  }
}`)

	sc := NewSynthClient()
	if sc.APIKey != "env-key-test" {
		t.Errorf("expected APIKey from env fallback, got %q", sc.APIKey)
	}
}

// TestNewSynthClient_NoConfigUsesHardcodedDefaults pins the no-config
// path: when nothing is configured, the hardcoded MiniMax defaults
// apply (with no API key — env vars fill the gap if available).
func TestNewSynthClient_NoConfigUsesHardcodedDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)
	// Clear env to ensure API key comes from defaults (none).
	t.Setenv("MINIMAX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")

	cfgFile := filepath.Join(dir, "mpm_config.json")
	writeConfigFile(t, cfgFile, `{}`)

	sc := NewSynthClient()
	if sc.Model != "MiniMax-M2.7" {
		t.Errorf("expected hardcoded default Model, got %q", sc.Model)
	}
	if sc.BaseURL != "https://api.minimax.io/anthropic/v1" {
		t.Errorf("expected hardcoded default BaseURL, got %q", sc.BaseURL)
	}
}
