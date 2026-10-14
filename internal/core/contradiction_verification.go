// contradiction_verification.go — the gate between "a pair was noticed"
// and "a memory may be penalised".
//
// Why this file exists (issue #10)
//
// shared.contradiction_log holds two different kinds of row under one
// table, distinguished only by detected_by:
//
//   - UNVERIFIED SIMILARITY CANDIDATES, written by --propose. Cosine
//     similarity shows two memories are RELATED. It does not show their
//     claims conflict. Near-identical duplicates score at the TOP of that
//     range, so the great majority of candidates are duplicates.
//   - CONTRADICTIONS an operator has adjudicated: a human read both
//     memories and concluded the claims actually conflict.
//
// LoadContradictionQueue selected detected_by and no consumer read it, so
// --apply resolved a candidate exactly as though it were an established
// conflict: the loser's weight was slashed, status became 'challenged',
// and a resolution memory was written. On a duplicate-heavy store that
// penalises a large population of memories that never contradicted
// anything — the same failure mode the 2026-10-13 retrieval write-path
// removal was made to stop, re-exposed one layer up at the operator CLI.
//
// The rule this file enforces
//
//	Verification is affirmative, never inferred.
//
// A row is eligible to penalise a memory only when detected_by carries an
// explicit operator adjudication. Everything else — a similarity candidate,
// a row that went to arbitration, a legacy row with a session id, a row
// with no detected_by at all, a value written by something this codebase
// has never heard of — is unverified and fails closed.
//
// Two properties are deliberately NOT used as evidence of verification,
// because neither can establish one:
//
//   - similarity score. A high cosine is the reason the row exists, not
//     the reason the claims conflict.
//   - the row being old, or being in the queue at all. Nothing about
//     being queued is a judgement.
//
// Why no schema change
//
// The `adjudicated:` segment reuses the compound detected_by idiom that
// applyArbitration already established for `theory:` (resolve.go:404): an
// additive provenance segment appended to one row by id, guarded by
// `resolved_at IS NULL`. That keeps the live schema untouched — no
// migration, no new column, no backfill — and, critically, preserves the
// recorded-state asymmetry docs/RECALL-MUTATION.md promises: rows
// predating this work keep their original detected_by and are read
// exactly as written, never reinterpreted.
//
// The segment is appended, never rewritten, so the detection provenance
// that brought a row to the queue stays legible next to the judgement
// made about it.

package internal

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"
)

// detected_by segment vocabulary.
//
// detected_by is a '|'-delimited provenance string, not an enum. Every
// writer appends; nothing overwrites. The segments this codebase writes:
//
//	similarity-scan   the row came from an explicit --propose scan
//	theory:<id>       the row was routed to arbitration (resolve.go)
//	adjudicated:<why> an operator concluded the claims actually conflict
const (
	// contradictionTheorySegmentPrefix is written by applyArbitration. It
	// records that a theory was proposed for this row, which is a triage
	// flag and NOT a verdict — the queue row stays open precisely because
	// nothing has been decided.
	contradictionTheorySegmentPrefix = "theory:"

	// contradictionAdjudicatedSegmentPrefix is written by
	// AdjudicateContradiction and is the ONLY thing that makes a row
	// eligible to penalise a memory.
	contradictionAdjudicatedSegmentPrefix = "adjudicated:"
)

// maxAdjudicationReasonBytes bounds the operator-supplied text stored in
// detected_by. The column is a provenance string that operators read in
// `mpm ops resolve-contradictions` output; it is not a document store.
const maxAdjudicationReasonBytes = 200

// ContradictionVerification is the verdict of
// ClassifyContradictionVerification.
type ContradictionVerification struct {
	// Adjudicated is true only when detected_by carries an explicit
	// operator adjudication. It is the single input to whether a memory
	// may be slashed.
	Adjudicated bool

	// DetectedBy is the raw value classified, kept so an error message
	// can show the operator exactly what was on the row.
	DetectedBy string

	// Reason explains, in operator terms, why an unadjudicated row is not
	// eligible. Empty when Adjudicated is true.
	Reason string
}

