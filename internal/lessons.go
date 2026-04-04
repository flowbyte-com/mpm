package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mpm/internal/config"
)

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

// LessonStore manages lessons in SQLite
type LessonStore struct {
	DatabasePath string
	db           *SQLiteConnection
}

// NewLessonStore creates a new lesson store
func NewLessonStore(dbPath string) *LessonStore {
	if dbPath == "" {
		mpmDir := config.GetMPMDir()
		dbPath = filepath.Join(mpmDir, "src", "db", "mpm.db")
	}
	return &LessonStore{DatabasePath: dbPath}
}

// Init initializes the lesson database and creates tables
func (s *LessonStore) Init() error {
	err := os.MkdirAll(filepath.Dir(s.DatabasePath), 0755)
	if err != nil {
		return err
	}

	database, err := NewSQLiteConnection(s.DatabasePath)
	if err != nil {
		return err
	}
	s.db = database

	// Create lessons table
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS lessons (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL DEFAULT 'insight',
			content TEXT NOT NULL,
			tags JSON,
			reinforcement_count INTEGER DEFAULT 1,
			source_session_id TEXT,
			created TEXT NOT NULL,
			content_hash TEXT
		)
	`)
	if err != nil {
		return err
	}

	// Create index for type and reinforcement
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_lessons_type ON lessons(type)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_lessons_reinforcement ON lessons(reinforcement_count)`)
	if err != nil {
		return err
	}

	// Create FTS5 virtual table for full-text search on lessons
	var fts5Available bool
	err = s.db.QueryRow("SELECT 1 FROM sqlite_master WHERE type='table' AND name='lessons_fts'").Scan(&fts5Available)
	if err == sql.ErrNoRows {
		_, err = s.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter unicode61');`)
		if err == nil {
			fts5Available = true
		}
	} else if err == nil {
		fts5Available = true
	}

	// Create FTS5 triggers
	if fts5Available {
		_, _ = s.db.Exec(`CREATE TRIGGER IF NOT EXISTS lessons_ai AFTER INSERT ON lessons BEGIN INSERT INTO lessons_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;`)
		_, _ = s.db.Exec(`CREATE TRIGGER IF NOT EXISTS lessons_ad AFTER DELETE ON lessons BEGIN DELETE FROM lessons_fts WHERE rowid = old.rowid; END;`)
		_, _ = s.db.Exec(`CREATE TRIGGER IF NOT EXISTS lessons_au AFTER UPDATE ON lessons BEGIN DELETE FROM lessons_fts WHERE rowid = old.rowid; INSERT INTO lessons_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;`)
	}

	return nil
}

// addColumnIfNotExists adds a column to a table if it doesn't exist (migration helper)
func (s *LessonStore) addColumnIfNotExists(table, column, colType string) error {
	_, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colType))
	return err
}

// GenerateLessonID generates a unique lesson ID
func GenerateLessonID() string {
	timestamp := time.Now().UnixNano()
	hash := sha256.Sum256([]byte(fmt.Sprintf("lesson-%d-%d", timestamp, time.Now().UnixNano())))
	return hex.EncodeToString(hash[:])[:12]
}

