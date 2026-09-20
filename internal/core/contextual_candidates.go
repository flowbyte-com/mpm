// contextual_candidates.go — Stage 2D deterministic candidate
// generation. Reads authoritative state, derives a bounded candidate
// set with structural reasons. NO ranking. NO LLM. NO embeddings.
// NO persisted derived state.
//
// Architectural placement
// ────────────────────────
//
// MPM's Projection Principle: authoritative state is persisted;
// views are derived at read time. This file is the read-time view
// layer for "what might matter to the current agent." It composes
// existing substrate surfaces (works, handoffs, recent_activity,
// theories, decisions, lessons, scratchpad, wakes, cascades,
// supersession chains) into a deterministic candidate set. Future
// stages can rank the result without re-querying substrate.
//
// The function `GenerateContextualCandidates` is the single public
// entry. Everything else is internal: per-source bounded helpers,
// deterministic dedup, structural reason vocabulary.
//
// Observational guarantee: GenerateContextualCandidates does not
// mutate any persistent state. It may be called repeatedly with
// identical inputs and must produce identical outputs.

package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ── Public input / output ────────────────────────────────────────────

// ContextQuery is the bounded representation of "what is this agent
// doing right now" used as input to candidate generation. All
// fields are optional; the generator works under partial context.
//
//   - MPMSessionID       : canonical MPM-owned session identity
//   - FrameworkSessionID : host-owned session identity
//   - FrameworkName      : e.g. "claude-code", "openclaw", "mpm-cli"
//   - WorkIDs            : active/open work the agent is reasoning about
//   - TopicIDs           : topics the agent is reasoning about
//   - ArtifactIDs        : explicit artifact pointers supplied by caller
//   - QueryText          : optional task text (NOT parsed in Stage 2D;
//                          carried for the next-stage ranking stage)
//   - Limits             : per-source + global bounds; zero values use
//                          defaults from DefaultCandidateLimits
type ContextQuery struct {
	MPMSessionID       string   `json:"mpm_session_id,omitempty"`
	FrameworkSessionID string   `json:"framework_session_id,omitempty"`
	FrameworkName      string   `json:"framework_name,omitempty"`
	WorkIDs            []string `json:"work_ids,omitempty"`
	TopicIDs           []string `json:"topic_ids,omitempty"`
	ArtifactIDs        []string `json:"artifact_ids,omitempty"`
	QueryText          string   `json:"query_text,omitempty"`
	Limits             CandidateLimits `json:"limits,omitempty"`
}

// CandidateLimits bounds per-source and total candidate counts. A
// zero value uses DefaultCandidateLimits.
type CandidateLimits struct {
	Work       int `json:"work,omitempty"`        // active work + recent
	Handoff    int `json:"handoff,omitempty"`     // latest relevant
	Activity   int `json:"activity,omitempty"`    // recent semantic
	Epistemic  int `json:"epistemic,omitempty"`   // theory/decision/lesson/evidence
	Cascade    int `json:"cascade,omitempty"`     // cascade obligations
	Wake       int `json:"wake,omitempty"`        // overdue + pending
	Scratchpad int `json:"scratchpad,omitempty"`  // active scratchpad items
	Topic      int `json:"topic,omitempty"`       // topic-bounded neighbors
	Global     int `json:"global,omitempty"`      // total cap after dedup
}

// DefaultCandidateLimits returns the canonical bounded defaults for
// Stage 2D. Tuned for a wake-context-friendly output (≤ ~50 dedup
// candidates, with per-source caps that preserve diversity).
func DefaultCandidateLimits() CandidateLimits {
	return CandidateLimits{
		Work:       8,
		Handoff:    3,
		Activity:   8,
		Epistemic:  8,
		Cascade:    4,
		Wake:       4,
		Scratchpad: 4,
		Topic:      4,
		Global:     50,
	}
}

