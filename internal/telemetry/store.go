// internal/telemetry/store.go — opens telemetry.db and applies DDL.
//
// Owns the database file exclusively. Single shared *sql.DB (WAL mode,
// busy_timeout=5000ms). Per CLAUDE.md H-5 the substrate discipline:
// one connection, no application-level MaxOpenConns throttling. The
// collector is write-only and runs at low QPS, so a single connection
// is sufficient.

package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

const ddl = `
CREATE TABLE IF NOT EXISTS telemetry_invocation (
  invocation_id        TEXT PRIMARY KEY,
  parent_invocation_id TEXT,
  session_id           TEXT,
  framework            TEXT NOT NULL,
  framework_version    TEXT,
  provider TEXT NOT NULL,
  model                TEXT NOT NULL,
  model_revision       TEXT,

  started_at           INTEGER NOT NULL,
  completed_at         INTEGER NOT NULL,
  received_at          INTEGER NOT NULL,

  status               TEXT NOT NULL,
  stop_reason          TEXT,

  input_tokens         INTEGER,
  output_tokens        INTEGER,
  cache_read_tokens    INTEGER,
  cache_write_tokens   INTEGER,
  reasoning_tokens     INTEGER,

  duration_ms          INTEGER,
  provider_metadata    TEXT NOT NULL DEFAULT '{}',
  schema_version       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tel_session     ON telemetry_invocation(session_id);
CREATE INDEX IF NOT EXISTS idx_tel_parent_inv  ON telemetry_invocation(parent_invocation_id);
CREATE INDEX IF NOT EXISTS idx_tel_started_at  ON telemetry_invocation(started_at);
CREATE INDEX IF NOT EXISTS idx_tel_framework   ON telemetry_invocation(framework);
`

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=wal&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(ddl); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply ddl: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// ErrStoreNotFound reports that a telemetry store does not exist at the
// requested path. Callers distinguish it from other open failures so a
// missing store can be handled as "no observations" rather than as a
// hard error.
var ErrStoreNotFound = errors.New("telemetry store not found")

// OpenReadOnly opens an EXISTING telemetry store for reading only.
//
// WHY THIS EXISTS. `Open` applies the DDL, which CREATES the database
// file when it is missing and can write to it otherwise. A read-only
// diagnostic must not materialise a store:
//
//   - `mpm-telemetry observe --dry-run` used to call `Open`, so a
//     "dry run" on a machine with no telemetry yet CREATED an empty
//     telemetry.db. A dry run that writes is not a dry run.
//   - `mpm-critic --telemetry-hunt` (#138) is documented as a READ-ONLY
//     diagnostic, but it also called `Open` — so every critic cycle
//     created a telemetry.db at the path it was looking for.
//
// The handle is opened with SQLite's `mode=ro`, so the file can be
// neither created nor modified, and a missing store is reported as
// ErrStoreNotFound instead of being materialised.
func OpenReadOnly(path string) (*Store, error) {
	clean := filepath.Clean(path)

	info, err := os.Stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrStoreNotFound, clean)
		}
		return nil, fmt.Errorf("stat telemetry store %s: %w", clean, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("telemetry store %s is a directory, not a database", clean)
	}

	abs, err := filepath.Abs(clean)
	if err != nil {
		return nil, fmt.Errorf("resolve telemetry store %s: %w", clean, err)
	}
	uri := (&url.URL{
		Scheme:   "file",
		Path:     abs,
		RawQuery: "mode=ro&_busy_timeout=5000",
	}).String()

	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	// Force the connection open and prove the schema is present. A
	// store whose ledger table is missing is a clear error here rather
	// than a confusing "no such table" from the middle of the hunt.
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM telemetry_invocation").Scan(&rows); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read telemetry store %s: %w", clean, err)
	}
	return &Store{db: db, path: clean}, nil
}

func (s *Store) DB() *sql.DB     { return s.db }
func (s *Store) Path() string   { return s.path }
func (s *Store) Close() error   { return s.db.Close() }

// QueryInvocation retrieves a single frame by invocation ID.
// Uses a transaction and delegates to loadFrameByID for consistency
// with the InsertFrame conflict-detection path.
func (s *Store) QueryInvocation(ctx context.Context, id string) (Frame, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Frame{}, err
	}
	defer tx.Rollback()
	return loadFrameByID(ctx, tx, id)
}

// QuerySession returns all frames for a given session ID, ordered by started_at.
func (s *Store) QuerySession(ctx context.Context, sessionID string) ([]Frame, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT invocation_id FROM telemetry_invocation
		WHERE session_id = ?
		ORDER BY started_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.loadMany(ctx, ids)
}

// QuerySince returns all frames with started_at >= cutoffSec, ordered by started_at.
func (s *Store) QuerySince(ctx context.Context, cutoffSec int64) ([]Frame, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT invocation_id FROM telemetry_invocation
		WHERE started_at >= ?
		ORDER BY started_at ASC`, cutoffSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.loadMany(ctx, ids)
}

// loadMany resolves a list of invocation IDs to Frames by calling QueryInvocation
// for each. N+1 pattern is acceptable for v1 (writes are infrequent, reads are infrequent).
func (s *Store) loadMany(ctx context.Context, ids []string) ([]Frame, error) {
	out := make([]Frame, 0, len(ids))
	for _, id := range ids {
		f, err := s.QueryInvocation(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
