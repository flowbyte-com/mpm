// auth_transport_test.go — hermetic HTTP regression tests for the
// authentication half of the wire.
//
// These tests deliberately use ONLY the pre-existing public surface
// (SynthClient + DoLLMRequest) so that they compile and run against
// the behaviour they are guarding. That is what makes them regression
// tests rather than restatements of the implementation: each one
// fails against the previous behaviour and passes against the
// corrected one.
//
// What they pin:
//
//   - MiniMax's Anthropic-compatible surface presents
//     `Authorization: Bearer`, at the exact path /anthropic/v1/messages.
//     This is HAL-9001 Finding 1: the protocol and the credential were
//     conflated, so the endpoint received `X-Api-Key` and answered 401.
//   - Ordinary Anthropic keeps `X-Api-Key`.
//   - Ordinary OpenAI-compatible keeps `Authorization: Bearer`.
//   - The documented-endpoint rule matches on the parsed HOST, never on
//     a substring of the whole URL.
//   - A credential is never forwarded to a different origin by a
//     redirect.
//   - An authentication refusal is never retried blindly.
package synth

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// routeToServer points every outbound request at srv, whatever host the
// URL names.
//
// The documented-endpoint rules are HOST-based, so a test that wants to
// observe the real MiniMax request shape cannot use a 127.0.0.1 base
// URL — it must name the real host. This transport makes that possible
// without any network egress: the connection always goes to the local
// test server while the request line and Host header keep the name the
// rule under test matched on.
//
// EVERY test in this file that names a real hostname MUST route through
// here. A test that reaches api.anthropic.com by accident sends the
// operator's network to a public endpoint and its result depends on
// that endpoint's live behaviour — neither is acceptable in a
// hermetic regression test.
func routeToServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // hermetic loopback test server
	}
}

// newRoutedTLSServer returns a TLS test server plus a cleanup that
// routes outbound traffic to it. Use it for every https base URL.
func newRoutedTLSServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	routeToServer(t, srv)
	return srv
}

// captureAuth records the authentication header the endpoint saw.
type captureAuth struct {
	mu    sync.Mutex
	path  string
	name  string
	value string
}

func (c *captureAuth) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.name, c.value = "", ""
	if v := r.Header.Get("Authorization"); v != "" {
		c.name, c.value = "Authorization", v
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		c.name, c.value = "X-Api-Key", v
	}
}

func (c *captureAuth) snapshot() (string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.name, c.value
}

// TestTransport_MiniMaxAnthropicSurfacePresentsBearer is the headline
// regression test for HAL-9001 Finding 1.
//
// MiniMax's Anthropic-compatible endpoint is reached at
// https://api.minimax.io/anthropic/v1 and answers POST
// /anthropic/v1/messages — but it authenticates with
// `Authorization: Bearer <key>`, not with the Anthropic `X-Api-Key`
// header. The substrate conflated the message protocol with the
// authentication scheme, so it sent `X-Api-Key` and the endpoint
// answered HTTP 401 for a completely correct configuration.
//
// The test asserts the exact header, the exact path and the request
// shape reaching the endpoint.
func TestTransport_MiniMaxAnthropicSurfacePresentsBearer(t *testing.T) {
	var cap captureAuth
	newRoutedTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))

	const baseURL = "https://api.minimax.io/anthropic/v1"
	sc := &SynthClient{
		Model:   "MiniMax-M3",
		APIKey:  "minimax-secret-key",
		BaseURL: baseURL,
		Wire:    inferWire(baseURL),
		Timeout: 5 * time.Second,
	}
	raw, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":      "MiniMax-M3",
		"max_tokens": 16,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with exactly two words: OK READY"},
		},
	})
	if err != nil {
		t.Fatalf("DoLLMRequest: %v", err)
	}
	if !strings.Contains(string(raw), "OK READY") {
		t.Fatalf("response body = %q, want the provider's completion", string(raw))
	}

	path, name, value := cap.snapshot()
	if path != "/anthropic/v1/messages" {
		t.Errorf("request path = %q, want /anthropic/v1/messages", path)
	}
	if name != "Authorization" {
		t.Errorf("authentication header = %q, want Authorization (MiniMax's Anthropic-compatible surface is bearer-authenticated)", name)
	}
	if value != "Bearer minimax-secret-key" {
		t.Errorf("Authorization value = %q, want %q", value, "Bearer minimax-secret-key")
	}
}

// TestTransport_AnthropicNativePresentsXAPIKey pins that ordinary
// Anthropic authentication is untouched by the MiniMax correction.
func TestTransport_AnthropicNativePresentsXAPIKey(t *testing.T) {
	var cap captureAuth
	newRoutedTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))

	const baseURL = "https://api.anthropic.com/v1"
	sc := &SynthClient{
		Model:   "claude-sonnet-4-5",
		APIKey:  "anthropic-secret-key",
		BaseURL: baseURL,
		Wire:    inferWire(baseURL),
		Timeout: 5 * time.Second,
	}
	if _, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "claude-sonnet-4-5",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); err != nil {
		t.Fatalf("DoLLMRequest: %v", err)
	}
	_, name, value := cap.snapshot()
	if name != "X-Api-Key" {
		t.Errorf("authentication header = %q, want X-Api-Key (ordinary Anthropic auth must be preserved)", name)
	}
	if value != "anthropic-secret-key" {
		t.Errorf("X-Api-Key value = %q, want the configured credential", value)
	}
}

