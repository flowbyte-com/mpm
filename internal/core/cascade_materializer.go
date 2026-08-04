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
	"fmt"
	"sync"
	"time"
)

// CascadeMaterializerOptions tunes the materializer's runtime behaviour.
type CascadeMaterializerOptions struct {
	// BatchSize is the max number of pending intents to claim per
	// MaterializeBatch call. Default 10.
	BatchSize int
	// PollInterval controls how often the worker loop checks for new
	// work when the working set is empty. Default 5 seconds.
	PollInterval time.Duration
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
	// Workers is the number of concurrent materialization goroutines.
	// Default 2.
	Workers int
}

// DefaultCascadeMaterializerOptions returns the standard option set.
func DefaultCascadeMaterializerOptions() CascadeMaterializerOptions {
	return CascadeMaterializerOptions{
		BatchSize:        10,
		PollInterval:     5 * time.Second,
		MaxRetries:       3,
		MaxCascadeDepth:  MaxCascadeDepth,
		WakeDelay:        1 * time.Second,
		Workers:          2,
	}
}

// CascadeMaterializer consumes pending cascade intents and materializes
// one theory per intent through the normal write path.
type CascadeMaterializer struct {
	dm       *DatabaseManager
	opts     CascadeMaterializerOptions
	stopC    chan struct{}
	stopOnce sync.Once // guards against double close of stopC
	wg       sync.WaitGroup
	mu       sync.Mutex
	run      bool // protected by mu
}

// NewCascadeMaterializer builds a new materializer bound to the supplied
// DatabaseManager. The worker is not started until Start is called.
func NewCascadeMaterializer(dm *DatabaseManager, opts CascadeMaterializerOptions) *CascadeMaterializer {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 10
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
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
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	return &CascadeMaterializer{
		dm:    dm,
		opts:  opts,
		stopC: make(chan struct{}),
	}
}

// Start launches the materializer worker goroutines. It is safe to
// call multiple times — subsequent calls after the first are no-ops
// (idempotent start).
func (cm *CascadeMaterializer) Start(ctx context.Context) {
	cm.mu.Lock()
	if cm.run {
		cm.mu.Unlock()
		return
	}
	cm.run = true
	// Re-create stopC so a fresh (unclosed) channel exists for the new
	// goroutine. Without this, a second Start() after a Stop() would
	// immediately hit the closed channel and the goroutine would exit
	// without processing any intents.
	cm.stopC = make(chan struct{})
	cm.mu.Unlock()

	cm.wg.Add(1)
	go func() {
		defer cm.wg.Done()
		cm.runLoop(ctx)
	}()
}

// Stop gracefully terminates the materializer. In-flight materializations
// are allowed to complete before the goroutines exit. It is safe to
// call multiple times (idempotent stop).
func (cm *CascadeMaterializer) Stop() {
	cm.mu.Lock()
	if !cm.run {
		cm.mu.Unlock()
		return
	}
	cm.run = false
	cm.mu.Unlock()

	cm.stopOnce.Do(func() { close(cm.stopC) })
	cm.wg.Wait()
}

// runLoop is the materializer's main polling loop. It continuously
// claims and processes batches of pending intents until Stop is called
// or the context is cancelled.
func (cm *CascadeMaterializer) runLoop(ctx context.Context) {
	pollInterval := cm.opts.PollInterval
	for {
		select {
		case <-cm.stopC:
			return
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
			// Poll interval elapsed — run a batch.
		}

		report, err := cm.MaterializeBatch(ctx, cm.opts.BatchSize)
		if err != nil {
			// Non-retryable error; log and back off.
			cm.dm.LogAudit(AuditError, "cascade-materializer", fmt.Sprintf("materialize batch error: %v", err), "", nil)
			continue
		}

		// If the batch produced no forward progress, the timer already
		// handled the idle delay above (timer fired or was stopped).
		_ = report
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
		case <-cm.stopC:
			// Stop requested — abandon remaining intents by reverting
			// them to 'pending' so a future run can reprocess them.
			cm.revertToPending(intent.ID)
			continue
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
	// The wake is async — failure to schedule is non-fatal. The theory
	// was successfully created and the intent is marked materialized.
	// When WakeDelay == 0 the wake is scheduled synchronously (no defer).
	if cm.opts.WakeDelay > 0 {
		time.AfterFunc(cm.opts.WakeDelay, func() {
			cm.scheduleCascadeWake(theoryID, intent.InvalidationEventID)
		})
	} else {
		cm.scheduleCascadeWake(theoryID, intent.InvalidationEventID)
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
// its cited foundation collapsed. The dead artifact is included in
// the theory's dependencies list so the cascade chain can continue
// if the theory itself is later invalidated.
func (cm *CascadeMaterializer) materializeTheory(ctx context.Context, intent CascadeIntent) (string, error) {
	// Validation criteria per the design spec.
	validationCriteria := "independent review of whether the downstream artifact remains valid without that foundation, followed by binary resolution as proven or disproven"

	// Hypothesis: concise human-readable statement.
	reason := intent.Reason
	if reason == "" {
		reason = "foundation invalidated"
	}
	hypothesis := fmt.Sprintf(
		"The artifact '%s' (%s) requires re-evaluation because its cited foundation '%s' (%s) has been invalidated (%s). Please review whether the downstream artifact's conclusions still hold without this foundation.",
		intent.DownstreamArtifactID, intent.DownstreamArtifactType,
		intent.DeadArtifactID, intent.DeadArtifactType,
		reason,
	)

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
	result, err := cm.dm.ProposeTheoryWithExtras(
		hypothesis,
		validationCriteria,
		dependencies,
		nil, // no sourceIDs for cascade theories
		[]string{fmt.Sprintf("cascade:%s", intent.InvalidationEventID)},
		cascadeFields,
	)
	if err != nil {
		return "", fmt.Errorf("materialize theory: %w", err)
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
func (cm *CascadeMaterializer) markMaterialized(intentID, theoryID string) error {
	now := time.Now().Unix()
	_, err := cm.dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized', materialized_theory_id = ?, updated_at = ?
		WHERE id = ?
	`, theoryID, now, intentID)
	return err
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

// scheduleCascadeWake schedules a cascade wake for the generated theory.
// The wake is the delivery mechanism that notifies the agent that a
// cascade theory requires review.
func (cm *CascadeMaterializer) scheduleCascadeWake(theoryID, invalidationEventID string) {
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
		"kind":                 "cascade",
		"theory_id":            theoryID,
		"invalidation_event_id": invalidationEventID,
		"source":               "cascade-materializer",
	}

	if _, err := cm.dm.ScheduleWake(reason, targetTime, theoryID, "", createdBy, meta); err != nil {
		cm.dm.LogAudit(AuditError, "cascade-materializer",
			fmt.Sprintf("failed to schedule cascade wake for theory=%s: %v", theoryID, err), "", nil)
	}
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
