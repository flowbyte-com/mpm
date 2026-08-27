// arbitration.go — Arc 1 closure: operator-driven resolution path.
//
// Split out of contradiction_log.go (F-009). Owns the "human
// arbitrates a close call, then we apply the verdict" path:
//
//   - ResolveArbitrationTheory     closes the loop on a PendingTheory
//                                  created by applyArbitration; the
//                                  operator names the winner, the
//                                  loser is implied by the theory's
//                                  dependencies, and the system
//                                  applies the slash.
//   - applyArbitrationResolution   the operator-driven counterpart to
//                                  applyResolution — same shape, same
//                                  transaction guarantee, but the
//                                  verdict comes from the operator
//                                  rather than provenance scoring.
//   - loadProvenanceInTx           transaction-aware provenance read,
//                                  used by applyArbitrationResolution
//                                  so the reads and writes see a
//                                  consistent view.
//   - UpdateSharedMemoryMetadata   shared-DB counterpart to
//                                  UpdateMemoryMetadata. Needed
//                                  because the arbitration theory
//                                  lives in shared.memories, not the
//                                  local memories table.
//   - ReinforceSharedMemory        shared-DB counterpart to
//                                  ReinforceMemory. Same reasoning.
//
// Queue CRUD itself lives in contradiction_log.go; the algorithmic
// resolution path (auto-decide) lives in resolve.go. The three
// files together implement the Arc 1 resolution loop.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// ResolveArbitrationTheory closes the loop on the close-call path.
// When the operator resolves a PendingTheory that was created by
// applyArbitration (i.e., a contradiction whose margin was below the
// threshold), this method:
//
//  1. Verifies the theory is an arbitration theory (has
//     metadata.arbitration_for_queue_id).
//  2. Reads dependencies (the two memory IDs from the original
//     pair).
//  3. Verifies winnerID is one of the two — the operator's call
//     is explicit; the OTHER dependency is the loser.
//  4. Applies the slash+resolution memory+queue-mark-resolved
//     dance atomically (single transaction; crash safety same as
//     the original applyResolution).
//  5. Resolves the theory itself (status='resolved', conclusion,
//     resolved_at), which the existing theory lifecycle handles.
//
// Returns the theory resolution result with extra fields describing
// the auto-slash side-effect (queue_id, loser_id, resolution_id)
// for the operator's audit.
//
// If the theory is NOT an arbitration theory, this method returns
// an error — the operator should use the plain ResolveTheory path
// instead. (Passing a non-arbitration theory through here would
// be a programmer error, not a normal flow.)
func (dm *DatabaseManager) ResolveArbitrationTheory(theoryID, winnerID, conclusion string) (map[string]interface{}, error) {
	if dm.db == nil {
		return nil, fmt.Errorf("ResolveArbitrationTheory: dm.db is nil")
	}
	if theoryID == "" || winnerID == "" {
		return nil, fmt.Errorf("ResolveArbitrationTheory: theoryID and winnerID are required")
	}
	// Arbitration theories live in the shared substrate (shared.memories).
	// Refuse with a clear message if the shared DB isn't attached, rather
	// than letting the QueryRow below surface a cryptic "no such table:
	// shared.memories" error. The shared DB is opt-in via MPM_SHARED_DB;
	// single-instance users never see this surface.
	if dm.SharedAttached() == "" {
		return nil, fmt.Errorf("ResolveArbitrationTheory: shared DB not attached (set MPM_SHARED_DB); arbitration theories require multi-agent shared epistemology mode")
	}

	// Read the theory. Must be collection='theories' and have
	// arbitration metadata. The metadata is a JSON column; we use
	// json_extract to pull the queue ID.
	var (
		coll     string
		depsJSON sql.NullString
		queueIDV sql.NullInt64
	)
	err := dm.db.QueryRow(`
		SELECT collection, dependencies, json_extract(metadata, '$.arbitration_for_queue_id')
		FROM shared.memories
		WHERE id = ? AND deleted_at IS NULL
	`, theoryID).Scan(&coll, &depsJSON, &queueIDV)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("ResolveArbitrationTheory: theory %s not found", theoryID)
	}
	if err != nil {
		return nil, fmt.Errorf("ResolveArbitrationTheory: read theory: %w", err)
	}
	if coll != "theories" {
		return nil, fmt.Errorf("ResolveArbitrationTheory: memory %s is not a theory (collection: %s)", theoryID, coll)
	}
	if !queueIDV.Valid {
		return nil, fmt.Errorf("ResolveArbitrationTheory: theory %s is not an arbitration theory (no arbitration_for_queue_id in metadata)", theoryID)
	}

	// Parse dependencies (JSON array of two memory IDs).
	if !depsJSON.Valid || depsJSON.String == "" {
		return nil, fmt.Errorf("ResolveArbitrationTheory: theory %s has no dependencies; cannot determine loser", theoryID)
	}
	var deps []string
	if err := json.Unmarshal([]byte(depsJSON.String), &deps); err != nil {
		return nil, fmt.Errorf("ResolveArbitrationTheory: parse dependencies: %w", err)
	}
	if len(deps) != 2 {
		return nil, fmt.Errorf("ResolveArbitrationTheory: arbitration theory should have exactly 2 dependencies, got %d", len(deps))
	}

	// Verify winnerID is in the dependency pair. Refuse if not —
	// the operator's call must reference a memory that was
	// actually part of the original contradiction.
	loserID := ""
	if deps[0] == winnerID {
		loserID = deps[1]
	} else if deps[1] == winnerID {
		loserID = deps[0]
	} else {
		return nil, fmt.Errorf("ResolveArbitrationTheory: winner %s is not in the theory's dependencies %v", winnerID, deps)
	}

	// Apply the slash+resolution+queue-mark in a single transaction.
	// This is the same shape as applyResolution, but driven by
	// explicit operator input (winnerID) rather than provenance
	// scoring. The reasoning for the verdict goes in the conclusion.
	resolutionResult, err := dm.applyArbitrationResolution(theoryID, queueIDV.Int64, winnerID, loserID, conclusion)
	if err != nil {
		return nil, fmt.Errorf("ResolveArbitrationTheory: apply resolution: %w", err)
	}

	// Now mark the theory itself as resolved. We do this inline
	// (rather than calling dm.ResolveTheory) because the existing
	// ResolveTheory queries the local memories table, not shared.
	// The arbitration theory lives in shared.memories. The
	// metadata patch + ReinforceMemory sequence mirrors
	// ResolveTheory's semantics, scoped to shared.memories.
	now := time.Now().UTC().Format(time.RFC3339)
	theoryPatch := map[string]interface{}{
		"status":         "resolved",
		"conclusion":     conclusion,
		"resolved_at":    now,
		"winner_id":      winnerID,
		"loser_id":       loserID,
		"queue_id":       queueIDV.Int64,
		"resolution_id":  resolutionResult["resolution_id"],
	}
	theoryPatchJSON, _ := json.Marshal(theoryPatch)
	if err := dm.UpdateSharedMemoryMetadata(theoryID, string(theoryPatchJSON)); err != nil {
		return nil, fmt.Errorf("ResolveArbitrationTheory: update theory metadata: %w", err)
	}
	if err := dm.ReinforceSharedMemory(theoryID, 1); err != nil {
		// Reinforce failure is non-fatal (theory is already
		// marked resolved); log and continue.
		_ = err
	}

	// Arc 2: operator-driven arbitration resolutions are also
	// epistemic events. Auto-broadcast the resolution memory
	// (NOT the theory itself) so receiving agents see the
	// concrete "X survives, Y is slashed" signal. Non-fatal:
	// a broadcast failure does NOT roll back the resolution.
	if resID, ok := resolutionResult["resolution_id"].(string); ok && resID != "" {
		_, _ = dm.BroadcastMemory(resID, BroadcastOpts{
			Kind:        "arbitration",
			SourceAgent: "arc1-arbitrator",
		})
	}

	// Merge the results.
	out := map[string]interface{}{
		"success":               true,
		"id":                    theoryID,
		"status":                "resolved",
		"conclusion":            conclusion,
		"queue_id":              queueIDV.Int64,
		"winner_id":             winnerID,
		"loser_id":              loserID,
		"resolution_memory_id":  resolutionResult["resolution_id"],
		"slash_amount":          resolutionResult["slash_amount"],
		"evidence_id":           resolutionResult["evidence_id"],
	}
	return out, nil
}

