// backfill_snapshots.go — derive _epistemic_snapshot blocks for
// memories saved before the snapshot resolver existed.
//
// v spec 2026-08-04: alpha testers importing legacy databases need
// _epistemic_snapshot stamps on memories that pre-date the resolver.
// For each pre-resolver memory we derive what we can from substrate
// state (creator from provenance + session_id column, validation from
// evidence table) and stamp the snapshot. The transient blocks
// (execution, provenance, context) are unrecoverable for memories
// written before the resolver existed — those fields are omitted
// entirely, leaving a partial snapshot rather than fabricating data.
//
// Why chunked:
//
//   SQLite WAL mode = writers block writers. A multi-minute single
//   transaction on a legacy database would block every concurrent
//   mcp-mcp save_to_memory. Chunked transactions (default 500) keep
//   each WAL lock brief and let the scheduler / live agents slip in
//   between batches.
//
// Why the 50ms yield:
//
//   After committing a batch, immediately opening the next transaction
//   can create a tight loop that starves concurrent writers. A
//   deliberate micro-sleep yields the lock back to the OS scheduler.
//   Adds <50ms wall-clock per chunk; saves the alpha tester from
//   mysterious "save is hanging" reports during migration.
//
// Idempotency:
//
//   The function skips memories that already carry _epistemic_snapshot.
//   If interrupted, re-running picks up exactly where it left off —
//   no atomic rollback required, no merge conflicts on resume.
package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────

// DefaultBackfillBatchSize is the per-transaction chunk size. Tuned
// for a standard workstation; override via BackfillOptions.BatchSize
// for resource-constrained VMs (down) or large migrations (up).
const DefaultBackfillBatchSize = 500

// BackfillWALYield is the deliberate lock-yield pause between batch
// commits. See file-level comment for why this exists.
const BackfillWALYield = 50 * time.Millisecond

// BackfillOptions configures a single backfill run.
type BackfillOptions struct {
	// BatchSize is the number of memories per transaction. ≤0 falls
	// back to DefaultBackfillBatchSize. Hard ceiling 10000 to prevent
	// the very long-running-transaction antipattern this whole module
	// exists to avoid.
	BatchSize int

	// DryRun, if true, counts candidates and previews up to 5 derived
	// snapshots without writing anything. Default false.
	DryRun bool

	// Logger receives progress and warning lines. nil → slog.Default().
	Logger *slog.Logger
}

// BackfillReport summarizes one backfill run.
type BackfillReport struct {
	Candidates int // total memories without _epistemic_snapshot
	Backfilled int // successfully stamped with derived snapshot
	Skipped    int // already had snapshot at fetch time (race-safe re-entry)
	Partial    int // no session_id derivable; only creator.agent_id/model stamped
	Errors     int // JSON parse failures + SQL errors during per-memory processing
	BatchesRun int // observability: how many transactions committed
	Previewed  int // dry-run only: how many derived snapshots were printed
}

// ─────────────────────────────────────────────────────────────────────
// Public entry point
// ─────────────────────────────────────────────────────────────────────

