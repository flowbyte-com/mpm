// redirect.go — the credential-bearing redirect guard.
//
// WHY THIS FILE EXISTS
//
// A request that carries an operator's credential must not be allowed to
// carry it to a second origin. Go's standard library is not enough:
//
//   - it drops `Authorization` on a cross-host redirect, but it treats
//     `X-Api-Key` as an ordinary header and copies it ANYWHERE;
//   - it copies `Authorization` to a SUBDOMAIN of the original host
//     (`isDomainOrSubdomain`), so `api.example.com` →
//     `evil.api.example.com` forwards the bearer token.
//
// The first version of this guard (in do_request.go) closed the hole for
// the synthesis client only, compared the raw "host:port" strings, and
// reported its refusals as transport failures. This file is the corrected,
// SHARED guard:
//
//   - the comparison is on the ORIGIN (scheme, host, EFFECTIVE port), so a
//     same-origin redirect that spells out the default port is allowed
//     while a subdomain, a scheme change or a port change is not;
//   - a refusal is a typed, identifiable outcome (`ErrCredentialRedirectRefused`),
//     so the caller can report it as a refusal instead of a reachability
//     problem and can refuse to retry it;
//   - the embedding providers use it too, so a bearer token sent to an
//     embedding endpoint cannot be forwarded to a subdomain.
//
// WHAT "SAME ORIGIN" MEANS HERE
//
// An origin is (scheme, host, port) — RFC 6454 §4.1. The port is the
// EFFECTIVE port: the explicit port when the URL gives one, otherwise the
// scheme's default (443 for https, 80 for http).
//
// Comparing the raw "host:port" strings — as the first version did —
// produces two wrong answers:
//
//   - `https://api.example.com` → `https://api.example.com:443` is
//     REFUSED as "cross-origin" when it is the same origin. The failure is
//     closed, but the diagnostic actively misleads the operator.
//   - `http://api.example.com` → `https://api.example.com` is ALLOWED
//     when it is a different origin (different scheme, and the effective
//     port changes from 80 to 443).
//
// Both are corrected by comparing (scheme, hostname, effective port).
package synth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrCredentialRedirectRefused is the sentinel wrapped by every deliberate
// refusal to follow a redirect that would carry a credential away from the
// configured origin.
//
// It exists so that a refusal is DISTINGUISHABLE from a transport failure.
// A refused redirect is a decision this client made on the operator's
// behalf; it must not consume a retry (repeating the identical request
// would be refused identically), and it must never be reported to the
// operator as "the endpoint is unreachable" — the endpoint answered.
//
// Recognise it with errors.Is.
var ErrCredentialRedirectRefused = errors.New("synth: refusing credential-bearing redirect")

// RedirectRefusalError describes one refused redirect.
//
// It carries the two origins and a one-clause reason. The reason names the
// difference that caused the refusal (host, scheme or port) so the operator
// can tell a subdomain redirect from a port change without reading source.
type RedirectRefusalError struct {
	// From is the origin the request was configured against.
	From string
	// To is the origin the redirect would have carried the credential to.
	To string
	// Reason explains why the redirect was refused.
	Reason string
}

// Error renders the refusal. The sentinel's own text is deliberately not
// repeated here: the sentinel is for errors.Is, the message is for humans.
func (e *RedirectRefusalError) Error() string {
	return fmt.Sprintf("refusing credential-bearing redirect from %s to %s: %s", e.From, e.To, e.Reason)
}

// Unwrap makes errors.Is(err, ErrCredentialRedirectRefused) true.
func (e *RedirectRefusalError) Unwrap() error { return ErrCredentialRedirectRefused }

// GuardCredentialRedirect refuses a redirect that would carry the request —
// and therefore the operator's credential — away from the origin they
// configured.
//
// It is installed as http.Client.CheckRedirect. A redirect is ALLOWED only
// when it stays on the same origin: same scheme, same host, same effective
// port.
//
// A SAME-ORIGIN redirect is allowed. The credential stays on the origin the
// operator configured, so nothing is disclosed.
//
// Everything else is refused:
//
//   - a different host — including a SUBDOMAIN of the configured host;
//   - a different scheme — including an https→http downgrade, which would
//     send the credential in cleartext, and an http→https upgrade, which is
//     a different origin (and changes the effective port);
//   - a different effective port.
//
// Refusing is the only behaviour that keeps the credential on the
// configured origin. Go's own redirect handling is strictly weaker: it
// forwards `X-Api-Key` to ANY host and `Authorization` to any subdomain.
//
// The error returned is a *RedirectRefusalError, so callers can recognise
// it with errors.Is(err, ErrCredentialRedirectRefused).
func GuardCredentialRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	origin := via[0].URL
	if sameOrigin(origin, req.URL) {
		return nil
	}
	return &RedirectRefusalError{
		From:   origin.Scheme + "://" + origin.Host,
		To:     req.URL.Scheme + "://" + req.URL.Host,
		Reason: redirectRefusalReason(origin, req.URL),
	}
}

// sameOrigin reports whether two URLs denote the same origin.
//
// An origin is (scheme, host, port) per RFC 6454 §4.1. The port compared
// is the EFFECTIVE port — the explicit port when the URL gives one,
// otherwise the scheme's default — so `https://api.example.com` and
// `https://api.example.com:443` are the same origin while
// `https://api.example.com:8443` is not.
func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

// effectivePort returns the port a URL actually talks to: the explicit
// port when the URL carries one, otherwise the scheme's default.
func effectivePort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// redirectRefusalReason names the difference that caused a refusal.
//
// The order matters: a host difference is the most common case (a
// subdomain), so it is reported first.
func redirectRefusalReason(origin, target *url.URL) string {
	switch {
	case !strings.EqualFold(origin.Hostname(), target.Hostname()):
		return "the credential would leave the configured origin (a different host — including a subdomain — is a different origin)"
	case !strings.EqualFold(origin.Scheme, target.Scheme):
		if strings.EqualFold(origin.Scheme, "https") {
			return "the credential would be sent in cleartext (" + origin.Scheme + " → " + target.Scheme + ")"
		}
		return "the credential would leave the configured origin (" + origin.Scheme + " → " + target.Scheme + " is a different origin)"
	case effectivePort(origin) != effectivePort(target):
		return "the credential would leave the configured origin (port " + effectivePort(target) + " is not port " + effectivePort(origin) + ")"
	default:
		return "the credential would leave the configured origin"
	}
}