// ClassifyContradictionVerification decides whether a queue row has been
// adjudicated by an operator.
//
// Pure: no database access, no writes, no side effects. The same input
// always yields the same verdict, so a row's eligibility is a property of
// the row rather than of the run.
//
// Fail-closed by construction. The function looks for exactly one thing —
// an `adjudicated:<reason>` segment — and treats its absence as the answer.
// It does not ask whether a value looks plausible, whether a row is
// familiar, or whether a threshold was met. A new detection source added
// in the future is therefore unverified until someone adjudicates it,
// which is the correct default: the cost of that is one extra operator
// step, and the cost of the opposite mistake is slashing memories that
// were never in conflict.
//
// Order of adjudication over source matters only in that an adjudication
// segment found anywhere wins. A row that went to arbitration before being
// adjudicated carries `similarity-scan|theory:t1|adjudicated:...` and is
// adjudicated; a row carrying only the first two is not.
func ClassifyContradictionVerification(detectedBy string) ContradictionVerification {
	v := ContradictionVerification{DetectedBy: detectedBy}

	segments := contradictionSegments(detectedBy)
	for _, seg := range segments {
		rest, ok := strings.CutPrefix(seg, contradictionAdjudicatedSegmentPrefix)
		if ok && strings.TrimSpace(rest) != "" {
			v.Adjudicated = true
			return v
		}
	}

	switch {
	case len(segments) == 0:
		v.Reason = "queue row carries no detected_by provenance, so no operator has adjudicated it. " +
			"Treat it as an unverified review item and adjudicate it explicitly before applying."

	case containsSegment(segments, similarityCandidateSource):
		v.Reason = "unverified similarity candidate: a high cosine score shows two memories are related, " +
			"not that their claims conflict. Near-identical duplicates score highest, so this pair may be " +
			"a duplicate rather than a contradiction. Read both memories, then adjudicate it explicitly."

	case hasPrefixSegment(segments, contradictionTheorySegmentPrefix):
		v.Reason = "queued for arbitration but not adjudicated: a theory was proposed, which is a triage " +
			"flag and not a verdict. Adjudicate it explicitly before applying."

	default:
		v.Reason = fmt.Sprintf("unrecognised detected_by provenance %q: this value is not an operator "+
			"adjudication, so the row cannot be assumed to have been reviewed. Adjudicate it explicitly "+
			"before applying.", detectedBy)
	}
	return v
}

