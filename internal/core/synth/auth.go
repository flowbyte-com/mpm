// auth.go — authentication scheme resolution, negotiation and refusal
// classification.
//
// WHY THIS FILE EXISTS
//
// The substrate used to conflate two independent concerns in a single
// "wire shape":
//
//   - the MESSAGE PROTOCOL (Anthropic-compatible / OpenAI-compatible),
//     which decides the URL path and the response-body parser; and
//   - the AUTHENTICATION SCHEME, which decides how the credential is
//     presented to the endpoint.
//
// The two do not always agree. MiniMax's Anthropic-compatible surface
// speaks the Anthropic message protocol — POST /v1/messages with a
// {"content":[{"type":"text",...}]} envelope — but authenticates with
// `Authorization: Bearer <key>`, NOT the Anthropic `X-Api-Key` header.
// A conflated design cannot express that endpoint at all, and the
// conflation was the root cause of HAL-9001 Finding 1: a correctly
// configured MiniMax profile produced HTTP 401 on every request,
// including the Doctor probe.
//
// This file owns the authentication half. The protocol half stays in
// wire.go.
//
// RESOLUTION ORDER
//
// The scheme is resolved per request, first match wins:
//
//  1. Explicit operator override (config.Profile.AuthScheme). For
//     endpoints whose authentication cannot be discovered reliably.
//  2. The endpoint's own documented rule, matched on the parsed URL
//     HOST against an exact domain set — never a substring search.
//  3. A scheme negotiated during an approved connection verification
//     for this exact (base URL, credential) pair.
//  4. The protocol default: OpenAI-compatible → Bearer,
//     Anthropic-compatible → X-Api-Key.
//
// Steps 1 and 2 are PREFERRED and are always tried before any
// alternative attempt. Step 3 only exists because a verification
// observed the endpoint refusing the scheme that was sent and naming
// the one it wanted. Step 4 is the historical behaviour, unchanged.
//
// BOUNDED NEGOTIATION
//
// A bounded alternative attempt is permitted ONLY when every one of
// these holds:
//
//   - the caller explicitly approved connection verification (the
//     Doctor active probe and the post-save verification probe set
//     AllowAuthNegotiation; normal synthesis never does);
//   - the refusal is an authentication refusal (401 / 403), not a
//     billing refusal (402) and not a transport failure;
//   - the provider's response names a DIFFERENT authentication header
//     — explicit evidence of a header mismatch, not a generic 401.
//
// A generic "invalid api key" is an INVALID CREDENTIAL. Repeating the
// request with another header cannot help and would be dishonest, so it
// never triggers an attempt.
//
// The attempt is bounded: at most one alternative, it consumes the
// plan's single recovery slot (so the bounded-execution invariant
// "1 fresh + 1 recovery per stage" still holds), and it never runs
// during normal application startup.

package synth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/flowbyte-com/mpm-core/config"
)

// AuthScheme names how a credential is presented to an endpoint.
//
// The vocabulary lives in the config package so that the configuration
// layer can VALIDATE an operator's value: the config package cannot
// import this one (this package imports config), so the canonical set
// has to sit below both.
//
// The alias keeps every existing `synth.AuthScheme...` reference
// working.
type AuthScheme = config.AuthScheme

const (
	// AuthSchemeNone sends no credential at all. Correct for
	// locally-hosted servers that run without authentication.
	AuthSchemeNone = config.AuthSchemeNone

	// AuthSchemeBearer sends `Authorization: Bearer <credential>`.
	// Used by OpenAI-compatible endpoints and by MiniMax on both of
	// its surfaces (including the Anthropic-compatible one).
	AuthSchemeBearer = config.AuthSchemeBearer

	// AuthSchemeXAPIKey sends `X-Api-Key: <credential>`. Used by the
	// Anthropic API and by Anthropic-protocol proxies.
	AuthSchemeXAPIKey = config.AuthSchemeXAPIKey
)

// ParseAuthScheme parses an operator-supplied scheme name.
//
// The accepted spellings are the canonical labels plus the obvious
// punctuation variants an operator might type. An empty string is NOT a
// scheme - it means "no override", and the caller falls through to the
// next resolution tier.
//
// An unrecognised value returns ok=false. The caller MUST NOT treat
// that as "no override": an operator who wrote `auth_scheme: "basic"`
// asked for something specific, and silently substituting a derived
// scheme hides the mistake behind a confusing 401.
func ParseAuthScheme(raw string) (AuthScheme, bool) {
	return config.ParseAuthScheme(raw)
}

