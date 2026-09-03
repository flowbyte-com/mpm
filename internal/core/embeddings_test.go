package internal

import (
	"os"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// helper: load empty config, return *Config
func emptyCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{}
}

func TestResolveEmbeddingConfig_Disabled(t *testing.T) {
	cfg := emptyCfg(t)
	cfg.Components = map[string]string{"embedding": "disabled"}
	got := resolveEmbeddingConfig(cfg)
	if got.Source != EmbeddingSourceDisabled {
		t.Errorf("Source = %v, want EmbeddingSourceDisabled", got.Source)
	}
	if !got.IntentionallyDisabled {
		t.Error("IntentionallyDisabled = false, want true")
	}
	if got.ProviderName != "null" {
		t.Errorf("ProviderName = %q, want \"null\"", got.ProviderName)
	}
}

func TestResolveEmbeddingConfig_Profile(t *testing.T) {
	cfg := emptyCfg(t)
	cfg.Profiles = map[string]config.Profile{
		"local-ollama": {
			Provider: "ollama",
			Model:    "nomic-embed-text",
			BaseURL:  "http://localhost:11434",
		},
	}
	cfg.Components = map[string]string{"embedding": "local-ollama"}
	got := resolveEmbeddingConfig(cfg)
	if got.Source != EmbeddingSourceProfile {
		t.Errorf("Source = %v, want EmbeddingSourceProfile", got.Source)
	}
	if got.ProfileName != "local-ollama" {
		t.Errorf("ProfileName = %q, want \"local-ollama\"", got.ProfileName)
	}
	if got.Status != EmbeddingStatusConfigured {
		t.Errorf("Status = %v, want EmbeddingStatusConfigured", got.Status)
	}
}

func TestResolveEmbeddingConfig_ProfileMissing(t *testing.T) {
	cfg := emptyCfg(t)
	cfg.Components = map[string]string{"embedding": "ghost"}
	got := resolveEmbeddingConfig(cfg)
	if got.Status != EmbeddingStatusMisconfigured {
		t.Errorf("Status = %v, want EmbeddingStatusMisconfigured", got.Status)
	}
	if got.LastError == nil {
		t.Error("LastError is nil, want error")
	}
}

func TestResolveEmbeddingConfig_EnvFallback(t *testing.T) {
	cfg := emptyCfg(t)
	t.Setenv("OLLAMA_ENDPOINT", "http://example.test:11434")
	t.Setenv("OLLAMA_MODEL", "test-model")
	got := resolveEmbeddingConfig(cfg)
	if got.Source != EmbeddingSourceEnvFallback {
		t.Errorf("Source = %v, want EmbeddingSourceEnvFallback", got.Source)
	}
	if got.ProviderName != "ollama:test-model" {
		t.Errorf("ProviderName = %q, want \"ollama:test-model\"", got.ProviderName)
	}
}

func TestResolveEmbeddingConfig_DisabledDoesNotFallThroughToEnv(t *testing.T) {
	cfg := emptyCfg(t)
	cfg.Components = map[string]string{"embedding": "disabled"}
	t.Setenv("OLLAMA_ENDPOINT", "http://example.test:11434")
	t.Setenv("OLLAMA_MODEL", "test-model")
	got := resolveEmbeddingConfig(cfg)
	if got.Source != EmbeddingSourceDisabled {
		t.Errorf("Source = %v, want EmbeddingSourceDisabled (env must NOT override disabled)", got.Source)
	}
}

func TestResolveEmbeddingConfig_Absent(t *testing.T) {
	cfg := emptyCfg(t)
	// explicit clear of env vars
	t.Setenv("OLLAMA_ENDPOINT", "")
	t.Setenv("OLLAMA_MODEL", "")
	got := resolveEmbeddingConfig(cfg)
	if got.Source != EmbeddingSourceAbsent {
		t.Errorf("Source = %v, want EmbeddingSourceAbsent", got.Source)
	}
	// suppress unused-import warning
	_ = os.Getenv
}

func TestEmbedText_NilForDisabled(t *testing.T) {
	// Force a disabled-source config via the test setter.
	prev := SetEmbedConfigForTest(&EmbeddingConfig{
		Source:       EmbeddingSourceDisabled,
		ProviderName: "null",
		Provider:     NullProvider{},
		Status:       EmbeddingStatusNull,
	})
	defer SetEmbedConfigForTest(prev)

	vec, err := EmbedText("hello")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if vec != nil {
		t.Errorf("vec = %v, want nil", vec)
	}
}

func TestEmbedText_ErrorOnProviderFailure(t *testing.T) {
	// Force a configured-but-unreachable provider by pointing at a closed port.
	prev := SetEmbedConfigForTest(&EmbeddingConfig{
		Source:       EmbeddingSourceEnvFallback,
		ProfileName:  "",
		ProviderName: "ollama:nomic-embed-text",
		Provider:     NewOllamaProvider("http://127.0.0.1:1", "nomic-embed-text"),
		Status:       EmbeddingStatusConfigured,
	})
	defer SetEmbedConfigForTest(prev)

	vec, err := EmbedText("hello")
	if err == nil {
		t.Fatal("err = nil, want error")
	}
	if vec != nil {
		t.Errorf("vec = %v, want nil", vec)
	}
}
