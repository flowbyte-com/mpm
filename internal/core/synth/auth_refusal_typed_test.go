// auth_refusal_typed_test.go — the typed contract for a negotiated
// authentication refusal (issue #65).
//
// The HTTP status travels with the error as a typed field rather than
// being parsed back out of a formatted message. Parsing a status out of
// a message is exactly what made the production classifier and the
// probe's duplicate drift apart, and it is why the negotiated path
// reported the FIRST attempt's status.

package synth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNegotiatedRefusal_CarriesTheFinalStatus pins that the typed error
// carries the status of the attempt that produced the failure, and
// keeps the first attempt's status alongside it.
func TestNegotiatedRefusal_CarriesTheFinalStatus(t *testing.T) {
	srv := negotiationServer(t, http.StatusPaymentRequired, `{"error":"payment required"}`)

	sc := &SynthClient{
		Model:   "m",
		APIKey:  "synthetic-negotiation-key-typed",
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
	var refusal *AuthRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("error does not carry *AuthRefusalError: %v", err)
	}
	if refusal.Status != http.StatusPaymentRequired {
		t.Errorf("Status = %d, want %d (the ALTERNATIVE attempt's status is the operative one)",
			refusal.Status, http.StatusPaymentRequired)
	}
	if refusal.InitialStatus != http.StatusUnauthorized {
		t.Errorf("InitialStatus = %d, want %d (the first attempt's status is kept alongside)",
			refusal.InitialStatus, http.StatusUnauthorized)
	}
	if !refusal.HasAlternativeAttempt() {
		t.Error("HasAlternativeAttempt() = false, want true")
	}
	if refusal.InitialScheme != AuthSchemeXAPIKey {
		t.Errorf("InitialScheme = %q, want %q", refusal.InitialScheme, AuthSchemeXAPIKey)
	}
	if refusal.Scheme != AuthSchemeBearer {
		t.Errorf("Scheme = %q, want %q", refusal.Scheme, AuthSchemeBearer)
	}
	// The class the operator-facing classifier reads is the FINAL
	// attempt's class. Both attempts were authentication refusals here,
	// so the class is FailureAuth either way — but it is the final
	// attempt's status that carries the billing meaning.
	if refusal.Class != FailureAuth {
		t.Errorf("Class = %v, want %v", refusal.Class, FailureAuth)
	}
}

// TestDirectRefusal_CarriesTheStatus pins that a single refused request
// also carries its status as a typed field.
func TestDirectRefusal_CarriesTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":"payment required"}`))
	}))
	defer srv.Close()

	sc := &SynthClient{
		Model:   "m",
		APIKey:  "synthetic-direct-refusal-key",
		BaseURL: srv.URL + "/v1",
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
	}
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the refusal to be reported as an error")
	}
	var refusal *AuthRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("error does not carry *AuthRefusalError: %v", err)
	}
	if refusal.Status != http.StatusPaymentRequired {
		t.Errorf("Status = %d, want %d", refusal.Status, http.StatusPaymentRequired)
	}
	if refusal.HasAlternativeAttempt() {
		t.Error("HasAlternativeAttempt() = true, want false (no alternative attempt was made)")
	}
}
