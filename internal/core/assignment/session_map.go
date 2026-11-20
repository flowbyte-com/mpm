package assignment

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	internal "github.com/flowbyte-com/mpm-core"
)

// The assignment-to-session mapping.
//
// WHAT THIS IS
//
// One row per ATTEMPT to execute one assignment in one native runtime
// session. The adapter writes the row BEFORE it launches the runtime, and
// the row carries the session id the launch will use — so an interruption
// between the write and the launch is recoverable by id rather than
// ambiguous.
//
// WHY IT IS NOT A FIELD ON agent_tasks
//
// PR 1's DDL is hash-pinned and `payload` is not in any transition's
// settable-column whitelist, so an assignment cannot carry a fact that
// becomes known later. The mapping is separate state with its own
// lifecycle, so it gets its own table (migration_agent_task_sessions.go).
//
// THE FENCE
//
// Every write here presents the attempt's claim epoch AND requires that
// epoch to still be the assignment's current claim, in the SAME statement:
//
//	WHERE id = ? AND claim_epoch = ?
//	  AND EXISTS (SELECT 1 FROM agent_tasks
//	               WHERE id = agent_task_sessions.task_id
//	                 AND claim_epoch = ?)
//
// A stale worker's write therefore matches NO ROW, atomically with the
// write itself. That is the property that stops a reclaimed worker from
// publishing its result as the current assignment's completion.

