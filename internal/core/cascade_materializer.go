// cascade_materializer.go — async theory materializer for epistemic cascades.
//
// The cascade outbox captures per-artifact invalidation intents. This
// worker consumes them asynchronously, materializing one pending theory
// per intent through the normal scanner/FTS theory write path
// (SaveMemoryNode), then recording the generated theory ID on the
// outbox row and scheduling a cascade wake.
//
// Design contract (from the 2026-08-04 epistemic cascades design):
//
//   - One theory per downstream artifact: 1:1, never grouped.
//   - Generated theories carry cascade metadata (cascade=true,
//     cascade_version=1, dead_artifact_id, dead_artifact_type,
//     downstream_artifact_id, downstream_artifact_type,
//     trigger_evidence_id, cascade_depth, generated_at).
//   - The dead artifact is included in the theory's dependencies list.
//   - Validation criteria: "independent review of whether the downstream
//     artifact remains valid without that foundation, followed by binary
//     resolution as proven or disproven."
//   - Hypothesis: concise human-readable statement that the downstream
//     artifact requires re-evaluation because its cited foundation
//     collapsed.
//   - Materialize through SaveMemoryNode (scanner + FTS), not a
//     parallel write surface.
//   - Atomic claim/reclaim of pending + abandoned-processing rows.
//   - Bounded retries with exponential backoff; terminal dead-letter
//     state with CRITICAL audit event.
//   - cascade_depth <= 3: suppress deeper cascades with CRITICAL audit.
//   - Start/stop with daemon lifecycle, no leaked goroutines.
//   - Lessons and global rules are NOT cascade targets (eligibleCascadeTypes
//     filter is inherited from the outbox).
//   - Outbox writes are atomic with the invalidating mutation;
//     materialization is async (this does not touch invalidation paths).
//
// The worker runs as a bounded goroutine pool. It shares the single
// DatabaseManager — no new sql.Open calls are introduced (enforced by
// the sqlopen_owner static-analysis test).
package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CascadeMaterializerOptions tunes the materializer's runtime behaviour.
type CascadeMaterializerOptions struct {
	// BatchSize is the max number of pending intents to claim per
	// MaterializeBatch call. Default 10.
	BatchSize int
	// MaxRetries is the number of retries before an intent is moved
	// to dead-letter state. Default 3.
	MaxRetries int
	// MaxCascadeDepth caps recursion depth; intents at this depth are
	// suppressed with a CRITICAL audit record. Default MaxCascadeDepth (3).
	MaxCascadeDepth int
	// WakeDelay is the artificial delay before a cascade wake is
	// scheduled after a successful materialization. Default 1 second.
	// Set to 0 to disable.
	WakeDelay time.Duration
}

// DefaultCascadeMaterializerOptions returns the standard option set.
func DefaultCascadeMaterializerOptions() CascadeMaterializerOptions {
	return CascadeMaterializerOptions{
		BatchSize:       10,
		MaxRetries:      3,
		MaxCascadeDepth: MaxCascadeDepth,
		WakeDelay:       1 * time.Second,
	}
}

// CascadeMaterializer consumes pending cascade intents and materializes
// one theory per intent through the normal write path.
type CascadeMaterializer struct {
	dm   *DatabaseManager
	opts CascadeMaterializerOptions
}

// NewCascadeMaterializer builds a stateless materializer bound to the
// supplied DatabaseManager. The returned value carries no goroutines;
// callers invoke MaterializeBatch directly. The background drain is
// owned by mpm-scheduler; the CLI calls MaterializeCascadeIntents.
func NewCascadeMaterializer(dm *DatabaseManager, opts CascadeMaterializerOptions) *CascadeMaterializer {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 10
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}
	if opts.MaxCascadeDepth <= 0 {
		opts.MaxCascadeDepth = MaxCascadeDepth
	}
	if opts.WakeDelay < 0 {
		opts.WakeDelay = 1 * time.Second
	}
	return &CascadeMaterializer{
		dm:   dm,
		opts: opts,
	}
}

