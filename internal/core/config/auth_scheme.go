// auth_scheme.go — the canonical authentication-scheme vocabulary.
//
// WHY THIS FILE EXISTS
//
// An operator can pin the authentication scheme for a profile:
//
//	mpm config profile set <name> auth_scheme bearer|x-api-key|none
//
// The CLI validates that value when it is typed. The CONFIGURATION FILE
// did not: a hand-edited `mpm_config.json` carrying `auth_scheme:
// "basic"` — or a plain typo — was silently dropped, and the endpoint
// was authenticated with a DERIVED scheme instead of the one the
// operator asked for.
//
// The failure direction is benign (it falls toward a derived scheme,
// never toward an unauthenticated request), but it is exactly the
// "silently ignored operator intent" class the field exists to
// eliminate: the operator is told nothing, and the symptom — a 401 from
// a gateway that wants a bearer token — points in exactly the wrong
// direction.
//
// WHY THE VOCABULARY LIVES HERE
//
// The configuration layer has to be able to VALIDATE the operator's
// value, and the configuration package cannot import the synth package:
// synth imports config, so config importing synth would be a cycle.
//
// The vocabulary therefore lives in config, and the synth package
// re-exports it as an alias. One source of truth, no cycle, and the
// configuration layer can validate without reaching sideways.

package config

import (
	"fmt"
	"sort"
	"strings"
)

// AuthScheme names how a credential is presented to an endpoint.
//
// The set is deliberately small: these are the mechanisms MPM
// implements and can therefore guarantee. A scheme that is not in this
// set is not silently approximated — an operator asking for an
// unsupported scheme gets an explicit error.
type AuthScheme string

const (
	// AuthSchemeNone sends no credential at all. Correct for
	// locally-hosted servers that run without authentication.
	AuthSchemeNone AuthScheme = "none"

	// AuthSchemeBearer sends `Authorization: Bearer <credential>`.
	// Used by OpenAI-compatible endpoints and by MiniMax on both of
	// its surfaces (including the Anthropic-compatible one).
	AuthSchemeBearer AuthScheme = "bearer"

	// AuthSchemeXAPIKey sends `X-Api-Key: <credential>`. Used by the
	// Anthropic API and by Anthropic-protocol proxies.
	AuthSchemeXAPIKey AuthScheme = "x-api-key"
)

// String returns the canonical lowercase label. Stable across releases:
// it is persisted in configuration and rendered in operator output.
func (a AuthScheme) String() string { return string(a) }

// ParseAuthScheme parses an operator-supplied scheme name.
//
// The accepted spellings are the canonical labels plus the obvious
// punctuation variants an operator might type. An empty string is NOT a
// scheme — it means "no override", and the caller falls through to the
// next resolution tier.
//
// An unrecognised value returns ok=false. The caller MUST NOT treat
// that as "no override": an operator who wrote `auth_scheme: "basic"`
// asked for something specific, and silently substituting a derived
// scheme hides the mistake.
func ParseAuthScheme(raw string) (AuthScheme, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", false
	case "bearer":
		return AuthSchemeBearer, true
	case "x-api-key", "x_api_key", "xapikey", "api-key":
		return AuthSchemeXAPIKey, true
	case "none", "no-auth", "noauth":
		return AuthSchemeNone, true
	}
	return "", false
}

// supportedAuthSchemes is the canonical list, in the order an error
// message names them.
var supportedAuthSchemes = []AuthScheme{
	AuthSchemeBearer,
	AuthSchemeXAPIKey,
	AuthSchemeNone,
}

// SupportedAuthSchemes returns the canonical list of supported schemes.
//
// It exists so that every message that names the supported set names
// the SAME set: the CLI's refusal, the runtime's refusal and the
// doctor's warning.
func SupportedAuthSchemes() []string {
	out := make([]string, 0, len(supportedAuthSchemes))
	for _, s := range supportedAuthSchemes {
		out = append(out, s.String())
	}
	return out
}

// ValidateAuthScheme reports whether raw is a usable authentication
// scheme.
//
// An empty value is valid: it means "no override".
//
// An unrecognised value is an error that NAMES THE SUPPORTED SET, so
// the operator can see at once what the correct spelling is.
func ValidateAuthScheme(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if _, ok := ParseAuthScheme(trimmed); ok {
		return nil
	}
	return fmt.Errorf("unsupported auth_scheme %q (supported: %s)", trimmed, strings.Join(SupportedAuthSchemes(), ", "))
}

// InvalidAuthSchemes returns one human-readable line per profile whose
// auth_scheme value is not a supported scheme.
//
// It is computed from the authoritative configuration on read — nothing
// is persisted about it — and it is what the doctor surfaces so the
// operator learns about a bad value before a request fails against a
// gateway.
func (c *Config) InvalidAuthSchemes() []string {
	if c == nil {
		return nil
	}
	out := []string{}
	for name, p := range c.Profiles {
		if err := ValidateAuthScheme(p.AuthScheme); err != nil {
			out = append(out, fmt.Sprintf("profile %q: %v", name, err))
		}
	}
	sort.Strings(out)
	return out
}
