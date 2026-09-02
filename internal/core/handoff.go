package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Handoff is a structured end-of-session record. The next session pulls the
// most recent unread handoff from wake context and uses it to continue work
// across the restart boundary. Handoffs are bootstrap data, not content —
// the dormant `sessions` table still holds content snapshots for those who
// want to keep full session logs.
//
// Timestamp fields are stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1). Display layer callers format at the boundary via
// FormatUnixSeconds / FormatOptionalUnixSeconds.
type Handoff struct {
	ID            string  `json:"id"`
	SessionID     string  `json:"session_id"`
	EndedAt       int64   `json:"ended_at"`
	EndedState    string  `json:"ended_state"`
	Summary       string  `json:"summary"`
	Commitments   []string `json:"commitments"`
	OpenQuestions []string `json:"open_questions"`
	ReadAt        *int64  `json:"read_at,omitempty"`
	ReadBy        string  `json:"read_by,omitempty"`
	CreatedAt     int64   `json:"created_at"`
}

// EndedState values — kept in sync with the CHECK constraint in schema.go.
const (
	HandoffClean       = "clean"       // normal end, all state written
	HandoffCrashed     = "crashed"     // unexpected exit, partial state
	HandoffInterrupted = "interrupted" // user closed session
	HandoffForceEnd    = "force_end"   // agent explicitly force-quit
)

// EndSession writes a handoff for the just-ended session. Safe to call
// after the agent has done its work. session_id is OPTIONAL — callers
// without an external session identifier (e.g. Claude Code, which boots
// without `MPM_SESSION_ID` and has no native UUID) can pass an empty
// string; the handoff is still preserved. The durable identity of the
// handoff is the MPM-generated `id` and `created_at`; session_id is
// opaque correlation metadata only.
//
// IDENTITY MODEL (2026-09-01 — external session IDs are optional):
//   - id:            MPM-generated PRIMARY KEY via GenerateID(). Stable
//                    across upserts; the canonical handle for any later
//                    reference (wake context, logs, foreign keys).
//   - created_at:    MPM-generated Unix-epoch timestamp at write time.
//                    Moves forward on every UPSERT so a stale row's age
//                    reflects the latest closeout.
//   - session_id:    OPTIONAL external identifier. May be a UUID, a
//                    numeric framework id, a framework-tagged string
//                    (e.g. "claude-code-2026-09-01"), or empty (stored
//                    as SQL NULL).
//
// UPSERT SEMANTICS (added 2026-06-26, see decision
// be61de1c4ef2ff4a-adjacent fix): session_handoffs.session_id is UNIQUE
// (NULL-distinct — see sqlite.org/lang_createtable §3). When the caller
// supplies a non-empty session_id, this is an UPSERT keyed on
// session_id: last writer wins. If a row already exists for this
// session_id, every column is overwritten in place. The id is preserved
// (existing id returned) so external references stay stable. The
// created_at column moves forward.
//
// When the caller supplies an EMPTY session_id, this is a plain INSERT
// (no UPSERT): the new row gets a fresh MPM-generated id and the
// session_id column is stored as SQL NULL. Multiple such rows coexist
// (each NULL is distinct from every other value under the UNIQUE
// constraint). This is the recovery shape for callers that boot without
// any external session identifier.
//
// Returns the persisted Handoff (with ID, timestamps populated) and a log
// row to the audit ledger on real error. If the DB is nil or the upsert
// fails, returns an error — but never panics. Callers should treat
// EndSession as best-effort: log the error, move on.
func (dm *DatabaseManager) EndSession(sessionID, summary, endedState string, commitments, openQuestions []string) (*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("EndSession: db not initialized")
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

	now := time.Now().UTC().Unix()
	newID := GenerateID()

	// Two write paths depending on whether the caller supplied an
	// external session identifier:
	//
	//   - non-empty: ON CONFLICT(session_id) DO UPDATE — UPSERT. The
	//     id is preserved across upserts (existing row's id is
	//     returned, not the freshly generated one) so external
	//     references stay valid. created_at moves forward on every
	//     upsert so the row's age reflects the latest closeout.
	//     read_at + read_by reset to NULL on every upsert — a handoff
	//     with new content is, by definition, unread. (Bug surfaced
	//     2026-07-06: 12 days of agent:main:main closeouts invisible
	//     to wake because the read_at from the row's first incarnation
	//     carried over across UPSERTs.)
	//
	//   - empty:     plain INSERT. Stored as SQL NULL on the
	//     session_id column. The freshly generated id is the row's
	//     durable identity; multiple NULL rows coexist because SQLite
	//     treats each NULL as distinct from every other value under
	//     the UNIQUE constraint.
	if sessionID != "" {
		_, err = dm.db.Exec(`
			INSERT INTO session_handoffs
				(id, session_id, ended_at, ended_state, summary, commitments, open_questions, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				id             = session_handoffs.id,
				ended_at       = excluded.ended_at,
				ended_state    = excluded.ended_state,
				summary        = excluded.summary,
				commitments    = excluded.commitments,
				open_questions = excluded.open_questions,
				created_at     = excluded.created_at,
				read_at        = NULL,
				read_by        = NULL`,
			newID, sessionID, now, endedState, summary,
			string(commitJSON), string(questionJSON), now,
		)
		if err != nil {
			// Audit the failure — meta-error: even the handoff writer failed.
			// The UNIQUE-constraint path no longer fires here (upsert handles
			// it), so any error from this Exec is a real problem: DB locked,
			// disk full, schema mismatch, etc.
			dm.LogAudit(AuditWarn, "handoff", "EndSession upsert failed: "+err.Error(), "", AuditContext{
				"session_id": sessionID,
			})
			return nil, fmt.Errorf("EndSession: upsert: %w", err)
		}
	} else {
		_, err = dm.db.Exec(`
			INSERT INTO session_handoffs
				(id, session_id, ended_at, ended_state, summary, commitments, open_questions, created_at)
			VALUES (?, NULL, ?, ?, ?, ?, ?, ?)`,
			newID, now, endedState, summary,
			string(commitJSON), string(questionJSON), now,
		)
		if err != nil {
			dm.LogAudit(AuditWarn, "handoff", "EndSession insert (no session_id) failed: "+err.Error(), "", AuditContext{})
			return nil, fmt.Errorf("EndSession: insert (no session_id): %w", err)
		}
	}

	// No audit log on the success path. The session_handoffs row IS the
	// audit trail for session endings — logging the same event to
	// system_audit_log too was doubling the noise without adding signal.
	// (Was: AuditWarn with full summary. Removed 2026-06-23. The upsert
	// continues this principle: silent on success, loud on real error.)

	// Read-back assertion (Defense Triad #3) proves persistence to the
	// substrate. Returns the canonical id (the one that survived the
	// upsert, or the freshly generated id on the no-session-id path).
	if sessionID != "" {
		persisted, err := dm.GetHandoffBySessionID(sessionID)
		if err != nil {
			return nil, fmt.Errorf("EndSession: read back: %w", err)
		}
		return persisted, nil
	}
	persisted, err := dm.GetHandoffByID(newID)
	if err != nil {
		return nil, fmt.Errorf("EndSession: read back by id: %w", err)
	}
	return persisted, nil
}

