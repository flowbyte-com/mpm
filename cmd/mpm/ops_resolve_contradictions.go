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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

const defaultResolveLimit = 100

// Proposal budget defaults and ceilings.
//
// The scan limit applies PER SOURCE: the federated scan reads up to
// `limit` local memories and up to `limit` shared memories, then
// deduplicates by ID, so the union is at most 2*limit.
//
// The comparison budget is NOT checked here against that upper bound. It
// is enforced inside the discovery pass itself, against the entries the
// pass was actually given (see FindSimilarityCandidatesBounded).
//
// That distinction matters. A check here would refuse a scan the pass
// could have completed inside the budget — the union's upper bound
// over-counts whenever local and shared overlap — and would approve a
// scan the pass later refuses. The budget has to be applied where the
// pairs are counted, or it is not applied at all.
const (
	// defaultProposeScanLimit is how many memories per source --propose
	// examines by default.
	defaultProposeScanLimit = 100

	// maxProposeScanLimit is the hard ceiling on the per-source scan
	// limit. Raising it does NOT bypass the comparison budget: a larger
	// window implies more pairs, and the discovery pass enforces the
	// comparison budget against the pairs it actually evaluates.
	maxProposeScanLimit = 2000

	defaultCompareBudget = mpminternal.DefaultCompareBudget
	minCompareBudget     = mpminternal.MinCompareBudget
	maxCompareBudget     = mpminternal.MaxCompareBudget

	defaultCandidateCap = mpminternal.DefaultCandidateCap
	maxCandidateCap     = mpminternal.MaxCandidateCap

	defaultWriteBudget = mpminternal.DefaultWriteBudget
	maxWriteBudget     = mpminternal.MaxWriteBudget
)

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

// proposeSummary is the nested `propose` object in the JSON envelope.
//
// It exists as one object in one place because the command used to emit
// the proposal statistics from a SEPARATE JSON line, which meant a
// consumer reading stdout had to handle two documents and had no way to
// know whether a proposal had even run.
//
// Every count here describes something that happened, not something that
// was attempted. In particular candidates_proposed counts rows that were
// CONFIRMED COMMITTED: it is zero on every failure path, including the
// uncertain one, because an unconfirmed write is not a write.
type proposeSummary struct {
	// Requested is false when --propose was not passed. The object is
	// always present so the envelope has one stable shape.
	Requested bool `json:"requested"`

	// ScanLimitPerSource is the per-source memory window that was
	// scanned.
	ScanLimitPerSource int `json:"scan_limit_per_source"`

	// EntriesScanned is how many memories the federated scan returned
	// after deduplication.
	EntriesScanned int `json:"entries_scanned"`

	// PairsExamined is how many pairs the discovery pass evaluated.
	PairsExamined int `json:"pairs_examined"`

	// CandidatesFoundInScan is how many qualifying candidates the scan
	// found WITHIN THE SCANNED WINDOW.
	//
	// It is never a corpus total. The scan is bounded by
	// scan_limit_per_source per source, so a memory outside that window
	// was never compared with anything. Read it together with
	// scan_complete: when that is false, the count describes a pass that
	// did not finish.
	CandidatesFoundInScan int `json:"candidates_found_in_scan"`

	// ScanComplete is true when the discovery pass examined every pair
	// the window implied and was not refused. False when the comparison
	// budget or the candidate cap stopped it.
	ScanComplete bool `json:"scan_complete"`

	// CandidatesMaterialised is how many candidates the scan retained
	// before any cap applied.
	CandidatesMaterialised int `json:"candidates_materialised"`

	// CandidatesProposed counts CONFIRMED COMMITTED queue rows. Zero on
	// every failure path.
	CandidatesProposed int `json:"candidates_proposed"`

	// CandidatesSkippedPending counts candidates whose pair was already
	// an unresolved queue row.
	CandidatesSkippedPending int `json:"candidates_skipped_pending"`

	// CandidatesSkippedDuplicate counts candidates whose canonical pair
	// appeared more than once in the batch.
	CandidatesSkippedDuplicate int `json:"candidates_skipped_duplicate"`

	// CandidatesSkippedConstrained counts eligible candidates whose
	// INSERT was suppressed by the table's own uniqueness constraint.
	//
	// This is a rare case — a resolved pair re-proposed within the same
	// second as its original detection — and it is reported rather than
	// hidden, so that "eligible 1, proposed 0" has an explanation.
	CandidatesSkippedConstrained int `json:"candidates_skipped_constrained"`

	// CompareBudget, CandidateCap and WriteBudget are the budgets that
	// were in force.
	CompareBudget int `json:"compare_budget"`
	CandidateCap  int `json:"candidate_cap"`
	WriteBudget   int `json:"write_budget"`

	// Completion names the outcome. One of:
	//
	//	not_run          --propose was not passed
	//	complete         the scan and the write both succeeded
	//	compare_budget   the scan was refused before any pair was examined
	//	candidate_cap    the scan found more candidates than it may retain
	//	write_budget     the batch was refused by the write budget
	//	invalid_candidate a candidate failed validation; the batch was refused
	//	db_busy          another writer held the lock
	//	db_insert        an insertion failed and was rolled back
	//	rollback         a rollback itself failed; the outcome is unknown
	//	uncertain_commit the commit failed; the outcome is unknown
	//	scan_failed      the scan could not read the corpus
	Completion string `json:"completion"`

	// CommitState is the transaction's disposition: not_run, refused,
	// rolled_back, uncertain or confirmed.
	CommitState string `json:"commit_state"`

	// Error carries the failure detail, or null on success.
	Error *string `json:"error"`
}

