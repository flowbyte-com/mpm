package assignment

import (
	"fmt"

	internal "github.com/flowbyte-com/mpm-core"
)

// maxLeaseSeconds bounds a lease. A lease is a window, and an unbounded
// window is not a window: it is a claim that never expires and never
// yields.
const maxLeaseSeconds int64 = 24 * 60 * 60

// Claim takes the lease on an assignment.
//
// IT IS A COMPARE-AND-SWAP. The caller presents the epoch it believes is
// current, and the write is `WHERE id = ? AND claim_epoch = ?`. Two
// simultaneous claims therefore cannot both succeed: the first takes
// epoch n → n+1, and the second's WHERE no longer matches.
//
// The semantics, precisely:
//
//   - an UNCLAIMED assignment (epoch 0) is granted the claim;
//   - a CLAIMED assignment whose lease is ALIVE is refused — the work is
//     leased to another holder, and a live lease is not stealable;
//   - a CLAIMED assignment whose lease has EXPIRED is reclaimed: the
//     epoch advances, the new owner is written, and the previous holder's
//     epoch is fenced out.
//
// The lease belongs to the claim, so a claim always sets a NEW lease —
// PR 1's rule, and the reason a reclaim cannot inherit the old window.
func (s *Store) Claim(p ClaimParams) (*Assignment, error) {
	if err := requireText("owner", p.Owner); err != nil {
		return nil, err
	}
	if p.LeaseSeconds <= 0 {
		return nil, fmt.Errorf("%w: the lease must be positive", ErrConstraint)
	}
	if p.LeaseSeconds > maxLeaseSeconds {
		return nil, fmt.Errorf("%w: the lease is longer than %d seconds", ErrConstraint, maxLeaseSeconds)
	}

	var out *Assignment
	err := s.db.WithTx(func(node internal.DBNode) error {
		current, err := getAssignmentTx(node, p.ID)
		if err != nil {
			return err
		}
		if p.ExpectedEpoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, p.ID, current.ClaimEpoch)
		}
		switch current.State {
		case StateAwaitingOperator, StateOperatorApproved, StateCancelled:
			return fmt.Errorf("%w: %s is %s and holds no claim",
				ErrConstraint, p.ID, current.State)
		}

		now := s.Now()
		reclaimed := false
		if current.ClaimEpoch > 0 {
			alive := current.LeaseExpiresAt != nil && *current.LeaseExpiresAt > now
			if alive {
				if current.Owner == p.Owner {
					return fmt.Errorf(
						"%w: %s is leased to %s until %d — renew it rather than claiming it",
						ErrAlreadyClaimed, p.ID, current.Owner, *current.LeaseExpiresAt)
				}
				return fmt.Errorf(
					"%w: %s is leased to %s until %d",
					ErrLeaseHeld, p.ID, current.Owner, *current.LeaseExpiresAt)
			}
			reclaimed = true
		}

		newEpoch := current.ClaimEpoch + 1
		newLease := now + p.LeaseSeconds

		if err := s.injectFault("claim"); err != nil {
			return err
		}
		res, err := node.ExecTracked(`
			UPDATE agent_tasks
			SET claim_epoch = ?, owner = ?, lease_expires_at = ?, updated_at = ?
			WHERE id = ? AND claim_epoch = ?`,
			5, newEpoch, p.Owner, newLease, now, p.ID, p.ExpectedEpoch)
		if err != nil {
			return storageRefusal(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("assignment: read the affected rows: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf("%w: %s was claimed by somebody else", ErrStaleClaim, p.ID)
		}

		if err := s.injectFault("claim-event"); err != nil {
			return err
		}
		epoch := newEpoch
		payload := map[string]interface{}{
			"owner":        p.Owner,
			"lease_until":  newLease,
			"reclaimed":    reclaimed,
		}
		if reclaimed {
			// The previous holder is recorded, so the ledger says
			// whose lease ended rather than only that a new one
			// began.
			payload["previous_owner"] = current.Owner
			if current.LeaseExpiresAt != nil {
				payload["previous_lease_until"] = *current.LeaseExpiresAt
			}
		}
		if _, err := appendEvent(node, p.ID, EventClaimed, current.State, current.State,
			p.Actor, &epoch, claimNote(reclaimed), payload, now); err != nil {
			return err
		}

		row, err := getAssignmentTx(node, p.ID)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Renew extends the holder's lease.
//
// A renewal is a CLAIM BY THE SAME HOLDER: the epoch advances and the
// lease is new. It has to be — PR 1's rule is that the lease changes only
// when the claim epoch advances, so a renewal that kept the epoch could
// not move the lease at all.
//
// The holder must present the epoch it holds, so a holder whose lease was
// already reclaimed cannot renew: its epoch is stale.
func (s *Store) Renew(p RenewParams) (*Assignment, error) {
	if err := requireText("owner", p.Owner); err != nil {
		return nil, err
	}
	if p.LeaseSeconds <= 0 {
		return nil, fmt.Errorf("%w: the lease must be positive", ErrConstraint)
	}
	if p.LeaseSeconds > maxLeaseSeconds {
		return nil, fmt.Errorf("%w: the lease is longer than %d seconds", ErrConstraint, maxLeaseSeconds)
	}

	var out *Assignment
	err := s.db.WithTx(func(node internal.DBNode) error {
		current, err := getAssignmentTx(node, p.ID)
		if err != nil {
			return err
		}
		if p.ExpectedEpoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, p.ID, current.ClaimEpoch)
		}
		if current.ClaimEpoch == 0 {
			return fmt.Errorf("%w: %s holds no claim to renew", ErrAlreadyClaimed, p.ID)
		}
		if current.Owner != p.Owner {
			return fmt.Errorf("%w: %s is leased to %s", ErrLeaseHeld, p.ID, current.Owner)
		}

		now := s.Now()
		newEpoch := current.ClaimEpoch + 1
		newLease := now + p.LeaseSeconds

		if err := s.injectFault("renew"); err != nil {
			return err
		}
		res, err := node.ExecTracked(`
			UPDATE agent_tasks
			SET claim_epoch = ?, lease_expires_at = ?, updated_at = ?
			WHERE id = ? AND claim_epoch = ?`,
			5, newEpoch, newLease, now, p.ID, p.ExpectedEpoch)
		if err != nil {
			return storageRefusal(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("assignment: read the affected rows: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf("%w: %s was reclaimed while the renewal was in flight",
				ErrStaleClaim, p.ID)
		}

		if err := s.injectFault("renew-event"); err != nil {
			return err
		}
		epoch := newEpoch
		if _, err := appendEvent(node, p.ID, EventClaimed, current.State, current.State,
			p.Actor, &epoch, "the lease was renewed",
			map[string]interface{}{
				"owner":       p.Owner,
				"lease_until": newLease,
				"renewed":     true,
			}, now); err != nil {
			return err
		}

		row, err := getAssignmentTx(node, p.ID)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// claimNote is the ledger note for a claim.
func claimNote(reclaimed bool) string {
	if reclaimed {
		return "the expired lease was reclaimed"
	}
	return "the assignment was claimed"
}
