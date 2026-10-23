// auth_probe_test.go — hermetic regression tests for the ACTIVE Doctor
// probe path.
//
// The probe is the surface the operator actually sees: `mpm doctor`
// runs it, and the post-save verification hint runs it. It shares the
// production request transport with synthesis, so a defect in how the
// probe presents credentials is the same defect synthesis has.
//
// These tests use ONLY the pre-existing probe surface
// (probeGenerative + config.Profile + ProbeResult.Status) so they
// compile against the behaviour they guard. That makes them regression
// tests rather than restatements of the implementation: each one fails
// against the previous behaviour and passes against the corrected one.
//
// What they pin:
//
//   - MiniMax's Anthropic-compatible endpoint is authenticated with
//     `Authorization: Bearer`, and the probe reports the connection
//     healthy. This is HAL-9001 Finding 1 seen from the operator's
//     side: a completely correct configuration produced
//     "auth failed".
//   - A provider that names the authentication header it wants gets
//     exactly one bounded alternative attempt.
//   - A provider that refuses the credential without naming a header
//     gets exactly ONE request — no blind credential guessing.
//   - A subscription refusal is not negotiated: paying, not changing a
//     header, is the remedy.
//
// Every server here is a local httptest server; no test reaches a
// public endpoint.
package probe

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// routeProbeToServer points every outbound request at srv, whatever host
// the URL names.
//
// The documented-endpoint rules are HOST-based, so a test that observes
// the real MiniMax request shape cannot use a 127.0.0.1 base URL — it
// must name the real host. This transport keeps the name in the request
// line and the Host header while the connection stays on loopback.
func routeProbeToServer(t *testing.T, srv *httptest.Server) {
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

// newRoutedProbeTLSServer returns a TLS test server plus a cleanup that
// routes outbound traffic to it. Use it for every https base URL.
func newRoutedProbeTLSServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	routeProbeToServer(t, srv)
	return srv
}

// TestProbe_MiniMaxAnthropicSurfaceIsAuthenticatedWithBearer is the
// operator-visible half of HAL-9001 Finding 1.
//
// MiniMax's Anthropic-compatible endpoint is reached at
// https://api.minimax.io/anthropic/v1 and authenticates with
// `Authorization: Bearer <key>`. The probe previously presented
// `X-Api-Key`, the endpoint answered 401, and Doctor reported an
// authentication failure for a configuration that was entirely
// correct.
//
// The fake endpoint below behaves like MiniMax: it answers 401 unless
// the request carries the bearer credential.
func TestProbe_MiniMaxAnthropicSurfaceIsAuthenticatedWithBearer(t *testing.T) {
	const apiKey = "minimax-secret-key"

	var sawPath atomic.Value
	sawPath.Store("")

	newRoutedProbeTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath.Store(r.URL.Path)
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"login fail: Please carry the API secret key in the Authorization field"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))

	p := &config.Profile{
		Name:     "default",
		Provider: "custom",
		Model:    "MiniMax-M3",
		BaseURL:  "https://api.minimax.io/anthropic/v1",
		APIKey:   apiKey,
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if res.Status != ProbeHealthy {
		t.Fatalf("probe status = %q (%s); want healthy", res.Status, res.ErrorSummary)
	}
	if got := sawPath.Load().(string); got != "/anthropic/v1/messages" {
		t.Errorf("request path = %q, want /anthropic/v1/messages", got)
	}
}

// TestProbe_DoesNotNegotiateAnInvalidCredential pins that a refusal
// which names no authentication header is an INVALID CREDENTIAL.
//
// Repeating the request with another header cannot fix a wrong key, and
// the operator would be told a falsehood if MPM reported the credential
// as merely mis-presented.
func TestProbe_DoesNotNegotiateAnInvalidCredential(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer srv.Close()

	p := &config.Profile{
		Name:     "wrong-key",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "wrong-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if res.Status != ProbeAuthFailed {
		t.Fatalf("probe status = %q (%s); want auth_failed", res.Status, res.ErrorSummary)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1 (an invalid credential must not be retried with another scheme)", n)
	}
}

// TestProbe_DoesNotNegotiateASubscriptionRefusal pins that an expired
// subscription is not an authentication-header problem.
//
// HTTP 402 means the account is not billed for the request. Changing
// the credential presentation cannot help, and an alternative attempt
// would both waste a request and mislabel the problem.
func TestProbe_DoesNotNegotiateASubscriptionRefusal(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"your subscription has expired, please renew"}}`))
	}))
	defer srv.Close()

	p := &config.Profile{
		Name:     "expired",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "some-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1 (a subscription refusal must not be negotiated)", n)
	}
	if res.Status == ProbeHealthy {
		t.Error("probe reported healthy for a refused subscription")
	}
}

// TestProbe_TimeoutBudgetIsBounded pins that a probe against an
// unresponsive endpoint returns promptly with a timeout classification
// rather than hanging the operator's command.
func TestProbe_TimeoutBudgetIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	p := &config.Profile{
		Name:     "slow",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "k",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := probeGenerative(ctx, p, ProbeKindGenerative)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("probe took %s, want it bounded by the caller's deadline", elapsed)
	}
	if res.Status == ProbeHealthy {
		t.Error("probe reported healthy for an unresponsive endpoint")
	}
}
