package internal

import (
	"database/sql"
	"encoding/json"
	"time"
)

// DLQEntry represents a failed synthesis event awaiting retry.
type DLQEntry struct {
	ID        string    // ULID, primary key
	MemoryID  string    // FK to memories.id
	Content   string    // full memory content
	Tags      []string  // JSON array
	Attempt   int       // number of attempts made
	LastError string    // most recent error message
	CreatedAt time.Time // when first enqueued
	NextRetry time.Time // next scheduled retry (ISO8601)
}

// ── Schema ──────────────────────────────────────────────────────────────────

// EnsureDLQSchema creates the DLQ table if it doesn't exist.
func EnsureDLQSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS synthesis_dlq (
			id          TEXT PRIMARY KEY,
			memory_id   TEXT NOT NULL,
			content     TEXT NOT NULL,
			tags        TEXT,
			attempt     INTEGER DEFAULT 0,
			last_error  TEXT,
			created_at  TEXT DEFAULT (datetime('now')),
			next_retry  TEXT
		)
	`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_dlq_next_retry ON synthesis_dlq(next_retry)`)
	return err
}

// ── Enqueue ─────────────────────────────────────────────────────────────────

// DLQEnqueue adds a failed synthesis event to the dead letter queue.
// Called when all vendors in the fallback chain have failed.
func DLQEnqueue(db *sql.DB, memoryID, content string, tags []string, lastErr error) error {
	id := GenerateID()

	tagsJSON, _ := json.Marshal(tags)
	lastErrStr := ""
	if lastErr != nil {
		lastErrStr = lastErr.Error()
	}

	// First attempt: retry in 1 minute
	nextRetry := time.Now().Add(1 * time.Minute).UTC()

	_, err := db.Exec(`
		INSERT INTO synthesis_dlq (id, memory_id, content, tags, attempt, last_error, created_at, next_retry)
		VALUES (?, ?, ?, ?, 0, ?, datetime('now'), ?)
	`, id, memoryID, content, string(tagsJSON), lastErrStr, nextRetry.Format(time.RFC3339))

	return err
}

// ── Retry Logic ─────────────────────────────────────────────────────────────

// backoffIntervals defines exponential backoff: 1m → 5m → 30m → 2h → 8h → 24h (capped)
var backoffIntervals = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	8 * time.Hour,
	24 * time.Hour,
}

func nextRetryDelay(attempt int) time.Duration {
	if attempt >= len(backoffIntervals) {
		return backoffIntervals[len(backoffIntervals)-1]
	}
	return backoffIntervals[attempt]
}

// DLQReadyt returns entries whose next_retry time has passed.
func DLQReady(db *sql.DB) ([]DLQEntry, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT id, memory_id, content, tags, attempt, last_error, created_at, next_retry
		FROM synthesis_dlq
		WHERE next_retry IS NOT NULL AND next_retry <= ?
		ORDER BY created_at ASC
		LIMIT 20
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []DLQEntry
	for rows.Next() {
		var e DLQEntry
		var tagsRaw string
		var lastErr, nextRetry, createdAt string
		if err := rows.Scan(&e.ID, &e.MemoryID, &e.Content, &tagsRaw, &e.Attempt, &lastErr, &createdAt, &nextRetry); err != nil {
			continue
		}
		e.LastError = lastErr
		if nextRetry != "" {
			t, _ := time.Parse(time.RFC3339, nextRetry)
			e.NextRetry = t
		}
		if createdAt != "" {
			t, _ := time.Parse(time.RFC3339, createdAt)
			e.CreatedAt = t
		}
		if tagsRaw != "" {
			json.Unmarshal([]byte(tagsRaw), &e.Tags)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// DLQRemove deletes a successfully processed entry.
func DLQRemove(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM synthesis_dlq WHERE id = ?`, id)
	return err
}

// DLQUpdateRetry updates attempt count and next_retry time after a failed retry.
func DLQUpdateRetry(db *sql.DB, id string, attempt int, lastErr error) error {
	delay := nextRetryDelay(attempt)
	nextRetry := time.Now().Add(delay).UTC()
	lastErrStr := ""
	if lastErr != nil {
		lastErrStr = lastErr.Error()
	}
	_, err := db.Exec(`
		UPDATE synthesis_dlq
		SET attempt = ?, last_error = ?, next_retry = ?
		WHERE id = ?
	`, attempt, lastErrStr, nextRetry.Format(time.RFC3339), id)
	return err
}

// ── Overflow Deferred Queue ──────────────────────────────────────────────

// OverflowEntry mirrors DLQEntry for raw_memories overflow_deferred entries.
type OverflowEntry struct {
	ID        string
	MemoryID  string
	Content   string
	Tags      []string
	Attempt   int
	LastError string
	CreatedAt time.Time
	NextRetry time.Time
}

// OverflowReady returns overflow_deferred raw_memory entries whose next_retry has passed.
func OverflowReady(db *sql.DB) ([]OverflowEntry, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT id, source_id, text, metadata, COALESCE(attempt, 0),
		       ingested_at, next_retry
		FROM raw_memories
		WHERE status IN ('pending', 'overflow_deferred')
		  AND next_retry IS NOT NULL AND next_retry <= ?
		ORDER BY ingested_at ASC
		LIMIT 20
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []OverflowEntry
	for rows.Next() {
		var e OverflowEntry
		var metadataStr, nextRetryStr string
		var ingestedAt float64
		if err := rows.Scan(&e.ID, &e.MemoryID, &e.Content, &metadataStr, &e.Attempt,
			&ingestedAt, &nextRetryStr); err != nil {
			continue
		}
		if nextRetryStr != "" {
			t, _ := time.Parse(time.RFC3339, nextRetryStr)
			e.NextRetry = t
		}
		e.CreatedAt = time.Unix(int64(ingestedAt), 0)
		if metadataStr != "" {
			var meta struct {
				Tags []string `json:"tags"`
			}
			json.Unmarshal([]byte(metadataStr), &meta)
			e.Tags = meta.Tags
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// OverflowResolve marks an overflow_deferred entry as successfully processed.
func OverflowResolve(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE raw_memories SET status = 'overflow_resolved' WHERE id = ?`, id)
	return err
}

// OverflowUpdateRetry updates attempt count and next_retry after a failed retry.
func OverflowUpdateRetry(db *sql.DB, id string, attempt int, lastErr error) error {
	delay := nextRetryDelay(attempt)
	nextRetry := time.Now().Add(delay).UTC()
	lastErrStr := ""
	if lastErr != nil {
		lastErrStr = lastErr.Error()
	}
	_, err := db.Exec(`UPDATE raw_memories SET attempt = ?, next_retry = ?, llm_notes = ? WHERE id = ?`,
		attempt, nextRetry.Format(time.RFC3339), lastErrStr, id)
	return err
}

// DLQStats returns count and oldest entry age for status reporting.
func DLQStats(db *sql.DB) (int, time.Time, error) {
	var count int
	row := db.QueryRow(`SELECT COUNT(*) FROM synthesis_dlq`)
	if err := row.Scan(&count); err != nil {
		return 0, time.Time{}, err
	}
	var oldestStr string
	row = db.QueryRow(`SELECT MIN(created_at) FROM synthesis_dlq`)
	if err := row.Scan(&oldestStr); err != nil || oldestStr == "" {
		return count, time.Time{}, nil
	}
	// Parse RFC3339 string — created_at stored as datetime('now') → "2026-05-30 19:57:51"
	t, err := time.Parse("2006-01-02 15:04:05", oldestStr)
	if err != nil {
		return count, time.Time{}, nil
	}
	return count, t, nil
}
