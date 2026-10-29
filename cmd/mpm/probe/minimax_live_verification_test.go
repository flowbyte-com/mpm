package probe

// LIVE MiniMax verification — issue #60.
//
// WHY THIS FILE EXISTS
// --------------------
// The MiniMax Anthropic-compatible authentication fix (HAL-9001
// Finding 1) was verified against hermetic fixtures only. The fixture
// tests prove the CLIENT sends `Authorization: Bearer` where it used to
// send `X-Api-Key`; they cannot prove that the live provider ACCEPTS it.
// Issue #60 asks for that second thing: one bounded request to the real
// endpoint with the operator's own credential.
//
// This file is that verification, in the form the next operator can
// re-run. It is opt-in: it SKIPS unless MINIMAX_API_KEY is present in
// the environment, so it is inert in CI and inert on any host that does
// not deliberately opt in.
//
// THE CREDENTIAL IS NEVER TOUCHED BY THIS TEST
// --------------------------------------------
// The profile below carries NO api_key. The credential is resolved by the
// production resolver (internal/synth.ResolveAPIKey), which reads
// MINIMAX_API_KEY from the environment for an Anthropic-protocol
// endpoint. The secret therefore never passes through a variable, a
// literal, a struct field or a format string in this file, and there is
// nothing here that could print it.
//
// The recording transport below records the request's SHAPE and nothing
// else: the path, the authentication header's NAME, the SCHEME PREFIX
// of its value ("Bearer" / "Basic" / ""), and whether the other
// authentication header was present. The credential VALUE is never
// stored, compared against a literal, or written anywhere.
//
// THE BOUNDS
// ----------
// Exactly ONE request reaches the provider per run. The test asserts
// that, which is how "normal operation does not unnecessarily retry or
// renegotiate authentication" is measured rather than assumed: a
// negotiation attempt would be a SECOND request, and the assertion would
// fail.
//
// The credential-redirect safeguard is examined WITHOUT the real
// credential: that half of the verification runs entirely against two
// local servers with a placeholder secret, so the operator's key is
// never presented to a second origin.
//
// WHAT THIS FILE DOES NOT ESTABLISH
// ---------------------------------
// It verifies the RELEASE-CANDIDATE SOURCE against the live provider. It
// does NOT verify a deployed revision: nothing here runs the installed
// binary, and issue #60's deployment-specific criterion stays open until
// a controlled deployment runs the same check against the installed
// build.

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/synth"
)

// liveMiniMaxBaseURL is the Anthropic-compatible surface MiniMax
// documents. The PROTOCOL half (Anthropic) and the AUTHENTICATION half
// (Bearer) are resolved independently — that independence is the fix.
const liveMiniMaxBaseURL = "https://api.minimax.io/anthropic/v1"

// liveMiniMaxModel is the model the release candidate names by default
// for a MiniMax profile.
const liveMiniMaxModel = "MiniMax-M2.7"

// recordedRequest is what the recording transport keeps. It is the
// request's SHAPE. It deliberately cannot hold the credential.
type recordedRequest struct {
	// Path is the request path, without any query string.
	Path string
	// AuthHeaderName is the name of the authentication header that was
	// present ("Authorization", "X-Api-Key", or "" for none).
	AuthHeaderName string
	// AuthScheme is the SCHEME PREFIX of the authentication header's
	// value ("Bearer", "Basic", "") — never the value itself.
	AuthScheme string
	// HasXAPIKey reports whether the Anthropic-style header was
	// present alongside. Its presence alongside a bearer credential is
	// the defect HAL-9001 Finding 1 was about.
	HasXAPIKey bool
}

// recordingTransport wraps the real transport and records the shape of
// every request that passes through it.
//
// It does not modify, buffer, reorder or rewrite the request: the
// credential goes to exactly the origin the client chose, over the real
// transport, with the real TLS verification. Only a shape is recorded
// alongside.
type recordingTransport struct {
	base    http.RoundTripper
	mu      sync.Mutex
	records []recordedRequest
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := recordedRequest{Path: req.URL.Path}

	// The authentication headers, shape only.
	if v := req.Header.Get("Authorization"); v != "" {
		rec.AuthHeaderName = "Authorization"
		if idx := strings.IndexByte(v, ' '); idx > 0 {
			rec.AuthScheme = v[:idx]
		} else {
			rec.AuthScheme = "" // a bare token, no scheme
		}
	}
	if v := req.Header.Get("X-Api-Key"); v != "" {
		rec.HasXAPIKey = true
		if rec.AuthHeaderName == "" {
			rec.AuthHeaderName = "X-Api-Key"
		}
	}

	rt.mu.Lock()
	rt.records = append(rt.records, rec)
	rt.mu.Unlock()

	return rt.base.RoundTrip(req)
}

