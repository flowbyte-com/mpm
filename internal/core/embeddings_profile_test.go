// embeddings_profile_test.go — HAL-9001 Finding 2.
//
// The embedding resolver selects its backend from the profile's
// provider name, and turns the profile's base URL into an HTTP
// request. Both are structural properties of the profile: a profile
// the resolver cannot act on is a configuration that fails later, at
// a point where the operator is no longer looking at it.
//
// 2026-10-23: the clean-install acceptance run saved an embedding
// profile the resolver rejected ("provider \"custom\" is not
// implemented"), and a base URL with a doubled colon that nothing
// refused.
package internal

import (
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestValidateEmbeddingProfile_RejectsTheUnimplementedProvider pins
// that "custom" — the vendor-neutral label the LLM path uses — is not
// an embedding provider.
//
// The LLM path can infer its wire shape from the endpoint, so it
// records "custom" and resolves later. The embedding resolver has no
// such inference: the provider name IS the backend selector.
func TestValidateEmbeddingProfile_RejectsTheUnimplementedProvider(t *testing.T) {
	p := &config.Profile{
		Name:     "embedding",
		Provider: "custom",
		Model:    "nomic-embed-text",
		BaseURL:  "http://127.0.0.1:11434",
	}
	if err := validateEmbeddingProfile(p); err == nil {
		t.Error("validateEmbeddingProfile accepted provider \"custom\"; the embedding resolver has no backend for it")
	}
}

// TestValidateEmbeddingProfile_AcceptsTheImplementedProviders pins
// that every provider the resolver implements is accepted.
func TestValidateEmbeddingProfile_AcceptsTheImplementedProviders(t *testing.T) {
	for _, provider := range []string{"ollama", "openai-compatible", "openai", "openrouter"} {
		p := &config.Profile{
			Name:     "embedding",
			Provider: provider,
			Model:    "some-model",
			BaseURL:  "http://127.0.0.1:11434",
		}
		if err := validateEmbeddingProfile(p); err != nil {
			t.Errorf("validateEmbeddingProfile rejected the implemented provider %q: %v", provider, err)
		}
	}
}

// TestValidateEmbeddingProfile_RejectsAMalformedBaseURL pins that a
// malformed endpoint is a structural problem.
//
// The doubled-colon form is what a hand-typed "http://" becomes when
// the colon is entered twice. It is not exotic and it is not
// secret — it is a typo, and a typo that used to be saved.
func TestValidateEmbeddingProfile_RejectsAMalformedBaseURL(t *testing.T) {
	p := &config.Profile{
		Name:     "embedding",
		Provider: "ollama",
		Model:    "nomic-embed-text",
		BaseURL:  "http:://127.0.0.1:11434",
	}
	if err := validateEmbeddingProfile(p); err == nil {
		t.Errorf("validateEmbeddingProfile accepted the malformed base URL %q", p.BaseURL)
	}
}

// TestValidateEmbeddingProfile_AcceptsUsableBaseURLs pins that the
// endpoints operators legitimately type are accepted, and that an
// empty base URL stays legal — the providers normalise it to their
// own default endpoint.
func TestValidateEmbeddingProfile_AcceptsUsableBaseURLs(t *testing.T) {
	for _, baseURL := range []string{
		"http://127.0.0.1:11434",
		"https://api.openai.com/v1",
		"http://localhost:1234/v1",
		"",
	} {
		p := &config.Profile{
			Name:     "embedding",
			Provider: "ollama",
			Model:    "nomic-embed-text",
			BaseURL:  baseURL,
		}
		if err := validateEmbeddingProfile(p); err != nil {
			t.Errorf("validateEmbeddingProfile rejected the usable base URL %q: %v", baseURL, err)
		}
	}
}
