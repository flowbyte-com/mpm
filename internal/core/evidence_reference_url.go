// evidence_reference_url.go — the reference-URL contract.
//
// A reference URL is EXPLICIT, USER-SUPPLIED reference metadata attached to an
// evidence row. It answers one question:
//
//	"what external reference did this claim come from?"
//
// What it is emphatically NOT:
//
//   - proof the URL is reachable, correct, or trustworthy
//   - something MPM fetched, resolved, cached, or verified
//   - an endorsement of, or authentication with, the referenced party
//   - a crawl target, a link checker, or a citation-correction engine
//
// MPM stores the string and hands it back unchanged. It never performs any
// network operation to do so, and the URL's presence has no effect on
// confidence, verification, or SourceGroupClass — a reference identifies a
// source; it does not corroborate a claim.
//
// Ownership: `evidence.reference_url` is the canonical and only home for a
// durable reference URL. It is deliberately NOT mirrored onto artifact rows
// (memories, lessons, works, …), because a URL identifies the source of a
// claim, not the artifact itself.
package internal

import (
	"fmt"
	"net/url"
	"strings"
)

// ReferenceURLMaxChars caps the persisted length of a reference URL.
//
// The value mirrors URIMaxChars (snapshot.go), which is this repository's
// existing ceiling for a persisted URI-shaped string. It is a storage bound,
// not a product limit: real-world URLs including query strings comfortably fit,
// and rejecting a longer value is preferable to accepting unbounded text that
// no downstream reader is designed to display.
const ReferenceURLMaxChars = 2048

// ReferenceURLSchemes is the accepted scheme vocabulary.
//
// Restricted to http/https deliberately. The feature is named "Reference URL"
// and models a web reference; `file`, `git`, and `ssh` are not web references,
// and adding them would imply capabilities (local path resolution, remote
// transport) that MPM does not have. If a future product need appears for
// another scheme, widen this map — and this map is the only place that
// decision lives.
var ReferenceURLSchemes = map[string]bool{
	"http":  true,
	"https": true,
}

// ValidateReferenceURL validates an explicitly supplied external reference URL
// and returns the value to persist.
//
// Contract:
//
//   - ABSENT is valid. An empty or whitespace-only string means "no reference
//     supplied" and returns ("", nil). This keeps every pre-existing caller —
//     which never sets the field — working unchanged, and keeps "omitted"
//     distinct from "invalid".
//   - PRESENT means an absolute http/https URL with a non-empty host. Anything
//     else is an error, returned before any DB work so a rejected URL leaves
//     zero partial state.
//   - NO NORMALIZATION. The returned string is the caller's input verbatim.
//     Query strings and fragments are preserved, path case is preserved,
//     parameter order is preserved, percent-encoding is preserved. Validation
//     never rewrites; a lossy transformation here would silently corrupt a
//     reference the user can no longer reproduce.
//
// This function performs no network I/O of any kind.
func ValidateReferenceURL(raw string) (string, error) {
	// Absent. Trim only for the emptiness test — the persisted value for a
	// genuinely-supplied URL is never trimmed either, so what is validated is
	// exactly what is stored.
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}

	if len(raw) > ReferenceURLMaxChars {
		return "", fmt.Errorf("reference_url exceeds max length (%d characters, got %d)",
			ReferenceURLMaxChars, len(raw))
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("reference_url is not a valid URL: %w", err)
	}

	if parsed.Scheme == "" {
		return "", fmt.Errorf("reference_url %q has no scheme; "+
			"an explicit external reference must be an absolute URL beginning with http:// or https://", raw)
	}
	if !ReferenceURLSchemes[strings.ToLower(parsed.Scheme)] {
		return "", fmt.Errorf("reference_url scheme %q is not supported; "+
			"accepted schemes are http and https", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("reference_url %q has no host", raw)
	}

	return raw, nil
}
