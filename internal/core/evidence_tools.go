// evidence_tools.go — DM methods for the confidence/evidence engine.
//
// Nine methods covering the full evidence + confidence surface:
//   - AddEvidence: insert an evidence row, return updated confidence
//   - ListEvidence: rows for an artifact
//   - QueryConfidenceHistory: full timeline
//   - QueryConfidenceChanges: recent delta events
//   - QueryConfidenceTrend: trajectory projection
//   - QueryMemoryQuality: per-creator survival stats
//   - ShowConfidence: current + history snapshot
//   - RecomputeConfidence: force manual recompute
//   - ExplainConfidence: reasoning trace for the current number
//
// All nine route through the underlying functions in audit.go,
// evidence.go, and confidence.go. The DM methods here exist only to
// give the JSON payload boundary a typed, validated entry point with
// consistent error wrapping and wire-format shape.
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// AddEvidence inserts an evidence row and returns the resulting confidence
// for the artifact. Validation boundary: required-arg preflight +
// IsValidEvidenceType + strength default. internal.AddEvidence runs the
// deeper checks (sensitive-content scan, type registry, recompute).
//
// Both `mpm call add_evidence` and the `add_evidence` MCP tool route
// through this method, so neither surface can bypass validation.
//
// Returns the same map the previous callAddEvidence returned:
//   {"success": true, "confidence": <float>}
//
// Required fields: artifact_id, type, source_group, created_by.
func (dm *DatabaseManager) AddEvidence(in EvidenceInput) (map[string]interface{}, error) {
	if in.ArtifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if in.Type == "" {
		return nil, fmt.Errorf("type is required")
	}
	if !IsValidEvidenceType(in.Type) {
		return nil, fmt.Errorf("invalid evidence type: %q", in.Type)
	}
	if in.SourceGroup == "" {
		return nil, fmt.Errorf("source_group is required")
	}
	if in.CreatedBy == "" {
		return nil, fmt.Errorf("created_by is required")
	}
	if in.ArtifactType == "" {
		in.ArtifactType = "memory"
	}
	// Fill strength from the registry default if the caller passed 0.
	if in.Strength == 0 {
		if def, ok := DefaultStrength(in.Type); ok {
			in.Strength = def
		}
	}
	// Default independence to 1.0 to match the call handler behavior.
	if in.IndependenceFactor == 0 {
		in.IndependenceFactor = 1.0
	}

	if err := AddEvidence(dm, in); err != nil {
		return nil, err
	}

	// Works don't have a confidence column — verification is handled via
	// DeriveWorkVerification, not the confidence model. Skip the confidence
	// read to avoid a "no such column" error on the works table.
	if in.ArtifactType == "work" {
		return map[string]interface{}{"success": true}, nil
	}

	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, ArtifactTable(in.ArtifactType)),
		in.ArtifactID,
	).Scan(&conf); err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}
	return map[string]interface{}{
		"success":    true,
		"confidence": conf,
	}, nil
}

