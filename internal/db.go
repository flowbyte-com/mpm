// db.go - Unified SQLite database for MPM
// Version: 2026-03-28 (Single Database Refactor)
// Description: Manages a single unified database at src/db/mpm.db

package internal

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mpm/internal/config"

	_ "github.com/mattn/go-sqlite3"
)

func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// dbFileName is the canonical filename for the MPM database.
// Previously mpm_memory.db - renamed 2026-04-01 to reflect its unified nature.
const dbFileName = "mpm.db"

// SQLiteConnection is a wrapper for sql.DB for backwards compatibility with existing code
type SQLiteConnection struct {
	*sql.DB
}

// NewSQLiteConnection opens a new SQLite database connection (for backwards compatibility)
func NewSQLiteConnection(dbPath string) (*SQLiteConnection, error) {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.Exec("PRAGMA foreign_keys = ON")
	db.Exec("PRAGMA journal_mode = WAL")
	// Wait up to 5 seconds for locks to clear before returning SQLITE_BUSY.
	// This handles concurrent GC + Add without immediate failure.
	db.Exec("PRAGMA busy_timeout = 5000")
	return &SQLiteConnection{DB: db}, nil
}

// WipeTableNames is the allowlist of tables that can be wiped/deleted
var WipeTableNames = map[string]string{
	"sessions": "sessions",
	"memories": "memories",
	"topics":   "topics",
}

// WipeRecord performs a hard delete with VACUUM (on SQLiteConnection for backwards compatibility)
func (sc *SQLiteConnection) WipeRecord(tier, id string) error {
	tableName := WipeTableNames[tier]
	if tableName == "" {
		return fmt.Errorf("cannot wipe from tier: %s", tier)
	}
	tx, err := sc.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec("DELETE FROM "+tableName+" WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = sc.DB.Exec("VACUUM")
	return err
}

// ShredDatabase securely wipes the entire database and recreates it fresh.
// NOTE: This only works if there are no other open connections to the database.
// Caller is responsible for ensuring all connections are closed before calling.
func ShredDatabase(dbPath string) error {
	// Close any connection we can open (best effort)
	// Note: This may not close connections held by other DatabaseManager/MemoryStore instances
	// Caller should ensure all connections are closed before calling this function.
	if db, err := NewSQLiteConnection(dbPath); err == nil {
		db.Close()
	}

	// Remove WAL and SHM files first (best effort)
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	os.Remove(dbPath + "-journal")

	// Overwrite with zeros before delete (secure delete simulation)
	if data, err := os.ReadFile(dbPath); err == nil {
		zeroed := make([]byte, len(data))
		os.WriteFile(dbPath, zeroed, 0600)
	}

	// Remove the database file
	if err := os.Remove(dbPath); err != nil {
		return fmt.Errorf("failed to remove database: %w", err)
	}

	// Recreate fresh database using MemoryStore
	store := NewMemoryStore("")
	store.SQLiteDBPath = dbPath
	if err := store.InitSQLite(); err != nil {
		return fmt.Errorf("failed to recreate database: %w", err)
	}
	store.DB.Close()

	return nil
}

// MarshalJSON is a helper for JSON marshaling (for backwards compatibility)
func MarshalJSON(v interface{}) (string, error) {
	bytes, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// UnmarshalJSON is a helper for JSON unmarshaling (for backwards compatibility)
func UnmarshalJSON(data string, v interface{}) error {
	return json.Unmarshal([]byte(data), v)
}

// ==================== DatabaseManager ====================

// DatabaseManager manages the single unified database
type DatabaseManager struct {
	db          *sql.DB
	dbPath      string       // stored for shareable store init
	sharedStore *MemoryStore // reused for self-maintenance; nil until first access

	watchdogPath string     // path to watchdog.jsonl for query observability
	watchdogMu   sync.Mutex // serializes watchdog log writes
}

const slowQueryThreshold = 100 * time.Millisecond // queries slower than this are logged as "slow"

// SQLDB returns the underlying *sql.DB for direct queries.
func (dm *DatabaseManager) SQLDB() *sql.DB {
	return dm.db
}

// IsOpen returns true if the database connection is non-nil.
func (dm *DatabaseManager) IsOpen() bool {
	return dm.db != nil
}

// ==================== Watchdog / Query Observability ====================

// watchdogOp represents a single operation entry written to watchdog.jsonl.
type watchdogOp struct {
	Timestamp  string `json:"timestamp"`
	Operation  string `json:"operation"`
	DurationMs int64  `json:"duration_ms"`
	Query      string `json:"query,omitempty"`
	Retries    int    `json:"retries,omitempty"`
	Error      string `json:"error,omitempty"`
	Slow       bool   `json:"slow"`
}

// logWatchdog appends a watchdog entry to watchdog.jsonl.
// File-write contention is serialised by watchdogMu so that concurrent
// DatabaseManager users do not corrupt the log.
func (dm *DatabaseManager) logWatchdog(entry watchdogOp) {
	if dm.watchdogPath == "" {
		return
	}
	dm.watchdogMu.Lock()
	defer dm.watchdogMu.Unlock()

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(dm.watchdogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
	f.Write([]byte("\n"))
}

// logWatchdogRaw appends raw JSON bytes to watchdog.jsonl under the dm mutex.
// Used by synthesis and other subsystems that want structured events with
// custom schemas rather than the watchdogOp query-timing format.
func (dm *DatabaseManager) logWatchdogRaw(line []byte) {
	if dm == nil || dm.watchdogPath == "" {
		return
	}
	dm.watchdogMu.Lock()
	defer dm.watchdogMu.Unlock()
	f, err := os.OpenFile(dm.watchdogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(line)
	f.Write([]byte("\n"))
}

// ExecTracked runs db.Exec with timing and optional retry-backoff.
// If retries > 0 the query is re-attempted on SQLITE_BUSY with exponential
// backoff (100ms, 200ms, 400ms, … capped at 5s).
func (dm *DatabaseManager) ExecTracked(query string, retries int, args ...interface{}) (sql.Result, error) {
	start := time.Now()
	attempts := 0
	backoff := 100 * time.Millisecond
	maxBackoff := 5 * time.Second

	for {
		attempts++
		result, err := dm.db.Exec(query, args...)
		elapsed := time.Since(start)

		if err != nil && isBusyError(err) && attempts <= retries {
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		entry := watchdogOp{
			Timestamp:  start.UTC().Format(time.RFC3339Nano),
			Operation:  "exec",
			DurationMs: elapsed.Milliseconds(),
			Query:      truncateQuery(query),
			Retries:    attempts - 1,
			Slow:       elapsed > slowQueryThreshold,
		}
		if err != nil {
			entry.Error = err.Error()
		}
		dm.logWatchdog(entry)

		return result, err
	}
}

// QueryTracked runs db.Query with timing.
func (dm *DatabaseManager) QueryTracked(query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	rows, err := dm.db.Query(query, args...)
	elapsed := time.Since(start)

	entry := watchdogOp{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		Operation:  "query",
		DurationMs: elapsed.Milliseconds(),
		Query:      truncateQuery(query),
		Slow:       elapsed > slowQueryThreshold,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	dm.logWatchdog(entry)

	return rows, err
}

// QueryRowTracked runs db.QueryRow with timing.
func (dm *DatabaseManager) QueryRowTracked(query string, args ...interface{}) *sql.Row {
	start := time.Now()
	row := dm.db.QueryRow(query, args...)
	elapsed := time.Since(start)

	entry := watchdogOp{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		Operation:  "queryrow",
		DurationMs: elapsed.Milliseconds(),
		Query:      truncateQuery(query),
		Slow:       elapsed > slowQueryThreshold,
	}
	// We cannot inspect the error without scanning the row, so we log
	// duration-only here. Callers that scan will see any sql.ErrNoRows etc.
	dm.logWatchdog(entry)

	return row
}

// isBusyError returns true when the error is an SQLITE_BUSY / database-locked.
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "cannot commit") && strings.Contains(msg, "locked")
}

// truncateQuery keeps only the first 120 characters of a query for watchdog
// log entries, preventing massive SQL strings from bloating the log.
func truncateQuery(q string) string {
	if len(q) > 120 {
		return q[:120] + "..."
	}
	return q
}

// WatchdogPath returns the path to the watchdog log file this manager writes to.
func (dm *DatabaseManager) WatchdogPath() string {
	return dm.watchdogPath
}

// ==================== Shared Store ====================

// getSharedStore returns a MemoryStore backed by dm.db, creating it once.
// This avoids redundant InitSQLite() calls in self-maintenance methods.
func (dm *DatabaseManager) getSharedStore() (*MemoryStore, error) {
	if dm.sharedStore != nil {
		return dm.sharedStore, nil
	}
	store := NewMemoryStore("")
	store.SQLiteDBPath = dm.dbPath
	store.DB = &SQLiteConnection{DB: dm.db}
	dm.sharedStore = store
	return store, nil
}

// NewDatabaseManager creates a new database manager with single unified database.
// The database is ALWAYS at mpm/src/db/mpm.db regardless of projectRoot.
// projectRoot is kept for API compatibility but is ignored for path resolution.
func NewDatabaseManager(projectRoot string) (*DatabaseManager, error) {
	mpmDir := config.GetMPMDir()
	dbDir := filepath.Join(mpmDir, "src", "db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	dbPath := filepath.Join(dbDir, dbFileName)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.Exec("PRAGMA foreign_keys = ON")
	db.Exec("PRAGMA journal_mode = WAL")
	db.Exec("PRAGMA synchronous = NORMAL")
	db.Exec("PRAGMA cache_size = -64000")

	manager := &DatabaseManager{
		db:           db,
		dbPath:       dbPath,
		watchdogPath: filepath.Join(filepath.Dir(dbPath), "watchdog.jsonl"),
	}

	if err := manager.initUnifiedSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return manager, nil
}

// NewDatabaseManagerForDB creates a DatabaseManager wrapping an existing *sql.DB.
// Use this for one-off CLI commands that don't need managed persistence.
func NewDatabaseManagerForDB(db *sql.DB) *DatabaseManager {
	wdPath := ""
	if mpmDir := config.GetMPMDir(); mpmDir != "" {
		wdPath = filepath.Join(mpmDir, "src", "db", "watchdog.jsonl")
	}
	return &DatabaseManager{db: db, watchdogPath: wdPath}
}

// InitSchema initializes the shared MPM schema (tables, indexes, migrations, FTS).
// Exported so test code can call it after NewDatabaseManagerForDB with a custom *sql.DB.
func (dm *DatabaseManager) InitSchema() error {
	return dm.initUnifiedSchema()
}

func (dm *DatabaseManager) initUnifiedSchema() error {
	// Use shared schema definitions from schema.go
	for _, sqlQuery := range BaseTables {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute SQL: %w\nSQL: %s", err, sqlQuery)
		}
	}

	// Migration: add new columns to existing databases (no-op if already present)
	// Use SafeMigrations from schema.go to ensure ALL column additions are covered
	for _, m := range SafeMigrations {
		if _, err := dm.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m[0], m[1], m[2])); err != nil {
			if !isDuplicateColumnError(err) {
				fmt.Fprintf(os.Stderr, "Warning: migration failed for %s.%s: %v\n", m[0], m[1], err)
			}
		}
	}

	// Backfill: set updated_at = created_at for rows migrated without updated_at
	dm.db.Exec(`UPDATE memories SET updated_at = created_at WHERE updated_at IS NULL`)

	// Use shared index definitions
	var indexErrs []error
	for _, sql := range CommonIndexes {
		if _, err := dm.db.Exec(sql); err != nil {
			indexErrs = append(indexErrs, err)
		}
	}
	if len(indexErrs) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: %d index creation errors (may be benign on re-run): %v\n", len(indexErrs), indexErrs)
	}

	// Try FTS5 tables - if they fail, continue without them (fallback search)
	if err := dm.initFTSTables(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: FTS5 initialization failed: %v (search will use LIKE fallback)\n", err)
	} else {
		// FTS5 tables ready — backfill any existing data that predates the triggers
		dm.backfillFTSTables()
	}

	// Memory revision triggers (independent of FTS5 — always required for versioning)
	revisionTriggers := []string{
		`CREATE TRIGGER IF NOT EXISTS memories_rev_ai AFTER INSERT ON memories
		BEGIN
			INSERT INTO memory_revisions (memory_id, version, content, weight, collection, is_long_term, created_at)
			VALUES (
				NEW.id,
				COALESCE((SELECT MAX(version) FROM memory_revisions WHERE memory_id = NEW.id), 0) + 1,
				NEW.content,
				COALESCE(NEW.weight, 0),
				NEW.collection,
				COALESCE(NEW.is_long_term, 0),
				STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')
			);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_rev_au AFTER UPDATE ON memories
		WHEN OLD.content != NEW.content
		   OR OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL
		   OR OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL
		   OR OLD.weight != NEW.weight
		   OR OLD.collection != NEW.collection
		BEGIN
			INSERT INTO memory_revisions (memory_id, version, content, weight, collection, is_long_term, created_at)
			VALUES (
				NEW.id,
				COALESCE((SELECT MAX(version) FROM memory_revisions WHERE memory_id = NEW.id), 0) + 1,
				NEW.content,
				COALESCE(NEW.weight, 0),
				NEW.collection,
				COALESCE(NEW.is_long_term, 0),
				STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')
			);
		END;`,
	}
	for _, sql := range revisionTriggers {
		if _, err := dm.db.Exec(sql); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: revision trigger creation failed: %v\nSQL: %s\n", err, sql)
		}
	}

	return nil
}

