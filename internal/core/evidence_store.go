// evidence_store.go — DB-touching code for the confidence/evidence foundation.
//
// RecomputeConfidence is the single authoritative entry point for updating
// an artifact's confidence. Every confidence change — from triggers, from
// idle_dream, from manual CLI, from future calibration code — flows through
// this function. This is the centralization the design discussion called for.
package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"
)

// RecomputeReason is the trigger cause for a confidence change. The string
// values must match the CHECK constraint on confidence_history.trigger.
type RecomputeReason string

// ProvenanceOnlySourceGroup is the source_group used by evidence rows
// that exist to record WHO/WHAT wrote an artifact, not to support the
// claim's truth.
//
// The memory_source_evidence_ai trigger writes one such row for every
// memory INSERT, attributed to the write's metadata source/model. Saving
// a claim does not make it more likely to be correct, and an artifact
// must not be able to raise its own confidence by existing — so these
// rows are deliberately excluded from every confidence computation.
// They remain queryable, which is the point: source attribution stays
// available without being mistaken for independent confirmation.
//
// This is a semantic boundary, not a schema one: evidence validation
// (docs/SPEC.md `source_group` vocabulary) does not accept
// 'auto_capture' from callers, so only the trigger produces it.
const ProvenanceOnlySourceGroup = "auto_capture"

// scoringEvidenceClause is the shared SQL predicate both confidence
// loaders append so the stored value, the recompute and the reasoning
// trace all see the same evidence set. Keeping it in one place is what
// prevents the stored/derived divergence this predicate fixes.
const scoringEvidenceClause = ` AND source_group <> '` + ProvenanceOnlySourceGroup + `'`

const (
	RecomputeReasonEvidenceAdded   RecomputeReason = "evidence_added"
	RecomputeReasonEvidenceUpdated RecomputeReason = "evidence_updated"
	RecomputeReasonEvidenceDeleted RecomputeReason = "evidence_deleted"
	RecomputeReasonEvidenceExpired RecomputeReason = "evidence_expired"
	RecomputeReasonDecayTick       RecomputeReason = "decay_tick"
	RecomputeReasonConceptDrift    RecomputeReason = "concept_drift"
	RecomputeReasonManual          RecomputeReason = "manual_recompute"
)

// addEvidenceInTx is the tx-aware primitive that AddEvidence wraps with
// its own WithTx. Extracted so callers that already hold a transaction
// (e.g. LogChangelogEntryWithConfirmations, which needs atomicity
// across a changelog memory write and one or more confirmation
// evidence rows) can reuse the same INSERT + recompute path without
// opening a nested transaction.
//
// The row's id is NOT passed in: it is derived from the observation's
// identity (see evidence_identity.go), and the evidence table's PRIMARY KEY
// is what enforces "at most one stored row per observation per artifact".
// The expiresAt argument is computed by AddEvidence before the WithTx
// boundary; passing it in keeps the in-tx path a pure side-effect with no
// derived-state computation, so any caller's tx stays a single INSERT +
// recompute.
//
// A delivery of an observation that is already recorded is a REPLAY: the
// INSERT is skipped by the primary key, nothing is recomputed, and the
// outcome says so. The replay is not an error — the observation IS
// recorded, which is exactly what the caller asked for.
func addEvidenceInTx(node DBNode, in EvidenceInput, expiresAt *int64) (EvidenceOutcome, error) {
	// The identity is derived BEFORE the delivery timestamp is filled in:
	// whether the caller stated an observation time is part of the identity,
	// and the fill-in would erase that distinction. See evidence_identity.go.
	identity, err := deriveObservationIdentity(in)
	if err != nil {
		return EvidenceOutcome{}, err
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now()
	}

	res, err := node.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                     strength, independence_factor, created_by, created_at, expires_at, notes,
		                     reference_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, 0, identity.RowID, in.ArtifactID, in.ArtifactType, in.Type, in.SourceGroup,
		in.Strength, in.IndependenceFactor, in.CreatedBy, in.CreatedAt.Unix(), expiresAt, in.Notes,
		// Absent persists as SQL NULL rather than ''. ValidateReferenceURL
		// guarantees a non-empty string always means "a reference was
		// supplied", so NULL and '' are never ambiguous on the read side —
		// and keeping the distinction in SQL lets a query ask
		// "which evidence carries an external reference?" directly.
		nullableReferenceURL(in.ReferenceURL))
	if err != nil {
		return EvidenceOutcome{}, fmt.Errorf("insert evidence: %w", err)
	}

	inserted, err := rowsAffected(res)
	if err != nil {
		return EvidenceOutcome{}, fmt.Errorf("insert evidence: %w", err)
	}
	if inserted == 0 {
		// The observation is already recorded: this delivery is a replay.
		// Reconcile it against what is stored — a strict-mode key that now
		// carries different content is a conflict, not a replay — and
		// record a reference URL the stored row does not have yet.
		if err := reconcileReplayedObservation(node, identity, in); err != nil {
			return EvidenceOutcome{}, err
		}
		return EvidenceOutcome{
			EvidenceID: identity.RowID,
			Duplicate:  true,
			Mode:       identity.Mode,
		}, nil
	}

	if err := RecomputeConfidence(node, in.ArtifactID, in.ArtifactType, RecomputeReasonEvidenceAdded); err != nil {
		return EvidenceOutcome{}, fmt.Errorf("recompute confidence: %w", err)
	}
	return EvidenceOutcome{
		EvidenceID: identity.RowID,
		Mode:       identity.Mode,
	}, nil
}

// rowsAffected reports how many rows an INSERT actually wrote. It tolerates
// a driver that does not implement RowsAffected for the statement.
func rowsAffected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// reconcileReplayedObservation resolves a delivery whose observation is
// already recorded.
//
// Two things can be true of a replay, and only one of them is an error:
//
//   - the delivery says the same thing the stored row says — that is a
//     replay, and the only write it may cause is recording a reference URL
//     the stored row does not carry yet; and
//   - the delivery says something DIFFERENT from the stored row while
//     claiming the same identity — that is a conflict, and the substrate
//     refuses it rather than silently keeping one of the two claims.
//
// The conflict check only applies in strict mode, where the caller named the
// observation: in best_effort mode the identity IS the content, so a match
// already means the content agrees.
//
// The check reads the stored row rather than assuming, so it is a fact about
// the ledger. It is skipped for an expired stored row: the challenge path
// (F7.1) and the contradiction resolver deliberately neutralize rows by
// expiring them, and a replay must not resurrect or contradict a
// deliberately neutralized observation.
func reconcileReplayedObservation(node DBNode, identity observationIdentity, in EvidenceInput) error {
	var (
		storedType, storedSourceGroup, storedCreatedBy string
		storedNotes                                    sql.NullString
		storedStrength, storedIndependence             float64
		storedExpiresAt                                sql.NullInt64
		storedReferenceURL                             sql.NullString
	)
	err := node.QueryRowTracked(`
		SELECT type, source_group, strength, independence_factor, created_by,
		       notes, expires_at, reference_url
		FROM evidence
		WHERE id = ?
	`, identity.RowID).Scan(
		&storedType, &storedSourceGroup, &storedStrength, &storedIndependence,
		&storedCreatedBy, &storedNotes, &storedExpiresAt, &storedReferenceURL,
	)
	if err != nil {
		return fmt.Errorf("read back evidence row %s: %w", identity.RowID, err)
	}

	live := !storedExpiresAt.Valid || storedExpiresAt.Int64 > time.Now().Unix()
	if identity.Mode == EvidenceObservationStrict && live {
		storedDigest := evidenceContentDigest(EvidenceInput{
			Type:               storedType,
			SourceGroup:        storedSourceGroup,
			Strength:           storedStrength,
			IndependenceFactor: storedIndependence,
			CreatedBy:          storedCreatedBy,
			Notes:              storedNotes.String,
		})
		if storedDigest != identity.ContentDigest {
			return fmt.Errorf(
				"observation identity conflict: the observation recorded as %s already says something "+
					"different, so this delivery is refused rather than merged into it",
				identity.RowID)
		}
	}

	// A reference URL the stored row does not carry yet is recorded on it.
	// The URL is provably confidence-neutral (evidence_reference_url.go), and
	// dropping caller-supplied provenance would lose data.
	if in.ReferenceURL != "" && !storedReferenceURL.Valid {
		if _, err := node.ExecTracked(
			`UPDATE evidence SET reference_url = ? WHERE id = ? AND reference_url IS NULL`,
			0, in.ReferenceURL, identity.RowID,
		); err != nil {
			return fmt.Errorf("record reference_url on evidence row %s: %w", identity.RowID, err)
		}
		// Write-path read-back assertion: the UPDATE returning no error does
		// not prove the value landed.
		var readBack sql.NullString
		if err := node.QueryRowTracked(
			`SELECT reference_url FROM evidence WHERE id = ?`, identity.RowID,
		).Scan(&readBack); err != nil {
			return fmt.Errorf("read back reference_url on evidence row %s: %w", identity.RowID, err)
		}
		if !readBack.Valid || readBack.String != in.ReferenceURL {
			return fmt.Errorf("reference_url did not persist on evidence row %s", identity.RowID)
		}
	}

	return nil
}

