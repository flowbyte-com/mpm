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
		{
			name:       "an unsupported override is ignored rather than silently approximated",
			baseURL:    "https://api.anthropic.com/v1",
			credential: "k",
			override:   "basic",
			want:       AuthSchemeXAPIKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveAuthScheme(tc.baseURL, tc.credential, tc.override); got != tc.want {
				t.Errorf("ResolveAuthScheme(%q, override=%q) = %q; want %q", tc.baseURL, tc.override, got, tc.want)
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
		if _, _, present := scheme.headerFor(""); present {
			t.Errorf("scheme %q presented a header with no credential", scheme)
		}
	}
	if _, _, present := AuthSchemeNone.headerFor("secret"); present {
		t.Error("the none scheme presented a credential header")
	}
}
