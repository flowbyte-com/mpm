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
	"fmt"
	"strconv"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

const defaultResolveLimit = 100

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
			fmt.Printf(`{"queue_size":0,"candidates_proposed":%d,"decisions":[]}`+"\n", proposedCount)
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
	decisions := make([]mpminternal.ResolutionDecision, 0, len(queue))
	applied := 0
	closeCalls := 0
	for _, row := range queue {
		dec, err := dm.ResolveOneContradiction(row, apply)
		if err != nil {
			usererror.Error("resolve row %v: %v", row["id"], err)
			// Continue processing the rest of the queue. A
			// single bad row shouldn't poison the batch.
			continue
		}
		decisions = append(decisions, dec)
		if dec.IsCloseCall {
			closeCalls++
		}
		if apply {
			applied++
		}
	}

	// Output.
	if jsonOutput {
		out := map[string]interface{}{
			"queue_size":  len(queue),
			"applied":     applied,
			"close_calls": closeCalls,
			"dry_run":     !apply,
			"decisions":   decisions,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	} else {
		printResolveDecisions(queue, decisions, apply)
	}
	return 0
}

// printResolveDecisions is the human-readable output. The dry-run
// and apply paths share the same renderer — the only difference
// is the header line, which tells the operator whether state was
// mutated.
func printResolveDecisions(queue []map[string]interface{}, decisions []mpminternal.ResolutionDecision, applied bool) {
	if applied {
		fmt.Println("⚡ Applied resolutions:")
	} else {
		fmt.Println("🔍 Dry-run: proposed resolutions (use --apply to execute):")
	}
	fmt.Println()

	for i, dec := range decisions {
		row := queue[i]
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
	}

	// Summary line.
	closeCalls := 0
	for _, d := range decisions {
		if d.IsCloseCall {
			closeCalls++
		}
	}
	if applied {
		fmt.Printf("✓ Applied %d resolutions (%d arbitrations proposed).\n",
			len(decisions)-closeCalls, closeCalls)
	} else {
		fmt.Printf("→ %d resolutions pending (%d arbitrations). Re-run with --apply to execute.\n",
			len(decisions)-closeCalls, closeCalls)
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
	entries, err := dm.LoadSimilarityScanEntries(scanLimit, "")
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
	fmt.Println(`mpm ops resolve-contradictions [--propose] [--dry-run] [--apply] [--json] [--limit=N] [--loser=ID] [--winner=ID]

Review and resolve rows in the shared queue (shared.contradiction_log).

WHAT IS IN THE QUEUE
  Two kinds of row, and the difference matters:
    - Similarity candidates — UNVERIFIED. Two memories scored high on
      cosine, which shows they are RELATED. It does not show their claims
      conflict. Near-identical duplicates score at the TOP of that range,
      so most candidates are duplicates, not contradictions.
    - Operator-established conflicts — a human adjudicated these.
  Candidates are added ONLY by --propose. Retrieval never adds them.

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
  mpm ops resolve-contradictions --apply --limit=50     # apply first 50
  mpm ops resolve-contradictions --loser=mem-abc-123    # narrow scope
  mpm ops resolve-contradictions --json --apply         # CI-friendly output`)
}