// Candidate is one element of the bounded candidate set. Every
// candidate carries at least one reason (explainability is
// mandatory). Materialization is deferred — candidates carry
// metadata + pointers, not full content.
type Candidate struct {
	// ID is the deterministic merge key: kind + artifact_id. Same
	// artifact discovered through multiple paths collapses to one
	// candidate with merged reasons.
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"` // memory, lesson, theory, decision, evidence, work, handoff, scratchpad, wake, activity
	ArtifactID         string   `json:"artifact_id"`
	Pointer            string   `json:"pointer,omitempty"`
	Timestamp          int64    `json:"timestamp,omitempty"`
	ActorKind          string   `json:"actor_kind,omitempty"`
	FrameworkName      string   `json:"framework_name,omitempty"`
	MPMSessionID       string   `json:"mpm_session_id,omitempty"`
	FrameworkSessionID string   `json:"framework_session_id,omitempty"`
	// Reasons is the bounded vocabulary explaining why this
	// candidate is potentially relevant. Order is deterministic
	// (sorted by CandidateReasonName).
	Reasons []string `json:"reasons"`
	// RelatedIDs lists artifacts structurally linked to this
	// candidate (dependencies, supersession chain, work
	// references, etc.). NOT a graph walk — at most one-hop
	// expansion per the Stage 2D brief.
	RelatedIDs []string `json:"related_ids,omitempty"`
	// LifecycleState captures the artifact's current lifecycle
	// tag (open/done for work, resolved/disproven for theory,
	// superseded/invalidated for decision, etc.). Empty if N/A.
	LifecycleState string `json:"lifecycle_state,omitempty"`
	// Source identifies which generator produced this candidate
	// (work, handoff, activity, epistemic, cascade, wake,
	// scratchpad, topic). Useful for debugging.
	Source string `json:"source"`
	// Summary is a bounded, secret-safe structural summary
	// suitable for diagnostic rendering. Length-capped.
	Summary string `json:"summary,omitempty"`
}

// CandidateGenerationResult is the public output. Sources is the
// per-source pre-dedup count (debug aid); Candidates is the
// post-dedup bounded set; Diagnostics captures generation metadata.
type CandidateGenerationResult struct {
	Candidates  []Candidate                `json:"candidates"`
	Sources     map[string]int             `json:"sources"`
	Diagnostics CandidateGenerationDiag    `json:"diagnostics"`
}

// CandidateGenerationDiag captures metadata about the generation run.
type CandidateGenerationDiag struct {
	GeneratedAt       int64  `json:"generated_at"`
	InputMpmSession   string `json:"input_mpm_session,omitempty"`
	InputFwSession    string `json:"input_framework_session,omitempty"`
	InputWorkIDs      int    `json:"input_work_ids"`
	InputTopicIDs     int    `json:"input_topic_ids"`
	InputArtifactIDs  int    `json:"input_artifact_ids"`
	LimitsUsed        CandidateLimits `json:"limits_used"`
	TotalRaw          int    `json:"total_raw"`
	TotalAfterDedup   int    `json:"total_after_dedup"`
	GlobalCapApplied  bool   `json:"global_cap_applied"`
}

// ── Reason vocabulary ───────────────────────────────────────────────

// CandidateReasonName is the typed bounded vocabulary of reasons
// that explain why a candidate exists. New reasons must be added
// here; downstream tools and tests key off these names.
type CandidateReasonName string

const (
	// ── Continuity ──
	ReasonSameMPMSession       CandidateReasonName = "same_mpm_session"
	ReasonSameFrameworkSession CandidateReasonName = "same_framework_session"
	ReasonHandoffForContext   CandidateReasonName = "handoff_for_current_context"
	ReasonOpenWork             CandidateReasonName = "open_work"
	ReasonRecentlyChangedWork  CandidateReasonName = "work_recently_changed"

	// ── Structural ──
	ReasonReferencedByActiveWork CandidateReasonName = "referenced_by_active_work"
	ReasonSharesTopic            CandidateReasonName = "shares_topic"
	ReasonExplicitDependency     CandidateReasonName = "explicit_dependency"
	ReasonDependsOnCurrent       CandidateReasonName = "depends_on_current_artifact"
	ReasonCurrentDependsOn       CandidateReasonName = "current_artifact_depends_on_candidate"
	ReasonExplicitReference      CandidateReasonName = "explicit_reference"

	// ── Epistemic ──
	ReasonFoundationSuperseded CandidateReasonName = "foundation_superseded"
	ReasonFoundationInvalidated CandidateReasonName = "foundation_invalidated"
	ReasonConfidenceChanged     CandidateReasonName = "confidence_changed"
	ReasonEvidenceAdded         CandidateReasonName = "evidence_added"
	ReasonTheoryResolved        CandidateReasonName = "theory_resolved"
	ReasonTheoryContradicted    CandidateReasonName = "theory_contradicted"
	ReasonDecisionSuperseded    CandidateReasonName = "decision_superseded"
	ReasonCascadePending        CandidateReasonName = "cascade_pending"
	ReasonCascadeResolved       CandidateReasonName = "cascade_resolved"

	// ── Temporal / activity ──
	ReasonRecentCrossAgentChange CandidateReasonName = "recent_cross_agent_change"
	ReasonRecentHumanChange      CandidateReasonName = "recent_human_change"
	ReasonUnknownSourceChange    CandidateReasonName = "unknown_source_change"

	// ── Obligation ──
	ReasonOverdueWake           CandidateReasonName = "overdue_wake"
	ReasonUnresolvedWake        CandidateReasonName = "unresolved_wake"
	ReasonOpenCommitment        CandidateReasonName = "open_commitment"
	ReasonUnresolvedQuestion    CandidateReasonName = "unresolved_question"
	ReasonIncompleteWork        CandidateReasonName = "incomplete_work"

	// ── Scratchpad / working ──
	ReasonActiveScratchpad    CandidateReasonName = "active_scratchpad"
	ReasonPromotedScratchpad  CandidateReasonName = "promoted_scratchpad"
	ReasonCurrentWorkEvidence CandidateReasonName = "current_work_evidence"

	// ── Provenance ──
	ReasonProvenanceUnknown CandidateReasonName = "provenance_unknown"
)