// dropFTS5Triggers removes all FTS5 triggers from the database.
// Called when FTS5 is not available in the current build, preventing
// leftover triggers (from a build compiled with FTS5) from firing on INSERT
// and causing "no such module: fts5" errors.
func (dm *DatabaseManager) dropFTS5Triggers() {
	triggers := []string{
		"DROP TRIGGER IF EXISTS sessions_ai",
		"DROP TRIGGER IF EXISTS sessions_ad",
		"DROP TRIGGER IF EXISTS sessions_au",
		"DROP TRIGGER IF EXISTS memories_ai",
		"DROP TRIGGER IF EXISTS memories_ad",
		"DROP TRIGGER IF EXISTS memories_au",
		"DROP TRIGGER IF EXISTS memories_au_content",
		"DROP TRIGGER IF EXISTS lessons_ai",
		"DROP TRIGGER IF EXISTS lessons_ad",
		"DROP TRIGGER IF EXISTS lessons_au",
		"DROP TRIGGER IF EXISTS topics_ai",
		"DROP TRIGGER IF EXISTS topics_ad",
		"DROP TRIGGER IF EXISTS topics_au",
		"DROP TRIGGER IF EXISTS references_ai",
		"DROP TRIGGER IF EXISTS references_ad",
		"DROP TRIGGER IF EXISTS references_au",
		"DROP TRIGGER IF EXISTS reference_chunks_ai",
		"DROP TRIGGER IF EXISTS reference_chunks_ad",
		"DROP TRIGGER IF EXISTS reference_chunks_au",
	}
	for _, t := range triggers {
		dm.db.Exec(t) // ignore errors — we're cleaning up, not enforcing
	}
}

