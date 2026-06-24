package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Handoff is a structured end-of-session record. The next session pulls the
// most recent unread handoff from wake context and uses it to continue work
// across the restart boundary. Handoffs are bootstrap data, not content —
// the dormant `sessions` table still holds content snapshots for those who
// want to keep full session logs.
type Handoff struct {
	ID            string     `json:"id"`
	SessionID     string     `json:"session_id"`
	EndedAt       time.Time  `json:"ended_at"`
	EndedState    string     `json:"ended_state"`
	Summary       string     `json:"summary"`
	Commitments   []string   `json:"commitments"`
	OpenQuestions []string   `json:"open_questions"`
	ReadAt        *time.Time `json:"read_at,omitempty"`
	ReadBy        string     `json:"read_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// EndedState values — kept in sync with the CHECK constraint in schema.go.
const (
	HandoffClean       = "clean"        // normal end, all state written
	HandoffCrashed     = "crashed"      // unexpected exit, partial state
	HandoffInterrupted = "interrupted"  // user closed session
	HandoffForceEnd    = "force_end"    // agent explicitly force-quit
)

// EndSession writes a handoff for the just-ended session. Safe to call
// after the agent has done its work. The session_id is opaque — usually a
// UUID the agent generates, but anything unique works.
//
// Returns the persisted Handoff (with ID, timestamps populated) and a log
// row to the audit ledger. If the DB is nil or the insert fails, returns
// an error — but never panics. Callers should treat EndSession as
// best-effort: log the error, move on.
func (dm *DatabaseManager) EndSession(sessionID, summary, endedState string, commitments, openQuestions []string) (*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("EndSession: db not initialized")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("EndSession: session_id is required")
	}
	if summary == "" {
		return nil, fmt.Errorf("EndSession: summary is required")
	}
	if endedState == "" {
		endedState = HandoffClean
	}

	// Normalize: ensure non-nil slices so JSON encoding produces "[]" not "null".
	if commitments == nil {
		commitments = []string{}
	}
	if openQuestions == nil {
		openQuestions = []string{}
	}

	commitJSON, err := json.Marshal(commitments)
	if err != nil {
		return nil, fmt.Errorf("EndSession: marshal commitments: %w", err)
	}
	questionJSON, err := json.Marshal(openQuestions)
	if err != nil {
		return nil, fmt.Errorf("EndSession: marshal open_questions: %w", err)
	}

	now := time.Now().UTC()
	id := GenerateID()

	_, err = dm.db.Exec(`
		INSERT INTO session_handoffs
			(id, session_id, ended_at, ended_state, summary, commitments, open_questions, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, now.Format("2006-01-02 15:04:05"), endedState, summary,
		string(commitJSON), string(questionJSON), now.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		// Audit the failure — meta-error: even the handoff writer failed.
		// The UNIQUE constraint case here is schema enforcement (caller
		// wrote a handoff for the same session_id already), which is
		// expected behavior under the Handoff Protocol's mid-session
		// trigger-phrase rule. Log as warn, not error — the system is
		// working as designed; the caller was overly eager.
		dm.LogAudit(AuditWarn, "handoff", "EndSession insert failed: "+err.Error(), "", AuditContext{
			"session_id": sessionID,
		})
		return nil, fmt.Errorf("EndSession: insert: %w", err)
	}

	// No audit log on the success path. The session_handoffs row IS the
	// audit trail for session endings — logging the same event to
	// system_audit_log too was doubling the noise without adding signal.
	// (Was: AuditWarn with full summary. Removed 2026-06-23.)

	return &Handoff{
		ID:            id,
		SessionID:     sessionID,
		EndedAt:       now,
		EndedState:    endedState,
		Summary:       summary,
		Commitments:   commitments,
		OpenQuestions: openQuestions,
		CreatedAt:     now,
	}, nil
}

// GetLatestUnreadHandoff returns the most recent handoff that has not been
// marked as read. Returns sql.ErrNoRows if no unread handoffs exist. Used
// by wake context to surface the previous session's handoff.
func (dm *DatabaseManager) GetLatestUnreadHandoff() (*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("GetLatestUnreadHandoff: db not initialized")
	}
	row := dm.db.QueryRow(`
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at
		FROM session_handoffs
		WHERE read_at IS NULL
		ORDER BY ended_at DESC
		LIMIT 1`)
	return scanHandoff(row)
}

// GetLatestHandoff returns the most recent handoff regardless of read state.
// Used by `mpm call session_handoff` when the agent wants to see the latest
// even if it was already read.
func (dm *DatabaseManager) GetLatestHandoff() (*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("GetLatestHandoff: db not initialized")
	}
	row := dm.db.QueryRow(`
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at
		FROM session_handoffs
		ORDER BY ended_at DESC
		LIMIT 1`)
	return scanHandoff(row)
}

// GetHandoffByID returns a specific handoff. Useful for re-reading or
// historical context.
func (dm *DatabaseManager) GetHandoffByID(id string) (*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("GetHandoffByID: db not initialized")
	}
	row := dm.db.QueryRow(`
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at
		FROM session_handoffs
		WHERE id = ?`, id)
	return scanHandoff(row)
}

// MarkHandoffRead marks a handoff as read. `readBy` is an opaque token
// identifying the reader (e.g. a wake context call timestamp, or the next
// session_id). Idempotent — calling on an already-read handoff is a no-op.
func (dm *DatabaseManager) MarkHandoffRead(id, readBy string) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("MarkHandoffRead: db not initialized")
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := dm.db.Exec(`
		UPDATE session_handoffs
		SET read_at = ?, read_by = ?
		WHERE id = ? AND read_at IS NULL`, now, readBy, id)
	if err != nil {
		return fmt.Errorf("MarkHandoffRead: %w", err)
	}
	return nil
}

