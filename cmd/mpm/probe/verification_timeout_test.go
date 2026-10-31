package probe

// verification_timeout_test.go — the connection-verification timeout and
// what it reports.
//
// THE DEFECT THIS FILE PINS
// --------------------------
// The post-save verification's bound was 1.5 seconds. The live MiniMax
// surface's own median latency is around a second and its slowest
// observed call was 4761ms — so a VALID credential was reported
// as a timeout.
//
// On a credential surface, a timeout is what an operator reads as "my
// credential is wrong". The provider never refused anything; it simply
// answered too late for a bound that had been set shorter than its own
// typical latency.
//
// The bound is now one number for every verification surface
// (ProbeTimeoutForVerification), it is measured rather than guessed, and
// the two bounds it depends on — the per-request bound and the total
// budget — are related by an assertion rather than by two constants
// that happen to sit near each other.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/synth"
)

// TestVerification_ASlowButValidProviderIsATimeoutNotAnAuthFailure is
// the headline classification test.
//
// The provider answers, but later than the bound. The operator must be
// told "timeout", and told NOTHING that would send them looking for a
// credential problem.
func TestVerification_ASlowButValidProviderIsATimeoutNotAnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Longer than the bound below, so the probe's deadline is what
		// ends the attempt.
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
	}, ProbeKindGenerative, 300*time.Millisecond)

	if res.Status != ProbeTimeout {
		t.Fatalf("status = %q, want %q — a provider that answered too late is a TIMEOUT",
			res.Status, ProbeTimeout)
	}
	if res.Status == ProbeAuthFailed {
		t.Error("a slow provider was reported as an authentication failure; the operator would go " +
			"looking for a credential problem they do not have")
	}
	if res.AuthFailure != "" {
		t.Errorf("AuthFailure = %q, want \"\" — a timeout judges nothing about the credential",
			res.AuthFailure)
	}
}

// TestVerification_TheProfilesTimeoutSecondsDoesNotShortenThe
// Verification is the independence test at the level the operator
// experiences it.
//
// `timeout_seconds` is the MODEL REQUEST timeout. An operator who set it
// to 3 seconds for a long generation must not thereby make every
// verification fail — and above all must not be told their credential
// was invalid.
func TestVerification_TheProfilesTimeoutSecondsDoesNotShortenTheVerification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(900 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	}))
	defer srv.Close()

	p := &config.Profile{
		Name:        "default",
		Provider:    "custom",
		Model:       "probe-model",
		BaseURL:     srv.URL + "/v1",
		TimeoutSecs: 3, // the MODEL REQUEST timeout
	}

	// The provider answers in 900ms — longer than the profile's 3
	// seconds would allow if the profile's value governed the
	// verification, and comfortably inside the verification's own
	// bound.
	res := probeGenerativeWithTimeout(context.Background(), p, ProbeKindGenerative,
		ProbeTimeoutForVerification)

	if res.Status != ProbeHealthy {
		t.Fatalf("status = %q, want %q — the profile's timeout_seconds must not govern the "+
			"verification (summary=%q)", res.Status, ProbeHealthy, res.ErrorSummary)
	}
}

// TestVerification_AnInvalidCredentialIsStillAnAuthFailure is the
// other direction of the distinction.
//
// The fix must not have softened the auth path: a refused credential is
// still reported as an auth failure, with the sub-kind the operator
// needs.
func TestVerification_AnInvalidCredentialIsStillAnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "a-key-the-provider-will-refuse",
	}, ProbeKindGenerative, ProbeTimeoutForVerification)

	if res.Status != ProbeAuthFailed {
		t.Fatalf("status = %q, want %q — a refused credential must still be an auth failure "+
			"(summary=%q)", res.Status, ProbeAuthFailed, res.ErrorSummary)
	}
	if res.AuthFailure == "" {
		t.Error("AuthFailure is empty; the operator is not told WHICH refusal this was")
	}
	if res.Status == ProbeTimeout {
		t.Error("an auth failure was reported as a timeout")
	}
}

// TestVerification_AForbiddenCredentialIsStillAnAuthFailure covers the
// sibling status.
func TestVerification_AForbiddenCredentialIsStillAnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"forbidden"}}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "a-key-the-provider-will-refuse",
	}, ProbeKindGenerative, ProbeTimeoutForVerification)

	if res.Status != ProbeAuthFailed {
		t.Fatalf("status = %q, want %q (summary=%q)", res.Status, ProbeAuthFailed, res.ErrorSummary)
	}
}

