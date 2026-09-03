package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// EmbeddingProvider is the interface for generating text embeddings.
// Implementations can be local (Ollama) or remote (OpenAI-compatible API).
type EmbeddingProvider interface {
	// Embed generates a vector embedding for the given text.
	// Returns the vector as float32s and an error.
	Embed(text string) ([]float32, error)

	// Name returns a human-readable name for this provider.
	Name() string
}

// NullProvider is a no-op provider used when embeddings are disabled.
type NullProvider struct{}

func (NullProvider) Embed(text string) ([]float32, error) { return nil, nil }
func (NullProvider) Name() string                         { return "null" }

// OllamaProvider hits a local Ollama endpoint for embeddings.
type OllamaProvider struct {
	Endpoint string // e.g. "http://localhost:11434/api/embeddings"
	Model    string // e.g. "nomic-embed-text"
	Timeout  time.Duration
	client   *http.Client
}

func NewOllamaProvider(endpoint, model string) *OllamaProvider {
	if endpoint == "" {
		endpoint = "http://localhost:11434/api/embeddings"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return &OllamaProvider{
		Endpoint: endpoint,
		Model:    model,
		Timeout:  30 * time.Second,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (p *OllamaProvider) Embed(text string) ([]float32, error) {
	payload := map[string]interface{}{
		"model":  p.Model,
		"prompt": text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, p.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed: Ollama returned %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}

	return result.Embedding, nil
}

func (p *OllamaProvider) Name() string {
	return "ollama:" + p.Model
}

// EmbeddingSource identifies where the active embedding configuration
// came from. Diagnostic only — runtime logic should branch on Source
// when the distinction matters (e.g., env fallback should not be
// reported as "profile").
type EmbeddingSource int

const (
	EmbeddingSourceProfile EmbeddingSource = iota
	EmbeddingSourceEnvFallback
	EmbeddingSourceDisabled
	EmbeddingSourceAbsent
)

func (s EmbeddingSource) String() string {
	switch s {
	case EmbeddingSourceProfile:
		return "profile"
	case EmbeddingSourceEnvFallback:
		return "env"
	case EmbeddingSourceDisabled:
		return "disabled"
	case EmbeddingSourceAbsent:
		return "absent"
	}
	return "unknown"
}

type EmbeddingStatus int

const (
	EmbeddingStatusConfigured EmbeddingStatus = iota
	EmbeddingStatusUnreachable
	EmbeddingStatusMisconfigured
	EmbeddingStatusNull
)

func (s EmbeddingStatus) String() string {
	switch s {
	case EmbeddingStatusConfigured:
		return "configured"
	case EmbeddingStatusUnreachable:
		return "unavailable"
	case EmbeddingStatusMisconfigured:
		return "misconfigured"
	case EmbeddingStatusNull:
		return "null"
	}
	return "unknown"
}

// EmbeddingConfig is the resolved embedding configuration for the
// current process. Source and Status together describe every
// operator-visible state. IntentionallyDisabled is the canonical
// signal that the operator opted out via the "disabled" sentinel.
type EmbeddingConfig struct {
	Source                EmbeddingSource
	ProfileName           string
	ProviderName          string
	Provider              EmbeddingProvider
	Status                EmbeddingStatus
	IntentionallyDisabled bool
	LastError             error
}

// resolveEmbeddingConfig applies the canonical precedence:
//   1. components.embedding == "disabled" → IntentionallyDisabled
//   2. components.embedding == "<profile>" → resolve Profile, validate, build provider
//   3. components.embedding absent → OLLAMA_* env fallback
//   4. neither → NullProvider
//
// The "disabled" sentinel is honored ONLY for the embedding
// component. Other components return nil from ProfileFor when
// their value is "disabled" — same as a missing binding.
func resolveEmbeddingConfig(cfg *config.Config) *EmbeddingConfig {
	// 1. disabled
	if cfg != nil && cfg.Components != nil {
		if name, ok := cfg.Components["embedding"]; ok && name == "disabled" {
			return &EmbeddingConfig{
				Source:                EmbeddingSourceDisabled,
				ProviderName:          "null",
				Provider:              NullProvider{},
				Status:                EmbeddingStatusNull,
				IntentionallyDisabled: true,
			}
		}
	}

	// 2. profile
	if cfg != nil && cfg.Components != nil {
		if name, ok := cfg.Components["embedding"]; ok && name != "" && name != "disabled" {
			p := cfg.ProfileFor("embedding")
			if p == nil {
				return &EmbeddingConfig{
					Source:       EmbeddingSourceProfile,
					ProfileName:  name,
					ProviderName: "null",
					Provider:     NullProvider{},
					Status:       EmbeddingStatusMisconfigured,
					LastError:    fmt.Errorf("profile %q referenced by components.embedding does not exist", name),
				}
			}
			if err := validateEmbeddingProfile(p); err != nil {
				return &EmbeddingConfig{
					Source:       EmbeddingSourceProfile,
					ProfileName:  name,
					ProviderName: "null",
					Provider:     NullProvider{},
					Status:       EmbeddingStatusMisconfigured,
					LastError:    err,
				}
			}
			prov, provErr := buildProvider(p)
			if provErr != nil {
				return &EmbeddingConfig{
					Source:       EmbeddingSourceProfile,
					ProfileName:  name,
					ProviderName: providerName(p),
					Provider:     NullProvider{},
					Status:       EmbeddingStatusMisconfigured,
					LastError:    provErr,
				}
			}
			return &EmbeddingConfig{
				Source:       EmbeddingSourceProfile,
				ProfileName:  name,
				ProviderName: providerName(p),
				Provider:     prov,
				Status:       EmbeddingStatusConfigured,
			}
		}
	}

	// 3. env fallback
	endpoint := os.Getenv("OLLAMA_ENDPOINT")
	model := os.Getenv("OLLAMA_MODEL")
	if endpoint != "" || model != "" {
		if endpoint == "" {
			endpoint = "http://localhost:11434/api/embeddings"
		}
		if model == "" {
			model = "nomic-embed-text"
		}
		return &EmbeddingConfig{
			Source:       EmbeddingSourceEnvFallback,
			ProviderName: "ollama:" + model,
			Provider:     NewOllamaProvider(endpoint, model),
			Status:       EmbeddingStatusConfigured,
		}
	}

	// 4. absent
	return &EmbeddingConfig{
		Source:       EmbeddingSourceAbsent,
		ProviderName: "null",
		Provider:     NullProvider{},
		Status:       EmbeddingStatusNull,
	}
}

func validateEmbeddingProfile(p *config.Profile) error {
	if p.Provider == "" {
		return fmt.Errorf("embedding profile %q: provider is required", p.Name)
	}
	if p.Model == "" {
		return fmt.Errorf("embedding profile %q: model is required", p.Name)
	}
	if p.Provider != "ollama" {
		return fmt.Errorf("embedding profile %q: provider %q is not implemented (only \"ollama\" is supported by this spec)", p.Name, p.Provider)
	}
	return nil
}

func buildProvider(p *config.Profile) (EmbeddingProvider, error) {
	switch p.Provider {
	case "ollama":
		return NewOllamaProvider(p.BaseURL, p.Model), nil
	}
	return nil, fmt.Errorf("buildProvider: no implementation for provider %q", p.Provider)
}

func providerName(p *config.Profile) string {
	return p.Provider + ":" + p.Model
}

var (
	defaultEmbedConfigOnce sync.Once
	defaultEmbedConfig     *EmbeddingConfig
)

// DefaultEmbeddingConfig returns a cached embedding config using environment variables.
// The config is probed once and then cached for the lifetime of the process.
// Checks OLLAMA_ENDPOINT + OLLAMA_MODEL first, falls back to NullProvider.
func DefaultEmbeddingConfig() *EmbeddingConfig {
	defaultEmbedConfigOnce.Do(func() {
		defaultEmbedConfig = probeEmbeddingConfig()
	})
	return defaultEmbedConfig
}

// probeEmbeddingConfig attempts to detect and configure an embedding provider.
func probeEmbeddingConfig() *EmbeddingConfig {
	cfg := &EmbeddingConfig{
		Provider:     NullProvider{},
		ProviderName: "null",
	}

	endpoint := "http://localhost:11434/api/embeddings"
	model := "nomic-embed-text"

	if ep := getEnv("OLLAMA_ENDPOINT", ""); ep != "" {
		endpoint = ep
	}
	if mod := getEnv("OLLAMA_MODEL", ""); mod != "" {
		model = mod
	}

	// Build probe payload safely using json.Marshal to prevent injection
	probePayload, _ := json.Marshal(map[string]string{"model": model, "prompt": "test"})

	// Probe: try to reach Ollama
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(probePayload))
	if err != nil {
		return cfg
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		cfg.Provider = NewOllamaProvider(endpoint, model)
		cfg.ProviderName = "ollama"
		resp.Body.Close()
	}

	return cfg
}

// EmbedText tries the real embedding provider; falls back to HashEmbed on failure.
// This is the correct usage in all hot paths (mpm add, cascade materialize,
// and any other ingestion surface). The fsnotify-based watch daemon was
// deprecated in commit 6588cb8 and hard-removed in 215fd09 — there is
// no watcher ingest hot path; file ingestion is operator-driven via
// `mpm cascade materialize`.
func EmbedText(text string) []float32 {
	cfg := DefaultEmbeddingConfig()
	if cfg.Provider.Name() != "null" {
		if vec, err := cfg.Provider.Embed(text); err == nil && len(vec) > 0 {
			return vec
		}
	}
	return HashEmbed(text)
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
