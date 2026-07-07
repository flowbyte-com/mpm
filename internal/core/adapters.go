package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type SchemaAdapter interface {
	Detect(db *sql.DB) bool
	FetchNew(db *sql.DB, cursor string) ([]Memory, string, error)
	Name() string
}

type AdapterRegistry struct {
	adapters []SchemaAdapter
}

func NewAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{
		adapters: []SchemaAdapter{
			&OpenClawAdapter{},
			&MPMAdapter{},
		},
	}
}

func (r *AdapterRegistry) Detect(db *sql.DB) SchemaAdapter {
	for _, a := range r.adapters {
		if a.Detect(db) {
			return a
		}
	}
	return nil
}

// MPMAdapter handles MPM's own memories table schema (used for cross-MPM sync/polling)
type MPMAdapter struct{}

func (a *MPMAdapter) Name() string {
	return "mpm"
}

func (a *MPMAdapter) Detect(db *sql.DB) bool {
	var memoriesExists int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memories'").Scan(&memoriesExists)
	if memoriesExists != 1 {
		return false
	}
	var colList string
	db.QueryRow("SELECT GROUP_CONCAT(name) FROM pragma_table_info('memories')").Scan(&colList)
	return strings.Contains(colList, "content") && strings.Contains(colList, "created_at")
}

func (a *MPMAdapter) FetchNew(db *sql.DB, cursor string) ([]Memory, string, error) {
	query := `SELECT id, content, session_id, tags, created_at FROM memories WHERE deleted_at IS NULL`
	var rows *sql.Rows
	var err error

	if cursor != "" {
		rows, err = db.Query(query+" AND created_at > ? ORDER BY created_at ASC", cursor)
	} else {
		rows, err = db.Query(query + " ORDER BY created_at ASC")
	}
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var memories []Memory
	var latestCursor string
	for rows.Next() {
		var id, content, sessionID, tags, createdAt string
		if err := rows.Scan(&id, &content, &sessionID, &tags, &createdAt); err != nil {
			slog.Warn("failed to scan memory row, skipping", "error", err.Error())
			continue
		}
		latestCursor = createdAt
		var tagSlice []string
		if tags != "" {
			json.Unmarshal([]byte(tags), &tagSlice)
		}
		mem := Memory{
			ID:        id,
			Content:   content,
			SessionID: sessionID,
			Tags:      tagSlice,
			Metadata:  map[string]interface{}{"source_id": id, "original_tags": tags},
			Source:    "mpm",
		}
		memories = append(memories, mem)
	}

	return memories, latestCursor, rows.Err()
}

type OpenClawAdapter struct{}

func (a *OpenClawAdapter) Name() string {
	return "openclaw"
}

func (a *OpenClawAdapter) Detect(db *sql.DB) bool {
	var chunksExists int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chunks'").Scan(&chunksExists)
	if chunksExists != 1 {
		return false
	}
	var colList string
	db.QueryRow("SELECT GROUP_CONCAT(name) FROM pragma_table_info('chunks')").Scan(&colList)
	return strings.Contains(colList, "hash") && strings.Contains(colList, "text") && strings.Contains(colList, "updated_at")
}

func (a *OpenClawAdapter) FetchNew(db *sql.DB, cursor string) ([]Memory, string, error) {
	var rows *sql.Rows
	var err error

	if cursor == "" {
		rows, err = db.Query(`
			SELECT id, path, source, start_line, end_line, hash, model, text, updated_at
			FROM chunks
			ORDER BY updated_at ASC
			LIMIT 100
		`)
	} else {
		rows, err = db.Query(`
			SELECT id, path, source, start_line, end_line, hash, model, text, updated_at
			FROM chunks
			WHERE updated_at > ?
			ORDER BY updated_at ASC
			LIMIT 100
		`, cursor)
	}
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var memories []Memory
	var lastUpdatedAt int64
	for rows.Next() {
		var id, path, source, hash, model, text string
		var startLine, endLine int
		var updatedAt int64
		if err := rows.Scan(&id, &path, &source, &startLine, &endLine, &hash, &model, &text, &updatedAt); err != nil {
			slog.Warn("failed to scan chunk row, skipping", "error", err.Error())
			continue
		}
		lastUpdatedAt = updatedAt
		mem := Memory{
			ID:      id,
			Content: text,
			Metadata: map[string]interface{}{
				"path":       path,
				"source":     source,
				"start_line": startLine,
				"end_line":   endLine,
				"model":      model,
				"updated_at": updatedAt,
			},
			Source: "openclaw",
		}
		memories = append(memories, mem)
	}

	var newCursor string
	if len(memories) > 0 {
		newCursor = fmt.Sprintf("%d", lastUpdatedAt)
	}

	return memories, newCursor, rows.Err()
}

func DetectSchemaAdapter(dbPath string) (SchemaAdapter, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("source DB not found: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open source DB: %w", err)
	}
	defer db.Close()

	registry := NewAdapterRegistry()
	adapter := registry.Detect(db)
	if adapter == nil {
		return nil, fmt.Errorf("unrecognized schema in %s", dbPath)
	}
	return adapter, nil
}
