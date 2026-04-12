package core

import (
	"database/sql"
	"time"
)

// Anchor represents a high-priority anchored memory.
type Anchor struct {
	ID        string
	Content   string
	Context   string
	Weight    int
	SessionID string
	CreatedAt string
}

// InsertAnchor saves a high-priority anchor to mini-bot.db.
// Idempotent: if an anchor with same content+context+session_id exists, updates weight to MAX(current, new).
func InsertAnchor(db *sql.DB, content, context, sessionID string, weight int) error {
	id := generateID()
	now := time.Now().Format(time.RFC3339)

	_, err := db.Exec(`
		INSERT INTO anchors (id, content, context, weight, session_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(content, context, session_id) DO UPDATE SET
			weight = MAX(weight, excluded.weight),
			created_at = excluded.created_at
	`, id, content, context, weight, sessionID, now)
	return err
}

// GetRecentAnchors returns the most recent anchors up to limit.
func GetRecentAnchors(db *sql.DB, limit int) ([]Anchor, error) {
	rows, err := db.Query(`
		SELECT id, content, context, weight, session_id, created_at
		FROM anchors ORDER BY created_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anchors []Anchor
	for rows.Next() {
		var a Anchor
		if err := rows.Scan(&a.ID, &a.Content, &a.Context, &a.Weight, &a.SessionID, &a.CreatedAt); err != nil {
			return nil, err
		}
		anchors = append(anchors, a)
	}
	return anchors, nil
}

// ShouldAnchor returns true if the interaction weight exceeds threshold.
func ShouldAnchor(weight int, threshold int) bool {
	return weight >= threshold
}

// FormatAnchorContext formats an anchor for use in system prompt context.
func FormatAnchorContext(a Anchor) string {
	return a.Content
}