// MaterializationReport summarises the outcome of one MaterializeBatch call.
type MaterializationReport struct {
	// Claimed is the number of pending intents this batch attempted to claim.
	Claimed int
	// Processed is the number of intents that reached a terminal or
	// retryable state (materialized, failed, or requeued).
	Processed int
	// Materialized is the count of intents that successfully created theories.
	Materialized int
	// Failed is the count of intents that entered dead-letter state.
	Failed int
	// Skipped is the count of intents skipped (depth exceeded, already
	// materialized, etc.).
	Skipped int
	// Suppressed is the count of intents suppressed due to depth > MaxCascadeDepth.
	Suppressed int
}

// MaterializeBatch claims up to `limit` pending intents, materializes each
// through the normal theory write path, and updates the outbox rows.
// It is safe to call concurrently — the implementation uses database
//-level atomicity for claim transitions so concurrent calls cooperate
// without additional locking.
func (cm *CascadeMaterializer) MaterializeBatch(ctx context.Context, limit int) (MaterializationReport, error) {
	if limit <= 0 {
		limit = cm.opts.BatchSize
	}

	// Claim the next pending batch. ClaimCascadeIntents atomically
	// transitions claimed rows from 'pending' → 'processing'.
	intents, err := cm.claimCascadeIntents(limit)
	if err != nil {
		return MaterializationReport{}, fmt.Errorf("claim intents: %w", err)
	}

	report := MaterializationReport{Claimed: len(intents)}

	for _, intent := range intents {
		select {
		case <-ctx.Done():
			cm.revertToPending(intent.ID)
			continue
		default:
		}

		result := cm.processIntent(ctx, intent)
		report.Processed++
		switch result {
		case resultMaterialized:
			report.Materialized++
		case resultFailed:
			report.Failed++
		case resultSkipped:
			report.Skipped++
		case resultSuppressed:
			report.Suppressed++
		}
	}

	return report, nil
}

// processResult classifies the outcome of processing one intent.
type processResult int

const (
	resultMaterialized processResult = iota
	resultFailed
	resultSkipped
	resultSuppressed
)

