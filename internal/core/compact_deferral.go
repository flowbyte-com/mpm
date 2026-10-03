// compact_deferral.go — the deferred state: how a refused batch is
// recorded, and how it is undone.
//
// A refusal is a considered judgement by the model, not a failure. The
// rows it was offered must stop being offered — otherwise the next
// drain reselects the identical rows, makes the identical call, gets
// the identical refusal, and loops. See
// docs/archive/2026-09-30-compact-refusal-lifecycle.md §3.
//
// The state lives in the existing `metadata` JSON column, alongside
// `compacted_into`, for the reason F7 gives: compacted_into may only
// be written when a lesson actually exists, and a refusal creates no
// lesson. A separate key family is required regardless of storage
// choice, and a metadata key means no schema migration for the rows
// themselves — only for the pressure view that counts them.

package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// The four deferral keys. Exactly four, exactly these, and requeue
// removes exactly these — see R3/R4 in the design. Adding a fifth
// without removing it from RequeueDeferred would make requeue
// non-invertible.
const (
	metaDeferralAt     = "compaction_deferred_at"
	metaDeferralReason = "compaction_deferred_reason"
	metaDeferralBatch  = "compaction_deferred_batch"
	metaDeferralSample = "compaction_deferred_sample"
)

// DeferralReasonRefusal is the only reason currently written. It is a
// named constant rather than a bare string because it is stored
// operator-visible and an operator filtering on it must be able to
// grep for exactly this token.
const DeferralReasonRefusal = "refusal_sentinel"

// compactDeferralSampleMax bounds the stored model response. The
// sample exists so an operator can see WHY a batch was refused
// without re-running the call; it is not the record of what happened,
// and copying an unbounded response into every row's metadata would
// multiply the cost of the annotation by the batch size for no
// diagnostic gain.
const compactDeferralSampleMax = 512

// EpistemicPressureCounts is the read model of epistemic_pressure_v.
//
// The four fields are not independent: RawCount is by construction
// ActionablePending + DeferredCount. They are read together so a
// caller can see a deferred backlog (raw 120, actionable 0) and
// distinguish it from an actionable one (raw 120, actionable 120) —
// the difference between "there is work" and "there is a backlog the
// model has already declined".
type EpistemicPressureCounts struct {
	RawCount          int `json:"raw_count"`
	LessonCount       int `json:"lesson_count"`
	DeferredCount     int `json:"deferred_count"`
	ActionablePending int `json:"actionable_pending"`
}

// PressureCounts reads the current pressure view.
func (dm *DatabaseManager) PressureCounts(ctx context.Context) (EpistemicPressureCounts, error) {
	var c EpistemicPressureCounts
	err := dm.db.QueryRowContext(ctx, `
		SELECT raw_count, lesson_count, deferred_count, actionable_pending
		FROM epistemic_pressure_v`).
		Scan(&c.RawCount, &c.LessonCount, &c.DeferredCount, &c.ActionablePending)
	if err != nil {
		return EpistemicPressureCounts{}, fmt.Errorf("pressure_counts: %w", err)
	}
	return c, nil
}

// newDeferralBatchID mints the stable id that groups one deferral.
//
// "cdb-" prefix so a batch id is visually distinguishable from a
// lesson id ("les-") in an operator's query output — an operator
// reading compaction_deferred_batch should not have to know the prefix
// conventions to tell a batch from a lesson.
func newDeferralBatchID(now time.Time, counter int) string {
	return fmt.Sprintf("cdb-%d-%03d", now.UTC().Unix(), counter)
}

// DeferralAnnotation is the set of values written to one deferral
// group. Every row in a single deferral receives identical values for
// all four fields, which is what makes "group these rows by batch" an
// exact query rather than a near-miss.
type DeferralAnnotation struct {
	At     time.Time
	Reason string
	Batch  string
	Sample string
}

