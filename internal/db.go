// db.go - Unified SQLite database for MPM
// Version: 2026-03-28 (Single Database Refactor)
// Description: Manages a single unified database at src/db/mpm.db

package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mpm/internal/config"

	_ "github.com/mattn/go-sqlite3"
)

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
	return &SQLiteConnection{DB: db}, nil
}

// WipeRecord performs a hard delete with VACUUM (on SQLiteConnection for backwards compatibility)
func (sc *SQLiteConnection) WipeRecord(tier, id string) error {
	if tier != "sessions" && tier != "memories" && tier != "topics" {
		return fmt.Errorf("cannot wipe from tier: %s", tier)
	}
	tx, err := sc.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", tier), id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = sc.DB.Exec("VACUUM")
	return err
}

// ShredDatabase securely wipes the entire database and recreates it fresh
func ShredDatabase(dbPath string) error {
	// Close existing connections
	if db, err := NewSQLiteConnection(dbPath); err == nil {
		db.Close()
	}

	// Overwrite with zeros before delete (secure delete simulation)
	if data, err := os.ReadFile(dbPath); err == nil {
		zeroed := make([]byte, len(data))
		os.WriteFile(dbPath, zeroed, 0600)
	}

	// Remove the database file
	if err := os.Remove(dbPath); err != nil {
		return fmt.Errorf("failed to remove database: %w", err)
	}

	// Remove WAL and SHM files if they exist
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	os.Remove(dbPath + "-journal")

	// Recreate fresh database
	store := NewMemoryStore("")
	store.InitSQLite()

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
	db *sql.DB
}

// SQLDB returns the underlying *sql.DB for direct queries.
func (dm *DatabaseManager) SQLDB() *sql.DB {
	return dm.db
}

// IsOpen returns true if the database connection is non-nil.
func (dm *DatabaseManager) IsOpen() bool {
	return dm.db != nil
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

	manager := &DatabaseManager{db: db}

	if err := manager.initUnifiedSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return manager, nil
}

// NewDatabaseManagerForDB creates a DatabaseManager wrapping an existing *sql.DB.
// Use this for one-off CLI commands that don't need daemon persistence.
func NewDatabaseManagerForDB(db *sql.DB) *DatabaseManager {
	return &DatabaseManager{db: db}
}

