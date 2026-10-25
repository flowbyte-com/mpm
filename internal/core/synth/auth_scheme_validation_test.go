// auth_scheme_validation_test.go — hermetic regression tests for the
// authentication-scheme configuration correctness (issue #68).
//
// An operator's explicit auth_scheme must never be SILENTLY IGNORED.
//
// The override used to be parsed with a matcher that returned ("", false)
// for anything it did not recognise, and the resolution simply moved on
// to the next tier. A hand-edited mpm_config.json carrying
// `auth_scheme: "basic"` — or a plain typo — was therefore dropped in
// silence: the endpoint was authenticated with a DERIVED scheme, it
// answered 401, and the symptom pointed in exactly the wrong direction.
//
// The scheme vocabulary lives in the config package so that the
// configuration layer can validate it; see
// internal/core/config/auth_scheme.go.
//
// Every credential here is synthetic and no request leaves the process
// except to a local test server.

package synth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestInvalidAuthOverride_RefusesTheRequest pins that a request against
// a profile with an unrecognised auth_scheme fails with an explicit
// error — and that NO credential is transmitted.
//
// The refusal happens before the request is built, so an endpoint that
// would happily accept the credential never sees it.
func TestInvalidAuthOverride_RefusesTheRequest(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK READY"}]}`))
	}))
	defer srv.Close()

	sc := &SynthClient{
		Model:        "m",
		APIKey:       "synthetic-credential-must-not-be-sent",
		BaseURL:      srv.URL + "/v1",
		Wire:         wireAnthropic,
		Timeout:      5 * time.Second,
		AuthOverride: "basic",
	}
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected the unrecognised auth_scheme to be refused")
	}
	if reached {
		t.Error("a request was made despite the unrecognised auth_scheme")
	}
	msg := err.Error()
	if !strings.Contains(msg, "basic") {
		t.Errorf("the refusal does not name the offending value: %v", err)
	}
	for _, want := range []string{"bearer", "x-api-key", "none"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name the supported scheme %q: %v", want, err)
		}
	}
}

// TestInvalidAuthOverride_DoesNotFallThroughToADerivedScheme pins that
// an unrecognised override is not replaced by a derived scheme.
//
// The derived scheme for an Anthropic-protocol endpoint is X-Api-Key.
// A silent fall-through would present the credential that way, and the
// operator would have no way to know their instruction was dropped.
func TestInvalidAuthOverride_DoesNotFallThroughToADerivedScheme(t *testing.T) {
	sc := &SynthClient{
		BaseURL:      "https://api.anthropic.com/v1",
		APIKey:       "synthetic-credential",
		Wire:         wireAnthropic,
		AuthOverride: "not-a-scheme",
	}
	got, err := sc.resolveAuth()
	if err == nil {
		t.Fatalf("resolveAuth accepted the unrecognised override and resolved to %q", got)
	}
	if got != "" {
		t.Errorf("resolveAuth returned %q alongside the error; no scheme may be presented", got)
	}
}

// TestValidAuthOverride_IsAccepted pins that the supported spellings
// keep working — the correction must not make a legitimate override
// unusable.
func TestValidAuthOverride_IsAccepted(t *testing.T) {
	cases := []struct {
		override string
		want     AuthScheme
	}{
		{"bearer", AuthSchemeBearer},
		{"x-api-key", AuthSchemeXAPIKey},
		{"x_api_key", AuthSchemeXAPIKey},
		{"api-key", AuthSchemeXAPIKey},
		{"none", AuthSchemeNone},
		{"no-auth", AuthSchemeNone},
		{"BEARER", AuthSchemeBearer},
	}
	for _, tc := range cases {
		t.Run(tc.override, func(t *testing.T) {
			sc := &SynthClient{
				BaseURL:      "https://api.anthropic.com/v1",
				APIKey:       "synthetic-credential",
				Wire:         wireAnthropic,
				AuthOverride: tc.override,
			}
			got, err := sc.resolveAuth()
			if err != nil {
				t.Fatalf("resolveAuth rejected the supported override %q: %v", tc.override, err)
			}
			if got != tc.want {
				t.Errorf("resolveAuth(%q) = %q; want %q", tc.override, got, tc.want)
			}
		})
	}
}

// TestInferWire_QueryStringDoesNotDecideTheProtocol pins that a URL's
// query string cannot decide the protocol — and therefore cannot decide
// the credential's presentation shape.
//
// A URL's query string is a request's parameters, not the endpoint's
// identity. Matching the whole URL meant that
// `https://api.anthropic.com/v1?mirror=ollama` inferred the OpenAI
// protocol, and the Anthropic credential was presented as
// `Authorization: Bearer` on Anthropic's own API.
//
// Host and path still decide it: that is where the provider signal
// actually lives.
func TestInferWire_QueryStringDoesNotDecideTheProtocol(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want wireShape
	}{
		{"a query string naming ollama", "https://api.anthropic.com/v1?mirror=ollama", wireAnthropic},
		{"a query string naming openai", "https://api.anthropic.com/v1?mirror=openai", wireAnthropic},
		{"a query string naming localhost", "https://api.anthropic.com/v1?host=localhost", wireAnthropic},
		{"a fragment naming ollama", "https://api.anthropic.com/v1#ollama", wireAnthropic},
		{"the host still decides", "https://api.openai.com/v1?mirror=anthropic", wireOpenAI},
		{"the path still decides", "https://gateway.example.com/openai/v1?x=1", wireOpenAI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferWire(tc.url); got != tc.want {
				t.Errorf("inferWire(%q) = %v; want %v", tc.url, got, tc.want)
			}
		})
	}
}