// ── Public entry ────────────────────────────────────────────────────

// GenerateContextualCandidates produces the deterministic bounded
// candidate set for the given query. The result is read-only; no
// table is mutated. Caller MUST NOT rely on this function for any
// ranking — every candidate is equally weighted by definition.
//
// Identity plumbing (Stage 2C/2C.1/2C.2) ensures session_id,
// mpm_session_id, framework_session_id, invocation_id,
// parent_invocation_id are independent authoritative axes; this
// function reads them as facts and never collapses one into
// another.
func (dm *DatabaseManager) GenerateContextualCandidates(q ContextQuery) (CandidateGenerationResult, error) {
	if dm == nil {
		return CandidateGenerationResult{}, fmt.Errorf("GenerateContextualCandidates: nil dm")
	}
	limits := q.Limits
	if isZeroLimits(limits) {
		limits = DefaultCandidateLimits()
	}

	// accumulator keyed by candidate ID (kind + artifact_id)
	acc := make(map[string]*candidateAccumulator)

	// Sources populated by per-source generators.
	sources := map[string]int{}

	// ── Source A: active work + work-referenced artifacts
	addWorkCandidates(dm, q, limits.Work, acc)
	sources["work"] = len(acc)

	// ── Source B: handoffs (read-only peek path)
	addHandoffCandidates(dm, q, limits.Handoff, acc)
	sources["handoff"] = len(acc)

	// ── Source C: recent semantic activity
	addActivityCandidates(dm, q, limits.Activity, acc)
	sources["activity"] = len(acc)

	// ── Source D: epistemic dependencies (theories, decisions, lessons, evidence)
	addEpistemicCandidates(dm, q, limits.Epistemic, acc)
	sources["epistemic"] = len(acc)

	// ── Source E: cascades
	addCascadeCandidates(dm, q, limits.Cascade, acc)
	sources["cascade"] = len(acc)

	// ── Source F: wakes / obligations
	addWakeCandidates(dm, q, limits.Wake, acc)
	sources["wake"] = len(acc)
	_ = sources // quiet unused warning if any tree-prune runs

	// ── Source G: scratchpad
	addScratchpadCandidates(dm, q, limits.Scratchpad, acc)
	sources["scratchpad"] = len(acc)

	// ── Source H: topic neighborhood
	addTopicCandidates(dm, q, limits.Topic, acc)
	sources["topic"] = len(acc)

	// Deterministic dedup: collapse accumulator entries to a single
	// candidate per (kind, artifact_id), merging reasons (sorted
	// lexicographically for determinism), related IDs (sorted +
	// deduped), and lifecycle state (first non-empty wins; sources
	// concatenated sorted for the audit trail).
	out := materialize(acc, limits.Global)
	globalCapApplied := len(out) >= limits.Global

	now := time.Now().Unix()
	return CandidateGenerationResult{
		Candidates: out,
		Sources:    sources,
		Diagnostics: CandidateGenerationDiag{
			GeneratedAt:      now,
			InputMpmSession:  q.MPMSessionID,
			InputFwSession:   q.FrameworkSessionID,
			InputWorkIDs:     len(q.WorkIDs),
			InputTopicIDs:    len(q.TopicIDs),
			InputArtifactIDs: len(q.ArtifactIDs),
			LimitsUsed:       limits,
			TotalRaw:         len(acc),
			TotalAfterDedup:  len(out),
			GlobalCapApplied: globalCapApplied,
		},
	}, nil
}