func (dm *DatabaseManager) initFTSTables() error {
	// Robust FTS5 availability check
	var available int
	dm.db.QueryRow("SELECT 1 FROM pragma_compile_options WHERE compile_options LIKE '%FTS5%'").Scan(&available)
	if available == 0 {
		// Fallback test: try creating a real FTS5 table in-memory
		testDB, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			dm.dropFTS5Triggers() // clean up any leftover triggers from a build that had FTS5
			return nil            // fail silently, search will use LIKE fallback
		}
		defer testDB.Close()
		_, err = testDB.Exec("CREATE VIRTUAL TABLE test_fts USING fts5(content, tokenize='porter unicode61');")
		if err != nil {
			dm.dropFTS5Triggers() // clean up any leftover triggers from a build that had FTS5
			return nil            // fail silently, search will use LIKE fallback
		}
	}

	// FTS5 is available. Use unicode61+porter for broad compatibility.
	ftsStatements := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(content, session_id, content_hash UNINDEXED, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, collection, session_id UNINDEXED, tags UNINDEXED, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS topics_fts USING fts5(name, description, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS references_fts USING fts5(title, content, tags, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS reference_chunks_fts USING fts5(section, content, tokenize='porter unicode61');`,

		`CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,

		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories WHEN old.deleted_at IS NULL AND new.deleted_at IS NOT NULL BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au_content AFTER UPDATE ON memories WHEN NOT (old.deleted_at IS NULL AND new.deleted_at IS NOT NULL) BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,

		`CREATE TRIGGER IF NOT EXISTS lessons_ai AFTER INSERT ON lessons BEGIN INSERT INTO lessons_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS lessons_ad AFTER DELETE ON lessons BEGIN DELETE FROM lessons_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS lessons_au AFTER UPDATE ON lessons BEGIN DELETE FROM lessons_fts WHERE rowid = old.rowid; INSERT INTO lessons_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;`,

		`CREATE TRIGGER IF NOT EXISTS topics_ai AFTER INSERT ON topics BEGIN INSERT INTO topics_fts(rowid, name, description) VALUES (new.rowid, new.name, new.description); END;`,
		`CREATE TRIGGER IF NOT EXISTS topics_ad AFTER DELETE ON topics BEGIN DELETE FROM topics_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS topics_au AFTER UPDATE ON topics BEGIN DELETE FROM topics_fts WHERE rowid = old.rowid; INSERT INTO topics_fts(rowid, name, description) VALUES (new.rowid, new.name, new.description); END;`,

		`CREATE TRIGGER IF NOT EXISTS references_ai AFTER INSERT ON reference_docs BEGIN INSERT INTO references_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS references_ad AFTER DELETE ON reference_docs BEGIN DELETE FROM references_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS references_au AFTER UPDATE ON reference_docs BEGIN DELETE FROM references_fts WHERE rowid = old.rowid; INSERT INTO references_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags); END;`,

		`CREATE TRIGGER IF NOT EXISTS reference_chunks_ai AFTER INSERT ON reference_chunks BEGIN INSERT INTO reference_chunks_fts(rowid, section, content) VALUES (new.rowid, new.section, new.content); END;`,
		`CREATE TRIGGER IF NOT EXISTS reference_chunks_ad AFTER DELETE ON reference_chunks BEGIN DELETE FROM reference_chunks_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS reference_chunks_au AFTER UPDATE ON reference_chunks BEGIN DELETE FROM reference_chunks_fts WHERE rowid = old.rowid; INSERT INTO reference_chunks_fts(rowid, section, content) VALUES (new.rowid, new.section, new.content); END;`,
	}

	for _, sqlQuery := range ftsStatements {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			// FTS5 creation failed — log and return error so we know search will use LIKE fallback
			fmt.Fprintf(os.Stderr, "FTS5 init error (search will use LIKE): %v\nSQL: %s\n", err, sqlQuery)
			return fmt.Errorf("FTS5 table/trigger creation failed: %v (search will use LIKE fallback)", err)
		}
	}
	return nil
}

// backfillFTSTables inserts existing rows into FTS5 tables.
// Called once after initFTSTables succeeds to index pre-existing data
// that predates the FTS5 triggers.
func (dm *DatabaseManager) backfillFTSTables() error {
	backfills := []struct {
		destTable string
		srcTable  string
		cols      string
		insertSQL string
	}{
		{
			destTable: "memories_fts",
			srcTable:  "memories",
			cols:      "id, content, collection, session_id, tags",
			insertSQL: `INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
					SELECT rowid, content, collection, COALESCE(session_id,''), COALESCE(tags,'[]')
					FROM memories WHERE deleted_at IS NULL`,
		},
		{
			destTable: "sessions_fts",
			srcTable:  "sessions",
			cols:      "id, content, session_id, content_hash",
			insertSQL: `INSERT INTO sessions_fts(rowid, content, session_id, content_hash)
					SELECT rowid, content, COALESCE(session_id,''), COALESCE(content_hash,'') FROM sessions`,
		},
		{
			destTable: "topics_fts",
			srcTable:  "topics",
			cols:      "id, name, description",
			insertSQL: `INSERT INTO topics_fts(rowid, name, description)
					SELECT rowid, COALESCE(name,''), COALESCE(description,'') FROM topics`,
		},
		{
			destTable: "lessons_fts",
			srcTable:  "lessons",
			cols:      "id, content, tags",
			insertSQL: `INSERT INTO lessons_fts(rowid, content, tags)
					SELECT rowid, content, COALESCE(tags,'[]') FROM lessons`,
		},
	}

	for _, b := range backfills {
		// Check if FTS table already has data (avoid duplicate backfills)
		var count int
		dm.db.QueryRow("SELECT COUNT(*) FROM " + b.destTable).Scan(&count)
		if count > 0 {
			continue // already populated
		}
		_, err := dm.db.Exec(b.insertSQL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: backfill FTS table %s failed: %v\n", b.destTable, err)
		}
	}
	return nil
}

func (dm *DatabaseManager) Close() error {
	if dm.db != nil {
		return dm.db.Close()
	}
	return nil
}

// ==================== CRUD OPERATIONS ====================

func (dm *DatabaseManager) SaveSession(sessionID, content, sourcePath string, metadata map[string]interface{}) (string, error) {
	hash := sha256.Sum256([]byte(content))
	contentHash := hex.EncodeToString(hash[:])
	id := GenerateID()

	metadataJSON := "{}"
	if metadata != nil {
		bytes, _ := json.Marshal(metadata)
		metadataJSON = string(bytes)
	}

	_, err := dm.db.Exec(`INSERT INTO sessions (id, session_id, content, content_hash, source_path, metadata) VALUES (?, ?, ?, ?, ?, ?)`,
		id, sessionID, content, contentHash, sourcePath, metadataJSON)
	return id, err
}

// UpdateSessionSummary upserts the LLM-generated summary for a session.
// Uses the session_id as the unique key.
func (dm *DatabaseManager) UpdateSessionSummary(sessionID, summary string) error {
	_, err := dm.db.Exec(`UPDATE sessions SET summary = ? WHERE session_id = ?`, summary, sessionID)
	return err
}

func (dm *DatabaseManager) GetSession(id string) (map[string]interface{}, error) {
	var sessionID, content, contentHash, sourcePath, metadataJSON string
	var createdAt time.Time

	err := dm.db.QueryRow(`SELECT session_id, content, content_hash, source_path, metadata, created_at FROM sessions WHERE id = ?`, id).
		Scan(&sessionID, &content, &contentHash, &sourcePath, &metadataJSON, &createdAt)
	if err != nil {
		return nil, err
	}

	var metadata map[string]interface{}
	if metadataJSON != "" {
		json.Unmarshal([]byte(metadataJSON), &metadata)
	}

	return map[string]interface{}{
		"id":           id,
		"session_id":   sessionID,
		"content":      content,
		"content_hash": contentHash,
		"source_path":  sourcePath,
		"metadata":     metadata,
		"created_at":   createdAt,
	}, nil
}

// GetLastSession returns the most recent session by created_at DESC.
func (dm *DatabaseManager) GetLastSession() (map[string]interface{}, error) {
	var id, sessionID, content, contentHash, sourcePath, metadataJSON string
	var createdAt time.Time

	err := dm.db.QueryRow(`SELECT id, session_id, content, content_hash, source_path, metadata, created_at FROM sessions ORDER BY created_at DESC LIMIT 1`).
		Scan(&id, &sessionID, &content, &contentHash, &sourcePath, &metadataJSON, &createdAt)
	if err != nil {
		return nil, err
	}

	var metadata map[string]interface{}
	if metadataJSON != "" {
		json.Unmarshal([]byte(metadataJSON), &metadata)
	}

	return map[string]interface{}{
		"id":           id,
		"session_id":   sessionID,
		"content":      content,
		"content_hash": contentHash,
		"source_path":  sourcePath,
		"metadata":     metadata,
		"created_at":   createdAt,
	}, nil
}

