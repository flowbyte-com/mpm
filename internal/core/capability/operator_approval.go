// operator_approval.go — MarkOperatorApproved Store method.
//
// Stamps `metadata.operator_approved_at` (int64 unix epoch seconds)
// on a capability row so the executor's operator-domain gate
// (executor.go: int64FromMeta(req.Capability.Metadata, "operator_approved_at"))
// admits the capability at invoke-time. Also writes a
// capability_events row with event_type='operator_approval' so the
// audit trail (`mpm skill audit <id>`) preserves the human-readable
// RFC3339 timestamp + actor for forensics.
//
// Metadata format (locked in by the executor gate):
//
//	metadata.operator_approved_at: int64 unix epoch seconds
//
// WHY TWO FIELDS:
//
//   - The executor gate only checks for the int64 being non-zero.
//   Strict int64 keeps the gate fast (no string parsing on the
//   hot path) and the on-disk column compact.
//
//   - A second field, metadata.operator_approved_at_rfc3339, is
//   stored alongside for human readability ("the operator approved
//   this at 2026-08-05T17:42:01Z"). The gate ignores it; the
//   audit-trail surfacing code uses it.
//
//   - The capability_events row carries the same RFC3339 string
//   + actor in its metadata so the audit trail can never
//   disagree with the metadata stamp (both writes happen in the
//   same transaction).
//
// State-restriction rationale:
//
//   - Refused on retired / fractured: a retired capability is dead
//   and shouldn't be re-approved; a fractured one is broken and
//   the operator should reconcile the fracture before approving
//   it for operator-domain execution.
//   - Allowed on validated / probation / active / degraded /
//   needs_revision: the operator might pre-approve a validated
//   primitive before its first invocation, or re-stamp an
//   already-approved capability to refresh the timestamp.
//
// Idempotency: re-running MarkOperatorApproved on an already-
// approved capability updates the timestamp + event row. The
// Store does NOT refuse re-stamping because operators sometimes
// need to assert "I re-verified this just now" — a fresh
// timestamp is the audit-preserving way to record that.
package capability

import (
	"errors"
	"fmt"
	"strings"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
)

// MarkOperatorApproved stamps metadata.operator_approved_at on the
// capability row and writes a matching capability_events row, both
// inside one transaction. Returns the updated capability for the
// caller to confirm the stamp landed.
//
// Refuses to stamp soft-deleted rows (ErrNotFound) and terminal
// states (retired / fractured surface as ErrOperatorApprovalRefused
// so the operator can see why their CLI call exited 1).
//
// Parameters:
//
//	id           — the capability id (e.g. "cap.list_capabilities")
//	actor        — the audit actor string (CLI passes "operator",
//	                tests pass "test")
//	approvedAt   — the timestamp to stamp. Tests inject a frozen
//	                clock; production uses Store.Now() if zero.
//	reason       — optional free-form rationale (e.g. "reviewed
//	                by sec-team 2026-08-05"). Empty string is OK;
//	                the audit row records "" rather than fabricating
//	                a value.
func (s *Store) MarkOperatorApproved(id, actor string, approvedAt time.Time, reason string) (*Capability, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("capability: MarkOperatorApproved: id is required")
	}
	if strings.TrimSpace(actor) == "" {
		actor = "operator"
	}
	if approvedAt.IsZero() {
		approvedAt = time.Unix(s.Now(), 0).UTC()
	}
	if approvedAt.Location() != time.UTC {
		approvedAt = approvedAt.UTC()
	}

	var updated *Capability
	err := s.withTx(func(n internal.DBNode) error {
		// 1. Load the row. getCapabilityTx returns ErrNotFound
		// for both missing AND soft-deleted rows — we don't want
		// to allow operator approval on rows that have been
		// shredded from the operator's view.
		c, err := s.getCapabilityTx(n, id)
		if err != nil {
			return err
		}

		// 2. State guard. retired and fractured are terminal
		// for the purpose of operator approval — the operator
		// should reconcile the underlying problem (retirement /
		// fracture cascade) rather than re-stamp approval.
		if c.State == StateRetired || c.State == StateFractured {
			return fmt.Errorf("%w: capability %s is in %s state",
				ErrOperatorApprovalRefused, id, c.State)
		}

		// 3. Merge metadata. Preserve any existing keys
		// (operator_approved_by, custom governance flags, etc.)
		// and overwrite the timestamp fields.
		meta := CapabilityMetadata{}
		for k, v := range c.Metadata {
			meta[k] = v
		}
		meta["operator_approved_at"] = approvedAt.Unix()
		meta["operator_approved_at_rfc3339"] = approvedAt.Format(time.RFC3339)
		meta["operator_approved_by"] = actor

		// 4. Update the row. WHERE id = ? AND deleted_at IS NULL
		// is a belt-and-suspenders guard against a concurrent
		// shred after our getCapabilityTx lookup.
		now := s.Now()
		q := `UPDATE capabilities
		      SET metadata = ?, updated_at = ?
		      WHERE id = ? AND deleted_at IS NULL`
		res, err := n.ExecTracked(q, 3, meta, now, id)
		if err != nil {
			return wrapDBError("MarkOperatorApproved:update", err)
		}
		nAffected, err := res.RowsAffected()
		if err != nil {
			return wrapDBError("MarkOperatorApproved:rowsAffected", err)
		}
		if nAffected == 0 {
			// The row was deleted between our load and
			// update. Surface ErrNotFound so the CLI
			// handler reports "not found" cleanly.
			return ErrNotFound
		}

		// 5. Audit event. lifecycle-table companion to the
		// metadata stamp. The event's metadata carries the
		// RFC3339 timestamp + actor so `mpm skill audit`
		// can render a human-readable timeline.
		eventMeta := EventMetadata{
			"operator_approved_at":      approvedAt.Unix(),
			"operator_approved_at_rfc3339": approvedAt.Format(time.RFC3339),
			"operator_approved_by":      actor,
		}
		if reason != "" {
			eventMeta["reason"] = reason
		}
		if err := s.insertTransitionEventTx(
			n, EventOperatorApproval, id,
			"", "", // no state transition (operator_approval is metadata-only)
			actor, reason, eventMeta,
		); err != nil {
			return fmt.Errorf("capability: MarkOperatorApproved:event: %w", err)
		}

		// 6. Reload the row so the caller sees the stamped
		// metadata (mirrors the simpleTransition pattern).
		reloaded, err := s.getCapabilityTx(n, id)
		if err != nil {
			return err
		}
		updated = reloaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ErrOperatorApprovalRefused is returned by MarkOperatorApproved when
// the target capability is in a state that should not be stamped
// (retired, fractured). Distinct from ErrNotFound so the CLI can
// surface "the capability exists but is in a state that refuses
// operator approval" rather than "the capability doesn't exist".
var ErrOperatorApprovalRefused = errors.New("capability: operator approval refused")
