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
//  1. Stamp the current confidence and weight into metadata so restoration
//     can return the memory to its prior epistemic standing WITHOUT
//     automatically re-promoting it. Restoration removes the "challenged"
//     status flag and reverts the weight — but does NOT silently restore
//     confidence to its pre-challenge high value, because a challenged
//     memory's evidence was neutralized during the challenge period.
//  2. Stamp `challenged_at` so the audit trail shows when the challenge
//     began (the F7.1 invariant: challenge must not silently destroy
//     history).
//  3. Neutralize existing evidence rows (set expires_at = now) so the
//     confidence recompute treats them as expired during the challenge
//     window. The evidence rows are NOT deleted — they remain in the
//     audit trail (confidence_history still references them) but they
//     cannot contribute to a "high confidence" recompute. On restore,
//     we explicitly DO NOT re-open those evidence rows; fresh evidence
//     is required to re-establish trust.
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

	// Transaction: apply challenge + neutralize evidence + save theory
	// atomically. Everything below shares this one transaction, so a
	// failure anywhere leaves the memory exactly as it was.
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer tx.Rollback()

	// 1. Apply the challenge state. This is the SAME primitive the other
	//    three challenge writers use (issue #24), so `mpm challenge`
	//    produces exactly the recovery metadata they do and
	//    `mpm challenge restore` can undo any of them uniformly.
	//
	//    It records the pre-challenge weight and confidence, reduces the
	//    weight by 2, drops confidence to the challenged floor, and links
	//    the theory created below — in one statement, so the record of
	//    what was changed cannot diverge from what was changed.
	applied, err := internal.ApplyChallengeInTx(tx, internal.ChallengeTableLocal, internal.ChallengeSpec{
		MemoryID:       id,
		SlashAmount:    2,
		DropConfidence: true,
		ChallengedAt:   nowSec,
		Fields: map[string]interface{}{
			"status":               "challenged",
			"challenged_theory_id": theoryID,
			"challenged_via":       internal.ChallengeViaTheory,
		},
	})
	if err != nil {
		return respond("", fmt.Sprintf("Error challenging memory: %v\n", err), 1)
	}
	if applied.RowsAffected == 0 {
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

	// 3. Confidence is already at the challenged floor: step 1 dropped it
	//    inside the same statement that recorded the challenge, so the
	//    F7.1 invariant ("a challenged memory cannot retain its
	//    pre-challenge high confidence") cannot drift from the metadata
	//    that describes the challenge.

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
//
// Reporting (issue #11)
// ---------------------
// The success line used to be a fixed string asserting the theory was
// "resolved as disproven", printed regardless of what the database did.
// It now reflects the result map's observed outcome.
//
// The exit code stays 0 when the memory half succeeded and only the
// theory half had nothing to act on. A non-zero code would tell the
// operator nothing was restored, when in fact the memory's weight and
// status were restored — and the documented remedy for a stale theory
// reference is not "retry the restore". The distinction is carried by the
// output text and by the machine-readable `complete` / `theory_resolution`
// fields, which is where a script should be reading it from. Full failure
// (error from the core) still exits 1.
func runChallengeRestore(dm internal.CoreDB, id string) int {
	res, err := dm.RestoreMemoryFromChallenge(id)
	if err != nil {
		return respond("", fmt.Sprintf("%v\n", err), 1)
	}
	theoryID, _ := res["theory_id"].(string)
	resolution, _ := res["theory_resolution"].(string)
	resolved, _ := res["theory_resolved"].(bool)

	// Which columns the restore actually put back. Reported because a
	// memory slashed by `--apply` or by operator arbitration has a
	// different recovery footprint from one challenged by a theory, and
	// "cleared" alone does not say which.
	var restored []string
	if w, _ := res["weight_restored"].(bool); w {
		restored = append(restored, "weight")
	}
	if rp, _ := res["retrieval_priority_restored"].(bool); rp {
		restored = append(restored, "retrieval_priority")
	}

	if resolved {
		fmt.Printf("✓ Theory %s resolved as disproven. Memory %s cleared.\n", theoryID, id)
		if len(restored) > 0 {
			fmt.Printf("  restored: %s\n", strings.Join(restored, ", "))
		}
		return 0
	}

	// A challenge that was never linked to a theory has no theory half to
	// run. That is a COMPLETE restore, not a partial one, and saying
	// "theory not resolved" here would send an operator looking for a
	// theory that was never created.
	if resolution == internal.TheoryResolutionNotApplicable {
		fmt.Printf("✓ Memory %s cleared.\n", id)
		fmt.Printf("  no linked theory (challenge carried no theory reference)\n")
		if len(restored) > 0 {
			fmt.Printf("  restored: %s\n", strings.Join(restored, ", "))
		}
		return 0
	}

	// Theory reference present but no row matched. Say exactly that, and
	// name the reference kind so an operator can tell a stale id from
	// legacy free text at a glance.
	refKind, _ := res["theory_reference"].(string)
	if refKind == "" {
		refKind = "unknown"
	}
	warning, _ := res["theory_warning"].(string)
	fmt.Printf("⚠ Memory %s cleared. Theory NOT resolved.\n", id)
	fmt.Printf("  no theory row matched reference %q (kind=%s)\n", theoryID, refKind)
	if len(restored) > 0 {
		fmt.Printf("  restored: %s\n", strings.Join(restored, ", "))
	}
	if warning != "" && !strings.Contains(warning, theoryID) {
		// The core's warning restates the reference on its own line; skip
		// it when it would only repeat the line above.
		fmt.Printf("  %s\n", warning)
	}
	return 0
}