// nullableReferenceURL maps the absent (empty) reference URL to SQL NULL so
// "no external reference was supplied" stays distinguishable in the database
// from any stored value. Callers that build an EvidenceInput literal without
// the field get NULL for free via the Go zero value.
func nullableReferenceURL(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// EvidenceInput is the public shape for adding evidence. The DB wrapper
// fills in the id and created_at if not provided.
type EvidenceInput struct {
	ArtifactID         string
	ArtifactType       string
	Type               string
	SourceGroup        string
	Strength           float64
	IndependenceFactor float64
	CreatedBy          string
	CreatedAt          time.Time
	ExpiresAt          *time.Time
	Notes              string
	// ReferenceURL is an OPTIONAL, explicitly user-supplied external
	// reference identifying where this evidence came from. Empty means no
	// reference was supplied and stays valid — every pre-existing caller
	// leaves it empty.
	//
	// It is metadata about a source, never proof about a claim: it does not
	// affect strength, confidence, SourceGroupClass, or derived verification,
	// and MPM never fetches or verifies it. See evidence_reference_url.go.
	ReferenceURL string
	// ObservationKey is an OPTIONAL, caller-supplied stable identity for the
	// OBSERVATION this evidence records — not for the delivery of it.
	//
	// Empty means "no observation identity was supplied", and the substrate
	// falls back to the submission's semantic fingerprint: two submissions
	// that agree in every semantic dimension are then ONE observation. A
	// non-empty key says "this is the same observation as the one recorded
	// under this key", which is what lets a caller record two INDEPENDENT
	// observations that happen to carry identical text.
	//
	// The key is used verbatim and is never persisted: only its SHA-256
	// digest reaches the stored identity. See evidence_identity.go for the
	// whole model, and for what a replay does and does not change.
	ObservationKey string
}

// EvidenceSourceGroupClass is how DeriveWorkVerification classifies
// evidence by its origin. The classes — not individual values — drive
// verification decisions.
type EvidenceSourceGroupClass string

const (
	// SourceGroupOutcome: direct observation that the intended result was
	// achieved. Sufficient for verified.
	SourceGroupClassOutcome EvidenceSourceGroupClass = "outcome"
	// SourceGroupAudit: an audit trail of process, not proof of outcome.
	// Alone it yields partial.
	SourceGroupClassAudit EvidenceSourceGroupClass = "audit"
	// SourceGroupAction: something ran, but no result was observed.
	SourceGroupClassAction EvidenceSourceGroupClass = "action"
)

// evidenceSourceGroupRegistry is the canonical source_group vocabulary.
// It is the SINGLE SOURCE OF TRUTH shared by (a) AddEvidence validation —
// unknown values are rejected instead of being silently ignored by the
// verifier later (audit finding F6) — and (b) DeriveWorkVerification's
// classification switch.
var evidenceSourceGroupRegistry = map[string]EvidenceSourceGroupClass{
	// Outcome sources.
	"filesystem":    SourceGroupClassOutcome,
	"test":          SourceGroupClassOutcome,
	"api_response":  SourceGroupClassOutcome,
	"manual_review": SourceGroupClassOutcome,
	// Audit sources.
	"git":      SourceGroupClassAudit,
	"ci":       SourceGroupClassAudit,
	"external": SourceGroupClassAudit,
	// Action sources.
	"tool_invocation": SourceGroupClassAction,
	"api_call":        SourceGroupClassAction,
	"process":         SourceGroupClassAction,
}

// ValidEvidenceSourceGroup reports whether source_group is part of the
// canonical vocabulary.
func ValidEvidenceSourceGroup(sg string) bool {
	_, ok := evidenceSourceGroupRegistry[sg]
	return ok
}

// EvidenceSourceGroupOfClass returns all source_group values in a class.
func EvidenceSourceGroupOfClass(class EvidenceSourceGroupClass) []string {
	var out []string
	for sg, c := range evidenceSourceGroupRegistry {
		if c == class {
			out = append(out, sg)
		}
	}
	sort.Strings(out)
	return out
}

// classifySourceGroup maps a source_group to its verifier class.
// Unregistered values return "" — but they cannot reach the verifier,
// because AddEvidence rejects them at the acceptance boundary.
func classifySourceGroup(sg string) EvidenceSourceGroupClass {
	return evidenceSourceGroupRegistry[sg]
}

// AddEvidence inserts an evidence row and triggers a confidence recompute.
//
// The evidence table intentionally has no AFTER INSERT/UPDATE/DELETE
// triggers; recompute is driven from Go (SQLite's connection-locking model
// deadlocks when a RegisterFunc callback attempts further writes). The
// recompute is invoked synchronously here so every confidence change still
// flows through RecomputeConfidence, matching the "single authoritative
// entry point" design.
//
// F6: source_group is validated against the canonical registry BEFORE any
// persistence. An unrecognized value used to be accepted, stored, and then
// silently ignored by DeriveWorkVerification — permanently capping work
// verification at whatever the remaining evidence supported. It is now a
// precise, actionable rejection with zero partial state.
//
// A delivery of an observation that is ALREADY recorded is a replay: no
// second row is written, no confidence is recomputed, and the artifact's
// confidence is untouched. Use AddEvidenceWithOutcome when the caller needs
// to know which of the two happened.
func AddEvidence(dm *DatabaseManager, in EvidenceInput) error {
	_, err := AddEvidenceWithOutcome(dm, in)
	return err
}

// AddEvidenceWithOutcome is AddEvidence plus the outcome of the delivery.
//
// The outcome says whether the delivery recorded a NEW observation or
// replayed one that is already recorded, and carries the id of the stored
// evidence row — the same id for every delivery of the same observation.
//
// Everything else about the write is exactly AddEvidence's: the same
// validation, the same transaction, the same single authoritative recompute.
func AddEvidenceWithOutcome(dm *DatabaseManager, in EvidenceInput) (EvidenceOutcome, error) {
	if !IsValidEvidenceType(in.Type) {
		return EvidenceOutcome{}, fmt.Errorf("invalid evidence type: %q", in.Type)
	}
	if in.ArtifactType == "" {
		return EvidenceOutcome{}, fmt.Errorf("artifact_type required")
	}
	if in.SourceGroup == "" {
		return EvidenceOutcome{}, fmt.Errorf("source_group required")
	}
	if in.CreatedBy == "" {
		return EvidenceOutcome{}, fmt.Errorf("created_by required")
	}
	// Validate the explicit reference URL BEFORE any DB work, alongside the
	// other acceptance checks, so a rejected value leaves zero partial state.
	// ValidateReferenceURL returns the input verbatim (or "" when absent) —
	// it never normalizes — so what was validated is exactly what is stored.
	//
	// reference_url participates in the scanner below for the same reason
	// notes does: a URL query string (?api_key=…, #access_token=…, a signed
	// S3 link) is a MORE common credential-smuggling vector than free text,
	// so leaving it unscanned would be a net regression against the
	// "every user field is scanned" invariant.
	referenceURL, err := ValidateReferenceURL(in.ReferenceURL)
	if err != nil {
		return EvidenceOutcome{}, err
	}
	in.ReferenceURL = referenceURL
	// Scan every user-supplied text field for secrets and poison phrases
	// BEFORE any DB work. The `notes` field is the obvious target, but
	// `SourceGroup` and `CreatedBy` can also smuggle content (the prior
	// comment claimed this was checked but only notes was). The
	// 20-pattern scanner is the same one used by MemoryStore.AddMemory.
	// Scanner runs before the vocabulary check so a hostile value gets
	// the security rejection, not a syntax complaint.
	if isSensitive, reason := isSensitiveContent(in.Notes); isSensitive {
		return EvidenceOutcome{}, fmt.Errorf("sensitive content in evidence notes: %s", reason)
	}
	if isSensitive, reason := isSensitiveContent(in.SourceGroup); isSensitive {
		return EvidenceOutcome{}, fmt.Errorf("sensitive content in evidence source_group: %s", reason)
	}
	if isSensitive, reason := isSensitiveContent(in.CreatedBy); isSensitive {
		return EvidenceOutcome{}, fmt.Errorf("sensitive content in evidence created_by: %s", reason)
	}
	if isSensitive, reason := isSensitiveContent(in.ReferenceURL); isSensitive {
		return EvidenceOutcome{}, fmt.Errorf("sensitive content in evidence reference_url: %s", reason)
	}
	if isPoisoned, reason := dm.scanPoisoned(in.Notes); isPoisoned {
		return EvidenceOutcome{}, fmt.Errorf("poison content in evidence notes: %s", reason)
	}
	if isPoisoned, reason := dm.scanPoisoned(in.SourceGroup); isPoisoned {
		return EvidenceOutcome{}, fmt.Errorf("poison content in evidence source_group: %s", reason)
	}
	if isPoisoned, reason := dm.scanPoisoned(in.CreatedBy); isPoisoned {
		return EvidenceOutcome{}, fmt.Errorf("poison content in evidence created_by: %s", reason)
	}
	if isPoisoned, reason := dm.scanPoisoned(in.ReferenceURL); isPoisoned {
		return EvidenceOutcome{}, fmt.Errorf("poison content in evidence reference_url: %s", reason)
	}
	if !ValidEvidenceSourceGroup(in.SourceGroup) {
		return EvidenceOutcome{}, fmt.Errorf(
			"invalid source_group %q: must be one of outcome=%v, audit=%v, action=%v "+
				"(run `mpm call mpm_evidence --payload '{\"action\":\"source_groups\"}'` to list them)",
			in.SourceGroup,
			EvidenceSourceGroupOfClass(SourceGroupClassOutcome),
			EvidenceSourceGroupOfClass(SourceGroupClassAudit),
			EvidenceSourceGroupOfClass(SourceGroupClassAction))
	}
	if in.IndependenceFactor == 0 {
		in.IndependenceFactor = 1.0
	}

	// The delivery timestamp is NOT filled in here. addEvidenceInTx fills it
	// in AFTER the observation identity has been derived, because whether the
	// caller stated an observation time is part of that identity and the
	// substrate's own delivery time is not. See evidence_identity.go.
	// Reject evidence for nonexistent artifacts BEFORE inserting anything.
	// The evidence table has no FK to memories/lessons (intentional, to
	// preserve evidence history even after artifact deletion in v2), so
	// without this check the row would land orphaned, RecomputeConfidence
	// would silently produce an unanchored value (lastPositiveAt=now →
	// no decay), and confidence_history would gain an audit row for an
	// artifact that doesn't exist. That drift is the canonical
	// "confidence diverges from evidence state" failure mode the
	// documentation warns about.
	exists, err := artifactExists(dm, in.ArtifactID, in.ArtifactType)
	if err != nil {
		return EvidenceOutcome{}, fmt.Errorf("artifact existence check: %w", err)
	}
	if !exists {
		return EvidenceOutcome{}, fmt.Errorf("artifact %s/%s does not exist", in.ArtifactType, in.ArtifactID)
	}

	var expiresAt *int64
	if in.ExpiresAt != nil {
		exp := in.ExpiresAt.Unix()
		expiresAt = &exp
	}

	// Wrap INSERT + recompute in a single transaction so a recompute failure
	// rolls back the evidence row. Without this, an orphan evidence row would
	// remain if the process dies between the INSERT and the recompute, or if
	// the recompute itself fails (e.g., CHECK constraint violation).
	var outcome EvidenceOutcome
	if err := dm.WithTx(func(node DBNode) error {
		out, err := addEvidenceInTx(node, in, expiresAt)
		if err != nil {
			return err
		}
		outcome = out
		return nil
	}); err != nil {
		return EvidenceOutcome{}, err
	}

	// A replay changed nothing: the evidence set is the same, so there is
	// nothing to re-derive and no observation to record. Re-deriving would
	// write the same verification again and append an evidence_observed
	// event for an observation that was not observed again.
	if outcome.Duplicate {
		return outcome, nil
	}

	// F7/F12: derived work verification must track evidence changes.
	// Post-completion or post-hoc evidence on a work previously left
	// works.verification stale until someone re-ran complete/cancel — a
	// verified claim could masquerade as current truth after contradictory
	// evidence arrived, and vice versa. Re-derive synchronously here so
	// EVERY evidence write path (MCP tool, mpm call, CLI `mpm evidence add`)
	// keeps the derived state current. The evidence_observed event uses the
	// otherwise-dormant ledger vocabulary so the recompute is inspectable.
	if in.ArtifactType == "work" {
		if _, err := dm.AppendWorkEvent(in.ArtifactID, WorkEvent{
			EventType: WorkEventTypeEvidenceObserved,
			Note:      fmt.Sprintf("%s/%s strength=%.2f", in.Type, in.SourceGroup, in.Strength),
		}, nil, nil); err != nil {
			// Non-fatal: the evidence row is committed; the event is
			// observability. Log and continue to derivation.
			slog.Warn("AddEvidence: evidence_observed event failed", "work", in.ArtifactID, "error", err.Error())
		}
		if _, err := dm.DeriveWorkVerification(in.ArtifactID); err != nil {
			return EvidenceOutcome{}, fmt.Errorf("derive work verification: %w", err)
		}
	}
	return outcome, nil
}

// RecomputeConfidence is the single authoritative entry point. It loads the
// current evidence set for the artifact, runs the math, and writes both
// the new confidence on the artifact and a new row in confidence_history.
//
// Accepts a DBNode so callers can run the recompute inside an active
// transaction (e.g., AddEvidence wraps insert + recompute in one Tx) or
// standalone (e.g., idle_dream's per-cycle decay recompute).
//
// Called by:
//   - Go application layer during AddEvidence (transactionally)
//   - idle_dream (for decay_tick recompute)
//   - Manual CLI (`mpm ops confidence recompute`)
//   - Future calibration/challenge code
//
// 2026-08-04 (Task 4): hard-confidence invalidation hook. The recompute
// detects a CROSSING of the configured threshold (old >= threshold AND
// new < threshold) and enqueues one cascade intent per downstream
// dependent inside the same tx as the confidence update + history
// INSERT. Ordinary weakening (e.g., 0.7 → 0.5) is a no-op at the
// outbox level; a recompute that leaves the artifact already below
// the threshold is also a no-op (the cross is the event, not the
// absolute value). The hook only fires when the recompute is run
// transactionally (node.Tx() != nil); standalone callers that hit the
// crossing silently skip the cascade — that matches the existing
// pre-cascade behavior where AddEvidence's WithTx wrapper was the
// only path that mattered.
func RecomputeConfidence(node DBNode, artifactID, artifactType string, reason RecomputeReason) error {
	_, err := recomputeConfidenceAt(node, artifactID, artifactType, reason, time.Now(), false)
	return err
}

// recomputeConfidenceAt is the shared body of every confidence
// recomputation. `now` is explicit so deterministic maintenance tests can
// drive the decay term; production callers use time.Now via
// RecomputeConfidence.
//
// `onlyIfChanged` is the maintenance contract: when the derived value is
// effectively identical to the stored one, do NOT write the artifact
// column and do NOT append a confidence_history row. That keeps a
// periodic decay pass from growing the history ledger with rows that
// record no change, while the changed/skipped distinction is reported by
// the caller. Ordinary evidence-driven recomputes pass false and keep
// their append-always behaviour.
//
// Returns whether the stored confidence changed.
func recomputeConfidenceAt(node DBNode, artifactID, artifactType string, reason RecomputeReason, now time.Time, onlyIfChanged bool) (bool, error) {
	// Validate artifact existence. Without this, a recompute against a
	// nonexistent artifact silently succeeds: loadEvidenceForRecompute
	// anchors lastPositiveAt to now (no decay), computeConfidence returns
	// the initial value, writeArtifactConfidence matches zero rows, and
	// the history INSERT still fires — polluting the audit trail with a
	// confidence record for an artifact that doesn't exist. That is the
	// exact "confidence diverges from evidence state" failure mode the
	// documentation warns against, so we surface a clear error instead.
	exists, err := artifactExists(node, artifactID, artifactType)
	if err != nil {
		return false, fmt.Errorf("artifact existence check: %w", err)
	}
	if !exists {
		return false, fmt.Errorf("artifact %s/%s does not exist", artifactType, artifactID)
	}

	// Snapshot the OLD confidence before the recompute. The crossing
	// detector compares old (this read) vs new (the post-recompute
	// value) — a recompute that stays above the threshold does not
	// cascade, and an artifact that is already below the threshold
	// does not re-cascade on a subsequent recompute.
	oldConf, hasOldConf := readArtifactConfidence(node, artifactID, artifactType)

	// Load the evidence set.
	ev, lastPositiveAt, err := loadEvidenceForRecompute(node, artifactID, artifactType, now)
	if err != nil {
		return false, fmt.Errorf("load evidence: %w", err)
	}

	// Run the math.
	conf := computeConfidence(artifactType, ev, now, lastPositiveAt, 0.005)

	// Maintenance no-op: nothing to record. The tolerance is far below
	// any observable movement (a day of decision decay moves the value by
	// ~2.4e-4; a second by ~3e-9), so this cannot mask a real change.
	if onlyIfChanged && hasOldConf && math.Abs(conf-oldConf) < 1e-6 {
		return false, nil
	}

	// Update the artifact's confidence column.
	if err := writeArtifactConfidence(node, artifactID, artifactType, conf); err != nil {
		return false, fmt.Errorf("write artifact confidence: %w", err)
	}

	// Append the history row.
	historyID := GenerateID()
	_, err = node.ExecTracked(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, 0, historyID, artifactID, artifactType, conf, now.Unix(), len(ev), string(reason))
	if err != nil {
		return false, fmt.Errorf("insert history row: %w", err)
	}

	// Hard-confidence invalidation hook (Task 4). Enqueue cascade
	// intents only on the crossing transition: old >= threshold AND
	// new < threshold. The threshold constant is shared with the
	// cascade outbox surface so the storage layer, the recompute
	// path, and the tests all read the same number.
	if tx := node.Tx(); tx != nil {
		crossed := (!hasOldConf || oldConf >= HardConfidenceInvalidationThreshold) &&
			conf < HardConfidenceInvalidationThreshold
		if crossed {
			if _, err := node.DM().EnqueueCascadeInvalidation(
				tx,
				artifactID, artifactType,
				"confidence_floor", "", 0,
			); err != nil {
				return false, fmt.Errorf("confidence cascade enqueue: %w", err)
			}
		}

		// Positive-direction cascade: confidence crosses UPWARD past
		// the proven threshold (HardConfidenceProvenThreshold).
		// Mirror image of the invalidation crossing above: old below
		// threshold AND new >= threshold. Downstream artifacts that
		// opted out via polarity='assumes_false' (i.e. they were
		// running on the assumption the foundation was uncertain)
		// are surfaced to the materializer so a reviewer can decide
		// whether their conclusions still hold given the foundation
		// is now solid.
		ceilingCrossed := (!hasOldConf || oldConf < HardConfidenceProvenThreshold) &&
			conf >= HardConfidenceProvenThreshold
		if ceilingCrossed {
			if _, err := node.DM().EnqueueCascadeFoundationProven(
				tx,
				artifactID, artifactType,
				ReasonConfidenceCeiling, "", 0,
			); err != nil {
				return false, fmt.Errorf("confidence positive cascade enqueue: %w", err)
			}
		}
	}

	return true, nil
}

// readArtifactConfidence reads the live confidence column from the
// artifact table. Returns (0, false) when the row is missing (the
// caller can fall back to the initial confidence or treat it as
// "already below threshold"). The cascade crossing detector in
// RecomputeConfidence uses this to distinguish "never had confidence"
// (treat as above threshold) from "had confidence and crossed".
func readArtifactConfidence(node DBNode, artifactID, artifactType string) (float64, bool) {
	// Table identifier is constrained by this switch — not user-
	// controllable. artifactType is one of the hardcoded values
	// ("memory", "decision", "theory", "lesson") that the cascade
	// materializer accepts; the `fmt.Sprintf` below is safe because
	// the table name is one of two constants. Mirrors ArtifactTable
	// in artifact_table.go; keeping both in lockstep is enforced by
	// the canonical-schema allow-list in canonical_dump.go.
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	var conf float64
	err := node.QueryRowTracked(fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, table), artifactID).Scan(&conf)
	if err != nil {
		return 0, false
	}
	return conf, true
}

