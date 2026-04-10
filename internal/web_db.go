package internal

import (
	"database/sql"
	"strings"
)

// ==================== Memory queries (for web UI) ====================

// QueryMemories returns memories matching the criteria
func (dm *DatabaseManager) QueryMemories(collection string, primeOnly bool, limit, offset int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `SELECT id, collection, content, session_id, tags, metadata, created_at, source_db, source_id, promoted_at FROM memories WHERE 1=1`
	args := []interface{}{}

	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}

	if primeOnly {
		query += " AND (metadata LIKE '%is_prime_directive%' OR tags LIKE '%is_prime_directive%')"
	}

	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mems := []map[string]interface{}{}
	for rows.Next() {
		var id, collection, content, tagsJSON, metadataJSON, createdAt string
		var sessionID, sourceDB, sourceID *string
		var promotedAt *float64

		if err := rows.Scan(&id, &collection, &content, &sessionID, &tagsJSON, &metadataJSON, &createdAt, &sourceDB, &sourceID, &promotedAt); err != nil {
			continue
		}

		m := map[string]interface{}{
			"id":         id,
			"collection": collection,
			"content":    content,
			"tags":       tagsJSON,
			"metadata":   metadataJSON,
			"created_at": createdAt,
		}
		if sessionID != nil {
			m["session_id"] = *sessionID
		}
		if sourceDB != nil {
			m["source_db"] = *sourceDB
		}
		if sourceID != nil {
			m["source_id"] = *sourceID
		}
		if promotedAt != nil {
			m["promoted_at"] = *promotedAt
		}
		mems = append(mems, m)
	}
	return mems, nil
}

// SearchMemories searches memories using FTS5 or LIKE fallback
func (dm *DatabaseManager) SearchMemories(q, collection string, primeOnly bool, limit, offset int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}

	escaped := strings.ReplaceAll(q, "\"", "\"\"")
	ftsQuery := "\"" + escaped + "\"*"

	// Try FTS5 first
	mems := []map[string]interface{}{}
	found := false
	testRows, _ := dm.db.Query(`SELECT 1 FROM memories_fts WHERE memories_fts MATCH ? LIMIT 1`, ftsQuery)
	if testRows != nil {
		if testRows.Next() {
			found = true
		}
		testRows.Close()
	}

	var query string
	var args []interface{}

	if found {
		query = `SELECT m.id, m.collection, m.content, m.session_id, m.tags, m.metadata, m.created_at, m.source_db, m.source_id, m.promoted_at FROM memories m JOIN memories_fts f ON m.rowid = f.rowid WHERE memories_fts MATCH ?`
		args = []interface{}{ftsQuery}
	} else {
		query = `SELECT id, collection, content, session_id, tags, metadata, created_at, source_db, source_id, promoted_at FROM memories WHERE content LIKE ?`
		args = []interface{}{"%" + q + "%"}
	}

	if collection != "" {
		if found {
			query += " AND m.collection = ?"
		} else {
			query += " AND collection = ?"
		}
		args = append(args, collection)
	}

	if primeOnly {
		if found {
			query += " AND (m.metadata LIKE '%is_prime_directive%' OR m.tags LIKE '%is_prime_directive%')"
		} else {
			query += " AND (metadata LIKE '%is_prime_directive%' OR tags LIKE '%is_prime_directive%')"
		}
	}

	if found {
		query += " ORDER BY rank LIMIT ? OFFSET ?"
	} else {
		query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	}
	args = append(args, limit, offset)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, coll, content, tagsJSON, metadataJSON, createdAt string
		var sessionID, sourceDB, sourceID *string
		var promotedAt *float64

		if err := rows.Scan(&id, &coll, &content, &sessionID, &tagsJSON, &metadataJSON, &createdAt, &sourceDB, &sourceID, &promotedAt); err != nil {
			continue
		}

		m := map[string]interface{}{
			"id":         id,
			"collection": coll,
			"content":    content,
			"tags":       tagsJSON,
			"metadata":   metadataJSON,
			"created_at": createdAt,
		}
		if sessionID != nil {
			m["session_id"] = *sessionID
		}
		if sourceDB != nil {
			m["source_db"] = *sourceDB
		}
		if sourceID != nil {
			m["source_id"] = *sourceID
		}
		if promotedAt != nil {
			m["promoted_at"] = *promotedAt
		}
		mems = append(mems, m)
	}
	return mems, nil
}

