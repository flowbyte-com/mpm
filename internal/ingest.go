package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Schema represents a detected source DB schema.
type Schema struct {
	DBType string // "openclaw"
	Tables []Table
	DBPath string
}

// Table represents a table in a source DB.
type Table struct {
	Name    string
	Columns []string
}

// OpenClawChunk represents a row from the OpenClaw chunks table.
type OpenClawChunk struct {
	ID         string
	Path       string
	Source     string
	StartLine  int
	EndLine    int
	Hash       string // content SHA-256
	Model      string
	Text       string
	UpdatedAt  int64
}

// RawMemory represents a staging entry in raw_memories.
type RawMemory struct {
	ID             string
	SourceID       string
	SourceDB       string
	ContentHash    string
	Text           string
	Metadata       string // JSON
	IngestedAt     float64
	Status         string
	LLMVerdict     string
	LLMNotes       string
	ReviewerPrompt string
	ExpiresAt      float64
	ImportBatch    string
	UpdatedAt      float64
}

// DetectSchema inspects a SQLite DB and returns its schema.
// Currently hardcoded for OpenClaw. Future: Obsidian, Logseq, etc.
func DetectSchema(dbPath string) (*Schema, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("source DB not found: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open source DB: %w", err)
	}
	defer db.Close()

	// Check for OpenClaw signature: chunks table with known columns
	var tableCount int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tableCount)

	var chunksExists int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chunks'").Scan(&chunksExists)

	if chunksExists == 1 {
		// Verify expected columns
		var colList string
		db.QueryRow("SELECT GROUP_CONCAT(name) FROM pragma_table_info('chunks')").Scan(&colList)
		if strings.Contains(colList, "hash") && strings.Contains(colList, "text") && strings.Contains(colList, "updated_at") {
			return &Schema{
				DBType: "openclaw",
				Tables: []Table{{Name: "chunks", Columns: strings.Split(colList, ",")}},
				DBPath: dbPath,
			}, nil
		}
	}

	return nil, fmt.Errorf("unrecognized schema in %s (chunks table not found or unexpected columns)", dbPath)
}

// ReadOpenClawChunks streams chunks from an OpenClaw SQLite DB.
// Returns a channel of chunk batches.
func ReadOpenClawChunks(dbPath string, batchSize int) (<-chan []OpenClawChunk, <-chan error, error) {
	if batchSize <= 0 {
		batchSize = 100
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open OpenClaw DB: %w", err)
	}

	errChan := make(chan error, 1)
	chunkChan := make(chan []OpenClawChunk, 10)

	go func() {
		defer close(chunkChan)
		defer close(errChan)
		defer db.Close()

		rows, err := db.Query(`
			SELECT id, path, source, start_line, end_line, hash, model, text, updated_at
			FROM chunks
			ORDER BY path, start_line
		`)
		if err != nil {
			errChan <- fmt.Errorf("query failed: %w", err)
			return
		}
		defer rows.Close()

		batch := make([]OpenClawChunk, 0, batchSize)
		for rows.Next() {
			var c OpenClawChunk
			if err := rows.Scan(&c.ID, &c.Path, &c.Source, &c.StartLine, &c.EndLine, &c.Hash, &c.Model, &c.Text, &c.UpdatedAt); err != nil {
				errChan <- fmt.Errorf("scan error: %w", err)
				continue
			}
			batch = append(batch, c)
			if len(batch) >= batchSize {
				chunkChan <- batch
				batch = make([]OpenClawChunk, 0, batchSize)
			}
		}
		if err := rows.Err(); err != nil {
			errChan <- err
		}
		if len(batch) > 0 {
			chunkChan <- batch
		}
	}()

	return chunkChan, errChan, nil
}

// IngestStats holds statistics from an ingest run.
type IngestStats struct {
	RowsRead     int
	RowsStaged   int
	RowsSkipped  int // dedup or already staged
	RowsRejected int // security filter
}

