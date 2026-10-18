package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm/cmd/mpm/render"
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

// challengeListLimitDefault is the row cap when --limit is not
// passed. Large enough to absorb the audit-time 41 from issue #20
// in a single call; small enough to keep the human-readable
// output scannable. The cap is also a load-bearing safety bound
// on the core read.
const challengeListLimitDefault = 50

// challengeListLimitMax is the upper bound enforced regardless of
// what the operator passes. The query is a single SELECT against
// indexed JSON; even at 10,000 rows the cost is bounded, but the
// intent here is "discovery surface", not "bulk export" — past
// that, the operator should use --json and a script.
const challengeListLimitMax = 1000

// handleChallengeList — `mpm challenge list [--json] [--limit N]`.
//
// Issue #20: the 41 memories left in `status='challenged'` by the
// retired auto-challenge defect had no operator-facing discovery
// surface. `mpm challenge restore <id>` was the sanctioned per-row
// recovery path, but it required the operator to already know
// which id to restore. This handler enumerates every challenged
// row with the columns an operator needs to make a per-row
// judgement: identity, current standing, the baselines the
// challenge recorded (so a no-op restore is visible before
// running it), which writer produced the challenge, and whether
// the row is restorable through MPM at all.
//
// Read-only by construction. The core primitive is a single
// SELECT with no write path; the handler does no writing. A
// regression test in challenge_list_test.go pins this contract
// with a full-table SHA snapshot before/after the call.
//
// HAL defect 1 (post #20): the previous version of this handler
// iterated `args` directly, but the router passes `args[1:]` to
// subcommand handlers — so the FIRST element the handler sees
// is the literal subcommand token "list", not the operator's
// first real argument. Without skipping it, every invocation
// would trip the strict-rejection check on the "list" token.
// The fix matches the existing handleChallengeRestore
// convention: treat args[0] as the subcommand token and parse
// from args[1:] onward.
func handleChallengeList(args []string) int {
	// Subcommand convention. The router passes the full
	// ["challenge", "list", ...] slice; this handler owns
	// the "list" half, so args[0] is the literal "list"
	// subcommand. The actual flags start at args[1].
	// `mpm challenge list` with no flags is a valid bare
	// invocation: skip the help check if args[0] is
	// exactly "list".
	if len(args) >= 1 && args[0] != "list" {
		return respond("", fmt.Sprintf("Error: unexpected subcommand %q for 'mpm challenge list'\n%s", args[0], challengeListUsage()), 1)
	}
	flagArgs := args[1:]

	// --help / -h / "help" short-circuit. Same defect, same fix
	// as handleMemoryList.
	for _, a := range flagArgs {
		if a == "-h" || a == "--help" || a == "help" {
			return respond("", challengeListUsage(), 0)
		}
	}

	// Single-pass argument parsing. The previous implementation
	// called stripMemoryFlagToken twice — once for --json, then
	// once for --limit on the ORIGINAL args — and discarded the
	// cleaned slice from the first call, so a command like
	// `mpm challenge list --json` would hit the "unknown
	// argument" check on `--json`. This pass walks the args once,
	// recognising --json/-j, --limit N, and --limit=N, and
	// accumulating anything else as an unknown.
	wantJSON := false
	limit := challengeListLimitDefault
	var unknown []string
	for i := 0; i < len(flagArgs); i++ {
		a := flagArgs[i]
		switch {
		case a == "--json" || a == "-j":
			wantJSON = true
		case a == "--limit":
			if i+1 >= len(flagArgs) {
				return respond("", fmt.Sprintf("Error: --limit requires a value\n%s", challengeListUsage()), 1)
			}
			val := flagArgs[i+1]
			n, err := parsePositiveInt(val)
			if err != nil {
				return respond("", fmt.Sprintf("Error: --limit value %q is not a positive integer\n%s", val, challengeListUsage()), 1)
			}
			limit = n
			i++ // skip the value
		case strings.HasPrefix(a, "--limit="):
			val := strings.TrimPrefix(a, "--limit=")
			n, err := parsePositiveInt(val)
			if err != nil {
				return respond("", fmt.Sprintf("Error: --limit value %q is not a positive integer\n%s", val, challengeListUsage()), 1)
			}
			limit = n
		case strings.HasPrefix(a, "-"):
			unknown = append(unknown, a)
		default:
			unknown = append(unknown, a)
		}
	}

	// Safety cap. Discovery surface, not bulk export — past 1000
	// the operator should use --json and a script.
	if limit > challengeListLimitMax {
		limit = challengeListLimitMax
	}

	// Strict rejection of unknown / malformed arguments. Mirrors
	// the handleDoctor strict-flag contract: refuse rather than
	// silently ignore, so a typo in the flag name is loud.
	if len(unknown) > 0 {
		return respond("", fmt.Sprintf("Error: unknown argument %q for 'mpm challenge list'\n%s", unknown[0], challengeListUsage()), 1)
	}

	dm := getDB()
	if dm == nil {
		return 1
	}
	return runChallengeList(dm, wantJSON, limit)
}

