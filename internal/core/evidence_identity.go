// evidence_identity.go — the observation identity behind an evidence row.
//
// WHY THIS FILE EXISTS.
//
// An evidence row records an OBSERVATION about an artifact. A delivery of that
// observation is a separate act: the same observation can be delivered twice
// (a retry, a replayed collector, a critic that re-evaluates every cycle),
// and two DIFFERENT observations can carry identical text.
//
// Before this mechanism the substrate could not tell those apart. Every
// delivery minted a fresh row id (internal/core/evidence_store.go), the
// evidence table carried no uniqueness guarantee beyond its primary key, and
// the confidence recompute summed every non-expired row in full
// (internal/core/confidence.go: computeConfidence). Two deliveries of ONE
// observation therefore raised the artifact's confidence twice — repetition
// mistaken for corroboration. That is defects D1 and D5 in the M5 ledger.
//
// THE MODEL.
//
//	observation identity — WHAT was observed, and therefore which deliveries
//	                      are the same claim about the world.
//	delivery            — WHEN and BY WHOM it was submitted.
//
// The correction makes the identity durable and lets the substrate enforce it
// at the storage boundary.
//
// THE MECHANISM — no schema change, no migration, no new index.
//
// The evidence row's own id becomes the observation's identity: it is derived
// deterministically from the observation's identity material, and the table's
// PRIMARY KEY becomes the uniqueness guarantee — "at most one stored row per
// observation per artifact". That is the same storage-level guarantee
// mpm-event/0.1 §3.5 gives a strict-mode event id, expressed with what the
// substrate already has: an existing identifier and an existing uniqueness
// guarantee.
//
// Legacy rows keep their random ids. A derived id has a distinct prefix and a
// distinct length, so the two populations can never collide, no legacy row
// needs rewriting, and no migration is needed.
//
// TWO IDENTITY MODES.
//
//	strict      — the caller supplies a stable observation key
//	              (EvidenceInput.ObservationKey). The key alone determines the
//	              identity: the same key is the same observation, and two
//	              different keys are two different observations even when their
//	              text is identical. This is how a caller distinguishes "two
//	              independent observations that happen to read the same" from
//	              "one observation delivered twice".
//
//	best_effort — no key was supplied. The identity is derived from the
//	              submission's semantic fingerprint, so two submissions that
//	              agree in every semantic dimension are ONE observation. This
//	              is the conservative direction: a delivery that carries no
//	              distinguishing information cannot be proven to be a second
//	              observation, and under-counting corroboration is safe while
//	              over-counting it is not.
//
// The fingerprint deliberately EXCLUDES the delivery-time and lifetime
// metadata — created_at (when it was delivered), expires_at (how long it
// lives) and reference_url (where a source is published, which is pinned not
// to affect strength, confidence or verification) — so a replay that differs
// only in those respects is still recognised as the same observation.
//
// ONE EXCEPTION TO "created_at IS NOT PART OF THE IDENTITY": when the
// CALLER states when the observation was made, that statement IS part of the
// identity. The substrate's own delivery timestamp is never part of it — a
// replay arrives later by definition — but a caller that says "I observed
// this at T" has said something about the observation that a second
// observation at T' does not say. That is how a caller records several
// observations of the same thing at different times (three checkpoints, two
// challenges from two reviewers) without needing an observation key.
//
// The fingerprint is length-prefixed, not delimiter-joined: a delimiter inside
// a note would otherwise let two different submissions produce the same
// fingerprint. (Same discipline as the mpm-event/0.1 derivation.)
//
// WHAT A REPLAY DOES.
//
// A replay is not an error and it is not a write:
//
//   - no second evidence row;
//   - no confidence recompute, so no confidence_history row and no change to
//     the artifact's confidence;
//   - no work-verification re-derivation and no evidence_observed event,
//     because the evidence set did not change.
//
// The one exception is a reference URL the replay carries and the stored row
// does not: it is recorded on the stored row (filling a NULL), because
// dropping caller-supplied provenance would lose data, and a reference URL is
// provably confidence-neutral.
//
// A replay never resurrects a neutralized observation. The challenge path
// (F7.1) and the contradiction resolver deliberately expire the rows they
// neutralize; a replay must not undo that, so the stored row stays
// authoritative.
//
// WHAT A REPLAY MAY NOT DO.
//
// In strict mode, the same key delivered with DIFFERENT content is a
// conflict, not a replay: the substrate refuses it (fail safely, no partial
// state) rather than silently keeping one of the two claims. The comparison
// is made against the stored row's own content, so the check is a fact about
// the ledger rather than an assumption about the caller.
//
// SCOPE.
//
// The mechanism lives entirely inside internal/core. The agent-facing
// surfaces (the mpm_evidence tool schema and the `mpm evidence add` flags)
// are NOT changed here: exposing a caller-supplied observation key is an
// externally visible API change, and it is Session A's to approve. Until it
// is, the best_effort mode is what every existing caller gets — which is
// enough to close D1 and D5, because the defect is about a DELIVERY being
// counted twice, not about a caller being unable to name an observation.
package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// EvidenceObservationKeyMaxChars caps the length of a caller-supplied
// observation key. A key is an identifier, not prose; a bound keeps the
// derived identity material small and predictable.
const EvidenceObservationKeyMaxChars = 256

// evidenceRowIDPrefix marks an evidence row id that was DERIVED from an
// observation identity rather than minted at random.
//
// Legacy rows carry random 16-character hex ids from GenerateID(). A derived
// id is prefixed and longer, so the two populations can never collide and no
// legacy row has to be rewritten. The prefix also makes the derived
// population auditable: an operator can tell an identity-backed row from a
// legacy one by looking at the id.
const evidenceRowIDPrefix = "obv1_"

