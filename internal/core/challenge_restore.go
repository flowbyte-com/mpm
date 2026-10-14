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
//
// Affected-row accounting (issue #11)
// -----------------------------------
//
// This function used to run the theory UPDATE without reading its
// RowsAffected, then return a map asserting `"theory_status":"disproven"`
// regardless. When challenged_theory_id did not resolve to a live row —
// which is the case for every row written by the retired auto-challenge
// defect, because that writer stored a human-readable reason in
// challenged_theory_id rather than a theory id — the statement matched zero
// rows, raised no error, the transaction committed, and the operator was
// told the theory had been resolved when nothing had been.
//
// Both statements now report what they actually did, and the result map is
// built from those observations rather than from intent. See the documented
// partial-success semantics on RestoreMemoryFromChallenge.
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Theory-resolution outcomes reported by RestoreMemoryFromChallenge.
//
// These are observations, not intents. `resolved` is only ever returned
// when the UPDATE actually affected a row.
const (
	// TheoryResolutionResolved means the linked theory row was found and
	// marked disproven.
	TheoryResolutionResolved = "resolved"

	// TheoryResolutionNotFound means no theory row matched the recorded
	// reference. The memory half of the restore still happened; only the
	// theory half had nothing to act on.
	TheoryResolutionNotFound = "not_found"
)

// Theory reference shapes, classified lexically from the stored value.
//
// This is a LABEL for reporting, never a precondition for acting. Whether a
// theory is resolved is decided solely by how many rows the UPDATE matched;
// nothing here searches for a "close enough" theory, because guessing which
// theory an operator meant is exactly the speculation that could mark an
// unrelated theory resolved.
const (
	// TheoryReferenceIdentifier is a GenerateID-shaped value (16 hex chars).
	TheoryReferenceIdentifier = "identifier"

	// TheoryReferenceFreeText is prose. Rows written by the retired
	// auto-challenge defect carry a human-readable reason here; those
	// predate the identifier-based contract and have no theory row to find.
	TheoryReferenceFreeText = "free_text"
)

// classifyTheoryReference labels a challenged_theory_id value by shape.
//
// Generated identifiers are 16 hex characters (see GenerateID). Anything
// else — prose, an id from another system, a truncated value — is reported
// as free_text so an operator can tell "the link is stale" apart from "this
// row never had a resolvable link".
func classifyTheoryReference(theoryID string) string {
	id := strings.TrimSpace(theoryID)
	if len(id) != 16 {
		return TheoryReferenceFreeText
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return TheoryReferenceFreeText
		}
	}
	return TheoryReferenceIdentifier
}

