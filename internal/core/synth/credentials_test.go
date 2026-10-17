package synth

import (
	"testing"
)

// clearCredentialEnv empties every provider variable the resolver can
// read, so a test observes only the source it is exercising. t.Setenv
// restores the prior values when the test ends.
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"MINIMAX_API_KEY", "ANTHROPIC_API_KEY",
		"OPENAI_API_KEY", "OPENROUTER_API_KEY", "OAI_COMPAT_API_KEY",
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY",
		"MISTRAL_API_KEY", "COHERE_API_KEY",
	} {
		t.Setenv(name, "")
	}
}

// TestResolveAPIKey_EnvironmentOnly pins the env-only credential: a
// profile with no api_key on disk authenticates from the provider's
// own environment variable. This is the configuration our install
// uses — the secret never enters mpm_config.json.
func TestResolveAPIKey_EnvironmentOnly(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "env-secret")

	got := ResolveAPIKey("", "https://api.minimax.io/anthropic/v1")
	if got != "env-secret" {
		t.Errorf("ResolveAPIKey with no configured key = %q, want the env value", got)
	}
}

// TestResolveAPIKey_ConfiguredKeyWins pins precedence: an explicitly
// configured api_key is authoritative and the environment is never
// consulted. Installations that store a key on disk must keep working
// unchanged, even when a stale variable happens to be exported.
func TestResolveAPIKey_ConfiguredKeyWins(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "env-secret")

	got := ResolveAPIKey("configured-secret", "https://api.minimax.io/anthropic/v1")
	if got != "configured-secret" {
		t.Errorf("configured key must win over the environment; got %q", got)
	}
}

// TestResolveAPIKey_MissingCredentialReturnsEmpty pins that a missing
// credential is reported honestly as "" rather than falling back to a
// default, a hash, or another provider's secret. Callers decide what an
// empty result means; the resolver never guesses.
func TestResolveAPIKey_MissingCredentialReturnsEmpty(t *testing.T) {
	clearCredentialEnv(t)

	if got := ResolveAPIKey("", "https://api.minimax.io/anthropic/v1"); got != "" {
		t.Errorf("missing credential must resolve to \"\", got %q", got)
	}
	if got := ResolveAPIKey("", "http://localhost:11434"); got != "" {
		t.Errorf("local endpoint with no env key must resolve to \"\", got %q", got)
	}
}

// TestResolveAPIKey_NoCrossProviderFallthrough pins credential
// isolation: an Anthropic-protocol provider must not accept an
// OpenAI-family secret, and a generic OpenAI-compatible endpoint must
// not borrow OPENAI_API_KEY or OPENROUTER_API_KEY. A provider reading
// another provider's credential is a silent credential leak.
func TestResolveAPIKey_NoCrossProviderFallthrough(t *testing.T) {
	cases := []struct {
		name       string
		baseURL    string
		envVar     string
		envValue   string
		wantDenied bool
	}{
		{
			name:       "minimax endpoint rejects an OpenAI key",
			baseURL:    "https://api.minimax.io/anthropic/v1",
			envVar:     "OPENAI_API_KEY",
			envValue:   "openai-secret",
			wantDenied: true,
		},
		{
			name:       "openai endpoint rejects a MiniMax key",
			baseURL:    "https://api.openai.com/v1",
			envVar:     "MINIMAX_API_KEY",
			envValue:   "minimax-secret",
			wantDenied: true,
		},
		{
			name:       "generic OpenAI-compatible endpoint rejects OPENAI_API_KEY",
			baseURL:    "http://192.0.2.10:1234/v1",
			envVar:     "OPENAI_API_KEY",
			envValue:   "openai-secret",
			wantDenied: true,
		},
		{
			name:       "local Ollama endpoint rejects a MiniMax key",
			baseURL:    "http://localhost:11434",
			envVar:     "MINIMAX_API_KEY",
			envValue:   "minimax-secret",
			wantDenied: true,
		},
		{
			name:       "openrouter endpoint rejects OPENAI_API_KEY",
			baseURL:    "https://openrouter.ai/api/v1",
			envVar:     "OPENAI_API_KEY",
			envValue:   "openai-secret",
			wantDenied: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearCredentialEnv(t)
			t.Setenv(tc.envVar, tc.envValue)
			if got := ResolveAPIKey("", tc.baseURL); tc.wantDenied && got != "" {
				t.Errorf("cross-provider fallthrough: %s must not satisfy %s; resolved %q",
					tc.envVar, tc.baseURL, got)
			}
		})
	}
}