// authHeaderFor returns the (header name, header value) pair that
// presents credential under this scheme.
//
// present is false when NO header is sent: either the scheme sends no
// credential, or there is no credential to send. Sending an empty
// `Authorization: Bearer ` to an endpoint that needs no credential is
// noise, and some servers reject a malformed Authorization header
// outright - an absent header is the honest representation of "no
// credential".
//
// It is a package-level function rather than a method because the
// scheme type is an alias of the config package's type, and Go does not
// allow methods to be declared on a non-local type.
func authHeaderFor(scheme AuthScheme, credential string) (name string, value string, present bool) {
	if credential == "" {
		return "", "", false
	}
	switch scheme {
	case AuthSchemeBearer:
		return "Authorization", "Bearer " + credential, true
	case AuthSchemeXAPIKey:
		return "X-Api-Key", credential, true
	case AuthSchemeNone:
		return "", "", false
	}
	return "", "", false
}

// protocolDefaultAuthScheme returns the scheme an endpoint gets when
// nothing better is known about it.
//
// The default FOLLOWS THE PROTOCOL, because that is how the two
// ecosystems actually work: OpenAI-compatible endpoints take a bearer
// token, Anthropic-protocol endpoints take X-Api-Key. MiniMax is the
// documented exception and is handled by documentedAuthScheme, not by
// a special case in the protocol dispatch.
func protocolDefaultAuthScheme(w wireShape) AuthScheme {
	if w == wireOpenAI {
		return AuthSchemeBearer
	}
	return AuthSchemeXAPIKey
}

// documentedBearerDomains are the API domains whose own documentation
// specifies bearer authentication.
//
// Membership is by DOMAIN, matched against the parsed URL host: the
// host must equal the domain or be a subdomain of it. A substring
// search over the whole base URL would also match
// `https://notminimax.io`, `https://minimax.io.example.com` and a
// query string containing the word — all of which are somebody else's
// server, and one of which would send the operator's credential with
// the wrong header shape.
//
// miniMax's Anthropic-compatible surface is
// `POST {base}/anthropic/v1/messages` with
// `Authorization: Bearer <MINIMAX_API_KEY>`; its OpenAI-compatible
// surface is `POST {base}/v1/chat/completions` with the same header.
//
// WHAT THE DOMAIN RULE COVERS — AND WHAT IT DOES NOT.
//
// The domain rule covers the AUTH half only: it sets Bearer for both of
// MiniMax's surfaces, and it does so by matching the parsed host, so
// `notminimax.io` and `minimax.io.example.com` are somebody else's
// server.
//
// The PROTOCOL half is still inferred from the URL. `https://api.minimax.io/v1`
// contains none of the OpenAI keywords, so the wire is the Anthropic one
// and the request path is `/messages` — not MiniMax's OpenAI-compatible
// `/v1/chat/completions`. The domain rule does NOT close that gap; an
// earlier version of this comment claimed it did, and the claim was
// wrong.
//
// The protocol is a separate concern from the authentication scheme,
// and the two are resolved independently: a correct MiniMax
// configuration is one whose base URL names the surface it means
// (`/anthropic/v1` for the Anthropic-compatible surface).
var documentedBearerDomains = []string{
	"minimax.io",
	"minimaxi.com",
}

// documentedAuthScheme returns the scheme documented by the endpoint
// itself, when the base URL names that endpoint unambiguously.
func documentedAuthScheme(baseURL string) (AuthScheme, bool) {
	host := endpointHost(baseURL)
	if host == "" {
		return "", false
	}
	for _, domain := range documentedBearerDomains {
		if hostMatchesDomain(host, domain) {
			return AuthSchemeBearer, true
		}
	}
	return "", false
}

// endpointHost extracts the lowercased hostname from a base URL.
//
// The URL is parsed rather than pattern-matched, so a malformed base
// URL (the operator's `http:://localhost:11434` typo) yields an empty
// host rather than a match on some substring. A base URL without a
// scheme is retried with "https://" so that an operator who omits it
// still gets the documented rule applied.
func endpointHost(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	return strings.TrimSuffix(host, ".")
}

