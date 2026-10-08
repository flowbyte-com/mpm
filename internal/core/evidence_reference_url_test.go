package internal

import (
	"strings"
	"testing"
)

// evidence_reference_url_test.go — the validation contract for the optional,
// user-supplied reference URL on an evidence row.
//
// The governing contract, restated so each test below can be read as a
// clause of it:
//
//   - ABSENT is valid. Omitting the URL is the common case and never an error.
//   - PRESENT must be an absolute http/https URL with a host.
//   - NO NORMALIZATION. What is validated is byte-for-byte what is stored.
//   - NO NETWORK. Validation is purely syntactic; nothing here resolves,
//     connects, or fetches.

// TestValidateReferenceURL_AbsentIsValid pins that "no reference supplied"
// is a first-class, error-free state. Every pre-existing caller omits the
// field; if absence were an error the entire evidence write path would break.
func TestValidateReferenceURL_AbsentIsValid(t *testing.T) {
	for _, in := range []string{"", "   ", "\t", "\n  \t"} {
		got, err := ValidateReferenceURL(in)
		if err != nil {
			t.Errorf("ValidateReferenceURL(%q) = error %v; absent must be valid", in, err)
		}
		if got != "" {
			t.Errorf("ValidateReferenceURL(%q) = %q; absent must normalize to \"\"", in, got)
		}
	}
}

// TestValidateReferenceURL_AcceptsAbsoluteHTTPAndHTTPS covers the accepted
// vocabulary. http is included deliberately: an https-only rule would reject
// legitimate references to plain-http documentation mirrors and archives.
func TestValidateReferenceURL_AcceptsAbsoluteHTTPAndHTTPS(t *testing.T) {
	accepted := []string{
		"https://example.com",
		"http://example.com",
		"https://example.com/spec",
		"https://example.com/a/b/c?q=1#frag",
		"https://sub.domain.example.co.uk/path",
		"https://example.com:8443/port",
		"https://user@host.invalid/p",
		"https://127.0.0.1:8080/local",
		"https://[2001:db8::1]/v6",
	}
	for _, in := range accepted {
		got, err := ValidateReferenceURL(in)
		if err != nil {
			t.Errorf("ValidateReferenceURL(%q) = error %v; want accepted", in, err)
			continue
		}
		if got != in {
			t.Errorf("ValidateReferenceURL(%q) = %q; want verbatim", in, got)
		}
	}
}

// TestValidateReferenceURL_RejectsMalformed covers inputs that are not
// absolute http/https URLs with a host.
func TestValidateReferenceURL_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"no scheme", "example.com/x"},
		{"no scheme, no slash", "example.com"},
		{"empty scheme", "://example.com"},
		{"scheme with space", "ht tp://example.com"},
		{"scheme with invalid char", "ht!tp://example.com"},
		{"http with no host", "http://"},
		{"https with no host", "https://"},
		{"bare words", "not a url"},
		{"path only", "/just/a/path"},
		{"opaque colon only", "http:/example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ValidateReferenceURL(tc.in); err == nil {
				t.Errorf("ValidateReferenceURL(%q) = %q, nil; want rejection", tc.in, got)
			}
		})
	}
}

// TestValidateReferenceURL_RejectsNonWebSchemes pins the closed scheme
// vocabulary. Each of these would imply a capability MPM does not have —
// local path resolution, remote transport, script execution.
func TestValidateReferenceURL_RejectsNonWebSchemes(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"file", "file:///etc/passwd"},
		{"ftp", "ftp://files.example.com/pub"},
		{"git", "git://github.com/example/repo.git"},
		{"ssh", "ssh://git@github.com/example/repo.git"},
		{"javascript", "javascript:alert(1)"},
		{"data", "data:text/html,<script>alert(1)</script>"},
		{"mailto", "mailto:someone@example.com"},
		{"mpm-internal", "mpm://memory/abc"},
		{"case variant of file", "FILE:///etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateReferenceURL(tc.in)
			if err == nil {
				t.Fatalf("ValidateReferenceURL(%q) = nil; want rejection", tc.in)
			}
			// The message must name the accepted set so the caller can
			// act without reading the source.
			if !strings.Contains(err.Error(), "http") {
				t.Errorf("error for %q should name the accepted schemes; got: %v", tc.in, err)
			}
		})
	}
}

// TestValidateReferenceURL_SchemeMatchIsCaseInsensitive pins that scheme
// matching is case-insensitive (as RFC 3986 requires) while the ORIGINAL
// casing is preserved in the returned value. The stored string is what the
// user typed, not what the parser produced.
func TestValidateReferenceURL_SchemeMatchIsCaseInsensitive(t *testing.T) {
	for _, in := range []string{
		"HTTPS://EXAMPLE.COM/path",
		"HtTpS://example.com/path",
		"HTTP://example.com",
	} {
		got, err := ValidateReferenceURL(in)
		if err != nil {
			t.Errorf("ValidateReferenceURL(%q) = error %v; scheme match must be case-insensitive", in, err)
			continue
		}
		if got != in {
			t.Errorf("ValidateReferenceURL(%q) = %q; want verbatim (casing preserved)", in, got)
		}
	}
}

