// internal/telemetry/store.go — opens telemetry.db and applies DDL.
//
// Owns the database file exclusively. Single shared *sql.DB (WAL mode,
// busy_timeout=5000ms). Per CLAUDE.md H-5 the substrate discipline:
// one connection, no application-level MaxOpenConns throttling. The
// collector is write-only and runs at low QPS, so a single connection
// is sufficient.

package telemetry

import (
	"database/sql"
	"fmt"

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

func (s *Store) DB() *sql.DB     { return s.db }
func (s *Store) Path() string   { return s.path }
func (s *Store) Close() error   { return s.db.Close() }
