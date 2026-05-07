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

const maxHistoryMessages = 3

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
// Merges with existing history (keeps last 50 messages total) rather than replacing.
func (sm *SessionManager) Save(chatID int64, messages []map[string]interface{}) error {
	// Load existing history and merge (fixes overwriting bug)
	existing, _ := sm.Get(chatID)
	merged := append(existing, messages...)
	if len(merged) > maxHistoryMessages {
		merged = merged[len(merged)-maxHistoryMessages:]
	}

	// Serialize merged messages
	serializable := make([]SessionMessage, len(merged))
	for i, m := range merged {
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

// SessionSummary represents a one-line summary of a conversation session.
type SessionSummary struct {
	SessionID    string
	Summary      string
	LastTopic    string
	MessageCount int
	CreatedAt    string
	UpdatedAt    string
}

// UpdateSessionSummary creates or updates a session summary.
func (sm *SessionManager) UpdateSessionSummary(chatID int64, summary string, topic string) error {
	now := time.Now().Format(time.RFC3339)
	key := sessionKey(chatID)
	_, err := sm.db.Exec(`
		INSERT INTO session_summaries (session_id, summary, last_topic, message_count, created_at, updated_at)
		VALUES (?, ?, ?, (SELECT COUNT(*) FROM sessions WHERE session_id = ?), ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			summary = excluded.summary,
			last_topic = excluded.last_topic,
			message_count = excluded.message_count,
			updated_at = excluded.updated_at
	`, key, summary, topic, key, now, now)
	return err
}

// GetSessionSummaries returns recent session summaries for a chat.
func (sm *SessionManager) GetSessionSummaries(chatID int64, limit int) ([]SessionSummary, error) {
	key := sessionKey(chatID)
	rows, err := sm.db.Query(`
		SELECT session_id, summary, last_topic, message_count, created_at, updated_at
		FROM session_summaries
		WHERE session_id = ?
		ORDER BY updated_at DESC LIMIT ?
	`, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []SessionSummary
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(&s.SessionID, &s.Summary, &s.LastTopic, &s.MessageCount, &s.CreatedAt, &s.UpdatedAt); err != nil {
			continue
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// SetIdentityKnowledge creates or updates an identity fact about the user.
func (sm *SessionManager) SetIdentityKnowledge(key, value, source string) error {
	now := time.Now().Format(time.RFC3339)
	id := fmt.Sprintf("identity:%s", key)
	_, err := sm.db.Exec(`
		INSERT INTO identity_knowledge (id, key, value, source, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			source = excluded.source,
			updated_at = excluded.updated_at
	`, id, key, value, source, now)
	return err
}

// GetAllIdentityKnowledge returns all identity knowledge as a map.
func (sm *SessionManager) GetAllIdentityKnowledge() (map[string]string, error) {
	rows, err := sm.db.Query(`SELECT key, value FROM identity_knowledge`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		result[k] = v
	}
	return result, rows.Err()
}

// FrontCortex holds persistent context loaded on every agent call.
// All items respect hard caps to prevent token bloat.
type FrontCortex struct {
	UserName       string
	CurrentProject string
	ActiveWork     string
	Preferences    string
	RecentTopics   []string // last 3 session summaries
	Anchors        []string // "weight:N content" strings, last 10
}

// LoadFrontCortex builds front cortex from session summaries, identity knowledge, and anchors.
// Caps: 3 summaries, 10 anchors, identity keys truncated to 200 chars.
func (sm *SessionManager) LoadFrontCortex(chatID int64) *FrontCortex {
	fc := &FrontCortex{}

	// Identity knowledge
	if identity, _ := sm.GetAllIdentityKnowledge(); identity != nil {
		fc.UserName = truncateTo(identity["user_name"], 80)
		fc.CurrentProject = truncateTo(identity["current_project"], 80)
		fc.ActiveWork = truncateTo(identity["active_work"], 80)
		fc.Preferences = truncateTo(identity["preferences"], 80)
	}

	// Session summaries (last 3)
	summaries, _ := sm.GetSessionSummaries(chatID, 3)
	for _, s := range summaries {
		fc.RecentTopics = append(fc.RecentTopics, truncateTo(s.Summary, 100))
	}

	return fc
}

// FormatFrontCortex renders a FrontCortex as a system prompt section with hard caps.
func FormatFrontCortex(fc *FrontCortex) string {
	if fc == nil {
		return ""
	}
	var sb strings.Builder

	// Identity section (max 200 chars per field, max 4 fields = 800 chars)
	hasIdentity := fc.UserName != "" || fc.CurrentProject != "" || fc.ActiveWork != "" || fc.Preferences != ""
	if hasIdentity {
		sb.WriteString("\n## Who I'm talking to\n")
		if fc.UserName != "" {
			sb.WriteString(fmt.Sprintf("- User: %s\n", fc.UserName))
		}
		if fc.CurrentProject != "" {
			sb.WriteString(fmt.Sprintf("- Project: %s\n", fc.CurrentProject))
		}
		if fc.ActiveWork != "" {
			sb.WriteString(fmt.Sprintf("- Active work: %s\n", fc.ActiveWork))
		}
		if fc.Preferences != "" {
			sb.WriteString(fmt.Sprintf("- Preferences: %s\n", fc.Preferences))
		}
	}

	// Recent context (max 3 summaries, 200 chars each)
	if len(fc.RecentTopics) > 0 {
		sb.WriteString("\n## Recent context\n")
		for _, topic := range fc.RecentTopics {
			sb.WriteString(fmt.Sprintf("- %s\n", topic))
		}
	}

	return sb.String()
}

func truncateTo(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// GetIdentityKnowledge returns a specific identity key.
func (sm *SessionManager) GetIdentityKnowledge(key string) (string, error) {
	var value string
	err := sm.db.QueryRow(`SELECT value FROM identity_knowledge WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// initSessionDB creates the full mini-bot.db schema (sessions, memories, lessons, anchors, tools, session_summaries, identity_knowledge).
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

	CREATE TABLE IF NOT EXISTS session_summaries (
		session_id TEXT PRIMARY KEY,
		summary TEXT NOT NULL,
		last_topic TEXT,
		message_count INTEGER DEFAULT 0,
		created_at TEXT,
		updated_at TEXT
	);

	CREATE TABLE IF NOT EXISTS identity_knowledge (
		id TEXT PRIMARY KEY,
		key TEXT UNIQUE NOT NULL,
		value TEXT NOT NULL,
		source TEXT DEFAULT 'inferred',
		updated_at TEXT
	);
	`

	_, err = db.Exec(schema)
	return err
}
