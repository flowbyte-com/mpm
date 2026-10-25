// sanitize_keylike_test.go — the probe's summary scrubber must not
// persist a credential in a shape the prefix list does not know.
//
// The synth layer embeds the raw provider body in its error string, and
// the probe persists a bounded, sanitized fragment of that body into the
// probe cache row's error_summary, which `mpm doctor` renders.
//
// The prefix scrub knew `sk-`, `sk-or-`, `xoxb-` and `Bearer `. A
// provider or a proxy that echoed a credential as `api_key=…` or
// `token: …` would have had it captured verbatim. The pattern for those
// shapes was declared and never wired up.

package probe

import (
	"strings"
	"testing"
)

// TestSanitizeError_ScrubsKeyLikePairs pins that a secret-bearing
// key=value pair is redacted from the persisted summary.
func TestSanitizeError_ScrubsKeyLikePairs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		leak string
	}{
		{"api_key pair", `provider said: api_key=super-secret-value`, "super-secret-value"},
		{"api-key pair", `provider said: api-key=super-secret-value`, "super-secret-value"},
		{"token pair", `provider said: token=super-secret-value`, "super-secret-value"},
		{"access_token pair", `provider said: access_token=super-secret-value`, "super-secret-value"},
		{"password pair", `provider said: password=super-secret-value`, "super-secret-value"},
		{"secret pair", `provider said: secret=super-secret-value`, "super-secret-value"},
		{"colon separator", `provider said: api_key: super-secret-value`, "super-secret-value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeError(tc.in)
			if strings.Contains(out, tc.leak) {
				t.Errorf("sanitizeError(%q) = %q; the credential survived", tc.in, out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("sanitizeError(%q) = %q; want a redaction marker", tc.in, out)
			}
		})
	}
}

// TestSanitizeError_KeepsOrdinaryText pins that the scrubber does not
// mangle ordinary diagnostics.
func TestSanitizeError_KeepsOrdinaryText(t *testing.T) {
	in := "provider refused request (auth) with scheme \"bearer\": API returned HTTP 401"
	out := sanitizeError(in)
	if out != in {
		t.Errorf("sanitizeError(%q) = %q; want the text unchanged", in, out)
	}
}