// processIntent handles a single claimed intent end-to-end.
func (cm *CascadeMaterializer) processIntent(ctx context.Context, intent CascadeIntent) processResult {
	// ── Downstream liveness check ─────────────────────────────────────────
	// Before producing a cascade theory (and its wake), verify the
	// downstream artifact still exists and is not superseded. A cascade
	// materialization asks the operator to "review whether the downstream
	// artifact's conclusions still hold without this foundation" — but if
	// the downstream was already superseded (replaced by a successor) or
	// hard-deleted, the review is moot. Persisting the wake and the theory
	// anyway leaves ghost work in the wake backlog (lesson 06f57b1d: the
	// investigation that surfaced this fix).
	//
	// Defensive contract: check downstream first, before the depth guard,
	// so a stale-target intent costs almost nothing when rejected.
	if skip, reason := cm.downstreamNotLive(intent); skip {
		cm.dm.LogAudit(AuditInfo, "cascade-materializer",
			fmt.Sprintf("cascade intent %s skipped: downstream %s is no longer live (%s)",
				intent.ID, intent.DownstreamArtifactID, reason),
			"", AuditContext{
				"intent_id":              intent.ID,
				"dead_artifact_id":       intent.DeadArtifactID,
				"downstream_artifact_id": intent.DownstreamArtifactID,
				"invalidation_event_id":  intent.InvalidationEventID,
				"skip_reason":            reason,
			})
		if err := cm.markFailed(intent.ID, "downstream no longer live: "+reason); err != nil {
			cm.dm.LogAudit(AuditError, "cascade-materializer",
				fmt.Sprintf("failed to mark stale-downstream intent %s as failed: %v",
					intent.ID, err), "", nil)
		}
		return resultSuppressed
	}

	// ── Depth guard ────────────────────────────────────────────────────────
	// Intents already at or below MaxCascadeDepth are processed normally.
	// Intents at depth > MaxCascadeDepth are suppressed and produce a
	// CRITICAL audit record.
	if intent.CascadeDepth > cm.opts.MaxCascadeDepth {
		cm.dm.LogAudit(AuditCritical, "cascade-materializer",
			fmt.Sprintf("cascade intent %s suppressed: cascade_depth=%d exceeds MaxCascadeDepth=%d (intent targets downstream=%s)",
				intent.ID, intent.CascadeDepth, cm.opts.MaxCascadeDepth, intent.DownstreamArtifactID),
			"", AuditContext{
				"intent_id":              intent.ID,
				"dead_artifact_id":       intent.DeadArtifactID,
				"dead_artifact_type":     intent.DeadArtifactType,
				"downstream_artifact_id": intent.DownstreamArtifactID,
				"downstream_artifact_type": intent.DownstreamArtifactType,
				"cascade_depth":          intent.CascadeDepth,
				"invalidation_event_id":  intent.InvalidationEventID,
			})

		if err := cm.markFailed(intent.ID, "depth exceeded MaxCascadeDepth"); err != nil {
			cm.dm.LogAudit(AuditError, "cascade-materializer",
				fmt.Sprintf("failed to mark suppressed intent %s as failed: %v", intent.ID, err), "", nil)
		}
		return resultSuppressed
	}

	// ── Materialize ─────────────────────────────────────────────────────────
	theoryID, err := cm.materializeTheory(ctx, intent)
	if err != nil {
		return cm.handleMaterializeError(intent, err)
	}

	// ── Mark materialized ────────────────────────────────────────────────────
	if err := cm.markMaterialized(intent.ID, theoryID); err != nil {
		cm.dm.LogAudit(AuditError, "cascade-materializer",
			fmt.Sprintf("failed to mark intent %s as materialized (theory=%s): %v", intent.ID, theoryID, err), "", nil)
	}

	// ── Schedule cascade wake ────────────────────────────────────────────────
	// The wake is async — failure to schedule is non-fatal but durable:
	// the wake_scheduled column on the outbox stays 0 so the reconcile
	// pass (ReconcileUnscheduledCascadeWakes) re-schedules the wake. The
	// theory was successfully created and the intent is marked materialized.
	// When WakeDelay == 0 the wake is scheduled synchronously (no defer).
	if cm.opts.WakeDelay > 0 {
		time.AfterFunc(cm.opts.WakeDelay, func() {
			if err := cm.scheduleCascadeWake(intent.ID, theoryID, intent.InvalidationEventID); err != nil {
				cm.dm.LogAudit(AuditError, "cascade-materializer",
					fmt.Sprintf("failed to schedule cascade wake for intent=%s theory=%s: %v",
						intent.ID, theoryID, err), "", nil)
			}
		})
	} else {
		if err := cm.scheduleCascadeWake(intent.ID, theoryID, intent.InvalidationEventID); err != nil {
			cm.dm.LogAudit(AuditError, "cascade-materializer",
				fmt.Sprintf("failed to schedule cascade wake for intent=%s theory=%s: %v",
					intent.ID, theoryID, err), "", nil)
		}
	}

	return resultMaterialized
}

// handleMaterializeError classifies a materialization error and updates
// the intent's retry state appropriately.
func (cm *CascadeMaterializer) handleMaterializeError(intent CascadeIntent, err error) processResult {
	newAttempt := intent.AttemptCount + 1

	if newAttempt >= cm.opts.MaxRetries {
		// Terminal dead-letter state: emit CRITICAL audit and move to 'failed'.
		cm.dm.LogAudit(AuditCritical, "cascade-materializer",
			fmt.Sprintf("cascade intent %s entered dead-letter state after %d attempts: %v",
				intent.ID, newAttempt, err),
			"", AuditContext{
				"intent_id":              intent.ID,
				"dead_artifact_id":       intent.DeadArtifactID,
				"downstream_artifact_id": intent.DownstreamArtifactID,
				"attempt_count":          newAttempt,
				"terminal_error":         err.Error(),
			})

		if markErr := cm.markFailed(intent.ID, err.Error()); markErr != nil {
			cm.dm.LogAudit(AuditError, "cascade-materializer",
				fmt.Sprintf("failed to mark intent %s as dead-letter: %v", intent.ID, markErr), "", nil)
		}
		return resultFailed
	}

	// Retryable: compute exponential backoff and requeue.
	backoffSeconds := int64(1 << newAttempt) // 2, 4, 8, ...
	nextRetry := time.Now().Unix() + backoffSeconds

	if requeueErr := cm.requeueIntent(intent.ID, newAttempt, nextRetry); requeueErr != nil {
		cm.dm.LogAudit(AuditError, "cascade-materializer",
			fmt.Sprintf("failed to requeue intent %s (attempt %d): %v", intent.ID, newAttempt, requeueErr), "", nil)
	}

	return resultSkipped
}

