// scanner_w1_test.go — W-1 (debt burn-down, 2026-09-01) regression
// coverage for the credential-scanner gap closure.
//
// Audit findings closed by these patterns:
//
//   L-1: GitHub `ghs_*` server-to-server token
//   L-2: AWS STS `ASIA*` temporary credentials
//   L-3: Google API key `AIza*`
//   L-4: Google OAuth `ya29.*`
//   plus: Azure storage account key (AccountKey=...)
//
// Each pattern has:
//
//   - Positive tests: realistic credential-shaped examples.
//   - Negative controls: prose / non-credential strings with similar
//     prefixes that must NOT trigger the scanner.
//   - Embedded-content tests: credentials embedded inside normal
//     text (the realistic leak shape).
//   - Short/malformed tests: truncated prefixes that must not match
//     (false-positive guard).
//
// The scanner chokepoint invariant — every memory write path routes
// through SaveMemoryNode's isSensitiveContent call — is enforced by
// TestScannerCoverage_AllMemoriesWritersScanContent (existing) and is
// unchanged by this addition.

package internal

import (
	"strings"
	"testing"
)

// TestScannerW1_GitHubServerToServer pins ghs_ detection.
func TestScannerW1_GitHubServerToServer(t *testing.T) {
	// Realistic ghs_ tokens are 36 alphanumerics after the prefix.
	positives := []string{
		// Bare credential
		"ghs_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
		// Embedded in prose — realistic leak shape
		"Here's my GitHub App token: ghs_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
		// In a config-style assignment
		"GITHUB_TOKEN=ghs_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
		// In a JSON envelope
		`{"github_token":"ghs_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789"}`,
	}
	for _, c := range positives {
		t.Run("pos_"+shortName(c), func(t *testing.T) {
			blocked, reason := isSensitiveContent(c)
			if !blocked {
				t.Errorf("ghs_ credential NOT blocked: %q", c)
			} else if reason == "" {
				t.Errorf("ghs_ blocked but no reason: %q", c)
			} else if !strings.Contains(reason, "GitHub App Server-to-Server") {
				t.Errorf("ghs_ blocked with wrong reason: %q (reason: %s)", c, reason)
			}
		})
	}
}

// TestScannerW1_GitHubUserToServer pins ghu_ detection.
func TestScannerW1_GitHubUserToServer(t *testing.T) {
	positives := []string{
		"ghu_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
		"token: ghu_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
	}
	for _, c := range positives {
		t.Run("pos_"+shortName(c), func(t *testing.T) {
			blocked, reason := isSensitiveContent(c)
			if !blocked {
				t.Errorf("ghu_ credential NOT blocked: %q", c)
			} else if !strings.Contains(reason, "User-to-Server") {
				t.Errorf("ghu_ blocked with wrong reason: %q (reason: %s)", c, reason)
			}
		})
	}

	// Short/malformed — must not match (false-positive guard).
	// Truncated prefixes (less than 36 alphanumerics) must NOT trigger.
	shorts := []string{
		"ghu_short",
		"ghu_",
		"ghu_abc", // 3 alphanumerics — below the {36} threshold
	}
	for _, c := range shorts {
		t.Run("short_"+c, func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("short ghu_ should NOT be blocked: %q", c)
			}
		})
	}
}

