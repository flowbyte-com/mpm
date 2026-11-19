package assignment

import (
	"database/sql"
	"fmt"
	"strings"
)

// Get reads one assignment.
//
// It is a READ: the history of an assignment is read with History, and
// neither writes anything.
func (s *Store) Get(id string) (*Assignment, error) {
	if err := requireText("id", id); err != nil {
		return nil, err
	}
	return s.getAssignment(id)
}

// Inbox lists the assignments addressed to a recipient.
//
// The recipient is an ADDRESS, not an identity: the listing says which
// work is addressed to a name. Who is answering the call is a question
// MPM cannot answer, and it does not pretend to.
//
// The states filter is optional; an empty filter returns every state.
func (s *Store) Inbox(q InboxQuery) ([]*Assignment, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultInboxLimit
	}
	if limit > maxInboxLimit {
		limit = maxInboxLimit
	}

	var (
		where []string
		args  []interface{}
	)
	if q.Recipient != "" {
		where = append(where, "recipient = ?")
		args = append(args, q.Recipient)
	}
	if q.ProjectID != "" {
		where = append(where, "project_id = ?")
		args = append(args, q.ProjectID)
	}
	for _, state := range q.States {
		if state == "" {
			continue
		}
		where = append(where, "state = ?")
		args = append(args, state)
	}

	query := `SELECT ` + assignmentColumns + ` FROM agent_tasks`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.SQLDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("assignment: read the inbox: %w", err)
	}
	defer rows.Close()

	out := []*Assignment{}
	for rows.Next() {
		a, err := scanAssignment(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("assignment: read the inbox: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assignment: read the inbox: %w", err)
	}
	return out, nil
}

// ListByRoot lists every assignment in one delegation tree.
//
// It is the tree view: the root and everything below it, so an operator
// can see the shape of the work and its budgets in one place.
func (s *Store) ListByRoot(rootID string) ([]*Assignment, error) {
	if err := requireText("root_id", rootID); err != nil {
		return nil, err
	}
	rows, err := s.db.SQLDB().Query(
		`SELECT `+assignmentColumns+` FROM agent_tasks WHERE root_id = ? ORDER BY depth, created_at, id`,
		rootID)
	if err != nil {
		return nil, fmt.Errorf("assignment: read the tree: %w", err)
	}
	defer rows.Close()

	out := []*Assignment{}
	for rows.Next() {
		a, err := scanAssignment(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("assignment: read the tree: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assignment: read the tree: %w", err)
	}
	return out, nil
}

// History reads an assignment's ledger, oldest first.
//
// The ledger is append-only, so this is the assignment's history and
// there is no way to edit it.
func (s *Store) History(id string) ([]Event, error) {
	if err := requireText("id", id); err != nil {
		return nil, err
	}
	rows, err := s.db.SQLDB().Query(`
		SELECT id, task_id, event_index, event_type,
		       COALESCE(from_state,''), COALESCE(to_state,''),
		       COALESCE(actor_address,''), COALESCE(actor_kind,''),
		       claim_epoch, COALESCE(note,''), payload, created_at
		FROM agent_task_events
		WHERE task_id = ?
		ORDER BY event_index`, id)
	if err != nil {
		return nil, fmt.Errorf("assignment: read the history: %w", err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		var (
			e       Event
			epoch   sql.NullInt64
			payload string
		)
		if err := rows.Scan(
			&e.ID, &e.TaskID, &e.EventIndex, &e.EventType,
			&e.FromState, &e.ToState, &e.ActorAddress, &e.ActorKind,
			&epoch, &e.Note, &payload, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("assignment: read the history: %w", err)
		}
		if epoch.Valid {
			v := epoch.Int64
			e.ClaimEpoch = &v
		}
		e.Payload = unmarshalPayload(payload)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assignment: read the history: %w", err)
	}
	return out, nil
}