// applyArbitrationResolution is the operator-driven counterpart to
// applyResolution. The difference: instead of computing the verdict
// from provenance scores, the verdict is the operator's explicit
// winnerID. The mechanics (slash, resolution memory, evidence, queue
// mark-resolved) are identical to applyResolution — same shape, same
// transaction guarantee, same evidence trail.
//
// Returns a map with the resolution_id, slash_amount, and evidence_id
// for the caller's audit.
func (dm *DatabaseManager) applyArbitrationResolution(theoryID string, queueID int64, winnerID, loserID, conclusion string) (map[string]interface{}, error) {
	// Defensive: shared must be attached — the caller (ResolveArbitrationTheory)
	// checks this, but applyArbitrationResolution is exported via ApplyArbitration
	// in contradiction_log.go too. Refuse early rather than failing deep in the
	// transaction with a confusing "no such table: shared.memories" error.
	if dm.SharedAttached() == "" {
		return nil, fmt.Errorf("applyArbitrationResolution: shared DB not attached (set MPM_SHARED_DB)")
	}
	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: begin: %w", err)
	}
	defer tx.Rollback()

	// Compute provenance for both sides. The slash amount is
	// derived from the loser's confidence (same formula as
	// applyResolution), so the visible outcome is consistent
	// with what the algorithm would have done if the margin
	// had been decisive. The only thing that's different is
	// WHO decided — a human, not the score.
	loserScore, err := dm.loadProvenanceInTx(tx, loserID)
	if err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: load loser provenance: %w", err)
	}
	winnerScore, err := dm.loadProvenanceInTx(tx, winnerID)
	if err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: load winner provenance: %w", err)
	}
	margin := winnerScore.Score - loserScore.Score
	if margin < 0 {
		margin = -margin
	}
	slashAmount := int(math.Round(loserScore.Confidence * SlashFraction * 100))

	// Write the resolution memory. Same shape as applyResolution.
	resolutionID := fmt.Sprintf("resolution-%s-%d", loserID, time.Now().UnixNano())
	content := fmt.Sprintf(
		"Resolution: memory_%s (loser, confidence=%.3f) slashed by %d weight units "+
			"in favor of memory_%s (winner, confidence=%.3f) by operator arbitration. "+
			"Conclusion: %s. See shared.contradiction_log id=%d, arbitration theory %s.",
		loserID, loserScore.Confidence, slashAmount,
		winnerID, winnerScore.Confidence,
		conclusion, queueID, theoryID,
	)
	tagsJSON := fmt.Sprintf(`["%s"]`, fmt.Sprintf("resolution:contradiction:%s-%s", loserID, winnerID))
	_, err = tx.Exec(`
		INSERT INTO shared.memories
			(id, collection, content, tags, weight, retrieval_priority, importance, confidence, metadata)
		VALUES (?, 'resolutions', ?, ?, 1.0, 0.7, 0.7, 0.9, ?)
	`, resolutionID, content, tagsJSON, fmt.Sprintf(`{"resolution_for_queue_id":%d,"winner":%q,"loser":%q,"margin":%f,"arbitration_theory":%q,"operator_conclusion":%q}`, queueID, winnerID, loserID, margin, theoryID, conclusion))
	if err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: write resolution memory: %w", err)
	}

	// Slash the loser. Same UPDATE shape as applyResolution.
	var exists bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM shared.memories WHERE id = ? AND deleted_at IS NULL)`, loserID).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: loser check: %w", err)
	}
	if exists {
		// Same JSON-merge approach as applyResolution: read the
		// existing metadata, merge in the patch, write back as a
		// single JSON object. String concat (`||`) would produce
		// invalid JSON.
		var existingMetaRaw sql.NullString
		if err := tx.QueryRow(`SELECT metadata FROM shared.memories WHERE id = ?`, loserID).Scan(&existingMetaRaw); err != nil {
			return nil, fmt.Errorf("applyArbitrationResolution: read loser metadata: %w", err)
		}
		loserPatch := map[string]interface{}{
			"status":                  "challenged",
			"challenged_by_resolution": resolutionID,
			"challenged_at":           time.Now().UTC().Format(time.RFC3339),
			"challenged_via":          "operator_arbitration",
			"arbitration_theory":      theoryID,
		}
		merged := map[string]interface{}{}
		if existingMetaRaw.Valid && existingMetaRaw.String != "" && existingMetaRaw.String != "{}" {
			if err := json.Unmarshal([]byte(existingMetaRaw.String), &merged); err != nil {
				return nil, fmt.Errorf("applyArbitrationResolution: parse existing loser metadata: %w", err)
			}
		}
		for k, v := range loserPatch {
			merged[k] = v
		}
		mergedJSON, _ := json.Marshal(merged)
		_, err = tx.Exec(`
			UPDATE shared.memories
			SET metadata = ?,
			    weight = MAX(1, weight - ?),
			    retrieval_priority = MAX(0.01, retrieval_priority - ?),
			    updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ? AND deleted_at IS NULL
		`, string(mergedJSON), slashAmount, float64(slashAmount)/100.0, loserID)
		if err != nil {
			return nil, fmt.Errorf("applyArbitrationResolution: slash loser: %w", err)
		}
	}

	// Mark the queue row resolved.
	_, err = tx.Exec(`
		UPDATE shared.contradiction_log
		SET resolved_at = CURRENT_TIMESTAMP,
		    resolution_memory_id = ?
		WHERE id = ? AND resolved_at IS NULL
	`, resolutionID, queueID)
	if err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: mark resolved: %w", err)
	}

	// Write the resolution as evidence FOR the winner (same as
	// applyResolution). Strength 1.0 — the operator's call is
	// the strongest possible signal.
	evidenceID := fmt.Sprintf("ev-resolution-%s-%d", winnerID, time.Now().UnixNano())
	now := time.Now().Unix()
	notes := fmt.Sprintf("Survived contradiction against %s via operator arbitration (queue_id=%d, theory_id=%s, conclusion=%q, resolution_id=%s).",
		loserID, queueID, theoryID, conclusion, resolutionID)
	_, err = tx.Exec(`
		INSERT INTO shared.evidence
			(id, artifact_id, artifact_type, type, source_group, strength, independence_factor, created_by, created_at, notes)
		VALUES (?, ?, 'memory', 'resolution_survived', ?, 1.0, 1.0, 'arc1-resolution-system', ?, ?)
	`, evidenceID, winnerID, fmt.Sprintf("resolution:%d", queueID), now, notes)
	if err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: write evidence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("applyArbitrationResolution: commit: %w", err)
	}

	return map[string]interface{}{
		"resolution_id": resolutionID,
		"slash_amount":  slashAmount,
		"evidence_id":   evidenceID,
		"margin":        margin,
	}, nil
}

// loadProvenanceInTx is a transaction-aware version of
// LoadMemoryProvenance. Used by applyArbitrationResolution so the
// provenance reads happen inside the same transaction as the
// resolution writes. Without this, a concurrent write to one of
// the memories between the read and the write could see different
// confidence values than what the slash was based on.
func (dm *DatabaseManager) loadProvenanceInTx(tx *sql.Tx, memoryID string) (ProvenanceScore, error) {
	ps := ProvenanceScore{MemoryID: memoryID}
	var (
		confidence   sql.NullFloat64
		updatedAt    sql.NullString
		retrievalPri sql.NullFloat64
	)
	err := tx.QueryRow(`
		SELECT
			COALESCE(confidence, 0.5),
			COALESCE(updated_at, CURRENT_TIMESTAMP),
			COALESCE(retrieval_priority, 0.5)
		FROM shared.memories
		WHERE id = ? AND deleted_at IS NULL
	`, memoryID).Scan(&confidence, &updatedAt, &retrievalPri)
	if err == sql.ErrNoRows {
		// Missing memory: zero provenance. Caller decides the
		// verdict logic.
		return ps, nil
	}
	if err != nil {
		return ps, fmt.Errorf("loadProvenanceInTx: %w", err)
	}
	ps.Confidence = confidence.Float64
	if t, perr := time.Parse(time.RFC3339, updatedAt.String); perr == nil {
		ps.LastReinforced = t
		ps.AgeDays = time.Since(t).Hours() / 24.0
	}
	var evCount int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM shared.evidence
		WHERE artifact_id = ? AND artifact_type = 'memory'
	`, memoryID).Scan(&evCount); err != nil {
		evCount = 0
	}
	ps.Reinforcement = math.Min(float64(evCount)/10.0, 1.0)
	ps.Score = 0.5*ps.Confidence + 0.3*(1.0/(1.0+ps.AgeDays)) + 0.2*ps.Reinforcement
	return ps, nil
}

