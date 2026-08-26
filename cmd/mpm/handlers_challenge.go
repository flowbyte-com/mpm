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

	// Memory metadata patch with forward link to theory. The pre-challenge
	// weight is recorded so `mpm challenge restore` can put the memory back
	// to its exact prior epistemic standing.
	patch := map[string]interface{}{
		"status":                  "challenged",
		"challenged_theory_id":    theoryID,
		"challenged_prior_weight": int(mem["weight"].(int)),
	}
	patchJSON, _ := json.Marshal(patch)

	// Transaction: patch memory + save theory atomically
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

	// 2. Save theory with back-link — canonical INTEGER Unix epoch (matches BaseTables created_at INTEGER and saveMemoryRow)
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
	// its exact prior epistemic standing — the weakened value from the
	// challenge must not linger (and any accidental inflation must not
	// persist either).
	priorWeight, hasPrior := meta["challenged_prior_weight"].(float64)

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

	// 2. Clear challenged status from memory (null removes keys per RFC 7396),
	//    and restore the pre-challenge weight when it was recorded.
	clearPatch := map[string]interface{}{"status": nil, "challenged_theory_id": nil, "challenged_prior_weight": nil}
	clearJSON, _ := json.Marshal(clearPatch)
	if hasPrior {
		_, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), weight = ? WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), int(priorWeight), id)
	} else {
		_, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), id)
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