// BackfillSnapshots derives _epistemic_snapshot blocks for every memory
// in the database that doesn't already carry one. Returns a report
// summarising the run. Caller is responsible for any UI / logging
// around the report.
//
// dm is the CoreDB interface so the CLI dispatcher (which holds a
// CoreDB, not the concrete *DatabaseManager) can call in directly
// without a type assertion.
//
// Concurrency: the function holds one transaction per batch and
// commits between batches. Long-running writers (mcp-mcp) are not
// blocked for the duration of a large backfill.
//
// Failure modes:
//   - SQL errors during fetch/update: counted in report.Errors,
//     logged, the batch is aborted and processing continues with the
//     next batch (the bad batch is skipped, not retried).
//   - Malformed metadata JSON: that memory is skipped (counted as
//     Partial if it lacks session_id, otherwise Error).
//   - Context cancellation: returns whatever progress was made plus
//     ctx.Err(). Batches already committed stay committed.
func BackfillSnapshots(
	ctx context.Context,
	dm CoreDB,
	opts BackfillOptions,
) (*BackfillReport, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBackfillBatchSize
	}
	if opts.BatchSize > 10000 {
		return nil, fmt.Errorf("backfill: batch size %d exceeds safety ceiling 10000", opts.BatchSize)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	report := &BackfillReport{}

	candidates, err := countMemoriesWithoutSnapshot(dm.SQLDB())
	if err != nil {
		return report, fmt.Errorf("count candidates: %w", err)
	}
	report.Candidates = candidates

	if candidates == 0 {
		opts.Logger.Info("backfill: no candidates found")
		return report, nil
	}

	opts.Logger.Info("backfill: starting",
		"candidates", candidates,
		"batch_size", opts.BatchSize,
		"dry_run", opts.DryRun)

	if opts.DryRun {
		previews, err := previewDerivedSnapshots(dm, 5)
		if err != nil {
			return report, fmt.Errorf("dry-run preview: %w", err)
		}
		report.Previewed = len(previews)
		for i, p := range previews {
			opts.Logger.Info("backfill: dry-run preview",
				"index", i+1,
				"memory_id", p.id,
				"derived_creator", p.creatorSummary,
				"derived_validation", p.validationSummary)
		}
		opts.Logger.Info("backfill: dry-run complete; no writes performed")
		return report, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}

		batch, err := fetchMemoriesWithoutSnapshot(dm.SQLDB(), opts.BatchSize)
		if err != nil {
			return report, fmt.Errorf("fetch batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		batchBackfilled, batchSkipped, batchPartial, batchErrors, err :=
			processBatch(ctx, dm.SQLDB(), dm, batch, opts.Logger)
		if err != nil {
			// processBatch returns error only on transaction-level
			// failure. Per-memory failures are counted in report.Errors
			// and don't abort the batch.
			return report, fmt.Errorf("commit batch: %w", err)
		}

		report.Backfilled += batchBackfilled
		report.Skipped += batchSkipped
		report.Partial += batchPartial
		report.Errors += batchErrors
		report.BatchesRun++

		opts.Logger.Info("backfill: batch committed",
			"batch_size", len(batch),
			"backfilled", batchBackfilled,
			"skipped", batchSkipped,
			"partial", batchPartial,
			"errors", batchErrors,
			"cumulative_backfilled", report.Backfilled,
			"cumulative_total", candidates)

		// WAL yield: let concurrent writers slip in between batches.
		// Cheap (<50ms) and protects the alpha tester's active agent
		// from "save is hanging during migration" reports.
		time.Sleep(BackfillWALYield)

		// Terminate when the batch came back short — no more rows
		// match the filter. (We don't track absolute offset because
		// stamping shrinks the candidate set; instead we trust the
		// short-batch signal as the natural end-of-stream.)
		if len(batch) < opts.BatchSize {
			break
		}
	}

	opts.Logger.Info("backfill: complete",
		"backfilled", report.Backfilled,
		"skipped", report.Skipped,
		"partial", report.Partial,
		"errors", report.Errors,
		"batches", report.BatchesRun)

	return report, nil
}

// ─────────────────────────────────────────────────────────────────────
// SQL helpers
// ─────────────────────────────────────────────────────────────────────

// countMemoriesWithoutSnapshot returns the number of memories that
// don't yet carry an _epistemic_snapshot block in their metadata.
// JSON path: json_extract(metadata, '$._epistemic_snapshot') IS NULL.
func countMemoriesWithoutSnapshot(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM memories
		WHERE deleted_at IS NULL
		  AND json_extract(metadata, '$._epistemic_snapshot') IS NULL
	`).Scan(&n)
	return n, err
}

// memoryRow is a memory that needs a snapshot stamped.
type memoryRow struct {
	ID        string
	SessionID sql.NullString
	Metadata  string // raw JSON; may be null/empty for legacy rows
}

// fetchMemoriesWithoutSnapshot returns up to limit memories that
// don't yet carry a snapshot, ordered by id ASC for stable iteration.
// Excludes rows that already carry a snapshot (defence-in-depth — the
// outer loop also tracks this, but the predicate keeps the result set
// honest even if a row was stamped between queries).
//
// Note: no OFFSET parameter. We previously paginated via OFFSET, but
// that approach breaks when stamping within a batch changes the
// filtered result set for the next query (OFFSET applies to the
// FILTERED result, so the loop terminates prematurely). Re-querying
// without OFFSET and terminating on a short batch is correct and
// simple; the cost is a full scan per batch, which is acceptable for
// a one-shot migration script.
func fetchMemoriesWithoutSnapshot(db *sql.DB, limit int) ([]memoryRow, error) {
	rows, err := db.Query(`
		SELECT id, session_id, COALESCE(metadata, '{}') AS metadata
		FROM memories
		WHERE deleted_at IS NULL
		  AND json_extract(metadata, '$._epistemic_snapshot') IS NULL
		ORDER BY id ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []memoryRow
	for rows.Next() {
		var m memoryRow
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Metadata); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// processBatch runs one transaction over the given batch of memories,
// stamping each one with a derived snapshot. Per-memory errors are
// counted and the batch continues; transaction-level errors are
// returned so the caller can abort the whole run.
//
// db is the SQL connection used for the transaction (memory UPDATE).
// evidenceDB is the CoreDB used for evidence lookups; queries go
// through a separate connection, which is acceptable because the
// evidence count is informational (status + trigger_evidence_id) and
// a transient inconsistency at backfill time doesn't affect
// downstream queries — the snapshot itself is point-in-time anyway.
func processBatch(
	ctx context.Context,
	db *sql.DB,
	evidenceDB CoreDB,
	batch []memoryRow,
	logger *slog.Logger,
) (backfilled, skipped, partial, errorCount int, err error) {
	// Open a single transaction for the whole batch. If the
	// transaction fails to begin, abort the whole batch.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, mem := range batch {
		if err := ctx.Err(); err != nil {
			return 0, 0, 0, 0, err
		}

		// Re-check under the transaction in case a concurrent save
		// stamped this row between our fetch and our update.
		var stillMissing bool
		if err := tx.QueryRowContext(ctx, `
			SELECT json_extract(metadata, '$._epistemic_snapshot') IS NULL
			FROM memories WHERE id = ?
		`, mem.ID).Scan(&stillMissing); err != nil {
			logger.Warn("backfill: re-check failed", "id", mem.ID, "error", err)
			errorCount++
			continue
		}
		if !stillMissing {
			skipped++
			continue
		}

		// Parse existing metadata (may be "{}" for rows with no metadata).
		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(mem.Metadata), &meta); err != nil {
			logger.Warn("backfill: metadata JSON unparseable", "id", mem.ID, "error", err)
			errorCount++
			continue
		}

		// Derive the partial snapshot.
		snap := deriveSnapshot(mem, meta, evidenceDB)

		// Stamp it.
		snapMap, err := structToMap(snap)
		if err != nil {
			logger.Warn("backfill: snapshot marshal failed", "id", mem.ID, "error", err)
			errorCount++
			continue
		}
		meta["_epistemic_snapshot"] = snapMap

		newMeta, err := json.Marshal(meta)
		if err != nil {
			logger.Warn("backfill: re-marshal failed", "id", mem.ID, "error", err)
			errorCount++
			continue
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE memories SET metadata = ?, updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ?
		`, string(newMeta), mem.ID); err != nil {
			logger.Warn("backfill: update failed", "id", mem.ID, "error", err)
			errorCount++
			continue
		}

		backfilled++
		if !mem.SessionID.Valid || mem.SessionID.String == "" {
			partial++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return backfilled, skipped, partial, errorCount, nil
}

// ─────────────────────────────────────────────────────────────────────
// Derivation
// ─────────────────────────────────────────────────────────────────────

// deriveSnapshot builds a partial EpistemicSnapshot from the substrate
// state available for a pre-resolver memory.
//
// Creator fields are pulled from metadata.provenance.{agent,model} (the
// legacy block already stamped by SaveMemoryWithContext's
// withActiveContextMeta) plus memories.session_id. If the legacy
// provenance block is missing (raw insert via AddMemory etc.), agent
// and model fall back to empty strings — not an error, just less rich
// provenance.
//
// Validation fields are pulled from the evidence table; for memories
// with zero evidence rows, status="unvalidated" and counts=0.
//
// Execution / provenance / context are intentionally NOT synthesised —
// those are unrecoverable for memories written before the resolver
// existed. A backfilled row carries a partial snapshot, never a
// fabricated one.
func deriveSnapshot(mem memoryRow, meta map[string]interface{}, dm CoreDB) *EpistemicSnapshot {
	// Creator from legacy provenance block (best-effort).
	var agentID, model string
	if prov, ok := meta["provenance"].(map[string]interface{}); ok {
		if a, ok := prov["agent"].(string); ok {
			agentID = a
		}
		if m, ok := prov["model"].(string); ok {
			model = m
		}
	}

	creator := &CreatorContext{
		AgentID:   agentID,
		SessionID: mem.SessionID.String,
		Model:     model,
	}

	// Validation from evidence table.
	validation := deriveValidation(mem.ID, dm)

	return &EpistemicSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Creator:       creator,
		Validation:    validation,
	}
}

// deriveValidation queries the evidence table for an artifact and
// produces a ValidationState block. Mirrors computeValidationState in
// snapshot.go but operates through the CoreDB interface so the CLI
// dispatcher can call it without a type assertion.
func deriveValidation(memoryID string, dm CoreDB) *ValidationState {
	state := &ValidationState{
		Status:        "unvalidated",
		EvidenceCount: 0,
	}

	raw, err := dm.ListEvidence(memoryID, "memory")
	if err != nil {
		// Don't fail the backfill on a query error — just stamp
		// unvalidated and let the operator investigate.
		slog.Warn("backfill: evidence query failed",
			"memory_id", memoryID, "error", err)
		return state
	}

	// dm.ListEvidence returns a map wrapping a []map[string]interface{}.
	// The map shape is: {"evidence": [...], "count": N} (see
	// evidence_tools.go ListEvidence for the contract). Pull the
	// inner slice and shape rows into the fields we need.
	rowsAny, _ := raw["evidence"].([]map[string]interface{})
	state.EvidenceCount = len(rowsAny)
	if len(rowsAny) == 0 {
		return state
	}

	// dm.ListEvidence orders by created_at DESC (newest first), so
	// index 0 is the most recent row — no need to walk backwards.
	for _, row := range rowsAny {
		t, _ := row["type"].(string)
		if t == "challenge" {
			state.Status = "contradicted"
			state.TriggerEvidenceID, _ = row["id"].(string)
			if ts, ok := row["created_at"].(float64); ok {
				state.LastValidatedAt = time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
			} else if ts, ok := row["created_at"].(int64); ok {
				state.LastValidatedAt = time.Unix(ts, 0).UTC().Format(time.RFC3339)
			}
			return state
		}
	}
	for _, row := range rowsAny {
		strength, _ := row["strength"].(float64)
		if strength > 0 {
			state.Status = "corroborated"
			state.TriggerEvidenceID, _ = row["id"].(string)
			if ts, ok := row["created_at"].(float64); ok {
				state.LastValidatedAt = time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
			} else if ts, ok := row["created_at"].(int64); ok {
				state.LastValidatedAt = time.Unix(ts, 0).UTC().Format(time.RFC3339)
			}
			return state
		}
	}
	// Evidence exists but no positive signal: stays unvalidated.
	return state
}

// ─────────────────────────────────────────────────────────────────────
// Dry-run preview
// ─────────────────────────────────────────────────────────────────────

// previewRow is a one-line summary used for dry-run output.
type previewRow struct {
	id                string
	creatorSummary    string
	validationSummary string
}

// previewDerivedSnapshots derives snapshots for up to `limit` memories
// without writing anything. Used by --dry-run to give the operator
// a feel for what the backfill will produce.
func previewDerivedSnapshots(dm CoreDB, limit int) ([]previewRow, error) {
	batch, err := fetchMemoriesWithoutSnapshot(dm.SQLDB(), limit)
	if err != nil {
		return nil, err
	}

	out := make([]previewRow, 0, len(batch))
	for _, mem := range batch {
		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(mem.Metadata), &meta); err != nil {
			out = append(out, previewRow{
				id:                mem.ID,
				creatorSummary:    "(metadata unparseable)",
				validationSummary: "(skipped)",
			})
			continue
		}
		snap := deriveSnapshot(mem, meta, dm)

		var cs string
		if snap.Creator != nil {
			cs = fmt.Sprintf("agent_id=%q session_id=%q model=%q",
				snap.Creator.AgentID, snap.Creator.SessionID, snap.Creator.Model)
		}
		var vs string
		if snap.Validation != nil {
			vs = fmt.Sprintf("status=%q evidence_count=%d trigger=%q",
				snap.Validation.Status, snap.Validation.EvidenceCount, snap.Validation.TriggerEvidenceID)
		}

		out = append(out, previewRow{id: mem.ID, creatorSummary: cs, validationSummary: vs})
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────
// Public helper for the CLI
// ─────────────────────────────────────────────────────────────────────

// BackfillSummarize returns a one-line summary suitable for stdout.
func (r *BackfillReport) BackfillSummarize() string {
	return fmt.Sprintf(
		"backfilled=%d skipped=%d partial=%d errors=%d batches=%d",
		r.Backfilled, r.Skipped, r.Partial, r.Errors, r.BatchesRun,
	)
}

// Sentinel errors for testing and CLI error mapping.
var (
	ErrBackfillNoCandidates = errors.New("backfill: no candidates")
	ErrBackfillAborted      = errors.New("backfill: aborted")
)