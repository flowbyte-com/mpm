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

import "fmt"

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
