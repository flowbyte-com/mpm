// redirect_guard_test.go — hermetic regression tests for the
// credential-bearing redirect guard (issue #67).
//
// Every test in this file uses LOCAL test servers and SYNTHETIC
// credentials. No real credential is ever sent anywhere, and no test
// may reach the public internet: the router below REFUSES to dial any
// hostname that has not been explicitly routed to a local test server.
//
// WHAT THEY PIN (the behavioural contract):
//
//   - a same-origin redirect that spells out the default port is
//     ALLOWED (the first version of the guard compared the raw
//     "host:port" strings and refused it as "cross-origin");
//   - a redirect to a different host — including a SUBDOMAIN — a
//     different port or a different scheme is REFUSED;
//   - a refused redirect is not retried, and the refusal's own message
//     reaches the operator.
//
// This file deliberately uses ONLY the pre-existing public surface
// (SynthClient + DoLLMRequest + Plan), so it compiles against the
// behaviour it guards and fails against the uncorrected behaviour. The
// reporting contract — the typed sentinel and the distinct failure class
// — lives in redirect_refusal_reporting_test.go, which needs the API
// this change introduces.

package synth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hostRouter routes outbound connections to local test servers by
// hostname.
//
// The redirect guard's subject matter is HOSTS, so a test cannot use
// 127.0.0.1 for both ends of a redirect. The router lets a test name
// `api.example.com` and `evil.api.example.com` while both resolve to
// local test servers.
//
// A hostname that is not routed is REFUSED, not dialled: a test that
// forgets to register a host fails loudly instead of reaching a public
// endpoint.
type hostRouter struct {
	mu    sync.Mutex
	hosts map[string]*httptest.Server
}

func newHostRouter() *hostRouter {
	return &hostRouter{hosts: map[string]*httptest.Server{}}
}

// route registers srv as the local server for host.
func (h *hostRouter) route(host string, srv *httptest.Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hosts[strings.ToLower(host)] = srv
}

func (h *hostRouter) lookup(host string) *httptest.Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[strings.ToLower(host)]
}

// install replaces http.DefaultTransport with the router and restores the
// original on cleanup.
func (h *hostRouter) install(t *testing.T) {
	t.Helper()
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("redirect test router: bad address %q: %w", addr, err)
			}
			srv := h.lookup(host)
			if srv == nil {
				return nil, fmt.Errorf("redirect test router: refusing to dial unrouted host %q", host)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // hermetic loopback test server
	}
}

// newRedirectServer starts a local test server and registers it under host.
func newRedirectServer(t *testing.T, router *hostRouter, host string, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	router.route(host, srv)
	return srv
}

// redirectClient builds a SynthClient pointed at baseURL with a synthetic
// credential. The credential is a literal test string, never a real one.
func redirectClient(baseURL string) *SynthClient {
	return &SynthClient{
		Model:   "m",
		APIKey:  "synthetic-credential-must-not-leak",
		BaseURL: baseURL,
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
	}
}

// TestRedirect_SameOriginWithExplicitDefaultPortIsAllowed pins that the
// guard compares the EFFECTIVE origin.
//
// `http://api.example.com` and `http://api.example.com:80` are the same
// origin (RFC 6454: scheme, host, port — where the port defaults to the
// scheme's default when the URL does not spell it out). The first version
// of the guard compared the raw "host:port" strings and refused this
// redirect as "cross-origin", with a message that told the operator the
// credential would leave the origin they configured. That is a false
// alarm: the credential stays exactly where it was configured to go.
func TestRedirect_SameOriginWithExplicitDefaultPortIsAllowed(t *testing.T) {
	router := newHostRouter()
	var reached atomic.Int32
	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if r.URL.Path == "/v1/messages" {
			// Same origin, spelled with the explicit default port.
			http.Redirect(w, r, "http://api.example.com:80/v1/messages-final", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	raw, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("DoLLMRequest: %v (a same-origin redirect that spells out the default port must be followed)", err)
	}
	if !strings.Contains(string(raw), "OK READY") {
		t.Fatalf("response body = %q, want the provider's completion", string(raw))
	}
	if n := reached.Load(); n != 2 {
		t.Errorf("server hits = %d, want 2 (the original request and the same-origin redirect)", n)
	}
}

// TestRedirect_CrossOriginRedirectIsRefusedNotRetried pins that a refused
// redirect is reported as a refusal.
//
// Three properties at once:
//
//  1. it is refused — the credential does not reach the other origin;
//  2. it is NOT retried — the identical request would be refused
//     identically, so repeating it is pure cost;
//  3. the refusal's own message reaches the operator — it is not buried
//     behind a generic transport-failure wording.
func TestRedirect_CrossOriginRedirectIsRefusedNotRetried(t *testing.T) {
	router := newHostRouter()
	var attempts atomic.Int32
	var leaked atomic.Value
	leaked.Store("")

	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Redirect(w, r, "http://other.example.com/v1/messages", http.StatusTemporaryRedirect)
	}))
	newRedirectServer(t, router, "other.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Api-Key")
		if got == "" {
			got = r.Header.Get("Authorization")
		}
		leaked.Store(got)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the cross-origin redirect to be refused, not followed")
	}
	if got := leaked.Load().(string); got != "" {
		t.Errorf("credential was forwarded to a different origin: %q", got)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts against the redirecting origin = %d, want 1 (a deliberate refusal must not be retried)", n)
	}
	if !strings.Contains(err.Error(), "the credential would leave the configured origin") {
		t.Errorf("the refusal's own message does not reach the operator: %v", err)
	}
}