// SessionAttempt is one attempt to execute an assignment in a native
// runtime session.
type SessionAttempt struct {
	ID              string `json:"id"`
	TaskID          string `json:"task_id"`
	Attempt         int64  `json:"attempt"`
	ClaimEpoch      int64  `json:"claim_epoch"`
	Runtime         string `json:"runtime"`
	IntendedAgent   string `json:"intended_agent"`
	NativeSessionID string `json:"native_session_id"`
	CorrelationRef  string `json:"correlation_ref"`
	WorkspaceID     string `json:"workspace_id"`
	WorkspacePath   string `json:"workspace_path"`
	LaunchState     string `json:"launch_state"`
	RuntimeOutcome  string `json:"runtime_outcome,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// The launch-state vocabulary. It matches the table's CHECK, and it only
// moves forward.
const (
	SessionIntent    = "intent"
	SessionLaunched  = "launched"
	SessionRunning   = "running"
	SessionCompleted = "completed"
	SessionFailed    = "failed"
	SessionAmbiguous = "ambiguous"
	SessionCancelled = "cancelled"
)

// The runtime vocabulary. Closed on purpose: an unknown runtime is a row
// nothing could reconcile.
const (
	RuntimeOpenCode = "opencode"
)

// BeginAttemptParams describes a launch INTENT.
//
// NativeSessionID is REQUIRED and is minted by the caller BEFORE this call.
// That is the whole point: OpenCode's `session.create` is not idempotent,
// but `opencode run --session <id>` creates-or-continues the session with
// the caller's id. Persisting the id first is what turns an interrupted
// launch into a recoverable fact.
type BeginAttemptParams struct {
	TaskID          string
	Epoch           int64
	Runtime         string
	IntendedAgent   string
	NativeSessionID string
	CorrelationRef  string
	WorkspaceID     string
	WorkspacePath   string
}

// BeginAttempt persists a launch INTENT and returns the row.
//
// It requires:
//   - the assignment to exist and to have a role that executes;
//   - the presented epoch to BE the assignment's current claim;
//   - the assignment to be in a state that may be executed;
//   - a non-empty, allow-listed runtime and session id.
//
// The intent is written in the SAME transaction that re-reads the
// assignment, so an epoch that moved between the check and the write
// cannot produce an intent for a claim the caller does not hold.
func (s *Store) BeginAttempt(p BeginAttemptParams) (*SessionAttempt, error) {
	if p.Runtime != RuntimeOpenCode {
		return nil, fmt.Errorf("%w: the runtime must be %q", ErrConstraint, RuntimeOpenCode)
	}
	if strings.TrimSpace(p.NativeSessionID) == "" {
		return nil, fmt.Errorf("%w: a launch intent must carry the session id it will use",
			ErrConstraint)
	}
	if strings.TrimSpace(p.IntendedAgent) == "" {
		return nil, fmt.Errorf("%w: a launch intent must name the agent definition",
			ErrConstraint)
	}

	var out *SessionAttempt
	err := s.db.WithTx(func(node internal.DBNode) error {
		task, err := getAssignmentTx(node, p.TaskID)
		if err != nil {
			return err
		}
		// BOTH ROLES HAVE EXECUTION PATHS: an implementer runs in the
		// worker containment, a reviewer in its own sandbox. The WORKER
		// path refuses a reviewer in Execute; the mapping itself does not
		// care which, because a review is executed and recovered the same
		// way.
		if task.Role != RoleImplementer && task.Role != RoleReviewer {
			return fmt.Errorf("%w: %s is a %s", ErrConstraint, task.ID, task.Role)
		}
		if p.Epoch != task.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, task.ID, task.ClaimEpoch)
		}
		if task.ClaimEpoch == 0 {
			return fmt.Errorf("%w: %s holds no claim", ErrConstraint, task.ID)
		}
		switch task.State {
		case StateAccepted, StateInProgress:
		default:
			return fmt.Errorf("%w: %s is %s and cannot be executed",
				ErrIllegalTransition, task.ID, task.State)
		}

		id, err := newID("ats")
		if err != nil {
			return err
		}
		now := s.Now()

		// The attempt number is assigned in SQL so two callers cannot
		// both compute "1" outside a transaction.
		if _, err := node.ExecTracked(`
			INSERT INTO agent_task_sessions (
				id, task_id, attempt, claim_epoch, runtime, intended_agent,
				native_session_id, correlation_ref, workspace_id, workspace_path,
				launch_state, created_at, updated_at
			)
			SELECT ?, ?, COALESCE(MAX(attempt), 0) + 1, ?, ?, ?, ?, ?, ?, ?,
			       ?, ?, ?
			  FROM agent_task_sessions
			 WHERE task_id = ?`,
			5, id, p.TaskID, p.Epoch, p.Runtime, p.IntendedAgent,
			p.NativeSessionID, p.CorrelationRef, p.WorkspaceID, p.WorkspacePath,
			SessionIntent, now, now, p.TaskID); err != nil {
			return storageRefusal(err)
		}

		// WRITE-PATH READ-BACK. A nil error from Exec is not proof of
		// persistence: a trigger may have skipped the row, or the SELECT
		// source may have been empty and inserted nothing.
		got, err := getAttemptTx(node, id)
		if err != nil {
			return err
		}
		if got == nil {
			return errors.New("assignment: the launch intent did not persist")
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LatestAttempt returns the highest-numbered attempt for a task, or nil.
func (s *Store) LatestAttempt(taskID string) (*SessionAttempt, error) {
	row := s.db.SQLDB().QueryRow(`
		SELECT `+attemptColumns+`
		  FROM agent_task_sessions
		 WHERE task_id = ?
		 ORDER BY attempt DESC
		 LIMIT 1`, taskID)
	return scanAttempt(row)
}

// AttemptsFor returns every attempt for a task, oldest first.
func (s *Store) AttemptsFor(taskID string) ([]*SessionAttempt, error) {
	rows, err := s.db.SQLDB().Query(`
		SELECT `+attemptColumns+`
		  FROM agent_task_sessions
		 WHERE task_id = ?
		 ORDER BY attempt ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("assignment: list the attempts: %w", err)
	}
	defer rows.Close()
	var out []*SessionAttempt
	for rows.Next() {
		a, err := scanAttemptRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AttemptByNativeSession answers the recovery question: does this native
// session already belong to an attempt?
func (s *Store) AttemptByNativeSession(runtime, sessionID string) (*SessionAttempt, error) {
	row := s.db.SQLDB().QueryRow(`
		SELECT `+attemptColumns+`
		  FROM agent_task_sessions
		 WHERE runtime = ? AND native_session_id = ?`, runtime, sessionID)
	return scanAttempt(row)
}

// SessionStateParams names the attempt a state write is about.
type SessionStateParams struct {
	AttemptID string
	Epoch     int64
	// Outcome is the runtime's own reported outcome, when it gave one.
	Outcome string
	// Error is a BOUNDED, credential-free note. Never a transcript.
	Error string
}

// MarkAttemptLaunched records that the runtime process was started.
func (s *Store) MarkAttemptLaunched(p SessionStateParams) (*SessionAttempt, error) {
	return s.setAttemptState(p, SessionLaunched, "")
}

// MarkAttemptRunning records that the runtime reports the session working.
func (s *Store) MarkAttemptRunning(p SessionStateParams) (*SessionAttempt, error) {
	return s.setAttemptState(p, SessionRunning, "")
}

// MarkAttemptCompleted records a REAL runtime completion.
//
// The caller must have observed the runtime's own outcome; this method
// records what it was told. It does not infer success, and an empty
// outcome is stored as "the runtime did not say" rather than as success.
func (s *Store) MarkAttemptCompleted(p SessionStateParams) (*SessionAttempt, error) {
	return s.setAttemptState(p, SessionCompleted, p.Outcome)
}

// MarkAttemptFailed records a truthful failure.
func (s *Store) MarkAttemptFailed(p SessionStateParams) (*SessionAttempt, error) {
	return s.setAttemptState(p, SessionFailed, p.Outcome)
}

// MarkAttemptAmbiguous records that the adapter could not determine
// whether work happened.
//
// This is a first-class state, not an error path: OpenCode offers no
// idempotent creation primitive, so an interruption between "the intent is
// persisted" and "we observed the run" is genuinely ambiguous. Recording
// the ambiguity is the honest answer, and it is what stops a retry from
// silently duplicating execution.
func (s *Store) MarkAttemptAmbiguous(p SessionStateParams) (*SessionAttempt, error) {
	return s.setAttemptState(p, SessionAmbiguous, p.Outcome)
}

// MarkAttemptCancelled records an operator cancellation.
func (s *Store) MarkAttemptCancelled(p SessionStateParams) (*SessionAttempt, error) {
	return s.setAttemptState(p, SessionCancelled, p.Outcome)
}

// setAttemptState is the ONE state write.
//
// THE FENCE IS THE WHERE CLAUSE. The presented epoch must match the
// attempt's epoch AND still be the assignment's current claim, in the same
// statement — so a reclaimed worker's write matches no row and reports a
// stale claim rather than overwriting the new holder's record.
func (s *Store) setAttemptState(p SessionStateParams, toState, outcome string) (*SessionAttempt, error) {
	var out *SessionAttempt
	err := s.db.WithTx(func(node internal.DBNode) error {
		current, err := getAttemptTx(node, p.AttemptID)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("%w: attempt %q does not exist", ErrNotFound, p.AttemptID)
		}
		if p.Epoch != current.ClaimEpoch {
			return fmt.Errorf("%w: attempt %s is at epoch %d",
				ErrStaleClaim, current.ID, current.ClaimEpoch)
		}
		if err := checkAttemptTransition(current.LaunchState, toState); err != nil {
			return fmt.Errorf("%w: attempt %s", err, current.ID)
		}

		var outcomeArg interface{}
		if outcome != "" {
			outcomeArg = outcome
		}
		var errorArg interface{}
		if p.Error != "" {
			errorArg = p.Error
		}
		now := s.Now()
		res, err := node.ExecTracked(`
			UPDATE agent_task_sessions
			   SET launch_state = ?, runtime_outcome = COALESCE(?, runtime_outcome),
			       last_error = COALESCE(?, last_error), updated_at = ?
			 WHERE id = ? AND claim_epoch = ?
			   AND EXISTS (SELECT 1 FROM agent_tasks
			                WHERE id = agent_task_sessions.task_id
			                  AND claim_epoch = ?)`,
			5, toState, outcomeArg, errorArg, now, p.AttemptID, p.Epoch, p.Epoch)
		if err != nil {
			return storageRefusal(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("assignment: read the affected rows: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf("%w: attempt %s was fenced out (the claim moved)", ErrStaleClaim, current.ID)
		}
		got, err := getAttemptTx(node, p.AttemptID)
		if err != nil {
			return err
		}
		if got == nil {
			return errors.New("assignment: the attempt state write did not persist")
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkAttemptTransition mirrors the table's forward-only trigger in Go, so
// the SERVICE refuses a backwards move with a typed error before the
// database does — and so the trigger is the backstop rather than the only
// defence.
func checkAttemptTransition(from, to string) error {
	if from == to {
		return nil
	}
	allowed := map[string]map[string]bool{
		// A blocking launch can complete before the adapter observes
		// any intermediate state, so `intent` reaches the terminal
		// states directly. That is forward, not backwards.
		SessionIntent: {
			SessionLaunched: true, SessionCompleted: true, SessionFailed: true,
			SessionAmbiguous: true, SessionCancelled: true,
		},
		SessionLaunched: {
			SessionRunning: true, SessionCompleted: true, SessionFailed: true,
			SessionAmbiguous: true, SessionCancelled: true,
		},
		SessionRunning: {
			SessionCompleted: true, SessionFailed: true,
			SessionAmbiguous: true, SessionCancelled: true,
		},
	}
	if allowed[from][to] {
		return nil
	}
	return fmt.Errorf("%w: the launch state cannot move from %q to %q", ErrIllegalTransition, from, to)
}

const attemptColumns = `id, task_id, attempt, claim_epoch, runtime, intended_agent,
	native_session_id, correlation_ref, workspace_id, workspace_path,
	launch_state, COALESCE(runtime_outcome, ''), COALESCE(last_error, ''),
	created_at, updated_at`

func getAttemptTx(node internal.DBNode, id string) (*SessionAttempt, error) {
	row := node.QueryRowTracked(`SELECT `+attemptColumns+`
		FROM agent_task_sessions WHERE id = ?`, id)
	a, err := scanAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func scanAttempt(row *sql.Row) (*SessionAttempt, error) {
	var a SessionAttempt
	err := row.Scan(&a.ID, &a.TaskID, &a.Attempt, &a.ClaimEpoch, &a.Runtime,
		&a.IntendedAgent, &a.NativeSessionID, &a.CorrelationRef, &a.WorkspaceID,
		&a.WorkspacePath, &a.LaunchState, &a.RuntimeOutcome, &a.LastError,
		&a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assignment: read the attempt: %w", err)
	}
	return &a, nil
}

func scanAttemptRow(rows *sql.Rows) (*SessionAttempt, error) {
	var a SessionAttempt
	if err := rows.Scan(&a.ID, &a.TaskID, &a.Attempt, &a.ClaimEpoch, &a.Runtime,
		&a.IntendedAgent, &a.NativeSessionID, &a.CorrelationRef, &a.WorkspaceID,
		&a.WorkspacePath, &a.LaunchState, &a.RuntimeOutcome, &a.LastError,
		&a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, fmt.Errorf("assignment: read the attempt: %w", err)
	}
	return &a, nil
}