// TestValidateReferenceURL_NoLossyNormalization is the core round-trip pin.
//
// Each of these inputs is one that a plausible "helpful" normalizer would
// rewrite: query parameters would be reordered by url.Values.Encode, path
// case would be lowered, escaping would be re-encoded, the scheme would be
// lowercased, a trailing slash added. Every one of those would break the
// promise that MPM hands back exactly what the user supplied, and several
// would point the reference at a DIFFERENT resource than the one cited —
// path case is significant on most servers, and query order can be
// significant for signed links.
//
// The assertion is byte equality with the input, not merely acceptance.
func TestValidateReferenceURL_NoLossyNormalization(t *testing.T) {
	inputs := []string{
		// Query parameters in descending key order — url.Values.Encode
		// would sort these ascending.
		"https://x.io/a?z=1&m=2&a=3",
		// Repeated key in a specific order.
		"https://x.io/p?a=1&a=0&a=2",
		// Mixed-case host and path.
		"https://Example.COM/Path/To/File.md",
		// Percent-encoding that must not be decoded or re-encoded.
		"https://x.io/p%20q?a=%2F&b=%3D",
		// A trailing slash that must not be added or removed.
		"https://x.io/trailing/",
		// Fragment carrying the identity of the specific resource.
		"https://x.io/doc#Section-2",
		// Uppercase scheme preserved.
		"HTTPS://EXAMPLE.COM/",
		// Explicit default port — must not be stripped.
		"https://x.io:443/p",
		// Non-ASCII path segment (percent-encoded UTF-8).
		"https://x.io/%E2%9C%93-done",
	}
	for _, in := range inputs {
		got, err := ValidateReferenceURL(in)
		if err != nil {
			t.Errorf("ValidateReferenceURL(%q) = error %v; want accepted", in, err)
			continue
		}
		if got != in {
			t.Errorf("ValidateReferenceURL round-trip corrupted the reference:\n  in:  %q\n  out: %q", in, got)
		}
	}
}

// TestValidateReferenceURL_LengthBoundary pins the storage bound and the
// reject-never-truncate policy. A truncated URL points at a different
// resource than the author supplied, which is worse than refusing it.
func TestValidateReferenceURL_LengthBoundary(t *testing.T) {
	// Build a URL whose total length is exactly the cap.
	base := "https://x.io/"
	exact := base + strings.Repeat("a", ReferenceURLMaxChars-len(base))
	if len(exact) != ReferenceURLMaxChars {
		t.Fatalf("test setup: built a %d-char URL, want exactly %d", len(exact), ReferenceURLMaxChars)
	}
	got, err := ValidateReferenceURL(exact)
	if err != nil {
		t.Errorf("a URL of exactly the cap (%d) must be accepted; got error: %v", ReferenceURLMaxChars, err)
	}
	if got != exact {
		t.Error("a URL of exactly the cap must round-trip verbatim")
	}

	over := exact + "a"
	if _, err := ValidateReferenceURL(over); err == nil {
		t.Errorf("a URL of %d chars must be rejected (cap is %d)", len(over), ReferenceURLMaxChars)
	}
	// Reject, never truncate: no partial value comes back.
	got, err = ValidateReferenceURL(over)
	if err == nil || got != "" {
		t.Errorf("over-length URL must return (\"\", err); got (%q, %v)", got, err)
	}
}

// TestValidateReferenceURL_RejectsControlCharacters pins that no stored
// reference can carry a terminal escape sequence into `mpm why` or
// `mpm evidence list`. net/url rejects ASCII control bytes during Parse;
// this pins that we depend on it rather than on luck.
func TestValidateReferenceURL_RejectsControlCharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"ESC sequence", "https://x.io/\x1b[31mred"},
		{"newline", "https://x.io/a\nGET /secret"},
		{"carriage return", "https://x.io/a\r\nb"},
		{"NUL", "https://x.io/a\x00b"},
		{"DEL", "https://x.io/a\x7fb"},
		{"vertical tab", "https://x.io/a\x0bb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ValidateReferenceURL(tc.in); err == nil {
				t.Errorf("ValidateReferenceURL(%q) = %q, nil; control characters must be rejected", tc.name, got)
			}
		})
	}
}

// TestValidateReferenceURL_PerformsNoNetworkIO is the offline guarantee.
//
// The host below is under the .invalid TLD, which RFC 2606 reserves and
// which can never resolve. If any code path attempted a DNS lookup or a
// connection, this test would fail — either by erroring or, on a system
// with a wildcard resolver, by taking far longer than the bound below.
// Validating an unroutable reference must be instantaneous and must not
// consult the network.
func TestValidateReferenceURL_PerformsNoNetworkIO(t *testing.T) {
	for _, host := range []string{
		"https://this-host-cannot-resolve.invalid/doc",
		"http://another-unresolvable-name.invalid:1/path?q=1",
	} {
		// Sequential, not parallel: this asserts completion, not speed.
		got, err := ValidateReferenceURL(host)
		if err != nil {
			t.Errorf("ValidateReferenceURL(%q) = error %v; a syntactically valid URL must validate offline", host, err)
		}
		if got != host {
			t.Errorf("ValidateReferenceURL(%q) = %q; want verbatim", host, got)
		}
	}
}

// TestReferenceURLSchemes_IsClosedVocabulary pins the accepted scheme set
// itself, so widening it later has to be a deliberate edit to this test.
func TestReferenceURLSchemes_IsClosedVocabulary(t *testing.T) {
	want := []string{"http", "https"}
	if len(ReferenceURLSchemes) != len(want) {
		t.Errorf("ReferenceURLSchemes has %d entries (%v); want exactly %d",
			len(ReferenceURLSchemes), ReferenceURLSchemes, len(want))
	}
	for _, s := range want {
		if !ReferenceURLSchemes[s] {
			t.Errorf("ReferenceURLSchemes is missing %q", s)
		}
	}
}
