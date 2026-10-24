// auth_probe_fields_test.go — regression tests for the NEW probe
// surfaces introduced by HAL-9001.
//
// These tests exercise capability that did not exist before the
// corrective pass (the explicit authentication-scheme override, the
// reported authentication scheme, and the authentication-failure
// sub-classification). They are therefore "existence-RED": against the
// previous behaviour they do not compile, because the fields they
// assert on are absent.
//
// They live in a separate file from auth_probe_test.go on purpose — the
// behavioural tests there must remain compilable against the original
// code so they can produce a genuine RED failure rather than a build
// error.
package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestProbe_ExplicitAuthSchemeOverrideIsHonoured pins the operator
// escape hatch.
//
// Some endpoints cannot be discovered reliably. An explicit override
// must win over every derived rule — including a documented-endpoint
// rule — so the operator can always pin the scheme.
func TestProbe_ExplicitAuthSchemeOverrideIsHonoured(t *testing.T) {
	const apiKey = "override-secret-key"

	var sawAuth atomic.Value
	sawAuth.Store("")

	newRoutedProbeTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))

	p := &config.Profile{
		Name:       "overridden",
		Provider:   "custom",
		Model:      "MiniMax-M3",
		BaseURL:    "https://api.minimax.io/anthropic/v1",
		APIKey:     apiKey,
		AuthScheme: "x-api-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if res.Status != ProbeHealthy {
		t.Fatalf("probe status = %q (%s); want healthy", res.Status, res.ErrorSummary)
	}
	if got := sawAuth.Load().(string); got != apiKey {
		t.Errorf("X-Api-Key = %q, want the configured credential (the explicit override must beat the documented rule)", got)
	}
}

// TestProbe_ReportsTheAuthenticationScheme pins that the probe reports
// which scheme it presented.
//
// Without this, an operator cannot tell whether MPM authenticated with
// the header the endpoint wanted, and a mismatch is indistinguishable
// from a wrong credential.
func TestProbe_ReportsTheAuthenticationScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	defer srv.Close()

	p := &config.Profile{
		Name:     "scheme",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "k",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if res.AuthScheme != "bearer" {
		t.Errorf("reported auth scheme = %q, want bearer (the OpenAI-protocol default)", res.AuthScheme)
	}
}

// TestProbe_ReportsTheAuthenticationFailureSubKind pins that the three
// authentication refusals stay distinguishable.
//
// An invalid credential, an unbilled subscription and an
// under-privileged key are three different problems with three
// different remedies. Collapsing them into one "auth failed" line is
// what made the first clean-install report untrustworthy.
func TestProbe_ReportsTheAuthenticationFailureSubKind(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"invalid credential", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, "invalid_credential"},
		{"expired subscription", http.StatusPaymentRequired, `{"error":{"message":"subscription expired"}}`, "subscription"},
		{"insufficient permission", http.StatusForbidden, `{"error":{"message":"this key may not use this model"}}`, "permission"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p := &config.Profile{
				Name:     "subkind",
				Provider: "custom",
				Model:    "some-model",
				BaseURL:  srv.URL + "/v1",
				APIKey:   "k",
			}

			res := probeGenerative(context.Background(), p, ProbeKindGenerative)
			if res.AuthFailure != tc.want {
				t.Errorf("reported auth failure = %q, want %q", res.AuthFailure, tc.want)
			}
		})
	}
}
