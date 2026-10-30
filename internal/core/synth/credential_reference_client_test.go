package synth

// credential_reference_client_test.go — the client refuses an
// UNRESOLVED credential before any request is made.
//
// WHY THIS IS A SEPARATE FILE
// ---------------------------
// credential_reference_test.go pins the RESOLUTION rule. This file pins
// what the client does with the result, and the observable difference
// is the important part:
//
//   - before: the placeholder went out on the wire, the provider
//     refused it, and the operator was told their credential was
//     invalid;
//   - now: the request never leaves the process, and the failure names
//     the configuration.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestClient_RefusesAnUnresolvableReferenceBeforeTheWire is the
// end-to-end guard.
//
// The provider must receive NOTHING: a placeholder presented as a
// bearer token is what produced the misleading "invalid credential"
// report in the first place.
//
// The endpoint is a local httptest server, so it is classified
// wireOpenAI and the credential variable it DECLARES is
// OAI_COMPAT_API_KEY. The reference names that variable, which is set to
// nothing — the "declared but unset" case, whose message must name the
// variable so the operator knows what to export.
//
// The endpoint is local on purpose: if the guard ever regressed, this
// test would then send a request to a loopback server, never to a real
// provider.
func TestClient_RefusesAnUnresolvableReferenceBeforeTheWire(t *testing.T) {
	clearCredentialEnv(t)

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	sc := NewClientForProfile(config.Profile{
		Name:    "default",
		Model:   "MiniMax-M3",
		BaseURL: srv.URL + "/v1",
		APIKey:  "$OAI_COMPAT_API_KEY", // declared by this endpoint, unset
	}, 5*time.Second, 16)

	if sc.CredentialError == nil {
		t.Fatal("the client did not record a credential error for an unresolvable reference")
	}
	if sc.APIKey != "" {
		t.Errorf("the client carries a credential (%q) for an unresolvable reference", sc.APIKey)
	}

	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "MiniMax-M3",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("the request succeeded; the credential was never resolved")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("the provider received %d request(s); an unresolved credential must never reach the wire", got)
	}
	if !strings.Contains(err.Error(), "OAI_COMPAT_API_KEY") {
		t.Errorf("the error does not name the variable to export: %v", err)
	}
}

// TestClient_AnUnresolvableReferenceIsNotAnAuthRefusal pins the
// distinction that matters for the operator.
//
// The failure must NOT be typed as an authentication refusal. An
// authentication refusal means "the endpoint answered and refused the
// credential", and the operator's remedy is to change the credential.
// Here the endpoint was never asked.
func TestClient_AnUnresolvableReferenceIsNotAnAuthRefusal(t *testing.T) {
	clearCredentialEnv(t)

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	sc := NewClientForProfile(config.Profile{
		Name:    "default",
		Model:   "MiniMax-M3",
		BaseURL: srv.URL + "/v1",
		APIKey:  "$OAI_COMPAT_API_KEY",
	}, 5*time.Second, 16)

	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "MiniMax-M3",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("the provider received %d request(s)", got)
	}
	var refusal *AuthRefusalError
	if asAuthRefusal(err, &refusal) {
		t.Errorf("the failure was typed as an authentication refusal (%v); the endpoint never answered, "+
			"so the credential was never judged", refusal)
	}
}

// TestClient_ResolvableReferencePresentsTheEnvironmentCredential is the
// positive half.
//
// The reference resolves, and the ENVIRONMENT's value is what reaches
// the wire — not the placeholder.
func TestClient_ResolvableReferencePresentsTheEnvironmentCredential(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "the-real-credential")

	var sawAuth atomic.Value
	sawAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	}))
	defer srv.Close()

	sc := NewClientForProfile(config.Profile{
		Name:    "default",
		Model:   "probe-model",
		BaseURL: srv.URL + "/v1",
		APIKey:  "$OAI_COMPAT_API_KEY", // this endpoint declares OAI_COMPAT_API_KEY
	}, 5*time.Second, 16)

	// The reference names a variable this endpoint does NOT declare
	// (it is OpenAI-compatible, so it declares OAI_COMPAT_API_KEY), so
	// the client must refuse rather than guess.
	if sc.CredentialError == nil {
		t.Fatal("a reference to a variable this endpoint does not declare was accepted")
	}

	// And the same reference against the endpoint that DOES declare it
	// resolves and reaches the wire.
	t.Setenv("OAI_COMPAT_API_KEY", "the-real-credential")
	sc = NewClientForProfile(config.Profile{
		Name:    "default",
		Model:   "probe-model",
		BaseURL: srv.URL + "/v1",
		APIKey:  "$OAI_COMPAT_API_KEY",
	}, 5*time.Second, 16)
	if sc.CredentialError != nil {
		t.Fatalf("a resolvable reference was refused: %v", sc.CredentialError)
	}

	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "probe-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	got, _ := sawAuth.Load().(string)
	if !strings.Contains(got, "the-real-credential") {
		t.Errorf("the wire carried %q, want the environment's credential — the placeholder must not "+
			"reach the provider", got)
	}
	if strings.Contains(got, "$OAI_COMPAT_API_KEY") {
		t.Errorf("the placeholder reached the wire: %q", got)
	}
}

// asAuthRefusal is errors.As specialised for *AuthRefusalError.
func asAuthRefusal(err error, target **AuthRefusalError) bool {
	for err != nil {
		if ar, ok := err.(*AuthRefusalError); ok {
			*target = ar
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
