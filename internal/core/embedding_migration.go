// embedding_migration.go — Forensic classifier + migration orchestrator.
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
	"os"
	"path/filepath"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
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

// ─── Migration orchestrator ──────────────────────────────────────────────────

// RunMigration performs the one-shot embedding remediation.
//   1. Pre-migration backup via VACUUM INTO.
//   2. Forensic classifier (RunForensicClassifier).
//   3. Mark synthetic theories (the 65 fabricated collision records).
//   4. Provenance-gated un-challenge.
//   5. Sentinel row for idempotency.
//
// Idempotent: a sentinel row at embedding_migration_log with
// reason='migration_applied' short-circuits subsequent runs.
func RunMigration(dm *DatabaseManager) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("RunMigration: nil database manager")
	}
	if already, err := IsAlreadyApplied(dm); err != nil {
		return err
	} else if already {
		return nil
	}

	backupPath, err := takeBackup(dm)
	if err != nil {
		return fmt.Errorf("RunMigration: backup: %w", err)
	}
	slog.Info("RunMigration: pre-migration backup", "path", backupPath)

	// 2. Forensic classifier.
	if err := RunForensicClassifier(dm); err != nil {
		return fmt.Errorf("RunMigration: classifier: %w", err)
	}

	// 3. Mark synthetic theories.
	if n, err := markSyntheticTheories(dm); err != nil {
		return fmt.Errorf("RunMigration: synthetic theories: %w", err)
	} else {
		slog.Info("RunMigration: theories marked synthetic", "count", n)
	}

	// 4. Provenance-gated un-challenge.
	if n, err := runProvenanceGatedUnchallenge(dm); err != nil {
		return fmt.Errorf("RunMigration: un-challenge: %w", err)
	} else {
		slog.Info("RunMigration: memories auto-unchallenged", "count", n)
	}

	// 5. Sentinel row.
	if err := recordSentinel(dm); err != nil {
		return fmt.Errorf("RunMigration: sentinel: %w", err)
	}
	return nil
}

// IsAlreadyApplied returns true if a sentinel row exists in embedding_migration_log.
func IsAlreadyApplied(dm *DatabaseManager) (bool, error) {
	var n int
	err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM embedding_migration_log
		WHERE reason = 'migration_applied'
	`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// takeBackup creates a timestamped backup via SQLite VACUUM INTO.
// The backup is stored in <workspace>/migrations/embeddings-<timestamp>.db.bak.
// NOTE: origin_weight column does NOT exist on memories (verified at e62f88f).
// The action-precondition heuristic uses weight<1.0 (memories with reduced
// trust have weight below the default of 1.0).
func takeBackup(dm *DatabaseManager) (string, error) {
	workspace := config.GetWorkspace()
	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	dir := filepath.Join(workspace, "migrations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "embeddings-"+ts+".db.bak")
	// VACUUM INTO is atomic (writes to temp then renames).
	if _, err := dm.db.Exec("VACUUM INTO ?", path); err != nil {
		return "", err
	}
	return path, nil
}

// markSyntheticTheories marks as synthetic=1 every theory (collection='theories')
// whose challenged-memory was hash-embedded (embedding_source='hash').
//
// NOTE: the memories.kind column does not exist in the current schema.
// The spec referenced kind IN ('semantic_collision', 'provenance_collision',
// 'unresolved_state_collision') but that column was never added.
// We identify collision theories by whether their challenged memory was
// hash-embedded.
//
// Theories (collection='theories') reference the challenged memory via
// json_extract(metadata, '$.challenged_memory_id') — NOT via source_id.
// Production theory creation lives in epistemology_tools.go:54-60 and
// stores the reference in metadata; source_id is NULL on real theories
// (194/194 in the live DB at the time of this migration).
func markSyntheticTheories(dm *DatabaseManager) (int, error) {
	res, err := dm.db.Exec(`
		UPDATE memories
		SET synthetic = 1
		WHERE synthetic = 0
		  AND collection = 'theories'
		  AND json_extract(metadata, '$.challenged_memory_id') IN (
			  SELECT id FROM memories
			  WHERE embedding_source = 'hash' AND deleted_at IS NULL
		  )
	`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// runProvenanceGatedUnchallenge restores weight for memories whose ONLY
// challenges are synthetic theories. Strict gate: every theory challenging
// the memory must be synthetic=1. Action-precondition: weight < 1.0
// (origin_weight column absent; weight<1.0 indicates trust reduction).
//
// NOTE: the memories.kind column does not exist, so all theories are
// treated uniformly. The gate uses NOT EXISTS to ensure ALL challenges
// are synthetic before restoring weight.
//
// Theory → challenged-memory reference lives in
// json_extract(metadata, '$.challenged_memory_id'), not source_id —
// see markSyntheticTheories for why.
func runProvenanceGatedUnchallenge(dm *DatabaseManager) (int, error) {
	// Find candidate memories: challenged only by synthetic theories, currently reduced.
	rows, err := dm.db.Query(`
		SELECT m.id, m.weight
		FROM memories m
		WHERE m.deleted_at IS NULL
		  AND m.weight < 1.0
		  AND EXISTS (
			  SELECT 1 FROM memories t
			  WHERE json_extract(t.metadata, '$.challenged_memory_id') = m.id
			    AND t.collection = 'theories'
			    AND t.synthetic = 1
		  )
		  AND NOT EXISTS (
			  SELECT 1 FROM memories t
			  WHERE json_extract(t.metadata, '$.challenged_memory_id') = m.id
			    AND t.collection = 'theories'
			    AND t.synthetic = 0
		  )
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type cand struct {
		id    string
		oldW  float64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.oldW); err != nil {
			return 0, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(cands) == 0 {
		return 0, nil
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	for _, c := range cands {
		newW := 1.0 // default weight; origin_weight column absent
		if _, err := tx.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, newW, c.id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`
			INSERT INTO embedding_migration_log
				(memory_id, old_weight, new_weight, reason, migrated_at)
			VALUES (?, ?, ?, 'unchallenge_provenance_gated', CAST(strftime('%s','now') AS INTEGER))
		`, c.id, c.oldW, newW); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(cands), nil
}

// recordSentinel writes the idempotency sentinel to embedding_migration_log.
func recordSentinel(dm *DatabaseManager) error {
	_, err := dm.db.Exec(`
		INSERT INTO embedding_migration_log
			(memory_id, old_weight, new_weight, reason, migrated_at)
		VALUES ('sentinel', 0, 0, 'migration_applied', CAST(strftime('%s','now') AS INTEGER))
	`)
	return err
}
