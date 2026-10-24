// auth_negotiation_override_test.go — HAL-9001 issue #64.
//
// The bounded authentication negotiation never consulted the
// operator's explicit `auth_scheme` override. An operator who pinned
// `auth_scheme=none` — meaning "present NO credential to this
// endpoint" — got the credential transmitted anyway: the provider's
// refusal body named the "Authorization" field, and the negotiation
// proposed Bearer without ever asking whether the operator had
// forbidden credential presentation.
//
// That is the one fail-open in the change. Explicit operator intent
// must outrank an automatic heuristic, and the alternative attempt is
// the attempt that actually transmits.
//
// What these tests pin:
//
//   - an explicit override is NEVER negotiated: exactly one request,
//     presented with the pinned scheme;
//   - `auth_scheme=none` transmits no credential at all;
//   - without an override, the bounded alternative attempt still
//     happens (the existing behaviour must not regress).
//
// Every endpoint here is a local httptest server; no test reaches a
// public endpoint and no real credential is used.
package synth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// refusalEndpoint is a stand-in provider that refuses every request
// with the given status and body, and records what it saw.
//
// It is the evidence for "was the credential transmitted, and in which
// header" — the question issue #64 is about.
type refusalEndpoint struct {
	mu       sync.Mutex
	attempts int
	headers  []http.Header

	status int
	body   string
}

func (e *refusalEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.attempts++
	e.headers = append(e.headers, r.Header.Clone())
	e.mu.Unlock()

	status := e.status
	if status == 0 {
		status = http.StatusUnauthorized
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(e.body))
}

// snapshot returns the number of requests received and a copy of the
// headers of each.
func (e *refusalEndpoint) snapshot() (int, []http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]http.Header, len(e.headers))
	copy(out, e.headers)
	return e.attempts, out
}

// presentedCredential reports the (header name, header value) the
// endpoint was given, or ("", "") when no credential was presented.
func presentedCredential(h http.Header) (string, string) {
	if v := h.Get("Authorization"); v != "" {
		return "Authorization", v
	}
	if v := h.Get("X-Api-Key"); v != "" {
		return "X-Api-Key", v
	}
	return "", ""
}

// TestNegotiation_ExplicitOverrideIsNeverNegotiated is the headline
// regression test for issue #64.
//
// The operator's pinned scheme governs the request. A provider that
// names a DIFFERENT authentication header in its refusal is evidence
// about a mis-presented credential — but only when the operator left
// the choice to MPM. When the operator pinned the scheme themselves,
// MPM has no business choosing a different one.
func TestNegotiation_ExplicitOverrideIsNeverNegotiated(t *testing.T) {
	const secret = "operator-secret-must-not-leak"

	cases := []struct {
		name       string
		override   string
		refusal    string
		wantHeader string
		wantValue  string
	}{
		{
			// The headline case: "present no credential to this
			// endpoint" must not become "present the credential".
			name:       "none is never negotiated into an authenticated attempt",
			override:   "none",
			refusal:    `{"error":{"message":"Missing Authorization header"}}`,
			wantHeader: "",
			wantValue:  "",
		},
		{
			name:       "x-api-key is not negotiated into bearer",
			override:   "x-api-key",
			refusal:    `{"error":{"message":"Missing Authorization header"}}`,
			wantHeader: "X-Api-Key",
			wantValue:  secret,
		},
		{
			name:       "bearer is not negotiated into x-api-key",
			override:   "bearer",
			refusal:    `{"error":{"message":"the x-api-key header is required for this endpoint"}}`,
			wantHeader: "Authorization",
			wantValue:  "Bearer " + secret,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := &refusalEndpoint{body: tc.refusal}
			srv := httptest.NewServer(ep)
			defer srv.Close()

			sc := &SynthClient{
				Model:   "m",
				APIKey:  secret,
				BaseURL: srv.URL + "/v1",
				Wire:    wireAnthropic,
				Timeout: 5 * time.Second,
				// The operator pinned the scheme.
				AuthOverride: tc.override,
				// The connection verification was approved, so
				// negotiation WOULD be permitted without the pin.
				AllowAuthNegotiation: true,
			}

			_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
				"model":    "m",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if err == nil {
				t.Fatal("expected the refused request to fail")
			}

			attempts, headers := ep.snapshot()
			if attempts != 1 {
				t.Fatalf("the endpoint received %d requests, want 1 — an explicit auth_scheme override must never be negotiated away", attempts)
			}

			gotName, gotValue := presentedCredential(headers[0])
			if gotName != tc.wantHeader || gotValue != tc.wantValue {
				t.Errorf("presented credential = (%q, %q), want (%q, %q) — the pinned scheme governs the request",
					gotName, gotValue, tc.wantHeader, tc.wantValue)
			}

			// No request in the session may carry the credential in a
			// shape the operator did not authorise.
			for i, h := range headers {
				if tc.wantHeader != "Authorization" && strings.Contains(h.Get("Authorization"), secret) {
					t.Errorf("request %d carried the credential in Authorization (%q), which the operator did not authorise",
						i+1, h.Get("Authorization"))
				}
				if tc.wantHeader != "X-Api-Key" && strings.Contains(h.Get("X-Api-Key"), secret) {
					t.Errorf("request %d carried the credential in X-Api-Key (%q), which the operator did not authorise",
						i+1, h.Get("X-Api-Key"))
				}
			}
		})
	}
}

