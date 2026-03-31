// db.go - Unified SQLite database for MPM
// Version: 2026-03-28 (Single Database Refactor)
// Description: Manages a single unified database at src/db/mpm_memory.db

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
	"time"

	_ "github.com/mattn/go-sqlite3"
)

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
	DB *sql.DB
}

// NewDatabaseManager creates a new database manager with single unified database
func NewDatabaseManager(projectRoot string) (*DatabaseManager, error) {
	dbDir := filepath.Join(projectRoot, "src", "db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	dbPath := filepath.Join(dbDir, "mpm_memory.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.Exec("PRAGMA foreign_keys = ON")
	db.Exec("PRAGMA journal_mode = WAL")
	db.Exec("PRAGMA synchronous = NORMAL")
	db.Exec("PRAGMA cache_size = -64000")

	manager := &DatabaseManager{DB: db}

	if err := manager.initUnifiedSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return manager, nil
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
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, embedding BLOB
		);`,

		`CREATE TABLE IF NOT EXISTS topic_memberships (
			session_id TEXT NOT NULL, topic_id TEXT NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, topic_id),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
			FOREIGN KEY (topic_id) REFERENCES topics(id) ON DELETE CASCADE
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
	}

	for _, sqlQuery := range baseStatements {
		if _, err := dm.DB.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute SQL: %w\nSQL: %s", err, sqlQuery)
		}
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
	dm.DB.QueryRow("SELECT 1 FROM pragma_compile_options WHERE compile_options LIKE '%FTS5%'").Scan(&available)
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

		`CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,

		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,
	}

	for _, sqlQuery := range ftsStatements {
		dm.DB.Exec(sqlQuery) // Ignore errors - FTS5 is optional
	}
}

func (dm *DatabaseManager) Close() error {
	if dm.DB != nil {
		return dm.DB.Close()
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

	_, err := dm.DB.Exec(`INSERT INTO sessions (id, session_id, content, content_hash, source_path, metadata) VALUES (?, ?, ?, ?, ?, ?)`,
		id, sessionID, content, contentHash, sourcePath, metadataJSON)
	return id, err
}

func (dm *DatabaseManager) GetSession(id string) (map[string]interface{}, error) {
	var sessionID, content, contentHash, sourcePath, metadataJSON string
	var createdAt time.Time

	err := dm.DB.QueryRow(`SELECT session_id, content, content_hash, source_path, metadata, created_at FROM sessions WHERE id = ?`, id).
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

	_, err := dm.DB.Exec(`INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, collection, content, sessionIDVal, string(tagsJSON), string(metadataJSON), embeddingJSON)
	return id, err
}

func (dm *DatabaseManager) SaveMode(name, content string) (string, error) {
	id := GenerateID()
	_, err := dm.DB.Exec(`INSERT OR REPLACE INTO modes (id, name, content) VALUES (?, ?, ?)`, id, name, content)
	return id, err
}

func (dm *DatabaseManager) SavePersona(name, content string) (string, error) {
	id := GenerateID()
	_, err := dm.DB.Exec(`INSERT OR REPLACE INTO personas (id, name, content) VALUES (?, ?, ?)`, id, name, content)
	return id, err
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
	rows, err := dm.DB.Query(query)
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
	tx, err := dm.DB.Begin()
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

	_, err = dm.DB.Exec("VACUUM")
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
