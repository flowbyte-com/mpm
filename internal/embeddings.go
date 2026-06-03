package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// EmbeddingProvider is the interface for generating text embeddings.
// Implementations can be local (Ollama) or remote (OpenAI-compatible API).
type EmbeddingProvider interface {
	// Embed generates a vector embedding for the given text.
	// Returns the vector as float32s and an error.
	Embed(text string) ([]float32, error)

	// Dimensions returns the dimensionality of vectors produced by this provider.
	Dimensions() int

	// Name returns a human-readable name for this provider.
	Name() string
}

// NullProvider is a no-op provider used when embeddings are disabled.
type NullProvider struct{}

func (NullProvider) Embed(text string) ([]float32, error) { return nil, nil }
func (NullProvider) Dimensions() int                      { return 0 }
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

func (p *OllamaProvider) Dimensions() int {
	// Probe Ollama for dimensions if needed
	// For nomic-embed-text: 768
	// For other models: query /api/tags and inspect
	return 768 // default; probe at init for accuracy
}

func (p *OllamaProvider) Name() string {
	return "ollama:" + p.Model
}

// EmbeddingConfig holds global embedding configuration.
type EmbeddingConfig struct {
	Provider     EmbeddingProvider
	ProviderName string // "ollama", "openai", "null"
}

// DefaultEmbeddingConfig returns a config using environment variables.
// Checks OLLAMA_ENDPOINT + OLLAMA_MODEL first, falls back to NullProvider.
func DefaultEmbeddingConfig() *EmbeddingConfig {
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

	// Probe: try to reach Ollama
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte(`{"model":"`+model+`","prompt":"test"}`)))
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
// This is the correct usage in all hot paths (mpm add, watcher ingest).
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
