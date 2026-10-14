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
// (status, challenged_theory_id, challenged_prior_weight,
// challenged_prior_retrieval_priority) are removed.
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
//
// Recovery contract (issue #24)
// ------------------------------
//
// A restore used to be reachable only for the ChallengeMemory family, and
// even then only when the row happened to be in the local `memories` table.
// The two Arc 1 writers slashed `shared.memories` and recorded nothing
// reversible, so:
//
//   - restore rejected them with "is not challenged" — the same message a
//     genuinely untouched memory gets, so an operator could not tell a
//     missing challenge from an unrecoverable one;
//   - and had it looked, it had no theory reference, no prior weight and no
//     prior retrieval_priority to act on.
//
// Restore is now driven by what the challenge actually recorded, which
// makes the three writers symmetric:
//
//	present in metadata      →  the challenge moved it  →  restore puts it back
//	absent from metadata     →  the challenge did NOT move it  →  restore leaves it alone
//
// That is why nothing is reconstructed. A column the challenge never
// touched must not be "restored" to a guessed value, because the honest
// reading of its absence is "this challenge did not change it" and the
// wrong one is "we lost the value" — and the second would let a restore
// clobber an unrelated later adjustment.
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

	// TheoryResolutionNotApplicable means the challenge was never linked
	// to a theory, so there is no theory half to run.
	//
	// This is a third outcome, not a flavour of not_found. The two Arc 1
	// writers record no `challenged_theory_id` because none exists: the
	// queue row's resolution_memory_id points at the resolution memory,
	// and an arbitration's theory is recorded under
	// `arbitration_theory` precisely so a restore does not go and mark a
	// verdict the operator already closed as 'resolved'. Reporting those
	// as `not_found` would tell the operator a theory was lost, when the
	// truth is that the restore is complete.
	TheoryResolutionNotApplicable = "not_applicable"
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
func resolveChallengedTheory(tx *sql.Tx, table ChallengeTable, theoryID, memoryID string) (int64, error) {
	resolveJSON, _ := json.Marshal(map[string]interface{}{"status": "disproven", "memory_id": nil})
	res, err := tx.Exec(fmt.Sprintf(`
		UPDATE %s
		SET metadata = json_patch(COALESCE(metadata,'{}'), ?)
		WHERE id = ?
		  AND collection = 'theories'
		  AND deleted_at IS NULL
		  AND COALESCE(
		        json_extract(metadata, '$.challenged_memory_id'),
		        json_extract(metadata, '$.memory_id')
		      ) = ?`, string(table)),
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

// challengedRow is a challenged memory's metadata plus the table it lives
// in. Which table matters: the Arc 1 writers slash `shared.memories` while
// the challenge surfaces slash the local `memories`, and a restore that
// only looked in one of them would be unreachable for half the writers.
type challengedRow struct {
	Table ChallengeTable
	Meta  map[string]interface{}
}

// readChallengedMetadata reads one table's metadata for a memory. found is
// false when the row is absent or soft-deleted, which is not an error.
func readChallengedMetadata(dm *DatabaseManager, table ChallengeTable, memoryID string) (meta map[string]interface{}, found bool, err error) {
	meta = map[string]interface{}{}
	var raw sql.NullString
	err = dm.db.QueryRow(fmt.Sprintf(
		`SELECT metadata FROM %s WHERE id = ? AND deleted_at IS NULL`, string(table)),
		memoryID).Scan(&raw)
	if err == sql.ErrNoRows {
		return meta, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s from %s: %w", memoryID, string(table), err)
	}
	if raw.Valid && raw.String != "" {
		if uerr := json.Unmarshal([]byte(raw.String), &meta); uerr != nil {
			return nil, false, fmt.Errorf("parse metadata for %s in %s: %w", memoryID, string(table), uerr)
		}
	}
	return meta, true, nil
}

// isChallengedMeta reports whether a metadata object carries the challenge
// flag. It reads pre-existing state and never sets it.
//
// It deliberately does NOT consider challenged_prior_* or challenged_at:
// those survive a restore as audit trail, so their presence says nothing
// about whether the challenge is still in force. `status` is the only
// authority (and the same authority the schema's aggregate and
// similarity_candidates_store.go's memoryIsChallenged use).
func isChallengedMeta(meta map[string]interface{}) bool {
	status, _ := meta["status"].(string)
	return status == "challenged"
}

// locateChallengedRow finds the challenged memory, wherever it lives.
//
// Local first: a memory id is authoritative in the manager's own table.
// The shared store is consulted only when the shared DB is attached, and a
// row there is preferred only when it is actually challenged — otherwise a
// stale shared copy would shadow a perfectly good local one.
//
// When neither store holds a challenged row but one of them holds the
// memory at all, that row is returned so the caller can say "not
// challenged" rather than "not found": the two are different operator
// problems and #24's contract requires them to read differently.
func (dm *DatabaseManager) locateChallengedRow(memoryID string) (*challengedRow, error) {
	type found struct {
		table ChallengeTable
		meta  map[string]interface{}
	}
	var (
		existing   []found
		challenged []found
	)
	for _, table := range dm.challengeTables() {
		meta, ok, err := readChallengedMetadata(dm, table, memoryID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		existing = append(existing, found{table, meta})
		if isChallengedMeta(meta) {
			challenged = append(challenged, found{table, meta})
		}
	}
	if len(challenged) > 0 {
		return &challengedRow{Table: challenged[0].table, Meta: challenged[0].meta}, nil
	}
	if len(existing) > 0 {
		return &challengedRow{Table: existing[0].table, Meta: existing[0].meta}, nil
	}
	return nil, fmt.Errorf("memory not found: %s", memoryID)
}

// unrecoverableChallengeError explains why a challenged row cannot be
// restored, and is deliberately a DIFFERENT message from "is not
// challenged". Before issue #24 both surfaced as the same string, so an
// operator looking at a memory slashed by `--apply` had no way to tell a
// missing challenge from an unrecoverable one — and no way to know that
// this memory's weight was permanently reduced.
//
// Nothing is reconstructed to paper over it. A pre-slash weight that was
// never recorded cannot be inferred: the memory may have been reinforced
// since, and inventing a value would write a number that never existed.
func unrecoverableChallengeError(memoryID string, meta map[string]interface{}) error {
	via, _ := meta["challenged_via"].(string)
	if via == "" {
		via = "an unrecorded challenge path"
	} else {
		via = "challenged_via=" + via
	}
	hasResolution, _ := meta["challenged_by_resolution"].(string)
	hint := "restore the memory from a backup or an out-of-band script"
	if hasResolution != "" {
		hint = fmt.Sprintf("re-derive the pre-slash weight from resolution %s, or restore the memory from a backup or an out-of-band script", hasResolution)
	}
	return fmt.Errorf(
		"memory %s is challenged (%s) but records no pre-challenge weight or retrieval priority, so the challenge cannot be reversed through MPM: %s. MPM will not reconstruct a prior value it never stored",
		memoryID, via, hint)
}

// readBaseline extracts a recorded prior value for one column.
//
// The three outcomes are kept distinct on purpose:
//
//	absent            → the challenge never moved this column
//	present, numeric  → the challenge moved it and this is where it started
//	present, not numeric → the record is corrupt
//
// The third case is not folded into the first. Treating a corrupt baseline
// as an absent one would report a restore that silently skipped a column
// the challenge had reduced — the same "half a restore reported as a whole"
// failure that #11 was about, one level down. And coercing it would write a
// number that never existed. Both are worse than refusing.
func readBaseline(meta map[string]interface{}, key string) (value float64, present bool, err error) {
	raw, ok := meta[key]
	if !ok || raw == nil {
		// Explicit JSON null is treated as absent: json_patch removes keys
		// that way, so a null is what "removed" looks like on the wire.
		return 0, false, nil
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, false, fmt.Errorf("recorded prior %s is %T, not a number", key, raw)
	}
	return f, true, nil
}

// RestoreMemoryFromChallenge resolves the challenged-theory record
// against the supplied memory id and clears the memory's challenged
// status.
//
// Partial-success semantics (issue #11)
// -------------------------------------
//
// The two halves of a restore are NOT equally recoverable, and the result
// map reports them separately rather than collapsing them into one
// "success":
//
//   - The MEMORY half (clear status, restore weight, restore retrieval
//     priority) always runs. It depends only on the memory row, which we
//     just read. If it fails, the transaction rolls back and an error is
//     returned — there is no partial write.
//   - The THEORY half runs only if the recorded reference actually matches
//     a live theory row that challenged THIS memory. A challenge with no
//     reference (`not_applicable`) has no theory half at all and is
//     complete when the memory half lands.
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
//
// Refusal (issue #24)
// ------------------
//
// A row with `status=challenged` but no recorded prior weight AND no
// recorded prior retrieval priority is refused outright, before any write.
// This is the historical shape produced by applyResolution and
// applyArbitrationResolution before they recorded anything: the columns
// were reduced and no record survives of what they were. There is no
// reconstruction path that would be honest, so the restore fails with a
// message that says which case this is.
func (dm *DatabaseManager) RestoreMemoryFromChallenge(memoryID string) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required for challenge restore")
	}

	loc, err := dm.locateChallengedRow(memoryID)
	if err != nil {
		return nil, err
	}
	meta := loc.Meta

	if !isChallengedMeta(meta) {
		return nil, fmt.Errorf("memory %s is not challenged", memoryID)
	}

	theoryID, _ := meta["challenged_theory_id"].(string)

	// What the challenge recorded is what the restore may undo. An absent
	// key is not a lost value — it is a claim that the challenge never
	// moved that column.
	priorWeight, weightRecorded, err := readBaseline(meta, "challenged_prior_weight")
	if err != nil {
		return nil, fmt.Errorf("memory %s is challenged but its recovery metadata is unusable: %w", memoryID, err)
	}
	priorRetrievalPriority, priorityRecorded, err := readBaseline(meta, "challenged_prior_retrieval_priority")
	if err != nil {
		return nil, fmt.Errorf("memory %s is challenged but its recovery metadata is unusable: %w", memoryID, err)
	}
	// challenged_prior_confidence is FORENSIC, not a restore target (F7.1:
	// restoration resets to the floor rather than re-promoting the prior
	// confidence), so only its PRESENCE is load-bearing — it marks that
	// this challenge dropped confidence and the restore must reset it. A
	// corrupt value there must not block a weight restore that is
	// otherwise sound.
	_, confidenceDropped := meta["challenged_prior_confidence"]

	if !weightRecorded && !priorityRecorded {
		// Nothing was recorded for anything the challenge moved. Refuse
		// rather than clear the flag and leave the slash in place, which
		// would report a restore that only half happened.
		return nil, unrecoverableChallengeError(memoryID, meta)
	}

	nowSec := time.Now().Unix()

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Resolve theory: status → disproven, memory_id → null (RFC 7396
	//    null removal). Zero affected rows is an observation, not an error:
	//    see the partial-success semantics documented above.
	//
	//    The theory usually lives in the same store as the memory, but
	//    not always: a challenge theory is created through the local
	//    MemoryStore while an arbitration theory lives in shared. Both
	//    stores are tried with identical exact-match guards, so the second
	//    attempt can only ever find a theory that back-links to THIS
	//    memory — it is not a speculative search, and it stops at the
	//    first store that matched nothing rather than accumulating
	//    candidates.
	var theoryRows int64
	if theoryID != "" {
		tried := map[ChallengeTable]bool{}
		for _, table := range append([]ChallengeTable{loc.Table}, dm.challengeTables()...) {
			if tried[table] {
				continue
			}
			tried[table] = true
			rows, rerr := resolveChallengedTheory(tx, table, theoryID, memoryID)
			if rerr != nil {
				return nil, rerr
			}
			if rows > 0 {
				theoryRows = rows
				break
			}
		}
	}

	// 2. Clear challenged status from memory, put back every column this
	//    challenge recorded a prior for, leave confidence at the
	//    challenged floor (do NOT silently re-promote to prior
	//    confidence) when the challenge dropped it, and stamp audit
	//    metadata so the cycle is reconstructable.
	clearPatch := map[string]interface{}{
		"status":                              nil,
		"challenged_theory_id":                nil,
		"challenged_prior_weight":             nil,
		"challenged_prior_retrieval_priority": nil,
		"restored_from_challenge":             true,
		"restored_at":                         nowSec,
	}
	clearJSON, _ := json.Marshal(clearPatch)

	setClauses := []string{
		`metadata = json_patch(COALESCE(metadata,'{}'), ?)`,
		`updated_at = CAST(strftime('%s','now') AS INTEGER)`,
	}
	args := []interface{}{string(clearJSON)}
	if weightRecorded {
		setClauses = append(setClauses, `weight = ?`)
		args = append(args, priorWeight)
	}
	if priorityRecorded {
		setClauses = append(setClauses, `retrieval_priority = ?`)
		args = append(args, priorRetrievalPriority)
	}
	if confidenceDropped {
		setClauses = append(setClauses, `confidence = ?`)
		args = append(args, ChallengedMemoryConfidenceFloor)
	}
	args = append(args, memoryID)

	memRes, err := tx.Exec(fmt.Sprintf(`
		UPDATE %s
		SET %s
		WHERE id = ? AND deleted_at IS NULL`, string(loc.Table), strings.Join(setClauses, ", ")), args...)
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
	via, _ := meta["challenged_via"].(string)
	result := map[string]interface{}{
		"success":                     true,
		"memory_id":                   memoryID,
		"memory_store":                storeLabel(loc.Table),
		"challenge_via":               via,
		"theory_id":                   theoryID,
		"theory_reference":            classifyTheoryReference(theoryID),
		"action":                      "restored",
		"memory_restored":             true,
		"weight_restored":             weightRecorded,
		"retrieval_priority_restored": priorityRecorded,
		"confidence_reset":            confidenceDropped,
		"theory_resolved":             theoryRows > 0,
		"theory_rows_affected":        theoryRows,
	}

	switch {
	case theoryID == "":
		// A challenge with no linked theory has no theory half to run.
		// This is complete, not partial.
		result["theory_status"] = ""
		result["theory_reference"] = ""
		result["theory_resolution"] = TheoryResolutionNotApplicable
		result["complete"] = true
	case theoryRows > 0:
		result["theory_status"] = "disproven"
		result["theory_resolution"] = TheoryResolutionResolved
		result["complete"] = true
	default:
		// No status to report: the row was never located. Reporting a
		// status here is what made issue #11 invisible.
		result["theory_status"] = ""
		result["theory_resolution"] = TheoryResolutionNotFound
		result["complete"] = false
		result["theory_warning"] = fmt.Sprintf(
			"memory %s was restored, but no theory matched reference %q (kind=%s); no theory was resolved",
			memoryID, theoryID, classifyTheoryReference(theoryID))
	}
	return result, nil
}

// storeLabel names a memories table in operator-facing language. The value
// travels in the result map, which `mpm call` and MCP callers read.
func storeLabel(table ChallengeTable) string {
	if table == ChallengeTableShared {
		return "shared"
	}
	return "local"
}
