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
	ID        string
	Path      string
	Source    string
	StartLine int
	EndLine   int
	Hash      string // content SHA-256
	Model     string
	Text      string
	UpdatedAt int64
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
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tableCount); err != nil {
		return nil, fmt.Errorf("detect schema: count tables: %w", err)
	}

	var chunksExists int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chunks'").Scan(&chunksExists); err != nil {
		return nil, fmt.Errorf("detect schema: probe chunks: %w", err)
	}

	if chunksExists == 1 {
		// Verify expected columns
		var colList string
		if err := db.QueryRow("SELECT GROUP_CONCAT(name) FROM pragma_table_info('chunks')").Scan(&colList); err != nil {
			return nil, fmt.Errorf("detect schema: pragma_table_info: %w", err)
		}
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
					ID:          GenerateID(),
					SourceID:    mem.ID,
					SourceDB:    adapter.Name(),
					ContentHash: hex.EncodeToString(contentHash[:]),
					Text:        mem.Content,
					Metadata:    string(metadataJSON),
					IngestedAt:  now,
					Status:      "pending",
					ImportBatch: importBatch,
					UpdatedAt:   now,
					ExpiresAt:   now + (30 * 24 * 60 * 60),
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

// contentHashExistsInMemory checks if a content hash is already in the memory table.
func (dm *DatabaseManager) contentHashExistsInMemory(contentHash string) (bool, error) {
	var count int
	if err := dm.db.QueryRow("SELECT COUNT(*) FROM memories WHERE content_hash = ?", contentHash).Scan(&count); err != nil {
		return false, fmt.Errorf("content hash existence check: %w", err)
	}
	return count > 0, nil
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
			return nil, fmt.Errorf("scanning raw memory row: %w", err)
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
			return nil, fmt.Errorf("scanning ingest batch row: %w", err)
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


