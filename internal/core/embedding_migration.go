// embedding_migration.go — Forensic classifier.
//
// Establishes provenance for every embedding row in the memories
// table. Idempotent: a second run is a no-op.
//
// Classification (per spec §5.4):
//   - JSON-parseable []float32 of length 256 → 'hash', dimension=256
//   - JSON-parseable []float32 of length != 256 → 'provider', dimension=<length>
//   - SQL NULL → 'null', dimension=NULL
//   - Literal string 'null' → 'null', dimension=NULL
//
// The classifier is read-only at the application level but updates
// embedding_source and embedding_dimension for any row whose values
// do not already match the classification. This is the operator-
// driven backfill; it is NOT auto-run on boot.
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
)

// RunForensicClassifier classifies every live memories row's
// embedding column into embedding_source and embedding_dimension.
// Idempotent.
func RunForensicClassifier(dm *DatabaseManager) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("RunForensicClassifier: nil database manager")
	}

	rows, err := dm.db.Query(`
		SELECT id, embedding
		FROM memories
		WHERE deleted_at IS NULL
	`)
	if err != nil {
		return fmt.Errorf("RunForensicClassifier: query: %w", err)
	}
	defer rows.Close()

	type update struct {
		id        string
		source    string
		dimension sql.NullInt64
	}
	var updates []update

	for rows.Next() {
		var id string
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			return fmt.Errorf("RunForensicClassifier: scan: %w", err)
		}

		// SQL NULL → 'null'
		if !raw.Valid {
			updates = append(updates, update{id, "null", sql.NullInt64{}})
			continue
		}

		// Literal 'null' string → 'null'
		if raw.String == "null" {
			updates = append(updates, update{id, "null", sql.NullInt64{}})
			continue
		}

		// JSON-parseable []float32
		var vec []float32
		if err := json.Unmarshal([]byte(raw.String), &vec); err != nil {
			// Unknown shape — log and treat as null.
			slog.Warn("RunForensicClassifier: unparseable embedding; treating as null",
				"memory_id", id, "error", err.Error())
			updates = append(updates, update{id, "null", sql.NullInt64{}})
			continue
		}

		dim := len(vec)
		source := "provider"
		if dim == 256 {
			source = "hash"
		}
		updates = append(updates, update{id, source, sql.NullInt64{Int64: int64(dim), Valid: true}})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("RunForensicClassifier: rows.Err: %w", err)
	}

	// Apply updates. SQLite is single-writer; sequential updates in
	// a single transaction keep this fast (1081 rows ≈ <1s).
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("RunForensicClassifier: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		UPDATE memories
		SET embedding_source = ?, embedding_dimension = ?
		WHERE id = ?
	`)
	if err != nil {
		return fmt.Errorf("RunForensicClassifier: prepare: %w", err)
	}
	defer stmt.Close()

	for _, u := range updates {
		var dimArg interface{}
		if u.dimension.Valid {
			dimArg = u.dimension.Int64
		} else {
			dimArg = nil
		}
		if _, err := stmt.Exec(u.source, dimArg, u.id); err != nil {
			return fmt.Errorf("RunForensicClassifier: exec %s: %w", u.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("RunForensicClassifier: commit: %w", err)
	}

	slog.Info("RunForensicClassifier: complete", "rows_scanned", len(updates))
	return nil
}
