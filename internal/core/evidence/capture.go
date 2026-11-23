package evidence

import (
	"fmt"
	"os"
	"strings"
)

// The CAPTURED EVIDENCE.
//
// The host captures what a reviewer produced and writes it beside the
// review. The assignment store records the DIGEST of those bytes and a
// POINTER to them.
//
// # WHY THE PUBLISHER MUST CHECK THEM
//
// The digest is a CLAIM. A caller can put any 64 hex characters in the
// store's column, and the publisher that copies that column into a
// record without checking it publishes the claim as though it were
// evidence.
//
// So the publisher checks the digest against the bytes BEFORE it
// publishes either. Three answers, and they are different facts:
//
//   - verified: the bytes are there, and they hash to the digest;
//   - unverifiable: the bytes are somewhere the publisher cannot read,
//     so the digest is published as the EXPLICIT unknown;
//   - contradicted: the bytes are there, and they do NOT hash to the
//     digest, so the digest is REFUSED and the contradiction is
//     published.
//
// # WHY THE FINDINGS THEMSELVES ARE NOT PUBLISHED
//
// The findings are a model's output. The contract publishes a DIGEST and
// a POINTER, never the text — so an external reader cannot check the
// digest against the bytes, because the bytes are not published. That
// is not a hole in the design: it is what "the evidence is not public"
// means. What the reader CAN read is the publisher's own answer, and
// that is why the answer is published with the digest.

// The capture statuses.
const (
	// CaptureNone says nothing is known about the capture.
	CaptureNone = "none"

	// CaptureVerified says the bytes were read and they hash to the
	// published digest.
	CaptureVerified = "verified"

	// CaptureUnverifiable says the bytes could not be read, so the
	// digest is published as the explicit unknown.
	CaptureUnverifiable = "unverifiable"

	// CaptureContradicted says the bytes were read and they do NOT hash
	// to the digest the store recorded. The digest is refused.
	CaptureContradicted = "contradicted"
)

// VerifyCapturedEvidence checks a captured digest against the bytes it
// names.
//
// It returns the status and a reason. The reason is for the operator's
// own output — it names what could not be checked — and never reaches a
// published record, which carries the status alone.
//
// A digest that was never recorded is CaptureNone, and the caller
// publishes the explicit unknown without a ceremony.
func VerifyCapturedEvidence(digest, ref string) (string, string) {
	digest = strings.TrimSpace(digest)
	ref = strings.TrimSpace(ref)
	if digest == "" || digest == Unknown {
		return CaptureNone, "no digest was recorded"
	}
	if ref == "" || ref == Unknown {
		return CaptureUnverifiable, "the capture names no pointer to its bytes"
	}
	if strings.Contains(ref, "://") {
		// The bytes live somewhere else — a URL, or another
		// repository. The publisher is on the operator's machine and
		// cannot reach them from here, so it says so rather than
		// publishing a digest it did not check.
		return CaptureUnverifiable, "the capture's bytes live outside the publisher's reach"
	}
	raw, err := os.ReadFile(ref)
	if err != nil {
		return CaptureUnverifiable, fmt.Sprintf("the capture's bytes could not be read: %v", err)
	}
	got := DigestBytes(raw)
	if !SameDigest(got, digest) {
		return CaptureContradicted, fmt.Sprintf(
			"the capture's bytes hash to %s and the store says %s", got, digest)
	}
	return CaptureVerified, ""
}
