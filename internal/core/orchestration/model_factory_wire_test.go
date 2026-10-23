// model_factory_wire_test.go — the review/critic path must present the
// same request shape as synthesis and as the Doctor probe.
//
// The production ModelFactory used to build its SynthClient with a bare
// struct literal, which left the wire at its zero value. The zero value
// selects the Anthropic shape, so the factory spoke the Anthropic
// protocol — and presented the Anthropic authentication header —
// regardless of the profile's endpoint. A profile pointing at an
// OpenAI-compatible endpoint therefore sent requests the endpoint could
// not accept, while the probe against the same profile spoke a
// different protocol entirely.
//
// These tests assert the request that actually reaches the endpoint:
// the path and the authentication header.
//
// Every server here is a local httptest server; no test reaches a
// public endpoint.
package orchestration

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// wireCapture records the request shape the endpoint saw.
type wireCapture struct {
	mu    sync.Mutex
	path  string
	name  string
	value string
}

func (c *wireCapture) record(r *http.Request) {
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

func (c *wireCapture) snapshot() (string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.name, c.value
}

// newRoutedFactoryServer stands up a TLS endpoint that answers in the
// shape matching the path it was asked for, and routes all outbound
// traffic to it.
//
// The endpoint shape follows the request so that a request made with
// the WRONG protocol still parses — otherwise the test would fail on
// the parse error and hide the path/header mismatch it is meant to
// report.
func newRoutedFactoryServer(t *testing.T, cap *wireCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"content\":\"merged\",\"tags\":[]}"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"content\":\"merged\",\"tags\":[]}"}]}`))
	}))
	t.Cleanup(srv.Close)

	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // hermetic loopback test server
	}
	return srv
}

// TestDefaultModelFactory_PresentsTheProfileEndpointProtocol pins that
// the review/critic path speaks the protocol the profile's endpoint
// expects.
//
// Two shapes, two rules:
//
//   - an OpenAI-compatible endpoint is reached at
//     /chat/completions with `Authorization: Bearer`;
//   - MiniMax's Anthropic-compatible endpoint is reached at
//     /messages with `Authorization: Bearer` — the Anthropic message
//     format does NOT imply the Anthropic authentication header.
func TestDefaultModelFactory_PresentsTheProfileEndpointProtocol(t *testing.T) {
	cases := []struct {
		name       string
		baseURL    string
		wantPath   string
		wantHeader string
		wantValue  string
	}{
		{
			name:       "OpenAI-compatible endpoint",
			baseURL:    "https://api.openai.com/v1",
			wantPath:   "/v1/chat/completions",
			wantHeader: "Authorization",
			wantValue:  "Bearer factory-secret-key",
		},
		{
			name:       "MiniMax Anthropic-compatible endpoint",
			baseURL:    "https://api.minimax.io/anthropic/v1",
			wantPath:   "/anthropic/v1/messages",
			wantHeader: "Authorization",
			wantValue:  "Bearer factory-secret-key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap wireCapture
			newRoutedFactoryServer(t, &cap)

			profile := config.Profile{
				Name:     "review",
				Provider: "custom",
				Model:    "TestModel-X",
				APIKey:   "factory-secret-key",
				BaseURL:  tc.baseURL,
			}

			mc, err := DefaultModelFactory()(profile)
			if err != nil {
				t.Fatalf("DefaultModelFactory returned error: %v", err)
			}
			if _, err := mc.Query(context.Background(), "review this"); err != nil {
				t.Fatalf("Query: %v", err)
			}

			path, name, value := cap.snapshot()
			if path != tc.wantPath {
				t.Errorf("request path = %q, want %q (the factory must speak the profile's endpoint protocol)", path, tc.wantPath)
			}
			if name != tc.wantHeader {
				t.Errorf("authentication header = %q, want %q", name, tc.wantHeader)
			}
			if value != tc.wantValue {
				t.Errorf("%s value = %q, want %q", tc.wantHeader, value, tc.wantValue)
			}
		})
	}
}
