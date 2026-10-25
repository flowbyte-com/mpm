// auth_refusal_status_test.go — the operator-facing status of a
// negotiated authentication refusal (issue #65).
//
// The bounded negotiation can make two requests, and each can be refused
// with a different status. Only the FINAL attempt describes the failure
// the operator has to act on:
//
//	401 → the credential is wrong      → rotate the key
//	402 → the account is unbilled       → pay
//	403 → the key is under-privileged   → request access
//
// The negotiated path reported the FIRST attempt's status, so an unbilled
// account was presented to the operator as an invalid credential. The
// remedy was completely wrong.
//
// These tests drive the REAL probe path against a local server with a
// synthetic credential.

package probe

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// negotiatedRefusalServer answers the FIRST scheme with firstStatus and
// a refusal that names the other header — so the bounded negotiation
// fires — and the second scheme with finalStatus.
func negotiatedRefusalServer(t *testing.T, firstStatus int, finalStatus int, finalBody string, attempts *atomic.Int32) {
	t.Helper()
	newRoutedProbeTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(finalStatus)
			_, _ = w.Write([]byte(finalBody))
			return
		}
		w.WriteHeader(firstStatus)
		_, _ = w.Write([]byte(`{"error":{"message":"authentication failed: please carry the API secret key in the Authorization field"}}`))
	}))
}

// TestProbe_NegotiatedSubscriptionIsNotReportedAsAnInvalidCredential pins
// that an unbilled account is reported as a subscription problem.
//
// Attempt #1 (X-Api-Key) → 401; attempt #2 (Bearer) → 402. The operator
// must be told to PAY, not to rotate a key.
func TestProbe_NegotiatedSubscriptionIsNotReportedAsAnInvalidCredential(t *testing.T) {
	var attempts atomic.Int32
	negotiatedRefusalServer(t, http.StatusUnauthorized, http.StatusPaymentRequired, `{"error":"payment required"}`, &attempts)

	p := &config.Profile{
		Name:     "unbilled",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "synthetic-unbilled-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if n := attempts.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2 (the original scheme plus exactly one bounded alternative)", n)
	}
	if res.AuthFailure != "subscription" {
		t.Errorf("auth failure = %q, want %q (the ALTERNATIVE attempt's 402 is the operative status)", res.AuthFailure, "subscription")
	}
}

// TestProbe_NegotiatedInvalidCredentialIsNotReportedAsAPermission pins
// the reverse direction.
//
// Attempt #1 (X-Api-Key) → 403; attempt #2 (Bearer) → 401. The operator
// must be told the credential is wrong, not that the key is
// under-privileged.
func TestProbe_NegotiatedInvalidCredentialIsNotReportedAsAPermission(t *testing.T) {
	var attempts atomic.Int32
	negotiatedRefusalServer(t, http.StatusForbidden, http.StatusUnauthorized, `{"error":"invalid api key"}`, &attempts)

	p := &config.Profile{
		Name:     "rotated",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "synthetic-rotated-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if n := attempts.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2 (the original scheme plus exactly one bounded alternative)", n)
	}
	if res.AuthFailure != "invalid_credential" {
		t.Errorf("auth failure = %q, want %q (the ALTERNATIVE attempt's 401 is the operative status)", res.AuthFailure, "invalid_credential")
	}
}

// TestProbe_NegotiatedServerErrorIsNotAnAuthenticationFailure pins that a
// 5xx on the alternative attempt is not reported as an authentication
// problem at all.
//
// Attempt #1 (X-Api-Key) → 401; attempt #2 (Bearer) → 500. The provider
// broke; the credential says nothing about it.
func TestProbe_NegotiatedServerErrorIsNotAnAuthenticationFailure(t *testing.T) {
	var attempts atomic.Int32
	negotiatedRefusalServer(t, http.StatusUnauthorized, http.StatusInternalServerError, `{"error":"upstream exploded"}`, &attempts)

	p := &config.Profile{
		Name:     "broken-upstream",
		Provider: "custom",
		Model:    "some-model",
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "synthetic-broken-upstream-key",
	}

	res := probeGenerative(context.Background(), p, ProbeKindGenerative)
	if n := attempts.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2 (the original scheme plus exactly one bounded alternative)", n)
	}
	if res.AuthFailure != "" {
		t.Errorf("auth failure = %q, want empty (a 5xx on the alternative attempt is not an authentication failure)", res.AuthFailure)
	}
	if res.Status == ProbeAuthFailed {
		t.Errorf("probe status = %q, want not auth_failed (the provider failed, not the credential)", res.Status)
	}
}