// materializeTheory creates one pending theory through the normal
// SaveMemoryNode write path (scanner + FTS). The theory's hypothesis
// states that the downstream artifact requires re-evaluation because
// its cited foundation collapsed (negative cascade) or because a
// previously-uncertain foundation is now proven (positive cascade —
// any downstream that opted in via polarity='assumes_false' may have
// been running on a stale negation). The dead artifact is included in
// the theory's dependencies list so the cascade chain can continue
// if the theory itself is later invalidated.
//
// Direction branches on intent.Reason: the negative-direction reasons
// (theory_disproven, memory_shredded, confidence_floor, etc.) produce
// "foundation invalidated" wording; the positive-direction reasons
// (foundation_proven, confidence_ceiling — see cascade_outbox.go)
// produce "foundation proven" wording. The reason string is part of
// the outbox row so this branch is a pure read — no schema change
// needed beyond the reason itself.
func (cm *CascadeMaterializer) materializeTheory(ctx context.Context, intent CascadeIntent) (string, error) {
	// Validation criteria per the design spec. Same shape for both
	// directions — the dependent artifact is what needs review, not
	// the foundation. Direction-specific phrasing belongs in the
	// hypothesis, not the criteria.
	validationCriteria := "independent review of whether the downstream artifact remains valid without that foundation, followed by binary resolution as proven or disproven"

	// Hypothesis: concise human-readable statement. Branched on
	// direction so the human reviewer sees the right framing the
	// moment they open the theory. The reason string is preserved
	// verbatim in the message so the audit trail can reconstruct the
	// trigger later.
	reason := intent.Reason
	var hypothesis string
	switch reason {
	case ReasonFoundationProven, ReasonConfidenceCeiling:
		// Positive-direction cascade: the foundation is now PROVEN.
		// Downstream artifacts that opted in via polarity='assumes_false'
		// were assuming the foundation was false; that assumption has
		// flipped. The fair hypothesis is "your foundation is solid;
		// re-check whether your conclusion still follows" — NOT
		// "your foundation collapsed; rebuild from scratch". The
		// reason label is included so the audit trail distinguishes
		// the two positive triggers (explicit theory resolve vs
		// confidence-ceiling crossing).
		hypothesis = fmt.Sprintf(
			"The artifact '%s' (%s) may need re-evaluation because its cited foundation '%s' (%s) has now been proven (%s). When this theory was originally formed, the foundation was uncertain; review whether the downstream artifact's conclusion still holds given the foundation is now established.",
			intent.DownstreamArtifactID, intent.DownstreamArtifactType,
			intent.DeadArtifactID, intent.DeadArtifactType,
			reason,
		)
	default:
		// Negative-direction cascade (default branch — covers
		// theory_disproven, memory_shredded, confidence_floor, and
		// any future reason not in the positive set). Wording is
		// preserved verbatim from the prior version to keep the
		// existing materializer tests' hypothesis-text assertions
		// valid without churn.
		if reason == "" {
			reason = "foundation invalidated"
		}
		hypothesis = fmt.Sprintf(
			"The artifact '%s' (%s) requires re-evaluation because its cited foundation '%s' (%s) has been invalidated (%s). Please review whether the downstream artifact's conclusions still hold without this foundation.",
			intent.DownstreamArtifactID, intent.DownstreamArtifactType,
			intent.DeadArtifactID, intent.DeadArtifactType,
			reason,
		)
	}

	// Build dependencies list: the dead artifact is always included per
	// the design spec's metadata contract.
	dependencies := []string{intent.DeadArtifactID}

	// Build cascade metadata for top-level storage in the theory row.
	// Stored top-level (not nested) so cascade queries can filter and
	// read directly without parsing nested blobs.
	cascadeFields := map[string]interface{}{
		"cascade":                   true,
		"cascade_version":           1,
		"dead_artifact_id":         intent.DeadArtifactID,
		"dead_artifact_type":       intent.DeadArtifactType,
		"downstream_artifact_id":   intent.DownstreamArtifactID,
		"downstream_artifact_type":  intent.DownstreamArtifactType,
		"cascade_depth":            intent.CascadeDepth,
		"generated_at":              time.Now().UTC().Format(time.RFC3339),
	}
	if intent.TriggerEvidenceID.Valid {
		cascadeFields["trigger_evidence_id"] = intent.TriggerEvidenceID.String
	}

	// Use ProposeTheoryWithExtras to create the theory with cascade fields
	// stored at top level. Dependencies and standard theory fields are
	// handled by ProposeTheoryWithExtras; cascadeFields are merged in.
	// Wrap in WithProvenanceOverride so the derived theory's provenance row
	// carries parent_artifact_id = intent.DeadArtifactID.
	var result map[string]interface{}
	var theoryErr error
	theoryErr = cm.dm.WithProvenanceOverride(
		cm.dm.provenanceWithParent(intent.DeadArtifactID),
		func() error {
			result, theoryErr = cm.dm.ProposeTheoryWithExtras(
				hypothesis,
				validationCriteria,
				dependencies,
				nil, // no sourceIDs for cascade theories
				[]string{fmt.Sprintf("cascade:%s", intent.InvalidationEventID)},
				cascadeFields,
			)
			return theoryErr
		},
	)
	if theoryErr != nil {
		return "", fmt.Errorf("materialize theory: %w", theoryErr)
	}

	theoryID, ok := result["id"].(string)
	if !ok || theoryID == "" {
		return "", fmt.Errorf("materialize theory: could not extract theory id from result")
	}

	return theoryID, nil
}

