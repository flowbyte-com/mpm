package assignment

import (
	"fmt"
)

// The dispatcher's two read paths.
//
// They are READS, and they are the only new surface the assignment
// service adds for PR 4. The dispatcher does not get a privileged read:
// it sees the same rows every other caller sees, through the same
// assignment-shaped projection.

// DispatchQuery selects the assignments a dispatcher will consider.
//
// The STATES ARE FIXED, not a parameter. They are the two states from
// which work can be dispatched, and a caller that could name its own
// states could name one that is waiting for the operator — so the
// vocabulary belongs to the service, not to the caller.
//
// The ORDER is deterministic — oldest first, then by id — so two
// dispatchers that see the same rows consider them in the same order,
// and a report is reproducible.
type DispatchQuery struct {
	// Limit bounds how many rows are returned. It must be positive.
	Limit int
}

// dispatchableStates are the states work can be dispatched from.
//
// `accepted` is work the recipient has committed to and nobody has
// started; `in_progress` is work that was started and whose session may
// need recovering. Everything else is not: `enqueued` has not been
// offered yet, `delivered` and `acknowledged` are not commitments, and
// the terminal and operator-gated states are finished or waiting for the
// operator.
const dispatchableStates = "('accepted', 'in_progress')"

// DispatchCandidates lists the assignments a dispatcher may consider.
//
// It does NOT decide eligibility. It returns the rows in a deterministic
// order and lets the caller apply its own policy — because the caller's
// policy (a backoff, an attempt cap) is the dispatcher's, and the
// service's policy (the budget, the lease, the epoch) is enforced by
// the writes the dispatcher goes on to make.
func (s *Store) DispatchCandidates(q DispatchQuery) ([]*Assignment, error) {
	if q.Limit <= 0 {
		return nil, fmt.Errorf("%w: a dispatch query must bound its rows", ErrConstraint)
	}
	// T53: superseded work is frozen and never dispatches. The original
	// (and anything below it, or a stale reviewer's slot) is excluded
	// here, so the dispatcher never considers work recovery froze.
	// Ambiguous-but-live tasks remain listed: Execute refuses them with
	// recovery_required, which is the dispatcher's own policy input.
	//
	// The historical test lives in Go (isHistorical), not in a second
	// copy of the trigger predicate: one implementation, no drift.
	//
	// Retrieval is BOUNDED BY DETERMINISTIC PAGINATION, not by a bare
	// SQL LIMIT: a LIMIT applied before the historical filter would let
	// a long run of frozen rows hide eligible work behind it, while
	// reading every row at once materialises the whole table (F10).
	// Each page is keyset-paged on the same (created_at, id) order the
	// dispatcher uses, so a later page can never repeat or skip a row.
	out := []*Assignment{}
	var lastCreated int64
	var lastID string
	for {
		rows, err := s.db.SQLDB().Query(
			`SELECT `+assignmentColumns+` FROM agent_tasks
			  WHERE state IN `+dispatchableStates+`
			    AND (created_at > ? OR (created_at = ? AND id > ?))
			  ORDER BY created_at ASC, id ASC
			  LIMIT ?`, lastCreated, lastCreated, lastID, DispatchCandidatePageSize)
		if err != nil {
			return nil, fmt.Errorf("assignment: read the dispatch candidates: %w", err)
		}
		page := 0
		for rows.Next() {
			a, err := scanAssignment(rows.Scan)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("assignment: read the dispatch candidates: %w", err)
			}
			page++
			lastCreated, lastID = a.CreatedAt, a.ID
			historical, err := s.isHistorical(a)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if historical {
				continue
			}
			out = append(out, a)
			if len(out) >= q.Limit {
				rows.Close()
				return out, nil
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("assignment: read the dispatch candidates: %w", err)
		}
		rows.Close()
		if page < DispatchCandidatePageSize {
			return out, nil
		}
	}
}

// DispatchCandidatePageSize bounds ONE page of dispatch-candidate
// retrieval. It is EXPORTED so a test can prove the pagination behaviour
// with a small population; production never writes it.
var DispatchCandidatePageSize = 256

// OutstandingAttempt is one attempt that has not reached a terminal state.
//
// It is the thing a dispatcher recovers: an attempt whose launch was
// interrupted, or whose outcome could not be determined.
type OutstandingAttempt struct {
	// Attempt is the durable attempt record.
	Attempt *SessionAttempt
	// Assignment is the assignment it belongs to, with the CURRENT
	// claim epoch and lease.
	Assignment *Assignment
}

// OutstandingAttempts lists the attempts that are not terminal.
//
// The terminal states are completed, failed and cancelled — the three a
// retry may follow. Everything else is outstanding: `intent` (the launch
// may or may not have happened), `launched` and `running` (it may still
// be working), and `ambiguous` (the adapter could not determine what
// happened, which is NOT the same as a failure).
//
// The ORDER is deterministic: the oldest outstanding attempt first, so a
// dispatcher recovers the longest-interrupted work first.
func (s *Store) OutstandingAttempts() ([]*OutstandingAttempt, error) {
	rows, err := s.db.SQLDB().Query(`
		SELECT ` + attemptColumns + `
		  FROM agent_task_sessions
		 WHERE launch_state NOT IN ('completed', 'failed', 'cancelled')
		 ORDER BY created_at ASC, attempt ASC`)
	if err != nil {
		return nil, fmt.Errorf("assignment: read the outstanding attempts: %w", err)
	}
	defer rows.Close()

	out := []*OutstandingAttempt{}
	for rows.Next() {
		a, err := scanAttemptRow(rows)
		if err != nil {
			return nil, fmt.Errorf("assignment: read the outstanding attempts: %w", err)
		}
		out = append(out, &OutstandingAttempt{Attempt: a})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assignment: read the outstanding attempts: %w", err)
	}
	// The assignment rows are read one by one: the attempt projection and
	// the assignment projection have different shapes, and joining them
	// into one scan would couple two shapes that have no reason to be
	// coupled.
	for _, o := range out {
		task, err := s.getAssignment(o.Attempt.TaskID)
		if err != nil {
			if err == ErrNotFound {
				continue
			}
			return nil, fmt.Errorf("assignment: read the outstanding attempts: %w", err)
		}
		o.Assignment = task
	}
	return out, nil
}
