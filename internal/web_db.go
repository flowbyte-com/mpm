package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ==================== Lightweight reference types for cross-ref display ====================

// TopicRef is a lightweight topic reference for cross-reference display
type TopicRef struct {
	ID   string
	Name string
	Role string // "manual", "auto", "related"
}

// ReferenceDocRef is a lightweight reference doc reference
type ReferenceDocRef struct {
	ID       string
	Title    string
	FilePath string
}

// MemoryRef is a lightweight memory reference (truncated content for lists)
type MemoryRef struct {
	ID         string
	Content    string
	Collection string
	Weight     int
}

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
		query = `SELECT m.id, m.collection, m.content, m.session_id, m.tags, m.metadata, m.created_at, m.source_db, m.source_id, m.promoted_at FROM memories m JOIN memories_fts f ON m.rowid = f.rowid WHERE memories_fts MATCH ? AND m.deleted_at IS NULL`
		args = []interface{}{ftsQuery}
	} else {
		query = `SELECT id, collection, content, session_id, tags, metadata, created_at, source_db, source_id, promoted_at FROM memories WHERE deleted_at IS NULL AND content LIKE ?`
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
		FROM memories WHERE id = ? AND deleted_at IS NULL
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

// GetMemoryByExternalID returns a memory by its source_db + source_id combination.
// Used for deduplication when polling external databases.
func (dm *DatabaseManager) GetMemoryByExternalID(sourceDB, sourceID string) (map[string]interface{}, error) {
	var id string
	var collection, content, tagsJSON, metadataJSON, createdAt string
	var sessionID *string
	var promotedAt *float64

	err := dm.db.QueryRow(`
		SELECT id, collection, content, session_id, tags, metadata, created_at, promoted_at
		FROM memories WHERE source_db = ? AND source_id = ?
	`, sourceDB, sourceID).Scan(&id, &collection, &content, &sessionID, &tagsJSON, &metadataJSON, &createdAt, &promotedAt)
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
	if promotedAt != nil {
		m["promoted_at"] = *promotedAt
	}
	return m, nil
}

// GetMemoryTopics returns all topics linked to a memory, ordered by role DESC (manual first) then name
func (dm *DatabaseManager) GetMemoryTopics(memoryID string) ([]TopicRef, error) {
	rows, err := dm.db.Query(`
		SELECT t.id, t.name, tm.role
		FROM topic_memberships tm
		JOIN topics t ON t.id = tm.topic_id
		WHERE tm.memory_id = ?
		ORDER BY tm.role DESC, t.name
	`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []TopicRef
	for rows.Next() {
		var r TopicRef
		if err := rows.Scan(&r.ID, &r.Name, &r.Role); err == nil {
			refs = append(refs, r)
		}
	}
	if refs == nil {
		refs = []TopicRef{}
	}
	return refs, rows.Err()
}

// GetReferenceDoc returns a reference doc by ID (lightweight, no chunks)
func (dm *DatabaseManager) GetReferenceDoc(docID string) (*ReferenceDocRef, error) {
	if docID == "" {
		return nil, nil
	}
	var title, filePath string
	err := dm.db.QueryRow(`SELECT title, file_path FROM reference_docs WHERE id = ?`, docID).Scan(&title, &filePath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ReferenceDocRef{ID: docID, Title: title, FilePath: filePath}, nil
}

// GetTopicTopMemories returns top-N memories for a topic (by weight DESC, created_at DESC)
// plus the total count of all linked memories (for the count chip).
// The content field is truncated to 120 chars for list display.
func (dm *DatabaseManager) GetTopicTopMemories(topicID string, limit int) ([]MemoryRef, int, error) {
	if limit <= 0 {
		limit = 3
	}

	rows, err := dm.db.Query(`
		SELECT m.id, m.content, m.collection, m.weight
		FROM topic_memberships tm
		JOIN memories m ON m.id = tm.memory_id
		WHERE tm.topic_id = ? AND tm.memory_id IS NOT NULL AND m.deleted_at IS NULL
		ORDER BY m.weight DESC, m.created_at DESC
		LIMIT ?
	`, topicID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var refs []MemoryRef
	for rows.Next() {
		var r MemoryRef
		var content string
		if err := rows.Scan(&r.ID, &content, &r.Collection, &r.Weight); err == nil {
			if len(content) > 120 {
				r.Content = content[:120] + "…"
			} else {
				r.Content = content
			}
			refs = append(refs, r)
		}
	}
	if refs == nil {
		refs = []MemoryRef{}
	}

	var total int
	dm.db.QueryRow(`SELECT COUNT(*) FROM topic_memberships WHERE topic_id = ? AND memory_id IS NOT NULL`, topicID).Scan(&total)

	return refs, total, rows.Err()
}

// UpdateMemory updates an existing memory's content and metadata.
func (dm *DatabaseManager) UpdateMemory(id, content string, tags map[string]interface{}, metadata map[string]interface{}) error {
	tagsJSON, _ := json.Marshal(tags)
	metadataJSON, _ := json.Marshal(metadata)
	embedding := HashEmbed(content)
	embeddingJSON, _ := json.Marshal(embedding)
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	_, err := dm.db.Exec(`
		UPDATE memories
		SET content = ?, tags = ?, metadata = ?, embedding = ?, content_hash = ?,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, content, string(tagsJSON), string(metadataJSON), string(embeddingJSON), contentHash, id)
	return err
}

// ReinforceMemory increments the reinforcement count and weight.
func (dm *DatabaseManager) ReinforceMemory(id string, delta int) error {
	if delta <= 0 {
		delta = 1
	}
	weightGain := (delta + 1) / 2
	if weightGain == 0 {
		weightGain = 1
	}
	_, err := dm.db.Exec(`
		UPDATE memories
		SET reinforcement_count = reinforcement_count + ?, weight = MIN(weight + ?, 100),
		    last_accessed_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, delta, weightGain, id)
	return err
}

// WeakenMemory decrements reinforcement count and reduces weight.
func (dm *DatabaseManager) WeakenMemory(id string, delta int) error {
	if delta <= 0 {
		delta = 1
	}
	weightLoss := (delta + 1) / 2
	_, err := dm.db.Exec(`
		UPDATE memories
		SET reinforcement_count = MAX(reinforcement_count - ?, 0),
		    weight = MAX(weight - ?, 0),
		    last_accessed_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, delta, weightLoss, id)
	return err
}

// SetMemoryTTL sets an expiration time on a memory.
func (dm *DatabaseManager) SetMemoryTTL(id string, expiresAt time.Time) error {
	if expiresAt.IsZero() {
		_, err := dm.db.Exec(`UPDATE memories SET expires_at = NULL WHERE id = ?`, id)
		return err
	}
	_, err := dm.db.Exec(`
		UPDATE memories SET expires_at = ? WHERE id = ?
	`, expiresAt.Format(time.RFC3339), id)
	return err
}

// PruneExpired removes memories that have passed their expires_at time.
func (dm *DatabaseManager) PruneExpired() (int, error) {
	result, err := dm.db.Exec(`
		DELETE FROM memories WHERE expires_at IS NOT NULL AND expires_at < CURRENT_TIMESTAMP
	`)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// GetMemoriesByRelevance returns memories ordered by composite relevance score.
func (dm *DatabaseManager) GetMemoriesByRelevance(collection string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}

	query := `
		SELECT id, collection, content, session_id, tags, metadata, created_at,
		       COALESCE(reinforcement_count, 0) as reinforcement_count,
		       COALESCE(weight, 1) as weight,
		       last_accessed_at, expires_at
		FROM memories
		WHERE deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)
		  AND (? = '' OR collection = ?)
		ORDER BY (COALESCE(reinforcement_count, 0) * 2) + (COALESCE(weight, 1) * 1.5) DESC,
		         COALESCE(last_accessed_at, created_at) DESC
		LIMIT ?
	`

	rows, err := dm.db.Query(query, collection, collection, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, collection, content, tagsJSON, metadataJSON, createdAt string
		var sessionID *string
		var reinforcementCount, weight int
		var lastAccessedAt, expiresAt *string

		err := rows.Scan(&id, &collection, &content, &sessionID, &tagsJSON, &metadataJSON,
			&createdAt, &reinforcementCount, &weight, &lastAccessedAt, &expiresAt)
		if err != nil {
			continue
		}

		m := map[string]interface{}{
			"id":                  id,
			"collection":          collection,
			"content":             content,
			"tags":                tagsJSON,
			"metadata":            metadataJSON,
			"created_at":          createdAt,
			"reinforcement_count": reinforcementCount,
			"weight":              weight,
		}
		if sessionID != nil {
			m["session_id"] = *sessionID
		}
		if lastAccessedAt != nil {
			m["last_accessed_at"] = *lastAccessedAt
		}
		if expiresAt != nil {
			m["expires_at"] = *expiresAt
		}
		results = append(results, m)
	}
	return results, rows.Err()
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
	query := `SELECT id, title, file_path, source_type, tags, total_chunks, last_indexed, created_at FROM reference_docs ORDER BY created_at DESC LIMIT ? OFFSET ?`
	rows, err := dm.db.Query(query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	refs := []map[string]interface{}{}
	for rows.Next() {
		var id, title, filePath, sourceType, tags, lastIndexed, createdAt string
		var totalChunks int
		if err := rows.Scan(&id, &title, &filePath, &sourceType, &tags, &totalChunks, &lastIndexed, &createdAt); err != nil {
			continue
		}
		refs = append(refs, map[string]interface{}{
			"id":           id,
			"title":        title,
			"file_path":    filePath,
			"source_type":  sourceType,
			"tags":         tags,
			"total_chunks": totalChunks,
			"last_indexed": lastIndexed,
			"created_at":   createdAt,
		})
	}
	return refs, nil
}

// GetReference returns a reference document with its chunks
func (dm *DatabaseManager) GetReference(refID string) (map[string]interface{}, error) {
	var id, title, filePath, sourceType, tags, lastIndexed, createdAt string
	var totalChunks int
	err := dm.db.QueryRow(`
		SELECT id, title, file_path, source_type, tags, total_chunks, last_indexed, created_at
		FROM reference_docs WHERE id = ?
	`, refID).Scan(&id, &title, &filePath, &sourceType, &tags, &totalChunks, &lastIndexed, &createdAt)
	if err != nil {
		return nil, err
	}

	ref := map[string]interface{}{
		"id":           id,
		"title":        title,
		"file_path":    filePath,
		"source_type":  sourceType,
		"tags":         tags,
		"total_chunks": totalChunks,
		"last_indexed": lastIndexed,
		"created_at":   createdAt,
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

	// Try FTS5 on references table first, fallback to LIKE
	found := false
	testRows, _ := dm.db.Query(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='references_fts'`)
	if testRows != nil {
		if testRows.Next() {
			found = true
		}
		testRows.Close()
	}

	var refs []map[string]interface{}
	var rows *sql.Rows
	var err error

	if found {
		escaped := strings.ReplaceAll(q, "\"", "\"\"")
		ftsQuery := "\"" + escaped + "\"*"
		rows, err = dm.db.Query(`SELECT id, title, file_path, source_type, tags, total_chunks, last_indexed, created_at FROM reference_docs WHERE id IN (SELECT rowid FROM references_fts WHERE references_fts MATCH ?) ORDER BY rank LIMIT ?`, ftsQuery, limit)
	} else {
		rows, err = dm.db.Query(`SELECT id, title, file_path, source_type, tags, total_chunks, last_indexed, created_at FROM reference_docs WHERE title LIKE ? OR content LIKE ? ORDER BY created_at DESC LIMIT ?`, "%"+q+"%", "%"+q+"%", limit)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, title, filePath, sourceType, tags, lastIndexed, createdAt string
		var totalChunks int
		if rows.Scan(&id, &title, &filePath, &sourceType, &tags, &totalChunks, &lastIndexed, &createdAt) == nil {
			refs = append(refs, map[string]interface{}{
				"id":           id,
				"title":        title,
				"file_path":    filePath,
				"source_type":  sourceType,
				"tags":         tags,
				"total_chunks": totalChunks,
				"last_indexed": lastIndexed,
				"created_at":   createdAt,
			})
		}
	}
	return refs, nil
}

// SearchReferenceChunks searches chunks within reference documents
func (dm *DatabaseManager) SearchReferenceChunks(q string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}

	// Try FTS5 on chunks first, fallback to LIKE
	found := false
	testRows, _ := dm.db.Query(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='reference_chunks_fts'`)
	if testRows != nil {
		if testRows.Next() {
			found = true
		}
		testRows.Close()
	}

	var chunks []map[string]interface{}
	var rows *sql.Rows
	var err error

	if found {
		escaped := strings.ReplaceAll(q, "\"", "\"\"")
		ftsQuery := "\"" + escaped + "\"*"
		rows, err = dm.db.Query(`SELECT rc.id, rc.doc_id, rc.chunk_index, rc.section, rc.content, r.title FROM reference_chunks rc JOIN reference_docs r ON rc.doc_id = r.id WHERE rc.id IN (SELECT rowid FROM reference_chunks_fts WHERE reference_chunks_fts MATCH ?) ORDER BY rank LIMIT ?`, ftsQuery, limit)
	} else {
		rows, err = dm.db.Query(`SELECT rc.id, rc.doc_id, rc.chunk_index, rc.section, rc.content, r.title FROM reference_chunks rc JOIN reference_docs r ON rc.doc_id = r.id WHERE rc.content LIKE ? ORDER BY rc.chunk_index LIMIT ?`, "%"+q+"%", limit)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, docID, section, content, title string
		var chunkIndex int
		if rows.Scan(&id, &docID, &chunkIndex, &section, &content, &title) == nil {
			chunks = append(chunks, map[string]interface{}{
				"id":          id,
				"doc_id":      docID,
				"chunk_index": chunkIndex,
				"section":     section,
				"content":     content,
				"doc_title":   title,
			})
		}
	}
	return chunks, nil
}

// DeleteReference removes a reference doc and its chunks (cascade from FK)
func (dm *DatabaseManager) DeleteReference(id string) error {
	_, err := dm.db.Exec(`DELETE FROM reference_chunks WHERE doc_id = ?`, id)
	if err != nil {
		return err
	}
	_, err = dm.db.Exec(`DELETE FROM reference_docs WHERE id = ?`, id)
	return err
}

// AddReference adds a reference document and its chunks in a transaction
func (dm *DatabaseManager) AddReference(doc *ReferenceDoc, chunks []ReferenceChunk) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tagsJSON, _ := MarshalJSON(doc.Tags)
	_, err = tx.Exec(`
		INSERT INTO reference_docs (id, title, file_path, source_type, tags, content, content_hash, total_chunks, last_indexed, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, doc.ID, doc.Title, doc.SourcePath, doc.SourceType, tagsJSON, doc.Content, doc.ContentHash, doc.TotalChunks, doc.LastIndexed, doc.Created)
	if err != nil {
		return err
	}

	for _, chunk := range chunks {
		_, err = tx.Exec(`
			INSERT INTO reference_chunks (id, doc_id, chunk_index, section, content, source_path)
			VALUES (?, ?, ?, ?, ?, ?)
		`, chunk.ID, chunk.DocID, chunk.ChunkIndex, chunk.Section, chunk.Content, chunk.SourcePath)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
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

// GetMemoryStats returns comprehensive memory statistics
func (dm *DatabaseManager) GetMemoryStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// Basic counts
	var total, active, deleted, ltm, reinforced, neverAccessed, expired int
	err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&total)
	if err != nil {
		return nil, err
	}
	dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`).Scan(&active)
	dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at IS NOT NULL`).Scan(&deleted)
	dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE is_long_term = 1 AND deleted_at IS NULL`).Scan(&ltm)
	dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE reinforcement_count > 0 AND deleted_at IS NULL`).Scan(&reinforced)
	dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE last_accessed_at IS NULL AND reinforcement_count = 0 AND deleted_at IS NULL`).Scan(&neverAccessed)
	dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE expires_at IS NOT NULL AND expires_at < CURRENT_TIMESTAMP`).Scan(&expired)

	stats["total"] = total
	stats["active"] = active
	stats["deleted"] = deleted
	stats["ltm"] = ltm
	stats["reinforced"] = reinforced
	stats["never_accessed"] = neverAccessed
	stats["expired"] = expired

	// By collection
	rows, err := dm.db.Query(`
		SELECT collection, COUNT(*) as count FROM memories
		WHERE deleted_at IS NULL GROUP BY collection ORDER BY count DESC
	`)
	if err == nil {
		defer rows.Close()
		var byCollection []map[string]interface{}
		for rows.Next() {
			var coll string
			var count int
			if rows.Scan(&coll, &count) == nil {
				byCollection = append(byCollection, map[string]interface{}{"collection": coll, "count": count})
			}
		}
		stats["by_collection"] = byCollection
	}

	// By tag (top 20)
	rows, err = dm.db.Query(`
		SELECT json_each.value as tag, COUNT(*) as count
		FROM memories, json_each(memory.tags)
		WHERE deleted_at IS NULL
		GROUP BY json_each.value
		ORDER BY count DESC
		LIMIT 20
	`)
	if err == nil {
		defer rows.Close()
		var byTag []map[string]interface{}
		for rows.Next() {
			var tag string
			var count int
			if rows.Scan(&tag, &count) == nil {
				byTag = append(byTag, map[string]interface{}{"tag": tag, "count": count})
			}
		}
		stats["by_tag"] = byTag
	}

	// Reinforcement distribution
	rows, err = dm.db.Query(`
		SELECT reinforcement_count, COUNT(*) as count
		FROM memories WHERE deleted_at IS NULL
		GROUP BY reinforcement_count ORDER BY reinforcement_count
	`)
	if err == nil {
		defer rows.Close()
		var dist []map[string]interface{}
		for rows.Next() {
			var rc, count int
			if rows.Scan(&rc, &count) == nil {
				dist = append(dist, map[string]interface{}{"reinforcement_count": rc, "count": count})
			}
		}
		stats["reinforce_dist"] = dist
	}

	return stats, nil
}

