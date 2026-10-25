// redirect_refusal_reporting_test.go — the REPORTING contract for a
// refused credential-bearing redirect (issue #67).
//
// The behavioural contract (what is allowed, what is refused, what is
// retried) lives in redirect_guard_test.go. This file pins the part that
// needs the API this change introduces:
//
//   - a refusal is a TYPED outcome (ErrCredentialRedirectRefused), so
//     callers can recognise it without matching on message text;
//   - a refusal has its OWN failure class
//     (FailureCredentialRedirectRefused) — it is not a transient
//     transport failure and it is not the zero value;
//   - the failure carries NO HTTP status, so nothing downstream can
//     report it as "the endpoint is unreachable".

package synth

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestRedirectRefusal_CarriesTheTypedSentinel pins that the refusal is
// recognisable through errors.Is, all the way out of DoLLMRequest.
//
// The error crosses two layers on its way out: the http.Client wraps the
// CheckRedirect error in a *url.Error, and the synth layer wraps that
// again. If either layer stopped preserving the chain, every caller
// would have to match on message text — the exact fragility that made
// the original defect hard to classify.
func TestRedirectRefusal_CarriesTheTypedSentinel(t *testing.T) {
	router := newHostRouter()
	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.example.com/v1/messages", http.StatusTemporaryRedirect)
	}))
	newRedirectServer(t, router, "other.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the cross-origin redirect to be refused")
	}
	if !errors.Is(err, ErrCredentialRedirectRefused) {
		t.Errorf("error does not carry ErrCredentialRedirectRefused: %v", err)
	}
}

// TestRedirectRefusal_HasItsOwnFailureClass pins that a refusal is
// classified as a refusal.
//
// It must not be FailureTransientTransport (that class consumes the
// recovery slot and repeats the request) and it must not be the zero
// value (FailureUnknown reads as "we could not tell what happened").
func TestRedirectRefusal_HasItsOwnFailureClass(t *testing.T) {
	router := newHostRouter()
	newRedirectServer(t, router, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.example.com/v1/messages", http.StatusTemporaryRedirect)
	}))
	newRedirectServer(t, router, "other.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	router.install(t)

	sc := redirectClient("http://api.example.com/v1")
	plan := NewPerCallPlan()
	_, err := sc.DoLLMRequestWithPlan(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, plan, AttemptFresh)
	if err == nil {
		t.Fatal("expected the cross-origin redirect to be refused")
	}
	stats := plan.Stats()
	if stats.LastFailureClass != FailureCredentialRedirectRefused {
		t.Errorf("failure class = %v, want FailureCredentialRedirectRefused", stats.LastFailureClass)
	}
	if stats.RetryCalls != 0 {
		t.Errorf("retry calls = %d, want 0 (a refusal must not consume the recovery slot)", stats.RetryCalls)
	}
	if stats.ActualProviderCalls != 1 {
		t.Errorf("provider calls = %d, want 1 (a refusal must not repeat the request)", stats.ActualProviderCalls)
	}
	if FailureCredentialRedirectRefused.String() != "credential_redirect_refused" {
		t.Errorf("FailureCredentialRedirectRefused.String() = %q, want credential_redirect_refused",
			FailureCredentialRedirectRefused.String())
	}
}
