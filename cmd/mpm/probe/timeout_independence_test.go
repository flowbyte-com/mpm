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
//   - the probe's timeout is a property of the PROBE (15s for every
//     connection verification), not of the provider, and it must not be
//     inherited from the profile — otherwise an operator who set
//     `timeout_seconds: 3` for a long generation would also make every
//     verification fail, and would be told their CREDENTIAL was
//     invalid;
//   - the request timeout must not be narrowed by the probe.
//
// The 12s bound is ONE number for every verification surface. It used to
// be two: 12s for Doctor, 1.5s for the post-save verification. The
// 1.5s bound was shorter than the provider's own median latency, so a
// VALID credential was reported as a timeout — see
// TestVerificationBoundExceedsTheMeasuredProviderLatency.
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
// defaults to 300 seconds. The verification bound is 15 seconds. If the
// probe ever started using the request timeout, a verification would
// hang for five minutes.
//
// It also pins the ONE-bound rule: there is a single verification
// timeout, shared by every connection-verification surface. Two
// surfaces with two bounds is how the post-save probe came to carry a
// 1.5s bound — shorter than the provider's own median latency, so a
// VALID credential was reported as a timeout.
func TestProbeTimeoutConstantsAreSeparateFromTheRequestTimeout(t *testing.T) {
	if ProbeTimeoutForVerification != 15*time.Second {
		t.Errorf("ProbeTimeoutForVerification = %v, want 15s", ProbeTimeoutForVerification)
	}
	if ProbeTimeoutForVerification >= time.Duration(config.DefaultTimeoutSeconds)*time.Second {
		t.Errorf("the verification timeout (%v) must be well inside the model request timeout (%ds) — "+
			"a verification must never wait as long as a generation",
			ProbeTimeoutForVerification, config.DefaultTimeoutSeconds)
	}
	// The names must not drift into a shared value.
	if strings.Contains(ProbeTimeoutForVerification.String(), "request") {
		t.Error("the probe's timeout must not be described as a request timeout")
	}
}

// TestVerificationBudgetExceedsThePerRequestBound is the structural
// guard for the relationship between the two bounds.
//
// The per-request bound is what a single request is allowed to take. The
// total budget is the ceiling on the WHOLE verification, across the
// first attempt and the one bounded recovery attempt it may make.
//
// The budget MUST exceed the per-request bound, and it must be the OUTER
// deadline. If it were shorter, the outer deadline would fire first and
// the probe would report a bare context deadline instead of the
// provider's own client timeout — the same failure wearing a different
// message, and indistinguishable from it where it is rendered.
func TestVerificationBudgetExceedsThePerRequestBound(t *testing.T) {
	if ProbeTotalBudget <= ProbeTimeoutForVerification {
		t.Fatalf("ProbeTotalBudget (%v) must exceed ProbeTimeoutForVerification (%v) — "+
			"the outer deadline must never fire before the per-request bound",
			ProbeTotalBudget, ProbeTimeoutForVerification)
	}
	// Two attempts, plus the backoff between them, must fit inside the
	// budget. If they did not, the budget would truncate the recovery
	// attempt and the probe would report a context deadline in place of
	// whatever the provider actually said.
	const attempts = 2
	if ProbeTotalBudget < attempts*ProbeTimeoutForVerification {
		t.Errorf("ProbeTotalBudget (%v) must cover %d attempts of %v",
			ProbeTotalBudget, attempts, ProbeTimeoutForVerification)
	}
}

// TestVerificationBoundExceedsTheMeasuredProviderLatency is the
// regression guard for the defect this bound was chosen to close.
//
// The live MiniMax Anthropic-compatible surface was measured through
// this canonical probe path (7 requests per model, three separate runs):
//
//	MiniMax-M3     min  415ms   median 560-700ms   max 4761ms
//	MiniMax-M2.7   min  444ms   median 603-1007ms  max 2220ms
//
// The post-save verification's bound used to be 1500ms — a THIRD of the
// slowest observed call. A valid credential was therefore reported as
// a timeout, which is what an operator reads as "my credential is
// wrong".
//
// The bound must clear the worst observed call with room to spare. This
// is a constant assertion with the measurement in the comment: it cannot
// detect a provider that gets slower, and it does not pretend to. What
// it CAN do is stop a future edit from quietly lowering the bound back
// under the latency it was measured against.
//
// The live measurement that produced these numbers is
// TestLive_VerificationLatency, in verification_latency_live_test.go —
// re-run it before changing this bound.
func TestVerificationBoundExceedsTheMeasuredProviderLatency(t *testing.T) {
	const worstObservedLatency = 4761 * time.Millisecond
	if ProbeTimeoutForVerification <= worstObservedLatency {
		t.Errorf("ProbeTimeoutForVerification = %v, want it well above the worst observed latency "+
			"(%v) — a valid credential must not be reported as a timeout",
			ProbeTimeoutForVerification, worstObservedLatency)
	}
	// And with room to spare, not merely above it: a bound that clears
	// the observed maximum by a hair is one provider hiccup away from
	// producing a false failure again.
	if ProbeTimeoutForVerification < 2*worstObservedLatency {
		t.Errorf("ProbeTimeoutForVerification = %v clears the worst observed latency (%v) by less than "+
			"2x; the margin is too thin to survive a slower day",
			ProbeTimeoutForVerification, worstObservedLatency)
	}
}