// loadEvidenceForRecompute returns the evidence set and the most recent
// positive-evidence timestamp (for decay anchoring).
//
// Requires that the artifact exists — callers (AddEvidence,
// RecomputeConfidence) validate this beforehand so the no-positive-evidence
// fallback below only ever anchors against a real artifact's created_at.
func loadEvidenceForRecompute(node DBNode, artifactID, artifactType string, now time.Time) ([]evidenceInput, time.Time, error) {
	rows, err := node.QueryTracked(`
		SELECT strength, independence_factor, created_at, expires_at
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?`+scoringEvidenceClause,
		artifactID, artifactType)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()

	var out []evidenceInput
	var lastPositiveAt time.Time
	for rows.Next() {
		var strength, independence float64
		var createdAt int64
		var expiresAt sql.NullInt64
		if err := rows.Scan(&strength, &independence, &createdAt, &expiresAt); err != nil {
			return nil, time.Time{}, err
		}
		// Skip expired evidence.
		if expiresAt.Valid && expiresAt.Int64 < now.Unix() {
			continue
		}
		ts := time.Unix(createdAt, 0)
		out = append(out, evidenceInput{
			Strength:     strength,
			Independence: independence,
			CreatedAt:    ts,
		})
		if strength > 0 && ts.After(lastPositiveAt) {
			lastPositiveAt = ts
		}
	}
	if lastPositiveAt.IsZero() {
		// No positive evidence — anchor decay to the artifact's creation time
		// so a brand-new artifact doesn't decay from the unix epoch.
		lastPositiveAt = now
		if createdAt, ok := readArtifactCreatedAt(node, artifactID, artifactType); ok {
			lastPositiveAt = createdAt
		}
	}
	return out, lastPositiveAt, rows.Err()
}

