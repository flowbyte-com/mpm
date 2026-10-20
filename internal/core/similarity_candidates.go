// similarity_candidates.go — discovery of UNVERIFIED similarity candidates.
//
// What this file is
//
// A pair of memories scoring high on cosine similarity is RELATED. That is
// not evidence that the two statements CONTRADICT each other: near-identical
// duplicates — the single most common case in a real memory store — score
// at the very top of that range. Treating a similarity score as a logical
// contradiction is a category error, and acting on it is what previously
// penalised 40 members of a single 75-row near-identical cluster that all
// asserted the same claim.
//
// So this file deliberately does not speak of contradictions. It produces
// SimilarityCandidates: pairs a human may want to look at. Nothing here
// asserts that a logical conflict exists, and nothing here decides one.
//
// Why it is a pure function
//
// FindSimilarityCandidates touches no database. It takes values and returns
// values. That is the whole point of the split:
//
//   - ordinary retrieval can no longer persist candidates, because
//     persistence is not this function's job and the only code that writes
//     them is an explicitly invoked operator command;
//   - the discovery logic is testable without a database, an embedding
//     provider, or a shared attachment;
//   - "did retrieval change anything?" has an unambiguous answer, because
//     the discovery path cannot write even if a future caller reaches it.
//
// The threshold below is the pre-existing constant from the old Phase 4
// scan, unchanged and not re-tuned. It is a REVIEW-WORKLIST selector, not a
// contradiction test: it answers "is this pair close enough to be worth a
// human's attention", and it makes no claim beyond that.

package internal

import (
	"fmt"
	"math"
)

// ── bounded discovery (issue #21) ──────────────────────────────────
//
// FindSimilarityCandidates above is the original, unbounded, pure
// discovery function. It is preserved verbatim: it has existing callers
// and existing tests, and the semantics it encodes — threshold,
// structural-prefix discount, classification, deterministic ordering —
// are not what issue #21 asked to change.
//
// What issue #21 asked for is a BOUNDED variant that an operator-facing
// command can call without the risk that "the corpus grew" becomes "the
// command takes an unbounded amount of work and writes an unbounded
// number of queue rows".
//
// The three budgets and what each one actually bounds:
//
//	compare budget   a TIME bound. Checked once, before any pair is
//	                 evaluated. Refuses the pass outright if the number of
//	                 pairs the scan WOULD evaluate exceeds the budget.
//	candidate cap    an ALLOCATION bound. Limits how many candidates are
//	                 retained. It is NOT a time bound: the pair loop keeps
//	                 running past the cap so the statistics stay truthful,
//	                 which means the cap does not by itself make a scan
//	                 fast.
//	write budget     a PERSISTENCE bound, enforced by the store layer
//	                 (similarity_candidates_store.go), not here.
//
// The distinction between the first two matters and is easy to get
// wrong. A cap on retained allocations says nothing about how long the
// scan takes, and a document that describes it as a runtime guarantee is
// making a claim the code does not support.

const (
	// DefaultCompareBudget is the default number of pair comparisons a
	// single bounded scan may evaluate.
	//
	// The default scan window is 100 memories per source, so the union is
	// at most 200 entries and C(200,2) = 19,900 pairs — about a fifth of
	// this budget. The headroom absorbs the local/shared overlap (a
	// memory present on both sides is deduped by ID, so it never
	// contributes twice) and an operator who raises the scan limit
	// without raising the comparison budget.
	DefaultCompareBudget = 100_000

	// MinCompareBudget is the smallest comparison budget an operator may
	// request. Below this the entry check would refuse almost every
	// possible scan window, which means the operator asked for something
	// that cannot succeed.
	MinCompareBudget = 1_000

	// MaxCompareBudget is the hard ceiling on the comparison budget.
	//
	// Ten million pairs is roughly 7.7e10 elementary operations at 768
	// dimensions, which is seconds rather than minutes on any hardware
	// this has run on. It is far enough above the default to allow a
	// deliberate deep scan and far enough below "unbounded" that the
	// number reads as a mistake when an operator types it.
	MaxCompareBudget = 10_000_000

	// DefaultCandidateCap is the default maximum number of candidates a
	// bounded scan may materialise.
	//
	// This is a ceiling on retained allocations, not on execution time.
	// Exceeding it fails the pass closed — the caller must not persist
	// anything from an incomplete discovery pass.
	DefaultCandidateCap = 10_000

	// MaxCandidateCap is the hard ceiling on the candidate cap. The cap
	// may be lowered by an operator who wants a narrower result set, but
	// it may not be raised: the number exists to bound how much a single
	// scan can retain, and a larger one would not bound anything.
	MaxCandidateCap = 10_000
)

