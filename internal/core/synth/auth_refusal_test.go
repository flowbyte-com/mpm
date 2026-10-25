// auth_refusal_test.go — hermetic regression tests for the status a
// negotiated authentication refusal reports (issue #65).
//
// The bounded negotiation can make TWO requests, and each can be refused
// with a different status. Only the FINAL attempt describes the failure
// the operator has to act on.
//
// The negotiated path reported the FIRST attempt's status and discarded
// the second one's, so:
//
//	attempt #1 (X-Api-Key) → 401 {"error":"invalid api key"}
//	attempt #2 (Bearer)    → 402 {"error":"payment required"}
//
// was reported to the operator as an INVALID CREDENTIAL. The account was
// in fact UNBILLED. The remedies are completely different: paying, not
// rotating a key.
//
// This file uses only the pre-existing public surface, so it compiles
// against the behaviour it guards. The typed-error contract lives in
// auth_refusal_typed_test.go.
//
// Every server here is a local httptest server and every credential is
// synthetic.

package synth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// negotiationServer answers the first scheme with a refusal that names
// the other header (so the bounded negotiation fires) and the second
// scheme with finalStatus.
func negotiationServer(t *testing.T, finalStatus int, finalBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(finalStatus)
			_, _ = w.Write([]byte(finalBody))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"authentication failed: please carry the API secret key in the Authorization field"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNegotiatedRefusal_MessageNamesBothStatuses pins that the operator
// can see what the provider said on the SECOND attempt.
//
// The first attempt's status alone is not enough: it describes the
// refusal MPM provoked by presenting the credential in the wrong form,
// not the state the operator is actually in.
func TestNegotiatedRefusal_MessageNamesBothStatuses(t *testing.T) {
	srv := negotiationServer(t, http.StatusPaymentRequired, `{"error":"payment required"}`)

	sc := &SynthClient{
		Model:   "m",
		APIKey:  "synthetic-negotiation-key-both-statuses",
		BaseURL: srv.URL + "/v1",
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
		// The probe is the explicitly approved connection verification.
		AllowAuthNegotiation: true,
	}
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the refusal to be reported as an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("the message does not name the first attempt's status (401): %v", err)
	}
	if !strings.Contains(msg, "402") {
		t.Errorf("the message does not name the alternative attempt's status (402): %v", err)
	}
	if !strings.Contains(msg, "payment required") {
		t.Errorf("the message does not carry the provider's second answer: %v", err)
	}
}

// TestNegotiatedRefusal_TransportFailureOnTheAlternativeIsNotAnAuthRefusal
// pins that a transport failure on the alternative attempt is not
// reported as an authentication refusal.
func TestNegotiatedRefusal_TransportFailureOnTheAlternativeIsNotAnAuthRefusal(t *testing.T) {
	srv := negotiationServer(t, http.StatusInternalServerError, `{"error":"upstream exploded"}`)

	sc := &SynthClient{
		Model:   "m",
		APIKey:  "synthetic-negotiation-key-5xx",
		BaseURL: srv.URL + "/v1",
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
		AllowAuthNegotiation: true,
	}
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the refusal to be reported as an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("the message does not name the alternative attempt's status (500): %v", err)
	}
}