func readArtifactCreatedAt(node DBNode, artifactID, artifactType string) (time.Time, bool) {
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	// The column has two shapes in the wild. `memories.created_at` is
	// `INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER))`
	// (schema.go) and the production write path stores an integer epoch
	// (db.go, saveMemoryRow), while some legacy rows and some writers
	// store an RFC3339 string. Both are read here.
	//
	// Reading only the string shape (as this function used to) made the
	// integer epoch unparseable, so the decay anchor silently fell back
	// to "now" and `tDays` was 0 — no decay was ever applied to an
	// artifact without positive evidence, which is the population this
	// anchor exists for.
	var raw interface{}
	err := node.QueryRowTracked(fmt.Sprintf(`SELECT created_at FROM %s WHERE id = ?`, table), artifactID).Scan(&raw)
	if err != nil {
		return time.Time{}, false
	}
	switch v := raw.(type) {
	case nil:
		return time.Time{}, false
	case int64:
		return time.Unix(v, 0), true
	case float64:
		return time.Unix(int64(v), 0), true
	case []byte:
		t, perr := parseTime(string(v))
		if perr != nil {
			return time.Time{}, false
		}
		return t, true
	case string:
		t, perr := parseTime(v)
		if perr != nil {
			return time.Time{}, false
		}
		return t, true
	default:
		return time.Time{}, false
	}
}