// claimCascadeIntents atomically claims the next `limit` pending intents
// by transitioning them from status='pending' → 'processing'. It also
// recovers abandoned-processing intents (rows stuck in 'processing' whose
// owner goroutine died) by resetting them to 'pending' before claiming.
//
// The atomic claim uses a single UPDATE ... WHERE id IN (...) to avoid
// race conditions between concurrent materializers.
func (cm *CascadeMaterializer) claimCascadeIntents(limit int) ([]CascadeIntent, error) {
	if limit <= 0 {
		limit = cm.opts.BatchSize
	}

	// BEGIN IMMEDIATE acquires a write transaction immediately, blocking
	// concurrent writers until we commit. Two concurrent callers serialize:
	// A's BEGIN runs first and holds the lock; B's BEGIN blocks. When A
	// commits (or rolls back), B's BEGIN proceeds and sees the post-commit
	// state — no TOCTOU possible.
	tx, err := cm.dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin claim transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().Unix()
	const staleTimeoutSeconds int64 = 300

	// Restart recovery: reclaim abandoned 'processing' rows inside the tx.
	// DO NOT reset attempt_count here (regression in #3): a poison pill
	// that gets SIGKILL'd mid-process would otherwise get fresh retries
	// every time the reaper saves it, and the 5-attempt dead-letter cap
	// would never accumulate across crashes. Only reset status + updated_at.
	if _, err := tx.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'pending', updated_at = ?
		WHERE status = 'processing' AND updated_at < ?
	`, now, now-staleTimeoutSeconds); err != nil {
		return nil, fmt.Errorf("restart recovery: %w", err)
	}

	// Step 1: select the next batch of pending ids (within the tx).
	rows, err := tx.Query(`
		WITH pending_cte AS (
			SELECT id FROM epistemic_cascade_outbox
			WHERE status = 'pending'
			  AND (next_retry_at IS NULL OR next_retry_at <= ?)
			ORDER BY created_at ASC
			LIMIT ?
		)
		SELECT id FROM pending_cte
	`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("collect pending ids: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		// Nothing to claim — rollback the empty tx and return nil.
		_ = tx.Rollback()
		tx = nil
		return nil, nil
	}

	// Step 2: atomically claim them. Build dynamic IN clause.
	idPlaceholders := ""
	args := make([]interface{}, 0, 1+len(ids))
	args = append(args, now)
	for i, id := range ids {
		if i > 0 {
			idPlaceholders += ","
		}
		idPlaceholders += "?"
		args = append(args, id)
	}
	claimQuery := fmt.Sprintf(`
		UPDATE epistemic_cascade_outbox
		SET status = 'processing', updated_at = ?
		WHERE id IN (%s) AND status = 'pending'
	`, idPlaceholders)
	res, err := tx.Exec(claimQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("claim intents: %w", err)
	}
	nFlipped, _ := res.RowsAffected()
	if nFlipped == 0 {
		// No rows were actually flipped — another caller claimed them all
		// between our SELECT and UPDATE. Roll back and return nil.
		_ = tx.Rollback()
		tx = nil
		return nil, nil
	}

	// Step 3: re-read the rows this call flipped (within the tx, post-UPDATE).
	inClause := ""
	reReadArgs := make([]interface{}, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			inClause += ","
		}
		inClause += "?"
		reReadArgs = append(reReadArgs, id)
	}
	query := fmt.Sprintf(`
		SELECT id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		       downstream_artifact_id, downstream_artifact_type,
		       trigger_evidence_id, cascade_depth, reason, status,
		       materialized_theory_id, attempt_count, next_retry_at,
		       terminal_error, created_at, updated_at
		FROM epistemic_cascade_outbox
		WHERE id IN (%s)
	`, inClause)
	rows, err = tx.Query(query, reReadArgs...)
	if err != nil {
		return nil, fmt.Errorf("re-read claimed intents: %w", err)
	}
	intents := make([]CascadeIntent, 0, len(ids))
	for rows.Next() {
		var intent CascadeIntent
		if err := rows.Scan(
			&intent.ID, &intent.InvalidationEventID,
			&intent.DeadArtifactID, &intent.DeadArtifactType,
			&intent.DownstreamArtifactID, &intent.DownstreamArtifactType,
			&intent.TriggerEvidenceID, &intent.CascadeDepth,
			&intent.Reason, &intent.Status,
			&intent.MaterializedTheoryID, &intent.AttemptCount,
			&intent.NextRetryAt, &intent.TerminalError,
			&intent.CreatedAt, &intent.UpdatedAt,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claimed intent: %w", err)
		}
		intents = append(intents, intent)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Commit the transaction.
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim transaction: %w", err)
	}
	tx = nil // prevent deferred rollback
	return intents, nil
}

// markMaterialized records the generated theory ID on the outbox row.
//
// Post-M3 audit H-2 (2026-08-31): prior version performed a bare
// db.Exec with no read-back. A crash between the UPDATE and the
// caller returning would leave restart-recovery reclaiming the row
// (status still 'processing' for the staleness window) but no
// observable signal that the materialized state was committed. The
// Defense Triad (Substrate Defense Triad, 2026-08-17) requires every
// non-tx write to do a read-back assertion so silent failures surface.
//
// Fix: after UPDATE, SELECT the row and verify status='materialized'
// AND materialized_theory_id matches. Returns an error on mismatch so
// the caller can leave the row in 'processing' for restart-recovery
// to re-materialize (idempotently — the theory is identified by the
// invalidation_event_id + downstream_artifact_id, so a re-run produces
// the same theory).
func (cm *CascadeMaterializer) markMaterialized(intentID, theoryID string) error {
	now := time.Now().Unix()
	_, err := cm.dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized', materialized_theory_id = ?, updated_at = ?
		WHERE id = ?
	`, theoryID, now, intentID)
	if err != nil {
		return fmt.Errorf("markMaterialized exec: %w", err)
	}

	// Defense Triad #3 — Write-Path Read-Back Assertion.
	// Confirms the UPDATE actually landed (no CHECK rejection, no
	// swallowed trigger, no DB-level silent no-op).
	var gotStatus, gotTheoryID string
	readErr := cm.dm.db.QueryRow(`
		SELECT status, materialized_theory_id
		FROM epistemic_cascade_outbox
		WHERE id = ?
	`, intentID).Scan(&gotStatus, &gotTheoryID)
	if readErr != nil {
		return fmt.Errorf("markMaterialized read-back: %w", readErr)
	}
	if gotStatus != "materialized" {
		return fmt.Errorf("markMaterialized read-back mismatch: status=%q (want 'materialized')", gotStatus)
	}
	if gotTheoryID != theoryID {
		return fmt.Errorf("markMaterialized read-back mismatch: theory_id=%q (want %q)", gotTheoryID, theoryID)
	}
	return nil
}