func (dm *DatabaseManager) initUnifiedSchema() error {
	// Base tables (always created)
	baseStatements := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, content TEXT NOT NULL,
			content_hash TEXT UNIQUE NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			source_path TEXT, metadata JSON, embedding BLOB
		);`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_session_id ON sessions(session_id);`,

		`CREATE TABLE IF NOT EXISTS topics (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT,
			parent_topic_id TEXT, tags JSON, is_active INTEGER DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, embedding BLOB
		);`,

		`CREATE TABLE IF NOT EXISTS topic_memberships (
			memory_id TEXT, session_id TEXT, topic_id TEXT NOT NULL,
			role TEXT DEFAULT 'related', created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (memory_id, topic_id),
			UNIQUE (session_id, topic_id),
			CHECK (memory_id IS NOT NULL OR session_id IS NOT NULL)
		);`,

		`CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
			session_id TEXT, tags JSON, metadata JSON, embedding BLOB,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL
		);`,

		`CREATE TABLE IF NOT EXISTS modes (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, content TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,

		`CREATE TABLE IF NOT EXISTS personas (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, content TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,

		`CREATE TABLE IF NOT EXISTS system_config (
			key TEXT PRIMARY KEY,
			raw_json TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			config_snapshot JSON
		);`,

		`CREATE TABLE IF NOT EXISTS lessons (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL DEFAULT 'insight',
			content TEXT NOT NULL,
			tags JSON,
			reinforcement_count INTEGER DEFAULT 1,
			source_session_id TEXT,
			created TEXT NOT NULL,
			content_hash TEXT
		);`,

		`CREATE TABLE IF NOT EXISTS raw_memories (
			id            TEXT PRIMARY KEY,
			source_id     TEXT,
			source_db     TEXT NOT NULL DEFAULT 'openclaw',
			content_hash  TEXT NOT NULL,
			text          TEXT NOT NULL,
			metadata      TEXT NOT NULL,
			ingested_at   REAL NOT NULL,
			status        TEXT NOT NULL DEFAULT 'pending',
			llm_verdict   TEXT,
			llm_notes     TEXT,
			reviewer_prompt TEXT,
			expires_at    REAL,
			import_batch  TEXT,
			updated_at    REAL NOT NULL
		);`,

		`CREATE TABLE IF NOT EXISTS external_db_cursors (
			db_label TEXT PRIMARY KEY,
			last_cursor TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,
	}

	for _, sqlQuery := range baseStatements {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute SQL: %w\nSQL: %s", err, sqlQuery)
		}
	}

	// Migration: add new columns to existing databases (no-op if already present)
	migrations := []string{
		`ALTER TABLE topics ADD COLUMN parent_topic_id TEXT`,
		`ALTER TABLE topics ADD COLUMN tags JSON`,
		`ALTER TABLE topics ADD COLUMN is_active INTEGER DEFAULT 1`,
		`ALTER TABLE topics ADD COLUMN updated_at DATETIME`,
		`ALTER TABLE topic_memberships ADD COLUMN memory_id TEXT`,
		`ALTER TABLE topic_memberships ADD COLUMN role TEXT DEFAULT 'related'`,
		`ALTER TABLE memories ADD COLUMN session_id TEXT`,
		// v5: add summary column to sessions for LLM-generated session summaries
		`ALTER TABLE sessions ADD COLUMN summary TEXT`,
		// v6 ingest: source tracking on memories
		`ALTER TABLE memories ADD COLUMN source_db TEXT`,
		`ALTER TABLE memories ADD COLUMN source_id TEXT`,
		`ALTER TABLE memories ADD COLUMN promoted_at REAL`,
		`ALTER TABLE memories ADD COLUMN deleted_at DATETIME`,
	}
	for _, sql := range migrations {
		dm.db.Exec(sql) // SQLite ignores duplicate column errors
	}

	// Indexes for fast lookups
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_topic_memberships_topic ON topic_memberships(topic_id)`,
		`CREATE INDEX IF NOT EXISTS idx_topic_memberships_memory ON topic_memberships(memory_id)`,
		`CREATE INDEX IF NOT EXISTS idx_topic_memberships_session ON topic_memberships(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_topics_parent ON topics(parent_topic_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_session ON memories(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_collection ON memories(collection)`,
		`CREATE INDEX IF NOT EXISTS idx_lessons_type ON lessons(type)`,
		`CREATE INDEX IF NOT EXISTS idx_lessons_reinforcement ON lessons(reinforcement_count)`,
		// v6 ingest: raw_memories indexes
		`CREATE INDEX IF NOT EXISTS idx_raw_memories_status ON raw_memories(status)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_memories_expires ON raw_memories(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_memories_import_batch ON raw_memories(import_batch)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_raw_memories_source_dedup ON raw_memories(source_db, source_id)`,
	}
	for _, sql := range indexes {
		dm.db.Exec(sql)
	}

	// Try FTS5 tables - if they fail, continue without them (fallback search)
	dm.initFTSTables()

	return nil
}

// initFTSTables attempts to create FTS5 virtual tables
// If FTS5 is not available (pure Go sqlite build), this silently skips
func (dm *DatabaseManager) initFTSTables() {
	// Test if FTS5 is available
	var available int
	dm.db.QueryRow("SELECT 1 FROM pragma_compile_options WHERE compile_options LIKE '%FTS5%'").Scan(&available)
	if available == 0 {
		// Try a simple FTS5 table to confirm availability
		testDB, err := sql.Open("sqlite3", ":memory:")
		if err == nil {
			_, err = testDB.Exec("CREATE VIRTUAL TABLE test_fts USING fts5(content);")
			testDB.Close()
			if err != nil {
				// FTS5 not available
				return
			}
		} else {
			return
		}
	}

	ftsStatements := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(content, session_id, content_hash UNINDEXED, tokenize='porter');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, collection, session_id UNINDEXED, tags UNINDEXED, tokenize='porter');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS topics_fts USING fts5(name, description, tokenize='porter');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter unicode61');`,

		`CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,

		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,

		`CREATE TRIGGER IF NOT EXISTS lessons_ai AFTER INSERT ON lessons BEGIN INSERT INTO lessons_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS lessons_ad AFTER DELETE ON lessons BEGIN DELETE FROM lessons_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS lessons_au AFTER UPDATE ON lessons BEGIN DELETE FROM lessons_fts WHERE rowid = old.rowid; INSERT INTO lessons_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;`,
	}

	for _, sqlQuery := range ftsStatements {
		dm.db.Exec(sqlQuery) // Ignore errors - FTS5 is optional
	}
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

func (dm *DatabaseManager) SaveMemory(collection, content, sessionID string, tags, metadata map[string]interface{}, embedding []float32) (string, error) {
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

	_, err := dm.db.Exec(`INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, collection, content, sessionIDVal, string(tagsJSON), string(metadataJSON), embeddingJSON)
	return id, err
}