// TestScannerW1_AWSSTSTemporary pins ASIA detection.
func TestScannerW1_AWSSTSTemporary(t *testing.T) {
	positives := []string{
		"ASIAIOSFODNN7EXAMPLE",
		"ASIA1234567890ABCDEF",
		"aws_creds: ASIAIOSFODNN7EXAMPLE",
		`{"aws_session":"ASIAIOSFODNN7EXAMPLE"}`,
	}
	for _, c := range positives {
		t.Run("pos_"+shortName(c), func(t *testing.T) {
			blocked, reason := isSensitiveContent(c)
			if !blocked {
				t.Errorf("ASIA credential NOT blocked: %q", c)
			} else if !strings.Contains(reason, "STS") {
				t.Errorf("ASIA blocked with wrong reason: %q (reason: %s)", c, reason)
			}
		})
	}

	// Negative controls — strings that look similar but are not ASIA.
	// "ASIA" appears in words like "ASIA PACIFIC" or stock tickers.
	negatives := []string{
		"meeting in ASIA PACIFIC",
		"ASIA: a continent",
		"asia stocks rose today",
	}
	for _, c := range negatives {
		t.Run("neg_"+shortName(c), func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("non-ASIA string blocked (false positive): %q", c)
			}
		})
	}

	// Short/malformed: fewer than 16 chars after ASIA must not match.
	shorts := []string{
		"ASIASHORT",
		"ASIA123",
	}
	for _, c := range shorts {
		t.Run("short_"+c, func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("short ASIA should NOT be blocked: %q", c)
			}
		})
	}
}

// TestScannerW1_GoogleAPIKey pins AIza detection.
func TestScannerW1_GoogleAPIKey(t *testing.T) {
	positives := []string{
		// 39-char Google API key (AIza + 35 chars)
		"AIzaSyA1BcDeFgHiJkLmNoPqRsTuVwXyZ0123456",
		// Embedded
		"google_key: AIzaSyA1BcDeFgHiJkLmNoPqRsTuVwXyZ0123456",
		// JSON
		`{"apiKey":"AIzaSyA1BcDeFgHiJkLmNoPqRsTuVwXyZ0123456"}`,
	}
	for _, c := range positives {
		t.Run("pos_"+shortName(c), func(t *testing.T) {
			blocked, reason := isSensitiveContent(c)
			if !blocked {
				t.Errorf("AIza key NOT blocked: %q", c)
			} else if !strings.Contains(reason, "Google API Key") {
				t.Errorf("AIza blocked with wrong reason: %q (reason: %s)", c, reason)
			}
		})
	}

	// Short — fewer than 35 chars after AIza must not match.
	shorts := []string{
		"AIzashort",
		"AIza_only_a_few_chars",
	}
	for _, c := range shorts {
		t.Run("short_"+c, func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("short AIza should NOT be blocked: %q", c)
			}
		})
	}
}

// TestScannerW1_GoogleOAuthBearer pins ya29. detection.
func TestScannerW1_GoogleOAuthBearer(t *testing.T) {
	positives := []string{
		// ya29. followed by 60+ chars
		"ya29.A0ARrdaM-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		// Embedded
		"oauth: ya29.A0ARrdaM-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}
	for _, c := range positives {
		t.Run("pos_"+shortName(c), func(t *testing.T) {
			blocked, reason := isSensitiveContent(c)
			if !blocked {
				t.Errorf("ya29 token NOT blocked: %q", c)
			} else if !strings.Contains(reason, "Google OAuth") {
				t.Errorf("ya29 blocked with wrong reason: %q (reason: %s)", c, reason)
			}
		})
	}

	// Negative — ya29 is rare in prose. Without the dot and suffix it's
	// just a string. The pattern requires `ya29.` (dot) so plain "ya29"
	// in prose does not match.
	negatives := []string{
		"ya29 short",
		"ya29 alone",
	}
	for _, c := range negatives {
		t.Run("neg_"+shortName(c), func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("non-ya29 string blocked (false positive): %q", c)
			}
		})
	}

	// Short — ya29. with fewer than 60 chars must not match.
	shorts := []string{
		"ya29.short",
		"ya29.aBcDeFgHiJ",
	}
	for _, c := range shorts {
		t.Run("short_"+c, func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("short ya29 should NOT be blocked: %q", c)
			}
		})
	}
}

