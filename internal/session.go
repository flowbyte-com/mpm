package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SessionStore struct {
	DB          *DatabaseManager
	BasePath    string
	MemoryStore *MemoryStore // Embedded for backwards compat
}

// NewSessionStoreWithDB creates a SessionStore with a DatabaseManager (new way)
func NewSessionStoreWithDB(db *DatabaseManager, basePath string) *SessionStore {
	return &SessionStore{
		DB:          db,
		BasePath:    basePath,
		MemoryStore: NewMemoryStore(basePath),
	}
}

// NewSessionStore creates a SessionStore (backwards compatible - accepts 1 or 2 args)
func NewSessionStore(args ...string) *SessionStore {
	var basePath string
	if len(args) >= 1 {
		basePath = args[0]
	}
	// Create MemoryStore first and initialize its DB
	memoryStore := NewMemoryStore(basePath)
	if err := memoryStore.InitSQLite(); err != nil {
		// Return store anyway - count queries will fail gracefully
		return &SessionStore{
			DB:          nil,
			BasePath:    basePath,
			MemoryStore: memoryStore,
		}
	}
	// Share the same initialized DB connection
	dm := &DatabaseManager{db: memoryStore.DB.DB}
	return &SessionStore{
		DB:          dm,
		BasePath:    basePath,
		MemoryStore: memoryStore,
	}
}

// SaveSnapshot cleans up the ID inconsistency and removes ghost logging.
func (ss *SessionStore) SaveSnapshot(sessionID string, content string, tags []string, privacyStr string) (*Session, error) {
	if ss.DB == nil {
		return nil, fmt.Errorf("database connection not initialized")
	}

	// Metadata session_id now strictly matches the lookup sessionID
	metadata := map[string]interface{}{
		"session_id": sessionID,
		"timestamp":  time.Now().Unix(),
		"tags":       tags,
		"privacy":    privacyStr,
	}

	// GenerateID is exported from db.go
	id, err := ss.DB.SaveSession(sessionID, content, "cli_snapshot", metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to save session snapshot: %w", err)
	}

	return &Session{
		ID:      id,
		SessionID: sessionID,
		Content: content,
		Type:    "cli_snapshot",
		Created: time.Now().Format(time.RFC3339),
	}, nil
}

// Session represents a session
type Session struct {
	ID         string
	SessionID  string
	Content    string
	Type       string
	Created    string
	StartTime  string
	EndTime    string
	Summary    string
	Tags       []string
	Notes      []string
	WorkingDir string
	GitBranch  string
	GitCommit  string
	Persona    string
	Modes      []string
	Privacy    string
	Metadata   map[string]interface{}
}

// GetSession retrieves a session by ID
func (ss *SessionStore) GetSession(sessionID string) (*Session, error) {
	if ss.DB == nil {
		return nil, fmt.Errorf("database connection not initialized")
	}
	// Find by session_id field
	query := `SELECT id, session_id, content, created_at, metadata FROM sessions WHERE session_id = ? LIMIT 1`
	row := ss.DB.db.QueryRow(query, sessionID)

	var id, sessID, content, createdAt, metadataJSON string
	err := row.Scan(&id, &sessID, &content, &createdAt, &metadataJSON)
	if err != nil {
		return nil, err
	}

	var metadata map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️ getSnapshot: failed to unmarshal metadata for session %s: %v\n", sessionID, err)
	}

	return &Session{
		ID:        id,
		SessionID: sessID,
		Content:   content,
		Created:   createdAt,
		Type:      "cli_snapshot",
		Metadata:  metadata,
	}, nil
}

// GetTodaySessions returns sessions from today
func (ss *SessionStore) GetTodaySessions() ([]*Session, error) {
	if ss.DB == nil {
		return nil, fmt.Errorf("database connection not initialized")
	}
	query := `SELECT id, session_id, content, created_at FROM sessions WHERE date(created_at) = date('now') ORDER BY created_at DESC`
	return ss.querySessions(query)
}

// GetYesterdaySessions returns sessions from yesterday
func (ss *SessionStore) GetYesterdaySessions() ([]*Session, error) {
	if ss.DB == nil {
		return nil, fmt.Errorf("database connection not initialized")
	}
	query := `SELECT id, session_id, content, created_at FROM sessions WHERE date(created_at) = date('now', '-1 day') ORDER BY created_at DESC`
	return ss.querySessions(query)
}

func (ss *SessionStore) querySessions(query string, args ...interface{}) ([]*Session, error) {
	rows, err := ss.DB.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*Session
	for rows.Next() {
		var id, sessionID, content, createdAt string
		if err := rows.Scan(&id, &sessionID, &content, &createdAt); err != nil {
			continue
		}
		sessions = append(sessions, &Session{
			ID:        id,
			SessionID: sessionID,
			Content:   content,
			Created:   createdAt,
		})
	}
	return sessions, nil
}

// StartSession creates a new session record
func (ss *SessionStore) StartSession(sessionID string) (*Session, error) {
	return &Session{
		ID:        sessionID,
		StartTime: time.Now().Format(time.RFC3339),
	}, nil
}

// EndSession marks a session as ended
func (ss *SessionStore) EndSession(sessionID string, summary string) error {
	return nil // No-op for now, sessions are immutable snapshots
}

func (ss *SessionStore) getActivePersona() string {
	if ss.BasePath == "" {
		return "default"
	}
	path := filepath.Join(ss.BasePath, "persona", "active.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "default"
	}

	var active struct {
		Persona string `json:"persona"`
	}
	if err := json.Unmarshal(data, &active); err != nil {
		return "default"
	}
	return active.Persona
}

func (ss *SessionStore) getActiveModes() []string {
	if ss.BasePath == "" {
		return []string{"standard"}
	}
	path := filepath.Join(ss.BasePath, "mode", "active.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{"standard"}
	}

	var active struct {
		Modes []string `json:"modes"`
	}
	if err := json.Unmarshal(data, &active); err != nil {
		return []string{"standard"}
	}
	return active.Modes
}

// GetSessionCount returns the number of sessions
func (ss *SessionStore) GetSessionCount() (int, error) {
	if ss.DB == nil {
		return 0, fmt.Errorf("database not initialized")
	}
	var count int
	err := ss.DB.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count)
	return count, err
}

// GetRecentSessions returns the most recent sessions ordered by creation date
func (ss *SessionStore) GetRecentSessions(limit int) ([]*Session, error) {
	if ss.DB == nil {
		return nil, fmt.Errorf("database connection not initialized")
	}
	query := `SELECT id, session_id, content, created_at FROM sessions ORDER BY created_at DESC LIMIT ?`
	return ss.querySessions(query, limit)
}
