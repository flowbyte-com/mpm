// evidence_store.go — DB-touching code for the confidence/evidence foundation.
//
// RecomputeConfidence is the single authoritative entry point for updating
// an artifact's confidence. Every confidence change — from triggers, from
// idle_dream, from manual CLI, from future calibration code — flows through
// this function. This is the centralization the design discussion called for.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// RecomputeReason is the trigger cause for a confidence change. The string
// values must match the CHECK constraint on confidence_history.trigger.
type RecomputeReason string

const (
	RecomputeReasonEvidenceAdded   RecomputeReason = "evidence_added"
	RecomputeReasonEvidenceUpdated RecomputeReason = "evidence_updated"
	RecomputeReasonEvidenceDeleted RecomputeReason = "evidence_deleted"
	RecomputeReasonEvidenceExpired RecomputeReason = "evidence_expired"
	RecomputeReasonDecayTick       RecomputeReason = "decay_tick"
	RecomputeReasonManual          RecomputeReason = "manual_recompute"
)

// EvidenceInput is the public shape for adding evidence. The DB wrapper
// fills in the id and created_at if not provided.
type EvidenceInput struct {
	ArtifactID         string
	ArtifactType       string
	Type               string
	SourceGroup        string
	Strength           float64
	IndependenceFactor float64
	CreatedBy          string
	CreatedAt          time.Time
	ExpiresAt          *time.Time
	Notes              string
}

// AddEvidence inserts an evidence row and triggers a confidence recompute.
//
// The DB trigger on the evidence table is registered in db.go and would
// fire the registered confidence_recompute function on INSERT/UPDATE/DELETE.
// In practice we call RecomputeConfidence directly here: SQLite's locking
// model (a single writer at a time) means the trigger callback cannot
// safely issue writes on a second connection while the INSERT that fired
// the trigger is still in progress, and using the trigger's own connection
// is not exposed by mattn/go-sqlite3's RegisterFunc API. Doing the
// recompute from Go here keeps the chain synchronous and matches the
// "single authoritative entry point" design — every confidence change still
// flows through RecomputeConfidence, just without going through the
// trigger indirection.
func AddEvidence(dm *DatabaseManager, in EvidenceInput) error {
	if !IsValidEvidenceType(in.Type) {
		return fmt.Errorf("invalid evidence type: %q", in.Type)
	}
	if in.ArtifactType == "" {
		return fmt.Errorf("artifact_type required")
	}
	if in.SourceGroup == "" {
		return fmt.Errorf("source_group required")
	}
	if in.CreatedBy == "" {
		return fmt.Errorf("created_by required")
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now()
	}
	if in.IndependenceFactor == 0 {
		in.IndependenceFactor = 1.0
	}

	id := GenerateID()
	var expiresAt *int64
	if in.ExpiresAt != nil {
		exp := in.ExpiresAt.Unix()
		expiresAt = &exp
	}

	_, err := dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                     strength, independence_factor, created_by, created_at, expires_at, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, 0, id, in.ArtifactID, in.ArtifactType, in.Type, in.SourceGroup,
		in.Strength, in.IndependenceFactor, in.CreatedBy, in.CreatedAt.Unix(), expiresAt, in.Notes)
	if err != nil {
		return fmt.Errorf("insert evidence: %w", err)
	}
	// The trigger may have fired confidence_recompute as well; we don't
	// depend on it because of the SQLite locking issue described above.
	// Call RecomputeConfidence directly so the confidence is updated
	// synchronously and history rows are appended.
	return RecomputeConfidence(dm, in.ArtifactID, in.ArtifactType, RecomputeReasonEvidenceAdded)
}

// RecomputeConfidence is the single authoritative entry point. It loads the
// current evidence set for the artifact, runs the math, and writes both
// the new confidence on the artifact and a new row in confidence_history.
//
// Called by:
//   - The SQLite triggers (via the registered SQL function)
//   - idle_dream (for decay_tick recompute)
//   - Manual CLI (`mpm ops confidence recompute`)
//   - Future calibration/challenge code
func RecomputeConfidence(dm *DatabaseManager, artifactID, artifactType string, reason RecomputeReason) error {
	now := time.Now()

	// Load the evidence set.
	ev, lastPositiveAt, err := loadEvidenceForRecompute(dm, artifactID, artifactType, now)
	if err != nil {
		return fmt.Errorf("load evidence: %w", err)
	}

	// Run the math.
	conf := computeConfidence(artifactType, ev, now, lastPositiveAt, 0.005)

	// Update the artifact's confidence column.
	if err := writeArtifactConfidence(dm, artifactID, artifactType, conf); err != nil {
		return fmt.Errorf("write artifact confidence: %w", err)
	}

	// Append the history row.
	historyID := GenerateID()
	_, err = dm.ExecTracked(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, 0, historyID, artifactID, artifactType, conf, now.Unix(), len(ev), string(reason))
	if err != nil {
		return fmt.Errorf("insert history row: %w", err)
	}
	return nil
}

// loadEvidenceForRecompute returns the evidence set and the most recent
// positive-evidence timestamp (for decay anchoring).
func loadEvidenceForRecompute(dm *DatabaseManager, artifactID, artifactType string, now time.Time) ([]evidenceInput, time.Time, error) {
	rows, err := dm.QueryTracked(`
		SELECT strength, independence_factor, created_at, expires_at
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
	`, artifactID, artifactType)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()

	var out []evidenceInput
	var lastPositiveAt time.Time
	for rows.Next() {
		var strength, independence float64
		var createdAt int64
		var expiresAt sql.NullInt64
		if err := rows.Scan(&strength, &independence, &createdAt, &expiresAt); err != nil {
			return nil, time.Time{}, err
		}
		// Skip expired evidence.
		if expiresAt.Valid && expiresAt.Int64 < now.Unix() {
			continue
		}
		ts := time.Unix(createdAt, 0)
		out = append(out, evidenceInput{
			Strength:    strength,
			Independence: independence,
			CreatedAt:   ts,
		})
		if strength > 0 && ts.After(lastPositiveAt) {
			lastPositiveAt = ts
		}
	}
	if lastPositiveAt.IsZero() {
		// No positive evidence — anchor decay to the artifact's creation time
		// so it decays from "now" rather than from 1970. Falls back to
		// epoch if even that isn't available.
		lastPositiveAt = now
		if createdAt, ok := readArtifactCreatedAt(dm, artifactID, artifactType); ok {
			lastPositiveAt = createdAt
		}
	}
	return out, lastPositiveAt, rows.Err()
}

func readArtifactCreatedAt(dm *DatabaseManager, artifactID, artifactType string) (time.Time, bool) {
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	var createdAt string
	err := dm.QueryRowTracked(fmt.Sprintf(`SELECT created_at FROM %s WHERE id = ?`, table), artifactID).Scan(&createdAt)
	if err != nil {
		return time.Time{}, false
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// writeArtifactConfidence updates the confidence column on the artifact
// table. The table is derived from the artifact type.
func writeArtifactConfidence(dm *DatabaseManager, artifactID, artifactType string, conf float64) error {
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	_, err := dm.ExecTracked(
		fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE id = ?`, table),
		0, conf, artifactID,
	)
	return err
}

// parseTime accepts RFC3339 or SQLite "YYYY-MM-DD HH:MM:SS" formats and
// returns the parsed time.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}