func (rt *recordingTransport) snapshot() []recordedRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]recordedRequest, len(rt.records))
	copy(out, rt.records)
	return out
}

// withRecordingTransport installs the recorder for the duration of the
// test and restores the real default transport afterwards.
//
// http.DefaultTransport is a package-level variable, and an http.Client
// with a nil Transport uses it — which is exactly how the synthesis
// client builds its per-request client. Replacing it is therefore a
// complete interception of the probe's egress, without touching
// production code.
func withRecordingTransport(t *testing.T) *recordingTransport {
	t.Helper()
	original := http.DefaultTransport
	rt := &recordingTransport{base: original}
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = original })
	return rt
}

// TestLive_MiniMaxAnthropicSurfaceAuthenticatesWithBearer is the live
// half of issue #60.
//
// It sends ONE bounded request to the real MiniMax Anthropic-compatible
// surface with the operator's credential and asserts:
//
//   - the provider ACCEPTS the request (the probe reaches healthy, not
//     auth_failed);
//   - the authentication scheme that was presented is bearer;
//   - the credential was presented as `Authorization: Bearer`, and NOT
//     as `X-Api-Key`;
//   - exactly ONE request was made — no retry, no renegotiation.
//
// It skips when MINIMAX_API_KEY is absent, so it is inert by default.
func TestLive_MiniMaxAnthropicSurfaceAuthenticatesWithBearer(t *testing.T) {
	if os.Getenv("MINIMAX_API_KEY") == "" {
		t.Skip("MINIMAX_API_KEY is not set; the live provider verification is opt-in")
	}

	rt := withRecordingTransport(t)

	// NO api_key: the credential comes from the environment through the
	// production resolver, so this test never holds the secret.
	p := &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    liveMiniMaxModel,
		BaseURL:  liveMiniMaxBaseURL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := probeGenerative(ctx, p, ProbeKindGenerative)

	// ---- the outcome, recorded for the issue ----
	t.Logf("LIVE MiniMax verification")
	t.Logf("  base URL (endpoint category) : %s", liveMiniMaxBaseURL)
	t.Logf("  model                         : %s", liveMiniMaxModel)
	t.Logf("  probe status                  : %s", res.Status)
	t.Logf("  probe auth scheme             : %s", res.AuthScheme)
	t.Logf("  probe auth failure class      : %s", res.AuthFailure)
	t.Logf("  probe error class             : %s", res.ErrorClass)
	t.Logf("  probe error summary           : %s", res.ErrorSummary)
	t.Logf("  latency                       : %d ms", res.LatencyMs)

	records := rt.snapshot()
	t.Logf("  requests observed             : %d", len(records))
	for i, r := range records {
		t.Logf("    request %d: path=%s auth_header=%s scheme=%q x_api_key_present=%t",
			i+1, r.Path, r.AuthHeaderName, r.AuthScheme, r.HasXAPIKey)
	}

	// ---- the assertions ----

	if res.Status != ProbeHealthy {
		t.Fatalf("the live provider did NOT accept the request: status=%q error_class=%q summary=%q",
			res.Status, res.ErrorClass, res.ErrorSummary)
	}

	if res.AuthScheme != "bearer" {
		t.Fatalf("the scheme presented was %q, want bearer", res.AuthScheme)
	}

	// Exactly one request: no retry, no renegotiation, no second probe.
	if len(records) != 1 {
		t.Fatalf("%d requests reached the provider, want exactly 1 — normal operation must not "+
			"retry or renegotiate authentication", len(records))
	}

	got := records[0]
	if got.AuthHeaderName != "Authorization" {
		t.Fatalf("the authentication header was %q, want Authorization", got.AuthHeaderName)
	}
	if got.AuthScheme != "Bearer" {
		t.Fatalf("the authentication scheme was %q, want Bearer", got.AuthScheme)
	}
	if got.HasXAPIKey {
		t.Fatal("the request carried X-Api-Key as well; the Anthropic-style header must not be " +
			"presented to an endpoint that expects a bearer credential")
	}
	if want := "/anthropic/v1/messages"; got.Path != want {
		t.Fatalf("the request path was %q, want %q", got.Path, want)
	}
}

