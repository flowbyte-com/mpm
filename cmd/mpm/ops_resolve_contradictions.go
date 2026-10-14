// ops_resolve_contradictions.go — `mpm ops resolve-contradictions` engine-room command.
//
// Operator's-eye view of the conflict resolution queue. Reads
// shared.contradiction_log, scores the contradictions by
// provenance, and either prints the proposed verdicts (dry-run,
// the default) or applies them in transactions (--apply).
//
// INVARIANTS:
//   1. Dry-run is the default. --apply must be explicit.
//   2. Each resolution is a single transaction: slash + write
//      resolution memory + mark queue row resolved. Crash mid-run
//      = no half-state.
//   3. Close calls (margin < threshold) propose a theory instead
//      of slashing. The queue row stays UNRESOLVED — human
//      arbitrates via the theory lifecycle.
//   4. Idempotent: running --apply twice produces zero new
//      changes. The resolved_at IS NULL filter is the worklist
//      boundary; once a row is resolved it's filtered out.
//   5. Only an ADJUDICATED row may penalise a memory. --apply over a
//      freshly-proposed candidate queue applies nothing, because a
//      cosine score shows relatedness and not conflict. Adjudication is
//      per-row and explicit (--adjudicate=ID:REASON). See
//      internal/core/contradiction_verification.go.
//
// Output formats:
//   - default: human-readable diff (suitable for terminal review)
//   - --json:  structured output (suitable for scripting / piping)
//
// Limits:
//   - --limit N caps the batch. Default 100. Operators clear
//     large queues in chunks.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

const defaultResolveLimit = 100

// adjudication is one parsed --adjudicate=ID:REASON request.
//
// It is a per-row decision by construction: each entry names exactly one
// queue row. There is deliberately no "adjudicate everything matching X"
// form, because adjudicating a batch without reading each pair is the
// defect this command's gating exists to prevent.
type adjudication struct {
	queueID int64
	reason  string
}

// resolveOutcome pairs a queue row with its decision so the renderer does
// not have to index two parallel slices. The unadjudicated rows are
// reported from the same slice as the applied ones, which is what keeps
// them visible rather than silently dropped.
type resolveOutcome struct {
	row     map[string]interface{}
	dec     mpminternal.ResolutionDecision
	pending bool // true when the row was refused for want of adjudication
}

