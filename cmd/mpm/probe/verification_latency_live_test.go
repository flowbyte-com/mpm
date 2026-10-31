package probe

// verification_latency_live_test.go — MEASURE the live provider's
// latency through the canonical probe path.
//
// WHY THIS FILE EXISTS
// --------------------
// The connection-verification bound (ProbeTimeoutForVerification) is a
// MEASUREMENT, not a preference. A verification bound shorter than the
// provider's own latency turns a working credential into a reported
// failure — and on a credential surface an operator reads a timeout as
// "my key is wrong".
//
// That is not hypothetical: the bound this replaced was 1.5 seconds, and
// the live MiniMax surface's slowest observed call was 4761ms. A valid
// credential was reported as a timeout.
//
// This file is the re-runnable measurement. It is what a future agent
// should run BEFORE changing the bound — and it is how the numbers in
// the bound's comment were obtained:
//
//	MiniMax-M3     min  415ms   median 560-700ms   max 4761ms
//	MiniMax-M2.7   min  444ms   median 603-1007ms  max 2220ms
//
// IT IS EXPLICITLY OPT-IN
// -----------------------
// It reaches a REAL provider and presents the operator's credential, so
// it runs only when the operator has asked for it for this run:
//
//	MPM_TEST_LIVE_PROVIDER=1 \
//	  CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
//	  go test -tags fts5 -v ./cmd/mpm/probe/ -run TestLive_VerificationLatency
//
// See internal/testenv/liveprovider.go for why the credential's presence
// is NOT sufficient as a gate: the ordinary `make test` / `make test-race`
// gates must never reach the network.
//
// THE CREDENTIAL IS NEVER TOUCHED BY THIS TEST
// --------------------------------------------
// The profiles below carry NO api_key. The credential is resolved by the
// production resolver (synth.ResolveCredential), which reads the
// provider's own environment variable. The secret therefore never passes
// through a variable, a literal, a struct field or a format string in
// this file, and there is nothing here that could print it.
//
// THE BOUNDS
// ----------
// Each measurement makes ONE request. The measurement is a latency
// measurement, and a latency measurement that silently retried would be
// measuring something other than what it claims.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm/internal/testenv"
)

// liveLatencyCase is one model to measure against the live provider.
type liveLatencyCase struct {
	model string
	base  string
}

// TestLive_VerificationLatency measures the provider's latency through
// the canonical probe path.
//
// It REPORTS the distribution — min, median, max — and, when the
// verification bound does not clear the slowest observed healthy call,
// it FAILS. That assertion is the point: it is the check that a bound
// change cannot quietly reintroduce a false-failure.
func TestLive_VerificationLatency(t *testing.T) {
	if !testenv.LiveProviderEnabledForKeys(t, "MINIMAX_API_KEY") {
		t.Skip("the live provider verification is not enabled for this run: " +
			"set MPM_TEST_LIVE_PROVIDER=1 and provide MINIMAX_API_KEY to run it")
	}

	const samples = 7
	cases := []liveLatencyCase{
		{model: "MiniMax-M3", base: "https://api.minimax.io/anthropic/v1"},
		{model: "MiniMax-M2.7", base: "https://api.minimax.io/anthropic/v1"},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			var latencies []time.Duration
			healthy := 0
			for i := 0; i < samples; i++ {
				p := &config.Profile{
					Name:     "default",
					Provider: "custom",
					Model:    tc.model,
					BaseURL:  tc.base,
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				res := probeGenerativeWithTimeout(ctx, p, ProbeKindGenerative, ProbeTimeoutForVerification)
				cancel()
				t.Logf("  run %d/%d: status=%v latency=%dms", i+1, samples, res.Status, res.LatencyMs)

				switch res.Status {
				case ProbeHealthy:
					healthy++
					latencies = append(latencies, time.Duration(res.LatencyMs)*time.Millisecond)
				case ProbeInvalidResponse:
					// The provider sometimes answers with a THINKING-ONLY
					// reply, which the production parser reports as an
					// empty completion. That is a provider behaviour, not
					// a credential fault and not a latency fault — but the
					// request DID complete in that time, so the latency
					// is still a valid data point for "how long does this
					// provider take".
					//
					// It is recorded rather than discarded, because
					// discarding the slowest responses would be exactly
					// the error this measurement exists to prevent.
					t.Logf("    (the provider returned a thinking-only reply; the latency is still a valid data point)")
					latencies = append(latencies, time.Duration(res.LatencyMs)*time.Millisecond)
				default:
					t.Fatalf("the provider did not accept a valid credential: status=%q error_class=%q summary=%q",
						res.Status, res.ErrorClass, res.ErrorSummary)
				}
			}

			if healthy == 0 {
				t.Fatalf("no call reached the healthy state; the measurement is empty")
			}
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			min := latencies[0]
			median := latencies[len(latencies)/2]
			max := latencies[len(latencies)-1]
			t.Logf("MEASURED %s: min=%v median=%v max=%v (n=%d, healthy=%d)", tc.model, min, median, max, len(latencies), healthy)

			// The bound MUST clear the slowest observed call, or a valid
			// credential is reported as a timeout.
			if ProbeTimeoutForVerification <= max {
				t.Errorf("the verification bound (%v) does not clear the slowest observed call "+
					"(%v) — a valid credential would be reported as a timeout", ProbeTimeoutForVerification, max)
			}
		})
	}
}