// GetSessionMemories returns the most recent memories for a given session ID.
func (dm *DatabaseManager) GetSessionMemories(sessionID string, limit int) ([]map[string]interface{}, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session ID is required")
	}
	if limit <= 0 {
		limit = 5
	}

	rows, err := dm.db.Query(`
		SELECT id, collection, content, tags, metadata, created_at
		FROM memories
		WHERE session_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var memID, collection, content, tagsJSON, metadataJSON, createdAt string
		if err := rows.Scan(&memID, &collection, &content, &tagsJSON, &metadataJSON, &createdAt); err != nil {
			return nil, err
		}
		var tags []string
		var metadata map[string]interface{}
		json.Unmarshal([]byte(tagsJSON), &tags)
		if metadataJSON != "" {
			json.Unmarshal([]byte(metadataJSON), &metadata)
		}
		results = append(results, map[string]interface{}{
			"id":         memID,
			"collection": collection,
			"content":    content,
			"tags":       tags,
			"metadata":   metadata,
			"created_at": createdAt,
		})
	}
	return results, rows.Err()
}

func (dm *DatabaseManager) SaveMemory(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, expiresAt ...time.Time) (string, error) {
	id := GenerateID()
	tagsJSON, _ := json.Marshal(tags)
	metadataJSON, _ := json.Marshal(metadata)

	embeddingJSON := "null"
	if embedding != nil {
		bytes, _ := json.Marshal(embedding)
		embeddingJSON = string(bytes)
	}

	// Handle empty sessionID as NULL to satisfy FK constraint
	var sessionIDVal interface{} = nil
	if sessionID != "" {
		sessionIDVal = sessionID
	}

	// is_long_term and weight for indexed queries
	isLTM := 0
	if isLongTerm {
		isLTM = 1
	}

	// Handle optional expires_at parameter
	var expiresAtStr interface{} = nil
	if len(expiresAt) > 0 && !expiresAt[0].IsZero() {
		expiresAtStr = expiresAt[0].UTC().Format(time.RFC3339)
	}

	_, err := dm.db.Exec(`INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding, is_long_term, weight, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, collection, content, sessionIDVal, string(tagsJSON), string(metadataJSON), embeddingJSON, isLTM, weight, expiresAtStr)
	return id, err
}

// UpdateMemoryMetadata patches the metadata JSON column for a specific memory ID.
// Uses SQLite's json_patch() to merge the patch into existing metadata in-place.
// Does NOT touch content — FTS index is not affected.
// Returns error if the memory ID does not exist.
//
// The UPDATE and the FTS sync statements run inside a single transaction so
// a crash mid-write cannot leave the FTS index out of sync with the canonical
// content. FTS sync errors are checked and returned (previously swallowed).
func (dm *DatabaseManager) UpdateMemoryMetadata(id string, patchJSON string) error {
	if !json.Valid([]byte(patchJSON)) {
		return fmt.Errorf("UpdateMemoryMetadata: invalid JSON patch: %s", patchJSON)
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: begin: %w", err)
	}
	defer tx.Rollback()

	if err := updateMemoryMetadataTx(tx, id, patchJSON); err != nil {
		return err
	}

	return tx.Commit()
}

// updateMemoryMetadataTx is the transactional body of UpdateMemoryMetadata.
// It runs the metadata UPDATE, the FTS DELETE, and the FTS INSERT against the
// supplied transaction. Exposed as a helper so callers that already own a
// transaction (e.g., ChallengeMemory) can join it without opening a second one.
func updateMemoryMetadataTx(tx *sql.Tx, id string, patchJSON string) error {
	// json_patch(NULL, '{}') returns NULL in SQLite, so COALESCE to {} is required.
	// This ensures a NULL metadata field doesn't cause the patch to fail.
	result, err := tx.Exec(`
		UPDATE memories
		SET metadata = json_patch(COALESCE(metadata, '{}'), ?),
			last_accessed_at = CURRENT_TIMESTAMP
		WHERE id = ? AND deleted_at IS NULL
	`, patchJSON, id)
	if err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: rows check: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("memory not found or deleted: %s", id)
	}

	// Metadata-only changes bypass the FTS trigger — manually sync FTS
	// for the updated row so the search index stays current. Errors are
	// checked and returned; a silent failure here would leave FTS stale
	// while the caller believes the update succeeded.
	if _, err := tx.Exec(`
		DELETE FROM memories_fts WHERE rowid = (
			SELECT rowid FROM memories WHERE id = ?
		)
	`, id); err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: fts delete: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
		SELECT rowid, content, collection, COALESCE(session_id,''), COALESCE(tags,'[]')
		FROM memories WHERE id = ? AND deleted_at IS NULL
	`, id); err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: fts insert: %w", err)
	}

	return nil
}

// SaveSystemConfig stores or updates a system config entry (keyed by source file name)
// Only updates if the content hash has changed (skip duplicate writes)
// Returns (updated bool, error)
func (dm *DatabaseManager) SaveSystemConfig(key, rawJSON, contentHash string, snapshotJSON string) (bool, error) {
	var existingHash string
	err := dm.db.QueryRow(`SELECT content_hash FROM system_config WHERE key = ?`, key).Scan(&existingHash)
	if err == nil && subtle.ConstantTimeCompare([]byte(existingHash), []byte(contentHash)) == 1 {
		// Unchanged — skip the write
		return false, nil
	}
	// Insert or replace with new content
	_, err = dm.db.Exec(`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash, updated_at, config_snapshot) VALUES (?, ?, ?, CURRENT_TIMESTAMP, ?)`,
		key, rawJSON, contentHash, snapshotJSON)
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetSystemConfig retrieves a system config entry by key
func (dm *DatabaseManager) GetSystemConfig(key string) (map[string]interface{}, error) {
	var rawJSON, contentHash, snapshotJSON string
	var updatedAt time.Time
	err := dm.db.QueryRow(`SELECT raw_json, content_hash, updated_at, config_snapshot FROM system_config WHERE key = ?`, key).
		Scan(&rawJSON, &contentHash, &updatedAt, &snapshotJSON)
	if err != nil {
		return nil, err
	}
	var snapshot map[string]interface{}
	if snapshotJSON != "" {
		json.Unmarshal([]byte(snapshotJSON), &snapshot)
	}
	return map[string]interface{}{
		"key":          key,
		"raw_json":     rawJSON,
		"content_hash": contentHash,
		"updated_at":   updatedAt,
		"snapshot":     snapshot,
	}, nil
}

// DeleteSystemConfig removes a system config entry by key.
// Returns nil even if the key does not exist (idempotent).
func (dm *DatabaseManager) DeleteSystemConfig(key string) error {
	_, err := dm.db.Exec(`DELETE FROM system_config WHERE key = ?`, key)
	return err
}

// GetAllSystemConfigs returns all system config entries
func (dm *DatabaseManager) GetAllSystemConfigs() ([]map[string]interface{}, error) {
	rows, err := dm.db.Query(`SELECT key, raw_json, content_hash, updated_at, config_snapshot FROM system_config ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []map[string]interface{}
	for rows.Next() {
		var key, rawJSON, contentHash, snapshotJSON string
		var updatedAt time.Time
		if err := rows.Scan(&key, &rawJSON, &contentHash, &updatedAt, &snapshotJSON); err != nil {
			return nil, err
		}
		var snapshot map[string]interface{}
		if snapshotJSON != "" {
			json.Unmarshal([]byte(snapshotJSON), &snapshot)
		}
		configs = append(configs, map[string]interface{}{
			"key":          key,
			"raw_json":     rawJSON,
			"content_hash": contentHash,
			"updated_at":   updatedAt,
			"snapshot":     snapshot,
		})
	}
	return configs, nil
}

// ==================== VECTOR SEARCH ====================

type searchResult struct {
	id         string
	content    string
	createdAt  time.Time
	similarity float32
}

