package assignment

import (
	"fmt"
	"strings"
)

// The dispatcher's two read paths.
//
// They are READS, and they are the only new surface the assignment
// service adds for PR 4. The dispatcher does not get a privileged read:
// it sees the same rows every other caller sees, through the same
// assignment-shaped projection.

// DispatchQuery selects the assignments a dispatcher will consider.
//
// The ORDER is deterministic — oldest first, then by id — so two
// dispatchers that see the same rows consider them in the same order, and
// a report is reproducible.
type DispatchQuery struct {
	// States are the assignment states the dispatcher will consider. An
	// empty list means the dispatcher names none, and it is an error: a
	// dispatcher that considers EVERY state would pick up work that is
	// waiting for the operator.
	States []string
	// Limit bounds how many rows are returned. It must be positive.
	Limit int
}

// DispatchCandidates lists the assignments a dispatcher may consider.
//
// It does NOT decide eligibility. It returns the rows in a deterministic
// order and lets the caller apply its own policy — because the caller's
// policy (a backoff, an attempt cap) is the dispatcher's, and the
// service's policy (the budget, the lease, the epoch) is enforced by the
// writes the dispatcher goes on to make.
func (s *Store) DispatchCandidates(q DispatchQuery) ([]*Assignment, error) {
	if len(q.States) == 0 {
		return nil, fmt.Errorf("%w: a dispatch query must name the states it considers",
			ErrConstraint)
	}
	if q.Limit <= 0 {
		return nil, fmt.Errorf("%w: a dispatch query must bound its rows", ErrConstraint)
	}
	where := make([]string, 0, len(q.States))
	args := make([]interface{}, 0, len(q.States)+1)
	for _, state := range q.States {
		if state == "" {
			continue
		}
		where = append(where, "state = ?")
		args = append(args, state)
	}
	if len(where) == 0 {
		return nil, fmt.Errorf("%w: a dispatch query must name the states it considers",
			ErrConstraint)
	}
	args = append(args, q.Limit)

	rows, err := s.db.SQLDB().Query(
		`SELECT `+assignmentColumns+` FROM agent_tasks
		  WHERE `+strings.Join(where, " OR ")+`
		  ORDER BY created_at ASC, id ASC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("assignment: read the dispatch candidates: %w", err)
	}
	defer rows.Close()
	out := []*Assignment{}
	for rows.Next() {
		a, err := scanAssignment(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("assignment: read the dispatch candidates: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

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