// GetHandoffBySessionID returns the handoff row for a session. Returns
// sql.ErrNoRows if none exists. Promoted from getHandoffBySessionID
// (private) when shred_handoff was added — callers that hold only a
// session identifier (the common case for tests, plugin shutdown
// hooks, and integration smoke scripts) need a public lookup path
// before shredding. The handoff id is required for DeleteHandoff, so
// callers that have only session_id must round-trip through here.
//
// Empty sessionID returns sql.ErrNoRows without hitting the database.
// The UNIQUE-on-session_id column allows multiple NULLs; rows stored
// with NULL session_id are not addressable via this lookup — callers
// wanting those should use GetHandoffByID or ListHandoffs. The early
// return keeps the SQL semantics clean (WHERE session_id = '' would
// not match NULL rows anyway, but the explicit ErrNoRows short-circuit
// avoids a query that is guaranteed to return zero rows).
//
// Timestamp fields are stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1). The driver scans them directly into int64.
func (dm *DatabaseManager) GetHandoffBySessionID(sessionID string) (*Handoff, error) {
	if sessionID == "" {
		return nil, sql.ErrNoRows
	}
	row := dm.db.QueryRow(`
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, created_at
		FROM session_handoffs WHERE session_id = ?`, sessionID)
	var h Handoff
	var commitJSON, questionJSON []byte
	if err := row.Scan(&h.ID, &h.SessionID, &h.EndedAt, &h.EndedState, &h.Summary,
		&commitJSON, &questionJSON, &h.CreatedAt); err != nil {
		return nil, err
	}
	if len(commitJSON) > 0 {
		_ = json.Unmarshal(commitJSON, &h.Commitments)
	}
	if len(questionJSON) > 0 {
		_ = json.Unmarshal(questionJSON, &h.OpenQuestions)
	}
	if h.Commitments == nil {
		h.Commitments = []string{}
	}
	if h.OpenQuestions == nil {
		h.OpenQuestions = []string{}
	}
	return &h, nil
}

// GetLatestUnreadHandoff returns the most recent handoff that has not been
// marked as read. Returns sql.ErrNoRows if no unread handoffs exist. Used
// by wake context to surface the previous session's handoff.
//
// Tie-breaker: ORDER BY ended_at DESC, rowid DESC — SQLite INTEGER
// Unix-epoch resolves to the second, so multiple handoffs inserted in
// the same second need a secondary sort. rowid DESC is strictly
// monotonic on INSERT and captures true chronological insertion order;
// id DESC would NOT suffice because GenerateID() produces SHA256
// prefixes which are not chronologically sortable.
// MPM-BUG-HANDOFF-SAME-SECOND-TIE-2026-08-27.
func (dm *DatabaseManager) GetLatestUnreadHandoff() (*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("GetLatestUnreadHandoff: db not initialized")
	}
	row := dm.db.QueryRow(`
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at
		FROM session_handoffs
		WHERE read_at IS NULL
		ORDER BY ended_at DESC, rowid DESC
		LIMIT 1`)
	return scanHandoff(row)
}

