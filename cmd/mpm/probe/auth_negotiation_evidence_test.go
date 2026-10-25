// auth_negotiation_evidence_test.go — hermetic regression tests for the
// bounded negotiation's EVIDENCE rule, seen from the operator's side
// (issue #66).
//
// The bounded alternative authentication attempt re-sends the operator's
// credential. It is permitted only when the provider's refusal
// EXPLICITLY names a different authentication header.
//
// The rule that shipped matched a bare substring anywhere in the response
// body, so a front-end proxy's own 401 page and a 403 permission body
// both authorised a credential-bearing retry. These tests drive the REAL
// probe path and count the requests the endpoint actually received.
//
// Every server here is a local httptest server and every credential is
// synthetic. No test reaches a public endpoint.

package probe

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestProbe_DoesNotNegotiateAProxyErrorPage pins that a front-end
// proxy's own 401 page does not authorise a credential-bearing retry.
//
// `<title>401 Authorization Required</title>` is the proxy's answer, not
// the provider's. The provider's opinion about the header was never
// expressed, so MPM has no evidence that the credential was presented in
// the wrong form.
//
// The base URL names a host that resolves to the Anthropic protocol, so
// the scheme MPM presents first is `X-Api-Key` — the body names
// `Authorization`, and the alternative scheme is therefore a real
// possibility. Without that, the body would name the scheme already in
// use and nothing would be negotiated either way.
func TestProbe_DoesNotNegotiateAProxyErrorPage(t *testing.T) {
	var attempts atomic.Int32
	newRoutedProbeTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<html><head><title>401 Authorization Required</title></head><body><h1>401 Authorization Required</h1></body></html>`))
	}))

	p := &config.Profile{
		Name:     "proxied",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "synthetic-proxy-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1 (a proxy's 401 page is not evidence of a header mismatch)", n)
	}
	if res.Status == ProbeHealthy {
		t.Error("probe reported healthy; the endpoint refused the credential")
	}
}

// TestProbe_DoesNotNegotiateAPermissionBody pins that a 403 whose body
// mentions "authorization" as a PERMISSION is not negotiated.
//
// The credential was accepted; the refusal is about what the account may
// do. Repeating the request with a different authentication header cannot
// help.
func TestProbe_DoesNotNegotiateAPermissionBody(t *testing.T) {
	var attempts atomic.Int32
	newRoutedProbeTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"your organization does not have authorization to use model X"}}`))
	}))

	p := &config.Profile{
		Name:     "under-privileged",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "synthetic-under-privileged-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1 (a permission refusal is not evidence of a header mismatch)", n)
	}
	if res.Status == ProbeHealthy {
		t.Error("probe reported healthy; the endpoint refused the request")
	}
}