// hostMatchesDomain reports whether host is domain or a subdomain of
// it. Both sides must already be normalized (lowercase, no trailing
// dot).
func hostMatchesDomain(host, domain string) bool {
	if host == "" || domain == "" {
		return false
	}
	if host == domain {
		return true
	}
	return strings.HasSuffix(host, "."+domain)
}

// derivedAuth resolves the scheme WITHOUT the operator's override:
//
//  1. the endpoint's own documented rule, matched on the parsed URL host
//     against an exact domain set;
//  2. a scheme negotiated during an approved connection verification for
//     this exact (base URL, credential) pair;
//  3. the protocol default: OpenAI-compatible -> Bearer,
//     Anthropic-compatible -> X-Api-Key.
//
// The protocol default follows the client's ACTUAL wire rather than a URL
// re-derivation, so the protocol and the scheme can never disagree.
func (sc *SynthClient) derivedAuth() AuthScheme {
	if s, ok := documentedAuthScheme(sc.BaseURL); ok {
		return s
	}
	if s, ok := negotiatedAuth(sc.BaseURL, sc.APIKey); ok {
		return s
	}
	return protocolDefaultAuthScheme(sc.Wire)
}

// ResolveAuthScheme resolves the authentication scheme for a base URL,
// inferring the message protocol from the URL.
//
// Deprecated: prefer the client's own resolution
// (SynthClient.resolveAuth), so that the protocol and the scheme can
// never disagree - a client whose protocol was set explicitly must use
// that protocol's default, not one re-derived from the URL.
//
// This helper cannot express a REFUSAL, so an unrecognised override
// falls through to a derived scheme - the behaviour it has always had,
// kept here so that existing callers keep working. The RUNTIME path
// behaves differently and better: SynthClient.resolveAuth refuses an
// unrecognised override with an explicit error, and the configuration
// layer validates the operator's value (config.ValidateAuthScheme).
//
// The resolution order is unchanged: a recognised override wins, then the
// endpoint's documented rule, then a negotiated scheme, then the
// protocol default.
func ResolveAuthScheme(baseURL, credential, override string) AuthScheme {
	if s, ok := ParseAuthScheme(override); ok {
		return s
	}
	sc := &SynthClient{
		BaseURL: baseURL,
		APIKey:  credential,
		Wire:    inferWire(baseURL),
	}
	return sc.derivedAuth()
}

// resolveAuth is the client's own authentication-scheme resolution.
//
// Order (first match wins):
//
//  1. the operator's explicit override (config.Profile.AuthScheme);
//  2. the endpoint's own documented rule, matched on the parsed URL
//     host against an exact domain set;
//  3. a scheme negotiated during an approved connection verification
//     for this exact (base URL, credential) pair;
//  4. the protocol default: OpenAI-compatible -> Bearer,
//     Anthropic-compatible -> X-Api-Key.
//
// The protocol default follows the client's ACTUAL wire rather than a
// URL re-derivation, so the protocol and the scheme can never disagree.
//
// An UNRECOGNISED override is an ERROR, not a fall-through.
//
// The override used to be parsed with a matcher that returned ("", false)
// for anything it did not recognise, and the resolution simply moved on
// to the next tier. An operator who wrote `auth_scheme: "basic"` - or
// simply misspelled the value - was therefore never told: the endpoint
// was authenticated with a DERIVED scheme and answered 401, and the
// symptom pointed in exactly the wrong direction.
//
// Resolution happens per request rather than once at construction, so a
// scheme discovered by a bounded negotiation is retained for subsequent
// operations on the same endpoint without any shared mutable client
// state.
func (sc *SynthClient) resolveAuth() (AuthScheme, error) {
	if strings.TrimSpace(sc.AuthOverride) != "" {
		if s, ok := ParseAuthScheme(sc.AuthOverride); ok {
			return s, nil
		}
		return "", fmt.Errorf("unsupported auth_scheme %q in the profile (supported: %s)",
			strings.TrimSpace(sc.AuthOverride), strings.Join(config.SupportedAuthSchemes(), ", "))
	}
	return sc.derivedAuth(), nil
}