// IngestFromAdapter ingests memories from any registered schema adapter.
// The adapter handles detection and fetching; this function applies dedup and staging.
func (dm *DatabaseManager) IngestFromAdapter(dbPath string, adapter SchemaAdapter, batchSize int, importBatch string, dryRun bool) (*IngestStats, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open DB: %w", err)
	}
	defer db.Close()

	if !adapter.Detect(db) {
		return nil, fmt.Errorf("adapter %s does not match schema at %s", adapter.Name(), dbPath)
	}

	stats := &IngestStats{}
	now := float64(time.Now().Unix())
	if importBatch == "" {
		importBatch = fmt.Sprintf("ingest_%d", time.Now().Unix())
	}

	// Stream in batches using the adapter
	var cursor string
	for {
		memories, newCursor, err := adapter.FetchNew(db, cursor)
		if err != nil {
			return stats, fmt.Errorf("fetch failed: %w", err)
		}

		for _, mem := range memories {
			stats.RowsRead++

			// Skip empty content
			if strings.TrimSpace(mem.Content) == "" {
				stats.RowsSkipped++
				continue
			}

			// Security filter
			if sensitive, reason := isSensitiveContent(mem.Content); sensitive {
				if !dryRun {
					h := sha256.Sum256([]byte(mem.Content))
					dm.insertRawMemoryRejectedFromAdapter(mem, now, importBatch, reason, hex.EncodeToString(h[:]))
				}
				stats.RowsRejected++
				continue
			}

			// Dedup: content hash in memories table
			contentHash := sha256.Sum256([]byte(mem.Content))
			exists, _ := dm.contentHashExistsInMemory(hex.EncodeToString(contentHash[:]))
			if exists {
				stats.RowsSkipped++
				continue
			}

			// Stage it
			if !dryRun {
				meta := mem.Metadata
				if meta == nil {
					meta = make(map[string]interface{})
				}
				meta["source"] = adapter.Name()
				metadataJSON, _ := json.Marshal(meta)

				raw := &RawMemory{
					ID:           GenerateID(),
					SourceID:     mem.ID,
					SourceDB:     adapter.Name(),
					ContentHash:  hex.EncodeToString(contentHash[:]),
					Text:         mem.Content,
					Metadata:     string(metadataJSON),
					IngestedAt:   now,
					Status:       "pending",
					ImportBatch:  importBatch,
					UpdatedAt:    now,
					ExpiresAt:    now + (30 * 24 * 60 * 60),
				}
				if err := dm.insertRawMemory(raw); err != nil {
					return stats, fmt.Errorf("insert failed: %w", err)
				}
			}
			stats.RowsStaged++
		}

		// If no new cursor or no memories, we're done
		if newCursor == "" || len(memories) == 0 {
			break
		}
		cursor = newCursor
	}

	return stats, nil
}

// insertRawMemoryRejectedFromAdapter inserts a rejected entry for adapter-based ingest
func (dm *DatabaseManager) insertRawMemoryRejectedFromAdapter(mem Memory, now float64, importBatch string, reason string, contentHash string) error {
	meta := mem.Metadata
	if meta == nil {
		meta = make(map[string]interface{})
	}
	metaJSON, _ := json.Marshal(meta)
	_, err := dm.db.Exec(`
		INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, ingested_at, status, llm_verdict, llm_notes, import_batch, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'rejected', 'toxic', ?, ?, ?)
	`, GenerateID(), mem.ID, mem.Source, contentHash, mem.Content, string(metaJSON), now, "security filter: "+reason, importBatch, now)
	return err
}

// IngestOpenClaw reads from an OpenClaw source DB and writes to raw_memories.
// It applies security filtering and deduplication inline.
func (dm *DatabaseManager) IngestOpenClaw(sourcePath string, batchSize int, importBatch string, dryRun bool) (*IngestStats, error) {
	adapter, err := DetectSchemaAdapter(sourcePath)
	if err != nil {
		return nil, err
	}
	return dm.IngestFromAdapter(sourcePath, adapter, batchSize, importBatch, dryRun)
}