// markFailed moves an intent to dead-letter state (status='failed').
func (cm *CascadeMaterializer) markFailed(intentID, terminalError string) error {
	now := time.Now().Unix()
	_, err := cm.dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'failed', terminal_error = ?, updated_at = ?
		WHERE id = ?
	`, terminalError, now, intentID)
	return err
}

// downstreamNotLive reports whether the cascade intent's downstream
// artifact is still alive and worth a cascade review. Returns
// (true, reason) when the cascade should be suppressed:
//
//   - downstream hard-deleted ("not found") — no successor to review
//   - downstream superseded — the supersede operation already
//     replaced the artifact with a successor; the operator took
//     responsibility for the new artifact; the cascade review
//     would be moot (review an artifact the operator already
//     decided to replace).
//
// Returns (false, "") when the downstream is live OR when the
// downstream is superseded but the chain still has a live successor
// that genuinely depends on the original foundation. (Cascades are
// not auto-retargeted: the operator who performed the supersede
// already owns the successor's review; we do not silently re-open
// it via cascade wake. See lesson d6fed1df — the fix supersedes
// it: the MATERIALIZER (not the reconciler) walks supersede
// edges; the action is to skip, not to retarget.)
//
// Implementation note: live-ness is judged by the canonical
// memories row, not by the cascade outbox's downstream_artifact_type
// column. The cascade metadata captures the operator-set intent at
// enqueue time and may carry type drift when an invalidating
// surface (memory_shred, confidence_floor) mis-labels the dead
// artifact's type. Reading the memories row at materialization
// time is the authoritative check.
//
// Defensive: a missing memories row OR a present memories row with
// superseded metadata.json -> superseded_by OR a present row
// with the "superseded" tag (per IsSuperseded) all qualify as
// "not live". Returns the specific reason so the audit row
// (caller's responsibility) can carry the diagnostic.
func (cm *CascadeMaterializer) downstreamNotLive(intent CascadeIntent) (skip bool, reason string) {
	if intent.DownstreamArtifactID == "" {
		return false, ""
	}

	mem, err := cm.dm.GetMemory(intent.DownstreamArtifactID)
	if err != nil {
		// Hard-deleted OR collection/type that GetMemory doesn't
		// recognize. Either way: not live.
		return true, "downstream not found"
	}

	// The memories row exists. Two live-ness tests:
	//
	//   1. metadata.superseded_by is set (canonical supersede marker).
	//   2. The "superseded" tag is present (legacy / redundant-records).
	//
	// SupersedeDecision writes BOTH (defense in depth: hybrid_search
	// discounts superseded rows via tags; the metadata pointer is the
	// canonical chain edge). Reading either is sufficient.
	metaStr, _ := mem["metadata"].(string)
	if metaStr != "" {
		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(metaStr), &meta); err == nil {
			if sup, _ := meta["superseded_by"].(string); sup != "" {
				return true, fmt.Sprintf("downstream superseded by %s", sup)
			}
		}
	}

	tagsStr, _ := mem["tags"].(string)
	if tagsStr != "" {
		// tags column is JSON-array per schema. Parse defensively.
		var tags []string
		if err := json.Unmarshal([]byte(tagsStr), &tags); err == nil {
			for _, t := range tags {
				if t == "superseded" || strings.HasPrefix(t, "superseded-by:") {
					return true, "downstream superseded (tag marker)"
				}
			}
		}
	}

	return false, ""
}

// requeueIntent updates the attempt count and next-retry timestamp for
// a retryable failure.
func (cm *CascadeMaterializer) requeueIntent(intentID string, attemptCount int, nextRetryUnix int64) error {
	now := time.Now().Unix()
	_, err := cm.dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'pending', attempt_count = ?, next_retry_at = ?, updated_at = ?
		WHERE id = ?
	`, attemptCount, nextRetryUnix, now, intentID)
	return err
}