// GetMemory returns a single memory by ID
func (dm *DatabaseManager) GetMemory(id string) (map[string]interface{}, error) {
	var collection, content, tagsJSON, metadataJSON, createdAt string
	var sessionID, sourceDB, sourceID *string
	var promotedAt *float64

	err := dm.db.QueryRow(`
		SELECT collection, content, session_id, tags, metadata, created_at, source_db, source_id, promoted_at
		FROM memories WHERE id = ?
	`, id).Scan(&collection, &content, &sessionID, &tagsJSON, &metadataJSON, &createdAt, &sourceDB, &sourceID, &promotedAt)
	if err != nil {
		return nil, err
	}

	m := map[string]interface{}{
		"id":         id,
		"collection": collection,
		"content":    content,
		"tags":       tagsJSON,
		"metadata":   metadataJSON,
		"created_at": createdAt,
	}
	if sessionID != nil {
		m["session_id"] = *sessionID
	}
	if sourceDB != nil {
		m["source_db"] = *sourceDB
	}
	if sourceID != nil {
		m["source_id"] = *sourceID
	}
	if promotedAt != nil {
		m["promoted_at"] = *promotedAt
	}
	return m, nil
}

// ShredMemory wraps the standalone ShredMemory function for DatabaseManager
func (dm *DatabaseManager) ShredMemory(id string) error {
	return ShredMemory(dm.db, id)
}

// ==================== Reference queries (for web UI) ====================

// ListReferences returns all reference documents
func (dm *DatabaseManager) ListReferences(limit, offset int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, title, source_path, source_type, tags, total_chunks, last_indexed, created FROM reference_docs ORDER BY created DESC LIMIT ? OFFSET ?`
	rows, err := dm.db.Query(query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	refs := []map[string]interface{}{}
	for rows.Next() {
		var id, title, sourcePath, sourceType, tags, lastIndexed, created string
		var totalChunks int
		if err := rows.Scan(&id, &title, &sourcePath, &sourceType, &tags, &totalChunks, &lastIndexed, &created); err != nil {
			continue
		}
		refs = append(refs, map[string]interface{}{
			"id":           id,
			"title":        title,
			"source_path":  sourcePath,
			"source_type":   sourceType,
			"tags":         tags,
			"total_chunks": totalChunks,
			"last_indexed": lastIndexed,
			"created":      created,
		})
	}
	return refs, nil
}

// GetReference returns a reference document with its chunks
func (dm *DatabaseManager) GetReference(refID string) (map[string]interface{}, error) {
	var id, title, sourcePath, sourceType, tags, lastIndexed, created string
	var totalChunks int
	err := dm.db.QueryRow(`
		SELECT id, title, source_path, source_type, tags, total_chunks, last_indexed, created
		FROM reference_docs WHERE id = ?
	`, refID).Scan(&id, &title, &sourcePath, &sourceType, &tags, &totalChunks, &lastIndexed, &created)
	if err != nil {
		return nil, err
	}

	ref := map[string]interface{}{
		"id":           id,
		"title":        title,
		"source_path":  sourcePath,
		"source_type":  sourceType,
		"tags":         tags,
		"total_chunks": totalChunks,
		"last_indexed": lastIndexed,
		"created":      created,
	}

	// Get chunks
	rows, err := dm.db.Query(`SELECT id, chunk_index, section, content FROM reference_chunks WHERE doc_id = ? ORDER BY chunk_index`, refID)
	if err == nil {
		defer rows.Close()
		chunks := []map[string]interface{}{}
		for rows.Next() {
			var chunkID, section, content string
			var chunkIndex int
			if rows.Scan(&chunkID, &chunkIndex, &section, &content) == nil {
				chunks = append(chunks, map[string]interface{}{
					"id":          chunkID,
					"chunk_index": chunkIndex,
					"section":     section,
					"content":     content,
				})
			}
		}
		ref["chunks"] = chunks
	}

	return ref, nil
}

// SearchReferences searches reference documents by title/content
func (dm *DatabaseManager) SearchReferences(q string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}
	// Try FTS5 on reference_docs title first, fallback to LIKE
	found := false
	testRows, _ := dm.db.Query(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='reference_docs_fts'`)

	var rows *sql.Rows
	var err error

	if testRows != nil {
		if testRows.Next() {
			found = true
		}
		testRows.Close()
	}

	var refs []map[string]interface{}
	if found {
		escaped := strings.ReplaceAll(q, "\"", "\"\"")
		ftsQuery := "\"" + escaped + "\"*"
		rows, err = dm.db.Query(`SELECT id, title, source_path, source_type, tags, total_chunks, last_indexed, created FROM reference_docs WHERE id IN (SELECT doc_id FROM reference_docs_fts WHERE reference_docs_fts MATCH ?) ORDER BY rank LIMIT ?`, ftsQuery, limit)
	} else {
		rows, err = dm.db.Query(`SELECT id, title, source_path, source_type, tags, total_chunks, last_indexed, created FROM reference_docs WHERE title LIKE ? OR tags LIKE ? ORDER BY created DESC LIMIT ?`, "%"+q+"%", "%"+q+"%", limit)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, title, sourcePath, sourceType, tags, lastIndexed, created string
		var totalChunks int
		if rows.Scan(&id, &title, &sourcePath, &sourceType, &tags, &totalChunks, &lastIndexed, &created) == nil {
			refs = append(refs, map[string]interface{}{
				"id":           id,
				"title":        title,
				"source_path":  sourcePath,
				"source_type":  sourceType,
				"tags":         tags,
				"total_chunks": totalChunks,
				"last_indexed": lastIndexed,
				"created":      created,
			})
		}
	}
	return refs, nil
}