func (dm *DatabaseManager) SaveMode(name, content string) (string, error) {
	id := GenerateID()
	_, err := dm.db.Exec(`INSERT OR REPLACE INTO modes (id, name, content) VALUES (?, ?, ?)`, id, name, content)
	return id, err
}

func (dm *DatabaseManager) SavePersona(name, content string) (string, error) {
	id := GenerateID()
	_, err := dm.db.Exec(`INSERT OR REPLACE INTO personas (id, name, content) VALUES (?, ?, ?)`, id, name, content)
	return id, err
}

// SaveSystemConfig stores or updates a system config entry (keyed by source file name)
// Only updates if the content hash has changed (skip duplicate writes)
// Returns (updated bool, error)
func (dm *DatabaseManager) SaveSystemConfig(key, rawJSON, contentHash string, snapshotJSON string) (bool, error) {
	var existingHash string
	err := dm.db.QueryRow(`SELECT content_hash FROM system_config WHERE key = ?`, key).Scan(&existingHash)
	if err == nil && existingHash == contentHash {
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

	query := fmt.Sprintf(`SELECT id, content, embedding, created_at FROM %s WHERE embedding IS NOT NULL AND embedding != 'null'`, tier)
	rows, err := dm.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []searchResult
	for rows.Next() {
		var id, content, embeddingJSON string
		var createdAt time.Time
		if err := rows.Scan(&id, &content, &embeddingJSON, &createdAt); err != nil {
			continue
		}

		var dbEmbedding []float32
		if err := json.Unmarshal([]byte(embeddingJSON), &dbEmbedding); err != nil || len(dbEmbedding) != len(queryEmbedding) {
			continue
		}

		sim := cosineSimilarity(queryEmbedding, dbEmbedding)
		results = append(results, searchResult{id, content, createdAt, sim})
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

// WipeRecord performs a hard delete with VACUUM (on DatabaseManager)
func (dm *DatabaseManager) WipeRecord(tier, id string) error {
	if tier != "sessions" && tier != "memories" && tier != "topics" {
		return fmt.Errorf("cannot wipe from tier: %s", tier)
	}
	tx, err := dm.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", tier), id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = dm.db.Exec("VACUUM")
	return err
}

// ShredMemory performs a hard delete on memories table with VACUUM
func ShredMemory(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec("DELETE FROM memories WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = db.Exec("VACUUM")
	return err
}

// ShredSession performs a hard delete on sessions table with VACUUM
func ShredSession(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec("DELETE FROM sessions WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = db.Exec("VACUUM")
	return err
}

// DeleteByID performs a hard delete with VACUUM (for general use)
func DeleteByID(db *sql.DB, table, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", table), id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = db.Exec("VACUUM")
	return err
}

// ShredTopic performs a hard delete on topics table with VACUUM
// Also deletes associated topic_memberships entries first
func ShredTopic(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete memberships first (explicit, for clarity)
	if _, err = tx.Exec("DELETE FROM topic_memberships WHERE topic_id = ?", id); err != nil {
		return err
	}

	// Delete the topic
	if _, err = tx.Exec("DELETE FROM topics WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = db.Exec("VACUUM")
	return err
}

// ==================== HELPERS ====================

func GenerateID() string {
	timestamp := time.Now().UnixNano()
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d", timestamp)))
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
		if err == nil {
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

// DeleteTopic soft-deletes a topic (memories are NOT deleted)
func (dm *DatabaseManager) DeleteTopic(topicID string) error {
	_, err := dm.db.Exec(`DELETE FROM topic_memberships WHERE topic_id = ?`, topicID)
	if err != nil {
		return err
	}
	_, err = dm.db.Exec(`UPDATE topics SET is_active = 0 WHERE id = ?`, topicID)
	return err
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
		FROM lessons_fts
		JOIN lessons l ON lessons_fts.rowid = l.rowid
		WHERE lessons_fts MATCH ?
		ORDER BY bm25(lessons_fts)
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