// artifactExists returns whether a row exists for the given (artifactID,
// artifactType) pair. The artifact table is derived from artifactType:
// lesson → lessons, everything else (memory/theory/decision) → memories.
// Returns false (not an error) when the row is missing or soft-deleted.
//
// This is the single chokepoint for "does the artifact this evidence
// refers to still exist?" Both AddEvidence and RecomputeConfidence
// call it before writing anything; without it, both could silently
// insert rows that drift from the canonical artifact state.
func artifactExists(node DBNode, artifactID, artifactType string) (bool, error) {
	var table, deletedClause string
	switch artifactType {
	case "lesson":
		table = "lessons"
		// lessons has a `deleted` column in some schemas; the lessons view
		// hides it. We use `deleted IS NULL OR deleted = 0` to be safe
		// across both shapes — for the current view shape, `deleted` is
		// not exposed, so this clause is benign.
		deletedClause = ""
	case "work":
		// works table; no soft-delete column.
		table = "works"
		deletedClause = ""
	default:
		table = "memories"
		deletedClause = " AND deleted_at IS NULL"
	}
	var found int
	err := node.QueryRowTracked(
		fmt.Sprintf(`SELECT 1 FROM %s WHERE id = ?%s`, table, deletedClause),
		artifactID,
	).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// writeArtifactConfidence updates the confidence column on the artifact
// table. The table is derived from the artifact type.
func writeArtifactConfidence(node DBNode, artifactID, artifactType string, conf float64) error {
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	_, err := node.ExecTracked(
		fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE id = ?`, table),
		0, conf, artifactID,
	)
	return err
}

// EvidenceComponent is one piece of evidence with its computed contribution.
type EvidenceComponent struct {
	ID                string    `json:"id,omitempty"`
	Type              string    `json:"type"`
	SourceGroup       string    `json:"source_group"`
	CreatedBy         string    `json:"created_by"`
	Strength          float64   `json:"strength"`
	Independence      float64   `json:"independence"`
	AgeDays           float64   `json:"age_days"`
	RecencyWeight     float64   `json:"recency_weight"`
	EffectiveStrength float64   `json:"effective_strength"`
	CreatedAt         time.Time `json:"created_at,omitempty"`
}

// DecayComponent breaks down the time-based confidence penalty.
type DecayComponent struct {
	Lambda         float64   `json:"lambda"`
	TDays          float64   `json:"t_days"`
	Penalty        float64   `json:"penalty"`
	LastPositiveAt time.Time `json:"last_positive_at"`
}

// ConfidenceExplanation is the full intermediate breakdown of f(evidence, decay).
// Unlike query_confidence_history (audit trail), this is the reasoning trace:
// "if I recomputed confidence right now, why did I get this number?"
type ConfidenceExplanation struct {
	ArtifactID      string              `json:"artifact_id"`
	ArtifactType    string              `json:"artifact_type"`
	Confidence      float64             `json:"confidence"`
	Initial         float64             `json:"initial_confidence"`
	InitialOdds     float64             `json:"initial_odds"`
	Positive        []EvidenceComponent `json:"positive_evidence"`
	Negative        []EvidenceComponent `json:"negative_evidence"`
	Decay           DecayComponent      `json:"decay"`
	TopContributors []TopContributor    `json:"top_contributors"`
}

// TopContributor is an evidence piece ranked by absolute impact on confidence.
// This answers "what's driving this belief?" without scrolling through all rows.
type TopContributor struct {
	EvidenceID  string  `json:"evidence_id"`
	Type        string  `json:"type"`
	SourceGroup string  `json:"source_group"`
	Impact      float64 `json:"impact"` // effective_strength; sign encodes direction
	Rank        int     `json:"rank"`
}

// ExplainConfidence returns the full component breakdown of f(evidence, decay)
// for an artifact. This is the reasoning trace — distinct from query_confidence_history
// which is the audit trail.
func ExplainConfidence(dm *DatabaseManager, artifactID, artifactType string) (*ConfidenceExplanation, error) {
	if artifactType == "" {
		artifactType = "memory"
	}

	// Validate artifact existence. Without this, an explanation for a
	// nonexistent artifact would still produce a breakdown: lastPositiveAt
	// would default to now (no decay), confidence would be the initial
	// value, and the operator would see a fabricated "everything is fine"
	// trace for an artifact that doesn't exist. Same root cause as the
	// RecomputeConfidence fix — surface the error instead.
	exists, err := artifactExists(dm, artifactID, artifactType)
	if err != nil {
		return nil, fmt.Errorf("artifact existence check: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("artifact %s/%s does not exist", artifactType, artifactID)
	}

	// Load all non-expired evidence rows.
	now := time.Now()
	rows, err := dm.QueryTracked(`
		SELECT id, type, source_group, created_by, strength, independence_factor, created_at, expires_at
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?`+scoringEvidenceClause+`
		ORDER BY created_at ASC
	`, artifactID, artifactType)
	if err != nil {
		return nil, fmt.Errorf("load evidence: %w", err)
	}
	defer rows.Close()

	var (
		allEvidence    []EvidenceComponent
		lastPositiveAt time.Time
	)
	for rows.Next() {
		var id, evType, source, by string
		var strength, independence float64
		var createdAt int64
		var expiresAt sql.NullInt64
		if err := rows.Scan(&id, &evType, &source, &by, &strength, &independence, &createdAt, &expiresAt); err != nil {
			return nil, err
		}
		ts := time.Unix(createdAt, 0)
		// Skip expired.
		if expiresAt.Valid && expiresAt.Int64 < now.Unix() {
			continue
		}
		// No independence normalization here — loadEvidenceForRecompute
		// passes the raw value through. The explanation must agree with
		// the computation: if a row has independence=0 in the DB, the
		// explanation reflects the same effective=0 the recompute would
		// produce. AddEvidence normalizes 0 → 1.0 at insert time, so
		// well-formed rows never have this; rows inserted via direct SQL
		// with independence=0 are treated as fully redundant by both
		// paths. Mixing normalization in only one path would silently
		// split the system: the recompute writes 0-contribution while
		// the explanation claims full contribution.
		ageDays := now.Sub(ts).Hours() / 24.0
		recency := math.Exp(-0.005 * ageDays)
		effective := strength * independence * recency

		c := EvidenceComponent{
			ID:                id,
			Type:              evType,
			SourceGroup:       source,
			CreatedBy:         by,
			Strength:          strength,
			Independence:      independence,
			AgeDays:           ageDays,
			RecencyWeight:     recency,
			EffectiveStrength: effective,
			CreatedAt:         ts,
		}
		allEvidence = append(allEvidence, c)

		if strength > 0 && ts.After(lastPositiveAt) {
			lastPositiveAt = ts
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Anchor decay to artifact creation if no positive evidence exists.
	if lastPositiveAt.IsZero() {
		lastPositiveAt = now
		if cat, ok := readArtifactCreatedAt(dm, artifactID, artifactType); ok {
			lastPositiveAt = cat
		}
	}

	initial := InitialConfidence(artifactType)
	initialOdds := initial / (1.0 - initial)
	lambda := decayLambda(artifactType)
	tDays := now.Sub(lastPositiveAt).Hours() / 24.0
	if tDays < 0 {
		tDays = 0
	}
	decayPenalty := lambda * tDays

	// Compute confidence using the pure formula.
	conf := computeConfidence(artifactType, toEvidenceInputs(allEvidence), now, lastPositiveAt, 0.005)

	// Split into positive/negative for the explanation.
	var pos, neg []EvidenceComponent
	for _, e := range allEvidence {
		if e.Strength >= 0 {
			pos = append(pos, e)
		} else {
			neg = append(neg, e)
		}
	}

	// Rank evidence by absolute impact; top 5 answer "what's driving this?"
	sorted := make([]EvidenceComponent, len(allEvidence))
	copy(sorted, allEvidence)
	sort.Slice(sorted, func(i, j int) bool {
		return math.Abs(sorted[i].EffectiveStrength) > math.Abs(sorted[j].EffectiveStrength)
	})
	topN := 5
	if topN > len(sorted) {
		topN = len(sorted)
	}
	var topContributors []TopContributor
	for i := 0; i < topN; i++ {
		topContributors = append(topContributors, TopContributor{
			EvidenceID:  sorted[i].ID,
			Type:        sorted[i].Type,
			SourceGroup: sorted[i].SourceGroup,
			Impact:      sorted[i].EffectiveStrength,
			Rank:        i + 1,
		})
	}

	return &ConfidenceExplanation{
		ArtifactID:   artifactID,
		ArtifactType: artifactType,
		Confidence:   conf,
		Initial:      initial,
		InitialOdds:  initialOdds,
		Positive:     pos,
		Negative:     neg,
		Decay: DecayComponent{
			Lambda:         lambda,
			TDays:          tDays,
			Penalty:        decayPenalty,
			LastPositiveAt: lastPositiveAt,
		},
		TopContributors: topContributors,
	}, nil
}

// toEvidenceInputs converts EvidenceComponent slices to the plain inputs needed
// by computeConfidence (which uses the internal evidenceInput struct).
func toEvidenceInputs(comps []EvidenceComponent) []evidenceInput {
	out := make([]evidenceInput, len(comps))
	for i, c := range comps {
		out[i] = evidenceInput{
			Strength:     c.Strength,
			Independence: c.Independence,
			CreatedAt:    c.CreatedAt,
		}
	}
	return out
}

// parseTime accepts RFC3339 or SQLite "YYYY-MM-DD HH:MM:SS" formats and
// returns the parsed time.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}

// Evidence is the DB-read view of an evidence row.
type Evidence struct {
	ID                 string
	ArtifactID         string
	ArtifactType       string
	Type               string
	SourceGroup        string
	Strength           float64
	IndependenceFactor float64
	CreatedBy          string
	CreatedAt          time.Time
	ExpiresAt          *time.Time
	Notes              string
	// ReferenceURL is the explicit, user-supplied external reference for
	// this evidence, or "" when none was supplied. It is returned exactly as
	// stored. The struct carries no JSON tags — fields serialize by Go name —
	// so this field reaches MCP output, CLI JSON, and `mpm why` for free.
	ReferenceURL string
}

// ListEvidenceForArtifact returns all non-expired evidence rows for an
// artifact, ordered by created_at ascending.
func ListEvidenceForArtifact(dm *DatabaseManager, artifactID, artifactType string) ([]Evidence, error) {
	rows, err := dm.QueryTracked(`
		SELECT id, artifact_id, artifact_type, type, source_group,
		       strength, independence_factor, created_by, created_at, expires_at, notes,
		       reference_url
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY created_at ASC
	`, artifactID, artifactType)
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var createdAt int64
		var expiresAt sql.NullInt64
		// notes is TEXT NULL in the schema; auto_capture rows insert NULL.
		// Scanning NULL into a concrete Go `string` is a runtime scan
		// failure: "converting NULL to string is unsupported". Per the
		// substrate defense triad (rule #2), nullable scalar columns must
		// bind to sql.NullString and be unwrapped at the Go boundary.
		var notes sql.NullString
		// reference_url is TEXT NULL for the same reason notes is.
		var referenceURL sql.NullString
		if err := rows.Scan(&e.ID, &e.ArtifactID, &e.ArtifactType, &e.Type, &e.SourceGroup,
			&e.Strength, &e.IndependenceFactor, &e.CreatedBy, &createdAt, &expiresAt, &notes,
			&referenceURL); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if referenceURL.Valid {
			e.ReferenceURL = referenceURL.String
		}
		if notes.Valid {
			e.Notes = notes.String
		}
		e.CreatedAt = time.Unix(createdAt, 0)
		if expiresAt.Valid {
			// expires_at <= now means the row is no longer authoritative.
			// The row remains in the table for the audit trail but is
			// filtered from derivation (T20-1: neutralized challenge rows
			// must not contribute to verification).
			if expiresAt.Int64 <= now {
				continue // skip expired (audit trail intact, derivation clean)
			}
			exp := time.Unix(expiresAt.Int64, 0)
			e.ExpiresAt = &exp
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEvidence returns all non-expired evidence rows across every
// artifact, ordered by created_at descending (newest first). Used by
// the CLI `mpm evidence list` (unfiltered) and the tool path's
// equivalent.
//
// 2026-09-10 regression repair (T45): added because the CLI
// regression repair wanted bare `mpm evidence list` to work
// alongside the existing `--artifact <id>` filtered form. The
// filtered path uses ListEvidenceForArtifact (above); the unfiltered
// path uses this function. Both apply the same expiry filter so
// the surface parity holds.
func ListEvidence(dm *DatabaseManager) ([]Evidence, error) {
	rows, err := dm.QueryTracked(`
		SELECT id, artifact_id, artifact_type, type, source_group,
		       strength, independence_factor, created_by, created_at, expires_at, notes,
		       reference_url
		FROM evidence
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var createdAt int64
		var expiresAt sql.NullInt64
		var notes sql.NullString
		var referenceURL sql.NullString
		if err := rows.Scan(&e.ID, &e.ArtifactID, &e.ArtifactType, &e.Type, &e.SourceGroup,
			&e.Strength, &e.IndependenceFactor, &e.CreatedBy, &createdAt, &expiresAt, &notes,
			&referenceURL); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if referenceURL.Valid {
			e.ReferenceURL = referenceURL.String
		}
		if notes.Valid {
			e.Notes = notes.String
		}
		e.CreatedAt = time.Unix(createdAt, 0)
		if expiresAt.Valid && expiresAt.Int64 <= now {
			continue
		}
		if expiresAt.Valid {
			exp := time.Unix(expiresAt.Int64, 0)
			e.ExpiresAt = &exp
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ConfidenceSnapshot is a point-in-time view of an artifact's confidence state.
type ConfidenceSnapshot struct {
	ArtifactID   string                 `json:"artifact_id"`
	ArtifactType string                 `json:"artifact_type"`
	Confidence   float64                `json:"confidence"`
	HistoryCount int                    `json:"history_count"`
	History      []ConfidenceHistoryRow `json:"history"`
}

// ConfidenceHistoryRow is one row of the confidence_history table.
type ConfidenceHistoryRow struct {
	Confidence    float64   `json:"confidence"`
	ComputedAt    time.Time `json:"computed_at"`
	EvidenceCount int       `json:"evidence_count"`
	Trigger       string    `json:"trigger"`
}

// GetConfidenceForArtifact returns the current confidence and the last `limit`
// history rows for an artifact, ordered by computed_at DESC.
func GetConfidenceForArtifact(dm *DatabaseManager, artifactID, artifactType string, limit int) (*ConfidenceSnapshot, error) {
	table := "memories"
	if artifactType == "lesson" {
		table = "lessons"
	}
	var conf float64
	err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, table),
		artifactID,
	).Scan(&conf)
	if err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}

	if limit <= 0 {
		limit = 10
	}
	rows, err := dm.QueryTracked(`
		SELECT confidence, computed_at, evidence_count, trigger
		FROM confidence_history
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY computed_at DESC
		LIMIT ?
	`, artifactID, artifactType, limit)
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	defer rows.Close()

	snap := &ConfidenceSnapshot{
		ArtifactID:   artifactID,
		ArtifactType: artifactType,
		Confidence:   conf,
		History:      []ConfidenceHistoryRow{},
	}
	for rows.Next() {
		var h ConfidenceHistoryRow
		var computedAt int64
		if err := rows.Scan(&h.Confidence, &computedAt, &h.EvidenceCount, &h.Trigger); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		h.ComputedAt = time.Unix(computedAt, 0)
		snap.History = append(snap.History, h)
	}
	snap.HistoryCount = len(snap.History)
	return snap, rows.Err()
}

// ConfidenceChange is one confidence-altering event: the new value plus the
// previous value (from which delta is computed). Distinct from
// query_confidence_history (full timeline, no delta) — this answers
// "what moved, by how much, and why, since when?"
type ConfidenceChange struct {
	ArtifactID    string    `json:"artifact_id"`
	ArtifactType  string    `json:"artifact_type"`
	NewConfidence float64   `json:"new_confidence"`
	OldConfidence float64   `json:"old_confidence"`
	Delta         float64   `json:"delta"`
	Trigger       string    `json:"trigger"`
	EvidenceCount int       `json:"evidence_count"`
	ComputedAt    time.Time `json:"computed_at"`
}

// ConfidenceChangesFilter narrows the result set for QueryConfidenceChanges.
// Zero-value fields use sensible defaults.
type ConfidenceChangesFilter struct {
	// Since: only return changes with computed_at > Since. Zero = last 24h.
	Since time.Time
	// Limit: max rows. Zero = 50.
	Limit int
	// ArtifactID: if non-empty, only return changes for this artifact.
	ArtifactID string
	// ArtifactType: if non-empty, only return changes for this artifact type.
	ArtifactType string
}

// QueryConfidenceChanges returns recent confidence-altering events with the
// delta and trigger included. This is event detection — what moved and why —
// distinct from query_confidence_history (full timeline).
//
// Implementation: SQLite window function LAG() partitions history by artifact
// and pulls the previous confidence per row in O(n). The WHERE clause filters
// to rows that have a predecessor (initial writes have no delta).
func QueryConfidenceChanges(dm *DatabaseManager, filter ConfidenceChangesFilter) ([]ConfidenceChange, error) {
	since := filter.Since
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	// Build SQL dynamically — filter conditions compose.
	conds := []string{"prev_confidence IS NOT NULL", "ranked.computed_at > ?"}
	args := []interface{}{since.Unix()}

	if filter.ArtifactID != "" {
		conds = append(conds, "ranked.artifact_id = ?")
		args = append(args, filter.ArtifactID)
	}
	if filter.ArtifactType != "" {
		conds = append(conds, "ranked.artifact_type = ?")
		args = append(args, filter.ArtifactType)
	}
	args = append(args, limit)

	whereClause := ""
	for i, c := range conds {
		if i == 0 {
			whereClause = "WHERE " + c
		} else {
			whereClause += " AND " + c
		}
	}

	query := fmt.Sprintf(`
		WITH ranked AS (
		  SELECT
		    artifact_id, artifact_type, confidence, trigger,
		    evidence_count, computed_at,
		    LAG(confidence) OVER (
		      PARTITION BY artifact_id, artifact_type
		      ORDER BY computed_at
		    ) AS prev_confidence
		  FROM confidence_history
		)
		SELECT artifact_id, artifact_type, confidence, prev_confidence,
		       confidence - prev_confidence, trigger, evidence_count, computed_at
		FROM ranked
		%s
		ORDER BY ranked.computed_at DESC
		LIMIT ?
	`, whereClause)

	rows, err := dm.QueryTracked(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query confidence changes: %w", err)
	}
	defer rows.Close()

	var out []ConfidenceChange
	for rows.Next() {
		var c ConfidenceChange
		var computedAt int64
		if err := rows.Scan(&c.ArtifactID, &c.ArtifactType, &c.NewConfidence,
			&c.OldConfidence, &c.Delta, &c.Trigger, &c.EvidenceCount, &computedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		c.ComputedAt = time.Unix(computedAt, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConfidenceTrend is the trajectory projection of f(evidence, decay) over time.
// Four orthogonal observation directions: state (explain_confidence), cause
// (query_confidence_changes), history (query_confidence_history), direction
// (query_confidence_trend). Trend completes the set.
//
// velocity = slope_per_day is the raw signal; trend is the human-readable label.
// Agents reason better from velocity than from labels.
type ConfidenceTrend struct {
	ArtifactID        string  `json:"artifact_id"`
	ArtifactType      string  `json:"artifact_type"`
	CurrentConfidence float64 `json:"current_confidence"`
	WindowDays        int     `json:"window_days"`
	OldestInWindow    float64 `json:"confidence_at_window_start"`
	DeltaWindow       float64 `json:"delta_window"`
	SlopePerDay       float64 `json:"velocity"` // velocity: raw signal
	Trend             string  `json:"trend"`    // "rising" | "falling" | "stable" | "insufficient_data"
	SampleCount       int     `json:"sample_count"`
}

// QueryConfidenceTrend returns the trajectory of confidence over a time
// window. Uses ordinary least-squares regression on (t_days, confidence)
// samples from confidence_history. Requires >=2 samples; returns
// Trend="insufficient_data" otherwise.
//
// Default window: 30 days. Threshold for stable: |slope_per_day| < 0.005.
func QueryConfidenceTrend(dm *DatabaseManager, artifactID, artifactType string, windowDays int) (*ConfidenceTrend, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	if artifactType == "" {
		artifactType = "memory"
	}

	now := time.Now()
	windowStart := now.Add(-time.Duration(windowDays) * 24 * time.Hour)

	// Read current confidence from the artifact's underlying table.
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	var currentConf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, table),
		artifactID,
	).Scan(&currentConf); err != nil {
		return nil, fmt.Errorf("read current confidence: %w", err)
	}

	// Pull all history rows in the window, ordered oldest-first.
	rows, err := dm.QueryTracked(`
		SELECT confidence, computed_at
		FROM confidence_history
		WHERE artifact_id = ? AND artifact_type = ?
		  AND computed_at > ?
		ORDER BY computed_at ASC
	`, artifactID, artifactType, windowStart.Unix())
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	defer rows.Close()

	type sample struct {
		tDays float64
		conf  float64
	}
	var samples []sample
	for rows.Next() {
		var conf float64
		var computedAt int64
		if err := rows.Scan(&conf, &computedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		tDays := float64(computedAt-windowStart.Unix()) / 86400.0
		samples = append(samples, sample{tDays: tDays, conf: conf})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(samples) < 2 {
		var oldest float64
		if len(samples) == 1 {
			oldest = samples[0].conf
		}
		return &ConfidenceTrend{
			ArtifactID:        artifactID,
			ArtifactType:      artifactType,
			CurrentConfidence: currentConf,
			WindowDays:        windowDays,
			OldestInWindow:    oldest,
			DeltaWindow:       currentConf - oldest,
			SampleCount:       len(samples),
			Trend:             "insufficient_data",
		}, nil
	}

	// Ordinary least squares: slope = covariance(t,c) / variance(t).
	var sumT, sumC, sumTC, sumTT float64
	n := float64(len(samples))
	for _, s := range samples {
		sumT += s.tDays
		sumC += s.conf
		sumTC += s.tDays * s.conf
		sumTT += s.tDays * s.tDays
	}
	meanT := sumT / n
	meanC := sumC / n
	denom := sumTT - n*meanT*meanT
	var slope float64
	if denom != 0 {
		slope = (sumTC - n*meanT*meanC) / denom
	}

	oldest := samples[0].conf
	trend := "stable"
	switch {
	case slope > 0.005:
		trend = "rising"
	case slope < -0.005:
		trend = "falling"
	}

	return &ConfidenceTrend{
		ArtifactID:        artifactID,
		ArtifactType:      artifactType,
		CurrentConfidence: currentConf,
		WindowDays:        windowDays,
		OldestInWindow:    oldest,
		DeltaWindow:       currentConf - oldest,
		SlopePerDay:       slope,
		Trend:             trend,
		SampleCount:       len(samples),
	}, nil
}

// MemoryQualityBySource aggregates per-creator memory statistics. Surfaces
// which models/agents produce memories that survive — measured by confidence
// trajectory, challenge rate, and net delta. Driven by auto-capture evidence
// from memory_source_evidence_ai trigger (created_by = provenance.model).
//
// All inputs are existing primitives: memories, evidence, confidence_history.
// No new schema.
type MemoryQualityBySource struct {
	Source               string  `json:"source"`
	MemoryCount          int     `json:"memory_count"`
	AvgCurrentConfidence float64 `json:"avg_current_confidence"`
	AvgConfidenceDelta   float64 `json:"avg_confidence_delta"`
	TotalEvidence        int     `json:"total_evidence"`
	PositiveEvidence     int     `json:"positive_evidence"`
	NegativeEvidence     int     `json:"negative_evidence"`
	ChallengeCount       int     `json:"challenge_count"`
	ChallengeRate        float64 `json:"challenge_rate"`
	SurvivalRate         float64 `json:"survival_rate"` // memories with confidence >= 0.5 / total
}

// QueryMemoryQualityBySource returns per-source memory statistics, sorted by
// memory_count descending. Joins memories → auto_capture evidence →
// confidence_history to compute the four orthogonal axes (volume, state,
// challenge, trajectory) per creator.
func QueryMemoryQualityBySource(dm *DatabaseManager) ([]MemoryQualityBySource, error) {
	// First pass: discover distinct sources from auto_capture evidence.
	rows, err := dm.QueryTracked(`
		SELECT e.created_by, COUNT(DISTINCT e.artifact_id)
		FROM evidence e
		WHERE e.source_group = 'auto_capture' AND e.artifact_type = 'memory'
		GROUP BY e.created_by
		ORDER BY COUNT(DISTINCT e.artifact_id) DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	var sources []string
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]MemoryQualityBySource, 0, len(sources))
	for _, source := range sources {
		// Memory count + positive/negative evidence breakdown.
		var mc, pe, ne, te int
		err := dm.QueryRowTracked(`
			SELECT
				COUNT(DISTINCT e.artifact_id),
				SUM(CASE WHEN e.strength > 0 THEN 1 ELSE 0 END),
				SUM(CASE WHEN e.strength < 0 THEN 1 ELSE 0 END),
				COUNT(e.id)
			FROM evidence e
			WHERE e.source_group = 'auto_capture' AND e.artifact_type = 'memory'
			  AND e.created_by = ?
		`, source).Scan(&mc, &pe, &ne, &te)
		if err != nil {
			return nil, fmt.Errorf("aggregate evidence: %w", err)
		}

		// Avg current confidence + avg delta (current - earliest history.confidence)
		// + count of memories that survived (confidence >= 0.5).
		//
		// Compute AVG and survived over DISTINCT memory rows to avoid the
		// join-multiplication bug: if memory m1 has 3 evidence rows, a naive
		// AVG(m.confidence) is still correct (same value repeated), but
		// SUM(CASE WHEN m.confidence >= 0.5 THEN 1 ELSE 0 END) would
		// return 3 instead of 1. The fix is to aggregate over a per-memory
		// row first (subquery) so each memory counts exactly once for
		// survived and contributes one value to the AVG.
		var avgConf, avgDelta float64
		var survived int
		err = dm.QueryRowTracked(`
			SELECT
				COALESCE(AVG(mc.confidence), 0),
				COALESCE(AVG(
					mc.confidence - COALESCE(
						(SELECT h.confidence FROM confidence_history h
						 WHERE h.artifact_id = mc.id AND h.artifact_type = 'memory'
						 ORDER BY h.computed_at ASC LIMIT 1),
						mc.confidence
					)
				), 0),
				COALESCE(SUM(CASE WHEN mc.confidence >= 0.5 THEN 1 ELSE 0 END), 0)
			FROM (
				SELECT DISTINCT m.id, m.confidence
				FROM memories m
				JOIN evidence e ON e.artifact_id = m.id
					AND e.artifact_type = 'memory' AND e.source_group = 'auto_capture'
				WHERE e.created_by = ?
			) mc
		`, source).Scan(&avgConf, &avgDelta, &survived)
		if err != nil {
			return nil, fmt.Errorf("aggregate confidence: %w", err)
		}

		// Challenge count.
		var challenges int
		err = dm.QueryRowTracked(`
			SELECT COUNT(DISTINCT e.artifact_id)
			FROM evidence e
			WHERE e.artifact_type = 'memory' AND e.type = 'challenge'
			  AND e.artifact_id IN (
			    SELECT artifact_id FROM evidence
			    WHERE source_group = 'auto_capture' AND created_by = ?
			  )
		`, source).Scan(&challenges)
		if err != nil {
			return nil, fmt.Errorf("aggregate challenges: %w", err)
		}

		var challengeRate, survivalRate float64
		if mc > 0 {
			challengeRate = float64(challenges) / float64(mc)
			survivalRate = float64(survived) / float64(mc)
		}

		out = append(out, MemoryQualityBySource{
			Source:               source,
			MemoryCount:          mc,
			AvgCurrentConfidence: avgConf,
			AvgConfidenceDelta:   avgDelta,
			TotalEvidence:        te,
			PositiveEvidence:     pe,
			NegativeEvidence:     ne,
			ChallengeCount:       challenges,
			ChallengeRate:        challengeRate,
			SurvivalRate:         survivalRate,
		})
	}
	return out, nil
}
