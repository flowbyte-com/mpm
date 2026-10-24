// endpoint.go — endpoint URL validation.
//
// An endpoint URL is what an operator types for a provider base URL.
// The runtime turns it into an HTTP request, so a malformed value is
// not caught until the request fails — or, worse, is written to the
// configuration file and only fails days later.
//
// These helpers let the configuration surfaces reject a malformed URL
// at the moment it is entered, with a message that says what a
// correct value looks like.
//
// 2026-10-23 (HAL-9001): the clean-install acceptance run saved an
// embedding endpoint typed as "http:://localhost:11434" — a doubled
// colon. Nothing rejected it: the wizard accepted the value, the
// configuration file recorded it, and the failure only surfaced later
// as an unreachable endpoint. The same class of typo must not reach
// the configuration file again.

package config

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateEndpointURL reports whether raw is a usable HTTP(S) endpoint
// URL.
//
// The rules are deliberately narrow:
//
//   - the value must be non-empty;
//   - it must carry an explicit http:// or https:// scheme — a bare
//     host ("localhost:11434") is ambiguous, because "localhost" parses
//     as a scheme and the rest as an opaque body;
//   - it must carry a host.
//
// A value such as "http:://localhost:11434" — the shape of a
// hand-typed typo — parses with an empty host and is rejected here
// rather than saved.
//
// The value is NOT rewritten. Callers that want a normalised form
// (the embedding providers, for instance, normalise their own paths)
// keep doing so themselves; this function only answers a question.
func ValidateEndpointURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("endpoint URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %v", trimmed, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%q must start with http:// or https://", trimmed)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", trimmed)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%q has no host", trimmed)
	}
	return nil
}
