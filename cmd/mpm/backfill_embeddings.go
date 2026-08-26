package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleBackfillEmbeddings runs the embedding backfill pipeline.
func handleBackfillEmbeddings(args []string) int {
	fs := flag.NewFlagSet("backfill-embeddings", flag.ContinueOnError)
	batchSize := fs.Int("batch-size", 50, "Memories per batch")
	dryRun := fs.Bool("dry-run", false, "Count only, don't write embeddings")
	collection := fs.String("collection", "", "Filter by collection (empty = all)")
	fs.Usage = func() {
		fmt.Println("Usage: mpm ops backfill-embeddings [flags]")
		fmt.Println("\nFlags:")
		fmt.Println("  --batch-size <n>   Memories per batch (default 50)")
		fmt.Println("  --collection <c>    Filter by collection (default: all)")
		fmt.Println("  --dry-run           Count only, don't write embeddings")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// Use the shared DM singleton (getDB) instead of opening a fresh
	// connection via mpminternal.NewDatabaseManager(""). The previous
	// code created a separate connection per call, which (a) re-ran
	// the schema migration on every invocation, (b) re-ATTACHed the
	// shared DB unnecessarily, and (c) bypassed the per-component
	// connection ownership pattern that CoreDB Phase 2 introduced.
	// The shared singleton is the canonical entry point: see
	// getDB() in cmd/mpm/handlers.go.
	dm := getDB()
	if dm == nil {
		return 1
	}
	defer dm.Close()

	// Count total missing (no provider needed for this)
	total, err := countMemoriesWithoutEmbedding(dm.SQLDB(), *collection)
	if err != nil {
		return usererror.Error("Error counting memories: %v", err)
	}
	fmt.Printf("   Total memories missing embeddings: %d\n", total)

	if *dryRun {
		return 0
	}

	if total == 0 {
		fmt.Println("   Nothing to do.")
		return 0
	}

	// Provider needed for actual embedding generation
	cfg := mpminternal.DefaultEmbeddingConfig()
	provider := cfg.Provider

	if provider.Name() == "null" {
		return usererror.Error("no embedding provider available.\nSet OLLAMA_ENDPOINT and OLLAMA_MODEL env vars and ensure Ollama is running.")
	}
	fmt.Printf("⚡ Embedding backfill using %s\n", provider.Name())

	// Process in batches
	batch := *batchSize
	processed := 0
	failed := 0
	offset := 0

	for {
		memories, err := fetchMemoriesWithoutEmbedding(dm.SQLDB(), *collection, batch, offset)
		if err != nil {
			usererror.Error("Error fetching batch: %v", err)
			break
		}
		if len(memories) == 0 {
			break
		}

		for _, mem := range memories {
			vec, err := provider.Embed(mem.Content)
			if err != nil {
				// Log to mirror.jsonl and continue
				logEmbeddingFailure(mem.ID, mem.Content, err)
				failed++
				continue
			}

			if err := updateMemoryEmbedding(dm.SQLDB(), mem.ID, vec); err != nil {
				logEmbeddingFailure(mem.ID, mem.Content, err)
				failed++
				continue
			}
			processed++
		}

		offset += batch
		fmt.Printf("   Progress: %d/%d done, %d failed\n", processed, total, failed)

		if len(memories) < batch {
			break
		}
	}

	fmt.Printf("\n⚡ Backfill complete: %d succeeded, %d failed (of %d total)\n", processed, failed, total)
	if failed > 0 {
		fmt.Println("   Failed entries logged to mirror.jsonl for review.")
	}
	return 0
}

// ── SQL helpers ───────────────────────────────────────────────────────────────

type memRow struct {
	ID        string
	Content   string
	CreatedAt string
}

func countMemoriesWithoutEmbedding(db *sql.DB, collection string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM memories WHERE (embedding IS NULL OR embedding = 'null') AND deleted_at IS NULL`
	args := []interface{}{}
	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}
	row := db.QueryRow(query, args...)
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func fetchMemoriesWithoutEmbedding(db *sql.DB, collection string, limit, offset int) ([]memRow, error) {
	query := `SELECT id, content, created_at FROM memories WHERE (embedding IS NULL OR embedding = 'null') AND deleted_at IS NULL`
	args := []interface{}{}
	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}
	query += " ORDER BY created_at ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []memRow
	for rows.Next() {
		var m memRow
		if err := rows.Scan(&m.ID, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning memory without embedding row: %w", err)
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

func updateMemoryEmbedding(db *sql.DB, id string, vec []float32) error {
	bytes, _ := json.Marshal(vec)
	_, err := db.Exec(`UPDATE memories SET embedding = ? WHERE id = ?`, string(bytes), id)
	return err
}

// ── Logging ───────────────────────────────────────────────────────────────────────

func logEmbeddingFailure(id, content string, err error) {
	entry := map[string]interface{}{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"type":      "embedding_failure",
		"memory_id": id,
		"error":     err.Error(),
	}
	data, _ := json.Marshal(entry)
	var buf bytes.Buffer
	buf.Write(data)
	buf.WriteByte('\n')

	f, openErr := os.OpenFile(config.GetMirrorPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if openErr != nil {
		slog.Warn("embedding-backfill: failed to open mirror log", "error", openErr)
		return
	}
	defer f.Close()
	f.Write(buf.Bytes())
}
