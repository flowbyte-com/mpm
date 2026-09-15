// probe_test.go — Wire-level generative + embedding probe tests using
// httptest.Server. Hermetic; no live network.

package probe

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// startFakeOpenAIServer stands up an OpenAI-protocol-compatible
// /v1/chat/completions endpoint that returns canned responses based on
// the configured handler (status, body). Token-required if apikey is
// non-empty; the handler validates it.
type openAIServerOpts struct {
	Status      int
	Body        string
	ExpectedKey string // when non-empty, request must carry Authorization: Bearer <key>
	HangForever bool
}

// buildOpenAIHandler is the shared handler factory for both
// startFakeOpenAIServer (default) and startFakeServerOnAddr (custom
// address). Extracted so Anthropic-wire tests can keep the auth + body
// logic consistent across listeners.
func buildOpenAIHandler(o openAIServerOpts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o.HangForever {
			// Sleep long enough to be observable, short enough that
			// httptest.Server.Close() doesn't hang the test suite.
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		if o.ExpectedKey != "" {
			// Accept either Bearer (OpenAI wire) or X-Api-Key (Anthropic
			// wire). The wire choice is determined by the test's
			// BaseURL keyword set, not by the path or body shape.
			bearer := r.Header.Get("Authorization")
			anthropicKey := r.Header.Get("X-Api-Key")
			ok := bearer == "Bearer "+o.ExpectedKey ||
				(o.ExpectedKey != "" && anthropicKey == o.ExpectedKey)
			if !ok {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key"}}`))
				return
			}
		}
		if o.Status != 0 && o.Status != 200 {
			w.WriteHeader(o.Status)
			_, _ = w.Write([]byte(o.Body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(o.Body))
	}
}

func startFakeOpenAIServer(t *testing.T, o openAIServerOpts) *httptest.Server {
	t.Helper()
	handler := buildOpenAIHandler(o)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/v1/messages", handler)
	mux.HandleFunc("/", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeGenerative_OpenAIHealthy(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Body: `{"choices":[{"message":{"content":"OK READY"}}]}`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeHealthy {
		t.Fatalf("want ProbeHealthy, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeGenerative_OpenAIEmptyCompletionIsInvalid(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Body: `{"choices":[{"message":{"content":""}}]}`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeInvalidResponse {
		t.Fatalf("want ProbeInvalidResponse (empty completion must NOT be healthy), got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeGenerative_Auth401(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Status:      401,
		Body:        `{"error":{"code":"invalid_api_key"}}`,
		ExpectedKey: "the-correct-key",
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
		APIKey:   "wrong-key",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeAuthFailed {
		t.Fatalf("want ProbeAuthFailed, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeGenerative_402IsNotAuthFailed(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Status: 402,
		Body:   `{"error":{"code":"insufficient_quota"}}`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status == ProbeAuthFailed {
		t.Fatalf("HTTP 402 must NOT map to ProbeAuthFailed; got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
	if r.Status != ProbeUnknown {
		t.Fatalf("want ProbeUnknown for HTTP 402 (preserve FailureRateLimit / FailureUnknown class), got %v", r.Status)
	}
}

func TestProbeGenerative_Generic404IsNotModelNotFound(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Status: 404,
		Body:   `<html>not found</html>`, // no model evidence
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status == ProbeModelNotFound {
		t.Fatalf("generic 404 must NOT be ProbeModelNotFound; got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
	if r.Status != ProbeUnknown {
		t.Fatalf("want ProbeUnknown for generic 404, got %v", r.Status)
	}
}

func TestProbeGenerative_404WithModelEvidenceIsModelNotFound(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Status: 404,
		Body:   `{"error":{"code":"model_not_found","message":"model not found: foo"}}`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeModelNotFound {
		t.Fatalf("404 with model evidence must be ProbeModelNotFound; got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeGenerative_ConnectionRefused(t *testing.T) {
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  "http://127.0.0.1:1", // no listener
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeUnreachable {
		t.Fatalf("want ProbeUnreachable, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeGenerative_TimeoutCancelsWithDeadline(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{HangForever: true})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
	}
	// 200ms — well below the production 12s default but enough to
	// observe a timeout classification.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeTimeout {
		t.Fatalf("want ProbeTimeout on hung connection + tight deadline, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeGenerative_AnthropicWireHealthy(t *testing.T) {
	// Anthropic-protocol test: bind a local server on a custom
	// hostname (no OpenAI-detection keywords) so production wire
	// inference routes it as Anthropic (X-Api-Key, /v1/messages).
	srv, cleanup := startFakeServerOnAddr(t, "127.0.0.2:0", openAIServerOpts{
		ExpectedKey: "anthropic-key",
		Body:        `{"content":[{"type":"text","text":"OK READY"}]}`,
	})
	defer cleanup()
	p := &config.Profile{
		Provider: "anthropic",
		Model:    "test-model",
		BaseURL:  srv.URL,
		APIKey:   "anthropic-key",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeHealthy {
		t.Fatalf("want ProbeHealthy on Anthropic wire, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeGenerative_AnthropicEmptyContentIsInvalid(t *testing.T) {
	srv, cleanup := startFakeServerOnAddr(t, "127.0.0.2:0", openAIServerOpts{
		ExpectedKey: "anthropic-key",
		Body:        `{"content":[{"type":"text","text":""}]}`,
	})
	defer cleanup()
	p := &config.Profile{
		Provider: "anthropic",
		Model:    "test-model",
		BaseURL:  srv.URL,
		APIKey:   "anthropic-key",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeInvalidResponse {
		t.Fatalf("Anthropic empty Content must be ProbeInvalidResponse, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

// startFakeServerOnAddr stands up an httptest server on a chosen
// address (e.g. "127.0.0.2:0") so URL-based wire inference in
// synth.InferWire produces a deterministic choice. Returns the server
// and a cleanup function for explicit teardown.
func startFakeServerOnAddr(t *testing.T, addr string, o openAIServerOpts) (*httptest.Server, func()) {
	t.Helper()
	handler := buildOpenAIHandler(o)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/v1/messages", handler)
	mux.HandleFunc("/", handler)
	srv := httptest.NewUnstartedServer(mux)
	// Start on the requested address.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("net.Listen(%q): %v", addr, err)
	}
	srv.Listener = listener
	srv.Start()
	cleanup := func() {
		srv.Close()
	}
	return srv, cleanup
}

func TestProbeGenerative_MalformedBodyIsInvalid(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Body: `not-json-{[`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "test-model",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeGenerative(ctx, p, ProbeKindGenerative)
	if r.Status != ProbeInvalidResponse {
		t.Fatalf("malformed body must be ProbeInvalidResponse, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

// Fingerprint test: changing the api_key invalidates the cache.
func TestComputeFingerprint_KeyChangeInvalidates(t *testing.T) {
	a := ComputeFingerprint(FingerprintInput{
		Provider: "openai-compatible", Model: "m", BaseURL: "http://x", Credential: "k1",
	})
	b := ComputeFingerprint(FingerprintInput{
		Provider: "openai-compatible", Model: "m", BaseURL: "http://x", Credential: "k2",
	})
	if a == b {
		t.Fatalf("fingerprint must change with credential")
	}
}

// BaseURL userinfo/query/fragment must be stripped from the persisted
// BaseURLSafe but the EXACT material bytes go into the fingerprint.
func TestSanitizeURL(t *testing.T) {
	got := sanitizeURL("https://user:pass@api.example.com/v1?token=abc#frag")
	want := "https://api.example.com/v1"
	if got != want {
		t.Fatalf("sanitizeURL: got %q, want %q", got, want)
	}
	// Same fingerprint for sanitized vs raw: fingerprint input is the
	// raw bytes (transient); the persisted BaseURLSafe is the sanitized
	// form. We don't test fingerprint determinism here — that's covered
	// by the inject test.
}

// ComputeFingerprint: model change invalidates the cache.
func TestComputeFingerprint_ModelChangeInvalidates(t *testing.T) {
	a := ComputeFingerprint(FingerprintInput{
		Provider: "openai-compatible", Model: "m1", BaseURL: "http://x", Credential: "",
	})
	b := ComputeFingerprint(FingerprintInput{
		Provider: "openai-compatible", Model: "m2", BaseURL: "http://x", Credential: "",
	})
	if a == b {
		t.Fatalf("fingerprint must change with model")
	}
}

// SanitizeError scrubs secret prefixes.
func TestSanitizeError_ScrubsSecrets(t *testing.T) {
	in := "auth failed: invalid api key sk-or-abcdef1234567890"
	out := sanitizeError(in)
	if strings.Contains(out, "sk-or-abcdef1234567890") {
		t.Fatalf("api key leaked in scrubbed summary: %q", out)
	}
}

// Status string is stable.
func TestProbeStatusString(t *testing.T) {
	cases := []struct {
		s    ProbeStatus
		want string
	}{
		{ProbeHealthy, "healthy"},
		{ProbeAuthFailed, "auth_failed"},
		{ProbeModelNotFound, "model_not_found"},
		{ProbeUnreachable, "unreachable"},
		{ProbeTimeout, "timeout"},
		{ProbeInvalidResponse, "invalid_response"},
		{ProbeDisabled, "disabled"},
		{ProbeUnknown, "unknown"},
	}
	for _, c := range cases {
		if c.s.String() != c.want {
			t.Fatalf("ProbeStatus.String() = %q, want %q", c.s.String(), c.want)
		}
	}
}

// JSON shape sanity: CachedProbe round-trips through Marshal/Unmarshal.
func TestCachedProbe_RoundTrip(t *testing.T) {
	in := CachedProbe{
		Fingerprint:  "abc123",
		Provider:     "openai-compatible",
		Model:        "m",
		BaseURLSafe:  "https://example.com/v1",
		Status:       "healthy",
		LatencyMs:    42,
		ErrorClass:   "FailureAuth",
		ErrorSummary: "auth failed",
		CheckedAt:    time.Now().Unix(),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out CachedProbe
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Fingerprint != in.Fingerprint || out.Status != in.Status || out.LatencyMs != in.LatencyMs {
		t.Fatalf("round trip drifted: %+v", out)
	}
}
