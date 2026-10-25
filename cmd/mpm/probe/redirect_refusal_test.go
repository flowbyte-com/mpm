// redirect_refusal_test.go — the operator-facing classification of a
// refused credential-bearing redirect (issue #67).
//
// A deliberate refusal is NOT a reachability problem. The endpoint
// answered — MPM declined to follow the redirect because it would have
// carried the credential off the configured origin.
//
// The first version of the guard reported the refusal as a transport
// failure, and the probe's classifier turned any error with no HTTP
// status into "endpoint unreachable". The operator was told the
// provider was down when the provider was in fact answering, and the
// guard's own explanation never reached them.
//
// This test drives the REAL production path (a local redirecting server
// → synth.DoLLMRequest → the probe's classifier) so it pins the whole
// chain rather than a hand-built error string.

package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/synth"
)

// TestProbe_RedirectRefusalIsNotReportedAsUnreachable pins that a
// refused redirect reaches the operator as a refusal.
func TestProbe_RedirectRefusalIsNotReportedAsUnreachable(t *testing.T) {
	// Two local servers on two ports: a different port is a different
	// origin, which is exactly what the guard must refuse.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	sc := &synth.SynthClient{
		Model:   "m",
		APIKey:  "synthetic-credential-must-not-leak",
		BaseURL: redirector.URL + "/v1",
		Timeout: 5 * time.Second,
	}
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the cross-origin redirect to be refused")
	}

	class, httpStatus := classifySynthError(err)
	status := classifyFromWire(err, class, httpStatus, "")

	if status == ProbeUnreachable {
		t.Errorf("a deliberate redirect refusal was reported as \"unreachable\"; the endpoint answered and MPM refused to follow the redirect (class=%q)", class)
	}
	if status != ProbeRedirectRefused {
		t.Errorf("probe status = %v, want ProbeRedirectRefused (class=%q, status=%d)", status, class, httpStatus)
	}
	if class != "FailureCredentialRedirectRefused" {
		t.Errorf("failure class = %q, want FailureCredentialRedirectRefused", class)
	}
	if httpStatus != 0 {
		t.Errorf("http status = %d, want 0 (a refusal is not an HTTP response)", httpStatus)
	}
	// The refusal's own explanation must survive to the operator.
	if !strings.Contains(err.Error(), "the credential would leave the configured origin") {
		t.Errorf("the refusal's explanation does not reach the operator: %v", err)
	}
}

// TestProbeStatusString_IncludesRedirectRefused pins the presentation
// label. The status string is persisted in the probe cache and rendered
// by the dashboard, so it must be stable.
func TestProbeStatusString_IncludesRedirectRefused(t *testing.T) {
	if got := ProbeRedirectRefused.String(); got != "redirect_refused" {
		t.Errorf("ProbeRedirectRefused.String() = %q, want %q", got, "redirect_refused")
	}
}