// resolveChallengedTheory marks the theory that challenged this memory as
// disproven, and returns how many rows it actually changed.
//
// Two guards, both load-bearing:
//
//   - `collection = 'theories'`. Without it, a free-text reference that
//     happened to collide with an ordinary memory's id would stamp that
//     unrelated memory's metadata `status=disproven`. The 41 legacy rows
//     store prose here; prose is exactly the kind of value that can
//     collide.
//   - the back-link check. Without it, a stale or mis-patched reference
//     resolves a theory belonging to some other memory.
//
// The back-link is checked under BOTH keys the challenge paths use:
// `challenged_memory_id` (ChallengeMemoryWithTheory, internal/core/
// epistemology_tools.go) and `memory_id` (the `mpm challenge` CLI handler,
// cmd/mpm/handlers_challenge.go). They name the same thing — the memory
// this theory contests — and both writers stamp exactly one of them.
// Restricting the guard to a single key would silently stop resolving
// theories created by the other writer, which is the same class of
// false-negative this fix exists to remove.
//
// Neither guard speculates: both only ever REJECT rows. No fallback search
// is attempted, so a reference that does not match exactly leaves the
// theory untouched rather than resolving a near-miss.
func resolveChallengedTheory(tx *sql.Tx, theoryID, memoryID string) (int64, error) {
	resolveJSON, _ := json.Marshal(map[string]interface{}{"status": "disproven", "memory_id": nil})
	res, err := tx.Exec(`
		UPDATE memories
		SET metadata = json_patch(COALESCE(metadata,'{}'), ?)
		WHERE id = ?
		  AND collection = 'theories'
		  AND deleted_at IS NULL
		  AND COALESCE(
		        json_extract(metadata, '$.challenged_memory_id'),
		        json_extract(metadata, '$.memory_id')
		      ) = ?`,
		string(resolveJSON), theoryID, memoryID)
	if err != nil {
		return 0, fmt.Errorf("resolve theory %s: %w", theoryID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		// Without this the count is unknown, and an unknown count must
		// never be reported as a resolution.
		return 0, fmt.Errorf("resolve theory %s: cannot determine affected rows: %w", theoryID, err)
	}
	return affected, nil
}

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
//
// Partial-success semantics (issue #11)
// -------------------------------------
//
// The two halves of a restore are NOT equally recoverable, and the result
// map reports them separately rather than collapsing them into one
// "success":
//
//   - The MEMORY half (clear status, restore weight) always runs. It
//     depends only on the memory row, which we just read. If it fails,
//     the transaction rolls back and an error is returned — there is no
//     partial write.
//   - The THEORY half runs only if the recorded reference actually matches
//     a live theory row that challenged THIS memory.
//
// When the theory is not found, the memory half is still committed and
// `complete` is false with `theory_resolution` = `not_found`. That is
// deliberate and is the alternative to refusing the restore outright:
// refusing would leave memories unrecoverable because their reference is
// stale, which is precisely the condition the operator is trying to
// repair. Un-restoring the memory is the safe half of the operation —
// the pre-challenge weight is genuinely known from the metadata, so
// reinstating it restores the state that existed before any challenge.
//
// What is NOT done: no fallback search for a "close enough" theory. If the
// reference does not match, nothing is resolved. Guessing which theory an
// operator meant is how an unrelated theory gets marked disproven.
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

	// 1. Resolve theory: status → disproven, memory_id → null (RFC 7396
	//    null removal). Zero affected rows is an observation, not an error:
	//    see the partial-success semantics documented above.
	theoryRows, err := resolveChallengedTheory(tx, theoryID, memoryID)
	if err != nil {
		return nil, err
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

	var memRes sql.Result
	if hasPrior {
		memRes, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), weight = ?, confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), int(priorWeight), ChallengedMemoryConfidenceFloor, memoryID)
	} else {
		memRes, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			string(clearJSON), ChallengedMemoryConfidenceFloor, memoryID)
	}
	if err != nil {
		return nil, fmt.Errorf("clear memory status: %w", err)
	}

	// The memory was read moments ago from the same database, so a zero
	// count here means the row changed underneath us. Committing a theory
	// resolution on top of that would report a restore that never landed.
	memRows, err := memRes.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("clear memory status: cannot determine affected rows: %w", err)
	}
	if memRows == 0 {
		return nil, fmt.Errorf("restore aborted: memory %s was not updated (no matching live row)", memoryID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Report observations, never intent. `theory_status` used to be the
	// literal "disproven" on every path; it now reflects what the database
	// actually did.
	result := map[string]interface{}{
		"success":              true,
		"memory_id":            memoryID,
		"theory_id":            theoryID,
		"theory_reference":     classifyTheoryReference(theoryID),
		"action":               "restored",
		"memory_restored":      true,
		"weight_restored":      hasPrior,
		"theory_resolved":      theoryRows > 0,
		"theory_rows_affected": theoryRows,
	}
	if theoryRows > 0 {
		result["theory_status"] = "disproven"
		result["theory_resolution"] = TheoryResolutionResolved
		result["complete"] = true
	} else {
		// No status to report: the row was never located. Reporting a
		// status here is what made this defect invisible.
		result["theory_status"] = ""
		result["theory_resolution"] = TheoryResolutionNotFound
		result["complete"] = false
		result["theory_warning"] = fmt.Sprintf(
			"memory %s was restored, but no theory matched reference %q (kind=%s); no theory was resolved",
			memoryID, theoryID, classifyTheoryReference(theoryID))
	}
	return result, nil
}