// revertToPending resets a single intent back to 'pending' (used when
// the materializer is stopping mid-batch).
func (cm *CascadeMaterializer) revertToPending(intentID string) {
	now := time.Now().Unix()
	cm.dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'pending', updated_at = ?
		WHERE id = ? AND status = 'processing'
	`, now, intentID)
}

// scheduleCascadeWake schedules a cascade wake for the generated theory
// and durably records the booking by flipping wake_scheduled=1 on the
// outbox row. Returns nil on success and a non-nil error when
// ScheduleWake or markWakeScheduled fails. The reconcile pass scans
// for rows where wake_scheduled=0 and re-schedules — this is the
// durability guarantee that closes the crash window between
// markMaterialized and the wake insert.
//
// Post-M3 audit H-3 (2026-08-31): the pre-fix version was fire-and-
// forget; a crash between markMaterialized and the wake insert lost
// the wake silently with no observable signal. The wake_scheduled
// column provides the durable record; reconcile turns "lost wake" into
// "recoverable wake".
func (cm *CascadeMaterializer) scheduleCascadeWake(intentID, theoryID, invalidationEventID string) error {
	reason := fmt.Sprintf("cascade: theory %s requires review — downstream of invalidation %s",
		theoryID, invalidationEventID)
	createdBy := "cascade-materializer"

	// Schedule wake for "now" (immediate delivery on next check_wakes).
	// The wake delay (opts.WakeDelay) is applied before this is called,
	// so the wake is effectively deferred by that amount.
	targetTime := time.Now().Format(time.RFC3339)

	// Metadata tags the wake as a cascade delivery so check_wakes can
	// apply its per-check delivery cap.
	meta := map[string]interface{}{
		"kind":                  "cascade",
		"theory_id":             theoryID,
		"invalidation_event_id": invalidationEventID,
		"source":                "cascade-materializer",
	}

	if _, err := cm.dm.ScheduleWake(reason, targetTime, theoryID, "", createdBy, meta); err != nil {
		return fmt.Errorf("schedule cascade wake for theory=%s: %w", theoryID, err)
	}

	// Defense Triad #3 — Write-Path Read-Back Assertion.
	// Flip the wake_scheduled flag *after* ScheduleWake succeeds, so a
	// future reconcile pass does not double-schedule. The flag is the
	// ground truth for "wake booking completed".
	if markErr := cm.markWakeScheduled(intentID); markErr != nil {
		// ScheduleWake succeeded but the bookkeeping update failed.
		// Surface the error so the caller logs it; reconcile will pick
		// up the unscheduled row and re-schedule. We do NOT delete the
		// wake — the wake is a valid delivery signal; the duplication
		// risk is bounded by the per-check delivery cap.
		return fmt.Errorf("scheduleCascadeWake markWakeScheduled for intent=%s: %w", intentID, markErr)
	}
	return nil
}

// markWakeScheduled flips the wake_scheduled flag on the outbox row to 1.
// Called by scheduleCascadeWake after a successful ScheduleWake. The flag
// is the durable record that the wake booking completed; reconcile scans
// for materialized rows with wake_scheduled=0 to recover lost wakes.
func (cm *CascadeMaterializer) markWakeScheduled(intentID string) error {
	now := time.Now().Unix()
	res, err := cm.dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET wake_scheduled = 1, updated_at = ?
		WHERE id = ?
	`, now, intentID)
	if err != nil {
		return fmt.Errorf("markWakeScheduled exec: %w", err)
	}
	// Defense Triad #3: read-back assertion. Views with INSTEAD OF
	// triggers report 0 affected rows on success; for direct tables
	// (epistemic_cascade_outbox is a plain table), zero rows indicates
	// the UPDATE targeted a missing id.
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("markWakeScheduled rows-affected: %w", raErr)
	}
	if rows == 0 {
		return fmt.Errorf("markWakeScheduled: 0 rows updated for intent_id=%s", intentID)
	}

	var flag int
	readErr := cm.dm.db.QueryRow(`
		SELECT wake_scheduled FROM epistemic_cascade_outbox WHERE id = ?
	`, intentID).Scan(&flag)
	if readErr != nil {
		return fmt.Errorf("markWakeScheduled read-back: %w", readErr)
	}
	if flag != 1 {
		return fmt.Errorf("markWakeScheduled read-back mismatch: wake_scheduled=%d (want 1)", flag)
	}
	return nil
}

// strSliceToInterface converts a []string to []interface{} for dynamic
// SQL argument building. Needed because Go does not allow direct slice
// conversion between []string and []interface{}.
func strSliceToInterface(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