func cosineSimilarity(a, b []float32) float32 {
	var dotProduct, normA, normB float32
	for i := range a {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

func (dm *DatabaseManager) VectorSearch(tier string, queryEmbedding []float32, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 10
	}
	if tier != "sessions" && tier != "memories" && tier != "topics" {
		return nil, fmt.Errorf("unsupported tier: %s", tier)
	}

	// Safe: whitelist enforced above; map lookup avoids fmt.Sprintf with user data
	baseQuery := map[string]string{
		"sessions": "SELECT id, content, embedding, created_at FROM sessions WHERE embedding IS NOT NULL",
		"memories": "SELECT id, content, embedding, created_at FROM memories WHERE embedding IS NOT NULL",
		"topics":   "SELECT id, content, embedding, created_at FROM topics WHERE embedding IS NOT NULL",
	}[tier]
	rows, err := dm.db.Query(baseQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []searchResult
	for rows.Next() {
		var id, content, embeddingJSON string
		var createdAt time.Time
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("VectorSearch: panic in rows loop: %v", r)
				}
			}()
			if err := rows.Scan(&id, &content, &embeddingJSON, &createdAt); err != nil {
				return
			}

			var dbEmbedding []float32
			if err := json.Unmarshal([]byte(embeddingJSON), &dbEmbedding); err != nil || len(dbEmbedding) != len(queryEmbedding) {
				return
			}

			sim := cosineSimilarity(queryEmbedding, dbEmbedding)
			results = append(results, searchResult{id, content, createdAt, sim})
		}()
		if err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(results, func(i, j int) bool { return results[i].similarity > results[j].similarity })
	if len(results) > limit {
		results = results[:limit]
	}

	var final []map[string]interface{}
	for _, r := range results {
		final = append(final, map[string]interface{}{
			"id":         r.id,
			"content":    r.content,
			"created_at": r.createdAt,
			"similarity": r.similarity,
		})
	}
	return final, nil
}

// ==================== SHRED PROTOCOL ====================

// WipeRecord performs a hard delete (on DatabaseManager).
//
// For the "topics" tier, the membership rows in topic_memberships and the
// parent row in topics are deleted in a single transaction so a crash mid-write
// cannot leave orphan memberships behind (the FK cascade would normally catch
// them, but only if FKs are enabled on the connection — a SQLite WAL crash can
// leave FK enforcement off until the next connection).
func (dm *DatabaseManager) WipeRecord(tier, id string) error {
	tableName := WipeTableNames[tier]
	if tableName == "" {
		return fmt.Errorf("cannot wipe from tier: %s", tier)
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("WipeRecord: begin: %w", err)
	}
	defer tx.Rollback()

	// Clean up topic_memberships when deleting a topic
	if tier == "topics" {
		if _, err := tx.Exec("DELETE FROM topic_memberships WHERE topic_id = ?", id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM "+tableName+" WHERE id = ?", id); err != nil {
		return err
	}

	return tx.Commit()
}

// ShredMemory performs a hard delete on memories table
func ShredMemory(db *sql.DB, id string) error {
	_, err := db.Exec("DELETE FROM memories WHERE id = ?", id)
	return err
}

// ShredSession performs a hard delete on sessions table
func ShredSession(db *sql.DB, id string) error {
	_, err := db.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

// DeleteByID performs a hard delete (for general use)
func DeleteByID(db *sql.DB, table, id string) error {
	tableName := WipeTableNames[table]
	if tableName == "" {
		return fmt.Errorf("cannot delete from table: %s", table)
	}
	_, err := db.Exec("DELETE FROM "+tableName+" WHERE id = ?", id)
	return err
}

// ShredTopic performs a hard delete on topics table.
// The membership rows in topic_memberships and the parent row in topics are
// deleted in a single transaction so a crash mid-write cannot leave orphan
// memberships behind.
func ShredTopic(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("ShredTopic: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM topic_memberships WHERE topic_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM topics WHERE id = ?", id); err != nil {
		return err
	}

	return tx.Commit()
}

// ==================== HELPERS ====================

var (
	idMu      sync.Mutex
	idCounter int64
)

func GenerateID() string {
	idMu.Lock()
	idCounter++
	ts := time.Now().UnixNano()
	input := fmt.Sprintf("%d-%d", ts, idCounter)
	idMu.Unlock()
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])[:16]
}

// =============================================================================
// Topic Management (exported for CLI use)
// =============================================================================

// CreateTopic creates a new topic and returns its ID
func (dm *DatabaseManager) CreateTopic(name, description, fromDate, toDate string) (string, error) {
	topicID := GenerateID()
	now := time.Now().UTC().Format(time.RFC3339)
	tagsJSON := "{}"
	if fromDate != "" || toDate != "" {
		tags := map[string]string{}
		if fromDate != "" {
			tags["from_date"] = fromDate
		}
		if toDate != "" {
			tags["to_date"] = toDate
		}
		bytes, _ := json.Marshal(tags)
		tagsJSON = string(bytes)
	}
	descStr := description
	if descStr == "" {
		descStr = "{}"
	}
	_, err := dm.db.Exec(`
		INSERT INTO topics (id, name, description, created_at, tags, is_active, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)`,
		topicID, name, descStr, now, tagsJSON, now)
	return topicID, err
}

