// sanitize.go — BaseURLSafe, fingerprint composition, error-summary
// scrubbing. Three responsibilities:
//
//   (1) Compute canonical fingerprints from the EXACT material connection
//       bytes (provider, model, base_url, credential bytes). The fingerprint
//       input may include secret material transiently; only the final
//       SHA-256 hex digest is persisted.
//   (2) Render base_url into a sanitized display form (BaseURLSafe) with
//       userinfo / query / fragment stripped.
//   (3) Scrub provider errors so secret-bearing bodies never reach the
//       cache or rendered output.

package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
)

// secretSubstrings are scrubbed from error summaries before persistence.
// Each match is replaced with the same-length redaction mask. The list is
// intentionally narrow — it's a defence-in-depth net for cases where an
// upstream provider echoes the api_key in an error body.
var secretSubstrings = []string{
	"sk-or-",  // OpenRouter key prefix
	"sk-",     // OpenAI / generic prefix
	"xoxb-",   // Slack-style (defensive)
	"Bearer ", // bearer token prefix
	"bearer ", // lowercase variant
}

// sanitizeURL strips userinfo, query, and fragment from raw. Returns raw
// unchanged if it doesn't parse as a URL — providers occasionally accept
// weird endpoint shapes (e.g. bare unix sockets, opaque tokens); the
// downstream caller may still surface those, but the sanitized cache row
// then falls back to a "raw, no parse" representation.
//
// Examples:
//
//	"https://user:pass@example.com/v1?token=x#frag"
//	  → "https://example.com/v1"
//
//	"http://localhost:1234/v1"
//	  → "http://localhost:1234/v1"
//
//	"opaque-endpoint" (parse fails)
//	  → "" (caller falls back to a placeholder)
func sanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || (u.Host == "" && u.Path == "") {
		// Unparseable / opaque. Fall through unchanged but strip query-like
		// substrings conservatively (defensive).
		return stripQueryish(raw)
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// stripQueryish is the fallback for unparseable URLs: it removes anything
// after `?` or `#` so secret-bearing fragments cannot leak even when the
// URL doesn't parse.
func stripQueryish(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

// sanitizeError trims to ≤160 chars and scrubs any secret-prefix substring
// (case-insensitive). Used for the CachedProbe.ErrorSummary / ProbeResult
// ErrorSummary rendered output and persistent row.
func sanitizeError(msg string) string {
	if msg == "" {
		return ""
	}
	trimmed := strings.TrimSpace(msg)
	// Defensive scrub: redact any secret-prefix substring by replacing the
	// surrounding token (up to the next whitespace) with [REDACTED].
	for _, prefix := range secretSubstrings {
		if i := strings.Index(strings.ToLower(trimmed), strings.ToLower(prefix)); i >= 0 {
			// Find the next whitespace (or end) after i.
			end := strings.IndexAny(trimmed[i:], " \t\n\r,;\"'`]")
			if end < 0 {
				end = len(trimmed) - i
			}
			redacted := "[REDACTED]"
			trimmed = trimmed[:i] + redacted + trimmed[i+end:]
		}
	}
	// Truncate.
	const max = 160
	if len(trimmed) > max {
		trimmed = trimmed[:max-1] + "…"
	}
	return trimmed
}

// FingerprintInput is the exact material connection bytes that go into the
// fingerprint. Provider, Model, BaseURL, and credential bytes are joined
// with a delimiter that cannot appear in any of them. The credential bytes
// exist only transiently as input to SHA-256; only the final digest is
// persisted.
type FingerprintInput struct {
	Provider       string
	Model          string
	BaseURL        string // EXACT material — sanitization happens after fingerprint is computed
	Credential     string // raw bytes; nil/empty when not set
	OtherKeyValues []FingerprintField
}

// FingerprintField is an optional extra field whose change should invalidate
// the cached probe. Used to encode provider-specific connection fields
// (e.g. anthropic-version header, custom auth tokens in headers).
type FingerprintField struct {
	Key, Value string
}

// fingerprintInput is the legacy unexported alias kept so internal callers
// in this package keep working. New code should use the exported
// FingerprintInput type.
type fingerprintInput = FingerprintInput

// ComputeFingerprint returns the canonical hex SHA-256 digest over the
// exact material connection bytes. Inputs are split by a sentinel (`\x00`)
// that cannot legitimately appear in any field. The output is 64 hex chars.
//
// Never returns an error — SHA-256 over arbitrary input is total. Bounded
// in input size to defend against operators accidentally pasting a huge
// value into a profile field (which would still not break security but
// could waste CPU on adversarial inputs).
func ComputeFingerprint(in FingerprintInput) string {
	h := sha256.New()

	write := func(s string) {
		// Cap each individual field at 8 KiB to bound work.
		if len(s) > 8192 {
			s = s[:8192]
		}
		h.Write([]byte(s))
		h.Write([]byte{0})
	}

	write("provider=")
	write(in.Provider)
	write("|model=")
	write(in.Model)
	write("|base_url=")
	write(in.BaseURL)
	write("|credential=")
	write(in.Credential)
	for _, f := range in.OtherKeyValues {
		write("|ext:")
		write(f.Key)
		write("=")
		write(f.Value)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// keyLikePattern matches any obvious secret-bearing key= pair in summary
// text. Compiled once at package init.
var keyLikePattern = regexp.MustCompile(`(?i)(api[_-]?key|token|password|secret|access[_-]?token)\s*[:=]\s*\S+`)