// parsePositiveInt parses a non-empty numeric string as a positive
// integer. Used by handleChallengeList to validate --limit values.
// Returns an error for empty input, non-numeric input, zero, or
// negative values.
func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("non-positive value %d", n)
	}
	return n, nil
}

// challengeListUsage is the single source of truth for the
// `mpm challenge list` help text. Kept short so the operator can
// read it on one screen; the per-row semantics are documented in
// docs/RECALL-MUTATION.md (issue #20).
func challengeListUsage() string {
	return "Usage: mpm challenge list [--json] [--limit N]\n" +
		"\n" +
		"Enumerate memories currently in status='challenged'.\n" +
		"Read-only. Show the columns needed to decide whether to\n" +
		"restore a row via 'mpm challenge restore <id>'.\n" +
		"\n" +
		"Flags:\n" +
		"  --json, -j    Emit the canonical JSON envelope (success, count, challenged).\n" +
		"  --limit N     Cap the row count (default 50, max 1000).\n" +
		"  -h, --help    Show this help.\n" +
		"\n" +
		"See docs/RECALL-MUTATION.md for the per-row restore semantics\n" +
		"and the policy on the residue from the retired auto-challenge defect.\n"
}

// runChallengeList is the injectable core of `mpm challenge list` so
// tests can drive it against a hermetic database.
//
// wantJSON selects the canonical JSON envelope; the human-readable
// path is the default. limit caps the row count returned (and
// rendered); the underlying SELECT is read-only.
//
// Truncation reporting: the JSON envelope carries BOTH `count`
// (rows in the returned slice) and `total_count` (rows the
// database had). When `truncated=true`, a script knows to
// re-query with a higher `--limit`. Without `total_count`, a
// script cannot distinguish "the limit was high enough" from
// "the limit capped the result" — a load-bearing distinction
// for the audit-time 41, where an operator who runs the
// command with the default `--limit=50` and gets `count=50`
// cannot tell whether the population is 50 or 4,100.
func runChallengeList(dm internal.CoreDB, wantJSON bool, limit int) int {
	rows, err := dm.ListChallengedMemories()
	if err != nil {
		if wantJSON {
			out, _ := json.Marshal(map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Failed to list challenged memories: %v", err),
			})
			return respond(string(out)+"\n", "", 1)
		}
		return respond("", fmt.Sprintf("Failed to list challenged memories: %v\n", err), 1)
	}

	totalCount := len(rows)
	truncated := false
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if wantJSON {
		envelope := map[string]interface{}{
			"success":     true,
			"count":       len(rows),
			"total_count": totalCount,
			"limit":       limit,
			"truncated":   truncated,
			"challenged":  challengedMemoriesToJSON(rows),
		}
		out, _ := json.MarshalIndent(envelope, "", "  ")
		return respond(string(out)+"\n", "", 0)
	}

	return renderChallengeListHuman(rows, totalCount, limit, truncated)
}

// challengedMemoriesToJSON converts a slice of core projections
// into the JSON-friendly shape the envelope carries. The shape is
// deliberately flat (no nested objects) so a script can read it
// without reaching for a JSONPath. Numeric values use Go's default
// JSON rendering for float64; absent baselines are surfaced as
// explicit nulls rather than omitted, so a script can distinguish
// "not recorded" from "0.0".
func challengedMemoriesToJSON(rows []internal.ChallengedMemory) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		entry := map[string]interface{}{
			"id":                       r.ID,
			"store":                    r.Store,
			"collection":               r.Collection,
			"current_weight":           r.CurrentWeight,
			"current_confidence":       r.CurrentConfidence,
			"prior_weight":             optionalFloat(r.PriorWeightPresent, r.PriorWeight),
			"prior_confidence":         optionalFloat(r.PriorConfidencePresent, r.PriorConfidence),
			"prior_retrieval_priority": optionalFloat(r.PriorRetrievalPriorityPresent, r.PriorRetrievalPriority),
			"challenged_via":           r.ChallengedVia,
			"challenged_at":            r.ChallengedAt,
			"challenged_at_iso":        internal.FormatUnixSeconds(r.ChallengedAt),
			"theory_id":                r.TheoryID,
			"theory_reference_shape":   r.TheoryReferenceShape,
			"restoreable":              r.Restoreable,
			"restore_weight_change":    optionalFloat(r.RestoreWeightChangePresent, r.RestoreWeightChange),
		}
		out = append(out, entry)
	}
	return out
}

