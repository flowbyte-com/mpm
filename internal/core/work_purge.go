// work_purge.go — CLI-only logical purge of a work item.
//
// Design: docs/archive/2026-09-30-work-archive-and-purge.md §5, §6.
//
// WHAT PURGE IS: logical removal from the active substrate. After a
// successful forced purge there is no `works` row, no `work_events`
// row, no work-owned evidence or provenance row, and no read surface
// that can retrieve the artifact.
//
// WHAT PURGE IS NOT: erasure. Purge makes no guarantee about bytes in
// the SQLite file, the WAL, a rollback journal, freelist pages, any
// pre-existing backup, or any filesystem snapshot. MPM has no
// secure-erasure capability in v1. Everything this file does is
// therefore a `DELETE` plus an audit row — no `secure_delete` pragma,
// no `VACUUM`, no `wal_checkpoint`, no implicit backup, and no scrubbed
// tombstone row. Those absences are load-bearing and are pinned by
// tests, not by good intentions: each one would silently change the
// lock profile, the write-amplification profile, or the erasure
// semantics the operator was told about.
//
// NOT CASCADE. There is no `--cascade` flag and no path rewrites
// `memories.dependencies`, a handoff, or any other substrate record.
// Memory shred enqueues cascade intents because retention is routine
// and the cascade contract is well-worn; purge is an exceptional
// administrative act where silently mutating unrelated memories is the
// wrong default. An inbound structural reference is a REFUSAL, not a
// trigger.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ============================================================================
// Reason codes
// ============================================================================

// ValidWorkPurgeReasonCodes is the closed enum for --reason-code.
//
// There is deliberately no `privacy` member: v1 purge is logical
// removal, not forensic or privacy-grade erasure, so a code named
// `privacy` would mislead even with a disclaimer attached (§5.2).
// `administrative` covers operator housekeeping.
var ValidWorkPurgeReasonCodes = []string{
	"test_debris",
	"accidental",
	"corrupted",
	"migration_cleanup",
	"administrative",
	"other",
}

// ErrWorkPurgeInvalidReasonCode is returned when --reason-code is
// missing, empty, or not a member of ValidWorkPurgeReasonCodes.
var ErrWorkPurgeInvalidReasonCode = fmt.Errorf("purge: --reason-code must be one of %v", ValidWorkPurgeReasonCodes)

// ErrWorkPurgeNotFound is returned when no works row carries the id.
var ErrWorkPurgeNotFound = fmt.Errorf("purge: no work item with that id")

// ErrWorkPurgeReferenced is returned when an inbound structural
// reference exists. The referrers are enumerated on the error value so
// the caller can print them; the underlying records are never modified.
var ErrWorkPurgeReferenced = fmt.Errorf("purge: work item is referenced by other records")

// WorkPurgeReasonCodeIsValid reports whether code is a member of the
// closed enum. Exported so the CLI validates with the same set the
// write path enforces.
func WorkPurgeReasonCodeIsValid(code string) bool {
	for _, c := range ValidWorkPurgeReasonCodes {
		if c == code {
			return true
		}
	}
	return false
}

// ============================================================================
// Request / result types
// ============================================================================

// WorkPurgeReferrer is one inbound structural reference that blocks a
// purge. Exactly one of ID / Detail is populated depending on Kind.
type WorkPurgeReferrer struct {
	// Kind is the referring table, one of:
	//   memory_dependency | epistemic_provenance | evidence | cascade_outbox
	Kind string `json:"kind"`
	// ID is the referring row's identifier, when the referring table
	// has one.
	ID string `json:"id,omitempty"`
	// Detail names the referrer when there is no single id — e.g. a
	// cascade outbox row is identified by its intent id and the
	// downstream artifact it was already enqueued against.
	Detail string `json:"detail,omitempty"`
}