// insertRawMemory inserts a new raw_memory entry (status=pending).
func (dm *DatabaseManager) insertRawMemory(raw *RawMemory) error {
	_, err := dm.db.Exec(`
		INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, ingested_at, status, import_batch, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, raw.ID, raw.SourceID, raw.SourceDB, raw.ContentHash, raw.Text, raw.Metadata, raw.IngestedAt, raw.Status, raw.ImportBatch, raw.UpdatedAt, raw.ExpiresAt)
	return err
}

// insertRawMemoryRejected inserts a rejected entry (security filter).
func (dm *DatabaseManager) insertRawMemoryRejected(chunk OpenClawChunk, now float64, importBatch string, reason string) error {
	metadata, _ := json.Marshal(map[string]interface{}{
		"path":       chunk.Path,
		"source":     chunk.Source,
		"start_line": chunk.StartLine,
		"end_line":   chunk.EndLine,
		"model":      chunk.Model,
		"updated_at": chunk.UpdatedAt,
	})
	// Hash for rejected content
	h := sha256.Sum256([]byte(chunk.Text))
	_, err := dm.db.Exec(`
		INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, ingested_at, status, llm_verdict, llm_notes, import_batch, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'rejected', 'toxic', ?, ?, ?)
	`, GenerateID(), chunk.ID, "openclaw", hex.EncodeToString(h[:]), chunk.Text, string(metadata), now, "security filter: "+reason, importBatch, now)
	return err
}

// contentHashExistsInMemory checks if a content hash is already in the memory table.
func (dm *DatabaseManager) contentHashExistsInMemory(contentHash string) (bool, error) {
	var count int
	err := dm.db.QueryRow("SELECT COUNT(*) FROM memories WHERE content_hash = ?", contentHash).Scan(&count)
	return count > 0, err
}

// sourceIDExistsInRawMemories checks if a source_id is already staged.
func (dm *DatabaseManager) sourceIDExistsInRawMemories(sourceDB, sourceID string) (bool, error) {
	var count int
	err := dm.db.QueryRow("SELECT COUNT(*) FROM raw_memories WHERE source_db = ? AND source_id = ?", sourceDB, sourceID).Scan(&count)
	return count > 0, err
}

// contentHashExistsInRawMemories checks if a content hash exists in raw_memories with a given status.
func (dm *DatabaseManager) contentHashExistsInRawMemories(contentHash, status string) (bool, error) {
	var count int
	err := dm.db.QueryRow("SELECT COUNT(*) FROM raw_memories WHERE content_hash = ? AND status = ?", contentHash, status).Scan(&count)
	return count > 0, err
}

// GetRawMemoriesByStatus returns all raw_memories with a given status.
func (dm *DatabaseManager) GetRawMemoriesByStatus(status string, limit int) ([]*RawMemory, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := dm.db.Query(`
		SELECT id, source_id, source_db, content_hash, text, metadata, ingested_at, status, llm_verdict, llm_notes, reviewer_prompt, expires_at, import_batch, updated_at
		FROM raw_memories
		WHERE status = ?
		ORDER BY ingested_at ASC
		LIMIT ?
	`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*RawMemory
	for rows.Next() {
		var r RawMemory
		var llmVerdict, llmNotes, reviewerPrompt *string
		var expiresAt *float64
		if err := rows.Scan(&r.ID, &r.SourceID, &r.SourceDB, &r.ContentHash, &r.Text, &r.Metadata, &r.IngestedAt, &r.Status, &llmVerdict, &llmNotes, &reviewerPrompt, &expiresAt, &r.ImportBatch, &r.UpdatedAt); err != nil {
			continue
		}
		if llmVerdict != nil {
			r.LLMVerdict = *llmVerdict
		}
		if llmNotes != nil {
			r.LLMNotes = *llmNotes
		}
		if reviewerPrompt != nil {
			r.ReviewerPrompt = *reviewerPrompt
		}
		if expiresAt != nil {
			r.ExpiresAt = *expiresAt
		}
		results = append(results, &r)
	}
	return results, nil
}

// UpdateRawMemoryVerdict updates the LLM verdict for a raw_memory entry.
func (dm *DatabaseManager) UpdateRawMemoryVerdict(id, verdict, notes string) error {
	_, err := dm.db.Exec(`
		UPDATE raw_memories SET llm_verdict = ?, llm_notes = ?, status = ?, updated_at = ?
		WHERE id = ?
	`, verdict, notes, verdict, float64(time.Now().Unix()), id)
	return err
}

// ResetStaleReviewing resets stuck 'reviewing' entries back to 'pending'.
func (dm *DatabaseManager) ResetStaleReviewing() (int, error) {
	staleThreshold := float64(time.Now().Unix()) - 3600 // 1 hour
	result, err := dm.db.Exec(`
		UPDATE raw_memories SET status = 'pending', updated_at = ?
		WHERE status = 'reviewing' AND updated_at < ?
	`, float64(time.Now().Unix()), staleThreshold)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// GetIngestStatus returns a summary of raw_memories counts by status.
func (dm *DatabaseManager) GetIngestStatus() (map[string]int, error) {
	rows, err := dm.db.Query(`
		SELECT status, COUNT(*) as count FROM raw_memories GROUP BY status
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err == nil {
			counts[status] = count
		}
	}
	return counts, nil
}