// CandidateDiscoveryStats describes what one bounded discovery pass
// examined.
//
// The counters are internally consistent by construction:
//
//	PairsExamined == PairsSkippedInvalid + PairsBelowThreshold + PairsAboveThreshold
//	PairsAboveThreshold == CandidatesFound + PairsDeclined
//
// The first identity says every pair the pass entered is accounted for
// exactly once. The second says every pair that cleared the threshold is
// either a candidate or a pair the classifier declined.
//
// A count that does not add up is worse than no count at all: an
// operator reading "19,900 pairs examined, 900 above threshold, 123
// candidates" needs those three numbers to be describing the same
// population, or the report is describing several different populations
// with one vocabulary.
type CandidateDiscoveryStats struct {
	// EntriesConsidered is the number of scan entries offered to the pass.
	EntriesConsidered int

	// EntriesSkipped is the number of entries that carried no usable
	// embedding and therefore took part in no comparison. Each such
	// entry is counted once, not once per pair it would have joined.
	EntriesSkipped int

	// PairsExamined counts the pairs the pass actually evaluated.
	PairsExamined int

	// PairsSkippedInvalid counts pairs the pass entered but could not
	// compare — mismatched dimensions, or a vector containing a
	// non-finite value.
	PairsSkippedInvalid int

	// PairsBelowThreshold counts comparable pairs whose discounted cosine
	// fell below ContradictionCandidateThreshold.
	PairsBelowThreshold int

	// PairsAboveThreshold counts comparable pairs at or above the
	// threshold.
	PairsAboveThreshold int

	// PairsDeclined counts above-threshold pairs the classifier declined
	// to emit. It is zero today: classifyPair always returns a candidate.
	// It is tracked separately so that a future branch which declines
	// cannot quietly break the accounting.
	PairsDeclined int

	// CandidatesFound counts pairs that classified into a candidate,
	// whether or not they were retained.
	CandidatesFound int

	// CandidatesMaterialised counts candidates retained in memory.
	//
	// On the cap-refusal path the returned slice is nil and this count is
	// the number retained BEFORE the refusal. That is why the pass refuses
	// rather than returning a truncated slice: a partial slice with a
	// count beside it reads as a complete answer.
	CandidatesMaterialised int

	// EstimatedPairs is C(n,2) over the entries offered — an UPPER BOUND on
	// the number of pairs the pass would evaluate.
	//
	// It is an upper bound rather than an exact count because an entry with
	// no usable embedding takes part in no comparison, so the pass may
	// examine fewer pairs than this. The comparison budget is checked
	// against this bound, and that is what makes the refusal independent
	// of how many pairs would actually have qualified.
	EstimatedPairs int64

	// CompareBudget is the comparison budget in force for this pass.
	CompareBudget int

	// CandidateCap is the materialisation cap in force for this pass.
	CandidateCap int

	// CompareBudgetHit is true when the pass was refused before any pair
	// was evaluated.
	CompareBudgetHit bool

	// CandidateCapHit is true when the materialisation cap was reached.
	CandidateCapHit bool
}