// WorkPurgeCounts is the per-table row count of what a purge removes.
// Serialized into work_purge_audit.counts.
//
// `works` is intentionally absent: it is always exactly 1 or the purge
// did not proceed (the preflight resolves the row first), so a field
// that can only ever hold 0 or 1 is noise in a permanent record.
type WorkPurgeCounts struct {
	WorkEvents          int `json:"work_events"`
	Evidence            int `json:"evidence"`
	ArtifactProvenance  int `json:"artifact_provenance"`
	EpistemicProvenance int `json:"epistemic_provenance"`
}

// Total is the number of rows the delete set removes.
func (c WorkPurgeCounts) Total() int {
	return c.WorkEvents + c.Evidence + c.ArtifactProvenance + c.EpistemicProvenance
}

// WorkPurgeRequest is the operator's input. Note the absence of any
// cascade and any erasure flag: neither exists in v1.
type WorkPurgeRequest struct {
	// WorkID must be a complete id. There is no prefix matching in v1 —
	// a purge is irreversible, and "I typed a prefix and it matched
	// something" is not a reasonable failure mode for that.
	WorkID string
	// ReasonCode is a member of ValidWorkPurgeReasonCodes. Required.
	ReasonCode string
	// Note is optional operator-authored free text. It is independent
	// input: it is never derived from the work item, and it permanently
	// survives the purge. A note is copied by the operator into a
	// permanent record — purge does not remove information you
	// manually place in it.
	Note string
	// Operator is recorded verbatim for the audit trail.
	Operator string
	// Force gates the write. Without it the call is a dry run and
	// nothing is written.
	Force bool
}

// WorkPurgeReport is the result of both a dry run and a forced purge,
// so the CLI renders both through the same path.
type WorkPurgeReport struct {
	WorkID   string
	WorkItem bool // whether a works row existed for the id
	// Title is the work item's title, for operator recognition in the
	// dry-run report. It is NEVER written to work_purge_audit — see
	// §5.6; it is present here only so the human confirming a purge can
	// see what they are about to remove.
	Title string
	// Status is the work item's status at purge time. Same constraint.
	Status string
	// ReasonCode is the operator-supplied justification, echoed so the
	// CLI can report what it validated. It is operator input, not
	// artifact data.
	ReasonCode string
	// DryRun is true when nothing was written.
	DryRun bool
	// Counts is what the delete set removes (dry run) or removed (forced).
	Counts WorkPurgeCounts
	// Referrers is the inbound structural reference set. Non-empty
	// means the purge refuses.
	Referrers []WorkPurgeReferrer
	// ResidualExposure names the known locations where the purged
	// content may survive (§5.7). Always populated on a dry run; the
	// dry run MUST disclose these, because purge's own guarantee stops
	// at the SQLite logical layer.
	ResidualExposure []string
	// AuditID is set only on a forced purge.
	AuditID string
	// PurgedAt is set only on a forced purge.
	PurgedAt int64
}

// PurgeResidualExposure is the mandatory dry-run disclosure (§5.7).
//
// These are real, verified locations, not boilerplate. In particular
// backups/critic-pre/ is written automatically and rotates at seven:
// ANY snapshot taken before a purge retains the purged content until
// rotation. Purge deleting rows from the live database does not touch
// any of them, and an operator who believes otherwise is worse off
// than one who was never offered the command.
var PurgeResidualExposure = []string{
	"the SQLite WAL and rollback journal (freelist pages are not overwritten)",
	"backups/critic-pre/ — automatic scheduler snapshots, rotating at 7; any snapshot taken before this purge retains the content until it rotates out",
	"user-created `mpm backup` dumps",
	"filesystem-level snapshots (Timeshift, btrfs/ZFS, LVM) — outside MPM's reach",
	"content already transcribed into a handoff summary, a memory body, or your own --note",
}

// ============================================================================
// Preflight
// ============================================================================

