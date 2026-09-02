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

// EmbeddingConfig holds global embedding configuration.
type EmbeddingConfig struct {
	Provider     EmbeddingProvider
	ProviderName string // "ollama", "openai", "null"
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