// ErrCompareBudgetExceeded is returned when a bounded scan would evaluate
// more pairs than its comparison budget allows.
//
// It is a distinct type rather than a plain error because the caller has
// two genuinely different responses to it: report the refusal and persist
// nothing, or ask the operator for a larger budget. Both are legitimate;
// silently proceeding with a partial scan is not.
type ErrCompareBudgetExceeded struct {
	// Entries is the number of scan entries the pass was offered.
	Entries int

	// EstimatedPairs is C(Entries,2) — the number of pairs the pass would
	// have evaluated.
	EstimatedPairs int64

	// CompareBudget is the budget that was exceeded.
	CompareBudget int
}

func (e *ErrCompareBudgetExceeded) Error() string {
	return fmt.Sprintf("similarity scan refused: %d entries would require %d pair comparisons, "+
		"which exceeds the comparison budget of %d. Raise --compare-budget (up to %d) "+
		"or lower --propose-limit.",
		e.Entries, e.EstimatedPairs, e.CompareBudget, MaxCompareBudget)
}

// ErrCandidateCapReached is returned when a bounded scan found more
// qualifying candidates than the materialisation cap allows.
//
// The pass is refused rather than truncated: a truncated result would be
// an arbitrary subset of the candidates, and persisting an arbitrary
// subset while reporting the whole number is exactly the "incomplete
// count reported as an exact total" failure the contract forbids.
type ErrCandidateCapReached struct {
	// CandidatesFound is how many qualifying candidates the pass found.
	CandidatesFound int

	// CandidateCap is the cap that was reached.
	CandidateCap int

	// PairsExamined is how many pairs the pass evaluated.
	PairsExamined int
}

func (e *ErrCandidateCapReached) Error() string {
	return fmt.Sprintf("similarity scan refused: %d qualifying candidates exceed the materialisation "+
		"cap of %d (after examining %d pairs). Narrow the scan window with --propose-limit "+
		"or raise --candidate-cap (up to %d).",
		e.CandidatesFound, e.CandidateCap, e.PairsExamined, MaxCandidateCap)
}

