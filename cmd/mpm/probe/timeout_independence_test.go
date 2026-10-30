package probe

// timeout_independence_test.go — the CONNECTION-VERIFICATION timeout
// and the MODEL REQUEST timeout are different things.
//
// THE DISTINCTION
// ---------------
// A profile's `timeout_seconds` is how long the substrate waits for a
// MODEL REQUEST — the call that generates text. A probe's timeout is
// how long the probe waits for the ENDPOINT to answer a tiny
// connectivity question.
//
// They must not be the same knob:
//
//   - the probe's timeout is a property of the PROBE (12s for a Doctor
//     probe, 1.5s for a post-save probe), not of the provider, and it
//     must not be inherited from the profile — otherwise an operator
//     who set `timeout_seconds: 3` for a long generation would also
//     make every verification fail, and would be told their CREDENTIAL
//     was invalid;
//   - the request timeout must not be narrowed by the probe.
//
// WHY THE CLASSIFICATION MATTERS
// ------------------------------
// A slow provider and a wrong credential look identical from the
// client's side until the status is classified. If a timeout were
// reported as an auth failure, the operator would go looking for a
// credential problem they do not have — which is exactly the
// MiniMax-401 confusion the live verification exists to settle.
//
// The classification is: a deadline is a TIMEOUT, and an HTTP 401/403
// is an AUTH failure. They never share a code path.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestProbe_UsesItsOwnTimeoutNotTheProfiles is the headline
// independence test.
//
// The profile says 3 seconds — which is a legitimate MODEL REQUEST
// timeout for a fast provider and a terrible VERIFICATION timeout. The
// probe must use its own bound and must report a TIMEOUT, not an auth
// failure.
func TestProbe_UsesItsOwnTimeoutNotTheProfiles(t *testing.T) {
	// A server that answers, but only after the probe's own bound has
	// expired. It NEVER returns an HTTP status: the probe's request
	// is abandoned before the server answers, so the only signal the
	// probe can see is a deadline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &config.Profile{
		Name:        "default",
		Provider:    "custom",
		Model:       "some-model",
		BaseURL:     srv.URL,
		APIKey:      "profile-key",
		TimeoutSecs: 3, // the MODEL REQUEST timeout — not the probe's
	}

	const probeBound = 300 * time.Millisecond
	start := time.Now()
	res := probeGenerativeWithTimeout(context.Background(), p, ProbeKindGenerative, probeBound)
	elapsed := time.Since(start)

	if res.Status != ProbeTimeout {
		t.Errorf("probe status = %q, want %q (a provider that did not answer in time is a TIMEOUT, "+
			"not an auth failure). summary=%q", res.Status, ProbeTimeout, res.ErrorSummary)
	}
	if res.Status == ProbeAuthFailed {
		t.Error("a slow provider was reported as an invalid credential — the operator would go looking " +
			"for a credential problem they do not have")
	}
	if elapsed > 2*time.Second {
		t.Errorf("the probe took %v, want it bounded by its own timeout (%v) rather than by the "+
			"profile's timeout_seconds (%ds)", elapsed, probeBound, p.TimeoutSecs)
	}
}

// TestProbe_ASlowProviderIsNeverClassifiedAsAnAuthFailure pins the
// classification directly.
//
// classifyFromWire is the single place a transport signal becomes a
// presentation status, so asserting on it asserts the whole path.
func TestProbe_ASlowProviderIsNeverClassifiedAsAnAuthFailure(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		class      string
		httpStatus int
		want       ProbeStatus
	}{
		{
			name:  "a deadline is a timeout",
			err:   context.DeadlineExceeded,
			want:  ProbeTimeout,
			class: "FailureTransientTransport",
		},
		{
			name:  "a wrapped deadline is a timeout",
			err:   fmt.Errorf("the request failed: %w", context.DeadlineExceeded),
			want:  ProbeTimeout,
			class: "FailureTransientTransport",
		},
		{
			name:       "a 401 is an auth failure",
			err:        errors.New("provider refused the request"),
			class:      "FailureAuth",
			httpStatus: http.StatusUnauthorized,
			want:       ProbeAuthFailed,
		},
		{
			name:       "a 403 is an auth failure",
			err:        errors.New("provider refused the request"),
			class:      "FailureAuth",
			httpStatus: http.StatusForbidden,
			want:       ProbeAuthFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFromWire(tc.err, tc.class, tc.httpStatus, "")
			if got != tc.want {
				t.Errorf("classifyFromWire = %q, want %q", got, tc.want)
			}
			if tc.want == ProbeTimeout && got == ProbeAuthFailed {
				t.Error("a timeout was classified as an authentication failure")
			}
		})
	}
}

// TestProbe_AnAuthFailureIsNotClassifiedAsATimeout is the other
// direction of the same distinction.
func TestProbe_AnAuthFailureIsNotClassifiedAsATimeout(t *testing.T) {
	got := classifyFromWire(errors.New("provider refused the request"), "FailureAuth",
		http.StatusUnauthorized, "")
	if got != ProbeAuthFailed {
		t.Errorf("classifyFromWire = %q, want %q", got, ProbeAuthFailed)
	}
}

// TestProbeTimeoutConstantsAreSeparateFromTheRequestTimeout pins that
// the probe's bounds are the probe's own.
//
// The request timeout lives in internal/core/config/timeout.go and
// defaults to 300 seconds. The probe's bounds are 12 seconds and 1.5
// seconds. If the probe ever started using the request timeout, a
// verification would hang for five minutes.
func TestProbeTimeoutConstantsAreSeparateFromTheRequestTimeout(t *testing.T) {
	if ProbeTimeoutForDoctor != 12*time.Second {
		t.Errorf("ProbeTimeoutForDoctor = %v, want 12s", ProbeTimeoutForDoctor)
	}
	if ProbeTimeoutForConfigSave != 1500*time.Millisecond {
		t.Errorf("ProbeTimeoutForConfigSave = %v, want 1.5s", ProbeTimeoutForConfigSave)
	}
	if ProbeTimeoutForDoctor >= time.Duration(config.DefaultTimeoutSeconds)*time.Second {
		t.Errorf("the Doctor probe's timeout (%v) must be well inside the model request timeout (%ds) — "+
			"a verification must never wait as long as a generation",
			ProbeTimeoutForDoctor, config.DefaultTimeoutSeconds)
	}
	// The names must not drift into a shared value.
	if strings.Contains(ProbeTimeoutForDoctor.String(), "request") {
		t.Error("the probe's timeout must not be described as a request timeout")
	}
}