// PruneOlderThan deletes memories created before the given time
func (dm *DatabaseManager) PruneOlderThan(before time.Time) (int, error) {
	result, err := dm.db.Exec(`
		DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL
	`, before.Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// PruneNeverAccessed deletes memories that were never accessed
func (dm *DatabaseManager) PruneNeverAccessed() (int, error) {
	result, err := dm.db.Exec(`
		DELETE FROM memories
		WHERE last_accessed_at IS NULL
		  AND reinforcement_count = 0
		  AND weight = 1
		  AND is_long_term = 0
		  AND deleted_at IS NULL
	`)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// GetMemoriesForExport retrieves memories with optional filters
func (dm *DatabaseManager) GetMemoriesForExport(collection, since, until string) ([]map[string]interface{}, error) {
	query := `
		SELECT id, collection, content, tags, metadata, created_at,
		       COALESCE(reinforcement_count, 0) as reinforcement_count,
		       COALESCE(weight, 1) as weight,
		       COALESCE(is_long_term, 0) as is_long_term,
		       COALESCE(last_accessed_at, '') as last_accessed_at,
		       COALESCE(expires_at, '') as expires_at
		FROM memories
		WHERE deleted_at IS NULL
	`
	args := []interface{}{}

	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}
	if since != "" {
		query += " AND created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		query += " AND created_at <= ?"
		args = append(args, until+" 23:59:59")
	}

	query += " ORDER BY created_at DESC"

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []map[string]interface{}
	for rows.Next() {
		var id, coll, content, tags, metadata, createdAt, lastAccessed, expiresAt string
		var rc, weight, isLongTerm int
		err := rows.Scan(&id, &coll, &content, &tags, &metadata, &createdAt, &rc, &weight, &isLongTerm, &lastAccessed, &expiresAt)
		if err != nil {
			continue
		}
		mem := map[string]interface{}{
			"id":                  id,
			"collection":          coll,
			"content":             content,
			"tags":                tags,
			"metadata":            metadata,
			"created_at":          createdAt,
			"reinforcement_count": rc,
			"weight":              weight,
			"is_long_term":        isLongTerm,
			"last_accessed_at":    lastAccessed,
			"expires_at":          expiresAt,
		}
		memories = append(memories, mem)
	}
	return memories, nil
}

// =============================================================================
// Self-Improving Memory System (DatabaseManager wrappers)
// =============================================================================

// RunSelfMaintenance runs all self-improvement processes on DatabaseManager.
func (dm *DatabaseManager) RunSelfMaintenance() (map[string]interface{}, error) {
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, err
	}

	stats, err := store.RunSelfMaintenance()
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"decayed_weights":          stats.DecayedWeights,
		"pruned_total":             stats.PrunedTotal,
		"consolidated":             stats.Consolidated,
		"never_accessed":           stats.NeverAccessed,
		"low_weight":               stats.LowWeight,
		"for_spaced_reinforcement": stats.ForReview,
	}, nil
}