// FindSimilarityCandidatesBounded is the budgeted form of
// FindSimilarityCandidates.
//
// Same discovery semantics — same threshold, same structural-prefix
// discount, same classification, same deterministic ordering — with two
// budgets applied and statistics returned.
//
// The comparison budget is checked ONCE, before any pair is evaluated.
// It is compared against C(n,2) over the entries offered, so the refusal
// is independent of how many pairs would actually have qualified: a
// corpus in which every pair scores above the threshold is refused
// exactly like a corpus in which none do. That is what makes the budget
// effective regardless of candidate density.
//
// The candidate cap bounds retained allocations only. The pair loop
// continues past the cap so that the statistics describe the whole
// scanned window rather than the truncated prefix, and so that
// CandidatesFound is a real count instead of an artefact of where the
// loop stopped. The cost of that choice is that the cap does not make
// the scan faster; the comparison budget does that, and it is checked
// first.
//
// On either refusal the returned candidate slice is nil. There is
// deliberately no way to receive a partial result: a caller that
// persisted candidates from an incomplete discovery pass would be
// persisting a number it cannot explain.
//
// Both budgets fail CLOSED. A non-positive comparison budget refuses the
// pass: with two or more entries there is at least one pair to compare,
// and a budget of zero permits none. A non-positive candidate cap refuses
// the pass as soon as a qualifying candidate is found, because a cap of
// zero permits no candidate to be retained; a window that contains no
// qualifying candidate satisfies such a cap and is not a refusal. A
// budget is a bound on what the pass may do, and a bound that cannot be
// met is a refusal — never an instruction to proceed without one.
func FindSimilarityCandidatesBounded(entries []ScanEntry, candidateCap, compareBudget int) ([]SimilarityCandidate, CandidateDiscoveryStats, error) {
	stats := CandidateDiscoveryStats{
		EntriesConsidered: len(entries),
		CompareBudget:     compareBudget,
		CandidateCap:      candidateCap,
	}

	n := len(entries)
	if n < 2 {
		// Nothing to compare. Not a refusal: an empty or single-entry
		// window is a complete scan that found nothing.
		return nil, stats, nil
	}

	// Overflow-safe pair count.
	//
	// n is the length of a slice the caller already holds in memory, so
	// it is bounded by RAM rather than by anything an operator can type.
	// int64 arithmetic is therefore safe: n*(n-1) overflows int64 only
	// above roughly 3e9 entries, which no in-memory slice reaches.
	estimated := int64(n) * int64(n-1) / 2
	stats.EstimatedPairs = estimated

	// A non-positive budget is not an unlimited budget. With two or more
	// entries there is at least one pair to compare, and a budget of zero
	// permits none — so the comparison fails closed. The `> 0` conjunct
	// that used to guard this check would have made a budget of zero mean
	// "compare everything", which is the opposite of what a budget of zero
	// says.
	if estimated > int64(compareBudget) {
		stats.CompareBudgetHit = true
		return nil, stats, &ErrCompareBudgetExceeded{
			Entries:        n,
			EstimatedPairs: estimated,
			CompareBudget:  compareBudget,
		}
	}

	var out []SimilarityCandidate
	for i := 0; i < n; i++ {
		ci := entries[i]
		if len(ci.Embedding) == 0 {
			// Counted once per entry, here. The inner loop skips the same
			// entry silently, because counting it per pair would count one
			// entry many times.
			stats.EntriesSkipped++
			continue
		}
		for j := i + 1; j < n; j++ {
			cj := entries[j]
			if len(cj.Embedding) == 0 {
				continue
			}
			stats.PairsExamined++

			// Comparability is checked explicitly rather than relying on
			// cosineSimilarity's 0 return for mismatched dimensions, which
			// a future refactor could change without anyone noticing that a
			// write path now hangs off an unvalidated score.
			if !validEmbeddingPair(ci.Embedding, cj.Embedding) {
				stats.PairsSkippedInvalid++
				continue
			}
			sim := cosineSimilarity(ci.Embedding, cj.Embedding)
			// Structural-prefix discount: when both memories open with the
			// same template marker ("CHOICE:", "## ", "Fact:", …) the
			// cosine is inflated by shared tokens rather than by the claims
			// themselves. Halve it before comparing to the threshold.
			if IsStructuralPrefix(ci.Content) && IsStructuralPrefix(cj.Content) {
				sim = sim * 0.5
			}
			if sim < ContradictionCandidateThreshold {
				stats.PairsBelowThreshold++
				continue
			}
			stats.PairsAboveThreshold++

			c := classifyPair(ci, cj, sim)
			if c == nil {
				stats.PairsDeclined++
				continue
			}
			stats.CandidatesFound++

			// The cap bounds retained allocations, not iteration. Keep
			// counting so the statistics stay truthful, and refuse the
			// pass at the end.
			//
			// A cap of zero permits no candidate to be retained, so the
			// first qualifying candidate refuses the pass. The `> 0`
			// conjunct that used to guard this check would have made a cap
			// of zero mean "retain everything", which is the opposite of
			// what a cap of zero says.
			if len(out) >= candidateCap {
				stats.CandidateCapHit = true
				continue
			}
			out = append(out, *c)
			stats.CandidatesMaterialised = len(out)
		}
	}

	if stats.CandidateCapHit {
		// Discard the partial result. The caller must not persist
		// candidates from an incomplete discovery pass.
		return nil, stats, &ErrCandidateCapReached{
			CandidatesFound: stats.CandidatesFound,
			CandidateCap:    candidateCap,
			PairsExamined:   stats.PairsExamined,
		}
	}
	return out, stats, nil
}

// comparePairCount returns C(n,2) as an int64 without overflow for any n a
// caller can hold in memory.
//
// Used by comparePairCountForSources to size a window the caller has NOT
// yet allocated, and by the tests that pin the pair-count arithmetic. The
// CLI does not call it: the comparison budget is enforced inside the pass
// itself, against the entries the pass was actually given.
func comparePairCount(n int) int64 {
	if n < 2 {
		return 0
	}
	return int64(n) * int64(n-1) / 2
}

