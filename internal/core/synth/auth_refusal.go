// auth_refusal.go — the typed authentication-refusal error.
//
// WHY THIS FILE EXISTS
//
// The bounded negotiation can make TWO requests: the first with the
// scheme MPM resolved, and — when the provider names a different one —
// a second with the named scheme. Each request can be refused with a
// DIFFERENT status.
//
// Only the FINAL attempt describes the failure the operator has to act
// on:
//
//	attempt #1 (X-Api-Key) → 401 {"error":"invalid api key"}
//	attempt #2 (Bearer)    → 402 {"error":"payment required"}
//
// The second refusal says the account is UNBILLED. The first says the
// credential is wrong. The remedies are completely different: paying, not
// rotating a key.
//
// The negotiated path used to report the FIRST attempt's status and
// discard the second one's, so an unbilled account was reported to the
// operator as an invalid credential. The operator would rotate a key that
// was fine.
//
// The status travels with the error as a typed field rather than being
// parsed back out of a formatted message. Parsing a status out of a
// message is exactly what made the two classifiers — the production one
// and the probe's duplicate — drift apart in the first place.
package synth

import "fmt"

// AuthRefusalError describes an authentication refusal, including the
// HTTP status of the attempt that produced it.
//
// The zero value is not meaningful: an AuthRefusalError always carries a
// Status.
type AuthRefusalError struct {
	// Class is the production failure class of the FINAL attempt — the
	// attempt whose status describes the failure the operator must act
	// on. It is what the operator-facing classifier reads.
	Class FailureClass

	// Status is the HTTP status of the FINAL attempt. Zero means the
	// final attempt never reached an HTTP response (a transport
	// failure).
	Status int

	// Scheme is the authentication scheme presented on the final
	// attempt.
	Scheme AuthScheme

	// InitialClass is the failure class of the FIRST attempt. It is the
	// zero value when no bounded alternative attempt was made.
	InitialClass FailureClass

	// InitialStatus is the HTTP status of the FIRST attempt. It is zero
	// when no bounded alternative attempt was made.
	InitialStatus int

	// InitialScheme is the authentication scheme presented on the first
	// attempt.
	InitialScheme AuthScheme

	// Detail is the final attempt's failure text — the provider's own
	// answer, which is the most diagnostic piece of information
	// available.
	Detail string
}

// Error renders the refusal.
//
// With no bounded alternative attempt the wording is exactly the wording
// MPM has always used for a single refused request. With one, BOTH
// statuses are named, so an operator can see what the provider said on
// the second attempt as well as on the first.
func (e *AuthRefusalError) Error() string {
	if e.InitialScheme == "" {
		return fmt.Sprintf("provider refused request (%s) with scheme %q: %s",
			e.Class, e.Scheme, e.Detail)
	}
	return fmt.Sprintf(
		"provider refused request (%s) with scheme %q (HTTP %d); the alternative authentication scheme %q was also refused (HTTP %d): %s",
		e.InitialClass, e.InitialScheme, e.InitialStatus, e.Scheme, e.Status, e.Detail)
}

// Unwrap exposes the underlying failure so that errors.Is / errors.As
// keep working across the layers.
func (e *AuthRefusalError) Unwrap() error { return nil }

// HasAlternativeAttempt reports whether a bounded alternative
// authentication attempt was made before this refusal.
func (e *AuthRefusalError) HasAlternativeAttempt() bool { return e.InitialScheme != "" }
