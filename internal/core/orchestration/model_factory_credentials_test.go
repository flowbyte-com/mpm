package orchestration

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// clearCredentialEnv empties every provider variable the resolver reads,
// so each test observes only the source it exercises.
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

// factoryAPIKey builds a client through the production factory and
// returns the credential it would present. The adapter field is
// unexported but reachable from inside this package.
func factoryAPIKey(t *testing.T, p config.Profile) string {
	t.Helper()
	client, err := DefaultModelFactory()(p)
	if err != nil {
		t.Fatalf("DefaultModelFactory(%s): %v", p.Name, err)
	}
	adapter, ok := client.(*synthClientAdapter)
	if !ok {
		t.Fatalf("factory returned %T, want *synthClientAdapter", client)
	}
	return adapter.sc.APIKey
}

// TestModelFactory_EnvironmentOnlyCredential is the regression this
// change exists for: a remotely-hosted profile carrying no api_key
// used to be refused with "profile %q has no api_key", which disabled
// mpm_request_review and every reviewer/reflect capability for any
// operator whose credential lived in the environment. It must now
// authenticate exactly like synthesis does.
func TestModelFactory_EnvironmentOnlyCredential(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "env-secret")

	got := factoryAPIKey(t, config.Profile{
		Name:     "minimax",
		Provider: "minimax",
		Model:    "MiniMax-M3",
		BaseURL:  "https://api.minimax.io/anthropic/v1",
	})
	if got != "env-secret" {
		t.Errorf("factory must resolve the environment credential; got %q", got)
	}
}

// TestModelFactory_ConfiguredKeyWins pins precedence at the call site:
// an explicit profile api_key is authoritative and the environment is
// never consulted, preserving every install that stores a key on disk.
func TestModelFactory_ConfiguredKeyWins(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "env-secret")

	got := factoryAPIKey(t, config.Profile{
		Name:     "minimax",
		Provider: "minimax",
		Model:    "MiniMax-M3",
		BaseURL:  "https://api.minimax.io/anthropic/v1",
		APIKey:   "configured-secret",
	})
	if got != "configured-secret" {
		t.Errorf("configured key must win over the environment; got %q", got)
	}
}

// TestModelFactory_MissingCredentialStillErrors pins that the fix did
// not turn a missing credential into a silent success. A remotely
// hosted profile with neither source must still fail, and the error
// must name the variable to export so an environment-only operator is
// not sent to edit the config file.
func TestModelFactory_MissingCredentialStillErrors(t *testing.T) {
	clearCredentialEnv(t)

	_, err := DefaultModelFactory()(config.Profile{
		Name:     "minimax",
		Provider: "minimax",
		Model:    "MiniMax-M3",
		BaseURL:  "https://api.minimax.io/anthropic/v1",
	})
	if err == nil {
		t.Fatal("missing credential must still be an error, not a silent success")
	}
	if !strings.Contains(err.Error(), "MINIMAX_API_KEY") {
		t.Errorf("error must name the environment variable to export; got %q", err.Error())
	}
	// The error must never carry the secret itself.
	if strings.Contains(err.Error(), "env-secret") {
		t.Error("error message leaked a credential value")
	}
}

// TestModelFactory_LocalProfileNeedsNoCredential pins that locally
// hosted providers (Ollama and friends) keep working with no credential
// at all — the env fallback must not make an unauthenticated local
// endpoint fail.
func TestModelFactory_LocalProfileNeedsNoCredential(t *testing.T) {
	clearCredentialEnv(t)

	client, err := DefaultModelFactory()(config.Profile{
		Name:     "embedding-local",
		Provider: "ollama",
		Model:    "llama3",
		BaseURL:  "http://localhost:11434",
	})
	if err != nil {
		t.Fatalf("local profile must not require a credential: %v", err)
	}
	adapter := client.(*synthClientAdapter)
	if adapter.sc.APIKey != "" {
		t.Errorf("local profile must carry no credential, got %q", adapter.sc.APIKey)
	}
}

// TestModelFactory_NoCrossProviderFallthroughAtCallSite pins provider
// isolation where it now matters: the factory must not let a MiniMax
// credential satisfy an unrelated provider's profile.
func TestModelFactory_NoCrossProviderFallthroughAtCallSite(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "minimax-secret")

	_, err := DefaultModelFactory()(config.Profile{
		Name:     "some-openai-proxy",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  "https://api.openai.com/v1",
	})
	if err == nil {
		t.Fatal("MINIMAX_API_KEY must not authenticate an OpenAI endpoint")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("error must name OPENAI_API_KEY for this endpoint; got %q", err.Error())
	}
}