// comparePairCountForSources returns the pair count implied by scanning
// limit entries per source across the given number of sources, after
// deduplication by memory ID.
//
// The union is bounded by 2*limit because a memory present on both sides
// is counted once. Using the upper bound is deliberate: the budget check
// must not be satisfiable by an assumption about how much overlap the
// corpus has.
func comparePairCountForSources(limit, sources int) int64 {
	if limit < 0 {
		limit = 0
	}
	if sources < 0 {
		sources = 0
	}
	// The product is computed in int64 so the multiply cannot wrap, and the
	// clamp happens BEFORE the narrowing to int. Clamping after an int
	// multiply would be clamping the overflowed value rather than the
	// product.
	union := int64(limit) * int64(sources)
	if union > math.MaxInt32 {
		// Cannot happen for any limit this codebase accepts, but the
		// narrowing to int below would truncate on a 32-bit platform.
		union = math.MaxInt32
	}
	return comparePairCount(int(union))
}

// ContradictionCandidateThreshold is the cosine at or above which a pair is
// surfaced for human review.
//
// It is deliberately NOT named for contradiction detection, because it does
// not detect contradiction. It is the pre-existing 0.85 constant carried over
// from the old Phase 4 scan so that changing this file does not silently
// change which pairs reach an operator. Re-tuning it is a product decision
// about review throughput, not a correctness fix.
const ContradictionCandidateThreshold = 0.85

// CandidateKind identifies WHY a pair surfaced. It describes the observed
// relationship, never a verdict about logical conflict.
type CandidateKind string

const (
	// CandidateStateCollision: one memory is already flagged `challenged`
	// and the other is not. The flag is pre-existing database state; this
	// scan never sets it.
	CandidateStateCollision CandidateKind = "state_collision"

	// CandidateProvenanceTier: the two memories sit at different
	// provenance tiers. Tier is a tiebreaker for who should win a dispute
	// IF one exists — it is not evidence that one does.
	CandidateProvenanceTier CandidateKind = "provenance_tier"

	// CandidateEqualTier: same tier, neither flagged. The weakest signal
	// of the three, and the branch the old code used to fire purely on
	// rank position.
	CandidateEqualTier CandidateKind = "equal_tier"
)

// SimilarityCandidate is an UNVERIFIED pair offered for human review.
//
// The zero value is not meaningful; construct via FindSimilarityCandidates.
type SimilarityCandidate struct {
	// MemoryA and MemoryB are sorted lexicographically so that (A,B) and
	// (B,A) collapse to one queue key. See EnqueueContradiction.
	MemoryA string
	MemoryB string

	// Similarity is the cosine after the structural-prefix discount.
	Similarity float64

	// Kind is the observed relationship, not a verdict.
	Kind CandidateKind

	// Evidence is a human-readable description of the observed
	// relationship. It never states that a contradiction was found,
	// because none was established.
	Evidence string
}

// ScanEntry is the minimum a scan needs about one memory. Supplying these
// directly (rather than passing HybridResult) keeps discovery independent of
// the retrieval pipeline and lets tests drive it with synthetic vectors.
type ScanEntry struct {
	ID           string
	Content      string
	Embedding    []float32
	IsChallenged bool
	MetadataJSON string
}

// provenanceTierFor ranks a provenance tier: absolute > high > standard >
// ephemeral. Unrecognised values default to standard, matching the old
// in-line closure in Phase 4.
func provenanceTierFor(compute string) int {
	switch compute {
	case "absolute":
		return 4
	case "high":
		return 3
	case "standard":
		return 2
	case "ephemeral":
		return 1
	default:
		return 2
	}
}