// strPtr returns a pointer to s, for the optional error field.
func strPtr(s string) *string { return &s }

// errorText renders an error for the envelope's error field.
//
// It never returns a pointer: the caller decides whether the field is
// populated, so a nil error cannot become an empty string that reads like
// a failure with no reason.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// proposeOptions carries the effective budgets for one proposal run.
type proposeOptions struct {
	scanLimit     int
	compareBudget int
	candidateCap  int
	writeBudget   int
}

// handleOpsResolveContradictions parses flags and dispatches the
// resolution loop. See printResolveHelp for the CLI shape.
func handleOpsResolveContradictions(args []string) int {
	dryRun := true
	apply := false
	applyRequested := false
	jsonOutput := false
	limit := defaultResolveLimit
	loserFilter := ""
	winnerFilter := ""
	propose := false
	proposeOpts := proposeOptions{
		scanLimit:     defaultProposeScanLimit,
		compareBudget: defaultCompareBudget,
		candidateCap:  defaultCandidateCap,
		writeBudget:   defaultWriteBudget,
	}
	// proposeOnlyFlags records the propose-scoped flags the operator
	// actually typed, so a flag that only means something with --propose
	// can be refused rather than silently ignored.
	var proposeOnlyFlags []string
	var adjudications []adjudication

	for _, arg := range args {
		switch {
		case arg == "--apply":
			apply = true
			applyRequested = true
			dryRun = false
		case arg == "--dry-run":
			dryRun = true
			apply = false
		case arg == "--json":
			jsonOutput = true
		case arg == "--propose":
			propose = true
		case strings.HasPrefix(arg, "--propose-limit="):
			raw := strings.TrimPrefix(arg, "--propose-limit=")
			if raw == "all" {
				usererror.Error("--propose-limit=all is not supported. Use a numeric "+
					"--propose-limit=N (%d..%d) to bound the scan.", 1, maxProposeScanLimit)
				return 1
			}
			n, err := strconv.Atoi(raw)
			if err != nil {
				usererror.Error("--propose-limit must be an integer, got %q", raw)
				return 1
			}
			if n < 1 || n > maxProposeScanLimit {
				usererror.Error("--propose-limit must be between 1 and %d, got %d", maxProposeScanLimit, n)
				return 1
			}
			proposeOpts.scanLimit = n
			proposeOnlyFlags = append(proposeOnlyFlags, "--propose-limit")
		case arg == "--propose-limit":
			usererror.Error("--propose-limit needs a value: use --propose-limit=N")
			return 1
		case strings.HasPrefix(arg, "--compare-budget="):
			raw := strings.TrimPrefix(arg, "--compare-budget=")
			n, err := strconv.Atoi(raw)
			if err != nil {
				usererror.Error("--compare-budget must be an integer, got %q", raw)
				return 1
			}
			if n < minCompareBudget || n > maxCompareBudget {
				usererror.Error("--compare-budget must be between %d and %d, got %d",
					minCompareBudget, maxCompareBudget, n)
				return 1
			}
			proposeOpts.compareBudget = n
			proposeOnlyFlags = append(proposeOnlyFlags, "--compare-budget")
		case arg == "--compare-budget":
			usererror.Error("--compare-budget needs a value: use --compare-budget=N")
			return 1
		case strings.HasPrefix(arg, "--candidate-cap="):
			raw := strings.TrimPrefix(arg, "--candidate-cap=")
			n, err := strconv.Atoi(raw)
			if err != nil {
				usererror.Error("--candidate-cap must be an integer, got %q", raw)
				return 1
			}
			if n < 1 || n > maxCandidateCap {
				usererror.Error("--candidate-cap must be between 1 and %d, got %d", maxCandidateCap, n)
				return 1
			}
			proposeOpts.candidateCap = n
			proposeOnlyFlags = append(proposeOnlyFlags, "--candidate-cap")
		case arg == "--candidate-cap":
			usererror.Error("--candidate-cap needs a value: use --candidate-cap=N")
			return 1
		case strings.HasPrefix(arg, "--write-budget="):
			raw := strings.TrimPrefix(arg, "--write-budget=")
			n, err := strconv.Atoi(raw)
			if err != nil {
				usererror.Error("--write-budget must be an integer, got %q", raw)
				return 1
			}
			if n < 1 || n > maxWriteBudget {
				usererror.Error("--write-budget must be between 1 and %d, got %d", maxWriteBudget, n)
				return 1
			}
			proposeOpts.writeBudget = n
			proposeOnlyFlags = append(proposeOnlyFlags, "--write-budget")
		case arg == "--write-budget":
			usererror.Error("--write-budget needs a value: use --write-budget=N")
			return 1
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

	// ── Flag-combination validation ─────────────────────────────────
	//
	// These checks run BEFORE getDBConcrete(). A combination that is
	// refused must not have opened a database, let alone written to one:
	// the operator should get the same answer whether or not a database
	// happens to exist.
	//
	// --propose with --apply is refused because the two do different
	// things to different tables: --propose writes review items, --apply
	// resolves them. Combining them in one invocation would make the
	// outcome depend on the order the rows happened to be processed.
	//
	// --propose with --adjudicate is refused for the same reason: an
	// adjudication is a judgement about a row that must already exist,
	// and the rows a proposal would create are not the rows an operator
	// has read.
	//
	// The propose-scoped budgets are refused without --propose because
	// they describe a scan that is not happening. Accepting them
	// silently would make the command report budgets it never applied.
	if propose {
		if applyRequested {
			usererror.Error("--propose and --apply cannot be combined. " +
				"--propose writes new review items to the queue; --apply resolves " +
				"existing ones. Run them as separate invocations.")
			return 1
		}
		if len(adjudications) > 0 {
			usererror.Error("--propose and --adjudicate cannot be combined. " +
				"An adjudication is a judgement about a queue row you have read; the " +
				"rows a proposal creates are not those. Run --adjudicate separately.")
			return 1
		}
	} else if len(proposeOnlyFlags) > 0 {
		usererror.Error("%s only applies to a proposal scan; add --propose or drop %s",
			strings.Join(proposeOnlyFlags, ", "), strings.Join(proposeOnlyFlags, ", "))
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
	//
	// NOTE: --propose writes to the queue even in dry-run mode. --dry-run
	// governs the RESOLUTION phase; --propose is a separate phase, and
	// its whole purpose is to add review items. A scan that found
	// candidates and enqueued none would be a scan that did nothing.
	proposeSummaryValue := proposeSummary{
		Requested:          propose,
		ScanLimitPerSource: proposeOpts.scanLimit,
		CompareBudget:      proposeOpts.compareBudget,
		CandidateCap:       proposeOpts.candidateCap,
		WriteBudget:        proposeOpts.writeBudget,
		Completion:         "not_run",
		CommitState:        "not_run",
	}
	var proposeErr error
	if propose {
		proposeSummaryValue, proposeErr = runBoundedProposal(dm, proposeOpts)
	}

	// The total unresolved count is read AFTER the proposal attempt, so it
	// describes the queue as the proposal left it — the number the operator
	// needs in order to decide whether to go and look for more.
	//
	// It is read BEFORE the review, which means it is the count as it stood
	// when the review began. Under --apply the review then resolves some of
	// those rows, so the remainder afterwards is this number minus `applied`.
	// The count is deliberately not moved to the end: a queue that cannot be
	// counted is discovered before anything is mutated, not after.
	//
	// On the failure path it is still worth having: in the uncertain case it
	// is the one number that tells the operator whether to go and look.
	//
	// A count failure is NOT fatal before the envelope is emitted.
	//
	// --json promises exactly one document per invocation, so a consumer
	// that received nothing could not tell a broken queue from a crash.
	// The failure is reported INSIDE the envelope instead: the total
	// becomes null, which is the honest representation of "not known",
	// and the error field carries the reason. Nothing downstream treats
	// the null as a zero.
	totalUnresolved, countErr := dm.CountContradictionQueue()
	var totalPtr *int
	if countErr == nil {
		total := totalUnresolved
		totalPtr = &total
	}

	// A failed proposal stops here, BEFORE the queue is loaded or any row
	// is resolved. Half a proposal followed by half a review would leave
	// the operator unable to tell which part of the output describes
	// durable state.
	if proposeErr != nil {
		envErr := errorText(proposeErr)
		if countErr != nil {
			envErr = envErr + "; " + errorText(countErr)
		}
		if jsonOutput {
			emitResolveEnvelope(jsonResolveEnvelope{
				QueueSize:             0,
				QueueTotalUnresolved:  totalPtr,
				Applied:               0,
				CloseCalls:            0,
				NeedsAdjudication:     0,
				NeedsAdjudicationRows: []map[string]interface{}{},
				DryRun:                !apply,
				Decisions:             []mpminternal.ResolutionDecision{},
				Propose:               proposeSummaryValue,
				Error:                 &envErr,
			})
		} else {
			// The human path says the same thing the JSON path says: what
			// was scanned, and that the proposal did not complete. An
			// operator reading only stderr would otherwise not know how far
			// the scan got.
			printProposalSummary(proposeSummaryValue)
			usererror.Error("propose candidates: %v", proposeErr)
			if countErr != nil {
				usererror.Error("count queue: %v", countErr)
			}
		}
		return 1
	}

	// The queue could not be counted. The queue lives in the shared
	// schema, so a count that fails means the queue itself is
	// unreachable — the review that follows could not describe it either.
	// The invocation stops here, AFTER the envelope, so a --json consumer
	// still gets its one document.
	if countErr != nil {
		envErr := errorText(countErr)
		if jsonOutput {
			emitResolveEnvelope(jsonResolveEnvelope{
				QueueSize:             0,
				QueueTotalUnresolved:  nil,
				Applied:               0,
				CloseCalls:            0,
				NeedsAdjudication:     0,
				NeedsAdjudicationRows: []map[string]interface{}{},
				DryRun:                !apply,
				Decisions:             []mpminternal.ResolutionDecision{},
				Propose:               proposeSummaryValue,
				Error:                 &envErr,
			})
		} else {
			if propose {
				printProposalSummary(proposeSummaryValue)
			}
			usererror.Error("count queue: %v", countErr)
		}
		return 1
	}

	// Load the unresolved queue. Filter to limit (defensive: if
	// limit is huge, we still cap at defaultResolveLimit * 10 to
	// prevent a runaway transaction).
	if limit > defaultResolveLimit*10 {
		limit = defaultResolveLimit * 10
	}
	queue, err := dm.LoadContradictionQueue(limit)
	if err != nil {
		// The envelope is emitted even though the review could not run.
		// --json promises exactly one document per invocation, and a
		// consumer that received nothing could not tell a failed review
		// from a crash.
		envErr := errorText(err)
		if jsonOutput {
			emitResolveEnvelope(jsonResolveEnvelope{
				QueueSize:             0,
				QueueTotalUnresolved:  totalPtr,
				Applied:               0,
				CloseCalls:            0,
				NeedsAdjudication:     0,
				NeedsAdjudicationRows: []map[string]interface{}{},
				DryRun:                !apply,
				Decisions:             []mpminternal.ResolutionDecision{},
				Propose:               proposeSummaryValue,
				Error:                 &envErr,
			})
		} else {
			if propose {
				printProposalSummary(proposeSummaryValue)
			}
			usererror.Error("load queue: %v", err)
		}
		return 1
	}
	if len(queue) == 0 {
		if jsonOutput {
			// Same key set as the populated branch below. A consumer
			// scripting --json should not have to branch on whether the
			// queue happened to be empty to find out whether
			// needs_adjudication exists.
			emitResolveEnvelope(jsonResolveEnvelope{
				QueueSize:             0,
				QueueTotalUnresolved:  totalPtr,
				Applied:               0,
				CloseCalls:            0,
				NeedsAdjudication:     0,
				NeedsAdjudicationRows: []map[string]interface{}{},
				DryRun:                !apply,
				Decisions:             []mpminternal.ResolutionDecision{},
				Propose:               proposeSummaryValue,
			})
		} else {
			if propose {
				printProposalSummary(proposeSummaryValue)
			}
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
		emitResolveEnvelope(jsonResolveEnvelope{
			QueueSize:             len(queue),
			QueueTotalUnresolved:  totalPtr,
			Applied:               applied,
			CloseCalls:            closeCalls,
			NeedsAdjudication:     pendingAdjudication,
			NeedsAdjudicationRows: pendingRows,
			DryRun:                !apply,
			Decisions:             decisions,
			Propose:               proposeSummaryValue,
		})
	} else {
		if propose {
			printProposalSummary(proposeSummaryValue)
		}
		printResolveDecisions(outcomes, apply, pendingAdjudication)
	}
	return 0
}

// jsonResolveEnvelope is the single JSON document this command emits.
//
// Every field is always present, so a consumer can read the envelope
// without first checking which branch produced it. The empty queue, the
// populated queue and the failure envelope all use this shape.
type jsonResolveEnvelope struct {
	// QueueSize is how many rows this invocation REVIEWED. It is
	// bounded by --limit and by the loser/winner filters.
	QueueSize int `json:"queue_size"`

	// QueueTotalUnresolved is how many unresolved rows the queue holds
	// in total, independent of the review batch. A queue with 4,000
	// unresolved rows reviewed 100 at a time reports 100 here and 4,000
	// there, which is the difference between "the batch is clear" and
	// "the queue is clear".
	//
	// It is a POINTER so that "not known" can be told from "zero". A
	// count that failed leaves it null and populates Error. A zero would
	// be a different claim: it would say the queue is empty.
	QueueTotalUnresolved *int `json:"queue_total_unresolved"`

	Applied               int                              `json:"applied"`
	CloseCalls            int                              `json:"close_calls"`
	NeedsAdjudication     int                              `json:"needs_adjudication"`
	NeedsAdjudicationRows []map[string]interface{}         `json:"needs_adjudication_rows"`
	DryRun                bool                             `json:"dry_run"`
	Decisions             []mpminternal.ResolutionDecision `json:"decisions"`

	// Propose carries the proposal statistics. It is always present.
	Propose proposeSummary `json:"propose"`

	// Error carries the reason this invocation could not do what was
	// asked, or null when it did.
	//
	// It exists because the command emits exactly one document per
	// --json invocation, including on the failure paths. Without it, a
	// consumer could not tell a completed empty review from a run that
	// died before it could describe anything: both would carry a zero
	// count and no rows.
	Error *string `json:"error"`
}

// emitResolveEnvelope writes the one JSON document for an invocation.
func emitResolveEnvelope(env jsonResolveEnvelope) {
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		usererror.Error("render json output: %v", err)
		return
	}
	fmt.Println(string(b))
}

// runBoundedProposal scans the corpus for similarity candidates and
// enqueues them under the configured budgets.
//
// The two phases are deliberately separate, and the boundary between them
// is a refusal boundary rather than a best-effort boundary:
//
//	discovery  bounded by the comparison budget and the candidate cap;
//	           a refusal here persists NOTHING
//	persistence bounded by the write budget; a refusal here persists
//	           NOTHING
//
// There is no path through this function that writes a partial batch.
func runBoundedProposal(dm *mpminternal.DatabaseManager, opts proposeOptions) (proposeSummary, error) {
	summary := proposeSummary{
		Requested:          true,
		ScanLimitPerSource: opts.scanLimit,
		CompareBudget:      opts.compareBudget,
		CandidateCap:       opts.candidateCap,
		WriteBudget:        opts.writeBudget,
		Completion:         "not_run",
		CommitState:        "not_run",
	}

	entries, err := dm.LoadSimilarityScanEntriesFederated(opts.scanLimit)
	if err != nil {
		summary.Completion = "scan_failed"
		summary.Error = strPtr(err.Error())
		return summary, err
	}
	summary.EntriesScanned = len(entries)

	candidates, stats, err := mpminternal.FindSimilarityCandidatesBounded(
		entries, opts.candidateCap, opts.compareBudget)
	summary.PairsExamined = stats.PairsExamined
	summary.CandidatesFoundInScan = stats.CandidatesFound
	summary.CandidatesMaterialised = stats.CandidatesMaterialised

	if err != nil {
		summary.ScanComplete = false
		var compareErr *mpminternal.ErrCompareBudgetExceeded
		if errors.As(err, &compareErr) {
			summary.Completion = "compare_budget"
			summary.Error = strPtr(err.Error())
			return summary, err
		}
		var capErr *mpminternal.ErrCandidateCapReached
		if errors.As(err, &capErr) {
			summary.Completion = "candidate_cap"
			summary.Error = strPtr(err.Error())
			return summary, err
		}
		summary.Completion = "scan_failed"
		summary.Error = strPtr(err.Error())
		return summary, err
	}
	summary.ScanComplete = true

	// Persist the batch atomically. The write budget is enforced inside
	// the transaction, against the unresolved-pair snapshot taken there.
	tx, txErr := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), candidates, opts.writeBudget)
	summary.CandidatesSkippedPending = tx.SkippedPending
	summary.CandidatesSkippedDuplicate = tx.SkippedDuplicate
	summary.CandidatesSkippedConstrained = tx.SkippedConstrained
	summary.CommitState = tx.CommitState.String()

	if txErr != nil {
		// A failed transaction wrote nothing. The count stays zero: an
		// unconfirmed write is not a write, and reporting the number of
		// INSERTs that were attempted would be reporting work that may
		// not be durable.
		summary.CandidatesProposed = 0
		summary.Completion = classifyProposalFailure(tx, txErr)
		summary.Error = strPtr(txErr.Error())
		return summary, txErr
	}

	summary.CandidatesProposed = tx.Inserted
	summary.Completion = "complete"
	return summary, nil
}

// classifyProposalFailure names the failure mode of a proposal
// transaction, so the operator can tell a transient lock from a broken
// insert from an unknown outcome.
//
// The distinction that matters most is between "we know nothing was
// written" and "we cannot prove it". An uncertain outcome is reported as
// uncertain, never as a success and never as a confirmed rollback.
//
// The mapping:
//
//	db_busy           the write lock could not be acquired; nothing written
//	write_budget      the batch was refused before the first INSERT
//	invalid_candidate a candidate failed validation; the batch was refused
//	db_unavailable    the queue could not be reached; nothing was attempted
//	db_insert         an INSERT failed and the rollback was confirmed
//	db_error          another statement failed; the transaction was rolled back
//	rollback          a ROLLBACK failed; the outcome is unknown
//	uncertain_commit  the COMMIT failed; the outcome is unknown
//
// A failed COMMIT whose subsequent ROLLBACK was confirmed still reports
// uncertain_commit: the commit phase is what failed, and the operator's
// next action — look at the queue before trusting it — is the same either
// way. The finer fact is in commit_state.
//
// Every classification describes what ACTUALLY happened. In particular a
// failure that occurred before the first INSERT is never reported as
// db_insert, because that name says an insert failed and a rollback was
// confirmed — two things that did not happen.
func classifyProposalFailure(tx mpminternal.TransactionResult, err error) string {
	var busyErr *mpminternal.ErrDatabaseBusy
	if errors.As(err, &busyErr) || tx.Busy {
		return "db_busy"
	}
	var budgetErr *mpminternal.ErrWriteBudgetExceeded
	if errors.As(err, &budgetErr) {
		return "write_budget"
	}
	// An unusable budget is a write-budget refusal, not a database
	// failure: nothing was written because the authorisation was invalid.
	var invalidBudgetErr *mpminternal.ErrInvalidWriteBudget
	if errors.As(err, &invalidBudgetErr) {
		return "write_budget"
	}
	// The flag is the fallback for a refusal that did not carry a typed
	// error.
	if tx.WriteBudgetHit {
		return "write_budget"
	}
	// A candidate that fails validation refuses the batch before the
	// first INSERT. Reporting it as a database failure would describe a
	// write that was never attempted.
	var invalidCandidateErr *mpminternal.ErrInvalidCandidate
	if errors.As(err, &invalidCandidateErr) {
		return "invalid_candidate"
	}
	// The queue could not be reached at all: the shared schema is not
	// attached, or the database itself could not be opened. Nothing was
	// attempted, so this is not an insert failure and not a rollback.
	var sharedErr *mpminternal.ErrSharedUnavailable
	if errors.As(err, &sharedErr) {
		return "db_unavailable"
	}
	var unavailableErr *mpminternal.ErrDatabaseUnavailable
	if errors.As(err, &unavailableErr) {
		return "db_unavailable"
	}
	var insertionErr *mpminternal.ErrProposalInsertion
	if errors.As(err, &insertionErr) {
		if insertionErr.Phase == "commit" {
			return "uncertain_commit"
		}
		if tx.RollbackError != nil {
			return "rollback"
		}
		return "db_insert"
	}
	if tx.CommitState == mpminternal.CommitStateUncertain {
		return "uncertain_commit"
	}
	// Any other failure inside the transaction. The statement that failed
	// was not an INSERT — it was the in-transaction read of the unresolved
	// pairs, or a driver-level failure — so reporting db_insert would
	// describe a write that never happened. The deferred cleanup rolled the
	// transaction back, so nothing was written.
	return "db_error"
}

// printProposalSummary renders the human-readable proposal line.
//
// It is used on both the success and the failure path, so an operator
// reading only a terminal sees the same picture a JSON consumer sees: how
// far the scan got, and whether anything was written.
//
// The row count is always the CONFIRMED COMMITTED count. On a refusal that
// number is zero, and the completion name says why.
func printProposalSummary(summary proposeSummary) {
	fmt.Printf("Scanned %d memories (%d pairs examined): %d unverified similarity candidate(s), %d new queue row(s).\n",
		summary.EntriesScanned, summary.PairsExamined, summary.CandidatesFoundInScan, summary.CandidatesProposed)
	if summary.CandidatesFoundInScan > 0 {
		fmt.Println("These are RELATED pairs, not established contradictions —")
		fmt.Println("similarity is highest where duplication is highest. Review, then resolve.")
	}
	if summary.Completion != "complete" && summary.Completion != "not_run" {
		// The outcome sentence is driven by the COMMIT STATE, not by the
		// completion name. The completion names the PHASE that failed; the
		// commit state says what that means for the queue.
		//
		// Saying "nothing was written" when the outcome could not be
		// established would be a claim the evidence does not support — and
		// it is the one claim an operator must not be given when the
		// queue's state is unknown.
		switch summary.CommitState {
		case "uncertain":
			fmt.Printf("Proposal did NOT complete (%s). The outcome could not be established — "+
				"verify the queue before trusting it.\n", summary.Completion)
		case "rolled_back":
			fmt.Printf("Proposal did NOT complete (%s). Nothing was written: the transaction "+
				"was rolled back.\n", summary.Completion)
		default:
			fmt.Printf("Proposal did NOT complete (%s). Nothing was written.\n", summary.Completion)
		}
	}
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

  SIMILARITY IS EVIDENCE OF RELATIONSHIP, NOT PROOF OF CONTRADICTION.
  A candidate row is a request to look at a pair. It is not a finding, and
  a scan producing many candidates is not a scan finding many conflicts.

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

REVIEW ORDER
  Queue rows are reviewed OLDEST FIRST. A queue of 4,000 unresolved rows
  reviewed 100 at a time therefore works through the stalest items before
  the freshest ones. The --json envelope reports both numbers:
  queue_size is the batch THIS invocation reviewed, and
  queue_total_unresolved is how many unresolved rows the queue holds in
  total. The two differ whenever the queue is longer than the batch.

  A proposal may add MORE rows than the 100-row review batch holds. The
  review batch is bounded; the WRITE that produced the candidates is
  separately budgeted, and those are different limits. Expect
  queue_total_unresolved to exceed queue_size after a busy scan.

PROPOSAL BUDGETS
  A proposal runs in two bounded phases, and each phase can refuse
  independently:

    scan       --propose-limit=N memories per source (default 100,
                max 2000). Pairs are compared only WITHIN that window,
                so a smaller limit is a cheaper and shallower scan, not a
                sample of the whole store. The limit applies separately
                to local and to shared memories, and duplicates across
                the two are removed before any comparison.

                --compare-budget=N caps how many pairs may be compared
                (default 100,000; range 1,000..10,000,000).

                --candidate-cap=N caps how many candidates may be
                retained (default and maximum 10,000).

    write      --write-budget=N caps how many NEW queue rows may be
                inserted (default and maximum 250).

  A refusal is fail-closed: the scan refuses to materialise a partial
  candidate set, and the write refuses to insert a partial batch. In
  neither case is anything persisted. Raising the scan limit never
  bypasses the comparison, candidate or write budgets.

  --propose-limit=all is NOT supported and is rejected explicitly. Use a
  number.

  A scan that hit a cap describes only the window it actually compared.
  The candidate count in the output is a count of what that bounded pass
  found, NOT a count of the whole corpus.

TRANSACTIONAL SEMANTICS
  The candidate batch is inserted in ONE transaction on a pinned
  connection (BEGIN IMMEDIATE), and the unresolved-pair check is repeated
  INSIDE that transaction. Consequences:

    - The batch is all-or-nothing. An insertion failure rolls the whole
      batch back; no partial batch is left behind.
    - The reported candidate count is the number of CONFIRMED COMMITTED
      rows. A failed or uncertain transaction reports zero, because an
      unconfirmed write is not a write.
    - A pair that is already an unresolved queue row is not re-added.
    - A previously RESOLVED pair may be proposed again, even when its
      evidence has not changed. Re-queueing a resolved pair is intended
      behaviour, not a duplicate bug.

  Concurrency: two writers proposing the same pair at the same time are
  serialised by the write lock, and the second one sees the first one's
  row inside its own transaction.

JSON OUTPUT
  --json prints EXACTLY ONE JSON object per invocation — one envelope,
  never two, on EVERY path including the failure paths. The envelope
  always carries the same keys:

    queue_size               rows reviewed in this batch
    queue_total_unresolved   unresolved rows in the queue in total, or
                              null when the queue could not be counted
                              (null means "not known", not "zero")
    applied                  resolutions applied (0 unless --apply)
    close_calls              near-tie arbitrations proposed
    needs_adjudication       rows awaiting an operator's judgement
    needs_adjudication_rows  those rows, with their queue ids
    dry_run                  true unless --apply
    decisions                per-row resolution decisions
    propose                  nested proposal statistics (always present)
    error                    why this invocation could not do what was
                              asked, or null when it did

  The nested "propose" object reports: requested, scan_limit_per_source,
  entries_scanned, pairs_examined, candidates_found_in_scan,
  scan_complete, candidates_materialised, candidates_proposed,
  candidates_skipped_pending, candidates_skipped_duplicate,
  candidates_skipped_constrained, compare_budget, candidate_cap,
  write_budget, completion and commit_state.

  "completion" names the outcome precisely:
    not_run            --propose was not passed
    complete           the scan and the write both succeeded
    compare_budget     the scan was refused by the comparison budget
    candidate_cap      the scan found more candidates than it may retain
    write_budget       the batch had more new pairs than it may insert
    invalid_candidate  a candidate failed validation; the batch was refused
    db_unavailable     the queue could not be reached; nothing was attempted
    db_busy            another writer held the lock
    db_insert          an insertion failed and the rollback was confirmed
    db_error           another statement failed; the transaction was rolled back
    rollback           a rollback itself failed; the outcome is unknown
    uncertain_commit   the commit failed; the outcome is unknown
    scan_failed        the scan could not read the corpus

  "commit_state" is the authoritative statement of what happened to the
  queue: refused, rolled_back, uncertain or confirmed. "uncertain" means
  the outcome could not be established and the queue must be verified.

  On any proposal failure the command exits NON-ZERO and stops before
  reviewing or resolving anything. A half-proposal followed by a
  half-review would leave the operator unable to tell which part of the
  output describes durable state.

Flags:
  --propose            Scan live memories and enqueue unverified similarity
                       candidates before reviewing. THIS WRITES TO THE
                       QUEUE, independent of --apply. Omit it and nothing
                       is ever added.
                       NOTE: --propose --dry-run STILL INSERTS PROPOSAL
                       ROWS. --dry-run governs the RESOLUTION phase.
  --propose-limit=N    Memories examined per source by --propose
                       (default 100, max 2000). "all" is rejected.
                       Requires --propose.
  --compare-budget=N   Max pairs compared by --propose
                       (default 100000, range 1000..10000000).
                       Requires --propose.
  --candidate-cap=N    Max candidates retained by --propose
                       (default 10000, max 10000). Requires --propose.
  --write-budget=N     Max new queue rows inserted by --propose
                       (default 250, max 250). Requires --propose.
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

REJECTED COMBINATIONS
  --propose with --apply        refused before any database access
  --propose with --adjudicate   refused before any database access
  --propose-limit / --compare-budget / --candidate-cap / --write-budget
  without --propose              refused before any database access

Examples:
  mpm ops resolve-contradictions                       # dry-run, 100 rows
  mpm ops resolve-contradictions --propose              # scan + enqueue, then review
  mpm ops resolve-contradictions --propose --propose-limit=500
  mpm ops resolve-contradictions --json                 # find queue row ids
  mpm ops resolve-contradictions --adjudicate=42:"mem-a claims X, mem-b claims not-X"
  mpm ops resolve-contradictions --apply                # apply adjudicated rows
  mpm ops resolve-contradictions --apply --limit=50     # apply first 50
  mpm ops resolve-contradictions --loser=mem-abc-123    # narrow scope
  mpm ops resolve-contradictions --json --apply         # CI-friendly output`)
}
