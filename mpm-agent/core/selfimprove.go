package core

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Lesson represents a learned lesson in the self-improvement system.
type Lesson struct {
	ID                 string
	Content            string
	Type               string
	Tags               string
	CreatedAt          string
	ReinforcementCount int
}

// ExtractLesson inserts or updates a lesson. If content already exists,
// reinforcement_count is incremented (idempotent).
func ExtractLesson(db *sql.DB, lesson Lesson) error {
	if lesson.CreatedAt == "" {
		lesson.CreatedAt = time.Now().Format(time.RFC3339)
	}
	reinforcementCount := lesson.ReinforcementCount
	if reinforcementCount == 0 {
		reinforcementCount = 1
	}

	_, err := db.Exec(`
		INSERT INTO lessons (id, content, type, tags, created_at, reinforcement_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(content) DO UPDATE SET
			reinforcement_count = reinforcement_count + 1,
			tags = COALESCE(lessons.tags, excluded.tags),
			created_at = excluded.created_at
	`, lesson.ID, lesson.Content, lesson.Type, lesson.Tags, lesson.CreatedAt, reinforcementCount)
	return err
}

// GetRecentLessons retrieves the most recent lessons ordered by reinforcement_count DESC.
func GetRecentLessons(db *sql.DB, limit int) ([]Lesson, error) {
	rows, err := db.Query(`
		SELECT id, content, type, tags, created_at, reinforcement_count
		FROM lessons
		ORDER BY reinforcement_count DESC, created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lessons []Lesson
	for rows.Next() {
		var l Lesson
		if err := rows.Scan(&l.ID, &l.Content, &l.Type, &l.Tags, &l.CreatedAt, &l.ReinforcementCount); err != nil {
			return nil, err
		}
		lessons = append(lessons, l)
	}
	return lessons, rows.Err()
}

// GetLessonsByTag retrieves lessons matching a tag, ordered by reinforcement_count DESC.
func GetLessonsByTag(db *sql.DB, tag string) ([]Lesson, error) {
	rows, err := db.Query(`
		SELECT id, content, type, tags, created_at, reinforcement_count
		FROM lessons
		WHERE tags LIKE ?
		ORDER BY reinforcement_count DESC, created_at DESC
	`, "%"+tag+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lessons []Lesson
	for rows.Next() {
		var l Lesson
		if err := rows.Scan(&l.ID, &l.Content, &l.Type, &l.Tags, &l.CreatedAt, &l.ReinforcementCount); err != nil {
			return nil, err
		}
		lessons = append(lessons, l)
	}
	return lessons, rows.Err()
}

// ProposeIdentityPatch writes a proposed change to IDENTITY_PATCH.md.
func ProposeIdentityPatch(binaryDir, patchContent string) error {
	path := filepath.Join(binaryDir, "IDENTITY_PATCH.md")
	return os.WriteFile(path, []byte(patchContent), 0644)
}

// ApplyIdentityPatch merges IDENTITY_PATCH.md into IDENTITY.md.
func ApplyIdentityPatch(binaryDir string) error {
	identityPath := filepath.Join(binaryDir, "IDENTITY.md")
	patchPath := filepath.Join(binaryDir, "IDENTITY_PATCH.md")

	// Read patch
	patchData, err := os.ReadFile(patchPath)
	if err != nil {
		return fmt.Errorf("read patch: %w", err)
	}

	// Read current identity for backup
	currentData, err := os.ReadFile(identityPath)
	if err != nil {
		// If no identity exists, proceed anyway
		currentData = []byte{}
	}

	// Write backup
	backupPath := identityPath + ".backup"
	if len(currentData) > 0 {
		if err := os.WriteFile(backupPath, currentData, 0644); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}

	// Write merged content to identity
	if err := os.WriteFile(identityPath, patchData, 0644); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}

	// Delete patch
	if err := os.Remove(patchPath); err != nil {
		return fmt.Errorf("remove patch: %w", err)
	}

	return nil
}

// RegisterSelfTool adds a self-generated tool to the tools table.
func RegisterSelfTool(db *sql.DB, name, description, definition string) error {
	id := GenerateID()
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`
		INSERT OR REPLACE INTO tools (id, name, description, definition, source, created_at)
		VALUES (
			COALESCE((SELECT id FROM tools WHERE name = ?), ?),
			?, ?, ?, 'self', ?
		)
	`, name, id, name, description, definition, now)
	return err
}

// GetSelfTools retrieves all self-generated tools.
func GetSelfTools(db *sql.DB) ([]map[string]interface{}, error) {
	rows, err := db.Query(`
		SELECT id, name, description, definition, source, created_at
		FROM tools
		WHERE source = 'self'
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tools []map[string]interface{}
	for rows.Next() {
		var id, name, description, definition, source, createdAt string
		if err := rows.Scan(&id, &name, &description, &definition, &source, &createdAt); err != nil {
			return nil, err
		}
		tools = append(tools, map[string]interface{}{
			"id":          id,
			"name":        name,
			"description": description,
			"definition":  definition,
			"source":      source,
			"created_at":  createdAt,
		})
	}
	return tools, rows.Err()
}
