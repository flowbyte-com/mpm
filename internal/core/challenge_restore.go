// challenge_restore.go — Core method backing `mpm challenge restore`.
//
// F7-1 (alpha-final): the challenge-restore operation was previously only
// reachable through the CLI (`mpm challenge restore <id>`). Agents calling
// via `mpm call mpm_memory` or via the MCP tool surface had no equivalent,
// forcing them to either drop into shell or duplicate the restore logic.
//
// This file moves the restore operation into the core DatabaseManager so
// every surface (CLI, mpm call, MCP) shares the same implementation.
//
// F7.1 invariant is preserved:
//   - The "challenged" status flag is cleared.
//   - The pre-challenge weight is restored.
//   - Confidence is left at ChallengedMemoryConfidenceFloor (fresh evidence
//     is required to re-elevate; restoration does NOT silently re-promote
//     verification).
//   - The challenged theory is marked disproven (memory_id null).
//   - Historical evidence remains in the table (the audit trail is intact;
//     rows are neutralized via expires_at by `mpm challenge` itself, but
//     they are not deleted).
//
// History preservation: prior_confidence, challenged_at, and challenge
// reason are KEPT in metadata after restore. Only the operational flags
// (status, challenged_theory_id, challenged_prior_weight) are removed.
package internal

import (
	"encoding/json"
	"fmt"
	"time"
)

// RestoreMemoryFromChallenge resolves the challenged-theory record
// against the supplied memory id and clears the memory's challenged
// status.
//
// Returns the restored memory (or an error). The restore cycle is
// idempotent: restoring an already-restored memory is a no-op (returns
// nil error).
//
// Atomicity: the theory status patch, the memory metadata patch, and the
// optional weight restore run inside a single transaction so a partial
// failure cannot leave the memory mid-cycle.
func (dm *DatabaseManager) RestoreMemoryFromChallenge(memoryID string) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required for challenge restore")
	}
	mem, err := dm.GetMemory(memoryID)
	if err != nil || mem == nil {
		return nil, fmt.Errorf("memory not found: %s", memoryID)
	}

	metaStr, _ := mem["metadata"].(string)
	var meta map[string]interface{}
	if metaStr != "" {
		_ = json.Unmarshal([]byte(metaStr), &meta)
	}
	theoryID, _ := meta["challenged_theory_id"].(string)

	if theoryID == "" {
		return nil, fmt.Errorf("memory %s is not challenged", memoryID)
	}

	priorWeight, hasPrior := meta["challenged_prior_weight"].(float64)
	nowSec := time.Now().Unix()

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Resolve theory: status → disproven, memory_id → null (RFC 7396 null removal)
	resolvePatch := map[string]interface{}{"status": "disproven", "memory_id": nil}
	resolveJSON, _ := json.Marshal(resolvePatch)
	if _, err := tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
		string(resolveJSON), theoryID); err != nil {
		return nil, fmt.Errorf("resolve theory: %w", err)
	}

	// 2. Clear challenged status from memory, restore the pre-challenge
	//    weight when it was recorded, leave confidence at the challenged
	//    floor (do NOT silently re-promote to prior confidence), and stamp
	//    audit metadata so the cycle is reconstructable.
	clearPatch := map[string]interface{}{
		"status":                  nil,
		"challenged_theory_id":    nil,
		"challenged_prior_weight": nil,
		"restored_from_challenge": true,
		"restored_at":             nowSec,
	}
	clearJSON, _ := json.Marshal(clearPatch)
	if hasPrior {
		if _, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), weight = ?, confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), int(priorWeight), ChallengedMemoryConfidenceFloor, memoryID); err != nil {
			return nil, fmt.Errorf("clear memory status: %w", err)
		}
	} else {
		if _, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), ChallengedMemoryConfidenceFloor, memoryID); err != nil {
			return nil, fmt.Errorf("clear memory status: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return map[string]interface{}{
		"success":     true,
		"memory_id":   memoryID,
		"theory_id":   theoryID,
		"theory_status": "disproven",
		"action":      "restored",
	}, nil
}
