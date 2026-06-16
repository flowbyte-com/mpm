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
	// Scan notes (and the user-supplied identifying fields) for secrets and
	// poison phrases. The `notes` field is the only free-form text on an
	// evidence row, but `SourceGroup`/`CreatedBy` can also smuggle content
	// in practice. The 20-pattern scanner is the same one used by
	// MemoryStore.AddMemory; bypass here would be a known audit finding.
	if isSensitive, reason := isSensitiveContent(in.Notes); isSensitive {
		return fmt.Errorf("sensitive content in evidence notes: %s", reason)
	}
	if isPoisoned, reason := isPoisoned(in.Notes); isPoisoned {
		return fmt.Errorf("poison content in evidence notes: %s", reason)
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

// Evidence is the DB-read view of an evidence row.
type Evidence struct {
	ID                 string
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

// ListEvidenceForArtifact returns all non-expired evidence rows for an
// artifact, ordered by created_at ascending.
func ListEvidenceForArtifact(dm *DatabaseManager, artifactID, artifactType string) ([]Evidence, error) {
	rows, err := dm.QueryTracked(`
		SELECT id, artifact_id, artifact_type, type, source_group,
		       strength, independence_factor, created_by, created_at, expires_at, notes
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY created_at ASC
	`, artifactID, artifactType)
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var createdAt int64
		var expiresAt sql.NullInt64
		if err := rows.Scan(&e.ID, &e.ArtifactID, &e.ArtifactType, &e.Type, &e.SourceGroup,
			&e.Strength, &e.IndependenceFactor, &e.CreatedBy, &createdAt, &expiresAt, &e.Notes); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		e.CreatedAt = time.Unix(createdAt, 0)
		if expiresAt.Valid {
			if expiresAt.Int64 < now {
				continue // skip expired
			}
			exp := time.Unix(expiresAt.Int64, 0)
			e.ExpiresAt = &exp
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ConfidenceSnapshot is a point-in-time view of an artifact's confidence state.
type ConfidenceSnapshot struct {
	ArtifactID   string                `json:"artifact_id"`
	ArtifactType string                `json:"artifact_type"`
	Confidence   float64               `json:"confidence"`
	HistoryCount int                   `json:"history_count"`
	History      []ConfidenceHistoryRow `json:"history"`
}

// ConfidenceHistoryRow is one row of the confidence_history table.
type ConfidenceHistoryRow struct {
	Confidence    float64   `json:"confidence"`
	ComputedAt    time.Time `json:"computed_at"`
	EvidenceCount int       `json:"evidence_count"`
	Trigger       string    `json:"trigger"`
}

// GetConfidenceForArtifact returns the current confidence and the last `limit`
// history rows for an artifact, ordered by computed_at DESC.
func GetConfidenceForArtifact(dm *DatabaseManager, artifactID, artifactType string, limit int) (*ConfidenceSnapshot, error) {
	table := "memories"
	if artifactType == "lesson" {
		table = "lessons"
	}
	var conf float64
	err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, table),
		artifactID,
	).Scan(&conf)
	if err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}

	if limit <= 0 {
		limit = 10
	}
	rows, err := dm.QueryTracked(`
		SELECT confidence, computed_at, evidence_count, trigger
		FROM confidence_history
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY computed_at DESC
		LIMIT ?
	`, artifactID, artifactType, limit)
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	defer rows.Close()

	snap := &ConfidenceSnapshot{
		ArtifactID:   artifactID,
		ArtifactType: artifactType,
		Confidence:   conf,
		History:      []ConfidenceHistoryRow{},
	}
	for rows.Next() {
		var h ConfidenceHistoryRow
		var computedAt int64
		if err := rows.Scan(&h.Confidence, &computedAt, &h.EvidenceCount, &h.Trigger); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		h.ComputedAt = time.Unix(computedAt, 0)
		snap.History = append(snap.History, h)
	}
	snap.HistoryCount = len(snap.History)
	return snap, rows.Err()
}