// optionalFloat renders a (present, value) pair as either the
// float or JSON null. The nil-vs-zero distinction is load-bearing:
// the operator needs to see "no prior weight recorded" as null
// rather than 0.0, because 0.0 would falsely report a row that
// was slashed to zero (which the schema does not allow — the
// weight floor is 1.0, so 0.0 means "absent").
func optionalFloat(present bool, value float64) interface{} {
	if !present {
		return nil
	}
	return value
}

// renderChallengeListHuman prints the canonical human-readable
// table for `mpm challenge list`.
//
// Columns: ID, store.collection, weight→prior (with restoreable
// marker), via, theory_ref, challenged_at.
//
// The weight column is the most important one: a "1.0←1.0" entry
// is a no-op-on-weight restore, a "1.0←2.0" entry is a clamped
// row that would actually move on restore. The marker after the
// weight column also makes Restoreable explicit: rows with no
// prior weight show "[!]" instead of a number.
//
// Truncation reporting: when the run sliced the result down to
// `limit`, the heading reports the total count plus the slice
// size ("showing N of M; rerun with a higher --limit..."). The
// JSON envelope carries the same information as `truncated` and
// `total_count` fields; the human and machine surfaces agree
// on what the operator needs to know.
func renderChallengeListHuman(rows []internal.ChallengedMemory, totalCount int, limit int, truncated bool) int {
	var out strings.Builder
	render.Heading(&out, "Challenge list")
	out.WriteString(fmt.Sprintf("%d memories in status='challenged'", totalCount))
	if truncated {
		out.WriteString(fmt.Sprintf(" (showing %d of %d; rerun with a higher --limit for the full list)", len(rows), totalCount))
	}
	out.WriteString("\n\n")

	if len(rows) == 0 {
		out.WriteString("No challenged memories.\n")
		return respond(out.String(), "", 0)
	}

	// Header line. Fixed-width columns keep the table readable
	// across rows; the content columns (id, theory_ref,
	// challenged_at) are variable-width and the operator can
	// pipe through `column -t -s $'\t'` if they want.
	out.WriteString("ID                                store.collection     weight→prior         via                theory_ref        challenged_at\n")
	out.WriteString("--------------------------------  -------------------  -------------------  -----------------  ----------------  ----------------------------\n")

	for _, r := range rows {
		// "1.0←1.0" for the common case where the clamp hit
		// the floor; "1.0←2.0" for clamped rows that would
		// actually move; "[!] no prior" for rows that carry
		// no baseline (unrestorable through MPM).
		weight := weightArrow(r)
		via := r.ChallengedVia
		if via == "" {
			via = "-"
		}
		theory := r.TheoryReferenceShape
		if theory == "" {
			theory = "-"
		}
		when := internal.FormatUnixSeconds(r.ChallengedAt)
		if when == "" {
			when = "-"
		}
		out.WriteString(fmt.Sprintf("%-32s  %-19s  %-19s  %-17s  %-16s  %s\n",
			truncate(r.ID, 32),
			truncate(r.Store+"."+r.Collection, 19),
			truncate(weight, 19),
			truncate(via, 17),
			truncate(theory, 16),
			when,
		))
	}

	return respond(out.String(), "", 0)
}

// weightArrow renders the weight column in a form that surfaces
// the no-op-on-weight case at a glance AND makes the absent-prior
// case impossible to misread as a genuine recorded prior of 1.0.
//
// The four cases:
//
//	PriorWeightPresent, current == prior  →  "1.0←1.0"  (no-op on weight; the prior was honestly 1.0)
//	PriorWeightPresent, current != prior  →  "1.0←2.0"  (restore would move it)
//	!PriorWeightPresent, !Restoreable     →  "[!] no prior"  (unrestorable through MPM)
//	!PriorWeightPresent, Restoreable (RP) →  "1.0←[absent]"  (restorable via retrieval_priority only; weight-delta is undefined)
//
// The third and fourth cases are the ones HAL flagged: when no
// prior weight was recorded, the human column must NOT show
// "1.0←1.0" — that would invent a historical weight value the
// system never recorded. The JSON envelope encodes the same
// distinction as 0.0 (number) vs null.
func weightArrow(r internal.ChallengedMemory) string {
	if !r.Restoreable {
		return "[!] no prior"
	}
	if !r.PriorWeightPresent {
		// Restoreable via retrieval_priority only. The
		// "[absent]" marker is the human analogue of
		// the JSON null in restore_weight_change: the
		// system has no prior weight value to show.
		return fmt.Sprintf("%.1f←[absent]", r.CurrentWeight)
	}
	return fmt.Sprintf("%.1f←%.1f", r.CurrentWeight, r.PriorWeight)
}