// FindSimilarityCandidates returns the unverified candidate pairs among
// entries, scored by cosine similarity.
//
// Pure: no database access, no writes, no side effects. Callers decide what
// to do with the result — including nothing at all, which is what ordinary
// retrieval now does.
//
// Three properties this function must keep:
//
//  1. It never mutates. There is no path from here to ChallengeMemory or to
//     the contradiction queue.
//  2. It never asserts contradiction. Every string it produces says what was
//     observed, and leaves the judgement to the reader.
//  3. It is deterministic. The same entries always yield the same candidates
//     in the same order, so a repeated run is diffable.
func FindSimilarityCandidates(entries []ScanEntry) []SimilarityCandidate {
	var out []SimilarityCandidate

	for i := 0; i < len(entries); i++ {
		ci := entries[i]
		if len(ci.Embedding) == 0 {
			continue
		}
		for j := i + 1; j < len(entries); j++ {
			cj := entries[j]
			if len(cj.Embedding) == 0 {
				continue
			}
			// Comparability is checked explicitly rather than relying on
			// cosineSimilarity's 0 return for mismatched dimensions, which
			// a future refactor could change without anyone noticing that a
			// write path now hangs off an unvalidated score.
			if !validEmbeddingPair(ci.Embedding, cj.Embedding) {
				continue
			}
			sim := cosineSimilarity(ci.Embedding, cj.Embedding)
			// Structural-prefix discount: when both memories open with the
			// same template marker ("CHOICE:", "## ", "Fact:", …) the
			// cosine is inflated by shared tokens rather than by the claims
			// themselves. Halve it before comparing to the threshold.
			if IsStructuralPrefix(ci.Content) && IsStructuralPrefix(cj.Content) {
				sim = sim * 0.5
			}
			if sim < ContradictionCandidateThreshold {
				continue
			}
			if c := classifyPair(ci, cj, sim); c != nil {
				out = append(out, *c)
			}
		}
	}
	return out
}

// classifyPair labels one above-threshold pair according to the observed
// relationship. Returns nil when the pair matches no branch — currently
// impossible, but keeping the nil case means a future branch that declines
// to emit cannot panic the scan.
func classifyPair(ci, cj ScanEntry, sim float32) *SimilarityCandidate {
	ciTier := provenanceTierFor(extractProvenanceCompute(ci.MetadataJSON))
	cjTier := provenanceTierFor(extractProvenanceCompute(cj.MetadataJSON))

	// Normalise to lexicographic order once, here, so every branch and the
	// eventual queue row name the pair the same way regardless of which
	// side won the loop ordering.
	a, b := ci.ID, cj.ID
	ta, tb := ciTier, cjTier
	chalA, chalB := ci.IsChallenged, cj.IsChallenged
	if a > b {
		a, b = b, a
		ta, tb = tb, ta
		chalA, chalB = chalB, chalA
	}

	switch {
	case chalA != chalB:
		// The flagged memory names the first slot so the operator sees the
		// row that already carries state.
		flagged, unflagged := a, b
		if !chalA {
			flagged, unflagged = b, a
		}
		return &SimilarityCandidate{
			MemoryA:    flagged,
			MemoryB:    unflagged,
			Similarity: float64(sim),
			Kind:       CandidateStateCollision,
			Evidence: fmt.Sprintf(
				"unverified similarity candidate (cosine=%.2f): memory %s already carries a challenge flag, %s does not",
				sim, flagged, unflagged),
		}

	case ta != tb:
		higher, lower := a, b
		hiTier, loTier := ta, tb
		if hiTier < loTier {
			higher, lower = lower, higher
			hiTier, loTier = loTier, hiTier
		}
		return &SimilarityCandidate{
			MemoryA:    higher,
			MemoryB:    lower,
			Similarity: float64(sim),
			Kind:       CandidateProvenanceTier,
			Evidence: fmt.Sprintf(
				"unverified similarity candidate (cosine=%.2f): %s sits at provenance tier %d, %s at tier %d; tier orders a dispute IF one exists",
				sim, higher, hiTier, lower, loTier),
		}

	default:
		return &SimilarityCandidate{
			MemoryA:    a,
			MemoryB:    b,
			Similarity: float64(sim),
			Kind:       CandidateEqualTier,
			Evidence: fmt.Sprintf(
				"unverified similarity candidate (cosine=%.2f) between equal-tier, unflagged memories %s and %s",
				sim, a, b),
		}
	}
}