// UpdateSharedMemoryMetadata is the shared-DB counterpart to
// UpdateMemoryMetadata. Mirrors the same JSON-patch semantics but
// operates on shared.memories. Used by ResolveArbitrationTheory to
// mark the arbitration theory as resolved; the existing
// UpdateMemoryMetadata only sees local memories.
//
// Implementation note: we DO NOT use string concatenation (`||`)
// on the JSON columns. SQLite treats `||` as plain string concat,
// which produces two adjacent JSON objects (invalid JSON for
// json_extract). Instead, we parse the patch into a map, fetch
// the existing metadata, deep-merge in Go, and write back the
// merged result. The merge rule: if the patch has a key that
// exists in the existing metadata, the patch value wins. This
// matches the existing UpdateMemoryMetadata's "last write wins"
// behavior at the JSON-object level.
func (dm *DatabaseManager) UpdateSharedMemoryMetadata(id string, patchJSON string) error {
	if !json.Valid([]byte(patchJSON)) {
		return fmt.Errorf("UpdateSharedMemoryMetadata: invalid JSON patch: %s", patchJSON)
	}
	var patch map[string]interface{}
	if err := json.Unmarshal([]byte(patchJSON), &patch); err != nil {
		return fmt.Errorf("UpdateSharedMemoryMetadata: parse patch: %w", err)
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("UpdateSharedMemoryMetadata: begin: %w", err)
	}
	defer tx.Rollback()

	// Read existing metadata.
	var existingRaw sql.NullString
	err = tx.QueryRow(`SELECT metadata FROM shared.memories WHERE id = ? AND deleted_at IS NULL`, id).Scan(&existingRaw)
	if err == sql.ErrNoRows {
		return fmt.Errorf("UpdateSharedMemoryMetadata: memory %s not found", id)
	}
	if err != nil {
		return fmt.Errorf("UpdateSharedMemoryMetadata: read: %w", err)
	}
	merged := map[string]interface{}{}
	if existingRaw.Valid && existingRaw.String != "" && existingRaw.String != "{}" {
		if err := json.Unmarshal([]byte(existingRaw.String), &merged); err != nil {
			return fmt.Errorf("UpdateSharedMemoryMetadata: parse existing: %w", err)
		}
	}
	for k, v := range patch {
		merged[k] = v
	}
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("UpdateSharedMemoryMetadata: marshal merged: %w", err)
	}

	_, err = tx.Exec(`
		UPDATE shared.memories
		SET metadata = ?,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND deleted_at IS NULL
	`, string(mergedJSON), id)
	if err != nil {
		return fmt.Errorf("UpdateSharedMemoryMetadata: update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("UpdateSharedMemoryMetadata: commit: %w", err)
	}
	return nil
}

// ReinforceSharedMemory is the shared-DB counterpart to
// ReinforceMemory. Increments weight and updates last_accessed_at
// in shared.memories. Failure is non-fatal (we log and continue) so
// that the resolution flow doesn't get blocked by a side-effect.
func (dm *DatabaseManager) ReinforceSharedMemory(id string, delta int) error {
	if delta <= 0 {
		delta = 1
	}
	weightGain := (delta + 1) / 2
	if weightGain == 0 {
		weightGain = 1
	}
	_, err := dm.db.Exec(`
		UPDATE shared.memories
		SET reinforcement_count = COALESCE(reinforcement_count, 0) + ?,
		    weight = COALESCE(weight, 1) + ?,
		    last_accessed_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND deleted_at IS NULL
	`, delta, weightGain, id)
	if err != nil {
		return fmt.Errorf("ReinforceSharedMemory: %w", err)
	}
	return nil
}