// evidenceRowIDHexChars is the length of the hex digest inside a derived
// evidence row id.
const evidenceRowIDHexChars = 32

// EvidenceObservationMode names how an observation's identity was determined.
type EvidenceObservationMode string

const (
	// EvidenceObservationStrict means the caller supplied a stable
	// observation key, and that key alone determined the identity.
	EvidenceObservationStrict EvidenceObservationMode = "strict"
	// EvidenceObservationBestEffort means the identity was derived from the
	// submission's semantic fingerprint, because no key was supplied.
	EvidenceObservationBestEffort EvidenceObservationMode = "best_effort"
)

// EvidenceOutcome reports what one evidence delivery did.
type EvidenceOutcome struct {
	// EvidenceID is the identity of the stored evidence row. It is the same
	// id for every delivery of the same observation.
	EvidenceID string
	// Duplicate is true when the delivery was recognised as a replay of an
	// observation that is already recorded: nothing was written.
	Duplicate bool
	// Mode is how the observation's identity was determined.
	Mode EvidenceObservationMode
}

// observationIdentity is the derived identity of one observation.
type observationIdentity struct {
	// Mode is how the identity was determined.
	Mode EvidenceObservationMode
	// RowID is the storage identity of the observation: the evidence row's
	// primary key, derived rather than minted.
	RowID string
	// ContentDigest is the semantic fingerprint of the observation's
	// content. It is used to detect a same-key delivery that contradicts
	// what is already recorded.
	ContentDigest string
}

// deriveObservationIdentity computes the durable identity of the observation
// an evidence submission carries.
//
// The identity is scoped to the artifact: the same observation key attached
// to two artifacts is two rows, because confidence is computed per artifact
// and one artifact's corroboration is not another's.
//
// The caller must derive the identity BEFORE the substrate fills in a
// delivery timestamp: whether the caller stated a time is itself part of the
// identity, and the fill-in would erase that distinction.
func deriveObservationIdentity(in EvidenceInput) (observationIdentity, error) {
	if err := validateObservationKey(in.ObservationKey); err != nil {
		return observationIdentity{}, err
	}

	digest := evidenceContentDigest(in)

	mode := EvidenceObservationBestEffort
	material := "h:" + digest
	switch {
	case in.ObservationKey != "":
		mode = EvidenceObservationStrict
		material = "k:" + in.ObservationKey
	case !in.CreatedAt.IsZero():
		// The caller stated when the observation was made. That statement
		// identifies the observation: two observations of the same thing at
		// two stated times are two observations.
		material = "h:" + digest + ":" + strconv.FormatInt(in.CreatedAt.UnixNano(), 10)
	}

	return observationIdentity{
		Mode:          mode,
		ContentDigest: digest,
		RowID:         derivedEvidenceRowID(in.ArtifactType, in.ArtifactID, mode, material),
	}, nil
}

// validateObservationKey rejects an observation key that cannot identify an
// observation unambiguously.
//
// An empty key is valid and means "no key supplied" — the caller did not
// identify the observation, and the best_effort fingerprint applies.
//
// The key is used VERBATIM as identity material: two keys that differ in any
// byte are two different observations. That is why an ambiguous key is
// rejected rather than normalized — normalizing " obs-1 " and "obs-1" into
// one identity would silently merge two callers' observations, and refusing
// them tells the caller exactly what to fix.
//
// The key is never persisted: only its SHA-256 digest reaches the stored
// identity. It is therefore not scanned for sensitive content the way a
// stored field is, and it is never echoed into an error message.
func validateObservationKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > EvidenceObservationKeyMaxChars {
		return fmt.Errorf("observation key is too long: %d chars, maximum %d",
			len(key), EvidenceObservationKeyMaxChars)
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("observation key is empty or whitespace-only: it would identify no observation")
	}
	if key != strings.TrimSpace(key) {
		return fmt.Errorf("observation key must not carry leading or trailing whitespace: " +
			"the key is used verbatim, so padding would identify a different observation")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("observation key must not contain control characters")
		}
	}
	return nil
}

// evidenceContentDigest is the semantic fingerprint of an evidence
// submission: what it claims, who claimed it, and how much it is worth.
//
// The delivery-time and lifetime metadata are deliberately absent:
// created_at says when the submission arrived, expires_at says how long the
// row lives, and reference_url says where a source is published (pinned not
// to affect strength, confidence or verification). None of them is part of
// WHAT was observed, and including them would make a replay look like a new
// observation.
func evidenceContentDigest(in EvidenceInput) string {
	material := observationMaterial(
		in.Type,
		in.SourceGroup,
		strconv.FormatFloat(in.Strength, 'g', -1, 64),
		strconv.FormatFloat(in.IndependenceFactor, 'g', -1, 64),
		in.CreatedBy,
		in.Notes,
	)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// derivedEvidenceRowID computes the storage identity of an observation.
//
// The id is a pure function of (artifact, mode, identity material), so the
// same observation always lands on the same primary key — and the primary
// key is what enforces "at most one stored row per observation".
func derivedEvidenceRowID(artifactType, artifactID string, mode EvidenceObservationMode, material string) string {
	encoded := observationMaterial(
		"mpm-evidence-observation/1",
		artifactType,
		artifactID,
		string(mode),
		material,
	)
	sum := sha256.Sum256([]byte(encoded))
	return evidenceRowIDPrefix + hex.EncodeToString(sum[:])[:evidenceRowIDHexChars]
}

// observationMaterial encodes the given fields length-prefixed.
//
// Length-prefixing removes the ambiguity a delimiter-joined encoding has: a
// note containing the delimiter cannot be mistaken for a field boundary, so
// two different submissions can never produce the same encoding.
func observationMaterial(fields ...string) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
	}
	return b.String()
}
