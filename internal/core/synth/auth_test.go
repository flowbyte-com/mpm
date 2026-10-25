// auth_test.go — unit coverage for the authentication scheme layer.
//
// The transport-level behaviour is pinned in auth_transport_test.go;
// this file pins the DECISION layer: how a scheme is chosen, how an
// override is honoured, what counts as evidence of a header mismatch,
// and how the sub-cases of an authentication refusal stay
// distinguishable.
package synth

import (
	"net/http"
	"strings"
	"testing"
)

// TestParseAuthScheme pins the accepted spellings and rejects
// everything else. An unsupported scheme must be an explicit error,
// never a silent approximation.
func TestParseAuthScheme(t *testing.T) {
	cases := []struct {
		raw    string
		want   AuthScheme
		wantOK bool
	}{
		{"", "", false},
		{"bearer", AuthSchemeBearer, true},
		{"Bearer", AuthSchemeBearer, true},
		{"BEARER", AuthSchemeBearer, true},
		{"  bearer  ", AuthSchemeBearer, true},
		{"x-api-key", AuthSchemeXAPIKey, true},
		{"X-Api-Key", AuthSchemeXAPIKey, true},
		{"x_api_key", AuthSchemeXAPIKey, true},
		{"none", AuthSchemeNone, true},
		{"basic", "", false},
		{"query-param", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseAuthScheme(tc.raw)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("ParseAuthScheme(%q) = (%q, %v); want (%q, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestResolveAuthScheme_Order pins the resolution order:
//
//	explicit override → documented endpoint → negotiated → protocol default
func TestResolveAuthScheme_Order(t *testing.T) {
	cases := []struct {
		name       string
		baseURL    string
		credential string
		override   string
		want       AuthScheme
	}{
		{
			name:       "documented MiniMax endpoint is bearer on the Anthropic surface",
			baseURL:    "https://api.minimax.io/anthropic/v1",
			credential: "k",
			want:       AuthSchemeBearer,
		},
		{
			name:       "documented MiniMax endpoint is bearer on the OpenAI surface",
			baseURL:    "https://api.minimax.io/v1",
			credential: "k",
			want:       AuthSchemeBearer,
		},
		{
			name:       "operator override beats the documented rule",
			baseURL:    "https://api.minimax.io/anthropic/v1",
			credential: "k",
			override:   "x-api-key",
			want:       AuthSchemeXAPIKey,
		},
		{
			name:       "ordinary Anthropic keeps the protocol default",
			baseURL:    "https://api.anthropic.com/v1",
			credential: "k",
			want:       AuthSchemeXAPIKey,
		},
		{
			name:       "ordinary OpenAI-compatible keeps the protocol default",
			baseURL:    "https://api.openai.com/v1",
			credential: "k",
			want:       AuthSchemeBearer,
		},
		{
			name:       "unknown Anthropic-protocol endpoint falls back to the protocol default",
			baseURL:    "https://gateway.example.com/v1",
			credential: "k",
			want:       AuthSchemeXAPIKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := &SynthClient{
				BaseURL:      tc.baseURL,
				APIKey:       tc.credential,
				Wire:         inferWire(tc.baseURL),
				AuthOverride: tc.override,
			}
			got, err := sc.resolveAuth()
			if err != nil {
				t.Fatalf("resolveAuth(%q, override=%q) failed: %v", tc.baseURL, tc.override, err)
			}
			if got != tc.want {
				t.Errorf("resolveAuth(%q, override=%q) = %q; want %q", tc.baseURL, tc.override, got, tc.want)
			}
		})
	}
}

// TestResolveAuth_UnsupportedOverrideIsRefused pins that an
// UNRECOGNISED operator override is refused rather than silently
// replaced with a derived scheme.
//
// The override used to be parsed with a matcher that returned ("", false)
// for anything it did not recognise, and the resolution simply moved on
// to the next tier. An operator who hand-edited `auth_scheme: "basic"`
// into mpm_config.json was never told: the endpoint was authenticated
// with a derived scheme and answered 401, and the symptom pointed in
// exactly the wrong direction.
//
// The refusal names the supported set, so the operator can see the
// correct spelling at once.
func TestResolveAuth_UnsupportedOverrideIsRefused(t *testing.T) {
	sc := &SynthClient{
		BaseURL:      "https://api.anthropic.com/v1",
		APIKey:       "k",
		Wire:         inferWire("https://api.anthropic.com/v1"),
		AuthOverride: "basic",
	}
	got, err := sc.resolveAuth()
	if err == nil {
		t.Fatalf("resolveAuth accepted the unsupported override %q and resolved to %q", "basic", got)
	}
	if got != "" {
		t.Errorf("resolveAuth returned a scheme alongside the error: %q", got)
	}
	msg := err.Error()
	for _, want := range []string{"basic", "bearer", "x-api-key", "none"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// TestResolveAuthScheme_NegotiatedIsRetained pins that a scheme
// discovered by a successful negotiation is retained for subsequent
// operations on the same endpoint and credential — and that changing
// either the endpoint or the credential invalidates it.
func TestResolveAuthScheme_NegotiatedIsRetained(t *testing.T) {
	const baseURL = "https://gateway.example.com/v1"
	const credential = "negotiated-secret"

	rememberNegotiatedAuth(baseURL, credential, AuthSchemeBearer)

	client := func(baseURL, credential, override string) *SynthClient {
		return &SynthClient{
			BaseURL:      baseURL,
			APIKey:       credential,
			Wire:         inferWire(baseURL),
			AuthOverride: override,
		}
	}
	resolve := func(baseURL, credential, override string) AuthScheme {
		t.Helper()
		got, err := client(baseURL, credential, override).resolveAuth()
		if err != nil {
			t.Fatalf("resolveAuth(%q, override=%q) failed: %v", baseURL, override, err)
		}
		return got
	}

	if got := resolve(baseURL, credential, ""); got != AuthSchemeBearer {
		t.Errorf("negotiated scheme was not retained: got %q, want %q", got, AuthSchemeBearer)
	}
	// A different credential must not inherit the retained scheme.
	if got := resolve(baseURL, "other-secret", ""); got != AuthSchemeXAPIKey {
		t.Errorf("negotiated scheme leaked across credentials: got %q, want the protocol default %q", got, AuthSchemeXAPIKey)
	}
	// A different endpoint must not inherit the retained scheme.
	if got := resolve("https://other.example.com/v1", credential, ""); got != AuthSchemeXAPIKey {
		t.Errorf("negotiated scheme leaked across endpoints: got %q, want the protocol default %q", got, AuthSchemeXAPIKey)
	}
	// An explicit override still wins over the retained scheme.
	if got := resolve(baseURL, credential, "x-api-key"); got != AuthSchemeXAPIKey {
		t.Errorf("override did not beat the retained scheme: got %q", got)
	}
}

// TestAuthMismatchEvidence pins what counts as evidence that the
// endpoint wanted a different authentication header.
//
// Evidence must be explicit. A generic refusal names no header and is
// therefore an invalid credential, not a mis-presented one.
func TestAuthMismatchEvidence(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		sent   AuthScheme
		want   AuthScheme
		wantOK bool
	}{
		{
			name:   "provider names the Authorization field",
			body:   `{"base_resp":{"status_code":1004,"status_msg":"login fail: Please carry the API secret key in the Authorization field"}}`,
			sent:   AuthSchemeXAPIKey,
			want:   AuthSchemeBearer,
			wantOK: true,
		},
		{
			name:   "provider names the x-api-key header",
			body:   `{"error":{"message":"the x-api-key header is required for this endpoint"}}`,
			sent:   AuthSchemeBearer,
			want:   AuthSchemeXAPIKey,
			wantOK: true,
		},
		{
			name:   "generic invalid credential is not evidence",
			body:   `{"error":{"type":"authentication_error","message":"invalid api key"}}`,
			sent:   AuthSchemeXAPIKey,
			wantOK: false,
		},
		{
			name:   "the header already sent is not evidence for a change",
			body:   `{"error":{"message":"Authorization rejected"}}`,
			sent:   AuthSchemeBearer,
			wantOK: false,
		},
		{
			name:   "empty body is not evidence",
			body:   "",
			sent:   AuthSchemeXAPIKey,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := authMismatchEvidence([]byte(tc.body), tc.sent)
			if ok != tc.wantOK {
				t.Fatalf("authMismatchEvidence ok = %v; want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("authMismatchEvidence = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyAuthFailure pins that the three authentication refusals
// stay distinguishable.
//
// An invalid credential, an unbilled subscription and an
// under-privileged key are three different problems with three
// different remedies. Collapsing them into one "auth failed" line is
// what made the first clean-install report untrustworthy.
func TestClassifyAuthFailure(t *testing.T) {
	cases := []struct {
		status int
		want   AuthFailureKind
	}{
		{http.StatusUnauthorized, AuthFailureInvalidCredential},
		{http.StatusPaymentRequired, AuthFailureSubscription},
		{http.StatusForbidden, AuthFailurePermission},
		{http.StatusBadRequest, AuthFailureNone},
		{http.StatusInternalServerError, AuthFailureNone},
		{0, AuthFailureNone},
	}
	for _, tc := range cases {
		got := ClassifyAuthFailure(tc.status)
		if got != tc.want {
			t.Errorf("ClassifyAuthFailure(%d) = %q; want %q", tc.status, got, tc.want)
		}
	}
}

// TestAuthScheme_HeaderFor_NoCredentialSendsNoHeader pins that an
// absent credential produces an absent header, not an empty bearer
// token. Some servers reject a malformed Authorization header
// outright; "no credential" is honestly represented by no header.
func TestAuthScheme_HeaderFor_NoCredentialSendsNoHeader(t *testing.T) {
	for _, scheme := range []AuthScheme{AuthSchemeBearer, AuthSchemeXAPIKey, AuthSchemeNone} {
		if _, _, present := authHeaderFor(scheme, ""); present {
			t.Errorf("scheme %q presented a header with no credential", scheme)
		}
	}
	if _, _, present := authHeaderFor(AuthSchemeNone, "secret"); present {
		t.Error("the none scheme presented a credential header")
	}
}