// DeleteReference removes a reference doc and its chunks
func (dm *DatabaseManager) DeleteReference(id string) error {
	_, err := dm.db.Exec(`DELETE FROM reference_chunks WHERE doc_id = ?`, id)
	if err != nil {
		return err
	}
	_, err = dm.db.Exec(`DELETE FROM reference_docs WHERE id = ?`, id)
	return err
}

// SearchTopics searches topics using FTS5 or LIKE fallback
func (dm *DatabaseManager) SearchTopics(q string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}

	escaped := strings.ReplaceAll(q, "\"", "\"\"")
	ftsQuery := "\"" + escaped + "\"*"

	found := false
	testRows, _ := dm.db.Query(`SELECT 1 FROM topics_fts WHERE topics_fts MATCH ? LIMIT 1`, ftsQuery)
	if testRows != nil {
		if testRows.Next() {
			found = true
		}
		testRows.Close()
	}

	var query string
	var args []interface{}

	if found {
		query = `SELECT t.id, t.name, COALESCE(t.description,''), t.created_at, COALESCE(t.tags,'{}') FROM topics t JOIN topics_fts f ON t.rowid = f.rowid WHERE topics_fts MATCH ?`
		args = []interface{}{ftsQuery}
	} else {
		query = `SELECT id, name, COALESCE(description,''), created_at, COALESCE(tags,'{}') FROM topics WHERE is_active = 1 AND (name LIKE ? OR description LIKE ?)`
		args = []interface{}{"%" + q + "%", "%" + q + "%"}
	}

	if found {
		query += " ORDER BY rank LIMIT ?"
	} else {
		query += " ORDER BY created_at DESC LIMIT ?"
	}
	args = append(args, limit)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	topics := []map[string]interface{}{}
	for rows.Next() {
		var id, name, description, createdAt, tags string
		rows.Scan(&id, &name, &description, &createdAt, &tags)
		topics = append(topics, map[string]interface{}{
			"id": id, "name": name, "description": description,
			"created_at": createdAt, "tags": tags,
		})
	}
	return topics, nil
}
