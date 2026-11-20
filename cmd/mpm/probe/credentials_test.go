package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// clearCredentialEnv empties every provider variable the shared
// resolver reads, so each test observes only the source it exercises.
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

// TestProbeGenerative_EnvironmentCredentialReachesTheWire is the
// regression this change exists for. The probe built its SynthClient
// from profile.api_key alone, so an operator whose credential lived
// only in the environment got a false "provider refused request (auth)
// 401" from `mpm config profile add` and from the doctor Critic row —
// while real synthesis against the same profile succeeded. This test
// pins that the environment credential is actually presented in the
// Authorization header, not merely resolved into a struct field.
//
// The endpoint is a local httptest server, so it is classified
// wireOpenAI and the resolver reads OAI_COMPAT_API_KEY — which is
// itself the point: the probe uses the shared mapping rather than a
// bespoke lookup.
func TestProbeGenerative_EnvironmentCredentialReachesTheWire(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("OAI_COMPAT_API_KEY", "env-secret-probe")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "probe",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
	}, ProbeKindGenerative, 5*time.Second)

	if res.Status != ProbeHealthy {
		t.Fatalf("probe status = %v (err %q), want healthy", res.Status, res.ErrorSummary)
	}
	if gotAuth != "Bearer env-secret-probe" {
		t.Errorf("Authorization header = %q, want the environment credential on the wire", gotAuth)
	}
}

// TestProbeGenerative_ConfiguredCredentialWins pins precedence at the
// probe call site: an explicit profile api_key is authoritative and
// the environment is never consulted.
func TestProbeGenerative_ConfiguredCredentialWins(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("OAI_COMPAT_API_KEY", "env-secret-probe")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "probe",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "configured-secret-probe",
	}, ProbeKindGenerative, 5*time.Second)

	if res.Status != ProbeHealthy {
		t.Fatalf("probe status = %v (err %q), want healthy", res.Status, res.ErrorSummary)
	}
	if gotAuth != "Bearer configured-secret-probe" {
		t.Errorf("configured key must win; Authorization = %q", gotAuth)
	}
}

// TestProbeGenerative_MissingCredentialStillFailsAuth pins that
// resolving from the environment did not turn a missing credential
// into a silent pass: with no credential anywhere, the endpoint still
// rejects the unauthenticated request and the probe still reports
// ProbeAuthFailed rather than healthy.
func TestProbeGenerative_MissingCredentialStillFailsAuth(t *testing.T) {
	clearCredentialEnv(t)

	var sentToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The wire layer always sets an Authorization header; with no
		// credential it carries an empty bearer token. What matters is
		// that no credential VALUE reaches the endpoint.
		sentToken = strings.TrimSpace(strings.TrimPrefix(
			r.Header.Get("Authorization"), "Bearer"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"login fail"}}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "probe",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
	}, ProbeKindGenerative, 5*time.Second)

	if sentToken != "" {
		t.Errorf("probe sent a credential value with none configured: %q", sentToken)
	}
	if res.Status == ProbeHealthy {
		t.Fatal("unauthenticated probe must not report healthy")
	}
	if res.ErrorClass != "FailureAuth" {
		t.Errorf("ErrorClass = %q, want FailureAuth", res.ErrorClass)
	}
}

// TestProbeGenerative_ErrorSummaryCarriesNoSecret pins that neither the
// summary nor the classification embeds the credential. A provider
// error body is echoed to operators, and the probe result is persisted
// to the probe cache, so a secret reaching either would land on disk.
func TestProbeGenerative_ErrorSummaryCarriesNoSecret(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("OAI_COMPAT_API_KEY", "super-secret-value")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"login fail"}}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "probe",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
	}, ProbeKindGenerative, 5*time.Second)

	if strings.Contains(res.ErrorSummary, "super-secret-value") {
		t.Error("ErrorSummary leaked the credential into the persisted probe cache")
	}
}