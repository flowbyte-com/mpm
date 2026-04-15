package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// SessionManager maps a Telegram chat ID to its conversation message history.
// History is persisted to the MPM SQLite database.
type SessionManager struct {
	db           *sql.DB
	writeTimeout time.Duration // Max time to wait for a write lock
}

// SessionMessage represents a single message in a Telegram conversation history.
type SessionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LoadSessionManager opens the mini-bot database and returns a SessionManager.
func LoadSessionManager(dbPath string) (*SessionManager, error) {
	// Ensure schema exists (sessions table only, no FTS5 needed)
	if err := initSessionDB(dbPath); err != nil {
		return nil, fmt.Errorf("init session db: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open mpm.db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to mpm.db: %w", err)
	}
	return &SessionManager{db: db, writeTimeout: 5 * time.Second}, nil
}

// Close closes the underlying database connection.
func (sm *SessionManager) Close() error {
	return sm.db.Close()
}

// sessionKey returns the session_id used for a Telegram chat.
func sessionKey(chatID int64) string {
	return fmt.Sprintf("telegram:%d", chatID)
}

// Get returns the conversation history for a chat, loaded from SQLite.
// Returns nil if no session exists.
func (sm *SessionManager) Get(chatID int64) ([]map[string]interface{}, error) {
	key := sessionKey(chatID)
	row := sm.db.QueryRow(`
		SELECT metadata FROM sessions
		WHERE session_id = ? AND source_path = 'telegram'
		ORDER BY created_at DESC LIMIT 1
	`, key)

	var raw string
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("load session: %w", err)
	}

	var messages []SessionMessage
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		return nil, fmt.Errorf("parse session history: %w", err)
	}

	result := make([]map[string]interface{}, len(messages))
	for i, m := range messages {
		result[i] = map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		}
	}
	return result, nil
}

// Save persists the conversation history for a chat to SQLite.
// Retries on database lock with exponential backoff.
func (sm *SessionManager) Save(chatID int64, messages []map[string]interface{}) error {
	// Serialize messages
	serializable := make([]SessionMessage, len(messages))
	for i, m := range messages {
		serializable[i] = SessionMessage{
			Role:    roleOf(m),
			Content: contentOf(m),
		}
	}

	raw, err := json.Marshal(serializable)
	if err != nil {
		return fmt.Errorf("serialize history: %w", err)
	}

	now := time.Now().Format(time.RFC3339)
	id := fmt.Sprintf("telegram:%d", chatID)
	key := sessionKey(chatID)

	// Retry loop for SQLite lock contention
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(50*(attempt)) * time.Millisecond)
		}
		_, err = sm.db.Exec(`
			INSERT INTO sessions (id, session_id, content, content_hash, created_at, source_path, metadata)
			VALUES (?, ?, '', ?, ?, 'telegram', ?)
			ON CONFLICT(id) DO UPDATE SET
				metadata = excluded.metadata,
				created_at = excluded.created_at,
				content_hash = excluded.content_hash
		`, id, key, now, now, string(raw))
		if err == nil {
			return nil
		}
		lastErr = err
		// Only retry on lock errors
		if !strings.Contains(err.Error(), "database is locked") &&
			!strings.Contains(err.Error(), "SQLITE_BUSY") {
			break
		}
	}
	return fmt.Errorf("save session: %w", lastErr)
}

func roleOf(m map[string]interface{}) string {
	if r, ok := m["role"].(string); ok {
		return r
	}
	return "user"
}

func contentOf(m map[string]interface{}) string {
	switch v := m["content"].(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, part := range v {
			if pm, ok := part.(map[string]interface{}); ok {
				if t, ok := pm["type"].(string); ok && t == "text" {
					if txt, ok := pm["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// initSessionDB creates the full mini-bot.db schema (sessions, memories, lessons, anchors, tools).
func initSessionDB(dbPath string) error {
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
		content TEXT NOT NULL,
		context TEXT,
		weight INTEGER DEFAULT 1,
		session_id TEXT,
		created_at TEXT,
		UNIQUE(content, context, session_id)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_anchors_dedup ON anchors(content, context, session_id);

	CREATE TABLE IF NOT EXISTS tools (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE,
		description TEXT,
		definition TEXT,
		source TEXT DEFAULT 'self',
		created_at TEXT
	);
	`

	_, err = db.Exec(schema)
	return err
}