// handleOpsResolveContradictions parses flags and dispatches the
// resolution loop. See printResolveHelp for the CLI shape.
func handleOpsResolveContradictions(args []string) int {
	dryRun := true
	apply := false
	jsonOutput := false
	limit := defaultResolveLimit
	loserFilter := ""
	winnerFilter := ""
	propose := false
	proposeLimit := 0
	var adjudications []adjudication

	for _, arg := range args {
		switch {
		case arg == "--apply":
			apply = true
			dryRun = false
		case arg == "--dry-run":
			dryRun = true
			apply = false
		case arg == "--json":
			jsonOutput = true
		case arg == "--propose":
			propose = true
		case strings.HasPrefix(arg, "--propose-limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--propose-limit="))
			if err != nil || n <= 0 {
				usererror.Error("--propose-limit must be a positive integer")
				return 1
			}
			proposeLimit = n
		case strings.HasPrefix(arg, "--adjudicate="):
			adj, err := parseAdjudicationFlag(strings.TrimPrefix(arg, "--adjudicate="))
			if err != nil {
				usererror.Error("%v", err)
				return 1
			}
			adjudications = append(adjudications, adj)
		case arg == "--help" || arg == "help":
			printResolveHelp()
			return 0
		case strings.HasPrefix(arg, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil || n <= 0 {
				usererror.Error("--limit must be a positive integer")
				return 1
			}
			limit = n
		case strings.HasPrefix(arg, "--loser="):
			loserFilter = strings.TrimPrefix(arg, "--loser=")
		case strings.HasPrefix(arg, "--winner="):
			winnerFilter = strings.TrimPrefix(arg, "--winner=")
		default:
			usererror.Error("unknown flag: %s", arg)
			printResolveHelp()
			return 1
		}
	}

	if apply && dryRun {
		// Shouldn't happen with current flag logic, but defensive.
		usererror.Error("--apply and --dry-run are mutually exclusive")
		return 1
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database not initialized")
		return 1
	}

	// --adjudicate runs BEFORE the queue is loaded, so the rows it
	// names are eligible in the same invocation that adjudicated them.
	//
	// It writes to the queue regardless of --apply, exactly as --propose
	// does. That is not an accident to be apologised for: recording that
	// an operator read a pair is a decision the operator has already made,
	// and asking them to also pass --apply to make their own decision
	// stick would be a worse interface. What the flag deliberately does
	// NOT do is apply anything — no memory weight moves here.
	for _, adj := range adjudications {
		if err := dm.AdjudicateContradiction(adj.queueID, adj.reason); err != nil {
			usererror.Error("adjudicate queue row %d: %v", adj.queueID, err)
			return 1
		}
	}

	// --propose: the ONLY way candidates reach the queue.
	//
	// Retrieval used to do this implicitly on every query, which meant a
	// read grew a persistent worklist. It now happens only because an
	// operator asked for it by name.
	//
	// These rows are UNVERIFIED SIMILARITY CANDIDATES. The scan compares
	// cosine similarity, which shows that two memories are related; it does
	// not show that their claims conflict. Near-identical duplicates score
	// at the very top of that range, so a candidate is a "look at this
	// pair" item, not a finding.
	var proposedCount int
	if propose {
		n, err := runProposeCandidates(dm, proposeLimit, jsonOutput)
		if err != nil {
			usererror.Error("propose candidates: %v", err)
			return 1
		}
		proposedCount = n
	}

	// Load the unresolved queue. Filter to limit (defensive: if
	// limit is huge, we still cap at defaultResolveLimit * 10 to
	// prevent a runaway transaction).
	if limit > defaultResolveLimit*10 {
		limit = defaultResolveLimit * 10
	}
	queue, err := dm.LoadContradictionQueue(limit)
	if err != nil {
		usererror.Error("load queue: %v", err)
		return 1
	}
	if len(queue) == 0 {
		if jsonOutput {
			// Same key set as the populated branch below. A consumer
			// scripting --json should not have to branch on whether the
			// queue happened to be empty to find out whether
			// needs_adjudication exists.
			fmt.Printf(`{"queue_size":0,"applied":0,"close_calls":0,"needs_adjudication":0,`+
				`"needs_adjudication_rows":[],"dry_run":%t,"candidates_proposed":%d,"decisions":[]}`+"\n",
				!apply, proposedCount)
		} else {
			fmt.Println("✓ Contradiction queue is empty.")
		}
		return 0
	}

	// Apply --loser/--winner filters if set. Filters are post-load
	// (not in SQL) because the queue is small (≤ limit) and the
	// filter is a convenience for the operator, not a query
	// optimization.
	if loserFilter != "" || winnerFilter != "" {
		filtered := queue[:0]
		for _, row := range queue {
			a, _ := row["memory_id_a"].(string)
			b, _ := row["memory_id_b"].(string)
			if loserFilter != "" && a != loserFilter && b != loserFilter {
				continue
			}
			if winnerFilter != "" && a != winnerFilter && b != winnerFilter {
				continue
			}
			filtered = append(filtered, row)
		}
		queue = filtered
	}

	// Resolve each row. In dry-run, this prints verdicts without
	// mutating. In --apply, the verdicts are applied transactionally
	// inside ResolveOneContradiction.
	//
	// A row no operator has adjudicated comes back as an
	// *UnverifiedContradictionError. That is an expected outcome with a
	// known next step, not a failure, so it is counted and explained
	// rather than dumped as an error line — a 100-row candidate queue
	// would otherwise print 100 identical complaints and bury any real
	// failure underneath. The row is still reported in full, so nothing
	// is silently discarded: it stays in the queue, waiting.
	outcomes := make([]resolveOutcome, 0, len(queue))
	applied := 0
	closeCalls := 0
	pendingAdjudication := 0
	for _, row := range queue {
		dec, err := dm.ResolveOneContradiction(row, apply)
		if err != nil {
			var unverified *mpminternal.UnverifiedContradictionError
			if errors.As(err, &unverified) {
				outcomes = append(outcomes, resolveOutcome{row: row, dec: dec, pending: true})
				pendingAdjudication++
				continue
			}
			usererror.Error("resolve row %v: %v", row["id"], err)
			// Continue processing the rest of the queue. A
			// single bad row shouldn't poison the batch.
			continue
		}
		outcomes = append(outcomes, resolveOutcome{row: row, dec: dec})
		if dec.IsCloseCall {
			closeCalls++
		}
		if apply {
			applied++
		}
	}

	// Output.
	if jsonOutput {
		decisions := make([]mpminternal.ResolutionDecision, 0, len(outcomes))
		pendingRows := make([]map[string]interface{}, 0, pendingAdjudication)
		for _, o := range outcomes {
			if o.pending {
				pendingRows = append(pendingRows, map[string]interface{}{
					"id":              o.row["id"],
					"memory_id_a":     o.row["memory_id_a"],
					"memory_id_b":     o.row["memory_id_b"],
					"detected_by":     o.row["detected_by"],
					"reason":          o.dec.VerificationNote,
					"adjudicate_with": fmt.Sprintf("--adjudicate=%v:<reason>", o.row["id"]),
				})
				continue
			}
			decisions = append(decisions, o.dec)
		}
		out := map[string]interface{}{
			"queue_size":              len(queue),
			"applied":                 applied,
			"close_calls":             closeCalls,
			"needs_adjudication":      pendingAdjudication,
			"needs_adjudication_rows": pendingRows,
			"dry_run":                 !apply,
			"decisions":               decisions,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	} else {
		printResolveDecisions(outcomes, apply, pendingAdjudication)
	}
	return 0
}

// parseAdjudicationFlag parses --adjudicate=QUEUE_ID:REASON.
//
// Split on the FIRST colon so a reason may itself contain colons, which
// is common in prose ("mem-a asserts X; mem-b: asserts not-X"). The queue
// id must still parse cleanly, which is what rejects the common mistake of
// passing a memory id here instead of a queue row id.
func parseAdjudicationFlag(value string) (adjudication, error) {
	rawID, reason, found := strings.Cut(value, ":")
	if !found {
		return adjudication{}, fmt.Errorf("--adjudicate needs a queue row id and a reason: --adjudicate=QUEUE_ID:REASON (got %q)", value)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if err != nil || id <= 0 {
		return adjudication{}, fmt.Errorf("--adjudicate queue row id must be a positive integer, got %q (find ids with --json)", rawID)
	}
	if strings.TrimSpace(reason) == "" {
		return adjudication{}, fmt.Errorf("--adjudicate for row %d needs a reason: record what you checked and why these two memories conflict", id)
	}
	return adjudication{queueID: id, reason: reason}, nil
}

// printResolveDecisions is the human-readable output. The dry-run
// and apply paths share the same renderer — the only difference
// is the header line, which tells the operator whether state was
// mutated.
//
// Rows awaiting adjudication are printed here rather than skipped. They
// are the normal state of a freshly-proposed queue, so hiding them would
// make the command look like it had nothing to show while leaving the
// operator with no idea which rows need their attention.
func printResolveDecisions(outcomes []resolveOutcome, applied bool, pendingAdjudication int) {
	if applied {
		fmt.Println("⚡ Applied resolutions:")
	} else {
		fmt.Println("🔍 Dry-run: proposed resolutions (use --apply to execute):")
	}
	fmt.Println()

	appliedCount := 0
	closeCalls := 0

	for _, o := range outcomes {
		dec := o.dec
		row := o.row

		if o.pending {
			fmt.Printf("  [NEEDS ADJUDICATION] queue_id=%v  (memory %s ↔ %s)\n",
				row["id"], row["memory_id_a"], row["memory_id_b"])
			fmt.Printf("    detected_by: %v\n", row["detected_by"])
			fmt.Printf("    why not applied: %s\n", dec.VerificationNote)
			fmt.Printf("    to apply: read both memories, then re-run with\n")
			fmt.Printf("      --adjudicate=%v:\"<what you checked and why they conflict>\"\n", row["id"])
			fmt.Println()
			continue
		}

		fmt.Printf("  [%s] queue_id=%v\n", dec.ResolutionTag, row["id"])
		fmt.Printf("    loser:  %s  (score=%.3f, confidence=%.3f, age=%.1fd)\n",
			dec.LoserID, dec.LoserScore.Score, dec.LoserScore.Confidence, dec.LoserScore.AgeDays)
		fmt.Printf("    winner: %s  (score=%.3f, confidence=%.3f, age=%.1fd)\n",
			dec.WinnerID, dec.WinnerScore.Score, dec.WinnerScore.Confidence, dec.WinnerScore.AgeDays)
		fmt.Printf("    margin: %.3f  (threshold=%.2f)\n", dec.Margin, mpminternal.ProvenanceThreshold)
		if dec.IsCloseCall {
			fmt.Printf("    verdict: ARBITRATION  → proposing theory (queue row stays open)\n")
		} else {
			fmt.Printf("    verdict: RESOLVE  → slash %d weight units from loser\n", dec.SlashAmount)
		}
		fmt.Printf("    rationale: %s\n", dec.Rationale)
		fmt.Println()

		appliedCount++
		if dec.IsCloseCall {
			closeCalls++
		}
	}

	// Summary line. The adjudication count is reported whether or not
	// anything was applied, because "nothing happened" and "nothing was
	// eligible" are very different states for an operator to be in.
	if applied {
		fmt.Printf("✓ Applied %d resolutions (%d arbitrations proposed).\n", appliedCount-closeCalls, closeCalls)
	} else {
		fmt.Printf("→ %d resolutions pending (%d arbitrations). Re-run with --apply to execute.\n", appliedCount-closeCalls, closeCalls)
	}
	if pendingAdjudication > 0 {
		fmt.Printf("⏸ %d row(s) need adjudication — no memory weight was changed for them.\n", pendingAdjudication)
	}
}

// runProposeCandidates scans live memories for above-threshold similarity
// pairs and enqueues them. Reached only via --propose.
//
// scanLimit caps the working set of memories examined (0 = every live
// memory). Pairs are evaluated only WITHIN that set, so a smaller limit is
// a cheaper and shallower scan, not a sample of the whole store.
//
// Returns the number of candidates enqueued. EnqueueContradiction is
// idempotent on the memory pair, so re-running over unchanged data enqueues
// nothing new — that is what stops a repeated scan from growing the queue.
func runProposeCandidates(dm *mpminternal.DatabaseManager, scanLimit int, jsonOutput bool) (int, error) {
	entries, err := dm.LoadSimilarityScanEntriesFederated(scanLimit)
	if err != nil {
		return 0, err
	}
	candidates := mpminternal.FindSimilarityCandidates(entries)

	inserted, err := dm.EnqueueSimilarityCandidates(candidates)
	if err != nil {
		return inserted, err
	}

	if jsonOutput {
		fmt.Printf(`{"scanned_memories":%d,"candidates_found":%d,"candidates_proposed":%d}`+"\n",
			len(entries), len(candidates), inserted)
		return inserted, nil
	}

	fmt.Printf("Scanned %d memories: %d unverified similarity candidate(s), %d new queue row(s).\n",
		len(entries), len(candidates), inserted)
	if len(candidates) > 0 {
		fmt.Println("These are RELATED pairs, not established contradictions —")
		fmt.Println("similarity is highest where duplication is highest. Review, then resolve.")
	}
	return inserted, nil
}

// printResolveHelp is the --help text. Kept in lock-step with the
// actual flag parsing above.
func printResolveHelp() {
	fmt.Println(`mpm ops resolve-contradictions [--propose] [--dry-run] [--apply] [--json] [--limit=N] [--loser=ID] [--winner=ID] [--adjudicate=ID:REASON]

Review and resolve rows in the shared queue (shared.contradiction_log).

WHAT IS IN THE QUEUE
  Two kinds of row, and the difference matters:
    - Similarity candidates — UNVERIFIED. Two memories scored high on
      cosine, which shows they are RELATED. It does not show their claims
      conflict. Near-identical duplicates score at the TOP of that range,
      so most candidates are duplicates, not contradictions.
    - Adjudicated conflicts — a human read both memories and concluded
      the claims actually conflict.
  Candidates are added ONLY by --propose. Retrieval never adds them.

WHAT --APPLY WILL AND WILL NOT DO
  --apply only penalises a memory for a row an operator has ADJUDICATED
  with --adjudicate. Applied to a freshly-proposed candidate queue it
  applies NOTHING and changes no memory weight — similarity is not a
  contradiction, and a batch --apply cannot be the thing that decides one.

  Rows awaiting adjudication are reported as NEEDS ADJUDICATION and left
  in the queue. They are never discarded.

  The workflow is therefore:
    1. --propose                 find candidate pairs
    2. (no flag)                 review them, read both memories
    3. --adjudicate=ID:REASON    for the ones that really do conflict
    4. --apply                   now those rows may penalise

Default mode is dry-run: prints proposed verdicts without mutating state.
Use --apply to execute resolutions in transactions.

Verdict logic (provenance-based scoring):
  - Compute score = 0.5 * confidence + 0.3 * freshness + 0.2 * reinforcement
  - Higher score wins. Loser is slashed by SlashFraction (50%) of weight.
  - If margin < threshold (0.10), propose a theory instead. Queue row
    stays open until the theory is resolved by the operator.

Each resolution is one transaction: slash + write resolution memory +
mark queue row resolved. Crash safety: no half-state.

Flags:
  --propose            Scan live memories and enqueue unverified similarity
                       candidates before reviewing. THIS WRITES TO THE
                       QUEUE, independent of --apply. Omit it and nothing
                       is ever added.
  --propose-limit=N    Cap memories examined by --propose (default: all)
  --adjudicate=ID:R    Record that you have read queue row ID and concluded
                       the two memories actually conflict. REPEATABLE, but
                       it is PER ROW: there is deliberately no way to
                       adjudicate a batch, because that is the mistake this
                       gate exists to prevent. The reason is recorded on the
                       row as audit trail, and the detection provenance that
                       brought it to the queue is preserved. Writes to the
                       queue independent of --apply; changes no memory
                       weight. Ids come from --json output.
  --dry-run            Print verdicts without mutating resolutions (default)
  --apply              Execute resolutions transactionally
  --json               Structured JSON output (for scripting)
  --limit=N            Max queue rows to process (default 100, cap 1000)
  --loser=ID           Only process rows where this memory is one side
  --winner=ID          Only process rows where this memory is one side
  --help               Print this help

Examples:
  mpm ops resolve-contradictions                       # dry-run, 100 rows
  mpm ops resolve-contradictions --propose              # scan + enqueue, then review
  mpm ops resolve-contradictions --json                 # find queue row ids
  mpm ops resolve-contradictions --adjudicate=42:"mem-a claims X, mem-b claims not-X"
  mpm ops resolve-contradictions --apply                # apply adjudicated rows
  mpm ops resolve-contradictions --apply --limit=50     # apply first 50
  mpm ops resolve-contradictions --loser=mem-abc-123    # narrow scope
  mpm ops resolve-contradictions --json --apply         # CI-friendly output`)
}