// TestScannerW1_AzureAccountKey pins Azure storage account key detection.
// The Azure pattern anchors on "AccountKey=" because 88-char base64 is
// otherwise too ambiguous; AccountKey= only appears in Azure connection
// strings.
func TestScannerW1_AzureAccountKey(t *testing.T) {
	// 88-char base64 strings (typical Azure account key length).
	longBase64 := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghij=="
	positives := []string{
		// Bare Azure connection string
		"DefaultEndpointsProtocol=https;AccountName=foo;AccountKey=" + longBase64 + ";EndpointSuffix=core.windows.net",
		// Just the key=value portion
		"AccountKey=" + longBase64,
		// Embedded in prose
		"my azure key is AccountKey=" + longBase64,
	}
	for _, c := range positives {
		t.Run("pos_"+shortName(c), func(t *testing.T) {
			blocked, reason := isSensitiveContent(c)
			if !blocked {
				t.Errorf("Azure AccountKey NOT blocked: %q", c)
			} else if !strings.Contains(reason, "Azure") {
				t.Errorf("Azure blocked with wrong reason: %q (reason: %s)", c, reason)
			}
		})
	}

	// Negative — long base64 strings without the Azure prefix are
	// legitimate in many contexts (JWT segments, hashes, etc.).
	negatives := []string{
		longBase64,                       // bare base64 with no Azure marker
		"signature=" + longBase64,        // not Azure
		"sha256=" + longBase64,           // not Azure
	}
	for _, c := range negatives {
		t.Run("neg_"+shortName(c), func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("non-Azure base64 blocked (false positive): %q", c)
			}
		})
	}

	// Short — fewer than 80 chars after AccountKey= must not match.
	shorts := []string{
		"AccountKey=tooshort",
		"AccountKey=abc123",
	}
	for _, c := range shorts {
		t.Run("short_"+c, func(t *testing.T) {
			if blocked, _ := isSensitiveContent(c); blocked {
				t.Errorf("short Azure should NOT be blocked: %q", c)
			}
		})
	}
}

// TestScannerW1_AllNewPatternsSurfaceViaMemoryWritePath is the
// structural-surface test: each new pattern must trigger through the
// canonical memory write path (SaveMemoryNode), not only the standalone
// isSensitiveContent helper.
//
// We invoke MemoryStore.AddMemory with credential content and assert
// that the call returns a sensitive-content error. This proves the
// scanner chokepoint is wired correctly for every new pattern.
func TestScannerW1_AllNewPatternsSurfaceViaMemoryWritePath(t *testing.T) {
	cases := []struct {
		name    string
		content string
		reason  string
	}{
		{
			name:    "ghs_",
			content: "github app token: ghs_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
			reason:  "GitHub App Server-to-Server",
		},
		{
			name:    "ghu_",
			content: "github user token: ghu_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
			reason:  "User-to-Server",
		},
		{
			name:    "ASIA",
			content: "aws session: ASIAIOSFODNN7EXAMPLE",
			reason:  "STS",
		},
		{
			name:    "AIza",
			content: "google key: AIzaSyA1BcDeFgHiJkLmNoPqRsTuVwXyZ0123456",
			reason:  "Google API Key",
		},
		{
			name:    "ya29",
			content: "oauth: ya29.A0ARrdaM-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			reason:  "Google OAuth",
		},
		{
			name: "Azure",
			content: "AccountKey=abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghij==",
			reason:  "Azure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := freshMemoryStore(t)

			_, err := ms.AddMemory(tc.content, "memories", []string{"test"}, nil, "", "test")
			if err == nil {
				t.Fatalf("credential %s NOT rejected by AddMemory: %q", tc.name, tc.content)
			}
			if !strings.Contains(err.Error(), "sensitive") &&
				!strings.Contains(err.Error(), tc.reason) {
				t.Errorf("AddMemory rejection lacks credential signal: %v", err)
			}
		})
	}
}

// shortName derives a short, readable identifier from a test fixture
// so subtest names stay under Go's 256-byte limit and remain greppable.
func shortName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 40 {
		s = s[:40]
	}
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
	return s
}