// TestTransport_OpenAICompatiblePresentsBearer pins that ordinary
// OpenAI-compatible authentication is untouched.
func TestTransport_OpenAICompatiblePresentsBearer(t *testing.T) {
	var cap captureAuth
	newRoutedTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	}))

	const baseURL = "https://api.openai.com/v1"
	sc := &SynthClient{
		Model:   "gpt-4o-mini",
		APIKey:  "openai-secret-key",
		BaseURL: baseURL,
		Wire:    inferWire(baseURL),
		Timeout: 5 * time.Second,
	}
	if _, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "gpt-4o-mini",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); err != nil {
		t.Fatalf("DoLLMRequest: %v", err)
	}
	path, name, value := cap.snapshot()
	if path != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", path)
	}
	if name != "Authorization" {
		t.Errorf("authentication header = %q, want Authorization", name)
	}
	if value != "Bearer openai-secret-key" {
		t.Errorf("Authorization value = %q, want %q", value, "Bearer openai-secret-key")
	}
}

// TestTransport_DocumentedRuleMatchesHostNotSubstring pins that the
// documented-endpoint rule is applied to the parsed HOST.
//
// A substring search for "minimax" would also match
// `https://notminimax.io`, `https://minimax.io.example.com` and a query
// string containing the word. Those are somebody else's server, and
// answering them with a bearer token would be wrong.
func TestTransport_DocumentedRuleMatchesHostNotSubstring(t *testing.T) {
	cases := []struct {
		name       string
		baseURL    string
		wantHeader string
	}{
		{"host merely contains the name", "https://notminimax.io/anthropic/v1", "X-Api-Key"},
		{"name is a prefix of another domain", "https://minimax.io.example.com/anthropic/v1", "X-Api-Key"},
		{"name appears in the path only", "https://api.minimax.io.evil.test/anthropic/v1", "X-Api-Key"},
		{"name appears in the query only", "https://gateway.example.com/v1?upstream=api.minimax.io", "X-Api-Key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap captureAuth
			newRoutedTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cap.record(r)
				_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
			}))

			sc := &SynthClient{
				Model:   "m",
				APIKey:  "k",
				BaseURL: tc.baseURL,
				Wire:    inferWire(tc.baseURL),
				Timeout: 5 * time.Second,
			}
			if _, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
				"model":    "m",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			}); err != nil {
				t.Fatalf("DoLLMRequest: %v", err)
			}
			_, name, _ := cap.snapshot()
			if name != tc.wantHeader {
				t.Errorf("authentication header = %q, want %q (the documented rule matches the parsed host, never a substring)", name, tc.wantHeader)
			}
		})
	}
}

// TestTransport_CrossOriginRedirectDoesNotForwardCredential pins that a
// redirect cannot carry the credential to a different origin.
//
// Go's default redirect handling drops `Authorization` on a cross-host
// redirect but keeps `X-Api-Key` — it treats that header as ordinary.
// Without an explicit guard, an Anthropic-protocol endpoint that
// redirects elsewhere would receive the operator's API key in a header
// the standard library considers harmless.
func TestTransport_CrossOriginRedirectDoesNotForwardCredential(t *testing.T) {
	var leaked atomic.Value
	leaked.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Api-Key")
		if got == "" {
			got = r.Header.Get("Authorization")
		}
		leaked.Store(got)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/messages", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	sc := &SynthClient{
		Model:   "m",
		APIKey:  "credential-must-not-leak",
		BaseURL: redirector.URL + "/v1",
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
	}
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if got := leaked.Load().(string); got != "" {
		t.Errorf("credential was forwarded to a different origin: %q", got)
	}
	if err == nil {
		t.Error("expected the cross-origin redirect to be refused, not followed")
	}
}

// TestTransport_AuthenticationRefusalIsNotRetriedBlindly pins that a
// refusal without evidence of a header mismatch is answered with ONE
// request.
//
// Retrying every 401 with a different authentication header would
// double the cost of every wrong-credential incident and would report a
// falsehood to the operator.
func TestTransport_AuthenticationRefusalIsNotRetriedBlindly(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer srv.Close()

	sc := &SynthClient{
		Model:   "m",
		APIKey:  "wrong-key",
		BaseURL: srv.URL + "/v1",
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
	}
	if _, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); err == nil {
		t.Fatal("expected an error on HTTP 401")
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1 (a refusal without header-mismatch evidence must not be retried)", n)
	}
}
