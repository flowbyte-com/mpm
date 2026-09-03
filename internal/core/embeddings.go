// Package internal — Embedding subsystem.
//
// The embedding subsystem resolves configuration from
// mpm_config.json's components["embedding"] binding, falling back
// to OLLAMA_ENDPOINT / OLLAMA_MODEL env vars when the binding is
// absent. The reserved sentinel "disabled" opts out cleanly.
//
// There is no runtime network probing. Operators who want to
// discover reachable providers run `mpm config detect-embedding`,
// which is explicitly diagnostic.
//
// See docs/superpowers/specs/2026-09-03-mpm-embedding-provider-design.md
// for the full design.
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
	// testEmbedConfig is installed by tests to override the cached config.
	testEmbedConfig *EmbeddingConfig
)

// SetEmbedConfigForTest installs a test config and returns the previous
// value so tests can defer restoration.
func SetEmbedConfigForTest(cfg *EmbeddingConfig) *EmbeddingConfig {
	prev := testEmbedConfig
	testEmbedConfig = cfg
	return prev
}

// ResetEmbedConfigForTest clears any test config, restoring the default.
// Tests that modify the embedding config should defer this to guarantee
// isolation regardless of test execution order.
func ResetEmbedConfigForTest() {
	testEmbedConfig = nil
}

// DefaultEmbeddingConfig returns a cached embedding config using
// mpm_config.json. The config is resolved once and then cached for
// the lifetime of the process.
//
// Resolution order (canonical; see docs/superpowers/specs/
// 2026-09-03-mpm-embedding-provider-design.md §4.1):
//   1. components.embedding == "disabled" → IntentionallyDisabled
//   2. components.embedding == "<profile>" → resolve profile
//   3. components.embedding absent → OLLAMA_* env fallback
//   4. neither → NullProvider
//
// No network probing at any step. Probing lives in
// `mpm config detect-embedding`.
func DefaultEmbeddingConfig() *EmbeddingConfig {
	if testEmbedConfig != nil {
		return testEmbedConfig
	}
	defaultEmbedConfigOnce.Do(func() {
		cfg, err := config.LoadConfig()
		if err != nil {
			// Treat load failure as "absent" rather than crashing
			// boot. Operators see the error via `mpm config show`.
			defaultEmbedConfig = &EmbeddingConfig{
				Source:       EmbeddingSourceAbsent,
				ProviderName: "null",
				Provider:     NullProvider{},
				Status:       EmbeddingStatusNull,
				LastError:    err,
			}
			return
		}
		defaultEmbedConfig = resolveEmbeddingConfig(cfg)
	})
	return defaultEmbedConfig
}

// EmbedText returns the embedding vector for the given text, or
// (nil, nil) when no provider is configured / reachable and the
// absence is acceptable. Returns (nil, err) when the provider was
// configured but failed.
//
// HashEmbed is NOT a fallback. Silent semantic substitution is the
// historical defect this spec eliminates. See docs/superpowers/specs/
// 2026-09-03-mpm-embedding-provider-design.md §4.3.
func EmbedText(text string) ([]float32, error) {
	cfg := DefaultEmbeddingConfig()
	switch cfg.Source {
	case EmbeddingSourceDisabled, EmbeddingSourceAbsent:
		return nil, nil
	case EmbeddingSourceProfile, EmbeddingSourceEnvFallback:
		vec, err := cfg.Provider.Embed(text)
		if err != nil {
			// Downgrade status for this process's lifetime.
			cfg.Status = EmbeddingStatusUnreachable
			cfg.LastError = err
			return nil, err
		}
		if len(vec) == 0 {
			err := fmt.Errorf("embed: provider %q returned zero-length vector", cfg.ProviderName)
			cfg.Status = EmbeddingStatusUnreachable
			cfg.LastError = err
			return nil, err
		}
		return vec, nil
	}
	return nil, nil
}

