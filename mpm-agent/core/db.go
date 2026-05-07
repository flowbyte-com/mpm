package core

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var MiniBotDBPath string

// ResolveMiniBotDBPath resolves the mini-bot.db path from binary directory.
func ResolveMiniBotDBPath() string {
	if MiniBotDBPath != "" {
		return MiniBotDBPath
	}
	binaryDir := GetBinaryDir()
	if binaryDir == "" {
		return "mini-bot.db"
	}
	return filepath.Join(binaryDir, "mini-bot.db")
}

// InitMiniBotDB creates the mini-bot.db schema if it doesn't exist.
func InitMiniBotDB(dbPath string) error {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("ensure dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()

	schema := `
	CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		collection TEXT DEFAULT 'general',
		content TEXT NOT NULL,
		session_id TEXT,
		tags TEXT,
		metadata TEXT,
		created_at TEXT,
		deleted_at TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_memories_session ON memories(session_id);
	CREATE INDEX IF NOT EXISTS idx_memories_created ON memories(created_at);
	CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, tags, content=memories, content_rowid=rowid);
	CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;
	CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;
	CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; INSERT INTO memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;

	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		session_id TEXT,
		content TEXT,
		content_hash TEXT,
		created_at TEXT,
		source_path TEXT,
		metadata TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_sessions_session ON sessions(session_id);

	CREATE TABLE IF NOT EXISTS lessons (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL UNIQUE,
		type TEXT DEFAULT 'insight',
		tags TEXT,
		created_at TEXT,
		reinforcement_count INTEGER DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS anchors (
		id TEXT PRIMARY KEY,
		facts TEXT,
		summary TEXT NOT NULL,
		tags TEXT,
		context TEXT,
		weight INTEGER DEFAULT 1,
		session_id TEXT,
		reference_count INTEGER DEFAULT 0,
		expires_at TEXT,
		created_at TEXT,
		is_condensed INTEGER DEFAULT 0,
		ancestor_ids TEXT,
		UNIQUE(summary, context, session_id)
	);
	CREATE INDEX IF NOT EXISTS idx_anchors_expires ON anchors(expires_at);
	CREATE INDEX IF NOT EXISTS idx_anchors_weight ON anchors(weight DESC);
	CREATE INDEX IF NOT EXISTS idx_anchors_condensed ON anchors(is_condensed) WHERE is_condensed = 0;

	CREATE TABLE IF NOT EXISTS tools (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE,
		description TEXT,
		definition TEXT,
		source TEXT DEFAULT 'self',
		created_at TEXT
	);
	`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// OpenDBForPath opens a SQLite db at a specific path.
func OpenDBForPath(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// GenerateID returns a random hex string ID for database records.
// Uses crypto/rand which can only fail in theoretically impossible conditions
// (no entropy available), so error is ignored.
func GenerateID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck // crypto/rand only fails if system has no entropy
	return fmt.Sprintf("%x", b)
}

// SetIdentityKnowledge creates or updates an identity fact in the identity_knowledge table.
func SetIdentityKnowledge(dbPath, key, value, source string) error {
	db, err := OpenDBForPath(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// Ensure identity_knowledge table exists
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS identity_knowledge (
		id TEXT PRIMARY KEY,
		key TEXT UNIQUE NOT NULL,
		value TEXT NOT NULL,
		source TEXT DEFAULT 'inferred',
		updated_at TEXT
	)`)

	now := time.Now().Format(time.RFC3339)
	id := fmt.Sprintf("identity:%s", key)
	_, err = db.Exec(`
		INSERT INTO identity_knowledge (id, key, value, source, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			source = excluded.source,
			updated_at = excluded.updated_at
	`, id, key, value, source, now)
	return err
}