// TestResolveAPIKey_AnthropicProtocolSecondaryFallback pins the
// Anthropic-protocol fallback order: MINIMAX_API_KEY is primary,
// ANTHROPIC_API_KEY secondary, so an Anthropic-protocol proxy without
// a MiniMax account still authenticates.
func TestResolveAPIKey_AnthropicProtocolSecondaryFallback(t *testing.T) {
	t.Run("primary wins when both are set", func(t *testing.T) {
		clearCredentialEnv(t)
		t.Setenv("MINIMAX_API_KEY", "minimax-secret")
		t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
		if got := ResolveAPIKey("", "https://api.minimax.io/anthropic/v1"); got != "minimax-secret" {
			t.Errorf("MINIMAX_API_KEY must take precedence, got %q", got)
		}
	})
	t.Run("secondary used when primary is absent", func(t *testing.T) {
		clearCredentialEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
		if got := ResolveAPIKey("", "https://api.anthropic.com/v1"); got != "anthropic-secret" {
			t.Errorf("ANTHROPIC_API_KEY fallback failed, got %q", got)
		}
	})
}

// TestAPIKeyEnvVars_ProviderMapping pins the base-URL to
// environment-variable mapping. Error messages name these variables, so
// a wrong entry would send an operator to export the wrong secret.
func TestAPIKeyEnvVars_ProviderMapping(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{"https://api.minimax.io/anthropic/v1", "MINIMAX_API_KEY"},
		{"https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "GEMINI_API_KEY"},
		{"https://api.x.ai/v1", "XAI_API_KEY"},
		{"https://api.mistral.ai/v1", "MISTRAL_API_KEY"},
		{"https://api.cohere.com/v1", "COHERE_API_KEY"},
		{"https://api.openai.com/v1", "OPENAI_API_KEY"},
		{"http://192.0.2.10:1234/v1", "OAI_COMPAT_API_KEY"},
		{"http://localhost:11434", "OAI_COMPAT_API_KEY"},
		// Back-compat pin: a host carrying no wire signal at all
		// falls back to the Anthropic wire, and therefore to
		// MINIMAX_API_KEY. This is long-standing documented
		// behaviour (see wire.go) and must not drift while
		// credential resolution is being changed.
		{"https://192.0.2.10:8000/v1", "MINIMAX_API_KEY"},
		{"https://example.invalid/custom", "MINIMAX_API_KEY"},
	}
	for _, tc := range cases {
		got := PrimaryAPIKeyEnvVar(tc.baseURL)
		if got != tc.want {
			t.Errorf("PrimaryAPIKeyEnvVar(%q) = %q, want %q", tc.baseURL, got, tc.want)
		}
	}
}

// TestResolveAPIKey_DoesNotMutateConfiguredValue pins that resolution
// never rewrites the caller's configured credential — the returned
// secret is used as-is, with no trimming, padding or normalisation.
func TestResolveAPIKey_DoesNotMutateConfiguredValue(t *testing.T) {
	clearCredentialEnv(t)
	const configured = "  spaced-key  "
	if got := ResolveAPIKey(configured, "https://api.minimax.io/anthropic/v1"); got != configured {
		t.Errorf("configured key must pass through verbatim, got %q", got)
	}
}