// ListEvidence returns all evidence rows for an artifact, newest first.
// Returns the rows under the "evidence" key — same shape as the
// previous callListEvidence.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) ListEvidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	rows, err := dm.QueryTracked(`
		SELECT id, artifact_id, artifact_type, type, source_group, strength,
		       independence_factor, created_by, created_at, expires_at, notes
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY created_at DESC
	`, artifactID, artifactType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, aid, atype, t, src, by string
		var notes sql.NullString
		var strength, ind float64
		var createdAt int64
		var expiresAt *int64
		if err := rows.Scan(&id, &aid, &atype, &t, &src, &strength, &ind, &by, &createdAt, &expiresAt, &notes); err != nil {
			return nil, err
		}
		row := map[string]interface{}{
			"id": id, "artifact_id": aid, "artifact_type": atype,
			"type": t, "source_group": src, "strength": strength,
			"independence_factor": ind, "created_by": by,
			"created_at": createdAt,
		}
		if notes.Valid {
			row["notes"] = notes.String
		} else {
			row["notes"] = ""
		}
		if expiresAt != nil {
			row["expires_at"] = *expiresAt
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]interface{}{"evidence": out}, nil
}

// QueryConfidenceHistory returns the confidence timeline for an artifact,
// newest first. limit <= 0 defaults to 50. Thin shim — both
// `mpm call query_confidence_history` and the `query_confidence_history`
// MCP tool route through this method.
//
// Returns the rows under the "history" key — same shape as the
// previous callQueryConfidenceHistory.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) QueryConfidenceHistory(artifactID, artifactType string, limit int) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := dm.QueryTracked(`
		SELECT computed_at, confidence, evidence_count, trigger
		FROM confidence_history
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY computed_at DESC
		LIMIT ?
	`, artifactID, artifactType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var computedAt int64
		var conf float64
		var evidenceCount int
		var trigger string
		if err := rows.Scan(&computedAt, &conf, &evidenceCount, &trigger); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"computed_at": computedAt, "confidence": conf,
			"evidence_count": evidenceCount, "trigger": trigger,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]interface{}{"history": out}, nil
}

// QueryConfidenceChanges returns recent confidence-altering events with
// delta and trigger. Wraps internal.QueryConfidenceChanges. Thin shim — both
// `mpm call query_confidence_changes` and the `query_confidence_changes`
// MCP tool route through this method.
//
// Returns: {"changes": [...], "count": N} — same shape as the previous
// callQueryConfidenceChanges.
func (dm *DatabaseManager) QueryConfidenceChanges(filter ConfidenceChangesFilter) (map[string]interface{}, error) {
	changes, err := QueryConfidenceChanges(dm, filter)
	if err != nil {
		return nil, fmt.Errorf("query confidence changes: %w", err)
	}
	return map[string]interface{}{
		"changes": changes,
		"count":   len(changes),
	}, nil
}

// QueryConfidenceTrend returns the trajectory projection of confidence
// over a time window. windowDays <= 0 defaults to 30.
//
// Both `mpm call query_confidence_trend` and the `query_confidence_trend`
// MCP tool route through this method, so neither surface can drift in
// the window-day defaulting.
//
// Returns: {"success": true, "trend": <ConfidenceTrend>}.
//
// Required: artifact_id. Optional: artifact_type (default "memory"),
// window_days (default 30).
func (dm *DatabaseManager) QueryConfidenceTrend(artifactID, artifactType string, windowDays int) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if windowDays <= 0 {
		windowDays = 30
	}
	trend, err := QueryConfidenceTrend(dm, artifactID, artifactType, windowDays)
	if err != nil {
		return nil, fmt.Errorf("query confidence trend: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"trend":   trend,
	}, nil
}

// QueryMemoryQuality returns per-creator memory statistics. Surfaces
// which models/agents produce memories that survive.
//
// Both `mpm call query_memory_quality` and the `query_memory_quality`
// MCP tool route through this method, so neither surface can drift in
// the wire format or bypass the underlying QueryMemoryQualityBySource
// pipeline that joins memories → auto_capture evidence → confidence_history.
//
// Returns: {"success": true, "sources": [...], "count": N}.
func (dm *DatabaseManager) QueryMemoryQuality() (map[string]interface{}, error) {
	stats, err := QueryMemoryQualityBySource(dm)
	if err != nil {
		return nil, fmt.Errorf("query memory quality: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"sources": stats,
		"count":   len(stats),
	}, nil
}

// ShowConfidence returns the current confidence and history for an artifact.
//
// The result shape is {"current": <float>, "history": {"history": [...]}} —
// the nested "history" map is the result of QueryConfidenceHistory, which
// is itself wrapped in a {"history": rows} map. This double-nesting is
// load-bearing: existing CLI callers and tests parse `result.history.history`.
// Do not flatten it.
//
// Both `mpm call show_confidence` and the `show_confidence` MCP tool route
// through this method, so neither surface can drift in the required-arg
// check, the artifact-type default, or the nested shape contract.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) ShowConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, ArtifactTable(artifactType)),
		artifactID,
	).Scan(&conf); err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}
	hist, err := dm.QueryConfidenceHistory(artifactID, artifactType, 50)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"current": conf,
		"history": hist,
	}, nil
}

// RecomputeConfidence forces a manual confidence recompute for an
// artifact and returns the new snapshot. Returns the same shape as
// dm.ShowConfidence so callers can read `result.current` and
// `result.history.history` consistently.
//
// Both `mpm call recompute_confidence` and the `recompute_confidence`
// MCP tool route through this method, so neither surface can drift
// in the required-arg check, the artifact-type default, or the
// recompute-then-snapshot ordering.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) RecomputeConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if err := RecomputeConfidence(dm, artifactID, artifactType, RecomputeReasonManual); err != nil {
		return nil, fmt.Errorf("recompute: %w", err)
	}
	return dm.ShowConfidence(artifactID, artifactType)
}

// ExplainConfidence returns the reasoning trace for an artifact's
// confidence: the full component breakdown of f(evidence, decay).
// Distinct from query_confidence_history (audit trail) — this answers
// "why did I get this number?"
//
// Both `mpm call explain_confidence` and the `explain_confidence` MCP tool
// route through this method, so neither surface can drift in the
// required-arg check, the artifact-type default, or the wrap shape.
//
// Returns: {"success": true, "explanation": <ConfidenceExplanation>}
// (the explanation is exposed as a generic map matching its JSON shape
// so callers can read `result.explanation.artifact_id` directly).
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) ExplainConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	exp, err := ExplainConfidence(dm, artifactID, artifactType)
	if err != nil {
		return nil, fmt.Errorf("explain confidence: %w", err)
	}
	// Expose the explanation as a generic map (its JSON shape) so both
	// surfaces can index into it by field name without needing to import
	// the internal type. JSON round-trip is the cheapest, drift-proof way
	// to keep the wire shape authoritative.
	expJSON, err := json.Marshal(exp)
	if err != nil {
		return nil, fmt.Errorf("encode explanation: %w", err)
	}
	var expMap map[string]interface{}
	if err := json.Unmarshal(expJSON, &expMap); err != nil {
		return nil, fmt.Errorf("decode explanation: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"explanation": expMap,
	}, nil
}