// GetOrCreateTopic looks up a topic by name, creating it if not found
func (dm *DatabaseManager) GetOrCreateTopic(name string) (string, error) {
	var id string
	err := dm.db.QueryRow(`SELECT id FROM topics WHERE name = ? AND is_active = 1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	return dm.CreateTopic(name, "", "", "")
}

// GetTopicByName returns a topic ID by name
func (dm *DatabaseManager) GetTopicByName(name string) (string, error) {
	var id string
	err := dm.db.QueryRow(`SELECT id FROM topics WHERE name = ? AND is_active = 1`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("topic not found")
	}
	return id, err
}

// GetTopic returns a topic record by ID
func (dm *DatabaseManager) GetTopic(id string) (map[string]interface{}, error) {
	var name, desc, createdAt, tagsJSON string
	err := dm.db.QueryRow(
		`SELECT name, COALESCE(description,''), created_at, COALESCE(tags,'{}') FROM topics WHERE id = ?`, id).Scan(
		&name, &desc, &createdAt, &tagsJSON)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("topic not found")
	}
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{
		"id": id, "name": name, "description": desc, "created_at": createdAt, "tags": tagsJSON,
	}
	if strings.Contains(tagsJSON, "from_date") {
		var tags map[string]string
		json.Unmarshal([]byte(tagsJSON), &tags)
		if v, ok := tags["from_date"]; ok {
			result["from_date"] = v
		}
		if v, ok := tags["to_date"]; ok {
			result["to_date"] = v
		}
	}
	return result, nil
}

// ListTopics returns all active topics with memory counts
func (dm *DatabaseManager) ListTopics() ([]map[string]interface{}, error) {
	rows, err := dm.db.Query(`
		SELECT t.id, t.name, t.created_at, COALESCE(t.tags,'{}'),
			   COUNT(tm.memory_id) as memory_count
		FROM topics t
		LEFT JOIN topic_memberships tm ON tm.topic_id = t.id AND tm.memory_id IS NOT NULL
		WHERE t.is_active = 1
		GROUP BY t.id
		ORDER BY t.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var topics []map[string]interface{}
	for rows.Next() {
		var id, name, createdAt, tagsJSON string
		var memoryCount int
		if err := rows.Scan(&id, &name, &createdAt, &tagsJSON, &memoryCount); err != nil {
			continue
		}
		m := map[string]interface{}{"id": id, "name": name, "created_at": createdAt, "memory_count": memoryCount, "tags": tagsJSON}
		if strings.Contains(tagsJSON, "from_date") {
			var tags map[string]string
			json.Unmarshal([]byte(tagsJSON), &tags)
			if v, ok := tags["from_date"]; ok {
				m["from_date"] = v
			}
			if v, ok := tags["to_date"]; ok {
				m["to_date"] = v
			}
		}
		topics = append(topics, m)
	}
	return topics, nil
}

// GetTopicMemories returns memories in a topic (manual + auto from date range)
func (dm *DatabaseManager) GetTopicMemories(topicID string) ([]map[string]interface{}, error) {
	topic, err := dm.GetTopic(topicID)
	if err != nil {
		return nil, err
	}
	fromDate, _ := topic["from_date"].(string)
	toDate, _ := topic["to_date"].(string)

	rows, err := dm.db.Query(`
		SELECT m.id, m.content, m.session_id, m.tags, m.created_at, tm.role
		FROM topic_memberships tm
		JOIN memories m ON m.id = tm.memory_id
		WHERE tm.topic_id = ?
		ORDER BY tm.created_at DESC
	`, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []map[string]interface{}
	seen := make(map[string]bool)

	for rows.Next() {
		var memID, content, sessionID, tags, createdAt, role string
		if err := rows.Scan(&memID, &content, &sessionID, &tags, &createdAt, &role); err != nil {
			continue
		}
		seen[memID] = true
		memories = append(memories, map[string]interface{}{
			"id": memID, "content": content, "session_id": sessionID, "tags": tags, "created_at": createdAt, "role": role,
		})
	}

	if fromDate != "" {
		to := toDate
		if to == "" {
			to = fromDate
		}
		autoRows, err := dm.db.Query(`
			SELECT id, content, session_id, tags, created_at FROM memories
			WHERE created_at >= ? AND created_at <= ? AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT 50
		`, fromDate+"T00:00:00Z", to+"T23:59:59Z")
		if err != nil {
			return memories, err
		}
		defer autoRows.Close()
		for autoRows.Next() {
			var memID, content, sessionID, tags, createdAt string
			if err := autoRows.Scan(&memID, &content, &sessionID, &tags, &createdAt); err != nil {
				continue
			}
			if !seen[memID] {
				seen[memID] = true
				memories = append(memories, map[string]interface{}{
					"id": memID, "content": content, "session_id": sessionID, "tags": tags, "created_at": createdAt, "role": "auto",
				})
			}
		}
	}
	return memories, nil
}

// AddMemoryToTopic adds a memory to a topic
func (dm *DatabaseManager) AddMemoryToTopic(memoryID, topicID, role string) error {
	if role == "" {
		role = "manual"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := dm.db.Exec(`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, ?, ?)`,
		memoryID, topicID, now, role)
	return err
}

// RemoveMemoryFromTopic removes a memory from a topic
func (dm *DatabaseManager) RemoveMemoryFromTopic(memoryID, topicID string) error {
	_, err := dm.db.Exec(`DELETE FROM topic_memberships WHERE memory_id = ? AND topic_id = ?`, memoryID, topicID)
	return err
}

// DeleteTopic soft-deletes a topic (memories are NOT deleted).
//
// Both writes run inside a single transaction with the soft-delete UPDATE
// happening first: if a caller lists "active topics" between operations, the
// deactivated topic will not appear even before the membership cleanup runs.
// Previously, the membership DELETE ran first, so a failed UPDATE left an
// active topic with no members — a confusing state for any UI listing.
func (dm *DatabaseManager) DeleteTopic(topicID string) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("DeleteTopic: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE topics SET is_active = 0 WHERE id = ?`, topicID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM topic_memberships WHERE topic_id = ?`, topicID); err != nil {
		return err
	}

	return tx.Commit()
}

// ==================== Lesson Operations ====================

// LessonType represents the type of lesson
type LessonType string

const (
	LessonTypeWarning  LessonType = "warning"  // "don't do X"
	LessonTypePractice LessonType = "practice" // "do Y"
	LessonTypeInsight  LessonType = "insight"  // "X leads to Y"
)

// Lesson represents a learned lesson
type Lesson struct {
	ID                 string     `json:"id"`
	Type               LessonType `json:"type"`
	Content            string     `json:"content"`
	Tags               []string   `json:"tags"`
	ReinforcementCount int        `json:"reinforcement_count"`
	SourceSessionID    string     `json:"source_session_id,omitempty"`
	Created            string     `json:"created"`
}

// AddLesson adds a new lesson, checking for duplicates by content hash
func (dm *DatabaseManager) AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error) {
	// Check for sensitive content before storing
	if isSensitive, name := isSensitiveContent(content); isSensitive {
		return nil, fmt.Errorf("lesson content blocked: %s detected", name)
	}

	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	// Check for existing lesson with same content
	var existingID string
	err := dm.db.QueryRow(`SELECT id FROM lessons WHERE content_hash = ?`, contentHash).Scan(&existingID)
	if err == nil {
		// Reinforce existing lesson
		_, err = dm.db.Exec(`UPDATE lessons SET reinforcement_count = reinforcement_count + 1 WHERE id = ?`, existingID)
		if err != nil {
			return nil, err
		}
		return dm.GetLesson(existingID)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Create new lesson
	id := GenerateID()
	tagsJSON, _ := MarshalJSON(tags)
	now := time.Now().Format(time.RFC3339)

	_, err = dm.db.Exec(`
		INSERT INTO lessons (id, type, content, tags, reinforcement_count, source_session_id, created, content_hash)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?)
	`, id, lessonType, content, tagsJSON, sourceSessionID, now, contentHash)
	if err != nil {
		return nil, err
	}

	return &Lesson{
		ID:                 id,
		Type:               lessonType,
		Content:            content,
		Tags:               tags,
		ReinforcementCount: 1,
		SourceSessionID:    sourceSessionID,
		Created:            now,
	}, nil
}

// GetLesson retrieves a lesson by ID
func (dm *DatabaseManager) GetLesson(id string) (*Lesson, error) {
	var lesson Lesson
	var tagsJSON string
	err := dm.db.QueryRow(`
		SELECT id, type, content, tags, reinforcement_count, source_session_id, created
		FROM lessons WHERE id = ?
	`, id).Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
	if err != nil {
		return nil, err
	}
	if tagsJSON != "" {
		UnmarshalJSON(tagsJSON, &lesson.Tags)
	}
	return &lesson, nil
}

// ListLessons returns all lessons, optionally filtered by type
func (dm *DatabaseManager) ListLessons(lessonType string) ([]*Lesson, error) {
	query := `SELECT id, type, content, tags, reinforcement_count, source_session_id, created FROM lessons`
	var args []interface{}
	if lessonType != "" {
		query += ` WHERE type = ?`
		args = append(args, lessonType)
	}
	query += ` ORDER BY reinforcement_count DESC, created DESC`

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lessons []*Lesson
	for rows.Next() {
		var lesson Lesson
		var tagsJSON string
		err := rows.Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
		if err != nil {
			continue
		}
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return lessons, nil
}

// SearchLessons performs FTS5 search across lessons
func (dm *DatabaseManager) SearchLessons(query string, limit int) ([]*Lesson, error) {
	if limit <= 0 {
		limit = 20
	}

	escaped := strings.ReplaceAll(query, "\"", "\"\"")
	ftsQuery := "\"" + escaped + "\"*"

	rows, err := dm.db.Query(`
		SELECT l.id, l.type, l.content, l.tags, l.reinforcement_count, l.source_session_id, l.created
		FROM lessons l
		WHERE l.id IN (SELECT id FROM lessons WHERE lessons_fts MATCH ?)
		ORDER BY l.reinforcement_count DESC, l.created DESC
		LIMIT ?
	`, ftsQuery, limit)
	if err != nil {
		return dm.searchLessonsLike(query, limit)
	}
	defer rows.Close()

	var lessons []*Lesson
	for rows.Next() {
		var lesson Lesson
		var tagsJSON string
		err := rows.Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
		if err != nil {
			continue
		}
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	if len(lessons) > 0 {
		return lessons, nil
	}
	return dm.searchLessonsLike(query, limit)
}

// searchLessonsLike is a fallback when FTS5 is unavailable
func (dm *DatabaseManager) searchLessonsLike(query string, limit int) ([]*Lesson, error) {
	q := "%" + strings.ToLower(query) + "%"
	rows, err := dm.db.Query(`
		SELECT id, type, content, tags, reinforcement_count, source_session_id, created
		FROM lessons
		WHERE LOWER(content) LIKE ? OR LOWER(tags) LIKE ?
		ORDER BY reinforcement_count DESC
		LIMIT ?
	`, q, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lessons []*Lesson
	for rows.Next() {
		var lesson Lesson
		var tagsJSON string
		err := rows.Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
		if err != nil {
			continue
		}
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	return lessons, nil
}

// DeleteLesson removes a lesson
func (dm *DatabaseManager) DeleteLesson(id string) error {
	_, err := dm.db.Exec(`DELETE FROM lessons WHERE id = ?`, id)
	return err
}

// =============================================================================
// Memory Revision Helpers (Epistemological Time Travel)
// =============================================================================

// MemoryRevision represents a single entry in the memory_revisions table.
type MemoryRevision struct {
	ID                 int64
	MemoryID           string
	Version            int
	Content            string
	Weight             int
	Collection         string
	IsLongTerm         bool
	IsChallenged       bool
	ChallengedTheoryID string
	CreatedAt          time.Time
}

// GetMemoryRevisions returns all versions for a memory in reverse-chronological
// order (newest first). Returns empty slice if the memory has no revisions.
func (dm *DatabaseManager) GetMemoryRevisions(memoryID string) ([]MemoryRevision, error) {
	rows, err := dm.db.Query(`
		SELECT id, memory_id, version, content, weight, collection,
		       is_long_term, is_challenged, COALESCE(challenged_theory_id, ''), created_at
		FROM memory_revisions
		WHERE memory_id = ?
		ORDER BY version DESC
	`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("GetMemoryRevisions: %w", err)
	}
	defer rows.Close()

	var revisions []MemoryRevision
	for rows.Next() {
		var r MemoryRevision
		var createdAt string
		if err := rows.Scan(&r.ID, &r.MemoryID, &r.Version, &r.Content,
			&r.Weight, &r.Collection, &r.IsLongTerm, &r.IsChallenged,
			&r.ChallengedTheoryID, &createdAt); err != nil {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05.999999999", createdAt); err == nil {
			r.CreatedAt = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
			r.CreatedAt = t
		} else if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			r.CreatedAt = t
		} else if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			r.CreatedAt = t
		}
		revisions = append(revisions, r)
	}
	if revisions == nil {
		return []MemoryRevision{}, nil
	}
	return revisions, rows.Err()
}

// GetMemoryRevisionAtTime returns the memory revision active at the given
// timestamp. Returns nil if the memory did not exist yet at that time.
//
// The query selects the most recent version whose created_at <= asOf.
// This provides point-in-time reconstruction without replaying a log.
func (dm *DatabaseManager) GetMemoryRevisionAtTime(memoryID string, asOf time.Time) (*MemoryRevision, error) {
	// First verify the memory existed at that time
	var createdAt string
	err := dm.db.QueryRow(`SELECT created_at FROM memories WHERE id = ?`, memoryID).Scan(&createdAt)
	if err != nil {
		return nil, nil // memory doesn't exist
	}
	var createdTime time.Time
	if t, err := time.Parse("2006-01-02 15:04:05.999999999", createdAt); err == nil {
		createdTime = t
	} else if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
		createdTime = t
	} else if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		createdTime = t
	} else if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
		createdTime = t
	} else {
		return nil, nil
	}
	if createdTime.After(asOf) {
		return nil, nil // memory didn't exist yet
	}

	// Check if the memory was soft-deleted before asOf
	var deletedAt *string
	err = dm.db.QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, memoryID).Scan(&deletedAt)
	if err == nil && deletedAt != nil && *deletedAt != "" {
		var deletedTime time.Time
		if t, err := time.Parse("2006-01-02 15:04:05.999999999", *deletedAt); err == nil {
			deletedTime = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", *deletedAt); err == nil {
			deletedTime = t
		} else if t, err := time.Parse(time.RFC3339Nano, *deletedAt); err == nil {
			deletedTime = t
		} else if t, err := time.Parse(time.RFC3339, *deletedAt); err == nil {
			deletedTime = t
		}
		if !deletedTime.IsZero() && deletedTime.Before(asOf) {
			return nil, nil // memory was deleted before asOf
		}
	}

	asOfStr := asOf.UTC().Format("2006-01-02 15:04:05.999999999")
	row := dm.db.QueryRow(`
		SELECT id, memory_id, version, content, weight, collection,
		       is_long_term, is_challenged, COALESCE(challenged_theory_id, ''), created_at
		FROM memory_revisions
		WHERE memory_id = ?
		  AND created_at <= ?
		ORDER BY version DESC
		LIMIT 1
	`, memoryID, asOfStr)

	var r MemoryRevision
	var revCreatedAt string
	err = row.Scan(&r.ID, &r.MemoryID, &r.Version, &r.Content,
		&r.Weight, &r.Collection, &r.IsLongTerm, &r.IsChallenged,
		&r.ChallengedTheoryID, &revCreatedAt)
	if err != nil {
		return nil, nil // no revision found for that time
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999", revCreatedAt); err == nil {
		r.CreatedAt = t
	} else if t, err := time.Parse("2006-01-02 15:04:05", revCreatedAt); err == nil {
		r.CreatedAt = t
	} else if t, err := time.Parse(time.RFC3339Nano, revCreatedAt); err == nil {
		r.CreatedAt = t
	} else if t, err := time.Parse(time.RFC3339, revCreatedAt); err == nil {
		r.CreatedAt = t
	}
	return &r, nil
}