// PreflightWorkPurge resolves the work item, discovers inbound
// structural references, and counts the delete set WITHOUT writing
// anything. This is what the dry run calls.
//
// It is also the forced path's first phase, run inside the purge
// transaction before any DELETE — following ShredMemoryWithCascade
// (memory_tools.go:672), discovery must happen while the row context
// still exists.
//
// STRUCTURAL ONLY. The id appearing in a handoff summary, a memory's
// free-text content, a work note, or a wake payload is NOT a
// reference. A 16-hex id occurring incidentally in prose is noise;
// treating it as a reference would make purge refuse for no reason.
func (dm *DatabaseManager) PreflightWorkPurge(node DBNode, workID string) (*WorkPurgeReport, error) {
	if workID == "" {
		return nil, fmt.Errorf("purge: work_id is required")
	}
	report := &WorkPurgeReport{
		WorkID:           workID,
		Referrers:        []WorkPurgeReferrer{},
		ResidualExposure: PurgeResidualExposure,
		Counts:           WorkPurgeCounts{},
	}

	// Resolve the target. A missing row is an error, not an empty
	// plan: "nothing would be deleted" must never be reported as a
	// successful dry run for an id that does not exist.
	var title, status string
	err := node.QueryRowTracked(
		`SELECT title, status FROM works WHERE id = ?`, workID,
	).Scan(&title, &status)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrWorkPurgeNotFound, workID)
	}
	if err != nil {
		return nil, fmt.Errorf("purge: resolve work item: %w", err)
	}
	report.WorkItem = true
	report.Title = title
	report.Status = status

	referrers, err := dm.discoverWorkPurgeReferrers(node, workID)
	if err != nil {
		return nil, err
	}
	report.Referrers = referrers

	counts, err := dm.countWorkPurgeDeleteSet(node, workID)
	if err != nil {
		return nil, err
	}
	report.Counts = counts
	return report, nil
}