// negotiatedAuthCache retains the scheme discovered by a successful
// bounded negotiation, keyed by the exact (base URL, credential) pair.
//
// Changing the endpoint or the credential changes the key, so the
// retained entry is invalidated implicitly and exactly when it should
// be.
//
// This is bounded operational state, not authoritative configuration:
// it records an observation, it is re-derivable by repeating the
// verification, and it never overrides an explicit operator override or
// the endpoint's documented rule.
//
// The retention lasts for the life of the PROCESS. An operator who wants
// a discovered scheme to survive a restart pins it durably with
// `mpm config profile set <name> auth_scheme <scheme>`; that is the
// operator's decision to make, not something a probe should write into
// their configuration behind their back.
var negotiatedAuthCache = struct {
	mu sync.RWMutex
	m  map[string]AuthScheme
}{m: map[string]AuthScheme{}}

// maxNegotiatedAuthEntries bounds the cache. An install has a handful
// of profiles; the bound exists only so a misconfigured install that
// churns endpoints cannot grow the map without limit.
const maxNegotiatedAuthEntries = 32

// negotiatedAuthKey derives the cache key from the endpoint and the
// credential. The credential is hashed, never stored as a key, so the
// map holds no recoverable secret.
func negotiatedAuthKey(baseURL, credential string) string {
	h := sha256.New()
	h.Write([]byte(baseURL))
	h.Write([]byte{0})
	h.Write([]byte(credential))
	return hex.EncodeToString(h.Sum(nil))
}

// rememberNegotiatedAuth records the scheme that a bounded negotiation
// proved works for this endpoint and credential.
func rememberNegotiatedAuth(baseURL, credential string, scheme AuthScheme) {
	if baseURL == "" || credential == "" || scheme == "" {
		return
	}
	negotiatedAuthCache.mu.Lock()
	defer negotiatedAuthCache.mu.Unlock()
	if len(negotiatedAuthCache.m) >= maxNegotiatedAuthEntries {
		for k := range negotiatedAuthCache.m {
			delete(negotiatedAuthCache.m, k)
			break
		}
	}
	negotiatedAuthCache.m[negotiatedAuthKey(baseURL, credential)] = scheme
}

// negotiatedAuth returns the retained scheme for this endpoint and
// credential, if one was negotiated.
func negotiatedAuth(baseURL, credential string) (AuthScheme, bool) {
	if baseURL == "" || credential == "" {
		return "", false
	}
	negotiatedAuthCache.mu.RLock()
	defer negotiatedAuthCache.mu.RUnlock()
	s, ok := negotiatedAuthCache.m[negotiatedAuthKey(baseURL, credential)]
	return s, ok
}

// AuthFailureKind distinguishes the sub-cases of an authentication
// refusal.
//
// The distinction matters because the correct response to each is
// different, and because a bounded negotiation must never fire on a
// refusal that repeating the request cannot fix.
type AuthFailureKind string

const (
	// AuthFailureNone means the failure was not an authentication
	// refusal.
	AuthFailureNone AuthFailureKind = ""

	// AuthFailureInvalidCredential means HTTP 401: the endpoint
	// rejected the credential. A different authentication header will
	// not help — the credential itself is wrong, expired or revoked.
	AuthFailureInvalidCredential AuthFailureKind = "invalid_credential"

	// AuthFailureSubscription means HTTP 402: the account is not
	// billed / the subscription does not cover the request. The
	// credential may be perfectly valid.
	AuthFailureSubscription AuthFailureKind = "subscription"

	// AuthFailurePermission means HTTP 403: the credential was
	// accepted but is not permitted to perform this request.
	AuthFailurePermission AuthFailureKind = "permission"
)

// String returns the canonical lowercase label.
func (k AuthFailureKind) String() string { return string(k) }

// ClassifyAuthFailure classifies an authentication refusal by HTTP
// status.
//
// The three cases stay distinguishable all the way to the operator:
// an invalid credential, an unbilled account and an under-privileged
// key are three different problems with three different remedies.
//
// The status alone is the discriminator: a provider that answers 402
// is telling the operator about billing, whatever its body says.
func ClassifyAuthFailure(status int) AuthFailureKind {
	switch status {
	case http.StatusUnauthorized:
		return AuthFailureInvalidCredential
	case http.StatusPaymentRequired:
		return AuthFailureSubscription
	case http.StatusForbidden:
		return AuthFailurePermission
	}
	return AuthFailureNone
}
