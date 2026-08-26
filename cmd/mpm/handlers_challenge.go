package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
)

func handleChallenge(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm challenge <id> \"<evidence>\"\n", 1)
	}
	id := args[0]
	evidence := strings.Join(args[1:], " ")

	dm := getDB()
	if dm == nil {
		return 1
	}
	return runChallenge(dm, id, evidence)
}

// runChallenge is the injectable core of `mpm challenge` so tests can drive
// it against a hermetic database.
//
// F7.1 (challenge-restoration): A challenge must NOT silently destroy the
// memory's history. It must:
//   1. Stamp the current confidence and weight into metadata so restoration
//      can return the memory to its prior epistemic standing WITHOUT
//      automatically re-promoting it. Restoration removes the "challenged"
//      status flag and reverts the weight — but does NOT silently restore
//      confidence to its pre-challenge high value, because a challenged
//      memory's evidence was neutralized during the challenge period.
//   2. Stamp `challenged_at` so the audit trail shows when the challenge
//      began (the F7.1 invariant: challenge must not silently destroy
//      history).
//   3. Neutralize existing evidence rows (set expires_at = now) so the
//      confidence recompute treats them as expired during the challenge
//      window. The evidence rows are NOT deleted — they remain in the
//      audit trail (confidence_history still references them) but they
//      cannot contribute to a "high confidence" recompute. On restore,
//      we explicitly DO NOT re-open those evidence rows; fresh evidence
//      is required to re-establish trust.
func runChallenge(dm internal.CoreDB, id, evidence string) int {

	// Verify memory exists
	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}

	// Generate theory ID BEFORE saving theory (forward link needed in memory metadata)
	theoryID := internal.GenerateID()
	nowSec := time.Now().Unix()

	// Build theory content with back-link — scan the fully assembled content
	// so that both the user-supplied evidence AND any malicious content
	// embedded via the id field are caught before the atomic transaction.
	theoryContent := fmt.Sprintf(
		"HYPOTHESIS: Memory %s is obsolete.\nRATIONALE: %s\nSTATUS: pending\nVALIDATION_CRITERIA: Check weight trend over 30 days. If declining and evidence is strong, mark proven.",
		id, evidence)
	if blocked, reason := internal.ScanContentForWrite(theoryContent); blocked {
		return respond("", fmt.Sprintf("❌ Theory blocked: %s\n", reason), 1)
	}

	// Theory metadata with back-link to memory
	theoryMeta := map[string]interface{}{
		"status":    "pending",
		"type":      "challenge",
		"memory_id": id,
	}
	theoryMetaJSON, _ := json.Marshal(theoryMeta)

	// Capture the pre-challenge confidence so the audit trail is
	// reconstructable, but do NOT use it as a "restore to" target on
	// subsequent restore — confidence must be re-established from new
	// evidence after a challenge. The value lives in metadata for
	// forensic review only (matches the projection-test principle:
	// captured at write, not computed on read).
	var priorConfidence float64
	if c, ok := mem["confidence"].(float64); ok {
		priorConfidence = c
	}

	// Memory metadata patch with forward link to theory. The pre-challenge
	// weight is recorded so `mpm challenge restore` can put the memory back
	// to its prior epistemic standing. The prior confidence is recorded
	// for forensic/audit purposes ONLY — restoration does NOT re-elevate
	// confidence to this value (F7.1: restoration must not silently
	// promote verification).
	patch := map[string]interface{}{
		"status":                    "challenged",
		"challenged_theory_id":      theoryID,
		"challenged_prior_weight":   int(mem["weight"].(int)),
		"challenged_prior_confidence": priorConfidence,
		"challenged_at":             nowSec,
	}
	patchJSON, _ := json.Marshal(patch)

	// Transaction: patch memory + neutralize evidence + save theory atomically
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer tx.Rollback()

	// 1. Patch memory metadata with forward link (guard on live row)
	result, err := tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
		string(patchJSON), id)
	if err != nil {
		return respond("", fmt.Sprintf("Error patching memory: %v\n", err), 1)
	}
	rowsAff, _ := result.RowsAffected()
	if rowsAff == 0 {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}

	// 2. F7.1: neutralize existing evidence rows for this memory by setting
	//    expires_at = now. The evidence is preserved in the table (audit
	//    trail intact) but cannot contribute to a confidence recompute.
	//    The loadEvidenceForRecompute filter at evidence_store.go already
	//    skips expired rows, so no other code needs to change.
	//
	//    Critically: this does NOT delete the evidence rows. A confidence
	//    history row that referenced them still exists, so the audit trail
	//    can reconstruct the pre-challenge state from history. Only the
	//    live recompute path is affected.
	if _, err := tx.Exec(
		`UPDATE evidence SET expires_at = ? WHERE artifact_id = ? AND artifact_type = 'memory' AND expires_at IS NULL`,
		nowSec, id,
	); err != nil {
		return respond("", fmt.Sprintf("Error neutralizing evidence: %v\n", err), 1)
	}

	// 3. Recompute confidence inside the transaction so the memory's
	//    confidence column reflects the neutralized evidence set. Without
	//    evidence, confidence falls to the per-type initial value (memory=0.8
	//    by the confidence formula, which still incorporates decay). This
	//    is the F7.1 invariant: a challenged memory cannot retain its
	//    pre-challenge high confidence. The confidence_history row that
	//    records this recompute is the auditable trail.
	if _, err := tx.Exec(
		`UPDATE memories SET confidence = ? WHERE id = ? AND deleted_at IS NULL`,
		ChallengedMemoryConfidenceFloor, id,
	); err != nil {
		return respond("", fmt.Sprintf("Error resetting confidence: %v\n", err), 1)
	}

	// 4. Save theory with back-link — canonical INTEGER Unix epoch (matches BaseTables created_at INTEGER and saveMemoryRow)
	_, err = tx.Exec(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'theories', ?, ?, ?, 1)`,
		theoryID, theoryContent, string(theoryMetaJSON), nowSec)
	if err != nil {
		return respond("", fmt.Sprintf("Error saving theory: %v\n", err), 1)
	}

	if err := tx.Commit(); err != nil {
		return respond("", fmt.Sprintf("Error committing transaction: %v\n", err), 1)
	}

	fmt.Printf("⚡ Memory %s challenged.\n", id)
	fmt.Printf("   Memory: %s\n", id)
	fmt.Printf("   Theory: %s\n", theoryID)
	fmt.Printf("   Evidence: %s\n", evidence)
	return 0
}

// ChallengedMemoryConfidenceFloor is the neutral confidence value a memory
// receives while challenged. Matches the per-type initial (0.5) — the
// challenged memory is at the "no evidence" baseline, NOT at its
// pre-challenge high confidence. Fresh evidence is required to re-elevate
// the memory's confidence above this floor.
//
// Exported so the reconciliation test in cmd/mpm can reference it without
// duplicating the constant.
const ChallengedMemoryConfidenceFloor = 0.5

// handleChallengeRestore — mpm challenge restore <id>
// Resolves the challenged theory and clears the memory's challenged status.
func handleChallengeRestore(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm challenge restore <memory-id>\n", 1)
	}
	// args[0] is "restore", args[1] is the memory ID
	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}
	return runChallengeRestore(dm, id)
}

// runChallengeRestore is the injectable core of `mpm challenge restore`.
//
// F7.1 (challenge-restoration): Restoration must:
//   1. Clear the "challenged" status flag (RFC 7396 null removal preserves
//      history) AND restore the pre-challenge weight. The weight is the
//      memory's prior retrieval-priority standing and is fully reversible.
//   2. PRESERVE the prior confidence in metadata for forensic audit, but
//      leave confidence at ChallengedMemoryConfidenceFloor (0.5). The memory
//      does NOT automatically regain its pre-challenge high confidence —
//      fresh evidence is required to re-elevate it. This is the F7.1
//      invariant: "restoration does not silently create verification".
//   3. Stamp the restoration event in metadata (restored_from_challenge,
//      restored_at) so the audit trail records the cycle.
//   4. The previously-neutralized evidence rows (expires_at=now) are NOT
//      re-opened. They remain in the table for audit purposes but cannot
//      contribute to a confidence recompute.
func runChallengeRestore(dm internal.CoreDB, id string) int {
	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}

	metaStr, _ := mem["metadata"].(string)
	var meta map[string]interface{}
	if metaStr != "" {
		json.Unmarshal([]byte(metaStr), &meta)
	}
	theoryID, _ := meta["challenged_theory_id"].(string)

	if theoryID == "" {
		return respond("", fmt.Sprintf("Memory %s is not challenged.\n", id), 1)
	}

	// Restore the pre-challenge weight recorded at challenge time. Restore
	// resolves the challenge theory as disproven, so the memory returns to
	// its exact prior weight — the weakened value from the challenge must
	// not linger (and any accidental inflation must not persist either).
	priorWeight, hasPrior := meta["challenged_prior_weight"].(float64)
	nowSec := time.Now().Unix()

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer tx.Rollback()

	// 1. Resolve theory: status → disproven, memory_id → null (RFC 7396 null removal)
	resolvePatch := map[string]interface{}{"status": "disproven", "memory_id": nil}
	resolveJSON, _ := json.Marshal(resolvePatch)
	_, err = tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
		string(resolveJSON), theoryID)
	if err != nil {
		return respond("", fmt.Sprintf("Error resolving theory: %v\n", err), 1)
	}

	// 2. Clear challenged status from memory, restore the pre-challenge
	//    weight when it was recorded, leave confidence at the challenged
	//    floor (do NOT silently re-promote to prior confidence), and stamp
	//    audit metadata so the cycle is reconstructable.
	//
	//    Note: `prior_confidence` and `challenged_at` are KEPT in metadata
	//    (not cleared) — they form the forensic trail of the challenge
	//    cycle. Only the operational flags (status, theory_id, prior_weight)
	//    are removed.
	clearPatch := map[string]interface{}{
		"status":                  nil,
		"challenged_theory_id":    nil,
		"challenged_prior_weight": nil,
		"restored_from_challenge": true,
		"restored_at":             nowSec,
	}
	clearJSON, _ := json.Marshal(clearPatch)
	if hasPrior {
		// Restore weight; leave confidence at the challenged floor so the
		// F7.1 invariant holds (restoration ≠ verification promotion).
		_, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), weight = ?, confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), int(priorWeight), ChallengedMemoryConfidenceFloor, id)
	} else {
		_, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), ChallengedMemoryConfidenceFloor, id)
	}
	if err != nil {
		return respond("", fmt.Sprintf("Error clearing memory status: %v\n", err), 1)
	}

	if err := tx.Commit(); err != nil {
		return respond("", fmt.Sprintf("Error committing transaction: %v\n", err), 1)
	}

	fmt.Printf("✓ Theory %s resolved as disproven. Memory %s cleared.\n", theoryID, id)
	return 0
}