// MarkLatestHandoffRead is a convenience for wake context: pulls the
// latest handoff (if any) and marks it read. Returns nil, nil if there's
// no handoff to mark.
func (dm *DatabaseManager) MarkLatestHandoffRead(readBy string) (*Handoff, error) {
	h, err := dm.GetLatestUnreadHandoff()
	if err != nil {
		return nil, err // includes sql.ErrNoRows
	}
	if err := dm.MarkHandoffRead(h.ID, readBy); err != nil {
		return h, fmt.Errorf("mark read: %w", err)
	}
	h.ReadAt = ptrTime(time.Now().UTC())
	h.ReadBy = readBy
	return h, nil
}

// ListHandoffs returns handoffs ordered newest first. If `unreadOnly` is
// true, filters to those not yet read.
func (dm *DatabaseManager) ListHandoffs(limit int, unreadOnly bool) ([]*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("ListHandoffs: db not initialized")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query := `
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at
		FROM session_handoffs`
	if unreadOnly {
		query += ` WHERE read_at IS NULL`
	}
	query += ` ORDER BY ended_at DESC LIMIT ?`

	rows, err := dm.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("ListHandoffs: %w", err)
	}
	defer rows.Close()

	var out []*Handoff
	for rows.Next() {
		h, err := scanHandoffRows(rows)
		if err != nil {
			// Skip individual scan errors but keep going.
			fmt.Fprintf(os.Stderr, "ListHandoffs: scan err: %v\n", err)
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// PruneHandoffs deletes handoffs older than `retentionDays`. The 90-day
// default is enforced by gc. Returns the number of rows deleted.
func (dm *DatabaseManager) PruneHandoffs(retentionDays int) (int64, error) {
	if dm == nil || dm.db == nil {
		return 0, fmt.Errorf("PruneHandoffs: db not initialized")
	}
	if retentionDays < 1 {
		retentionDays = 90
	}
	result, err := dm.db.Exec(`
		DELETE FROM session_handoffs
		WHERE created_at < datetime('now', '-' || ? || ' days')`, retentionDays)
	if err != nil {
		return 0, fmt.Errorf("PruneHandoffs: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// --- helpers ---

// scanHandoff reads one row from a *sql.Row into a Handoff. Handles JSON
// unmarshal of commitments and open_questions. Returns sql.ErrNoRows
// unchanged so callers can detect missing-handoff cleanly.
func scanHandoff(row *sql.Row) (*Handoff, error) {
	var (
		h            Handoff
		endedAt      string
		commitJSON   string
		questionJSON string
		readAt       sql.NullString
		readBy       sql.NullString
		createdAt    string
	)
	err := row.Scan(
		&h.ID, &h.SessionID, &endedAt, &h.EndedState, &h.Summary,
		&commitJSON, &questionJSON, &readAt, &readBy, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	if t, perr := parseSQLiteTime(endedAt); perr == nil {
		h.EndedAt = t
	}
	if t, perr := parseSQLiteTime(createdAt); perr == nil {
		h.CreatedAt = t
	}
	if readAt.Valid {
		if t, perr := parseSQLiteTime(readAt.String); perr == nil {
			h.ReadAt = &t
		}
	}
	if readBy.Valid {
		h.ReadBy = readBy.String
	}
	if err := json.Unmarshal([]byte(commitJSON), &h.Commitments); err != nil {
		h.Commitments = []string{}
	}
	if err := json.Unmarshal([]byte(questionJSON), &h.OpenQuestions); err != nil {
		h.OpenQuestions = []string{}
	}
	return &h, nil
}

// scanHandoffRows is the *sql.Rows variant for ListHandoffs.
func scanHandoffRows(rows *sql.Rows) (*Handoff, error) {
	var (
		h            Handoff
		endedAt      string
		commitJSON   string
		questionJSON string
		readAt       sql.NullString
		readBy       sql.NullString
		createdAt    string
	)
	err := rows.Scan(
		&h.ID, &h.SessionID, &endedAt, &h.EndedState, &h.Summary,
		&commitJSON, &questionJSON, &readAt, &readBy, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	if t, perr := parseSQLiteTime(endedAt); perr == nil {
		h.EndedAt = t
	}
	if t, perr := parseSQLiteTime(createdAt); perr == nil {
		h.CreatedAt = t
	}
	if readAt.Valid {
		if t, perr := parseSQLiteTime(readAt.String); perr == nil {
			h.ReadAt = &t
		}
	}
	if readBy.Valid {
		h.ReadBy = readBy.String
	}
	if err := json.Unmarshal([]byte(commitJSON), &h.Commitments); err != nil {
		h.Commitments = []string{}
	}
	if err := json.Unmarshal([]byte(questionJSON), &h.OpenQuestions); err != nil {
		h.OpenQuestions = []string{}
	}
	return &h, nil
}

// parseSQLiteTime handles both "YYYY-MM-DD HH:MM:SS" (DATETIME format) and
// "YYYY-MM-DDTHH:MM:SSZ" (RFC3339) since both may appear in old rows from
// the dormant sessions table or hand-written test data.
func parseSQLiteTime(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("parseSQLiteTime: no layout matched %q", s)
}

// ptrTime returns a pointer to a time.Time (used for nullable time fields
// in JSON output).
func ptrTime(t time.Time) *time.Time {
	return &t
}