// candidateAccumulator is the internal per-key merge struct used
// during the source-by-source accumulation pass. Reasons and
// related_ids accumulate via set semantics; lifecycle_state and
// summary retain first-wins; sources accumulate all.
type candidateAccumulator struct {
	kind           string
	artifactID     string
	pointer        string
	timestamp      int64
	actorKind      string
	frameworkName  string
	mpmSessionID   string
	fwSessionID    string
	reasons        map[string]struct{}
	relatedIDs     map[string]struct{}
	sources        map[string]struct{}
	lifecycleState string
	summary        string
}

// materialize converts the accumulator map into a sorted,
// bounded []Candidate. Deterministic order: by kind ascending,
// then by artifact_id ascending.
func materialize(acc map[string]*candidateAccumulator, globalCap int) []Candidate {
	out := make([]Candidate, 0, len(acc))
	for _, a := range acc {
		reasons := setToSortedSlice(a.reasons)
		relatedIDs := setToSortedSlice(a.relatedIDs)
		sources := setToSortedSlice(a.sources)
		// Stable, deterministic id is kind + ":" + artifact_id.
		id := a.kind + ":" + a.artifactID
		// Cap summary length defensively (any caller-supplied
		// description gets truncated to 200 chars to keep the
		// candidate envelope bounded).
		summary := a.summary
		if len(summary) > 200 {
			summary = summary[:200]
		}
		out = append(out, Candidate{
			ID:                 id,
			Kind:               a.kind,
			ArtifactID:         a.artifactID,
			Pointer:            a.pointer,
			Timestamp:          a.timestamp,
			ActorKind:          a.actorKind,
			FrameworkName:      a.frameworkName,
			MPMSessionID:       a.mpmSessionID,
			FrameworkSessionID: a.fwSessionID,
			Reasons:            reasons,
			RelatedIDs:         relatedIDs,
			LifecycleState:     a.lifecycleState,
			Source:             strings.Join(sources, ","),
			Summary:            summary,
		})
	}
	// Deterministic sort: kind, then artifact_id.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ArtifactID < out[j].ArtifactID
	})
	// Apply global cap.
	if globalCap > 0 && len(out) > globalCap {
		out = out[:globalCap]
	}
	return out
}

func setToSortedSlice(s map[string]struct{}) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isZeroLimits(l CandidateLimits) bool {
	return l.Work == 0 && l.Handoff == 0 && l.Activity == 0 &&
		l.Epistemic == 0 && l.Cascade == 0 && l.Wake == 0 &&
		l.Scratchpad == 0 && l.Topic == 0 && l.Global == 0
}

// candidateKey returns the deterministic merge key.
func candidateKey(kind, artifactID string) string {
	return kind + ":" + artifactID
}

// ensureCandidate returns the accumulator for (kind, artifactID),
// creating it if absent.
func ensureCandidate(acc map[string]*candidateAccumulator, kind, artifactID string) *candidateAccumulator {
	key := candidateKey(kind, artifactID)
	a, ok := acc[key]
	if !ok {
		a = &candidateAccumulator{
			kind:       kind,
			artifactID: artifactID,
			reasons:    map[string]struct{}{},
			relatedIDs: map[string]struct{}{},
			sources:    map[string]struct{}{},
		}
		acc[key] = a
	}
	return a
}

// addReason appends a reason to the accumulator, deduping via set.
func addReason(a *candidateAccumulator, r CandidateReasonName) {
	if a == nil || r == "" {
		return
	}
	a.reasons[string(r)] = struct{}{}
}

// addRelated appends a related artifact id.
func addRelated(a *candidateAccumulator, id string) {
	if a == nil || id == "" {
		return
	}
	a.relatedIDs[id] = struct{}{}
}

// markSource records the generator that produced this candidate.
func markSource(a *candidateAccumulator, source string) {
	if a == nil || source == "" {
		return
	}
	a.sources[source] = struct{}{}
}

// hashKey produces a short deterministic fingerprint for content
// deduplication. NOT for security; just for stable identity.
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:8])
}

// nullIfEmpty returns nil for empty strings so SQL NULL semantics
// apply at write sites.
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nullStringScan scans a nullable string into a pointer-to-string
// idiom that returns "" when the column is SQL NULL.
func nullStringScan(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

// jsonMarshalForDebug is a defensive helper used by source helpers
// when they need to embed a brief JSON projection. Kept inline (vs.
// imported) to avoid pulling json into the candidate.go file's
// public surface; internal helpers can re-export via a future
// debug surface.
func jsonMarshalForDebug(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
