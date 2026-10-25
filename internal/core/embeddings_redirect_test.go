// embeddings_redirect_test.go — the credential-bearing redirect guard on
// the EMBEDDING providers (issue #67).
//
// The synthesis client got a redirect guard; the embedding providers did
// not. Go's standard library copies the `Authorization` header to any
// SUBDOMAIN of the original host, so an embedding endpoint that
// redirects api.example.com → evil.api.example.com handed the
// operator's bearer token to a host they never configured.
//
// These tests use LOCAL test servers and SYNTHETIC credentials. The
// router refuses to dial any hostname that has not been registered to a
// local test server, so no test can reach the public internet.

package internal

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
)

// embedHostRouter routes outbound connections to local test servers by
// hostname, so a test can name `api.example.com` and
// `evil.api.example.com` while both resolve to loopback.
type embedHostRouter struct {
	mu    sync.Mutex
	hosts map[string]*httptest.Server
}

func newEmbedHostRouter() *embedHostRouter {
	return &embedHostRouter{hosts: map[string]*httptest.Server{}}
}

func (h *embedHostRouter) route(host string, srv *httptest.Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hosts[strings.ToLower(host)] = srv
}

func (h *embedHostRouter) lookup(host string) *httptest.Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[strings.ToLower(host)]
}

func (h *embedHostRouter) install(t *testing.T) {
	t.Helper()
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("embedding redirect test router: bad address %q: %w", addr, err)
			}
			srv := h.lookup(host)
			if srv == nil {
				return nil, fmt.Errorf("embedding redirect test router: refusing to dial unrouted host %q", host)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // hermetic loopback test server
	}
}

func newEmbedServer(t *testing.T, router *embedHostRouter, host string, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	router.route(host, srv)
	return srv
}

// TestEmbedding_RedirectToSubdomainDoesNotForwardCredential pins that the
// embedding provider refuses a redirect to a subdomain of the configured
// host.
//
// Go's own redirect handling copies `Authorization` to any subdomain, so
// without the guard the operator's bearer token would reach
// evil.api.example.com.
func TestEmbedding_RedirectToSubdomainDoesNotForwardCredential(t *testing.T) {
	router := newEmbedHostRouter()
	var leaked atomic.Value
	leaked.Store("")

	newEmbedServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.api.example.com/v1/embeddings", http.StatusTemporaryRedirect)
	}))
	newEmbedServer(t, router, "evil.api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
	}))
	router.install(t)

	p := NewOpenAICompatibleProvider("http://api.example.com/v1", "text-embedding-3-small", "synthetic-embedding-key")
	_, err := p.Embed("hello")
	if got := leaked.Load().(string); got != "" {
		t.Errorf("credential was forwarded to a subdomain: %q", got)
	}
	if err == nil {
		t.Fatal("expected the subdomain redirect to be refused, not followed")
	}
}

// TestEmbedding_RedirectToUnrelatedHostIsRefused pins that a redirect to
// an unrelated host is refused outright.
func TestEmbedding_RedirectToUnrelatedHostIsRefused(t *testing.T) {
	router := newEmbedHostRouter()
	var leaked atomic.Value
	leaked.Store("")

	newEmbedServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.example.com/v1/embeddings", http.StatusTemporaryRedirect)
	}))
	newEmbedServer(t, router, "other.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
	}))
	router.install(t)

	p := NewOpenAICompatibleProvider("http://api.example.com/v1", "text-embedding-3-small", "synthetic-embedding-key")
	_, err := p.Embed("hello")
	if got := leaked.Load().(string); got != "" {
		t.Errorf("credential was forwarded to a different origin: %q", got)
	}
	if err == nil {
		t.Fatal("expected the cross-origin redirect to be refused, not followed")
	}
	if !strings.Contains(err.Error(), "refusing credential-bearing redirect") {
		t.Errorf("the refusal's explanation does not reach the operator: %v", err)
	}
}

// TestEmbedding_SameOriginRedirectIsFollowed pins that the guard does not
// break ordinary same-origin redirects on the embedding path.
func TestEmbedding_SameOriginRedirectIsFollowed(t *testing.T) {
	router := newEmbedHostRouter()
	var seen atomic.Value
	seen.Store("")

	newEmbedServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			http.Redirect(w, r, "http://api.example.com/v1/embeddings/", http.StatusTemporaryRedirect)
			return
		}
		seen.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
	}))
	router.install(t)

	p := NewOpenAICompatibleProvider("http://api.example.com/v1", "text-embedding-3-small", "synthetic-embedding-key")
	vec, err := p.Embed("hello")
	if err != nil {
		t.Fatalf("Embed: %v (a same-origin redirect must be followed)", err)
	}
	if len(vec) != 2 {
		t.Errorf("vector length = %d, want 2", len(vec))
	}
	if got := seen.Load().(string); got != "Bearer synthetic-embedding-key" {
		t.Errorf("credential seen at the same origin = %q, want the configured bearer token", got)
	}
}