// =============================================================================
// Overflow Deferred Enqueue (Ingestion Backpressure)
// =============================================================================

// DLQEnqueueOverflow writes an overflowed event to the raw_memories table with
// status='overflow_deferred'. Called from the non-blocking default branch of
// Enqueue() when the channel buffer is saturated. Runs in an isolated goroutine
// to prevent blocking the hot fsnotify loop. Overflow entries follow the same
// exponential backoff timeline as normal DLQ entries via processDLQ.
func (dm *DatabaseManager) DLQEnqueueOverflow(event MemoryEvent) error {
	id := GenerateID()
	contentHash := sha256.Sum256([]byte(event.Content))

	tagsJSON, _ := json.Marshal(event.Tags)
	metadata := fmt.Sprintf(`{"tags":%s,"overflowed":true,"overflowed_at":"%s"}`,
		string(tagsJSON), time.Now().UTC().Format(time.RFC3339))

	now := float64(time.Now().Unix())
	expiresAt := float64(time.Now().Add(24 * time.Hour).Unix())
	nextRetry := time.Now().Add(1 * time.Minute).UTC().Format(time.RFC3339)

	_, err := dm.db.Exec(`
		INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata,
			ingested_at, status, import_batch, updated_at, expires_at, next_retry, attempt)
		VALUES (?, ?, 'overflow', ?, ?, ?, ?, 'overflow_deferred', ?, ?, ?, ?, 0)
	`, id, event.ID, hex.EncodeToString(contentHash[:]), event.Content, metadata,
		now, "", now, expiresAt, nextRetry)
	return err
}

// =============================================================================
// Cognitive Immune System: Async Challenge
// =============================================================================

