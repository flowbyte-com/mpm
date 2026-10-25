// auth_evidence_test.go — hermetic regression tests for the bounded
// negotiation's EVIDENCE rule (issue #66).
//
// The bounded alternative authentication attempt is permitted only when
// the provider's refusal explicitly names a DIFFERENT authentication
// header. The implementation that shipped matched a bare substring
// anywhere in the response body, so:
//
//   - a front-end proxy's own 401 page ("401 Authorization Required")
//     authorised a credential-bearing retry against a server that was
//     never the provider;
//   - a 403 permission body ("your organization does not have
//     authorization to use model X") authorised a retry even though the
//     credential was ACCEPTED and the refusal was about permissions;
//   - a body that echoed the request back authorised a retry.
//
// The contract says a *mention* is not a *requirement*. These tests pin
// the difference.
//
// Every body here is a synthetic fixture. No credential is sent anywhere.

package synth

import "testing"

// TestAuthMismatchEvidence_RequiresTheHeaderToBeNamed pins the POSITIVE
// cases: a provider that names the header it wants, in a way that frames
// it as the header it REQUIRES.
func TestAuthMismatchEvidence_RequiresTheHeaderToBeNamed(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		sent   AuthScheme
		want   AuthScheme
		wantOK bool
	}{
		{
			name:   "MiniMax names the Authorization field",
			body:   `{"base_resp":{"status_code":1004,"status_msg":"login fail: Please carry the API secret key in the Authorization field"}}`,
			sent:   AuthSchemeXAPIKey,
			want:   AuthSchemeBearer,
			wantOK: true,
		},
		{
			name:   "OpenAI-compatible names the Authorization field",
			body:   `{"error":{"message":"please carry the API secret key in the Authorization field"}}`,
			sent:   AuthSchemeXAPIKey,
			want:   AuthSchemeBearer,
			wantOK: true,
		},
		{
			name:   "the x-api-key header is required",
			body:   `{"error":{"message":"the x-api-key header is required for this endpoint"}}`,
			sent:   AuthSchemeBearer,
			want:   AuthSchemeXAPIKey,
			wantOK: true,
		},
		{
			name:   "the Authorization header is missing",
			body:   `{"error":{"message":"Missing Authorization header"}}`,
			sent:   AuthSchemeXAPIKey,
			want:   AuthSchemeBearer,
			wantOK: true,
		},
		{
			name:   "the expected header is Authorization",
			body:   `{"error":{"message":"expected header: Authorization"}}`,
			sent:   AuthSchemeXAPIKey,
			want:   AuthSchemeBearer,
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := authMismatchEvidence([]byte(tc.body), tc.sent)
			if ok != tc.wantOK {
				t.Fatalf("authMismatchEvidence ok = %v; want %v (body: %s)", ok, tc.wantOK, tc.body)
			}
			if ok && got != tc.want {
				t.Errorf("authMismatchEvidence = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestAuthMismatchEvidence_RejectsAMentionThatIsNotARequirement pins
// that a bare MENTION never authorises a credential-bearing retry.
//
// Each of these bodies contains the word "authorization" or a header
// name. None of them says "this header is required".
func TestAuthMismatchEvidence_RejectsAMentionThatIsNotARequirement(t *testing.T) {
	cases := []struct {
		name string
		body string
		sent AuthScheme
	}{
		{
			name: "a permission body mentions authorization",
			body: `{"error":{"message":"your organization does not have authorization to use model X"}}`,
			sent: AuthSchemeXAPIKey,
		},
		{
			name: "a proxy's 401 page",
			body: `<html><head><title>401 Authorization Required</title></head><body><h1>401 Authorization Required</h1></body></html>`,
			sent: AuthSchemeXAPIKey,
		},
		{
			name: "a proxy's plain-text 401 page",
			body: `401 Authorization Required`,
			sent: AuthSchemeXAPIKey,
		},
		{
			name: "an echoed request",
			body: `{"received":{"headers":{"authorization":"Bearer masked"}}}`,
			sent: AuthSchemeXAPIKey,
		},
		{
			name: "a generic invalid credential",
			body: `{"error":{"type":"authentication_error","message":"invalid api key"}}`,
			sent: AuthSchemeXAPIKey,
		},
		{
			name: "the header already sent is not evidence for a change",
			body: `{"error":{"message":"Authorization rejected"}}`,
			sent: AuthSchemeBearer,
		},
		{
			name: "an empty body",
			body: "",
			sent: AuthSchemeXAPIKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := authMismatchEvidence([]byte(tc.body), tc.sent)
			if ok {
				t.Fatalf("authMismatchEvidence = %q, want no evidence (a mention is not a requirement): body=%s", got, tc.body)
			}
		})
	}
}

// TestAuthMismatchEvidence_ReadsOnlyTheProviderErrorMessage pins that a
// header named OUTSIDE the provider's error message is not evidence.
//
// The provider's message field says nothing about a header; a different
// field mentions one. Only the message counts.
func TestAuthMismatchEvidence_ReadsOnlyTheProviderErrorMessage(t *testing.T) {
	body := `{"hint":{"note":"use the Authorization header"},"error":{"message":"invalid api key"}}`
	if got, ok := authMismatchEvidence([]byte(body), AuthSchemeXAPIKey); ok {
		t.Fatalf("authMismatchEvidence = %q, want no evidence (the header was named outside the error message)", got)
	}
}

// TestAuthMismatchEvidence_AcceptsTheMessageFieldOfSeveralShapes pins
// that the message is found in the shapes providers actually use.
func TestAuthMismatchEvidence_AcceptsTheMessageFieldOfSeveralShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"error.message", `{"error":{"message":"Missing Authorization header"}}`},
		{"error as a string", `{"error":"Missing Authorization header"}`},
		{"top-level message", `{"message":"Missing Authorization header"}`},
		{"base_resp.status_msg", `{"base_resp":{"status_code":1004,"status_msg":"Missing Authorization header"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := authMismatchEvidence([]byte(tc.body), AuthSchemeXAPIKey)
			if !ok {
				t.Fatalf("authMismatchEvidence found no evidence in %s", tc.name)
			}
			if got != AuthSchemeBearer {
				t.Errorf("authMismatchEvidence = %q, want %q", got, AuthSchemeBearer)
			}
		})
	}
}