// contradictionSegments splits a detected_by value into its provenance
// segments, trimming each and dropping empties. A row whose detected_by is
// NULL, empty, or only separators yields no segments at all — which is the
// fail-closed case, not an error.
func contradictionSegments(detectedBy string) []string {
	if strings.TrimSpace(detectedBy) == "" {
		return nil
	}
	parts := strings.Split(detectedBy, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsSegment(segments []string, want string) bool {
	for _, s := range segments {
		if s == want {
			return true
		}
	}
	return false
}

func hasPrefixSegment(segments []string, prefix string) bool {
	for _, s := range segments {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// stripForgedAdjudication removes any `adjudicated:` segment from a
// provenance value on the queue-CREATION path, reporting whether one was
// present.
//
// AdjudicateContradiction is the only writer that may introduce an
// adjudication segment, because it is the only action that corresponds to an
// operator having read both memories. EnqueueContradictionReport is not that
// action — it is how a pair enters the review queue — so a segment arriving
// through it was not put there by a judgement about the claims. It is
// provenance text from a producer, and ClassifyContradictionVerification
// deliberately does not treat producer-supplied text as a verdict.
//
// The invariant this guards currently holds only because nothing calls
// EnqueueContradiction with such a value: the sole production caller,
// EnqueueSimilarityCandidates, sets the constant `similarity-scan`. That is
// an accident of today's call graph, not an enforced boundary, and the field
// is exported JSON that a future caller could fill from a session id, an
// agent id, or a mirrored event. Enforcing it here makes the guarantee
// structural rather than incidental, so adding such a caller is a no-op
// instead of a silent privilege escalation.
//
// Stripping rather than rejecting is deliberate. The pair is still a review
// item and the operator still needs it in the queue; only the unauthorised
// verification claim is discarded. Rejecting the enqueue would lose real
// work to close a forgery that has no producer.
//
// The match predicate is the classifier's own: the prefix plus a non-empty
// reason. A bare `adjudicated:` carries no claim to begin with and is left
// in place, so this cannot disagree with ClassifyContradictionVerification
// about which values are meaningful.
func stripForgedAdjudication(detectedBy string) (string, bool) {
	segments := contradictionSegments(detectedBy)
	if len(segments) == 0 {
		return detectedBy, false
	}

	kept := make([]string, 0, len(segments))
	forged := false
	for _, seg := range segments {
		rest, isAdjudication := strings.CutPrefix(seg, contradictionAdjudicatedSegmentPrefix)
		if isAdjudication && strings.TrimSpace(rest) != "" {
			forged = true
			continue
		}
		kept = append(kept, seg)
	}
	if !forged {
		return detectedBy, false
	}
	return strings.Join(kept, "|"), true
}

// UnverifiedContradictionError is returned when a caller asks to apply a
// row that no operator has adjudicated.
//
// It is a distinct type rather than a plain error so that a caller can
// tell "this row needs a human decision" — an expected outcome with a
// well-defined next step — apart from "this row failed", which is not.
// The CLI reports the first as a counted, explained category instead of
// discarding it.
type UnverifiedContradictionError struct {
	// QueueRowID is the shared.contradiction_log row id, as a string
	// because that is how SQLite returns it through LoadContradictionQueue.
	QueueRowID string

	// DetectedBy is the raw provenance string that failed the check.
	DetectedBy string

	// Reason is the operator-facing explanation from the classifier.
	Reason string
}

func (e *UnverifiedContradictionError) Error() string {
	return fmt.Sprintf("queue row %s is not adjudicated and cannot be applied automatically: %s "+
		"(detected_by=%q). Adjudicate it explicitly with "+
		"`mpm ops resolve-contradictions --adjudicate=%s:<reason>` once you have read both memories.",
		e.QueueRowID, e.Reason, e.DetectedBy, e.QueueRowID)
}

// AdjudicateContradiction records that an operator has read a queue row's
// two memories and concluded their claims actually conflict. It is the
// only thing that makes a row eligible to penalise a memory.
//
// This is deliberately NOT a force flag. A force flag would answer "apply
// this anyway", leaving the question of whether the pair is a real
// contradiction unanswered and unrecoverable afterwards. This answers the
// question itself, records the answer and the reason on the row, and is
// per-row: an operator adjudicating queue row 42 says nothing about row
// 43. Batch adjudication is the defect being fixed, so it is not
// reachable.
//
// Additive: the row's existing detected_by segments are preserved, so the
// detection provenance remains visible next to the judgement.
//
// Idempotent: adjudicating an already-adjudicated row is a no-op that
// returns nil. Re-running a batch must not append a second segment, and an
// operator who already read the pair should not be blocked from re-running
// the command.
//
// Refuses a resolved row: adjudication is aimed at live review items.
// Re-adjudicating settled history is a mistake worth reporting rather
// than silently accepting.
func (dm *DatabaseManager) AdjudicateContradiction(queueID int64, reason string) error {
	if dm.db == nil {
		return fmt.Errorf("AdjudicateContradiction: dm.db is nil")
	}
	if queueID <= 0 {
		return fmt.Errorf("AdjudicateContradiction: queue row id must be positive, got %d", queueID)
	}
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		// The reason is the audit trail. Without it, an "adjudicated"
		// marker records only that something happened, which is not
		// enough to distinguish a considered decision from a reflex.
		return fmt.Errorf("AdjudicateContradiction: a reason is required — record what you checked and why these two memories conflict")
	}
	safeReason := sanitizeAdjudicationReason(trimmed)
	if safeReason == "" {
		return fmt.Errorf("AdjudicateContradiction: reason %q contains no usable characters after sanitisation", reason)
	}

	var detectedBy sql.NullString
	err := dm.db.QueryRow(`
		SELECT detected_by FROM shared.contradiction_log
		WHERE id = ? AND resolved_at IS NULL`, queueID).Scan(&detectedBy)
	if err == sql.ErrNoRows {
		return fmt.Errorf("AdjudicateContradiction: no unresolved queue row with id %d — check the id from `mpm ops resolve-contradictions --json`", queueID)
	}
	if err != nil {
		return fmt.Errorf("AdjudicateContradiction: read row %d: %w", queueID, err)
	}

	if ClassifyContradictionVerification(detectedBy.String).Adjudicated {
		return nil
	}

	// A row with no prior provenance gets the adjudication segment alone.
	// It does NOT get stamped `similarity-scan`: that would invent a
	// detection history the row never had, and this file's whole argument
	// is that provenance is not to be guessed at.
	segment := contradictionAdjudicatedSegmentPrefix + safeReason
	base := strings.TrimRight(detectedBy.String, "|")
	newValue := segment
	if base != "" {
		newValue = base + "|" + segment
	}

	// Same guard shape as applyArbitration's theory linkage: one row, and
	// only while it is unresolved.
	if _, err := dm.db.Exec(`
		UPDATE shared.contradiction_log SET detected_by = ?
		WHERE id = ? AND resolved_at IS NULL`, newValue, queueID); err != nil {
		return fmt.Errorf("AdjudicateContradiction: update row %d: %w", queueID, err)
	}
	return nil
}

// sanitizeAdjudicationReason makes operator text safe to store as one
// segment of a '|'-delimited provenance string.
//
// The separator matters most: a reason containing '|' would inject a
// segment boundary and could forge the shape of another provenance
// value, so it is replaced rather than escaped. Control characters are
// dropped so the column stays one readable line, and the length is
// bounded because this is a provenance note, not a document.
func sanitizeAdjudicationReason(reason string) string {
	var b strings.Builder
	for _, r := range reason {
		switch {
		case r == '|':
			b.WriteRune('/')
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			// drop other control characters entirely
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxAdjudicationReasonBytes {
		out = out[:maxAdjudicationReasonBytes]
		// Slicing on a byte boundary can split a multi-byte rune and leave
		// invalid UTF-8, which SQLite will not carry as clean TEXT and which
		// every later reader of detected_by then has to cope with. Drop the
		// partial rune instead of persisting a broken one.
		for len(out) > 0 && !utf8.ValidString(out) {
			_, size := utf8.DecodeLastRuneInString(out)
			out = out[:len(out)-size]
		}
		out = strings.TrimSpace(out)
	}
	return out
}