// TestRedirect_SubdomainRedirectIsRefused pins the case Go's own
// redirect handling is weakest on.
//
// Go copies the `Authorization` header to any SUBDOMAIN of the original
// host. An endpoint that redirects api.example.com → evil.api.example.com
// would therefore hand the operator's bearer token to a host the
// operator never configured.
func TestRedirect_SubdomainRedirectIsRefused(t *testing.T) {
	router := newHostRouter()
	var leaked atomic.Value
	leaked.Store("")

	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.api.example.com/v1/messages", http.StatusTemporaryRedirect)
	}))
	newRedirectServer(t, router, "evil.api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the subdomain redirect to be refused, not followed")
	}
	if got := leaked.Load().(string); got != "" {
		t.Errorf("credential was forwarded to a subdomain: %q", got)
	}
	if !strings.Contains(err.Error(), "the credential would leave the configured origin") {
		t.Errorf("the refusal's own message does not reach the operator: %v", err)
	}
}

// TestRedirect_PortChangeIsRefused pins that a different port is a
// different origin. Two services on the same host are two different
// origins, and a credential forwarded between them has left the origin
// the operator configured.
func TestRedirect_PortChangeIsRefused(t *testing.T) {
	router := newHostRouter()
	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://api.example.com:8443/v1/messages", http.StatusTemporaryRedirect)
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the port-changing redirect to be refused, not followed")
	}
	if !strings.Contains(err.Error(), "the credential would leave the configured origin") {
		t.Errorf("the refusal's own message does not reach the operator: %v", err)
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("refusal does not name the port difference: %v", err)
	}
}

// TestRedirect_SchemeDowngradeIsRefused pins that an https → http
// redirect is refused: it would send the credential in cleartext.
func TestRedirect_SchemeDowngradeIsRefused(t *testing.T) {
	router := newHostRouter()
	var leaked atomic.Value
	leaked.Store("")

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://api.example.com/v1/messages", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(tlsSrv.Close)
	router.route("api.example.com", tlsSrv)

	newRedirectServer(t, router, "plain.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("X-Api-Key"))
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	router.install(t)

	sc := redirectClient("https://api.example.com/v1")
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the https→http redirect to be refused, not followed")
	}
	if got := leaked.Load().(string); got != "" {
		t.Errorf("credential was forwarded in cleartext: %q", got)
	}
	if !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("refusal does not name the cleartext downgrade: %v", err)
	}
}

// TestRedirect_SchemeUpgradeIsRefused pins that an http → https redirect
// is refused as well.
//
// The scheme is part of the origin (RFC 6454), and the upgrade also
// changes the effective port (80 → 443). The first version of the guard
// compared the raw "host:port" strings and therefore ALLOWED this
// redirect, which is a different origin.
func TestRedirect_SchemeUpgradeIsRefused(t *testing.T) {
	router := newHostRouter()
	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://api.example.com/v1/messages", http.StatusTemporaryRedirect)
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the http→https redirect to be refused, not followed")
	}
	if !strings.Contains(err.Error(), "the credential would leave the configured origin") {
		t.Errorf("the refusal's own message does not reach the operator: %v", err)
	}
}

// TestRedirect_SameOriginRedirectKeepsTheCredential pins that a redirect
// that stays on the configured origin still carries the credential: the
// guard must not break ordinary same-origin redirects (some providers
// redirect `/v1/messages` → `/v1/messages/`).
func TestRedirect_SameOriginRedirectKeepsTheCredential(t *testing.T) {
	router := newHostRouter()
	var seen atomic.Value
	seen.Store("")

	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			http.Redirect(w, r, "http://api.example.com/v1/messages/", http.StatusTemporaryRedirect)
			return
		}
		seen.Store(r.Header.Get("X-Api-Key"))
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	if _, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); err != nil {
		t.Fatalf("DoLLMRequest: %v (a same-origin redirect must be followed)", err)
	}
	if got := seen.Load().(string); got != "synthetic-credential-must-not-leak" {
		t.Errorf("credential seen at the same origin = %q, want the configured credential", got)
	}
}