// AddLesson adds a new lesson, checking for duplicates by content hash
func (s *LessonStore) AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("lesson store not initialized")
	}

	// Compute content hash for deduplication
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	// Check for existing lesson with same content
	existing, err := s.GetLessonByHash(contentHash)
	if err == nil && existing != nil {
		// Reinforce existing lesson instead of creating duplicate
		existing.ReinforcementCount++
		_, err = s.db.Exec(`UPDATE lessons SET reinforcement_count = ? WHERE id = ?`, existing.ReinforcementCount, existing.ID)
		if err != nil {
			return nil, err
		}
		return existing, nil
	}

	// Create new lesson
	lesson := &Lesson{
		ID:                 GenerateLessonID(),
		Type:               lessonType,
		Content:            content,
		Tags:               tags,
		ReinforcementCount: 1,
		SourceSessionID:    sourceSessionID,
		Created:            time.Now().Format(time.RFC3339),
	}

	tagsJSON, _ := MarshalJSON(tags)
	_, err = s.db.Exec(`
		INSERT INTO lessons (id, type, content, tags, reinforcement_count, source_session_id, created, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, lesson.ID, lesson.Type, lesson.Content, tagsJSON, lesson.ReinforcementCount, lesson.SourceSessionID, lesson.Created, contentHash)
	if err != nil {
		return nil, err
	}

	return lesson, nil
}

// GetLessonByHash retrieves a lesson by its content hash
func (s *LessonStore) GetLessonByHash(contentHash string) (*Lesson, error) {
	var lesson Lesson
	var tagsJSON string
	err := s.db.QueryRow(`
		SELECT id, type, content, tags, reinforcement_count, source_session_id, created
		FROM lessons WHERE content_hash = ?
	`, contentHash).Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
	if err != nil {
		return nil, err
	}
	if tagsJSON != "" {
		_ = UnmarshalJSON(tagsJSON, &lesson.Tags)
	}
	return &lesson, nil
}

// GetLesson retrieves a lesson by ID
func (s *LessonStore) GetLesson(id string) (*Lesson, error) {
	var lesson Lesson
	var tagsJSON string
	err := s.db.QueryRow(`
		SELECT id, type, content, tags, reinforcement_count, source_session_id, created
		FROM lessons WHERE id = ?
	`, id).Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
	if err != nil {
		return nil, err
	}
	if tagsJSON != "" {
		_ = UnmarshalJSON(tagsJSON, &lesson.Tags)
	}
	return &lesson, nil
}

// ListLessons returns all lessons, optionally filtered by type
func (s *LessonStore) ListLessons(lessonType LessonType) ([]*Lesson, error) {
	query := `SELECT id, type, content, tags, reinforcement_count, source_session_id, created FROM lessons`
	var args []interface{}

	if lessonType != "" {
		query += ` WHERE type = ?`
		args = append(args, lessonType)
	}
	query += ` ORDER BY reinforcement_count DESC, created DESC`

	rows, err := s.db.Query(query, args...)
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
			_ = UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	return lessons, nil
}

// SearchLessons performs FTS5 search across lessons
func (s *LessonStore) SearchLessons(query string, limit int) ([]*Lesson, error) {
	if limit <= 0 {
		limit = 20
	}

	// Escape FTS5 special characters
	escaped := strings.ReplaceAll(query, "\"", "\"\"")
	ftsQuery := "\"" + escaped + "\"*"

	var lessons []*Lesson

	rows, err := s.db.Query(`
		SELECT l.id, l.type, l.content, l.tags, l.reinforcement_count, l.source_session_id, l.created
		FROM lessons_fts
		JOIN lessons l ON lessons_fts.rowid = l.rowid
		WHERE lessons_fts MATCH ?
		ORDER BY bm25(lessons_fts)
		LIMIT ?
	`, ftsQuery, limit)

	if err != nil {
		// Fall back to LIKE search
		return s.searchLessonsLike(query, limit)
	}
	defer rows.Close()

	for rows.Next() {
		var lesson Lesson
		var tagsJSON string
		err := rows.Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
		if err != nil {
			continue
		}
		if tagsJSON != "" {
			_ = UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}

	if len(lessons) > 0 {
		return lessons, nil
	}

	return s.searchLessonsLike(query, limit)
}

// searchLessonsLike is a fallback when FTS5 is unavailable or returns no results
func (s *LessonStore) searchLessonsLike(query string, limit int) ([]*Lesson, error) {
	q := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.Query(`
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
			_ = UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	return lessons, nil
}

// DeleteLesson removes a lesson
func (s *LessonStore) DeleteLesson(id string) error {
	_, err := s.db.Exec(`DELETE FROM lessons WHERE id = ?`, id)
	return err
}

// GetLessonStats returns statistics about lessons
func (s *LessonStore) GetLessonStats() (map[string]interface{}, error) {
	stats := map[string]interface{}{
		"total_lessons": 0,
		"by_type":        map[string]int{},
	}

	var total int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&total)
	if err != nil {
		return stats, err
	}
	stats["total_lessons"] = total

	// Count by type
	types := []LessonType{LessonTypeWarning, LessonTypePractice, LessonTypeInsight}
	typeCounts := map[string]int{}
	for _, t := range types {
		var count int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM lessons WHERE type = ?`, t).Scan(&count)
		if err == nil {
			typeCounts[string(t)] = count
		}
	}
	stats["by_type"] = typeCounts

	return stats, nil
}