// DeferBatch annotates every id in the batch as deferred, atomically.
//
// All-or-nothing is not a nicety here. compaction_deferred_batch is
// what makes a deferral groupable and inspectable; a partial write
// would leave 30 of 50 marked, and the unmarked 20 would be reselected
// as a fresh batch, offered again, and very probably refused again —
// recreating the loop this design removes, with a corrupt annotation
// on top of it to diagnose.
//
// The timestamp and the batch id are computed ONCE, in the caller, and
// threaded through. Computing them per-row would produce values that
// are equal in practice but not identical in the database, which is
// enough to make grouping-by-batch a query that nearly matches.
func (dm *DatabaseManager) DeferBatch(ctx context.Context, ids []string, ann DeferralAnnotation) error {
	if len(ids) == 0 {
		return nil
	}
	if ann.At.IsZero() {
		return fmt.Errorf("defer_batch: annotation timestamp must be computed by the caller")
	}
	if ann.Batch == "" {
		return fmt.Errorf("defer_batch: annotation batch id must be non-empty")
	}
	if ann.Reason == "" {
		return fmt.Errorf("defer_batch: annotation reason must be non-empty")
	}

	sample := ann.Sample
	if len(sample) > compactDeferralSampleMax {
		sample = sample[:compactDeferralSampleMax]
	}
	at := ann.At.UTC().Format(time.RFC3339)

	tx, err := dm.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("defer_batch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// One prepared statement, looped inside the transaction. This is
	// not the "loop of independent bare-db writes" the design forbids:
	// every statement runs on tx, so a failure at row 30 rolls back
	// rows 1–29 with it.
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE memories
		SET metadata = json_set(
		        COALESCE(metadata, '{}'),
		        '$.`+metaDeferralAt+`', ?,
		        '$.`+metaDeferralReason+`', ?,
		        '$.`+metaDeferralBatch+`', ?,
		        '$.`+metaDeferralSample+`', ?
		    ),
		    updated_at = ?
		WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("defer_batch: prepare: %w", err)
	}
	defer stmt.Close()

	nowUnix := time.Now().Unix()
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, at, ann.Reason, ann.Batch, sample, nowUnix, id)
		if err != nil {
			return fmt.Errorf("defer_batch: update %s: %w", id, err)
		}
		// Write verification: db.Exec() == nil is not sufficient
		// evidence that the intended state persisted (CLAUDE.md §4).
		// A row that silently failed to update would be reselected and
		// re-offered, which is the loop.
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("defer_batch: rows affected %s: %w", id, err)
		}
		if n != 1 {
			return fmt.Errorf("defer_batch: %s: expected 1 row updated, got %d", id, n)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("defer_batch: commit: %w", err)
	}
	return nil
}

// deferRawBatch is the refusal path's entry point: mint one annotation
// for the whole batch and apply it.
//
// The batch id combines a millisecond timestamp with a per-process
// counter so two deferrals in the same millisecond still get distinct,
// stable group ids.
func (dm *DatabaseManager) deferRawBatch(ctx context.Context, ids []string, reason, sample string) (DeferralAnnotation, error) {
	now := time.Now().UTC()
	ann := DeferralAnnotation{
		At:     now,
		Reason: reason,
		Batch:  newDeferralBatchID(now, atomicDeferralCounter()),
		Sample: sample,
	}
	if err := dm.DeferBatch(ctx, ids, ann); err != nil {
		return DeferralAnnotation{}, err
	}
	return ann, nil
}

// memoryHasDeferral is the annotation test on its own, with no
// dependence on U. Both the operator's inspection read and the requeue
// target selection use it.
//
// The null/empty guard is load-bearing, not defensive decoration.
// json_extract(”) raises "malformed JSON" rather than returning NULL
// (see migration_epistemic_pressure_deferral.go for the full note and
// the verification), so an unguarded conjunct turns every
// empty-metadata row in the table into a query error. The existing
// memoryIsUncompacted predicate escapes this only because its json_extract
// sits behind an OR arm that short-circuits; a standalone conjunct has
// no such luck.
const memoryHasDeferral = `(metadata IS NOT NULL AND metadata != ''
     AND json_extract(metadata, '$.` + metaDeferralAt + `') IS NOT NULL)`

// DeferralCounts reads how many rows are deferred and how many are
// actionable, without the other two view columns.
func (dm *DatabaseManager) DeferralCounts(ctx context.Context) (deferred, actionable int, err error) {
	err = dm.db.QueryRowContext(ctx, `
		SELECT deferred_count, actionable_pending
		FROM epistemic_pressure_v`).Scan(&deferred, &actionable)
	if err != nil {
		return 0, 0, fmt.Errorf("deferral_counts: %w", err)
	}
	return deferred, actionable, nil
}

// ListDeferred returns up to limit deferred rows, oldest first, with
// their annotations. It is the inspectability half of R5: an operator
// can see WHAT is deferred and WHY before deciding whether to requeue.
//
// Read-only. It never mutates, never marks, and never re-batches.
func (dm *DatabaseManager) ListDeferred(ctx context.Context, limit int) ([]DeferredRow, error) {
	if limit <= 0 {
		limit = compactBatchSize
	}
	rows, err := dm.db.QueryContext(ctx, `
		SELECT id, content, created_at, metadata
		FROM memories
		WHERE collection = 'memories'
		  AND deleted_at IS NULL
		  AND `+memoryHasDeferral+`
		ORDER BY created_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list_deferred: %w", err)
	}
	defer rows.Close()

	var out []DeferredRow
	for rows.Next() {
		var r DeferredRow
		var raw sql.NullString
		if err := rows.Scan(&r.ID, &r.Content, &r.CreatedAt, &raw); err != nil {
			return nil, fmt.Errorf("list_deferred scan: %w", err)
		}
		if raw.Valid && raw.String != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(raw.String), &m); err == nil {
				r.DeferredAt, _ = m[metaDeferralAt].(string)
				r.Reason, _ = m[metaDeferralReason].(string)
				r.Batch, _ = m[metaDeferralBatch].(string)
				r.Sample, _ = m[metaDeferralSample].(string)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list_deferred rows: %w", err)
	}
	return out, nil
}

// DeferredRow is one deferral as an operator sees it.
type DeferredRow struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	CreatedAt  string `json:"created_at"`
	DeferredAt string `json:"deferred_at"`
	Reason     string `json:"reason"`
	Batch      string `json:"batch"`
	Sample     string `json:"sample,omitempty"`
}
