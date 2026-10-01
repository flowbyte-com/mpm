// compact_requeue.go — Phase 6, R1–R5 of the compact-refusal lifecycle.
//
// Requeue is the inverse of deferral, and it is the only way a
// deferred row becomes selectable again. Everything about it follows
// from that: it is an operator action, it is bounded, it is
// guaranteed-auditable, and it must be exactly invertible.
//
// Exactly invertible is the constraint that shapes the code. After a
// requeue, the row must be indistinguishable from a row that had never
// been deferred — except for the four keys deferral added, which must
// be gone. If requeue leaves anything behind, the next operator to
// read the row is reasoning about a state that no longer exists.
//
// What requeue must NOT do is the more dangerous half of the
// symmetry. Clearing compacted_into would make a row whose lesson
// already exists eligible for synthesis again, duplicating a lesson
// and silently undoing real work. The two key families are independent
// and stay that way in both directions.

package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// compactRequeueMaxLimit is the hard ceiling on one requeue call.
//
// The cap is not about protecting the database; requeue is a handful of
// indexed UPDATEs. It is about model-call pressure. Requeueing a large
// backlog in one call hands that whole backlog back to the next drain,
// which will try to synthesize it. An operator who wants 400 rows back
// should make that an explicit sequence of bounded decisions, each
// visible in the audit log, rather than one call that undoes an hour
// of refusals.
const compactRequeueMaxLimit = 1000

// RequeueResult reports what one requeue call did.
type RequeueResult struct {
	// Requeued is the number of rows returned to the selectable pool.
	Requeued int `json:"requeued"`
	// RowIDs are the exact rows requeued, in the order they were
	// selected (oldest first). Present so the operator's action is
	// verifiable after the fact rather than inferred from a count.
	RowIDs []string `json:"row_ids"`
	// Limit is the effective bound after clamping, so a caller that
	// asked for more than the cap can see what it actually got.
	Limit int `json:"limit"`
	// RemainingDeferred is the deferred count after the call. An
	// operator requeuing a 200-row backlog in 50-row steps needs to
	// know how much is left without a second query.
	RemainingDeferred int `json:"remaining_deferred"`
	// AuditID is the id of the forensic trail row. It is always
	// populated on a successful requeue: the trail is written in the
	// same transaction as the mutation, so an empty value is not a
	// reachable state.
	AuditID string `json:"audit_id,omitempty"`
}

// RequeueDeferred returns up to limit deferred rows to the compaction
// pool, oldest first.
//
// R2: the target is bounded and there is no unbounded mode. A
// non-positive limit means "use the default", which is
// compactBatchSize — the same bound a single compaction batch carries,
// so an operator who requeues without naming a limit is doing one
// batch's worth of undoing, not an arbitrary amount. There is no value
// of this argument that means "all"; a limit above the hard cap is
// clamped, and the effective value is reported back so a caller that
// asked for more can see what it got.
//
// R3/R4: the update removes exactly the four compaction_deferred_*
// keys and touches nothing else — not compacted_into, not content, not
// created_at. Unrelated metadata keys on the same row survive, because
// requeue removes what deferral wrote, not the whole document.
//
// R1: nothing else calls this. It is not reachable from the drain, not
// implied by a wake, not set by any scheduler tick, and not defaulted
// by a bare compact call. The absence of those call sites is the
// guarantee; there is no flag guarding against them.
func (dm *DatabaseManager) RequeueDeferred(ctx context.Context, limit int) (*RequeueResult, error) {
	if limit <= 0 {
		limit = compactBatchSize
	}
	if limit > compactRequeueMaxLimit {
		limit = compactRequeueMaxLimit
	}

	ids, err := dm.selectDeferredForRequeue(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := &RequeueResult{Limit: limit}
	if len(ids) == 0 {
		return result, nil
	}
	result.RowIDs = ids
	result.Requeued = len(ids)

	if err := dm.clearDeferralMetadata(ctx, result, ids); err != nil {
		return nil, err
	}

	remaining, _, err := dm.DeferralCounts(ctx)
	if err != nil {
		return nil, err
	}
	result.RemainingDeferred = remaining

	return result, nil
}

// selectDeferredForRequeue picks the oldest deferred rows, bounded by
// limit, in its own read so the mutation below is a pure
// primary-key-bounded UPDATE with no ordering concern.
func (dm *DatabaseManager) selectDeferredForRequeue(ctx context.Context, limit int) ([]string, error) {
	rows, err := dm.db.QueryContext(ctx, `
		SELECT id
		FROM memories
		WHERE `+memoryHasDeferral+`
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("requeue_deferred: select: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("requeue_deferred: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("requeue_deferred: rows: %w", err)
	}
	return ids, nil
}

// clearDeferralMetadata removes exactly the four deferral keys from
// every id, atomically.
//
// json_remove is the whole operation. There is no read-modify-write of
// the metadata document anywhere in this path: doing that in Go would
// mean a lost update whenever a deferral landed between the read and
// the write, which would silently drop a refusal annotation. The
// database does the structural edit.
//
// The audit row is written in THIS transaction, after the updates and
// before the commit. R5 promises an audit row for every requeue, and a
// promise that can be broken by a disk-full or a closed handle is not
// a promise. The counter-argument — that a trail problem should not
// block the operator's decision — is real but weaker here than it is
// for a read: the mutation is trivially repeatable, so refusing to
// commit leaves the operator able to try again immediately, whereas
// committing un-audited leaves an override with no record that it ever
// happened. That is the failure PurgeWork already refuses to accept,
// and it is why its audit row lives inside the same transaction
// (internal/core/work_purge.go).
//
// The tiebreaker is id ASC alongside created_at ASC. created_at is not
// unique in this schema, and without a deterministic tiebreak two calls
// with the same limit over the same backlog could select overlapping
// but differently-ordered sets, making the operator's audit trail
// harder to read than it needs to be.
func (dm *DatabaseManager) clearDeferralMetadata(ctx context.Context, result *RequeueResult, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	tx, err := dm.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("requeue_deferred: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE memories
		SET metadata = json_remove(
		        COALESCE(metadata, '{}'),
		        '$.`+metaDeferralAt+`',
		        '$.`+metaDeferralReason+`',
		        '$.`+metaDeferralBatch+`',
		        '$.`+metaDeferralSample+`'
		    ),
		    updated_at = ?
		WHERE id = ? AND `+memoryHasDeferral)
	if err != nil {
		return fmt.Errorf("requeue_deferred: prepare: %w", err)
	}
	defer stmt.Close()

	nowUnix := time.Now().Unix()
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, nowUnix, id)
		if err != nil {
			return fmt.Errorf("requeue_deferred: update %s: %w", id, err)
		}
		// Write verification (CLAUDE.md §4). A row that did not
		// actually update is still deferred: it would not resurface
		// for the operator, it would just silently not have been
		// requeued, and the reported count would be a lie.
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("requeue_deferred: rows affected %s: %w", id, err)
		}
		if n != 1 {
			return fmt.Errorf("requeue_deferred: %s: expected 1 row updated, got %d — the row was not deferred", id, n)
		}
	}

	auditID, err := dm.writeCompactRequeueAudit(ctx, tx, result)
	if err != nil {
		return err
	}
	result.AuditID = auditID

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("requeue_deferred: commit: %w", err)
	}
	return nil
}