// TestVerification_AnUnresolvableCredentialIsNotATransportFailure is
// the third distinction, and the newest.
//
// A profile whose api_key names an environment variable this endpoint
// does not declare is a CONFIGURATION problem. Reporting it as
// "unreachable" sends the operator to look at the network; reporting it
// as an auth failure sends them to rotate a credential that was never
// the problem.
//
// The message carries the reason, so it must survive to the summary.
func TestVerification_AnUnresolvableCredentialIsNotATransportFailure(t *testing.T) {
	for _, name := range []string{
		"MINIMAX_API_KEY", "ANTHROPIC_API_KEY",
		"OPENAI_API_KEY", "OPENROUTER_API_KEY", "OAI_COMPAT_API_KEY",
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY",
		"MISTRAL_API_KEY", "COHERE_API_KEY",
	} {
		t.Setenv(name, "")
	}

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "$OAI_COMPAT_API_KEY", // declared, but unset
	}, ProbeKindGenerative, ProbeTimeoutForVerification)

	if res.Status == ProbeUnreachable {
		t.Errorf("status = %q — an unresolvable credential was reported as a transport failure; the "+
			"operator would go looking at the network", res.Status)
	}
	if res.Status == ProbeAuthFailed {
		t.Errorf("status = %q — an unresolvable credential was reported as an auth failure; the "+
			"credential was never judged", res.Status)
	}
	if requests != 0 {
		t.Errorf("the provider received %d request(s); an unresolved credential must never reach the wire",
			requests)
	}
	if !strings.Contains(res.ErrorSummary, "OAI_COMPAT_API_KEY") {
		t.Errorf("the summary does not name the variable to export: %q", res.ErrorSummary)
	}
}

// TestVerification_AnUndeclaredReferenceIsNotExpanded pins the
// exfiltration guard at the probe surface.
//
// The probe is a verification surface: it is the one place an operator
// is told "your credential is fine" or "your credential is wrong". It
// must not resolve a variable the endpoint does not declare, and it
// must not put one on the wire.
func TestVerification_AnUndeclaredReferenceIsNotExpanded(t *testing.T) {
	for _, name := range []string{
		"MINIMAX_API_KEY", "ANTHROPIC_API_KEY",
		"OPENAI_API_KEY", "OPENROUTER_API_KEY", "OAI_COMPAT_API_KEY",
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY",
		"MISTRAL_API_KEY", "COHERE_API_KEY",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("TOTALLY_UNRELATED_TOKEN", "a-secret-that-must-not-be-sent-anywhere")

	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "$TOTALLY_UNRELATED_TOKEN",
	}, ProbeKindGenerative, ProbeTimeoutForVerification)

	if sawAuth != "" {
		t.Errorf("a credential reached the wire (%q) from an undeclared reference", sawAuth)
	}
	if strings.Contains(res.ErrorSummary, "a-secret-that-must-not-be-sent-anywhere") {
		t.Errorf("the summary leaked the environment value: %q", res.ErrorSummary)
	}
	if res.Status == ProbeHealthy {
		t.Error("the verification reported success for an unresolvable credential")
	}
}

// TestVerification_TheSummaryCarriesNoSecret is the secret-hygiene
// guard on the rendered surface.
//
// The summary is persisted in the probe cache and printed to the
// operator. It must never carry a secret.
func TestVerification_TheSummaryCarriesNoSecret(t *testing.T) {
	const secret = "sk-live-a-very-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A provider that echoes the key back in its error body — the
		// case the sanitiser exists for.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key: ` + secret + `"}}`))
	}))
	defer srv.Close()

	res := probeGenerativeWithTimeout(context.Background(), &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    "probe-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   secret,
	}, ProbeKindGenerative, ProbeTimeoutForVerification)

	if strings.Contains(res.ErrorSummary, secret) {
		t.Errorf("the summary leaked the credential: %q", res.ErrorSummary)
	}
}

// TestVerification_CredentialErrorIsTyped pins that the failure the
// probe sees is the substrate's typed credential failure, so the
// classification can recognise it without parsing a message.
func TestVerification_CredentialErrorIsTyped(t *testing.T) {
	var credErr *synth.CredentialError
	err := error(&synth.CredentialError{
		Kind:       synth.CredentialReferenceUnset,
		Variable:   "MINIMAX_API_KEY",
		Recognised: []string{"MINIMAX_API_KEY"},
	})
	if !errors.As(err, &credErr) {
		t.Fatal("the credential error is not reachable through errors.As")
	}
	if credErr.Variable != "MINIMAX_API_KEY" {
		t.Errorf("Variable = %q", credErr.Variable)
	}
}