// GetSpacedReinforcementReview returns memories for spaced reinforcement review.
func (dm *DatabaseManager) GetSpacedReinforcementReview(daysSinceAccess, limit int) ([]map[string]interface{}, error) {
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, err
	}

	memories, err := store.SpacedReinforcementReview(daysSinceAccess, limit)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, 0, len(memories))
	for _, mem := range memories {
		result = append(result, map[string]interface{}{
			"id":                  mem.ID,
			"collection":          mem.Collection,
			"content":             mem.Content,
			"tags":                mem.Tags,
			"weight":              mem.Weight,
			"reinforcement_count": mem.ReinforcementCount,
			"last_accessed_at":    mem.LastAccessedAt,
		})
	}
	return result, nil
}

// GetContextualMemories returns memories relevant to a given context.
func (dm *DatabaseManager) GetContextualMemories(contextTags []string, sessionContext string, limit int) ([]map[string]interface{}, error) {
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, err
	}

	memories, err := store.GetContextualMemories(contextTags, sessionContext, limit)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, 0, len(memories))
	for _, mem := range memories {
		result = append(result, map[string]interface{}{
			"id":                  mem.ID,
			"collection":          mem.Collection,
			"content":             mem.Content,
			"tags":                mem.Tags,
			"weight":              mem.Weight,
			"reinforcement_count": mem.ReinforcementCount,
		})
	}
	return result, nil
}
