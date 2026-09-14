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
	"net/url"
	"net/http"
	"os"
	"strings"
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

// OpenAICompatibleProvider hits an OpenAI-compatible `/v1/embeddings`
// endpoint. Covers LocalAI, LM Studio's embeddings tab, vLLM,
// llama.cpp-compatible servers, and any host that speaks the
// OpenAI embedding protocol.
//
// 2026-09-14 release-pass: the previous design was Ollama-only.
// Embedding capability is now provider-neutral: any endpoint that
// accepts `{input, model}` and returns `{data:[{embedding:[...]}]}` is
// supported. This matches the wire dispatch already used for LLM
// synthesis (`internal/core/synth/wire.go:inferWire`).
type OpenAICompatibleProvider struct {
	Endpoint string // e.g. "http://localhost:1234/v1"
	Model    string // e.g. "text-embedding-3-small"
	APIKey   string // optional — local servers may not require one
	Timeout  time.Duration
	client   *http.Client
}

func NewOpenAICompatibleProvider(endpoint, model, apiKey string) *OpenAICompatibleProvider {
	if endpoint == "" {
		endpoint = "http://localhost:1234/v1"
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	return &OpenAICompatibleProvider{
		Endpoint: endpoint,
		Model:    model,
		APIKey:   apiKey,
		Timeout:  30 * time.Second,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (p *OpenAICompatibleProvider) Embed(text string) ([]float32, error) {
	payload := map[string]interface{}{
		"input": text,
		"model": p.Model,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}

	// OpenAI-compatible endpoints expect POST {endpoint}/embeddings.
	// The user-supplied endpoint may already include /v1; tolerate
	// both forms.
	u := strings.TrimRight(p.Endpoint, "/")
	if !strings.HasSuffix(u, "/embeddings") {
		u = u + "/embeddings"
	}

	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed: endpoint returned %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// OpenAI shape: {"data":[{"embedding":[...], "index":0, "object":"embedding"}], ...}
	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}
	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embed: endpoint returned no embedding for model %q", p.Model)
	}
	return result.Data[0].Embedding, nil
}

func (p *OpenAICompatibleProvider) Name() string {
	return "openai-compatible:" + p.Model
}

// OllamaProvider hits a local Ollama endpoint for embeddings.
//
// Ollama moved from POST /api/embeddings (body: {"model","prompt"}) to
// POST /api/embed (body: {"model","input"}, response: {"embeddings":[[...]]}).
// See https://github.com/ollama/ollama/blob/main/docs/api.md#generate-embeddings.
type OllamaProvider struct {
	Endpoint string // e.g. "http://localhost:11434/api/embed"
	Model    string // e.g. "nomic-embed-text"
	Timeout  time.Duration
	client   *http.Client
}

func NewOllamaProvider(endpoint, model string) *OllamaProvider {
	if endpoint == "" {
		endpoint = "http://localhost:11434/api/embed"
	} else {
		// Accept either:
		//   - a bare host  ("http://localhost:11434")
		//   - the legacy   ("http://localhost:11434/api/embeddings")
		//   - the modern   ("http://localhost:11434/api/embed")
		// and normalize to the modern endpoint.
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			// Fall through unchanged; Embed() will surface a clearer error.
		} else if u.Path == "" || u.Path == "/" {
			u.Path = "/api/embed"
			endpoint = u.String()
		} else if u.Path == "/api/embeddings" {
			u.Path = "/api/embed"
			endpoint = u.String()
		}
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
		"model": p.Model,
		"input": text,
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

	// /api/embed returns {"embeddings": [[...]]} (plural, outer array). Some
	// installations may still emit the legacy singular {"embedding": [...]}
	// shape; accept both.
	var result struct {
		Embedding  []float32   `json:"embedding"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}
	if len(result.Embedding) > 0 {
		return result.Embedding, nil
	}
	if len(result.Embeddings) > 0 {
		return result.Embeddings[0], nil
	}
	return nil, fmt.Errorf("embed: Ollama returned no embedding for model %q", p.Model)
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

	// 3. env fallback — Ollama first (historical precedence),
	// then OpenAI-compatible (added in the 2026-09-14
	// capability-oriented expansion). Provider-specific env
	// vars win over the generic OPENAI_API_KEY so operators
	// with multiple keys set still route correctly.
	if endpoint, model, ok := ollamaEnvFallback(); ok {
		return &EmbeddingConfig{
			Source:       EmbeddingSourceEnvFallback,
			ProviderName: "ollama:" + model,
			Provider:     NewOllamaProvider(endpoint, model),
			Status:       EmbeddingStatusConfigured,
		}
	}
	if endpoint, model, ok := openAICompatEnvFallback(); ok {
		return &EmbeddingConfig{
			Source:       EmbeddingSourceEnvFallback,
			ProviderName: "openai-compatible:" + model,
			Provider:     NewOpenAICompatibleProvider(endpoint, model, ""),
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

// ollamaEnvFallback returns the canonical Ollama env-fallback
// tuple (endpoint, model, true) when OLLAMA_ENDPOINT or
// OLLAMA_MODEL is set. False means no Ollama fallback was
// configured. Preserves the pre-2026-09-14 precedence: the
// Ollama env vars take priority over the OpenAI-compatible
// fallback so existing operators don't see a silent swap.
func ollamaEnvFallback() (endpoint, model string, ok bool) {
	endpoint = os.Getenv("OLLAMA_ENDPOINT")
	model = os.Getenv("OLLAMA_MODEL")
	if endpoint == "" && model == "" {
		return "", "", false
	}
	if endpoint == "" {
		endpoint = "http://localhost:11434/api/embed"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return endpoint, model, true
}

// openAICompatEnvFallback returns the OpenAI-compatible env
// fallback tuple (endpoint, model, true) when one of the
// provider-specific endpoint env vars is set AND a key env
// var is present. False means no OpenAI-compatible fallback
// was configured.
//
// Provider-specific env var precedence (most-specific match
// first):
//   - OPENAI_ENDPOINT    + OPENAI_API_KEY
//   - OPENROUTER_ENDPOINT+ OPENROUTER_API_KEY
//   - OAI_COMPAT_ENDPOINT + any *_API_KEY   (generic)
//
// The API key env var is consumed inside NewOpenAICompatibleProvider
// at runtime — we don't inject it into the env here.
func openAICompatEnvFallback() (endpoint, model string, ok bool) {
	if endpoint = os.Getenv("OPENAI_ENDPOINT"); endpoint != "" {
		// Credential isolation: OPENAI_ENDPOINT reads ONLY
		// OPENAI_API_KEY. OPENROUTER_API_KEY is NOT consulted.
		if !hasAnyKeyEnv("OPENAI_API_KEY") {
			return "", "", false
		}
		model = os.Getenv("OPENAI_EMBEDDING_MODEL")
		if model == "" {
			model = "text-embedding-3-small"
		}
		return endpoint, model, true
	}
	if endpoint = os.Getenv("OPENROUTER_ENDPOINT"); endpoint != "" {
		// Credential isolation: OPENROUTER_ENDPOINT reads ONLY
		// OPENROUTER_API_KEY. OPENAI_API_KEY is NOT consulted.
		if !hasAnyKeyEnv("OPENROUTER_API_KEY") {
			return "", "", false
		}
		model = os.Getenv("OPENROUTER_EMBEDDING_MODEL")
		if model == "" {
			model = "openai/text-embedding-3-small"
		}
		return endpoint, model, true
	}
	if endpoint = os.Getenv("OAI_COMPAT_ENDPOINT"); endpoint != "" {
		// Credential isolation: generic OpenAI-compatible
		// endpoints read ONLY OAI_COMPAT_API_KEY (the dedicated
		// generic key). OPENAI_API_KEY and OPENROUTER_API_KEY
		// are NEVER consulted — operators with a local LM Studio
		// / LocalAI / vLLM endpoint MUST configure a separate
		// key (or set the profile's api_key explicitly).
		if !hasAnyKeyEnv("OAI_COMPAT_API_KEY") {
			return "", "", false
		}
		model = os.Getenv("OAI_COMPAT_EMBEDDING_MODEL")
		if model == "" {
			model = "text-embedding-3-small"
		}
		return endpoint, model, true
	}
	return "", "", false
}

// hasAnyKeyEnv reports whether at least one of the named env
// vars is non-empty. Used by openAICompatEnvFallback to require
// the operator to opt in to the OpenAI-compatible fallback
// explicitly via a key env var — a bare OPENAI_ENDPOINT without
// any key is rejected.
func hasAnyKeyEnv(names ...string) bool {
	for _, n := range names {
		if os.Getenv(n) != "" {
			return true
		}
	}
	return false
}

func validateEmbeddingProfile(p *config.Profile) error {
	if p.Provider == "" {
		return fmt.Errorf("embedding profile %q: provider is required", p.Name)
	}
	if p.Model == "" {
		return fmt.Errorf("embedding profile %q: model is required", p.Name)
	}
	// 2026-09-14 release-pass: embedding capability is no
	// longer Ollama-exclusive. OpenAI-compatible endpoints
	// (LocalAI, LM Studio, vLLM, llama.cpp, HF TEI) speak
	// `/v1/embeddings` and are first-class.
	//
	// "openai" and "openrouter" are explicit first-class
	// provider names but reuse the OpenAI-compatible transport
	// (same wire shape, same auth header).
	switch p.Provider {
	case "ollama", "openai-compatible", "openai", "openrouter":
		return nil
	}
	return fmt.Errorf("embedding profile %q: provider %q is not implemented (supported: \"ollama\", \"openai\", \"openai-compatible\", \"openrouter\")", p.Name, p.Provider)
}

func buildProvider(p *config.Profile) (EmbeddingProvider, error) {
	switch p.Provider {
	case "ollama":
		return NewOllamaProvider(p.BaseURL, p.Model), nil
	case "openai-compatible", "openai", "openrouter":
		return NewOpenAICompatibleProvider(p.BaseURL, p.Model, p.APIKey), nil
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