// writeCompactRequeueAudit writes the forensic trail row for a
// requeue, inside the caller's transaction.
//
// R5 asks for two things: a record that a requeue happened, and the
// bounded target that was actually cleared. These arrive by two
// different routes, and it is worth being precise about which is which.
//
//	"a requeue happened" — the dispatch hook (cmd/mpm/audit_hook.go)
//	writes a tool_invocations row, but ONLY for calls that arrive via
//	`mpm call` or the MCP server. Requeue is deliberately not reachable
//	from either: it is CLI-only, and the CLI router
//	(cmd/mpm/router.go → handleCompact) calls this method directly
//	without passing through recordToolInvocation. So on the one path
//	requeue actually takes, no tool_invocations row exists. This row
//	is therefore not a supplement to one — it is the only record that
//	an override happened at all.
//
//	"the target cleared" — not derivable from the mutation itself. The
//	row ids and the count are what an operator needs to reconstruct
//	why a refused batch was offered again, so they are carried here.
//
// AuditInfo, not a warn/error level: an operator overriding the model's
// judgement is a deliberate state mutation, not an anomaly. It goes in
// the same stream as the skill-workshop outcomes — a record for a human
// reading later, deliberately excluded from cluster detection and from
// wake context, because a routine requeue must never wake an agent.
//
// An insert failure is returned, not swallowed. The caller is inside
// the requeue transaction, so returning an error rolls the mutation
// back; a committed requeue that left no record would be exactly the
// unaudited override R5 exists to prevent.
func (dm *DatabaseManager) writeCompactRequeueAudit(ctx context.Context, tx *sql.Tx, result *RequeueResult) (string, error) {
	if dm == nil || result == nil {
		return "", fmt.Errorf("requeue_deferred: audit: nil database manager or result")
	}
	auditID := GenerateID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO system_audit_log (id, level, component, message, stack_trace, context, created_at)
		 VALUES (?, ?, 'compact', ?, ?, ?, CAST(strftime('%s','now') AS INTEGER))`,
		auditID, string(AuditInfo),
		fmt.Sprintf("requeued %d deferred rows (limit %d)", result.Requeued, result.Limit),
		sql.NullString{},
		auditContextJSON(map[string]any{
			"requeued": result.Requeued,
			"limit":    result.Limit,
			"row_ids":  result.RowIDs,
		}),
	); err != nil {
		return "", fmt.Errorf("requeue_deferred: write audit row: %w", err)
	}
	return auditID, nil
}

// auditContextJSON marshals an audit context blob, degrading to an
// absent context rather than failing. An audit row with no context is
// still a usable record of the headline; a panic while building one is
// not acceptable on this path.
func auditContextJSON(ctx AuditContext) sql.NullString {
	if len(ctx) == 0 {
		return sql.NullString{}
	}
	b, err := json.Marshal(ctx)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}