// ChallengeMemoryAsync logs a contradiction collision to mirror.jsonl in an
// independent, detached goroutine. Does NOT block the search query or write to
// the database — only appends to the audit mirror. The evidence parameter
// describes which memories collided and why.
func (dm *DatabaseManager) ChallengeMemoryAsync(memoryID string, evidence string) {
	go func() {
		mirrorPath := filepath.Join(config.GetMPMDir(), "src", "db", "mirror.jsonl")
		f, err := os.OpenFile(mirrorPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()

		entry := map[string]interface{}{
			"event":     "contradiction_detected",
			"memory_id": memoryID,
			"evidence":  evidence,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		line, _ := json.Marshal(entry)
		dm.watchdogMu.Lock()
		f.WriteString(string(line) + "\n")
		dm.watchdogMu.Unlock()
	}()
}

// ChallengeMemory applies provenance-based contradiction resolution:
// slashes the loser's weight and sets challenged status in the DB.
// The slashAmount is the weight reduction (positive integer).
//
// All four operations (pre-check, metadata patch with FTS sync, weight update,
// async log) are wrapped in a single transaction. A concurrent ReinforceMemory
// cannot interleave between the metadata patch and the weight decrement, so
// the challenged memory is always observed in a consistent state.
func (dm *DatabaseManager) ChallengeMemory(memoryID string, slashAmount int, evidence string) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("ChallengeMemory: begin: %w", err)
	}
	defer tx.Rollback()

	// Verify the memory exists and is not hard-deleted inside the transaction
	// so a concurrent soft-delete between the check and the writes is caught
	// by the WHERE-deleted_at-IS-NULL clause on each subsequent UPDATE.
	var exists bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM memories WHERE id = ? AND deleted_at IS NULL)`, memoryID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("ChallengeMemory: select check: %w", err)
	}
	if !exists {
		return fmt.Errorf("ChallengeMemory: memory not found or deleted: %s", memoryID)
	}

	patch := map[string]interface{}{
		"status":               "challenged",
		"challenged_theory_id": evidence,
	}
	patchJSON, _ := json.Marshal(patch)
	if err := updateMemoryMetadataTx(tx, memoryID, string(patchJSON)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE memories SET weight = MAX(1, weight - ?), updated_at = CURRENT_TIMESTAMP WHERE id = ? AND deleted_at IS NULL`, slashAmount, memoryID); err != nil {
		return fmt.Errorf("ChallengeMemory: weight update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ChallengeMemory: commit: %w", err)
	}

	// Async watchdog log is fire-and-forget and runs only after a successful
	// commit, so a logged entry always reflects a committed state.
	dm.ChallengeMemoryAsync(memoryID, evidence)
	return nil
}

// extractProvenanceCompute reads the provenance.compute field from metadata JSON.
// Returns empty string if not found, allowing graceful default to "standard".
func extractProvenanceCompute(metadataJSON string) string {
	if metadataJSON == "" || metadataJSON == "{}" || metadataJSON == "null" {
		return ""
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return ""
	}
	prov, ok := meta["provenance"].(map[string]interface{})
	if !ok {
		return ""
	}
	compute, _ := prov["compute"].(string)
	return compute
}

// =============================================================================
// Heartbeat-Driven Knowledge Lifecycle (Decay & Archival)
// =============================================================================

// DecayWeights applies exponential step-down decay to LTM memories whose
// updated_at predates the interval window. Each decayed memory has its weight
// reduced by at least 1 (floor = 1) and its updated_at refreshed so the same
// memory is not decayed again on the next heartbeat pass.
func (dm *DatabaseManager) DecayWeights(policies map[string]DecayPolicy, intervalDays int) (int, error) {
	if intervalDays <= 0 {
		intervalDays = 7
	}
	if policies == nil {
		policies = DefaultDecayPolicies
	}

	total := 0
	for collection, policy := range policies {
		if policy.DecayPercent <= 0 {
			continue
		}
		decayFactor := policy.DecayPercent / 100.0

		// The exponential step-down formula: subtract at least 1, never go below 1.
		// CAST to INTEGER truncates toward zero, so MAX(1, …) guarantees minimum delta.
		// Provenance multiplier via json_extract:
		//   absolute → * 0.0 (exempt), high → * 0.5, ephemeral → * 2.0, else → * 1.0
		result, err := dm.db.Exec(`
			UPDATE memories
			SET weight = MAX(weight - MAX(1, CAST(weight * ? *
			    COALESCE(
			        CASE json_extract(metadata, '$.provenance.compute')
			            WHEN 'absolute' THEN 0.0
			            WHEN 'high' THEN 0.5
			            WHEN 'ephemeral' THEN 2.0
			            ELSE 1.0
			        END, 1.0) AS INTEGER)), 1),
			    updated_at = STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')
			WHERE is_long_term = 1
			  AND deleted_at IS NULL
			  AND collection = ?
			  AND updated_at < DATETIME('now', '-' || ? || ' days')
			  AND weight > 1
		`, decayFactor, collection, intervalDays)
		if err != nil {
			return total, fmt.Errorf("DecayWeights: collection %s: %w", collection, err)
		}
		affected, _ := result.RowsAffected()
		total += int(affected)
	}
	return total, nil
}

// ArchiveStaleMemories soft-deletes weight=1 memories whose updated_at predates
// the archive window. The soft-delete trips the memories_rev_au trigger,
// capturing the terminal state in memory_revisions for --as-of retrospectivity.
// A hard LIMIT of 100 prevents un-indexed full-table sweeps under active WAL.
func (dm *DatabaseManager) ArchiveStaleMemories(archiveDays int) (int, error) {
	if archiveDays <= 0 {
		archiveDays = 30
	}
	result, err := dm.db.Exec(`
		UPDATE memories
		SET deleted_at = STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')
		WHERE id IN (
			SELECT id FROM memories
			WHERE weight = 1
			  AND deleted_at IS NULL
			  AND updated_at < DATETIME('now', '-' || ? || ' days')
			LIMIT 100
		)
	`, archiveDays)
	if err != nil {
		return 0, fmt.Errorf("ArchiveStaleMemories: %w", err)
	}
	affected, _ := result.RowsAffected()
	return int(affected), nil
}

// RunLifecycleDecayAndArchival runs a single heartbeat pass of the decay sweep
// followed by stale-memory archival. Both operations run on an isolated SQLite
// connection to avoid hot-path WAL contention with the fsnotify ingestion thread.
// Metrics are logged to mirror.jsonl as a structured [lifecycle] token line.
func (dm *DatabaseManager) RunLifecycleDecayAndArchival(decayRate float64, archiveDays int) error {
	// Reuse the existing DB connection pool to avoid WAL exhaustion
	return dm.runLifecycleInline(decayRate, archiveDays)
}

// runLifecycleInline executes the decay + archival pass on the current
// DatabaseManager connection. Extracted so both inline and isolated paths
// share the same logic.
func (dm *DatabaseManager) runLifecycleInline(decayRate float64, archiveDays int) error {
	intervalDays := archiveDays / 7
	if intervalDays < 1 {
		intervalDays = 1
	}

	// Apply the same decay rate to all non-audit collections
	nonAudit := []string{"default", "session", "memories", "theories"}
	policy := make(map[string]DecayPolicy, len(nonAudit))
	for _, c := range nonAudit {
		policy[c] = DecayPolicy{DecayPercent: decayRate, Floor: 1}
	}
	decayed, _ := dm.DecayWeights(policy, intervalDays)
	if decayed < 0 {
		decayed = 0
	}

	archived, _ := dm.ArchiveStaleMemories(archiveDays)
	if archived < 0 {
		archived = 0
	}

	line := fmt.Sprintf("[lifecycle] decay swept: %d weights adjusted, %d records archived to revisions ledger\n",
		decayed, archived)
	dm.logWatchdogRaw([]byte(line))
	return nil
}

// GetLessonStats returns statistics about lessons
func (dm *DatabaseManager) GetLessonStats() (map[string]interface{}, error) {
	stats := map[string]interface{}{
		"total_lessons": 0,
		"by_type":       map[string]int{},
	}

	var total int
	err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&total)
	if err != nil {
		return stats, err
	}
	stats["total_lessons"] = total

	typeCounts := map[string]int{}
	for _, t := range []LessonType{LessonTypeWarning, LessonTypePractice, LessonTypeInsight} {
		var count int
		err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons WHERE type = ?`, t).Scan(&count)
		if err == nil {
			typeCounts[string(t)] = count
		}
	}
	stats["by_type"] = typeCounts

	return stats, nil
}

// ProvenancePreamble extracts the provenance trust signal from metadata JSON
// and returns a formatted preamble string. Returns empty string if no provenance
// is found, allowing graceful degradation.
func ProvenancePreamble(metadataJSON string) string {
	if metadataJSON == "" || metadataJSON == "{}" || metadataJSON == "null" {
		return ""
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return ""
	}
	prov, ok := meta["provenance"].(map[string]interface{})
	if !ok {
		return ""
	}
	model, _ := prov["model"].(string)
	compute, _ := prov["compute"].(string)

	switch compute {
	case "absolute":
		return "[Source: Human | Authority: Absolute]"
	case "high":
		label := model
		if label == "" {
			label = "High-Compute"
		}
		return "[Source: " + label + " | Compute: High]"
	default:
		return ""
	}
}