// PromoteRawMemory moves an approved raw_memory entry to the memories table.
func (dm *DatabaseManager) PromoteRawMemory(raw *RawMemory) error {
	now := time.Now()
	metadata := map[string]interface{}{
		"source_db": raw.SourceDB,
		"source_id": raw.SourceID,
		"import_batch": raw.ImportBatch,
	}
	metadataJSON, _ := json.Marshal(metadata)

	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, source_db, source_id, promoted_at)
		VALUES (?, 'memories', ?, '[]', ?, ?, ?, ?, ?)
	`, GenerateID(), raw.Text, string(metadataJSON), now.Format(time.RFC3339), raw.SourceDB, raw.SourceID, float64(now.Unix()))
	return err
}

// ListIngestBatches returns distinct import batches with counts.
func (dm *DatabaseManager) ListIngestBatches() ([]map[string]interface{}, error) {
	rows, err := dm.db.Query(`
		SELECT import_batch, status, COUNT(*) as count
		FROM raw_memories
		GROUP BY import_batch, status
		ORDER BY MAX(updated_at) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	batches := map[string]map[string]interface{}{}
	for rows.Next() {
		var batch string
		var status string
		var count int
		if err := rows.Scan(&batch, &status, &count); err != nil {
			continue
		}
		if _, ok := batches[batch]; !ok {
			batches[batch] = map[string]interface{}{"batch": batch}
		}
		batches[batch][status] = count
	}

	var result []map[string]interface{}
	for _, b := range batches {
		result = append(result, b)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("ListIngestBatches rows error: %w", err)
	}
	return result, nil
}

// GetExternalDBCursor retrieves the last-seen cursor for a given external DB label.
// Returns ("", nil) if no cursor exists yet.
func (dm *DatabaseManager) GetExternalDBCursor(label string) (string, error) {
	var cursor string
	err := dm.db.QueryRow(`SELECT last_cursor FROM external_db_cursors WHERE db_label = ?`, label).Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return cursor, err
}

// SetExternalDBCursor persists the cursor for a given external DB label.
// Uses INSERT OR REPLACE so restarts don't lose cursor.
func (dm *DatabaseManager) SetExternalDBCursor(label, cursor string) error {
	_, err := dm.db.Exec(`
		INSERT OR REPLACE INTO external_db_cursors (db_label, last_cursor, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)`,
		label, cursor)
	return err
}