// GetLatestHandoff returns the most recent handoff regardless of read state.
// Used by `mpm call session_handoff` when the agent wants to see the latest
// even if it was already read. Same tie-breaker as GetLatestUnreadHandoff.
func (dm *DatabaseManager) GetLatestHandoff() (*Handoff, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("GetLatestHandoff: db not initialized")
	}
	row := dm.db.QueryRow(`
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at
		FROM session_handoffs
		ORDER BY ended_at DESC, rowid DESC
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
	now := time.Now().UTC().Unix()
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
	now := time.Now().UTC().Unix()
	h.ReadAt = &now
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
	query += ` ORDER BY ended_at DESC, rowid DESC LIMIT ?`

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
			slog.Warn("ListHandoffs: scan error, skipping row", "error", err.Error())
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
		WHERE created_at < CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER)`, retentionDays)
	if err != nil {
		return 0, fmt.Errorf("PruneHandoffs: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// DeleteHandoff removes a single handoff by id. Returns
// (rowsAffected, error) so callers can distinguish "not found" (0 rows,
// no error) from a real DB failure. Use session_id lookup first when
// the caller holds the session identifier but not the handoff id.
//
// Handoffs are bootstrap data: destroying one removes the previous
// session's commitments, open questions, and ended-state from wake
// context permanently. The destruction is loud (AuditWarn) because it
// is irreversible and there is no recycle bin — operators reading the
// audit ledger should see exactly when a handoff was removed and why.
//
// Closes MPM-GAP-SHRED-HANDOFF-2026-08-19: before this method,
// the legacy `mpm_session` tool had no shred path for handoffs
// (it was later split into `mpm_handoff` / `mpm_scratchpad`; shred
// is the `mpm_handoff` action) and tests had to bypass the supported
// interface (direct SQL) to clean up after themselves.
func (dm *DatabaseManager) DeleteHandoff(id string) (int64, error) {
	if dm == nil || dm.db == nil {
		return 0, fmt.Errorf("DeleteHandoff: db not initialized")
	}
	if id == "" {
		return 0, fmt.Errorf("DeleteHandoff: id is required")
	}
	result, err := dm.db.Exec(`DELETE FROM session_handoffs WHERE id = ?`, id)
	if err != nil {
		dm.LogAudit(AuditWarn, "handoff", "DeleteHandoff failed: "+err.Error(), "", AuditContext{
			"handoff_id": id,
		})
		return 0, fmt.Errorf("DeleteHandoff: %w", err)
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		dm.LogAudit(AuditWarn, "handoff", "DeleteHandoff shredded handoff row", "", AuditContext{
			"handoff_id": id,
		})
	}
	return n, nil
}

// --- helpers ---

// scanHandoff reads one row from a *sql.Row into a Handoff. Handles JSON
// unmarshal of commitments and open_questions. Returns sql.ErrNoRows
// unchanged so callers can detect missing-handoff cleanly.
//
// Timestamp fields are stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1). read_at is nullable → sql.NullInt64.
// session_id is nullable (external session identifiers are optional
// — see EndSession for the contract); rows stored with NULL session_id
// surface to callers as Handoff.SessionID == "".
func scanHandoff(row *sql.Row) (*Handoff, error) {
	var (
		h            Handoff
		sessionID    sql.NullString
		commitJSON   string
		questionJSON string
		readAt       sql.NullInt64
		readBy       sql.NullString
	)
	err := row.Scan(
		&h.ID, &sessionID, &h.EndedAt, &h.EndedState, &h.Summary,
		&commitJSON, &questionJSON, &readAt, &readBy, &h.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if sessionID.Valid {
		h.SessionID = sessionID.String
	}
	if readAt.Valid {
		v := readAt.Int64
		h.ReadAt = &v
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
		sessionID    sql.NullString
		commitJSON   string
		questionJSON string
		readAt       sql.NullInt64
		readBy       sql.NullString
	)
	err := rows.Scan(
		&h.ID, &sessionID, &h.EndedAt, &h.EndedState, &h.Summary,
		&commitJSON, &questionJSON, &readAt, &readBy, &h.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if sessionID.Valid {
		h.SessionID = sessionID.String
	}
	if readAt.Valid {
		v := readAt.Int64
		h.ReadAt = &v
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
//
// Deprecated: handoff scan paths now read INTEGER directly via the driver.
// parseSQLiteTime survives only because callers in other packages (wake
// context, scratchpad) still feed timestamp strings from raw SQL — drop
// when those go away.
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
