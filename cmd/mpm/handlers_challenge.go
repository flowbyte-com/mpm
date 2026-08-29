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

	// F-C3: refuse empty evidence. The hostile test surfaced that
	// `mpm challenge <id> ""` silently produced a "challenge" with
	// zero rationale, leaving the audit trail without any explanation
	// for the weight demotion. Evidence is the load-bearing
	// justification for the challenge; an empty string is operationally
	// equivalent to "no reason given", which is rejected.
	if strings.TrimSpace(evidence) == "" {
		return respond("", "Error: challenge requires non-empty evidence\n  Usage: mpm challenge <id> \"<evidence>\"\n", 1)
	}

	// F-C4: refuse to challenge an already-challenged memory. The
	// hostile test surfaced that re-challenging a memory that was
	// already in status="challenged" stacked weight demotions (-2 per
	// call) and left the prior challenged_theory_id overwritten in
	// metadata, destroying the audit trail. A second challenge against
	// the same memory must either be: (a) treated as a no-op with a
	// clear message, or (b) explicitly supersede the prior challenge.
	// We choose (a) — silent re-challenge was the bug; an explicit
	// "restore then re-challenge" workflow gives the operator a
	// moment to reconsider.
	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}
	if metaStr, ok := mem["metadata"].(string); ok && metaStr != "" {
		var status map[string]interface{}
		if err := json.Unmarshal([]byte(metaStr), &status); err == nil {
			if s, _ := status["status"].(string); s == "challenged" {
				return respond("", fmt.Sprintf("Memory %s is already challenged. Restore it first with `mpm challenge restore %s` if you want to re-challenge.\n", id, id), 1)
			}
		}
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

	// 1b. Weaken the memory's weight by 2 (same as ChallengeMemory).
	//     The prior weight is already captured in metadata at step 1.
	if _, err := tx.Exec(
		`UPDATE memories SET weight = MAX(1, weight - 2), updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL`,
		id,
	); err != nil {
		return respond("", fmt.Sprintf("Error weakening memory: %v\n", err), 1)
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
// F7-1 (alpha-final): this is now a thin wrapper around the canonical
// DatabaseManager.RestoreMemoryFromChallenge method, so the CLI, mpm
// call, and MCP surfaces all share one implementation. The detailed
// invariant commentary lives in internal/core/challenge_restore.go.
func runChallengeRestore(dm internal.CoreDB, id string) int {
	res, err := dm.RestoreMemoryFromChallenge(id)
	if err != nil {
		return respond("", fmt.Sprintf("%v\n", err), 1)
	}
	theoryID, _ := res["theory_id"].(string)
	fmt.Printf("✓ Theory %s resolved as disproven. Memory %s cleared.\n", theoryID, id)
	return 0
}