// TestLive_MiniMaxSchemeResolutionIsHostExact asserts the scheme rule
// itself, without a network request.
//
// The documented rule is matched on the PARSED HOST against an exact
// domain set, so a lookalike host must NOT inherit the bearer rule —
// sending the operator's credential to `notminimax.io` in the wrong
// header shape would be the failure this rule exists to prevent.
//
// This is an offline assertion and costs no provider request.
func TestLive_MiniMaxSchemeResolutionIsHostExact(t *testing.T) {
	cases := []struct {
		baseURL string
		want    synth.AuthScheme
		why     string
	}{
		{"https://api.minimax.io/anthropic/v1", synth.AuthSchemeBearer, "the documented MiniMax surface"},
		{"https://api.minimaxi.com/anthropic/v1", synth.AuthSchemeBearer, "MiniMax's other documented domain"},
		{"https://api.anthropic.com/v1", synth.AuthSchemeXAPIKey, "ordinary Anthropic keeps X-Api-Key"},
		{"https://notminimax.io/anthropic/v1", synth.AuthSchemeXAPIKey, "a lookalike host is not MiniMax"},
		{"https://minimax.io.example.com/anthropic/v1", synth.AuthSchemeXAPIKey, "a subdomain of a lookalike is not MiniMax"},
	}
	for _, tc := range cases {
		got := synth.ResolveAuthScheme(tc.baseURL, "placeholder", "")
		if got != tc.want {
			t.Errorf("%s: resolved scheme = %q, want %q", tc.why, got, tc.want)
		}
	}
}

// originRouter routes outbound requests to local servers by the HOST
// the URL names.
//
// The documented-endpoint rules are HOST-based, so a test that observes
// the real request shape cannot use a 127.0.0.1 base URL — it must name
// the real host. This transport keeps the name in the request line and
// the Host header while the connection stays on loopback.
//
// It exists so a REDIRECT can be examined across two origins: the first
// origin is reached under one host name and the redirect target under
// another, and each is a separate local server that can record what it
// was given.
type originRouter struct {
	mu       sync.Mutex
	byHost   map[string]*httptest.Server
	fallback http.RoundTripper
}

func (r *originRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	srv := r.byHost[strings.ToLower(req.URL.Hostname())]
	r.mu.Unlock()
	if srv == nil {
		return r.fallback.RoundTrip(req)
	}
	return (&http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // hermetic loopback test server
	}).RoundTrip(req)
}

// TestLive_CredentialIsNotForwardedToAnotherOrigin examines the redirect
// and credential-origin safeguards.
//
// It does NOT use the operator's credential: the two servers are local
// and the secret is a placeholder. The point is to prove that a
// cross-origin redirect is REFUSED — that the client does not present
// the credential to the second origin — and that the refusal is a typed
// decision rather than a transport failure.
//
// The demonstration deliberately uses the Anthropic-style credential
// (X-Api-Key), because that is where Go's standard library is
// dangerous: it drops `Authorization` on a cross-host redirect but
// copies `X-Api-Key` ANYWHERE. The guard has to hold for the header
// that the standard library would have leaked.
func TestLive_CredentialIsNotForwardedToAnotherOrigin(t *testing.T) {
	const placeholder = "placeholder-not-a-real-credential"

	// The SECOND origin: it records anything that reaches it.
	var secondOriginHits atomic.Int64
	var secondOriginSawCredential atomic.Bool
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondOriginHits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			secondOriginSawCredential.Store(true)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	t.Cleanup(second.Close)

	// The FIRST origin: it redirects to the second one.
	var firstOriginSawCredential atomic.Bool
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			firstOriginSawCredential.Store(true)
		}
		http.Redirect(w, r, "https://second.example/anthropic/v1/messages", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(first.Close)

	original := http.DefaultTransport
	router := &originRouter{
		byHost: map[string]*httptest.Server{
			"first.example":  first,
			"second.example": second,
		},
		fallback: original,
	}
	http.DefaultTransport = router
	t.Cleanup(func() { http.DefaultTransport = original })

	sc := &synth.SynthClient{
		Model:   liveMiniMaxModel,
		APIKey:  placeholder,
		BaseURL: "https://first.example/anthropic/v1",
		Wire:    synth.InferWire("https://first.example/anthropic/v1"),
		Timeout: 10 * time.Second,
	}

	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    liveMiniMaxModel,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the cross-origin redirect to be refused, got no error")
	}

	// The refusal must be TYPED: a decision this client made, not a
	// transport failure. A transport failure would be retried and
	// reported as "the endpoint is unreachable".
	if !errors.Is(err, synth.ErrCredentialRedirectRefused) {
		t.Fatalf("expected a typed credential-redirect refusal, got %v", err)
	}

	// The credential WAS presented to the configured origin — that is
	// correct, and it is what makes the refusal meaningful.
	if !firstOriginSawCredential.Load() {
		t.Error("the configured origin did not receive the credential; the test proves nothing")
	}
	// And it was NOT forwarded to the second origin.
	if secondOriginSawCredential.Load() {
		t.Error("the credential was forwarded to a second origin — the redirect guard did not hold")
	}

	t.Logf("  redirect refused             : %v", err)
	t.Logf("  first origin saw credential  : %t", firstOriginSawCredential.Load())
	t.Logf("  second origin reached        : %d time(s)", secondOriginHits.Load())
	t.Logf("  second origin saw credential : %t", secondOriginSawCredential.Load())
}
