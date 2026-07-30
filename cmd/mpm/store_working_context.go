// cmd/mpm/stores/working_context_store.go — Stores layer for Wave 1.
//
// The Store layer is PURE data access. It owns queries against SQLite
// (the `ephemeral_scratchpad` table for working context) and nothing
// else. Behaviour — validation, expiry policy, cross-cutting promote
// logic — lives in the Services layer above.
//
// Layering contract per the cognitive-interface RFC (v0.4):
//   SQLite → Stores → Services → Formatters → Encoders → Renderers → Commands
//
// A service that wraps a store without owning behaviour is accidental
// complexity (RFC §7). This Store is allowed to be thin because the
// WorkingContextService above it owns real behaviour (expiry policy,
// promote-crosses-tables, clear-with-policy).
//
// Implementation note: this Store adopts the existing SQLite schema
// (`ephemeral_scratchpad`) used by the `flush_scratchpad`,
// `read_scratchpad`, `discard_scratchpad`, and `promote_scratchpad`
// MCP tools. We do not change the schema here; the Store is a typed
// wrapper over the same DB rows the MCP path already uses.

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ErrWorkingContextNotFound is returned by WorkingContextStore.Load when
// no row exists for the given session_id. Distinct from sql.ErrNoRows so
// callers don't have to import database/sql.
var ErrWorkingContextNotFound = errors.New("working context not found for session")

// WorkingContext is the domain model the Store handles. Pure data —
// no behaviour, no methods beyond accessors. Behaviour lives in
// WorkingContextService.
//
// Timestamp fields are stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1).
type WorkingContext struct {
	SessionID  string // primary key into ephemeral_scratchpad
	Thesis     string // one-line summary the agent writes
	Supporting string // raw JSON or structured context dump
	UpdatedAt  int64  // last write timestamp (Unix-epoch seconds)
	ExpiresAt  int64  // TTL boundary; service enforces this
}

// IsExpired reports whether the working context is past its expiry.
// Pure predicate on the model — does NOT mutate state.
func (wc *WorkingContext) IsExpired(now time.Time) bool {
	if wc == nil {
		return false
	}
	return wc.ExpiresAt > 0 && now.After(time.Unix(wc.ExpiresAt, 0))
}

// WorkingContextStore is the persistence boundary for ephemeral
// working context. Constructed once via NewWorkingContextStore and
// shared across handlers / services.
type WorkingContextStore struct {
	dm *mpminternal.DatabaseManager
}

// NewWorkingContextStore wires the Store to the shared DatabaseManager.
// Returns nil if dm is nil — callers check.
func NewWorkingContextStore(dm *mpminternal.DatabaseManager) *WorkingContextStore {
	if dm == nil {
		return nil
	}
	return &WorkingContextStore{dm: dm}
}

// Load fetches the working context for a session. Returns
// ErrWorkingContextNotFound when no row exists.
//
// Note: this Store does NOT enforce expiry; that's the Service's job.
// A Store that applied business rules would couple persistence to
// behaviour, exactly the layering violation the RFC forbids.
func (s *WorkingContextStore) Load(sessionID string) (*WorkingContext, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	var wc WorkingContext
	var updatedAt, expiresAt sql.NullInt64
	row := s.dm.QueryRowTracked(
		`SELECT session_id, thesis, supporting, updated_at, decay_at
		 FROM ephemeral_scratchpad
		 WHERE session_id = ?`,
		sessionID,
	)
	if err := row.Scan(&wc.SessionID, &wc.Thesis, &wc.Supporting, &updatedAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWorkingContextNotFound
		}
		return nil, fmt.Errorf("working context load: %w", err)
	}
	if updatedAt.Valid {
		wc.UpdatedAt = updatedAt.Int64
	}
	if expiresAt.Valid {
		wc.ExpiresAt = expiresAt.Int64
	}
	return &wc, nil
}

// Save inserts or updates a working context row. ON CONFLICT semantics
// keep the same row id per session_id (exactly one context per session).
func (s *WorkingContextStore) Save(wc *WorkingContext) error {
	if wc == nil {
		return fmt.Errorf("working context is nil")
	}
	if wc.SessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	if wc.Thesis == "" {
		return fmt.Errorf("thesis is required")
	}
	// decay_at = 24h ahead from now if unset (matches flush_scratchpad default).
	expiresAt := wc.ExpiresAt
	if expiresAt == 0 {
		expiresAt = time.Now().UTC().Add(24 * time.Hour).Unix()
	}
	_, err := s.dm.ExecTracked(`
		INSERT INTO ephemeral_scratchpad (session_id, thesis, supporting, decay_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			thesis = excluded.thesis,
			supporting = excluded.supporting,
			updated_at = CAST(strftime('%s','now') AS INTEGER),
			decay_at = excluded.decay_at;`,
		0, // retries=0 → standard single attempt, no backoff loop
		wc.SessionID, wc.Thesis, wc.Supporting,
		expiresAt)
	if err != nil {
		return fmt.Errorf("working context save: %w", err)
	}
	return nil
}

// Delete hard-deletes the working context row for a session.
// Idempotent: deleting a non-existent row is a no-op.
func (s *WorkingContextStore) Delete(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	_, err := s.dm.ExecTracked(
		`DELETE FROM ephemeral_scratchpad WHERE session_id = ?`,
		0,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("working context delete: %w", err)
	}
	return nil
}

// parseSQLiteTime parses the SQLite CURRENT_TIMESTAMP format used
// throughout the schema. Empty string → zero time + parse error so
// callers can branch (typically == "no expiry set").
func parseSQLiteTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty sqlite timestamp")
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}