// discoverWorkPurgeReferrers finds every inbound structural reference.
//
// The four detection paths mirror the table in §6.3 exactly, and each
// resolves identity structurally — JSON membership via json_each, exact
// column equality — never by substring search over prose.
func (dm *DatabaseManager) discoverWorkPurgeReferrers(node DBNode, workID string) ([]WorkPurgeReferrer, error) {
	out := []WorkPurgeReferrer{}

	// Path 1: a memory whose dependencies JSON contains this id.
	// json_each gives real array membership: a memory that merely
	// mentions the id in prose does not match.
	rows, err := node.QueryTracked(`
		SELECT id, collection
		FROM memories
		WHERE deleted_at IS NULL
		  AND dependencies IS NOT NULL
		  AND dependencies != ''
		  AND EXISTS (
		    SELECT 1 FROM json_each(dependencies)
		    WHERE value = ?
		  )
	`, workID)
	if err != nil {
		return nil, fmt.Errorf("purge: discover memory dependencies: %w", err)
	}
	for rows.Next() {
		var id, collection string
		if err := rows.Scan(&id, &collection); err != nil {
			rows.Close()
			return nil, fmt.Errorf("purge: discover memory dependencies scan: %w", err)
		}
		out = append(out, WorkPurgeReferrer{
			Kind:   "memory_dependency",
			ID:     id,
			Detail: "memory " + collection + " lists this work id in dependencies",
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("purge: discover memory dependencies rows: %w", err)
	}

	// Path 2: epistemic_provenance INBOUND citations only — rows where
	// this work is the cited foundation (source_id) and some other
	// artifact is the citer (downstream_id).
	//
	// Direction matters, and the column names are the opposite of what
	// they look like. Verified against the only production writer,
	// recordSourceCitationsNode (epistemology_tools.go:677): it takes
	// the ids a caller is CITING as `sourceIDs` and the id it just
	// minted as `downstreamID`, writing the former into source_id and
	// the latter into downstream_id. The consumer agrees —
	// discoverPositiveCascadeTargets (cascade_outbox.go:857) resolves a
	// foundation by `WHERE ep.source_id = ?` and reads back
	// ep.downstream_id, so the affected artifacts are the downstream
	// ones. source_id is the thing being relied upon; downstream_id is
	// the thing doing the relying.
	//
	// Therefore: a row with source_id = this work is another artifact
	// relying on the work, and is an inbound referrer. A row with
	// downstream_id = this work is a citation the work itself made, and
	// belongs in the delete set — treating it as a referrer would let a
	// work item's own outbound citation veto its own deletion, leaving
	// the delete in PurgeWork unreachable in practice.
	rows, err = node.QueryTracked(`
		SELECT downstream_id, source_type
		FROM epistemic_provenance
		WHERE source_id = ?
	`, workID)
	if err != nil {
		return nil, fmt.Errorf("purge: discover epistemic provenance: %w", err)
	}
	for rows.Next() {
		var downstreamID, sourceType string
		if err := rows.Scan(&downstreamID, &sourceType); err != nil {
			rows.Close()
			return nil, fmt.Errorf("purge: discover epistemic provenance scan: %w", err)
		}
		out = append(out, WorkPurgeReferrer{
			Kind:   "epistemic_provenance",
			ID:     downstreamID,
			Detail: "cites this work (source_type=" + sourceType + ")",
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("purge: discover epistemic provenance rows: %w", err)
	}

	// Path 3: evidence owned by ANOTHER artifact that cites this work.
	// Work-owned evidence (artifact_type='work') is not a referrer — it
	// is part of the delete set, and refusing on it would make purge
	// impossible for exactly the items that have evidence.
	rows, err = node.QueryTracked(`
		SELECT id, artifact_type
		FROM evidence
		WHERE artifact_id = ? AND artifact_type != 'work'
	`, workID)
	if err != nil {
		return nil, fmt.Errorf("purge: discover evidence referrers: %w", err)
	}
	for rows.Next() {
		var id, artifactType string
		if err := rows.Scan(&id, &artifactType); err != nil {
			rows.Close()
			return nil, fmt.Errorf("purge: discover evidence referrers scan: %w", err)
		}
		out = append(out, WorkPurgeReferrer{
			Kind: "evidence",
			ID:   id,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("purge: discover evidence referrers rows: %w", err)
	}

	// Path 4: a cascade intent already enqueued against this id. The
	// materializer may be holding work derived from it; purging the
	// source first would leave the outbox pointing at nothing.
	rows, err = node.QueryTracked(`
		SELECT id, downstream_artifact_id
		FROM epistemic_cascade_outbox
		WHERE dead_artifact_id = ?
	`, workID)
	if err != nil {
		return nil, fmt.Errorf("purge: discover cascade outbox: %w", err)
	}
	for rows.Next() {
		var id, downstream string
		if err := rows.Scan(&id, &downstream); err != nil {
			rows.Close()
			return nil, fmt.Errorf("purge: discover cascade outbox scan: %w", err)
		}
		out = append(out, WorkPurgeReferrer{
			Kind: "cascade_outbox",
			ID:   id,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("purge: discover cascade outbox rows: %w", err)
	}

	return out, nil
}

// countWorkPurgeDeleteSet counts the rows §5.5 removes. Counting is
// separate from deleting so the dry run can report a real number rather
// than a table list.
func (dm *DatabaseManager) countWorkPurgeDeleteSet(node DBNode, workID string) (WorkPurgeCounts, error) {
	var c WorkPurgeCounts
	counts := []struct {
		dest *int
		sql  string
	}{
		{&c.WorkEvents, `SELECT COUNT(*) FROM work_events WHERE work_id = ?`},
		{&c.Evidence, `SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'work'`},
		{&c.ArtifactProvenance, `SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'`},
		// downstream_id is deleted, not source_id. downstream_id is the
		// CITING artifact, so these are the citations this work made
		// itself and they belong to it. A row with source_id = this work
		// is an inbound citation another artifact made, and preflight
		// has already refused on it, so none can exist here.
		{&c.EpistemicProvenance, `SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`},
	}
	for _, cc := range counts {
		if err := node.QueryRowTracked(cc.sql, workID).Scan(cc.dest); err != nil {
			return WorkPurgeCounts{}, fmt.Errorf("purge: count delete set: %w", err)
		}
	}
	return c, nil
}

// ============================================================================
// Purge
// ============================================================================

// PurgeWork removes a work item from the substrate.
//
// Dry run is the default: without req.Force nothing is written, and the
// returned report describes exactly what a forced run would remove plus
// the inbound references that would block it.
//
// The forced path is one transaction: preflight, delete set, and audit
// insert all commit together or not at all. A failure after the first
// DELETE leaves zero partial deletions — the operator never sees a work
// item with no ledger, or an audit row claiming a purge that did not
// happen.
//
// NOT on the CoreDB interface, deliberately. Purge is CLI-only (§5.3):
// it is absent from `mpm_work` and from `mpm_system` because both are
// agent-reachable, and an agent-reachable irreversible delete is a
// different tool with a different risk profile than an operator
// command. Keeping the method off CoreDB makes that structural rather
// than a matter of remembering not to wire it up — the MCP handlers
// only ever hold a CoreDB. `--backup` is likewise a CLI concern: the
// CLI takes the dump before calling in, so purge itself creates no
// backup of any kind.
func (dm *DatabaseManager) PurgeWork(req WorkPurgeRequest) (*WorkPurgeReport, error) {
	if req.WorkID == "" {
		return nil, fmt.Errorf("purge: work_id is required")
	}
	// Reason code is validated before the dry-run/force branch, so a
	// dry run cannot be used to launder an invalid code past the
	// operator and into a later forced run.
	if req.ReasonCode == "" || !WorkPurgeReasonCodeIsValid(req.ReasonCode) {
		return nil, ErrWorkPurgeInvalidReasonCode
	}

	// Phase 1 (read-only): preflight outside the transaction. On the
	// dry-run path this is the whole operation.
	report, err := dm.PreflightWorkPurge(dm, req.WorkID)
	if err != nil {
		return nil, err
	}
	report.DryRun = !req.Force
	report.ReasonCode = req.ReasonCode
	if !req.Force {
		return report, nil
	}
	// A forced purge does not override a reference. The operator
	// resolves the referrers and retries; purge does not rewrite them.
	if len(report.Referrers) > 0 {
		return report, fmt.Errorf("%w: %d inbound structural reference(s) must be resolved first",
			ErrWorkPurgeReferenced, len(report.Referrers))
	}

	// Phase 2: preflight + delete + audit in one transaction. Preflight
	// repeats inside the tx rather than reusing the report above: a
	// referrer could appear between the two reads, and a purge that
	// raced a new reference would silently orphan it.
	auditID := GenerateID()
	purgedAt := time.Now().Unix()
	var counts WorkPurgeCounts

	err = dm.WithTx(func(tx DBNode) error {
		referrers, err := dm.discoverWorkPurgeReferrers(tx, req.WorkID)
		if err != nil {
			return err
		}
		if len(referrers) > 0 {
			return fmt.Errorf("%w: %d inbound structural reference(s) must be resolved first",
				ErrWorkPurgeReferenced, len(referrers))
		}

		// Delete set (§5.5), in dependency order: children before the
		// parent, so no statement can ever observe a works row whose
		// ledger is already gone outside this transaction.
		//
		// work_events is MANDATORY, not optional. Those rows carry the
		// same note/title/content free text as the works row; a purge
		// that left them would defeat its own purpose.
		deletes := []struct {
			dest *int
			sql  string
		}{
			{&counts.WorkEvents, `DELETE FROM work_events WHERE work_id = ?`},
			{&counts.Evidence, `DELETE FROM evidence WHERE artifact_id = ? AND artifact_type = 'work'`},
			{&counts.ArtifactProvenance, `DELETE FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'`},
			{&counts.EpistemicProvenance, `DELETE FROM epistemic_provenance WHERE downstream_id = ?`},
		}
		for _, d := range deletes {
			res, err := tx.ExecTracked(d.sql, 0, req.WorkID)
			if err != nil {
				return fmt.Errorf("purge: delete: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("purge: rows affected: %w", err)
			}
			*d.dest = int(n)
		}

		res, err := tx.ExecTracked(`DELETE FROM works WHERE id = ?`, 0, req.WorkID)
		if err != nil {
			return fmt.Errorf("purge: delete works row: %w", err)
		}
		worksDeleted, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("purge: rows affected: %w", err)
		}
		if worksDeleted != 1 {
			// Preflight resolved the row moments ago inside this same
			// transaction. A concurrent delete is the only way to get
			// here, and committing a purge audit row for a purge that
			// deleted nothing would be a false record.
			return fmt.Errorf("purge: works row disappeared during purge (expected 1 row deleted, got %d)", worksDeleted)
		}

		// Audit row. Every value below is either operator input or a
		// count of what was deleted. Nothing is read out of the work
		// item or its events — the work row is already gone by this
		// point, and no code path in this function ever copies title,
		// content, or an event note into this record. The table has no
		// column that could hold them (§5.6).
		countsJSON, err := json.Marshal(counts)
		if err != nil {
			return fmt.Errorf("purge: encode counts: %w", err)
		}
		if _, err := tx.ExecTracked(`
			INSERT INTO work_purge_audit
				(id, work_id, purged_at, reason_code, note, operator, counts)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, 0, auditID, req.WorkID, purgedAt, req.ReasonCode,
			nullableOrNil(req.Note), nullableOrNil(req.Operator), string(countsJSON)); err != nil {
			return fmt.Errorf("purge: write audit row: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	report.DryRun = false
	report.Counts = counts
	report.AuditID = auditID
	report.PurgedAt = purgedAt

	// Write verification (CLAUDE.md §4). The transaction committed
	// without error, which is not on its own evidence that the
	// intended state persisted. Read the authoritative tables back.
	verified, err := dm.verifyWorkPurged(req.WorkID)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, fmt.Errorf("purge: post-write read-back found residual rows for %s; "+
			"the transaction committed but the delete set is incomplete", req.WorkID)
	}
	return report, nil
}

// verifyWorkPurged reads the authoritative tables back after a commit.
// A purge that reports success while rows remain is the failure mode
// this exists to prevent.
func (dm *DatabaseManager) verifyWorkPurged(workID string) (bool, error) {
	probes := []struct {
		label string
		sql   string
	}{
		{"works", `SELECT COUNT(*) FROM works WHERE id = ?`},
		{"work_events", `SELECT COUNT(*) FROM work_events WHERE work_id = ?`},
		{"evidence", `SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'work'`},
		{"artifact_provenance", `SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'`},
		{"epistemic_provenance", `SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`},
	}
	for _, p := range probes {
		var n int
		if err := dm.db.QueryRow(p.sql, workID).Scan(&n); err != nil {
			return false, fmt.Errorf("purge: verify %s: %w", p.label, err)
		}
		if n != 0 {
			return false, nil
		}
	}
	// The audit row must exist, or the purge left no record of itself.
	var audited int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM work_purge_audit WHERE work_id = ?`, workID).Scan(&audited); err != nil {
		return false, fmt.Errorf("purge: verify audit row: %w", err)
	}
	if audited < 1 {
		return false, nil
	}
	return true, nil
}

// nullableString maps "" to a nil argument so an omitted note/operator
// stores SQL NULL rather than an empty string. The distinction is
// meaningful in a permanent record: "no note given" is not the same
// statement as "a note consisting of nothing".
func nullableOrNil(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