// TestNegotiation_WithoutAnOverrideTheBoundedAlternativeStillHappens
// pins that the correction does not disable negotiation itself.
//
// The bounded alternative attempt is the thing that makes an
// undiscoverable endpoint work at all. Suppressing it globally would
// trade one fail-open for a permanent authentication failure.
func TestNegotiation_WithoutAnOverrideTheBoundedAlternativeStillHappens(t *testing.T) {
	const secret = "gateway-secret"

	var attempts atomic.Int32
	var sawBearer atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") == "Bearer "+secret {
			sawBearer.Store(true)
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"please carry the API secret key in the Authorization field"}}`))
	}))
	defer srv.Close()

	sc := &SynthClient{
		Model:                "m",
		APIKey:               secret,
		BaseURL:              srv.URL + "/v1",
		Wire:                 wireAnthropic,
		Timeout:              5 * time.Second,
		AllowAuthNegotiation: true,
	}

	raw, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("DoLLMRequest: %v", err)
	}
	if !strings.Contains(string(raw), "OK READY") {
		t.Errorf("response body = %q, want the provider's completion", string(raw))
	}
	if !sawBearer.Load() {
		t.Error("the bounded alternative attempt did not present the bearer credential")
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2 (the original scheme plus exactly one bounded alternative)", n)
	}
}

// TestNegotiation_PinnedSchemeIsReportedInTheFailure pins that the
// refusal names the scheme that was actually presented.
//
// Without the scheme in the message, an operator who pinned a scheme
// cannot tell from the failure whether MPM honoured the pin — the
// exact ambiguity that made issue #64 hard to see from the outside.
func TestNegotiation_PinnedSchemeIsReportedInTheFailure(t *testing.T) {
	ep := &refusalEndpoint{body: `{"error":{"message":"Missing Authorization header"}}`}
	srv := httptest.NewServer(ep)
	defer srv.Close()

	sc := &SynthClient{
		Model:                "m",
		APIKey:               "secret",
		BaseURL:              srv.URL + "/v1",
		Wire:                 wireAnthropic,
		Timeout:              5 * time.Second,
		AuthOverride:         "none",
		AllowAuthNegotiation: true,
	}

	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the refused request to fail")
	}
	if !strings.Contains(err.Error(), "none") {
		t.Errorf("failure message = %q, want it to name the presented scheme %q", err.Error(), "none")
	}
}
