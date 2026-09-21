// contextual_selection.go — Stage 2E.1 deterministic contextual
// selection policy. Pure categorical function over the Stage 2D
// candidate universe. NO numeric scoring. NO LLM. NO mutation.
// NO embeddings. NO DB access. NO wall-clock reads inside the body.
//
// Architectural placement
// ────────────────────────
//
// Stage 2D produces a deterministic, bounded, observational
// candidate universe. Stage 2E.1 is the second-stage view layer:
// it consumes the candidate universe and produces a bounded,
// ordered, explainable projection suitable for direct
// injection into an agent's working context. Future stages
// (Stage 2E.2 materialization, Stage 2E.3 wake-context delivery)
// build on top of this projection.
//
// The function `SelectContextualCandidates` is the single
// exported entry. Everything else is internal: the band
// vocabulary, the inspectable policy table, combination rules,
// supersession compression, deterministic tie-breaks, and
// pre-zeroed diagnostics.
//
// Observational guarantee: SelectContextualCandidates does not
// mutate any persistent state nor any input Candidate value.
// It may be called repeatedly with identical inputs and must
// produce identical outputs (modulo `now`, which is an explicit
// input for determinism and test pinning).
//
// Determinism contract:
//   - Band order (CanonicalBands) is fixed.
//   - combinationRules slice order is fixed.
//   - SelectionFamily order is fixed.
//   - Tie-break within band: combination rule order > family order
//     > timestamp DESC > ID ASC.
//   - `now` is an explicit parameter; tests can pin it.

package internal

import (
	"sort"
	"strings"
)

// SelectionHardMaxItems is the package-level safety ceiling for
// selection output size. Not caller-adjustable. HardMax wins.
const SelectionHardMaxItems = 20

// SelectionBand is the closed, ordered vocabulary of categorical
// priority bands. Lower band index = higher priority. Order is
// FIXED.
//
//	BandObligation  : current context requiring action or
//	                  reconsideration NOW (overdue wake; direct
//	                  dependency whose foundation changed;
//		direct artifact with a pending cascade obligation).
//	BandDirect      : explicit structural callouts (active work,
//	                  work-referenced, explicit refs, explicit deps).
//	BandContinuity  : identity threads (same MPM/framework
//	                  session, handoff pickup, working scratchpad).
//	BandChange      : something-moved signals (cross-agent /
//	                  human activity, foundation / evidence /
//	                  confidence moves, supersession, cascade
//	                  resolved, cascade pending alone).
//	BandSupporting  : background (topic neighbors, unknown
//	                  provenance, future-unresolved wakes).
type SelectionBand int

const (
	BandObligation SelectionBand = iota
	BandDirect
	BandContinuity
	BandChange
	BandSupporting
)

// CanonicalBands is the closed ordered list of every
// SelectionBand. Used for diagnostics initialization
// (pre-zero entries) and for the band-order primary tie-break.
var CanonicalBands = []SelectionBand{
	BandObligation, BandDirect, BandContinuity, BandChange, BandSupporting,
}

// String returns the canonical lowercase name for diagnostics.
func (b SelectionBand) String() string {
	switch b {
	case BandObligation:
		return "obligation"
	case BandDirect:
		return "direct"
	case BandContinuity:
		return "continuity"
	case BandChange:
		return "change"
	case BandSupporting:
		return "supporting"
	default:
		return "unknown"
	}
}

// PolicyRationale is the closed, typed vocabulary of explanation
// codes. Every SelectedItem carries one PolicyRationale value.
type PolicyRationale string

const (
	RationaleFireNowDue             PolicyRationale = "fire_now_due"
	RationaleReconsiderCurrent      PolicyRationale = "reconsider_current_context"
	RationaleDirectExplicit         PolicyRationale = "direct_explicit"
	RationaleDirectActiveWork       PolicyRationale = "direct_active_work"
	RationaleContinuityIdentity     PolicyRationale = "continuity_identity"
	RationaleContinuityHandoff      PolicyRationale = "continuity_handoff"
	RationaleContinuityWorkingState PolicyRationale = "continuity_working_state"
	RationaleContinuityCrossAgent   PolicyRationale = "continuity_cross_agent_change"
	RationaleChangeRecent           PolicyRationale = "change_recent_activity"
	RationaleChangeCascadePending   PolicyRationale = "change_cascade_pending"
	RationaleChangeCascadeResolved  PolicyRationale = "change_cascade_resolved"
	RationaleChangeFoundationMoved  PolicyRationale = "change_foundation_moved"
	RationaleChangeConfidence       PolicyRationale = "change_confidence"
	RationaleChangeEvidence         PolicyRationale = "change_evidence"
	RationaleChangeSupersession     PolicyRationale = "change_supersession"
	RationaleSupportingTopic        PolicyRationale = "supporting_topic_neighbor"
	RationaleSupportingProvenance   PolicyRationale = "supporting_provenance"
	RationaleFutureObligation       PolicyRationale = "future_obligation"
	RationaleUnmapped               PolicyRationale = "unmapped_reason"
)

// SelectionLimits bounds the selection output.
//
//	MaxItems  : caller-requested maximum selection count. Clamped
//	            to [1, SelectionHardMaxItems] by validatePolicy.
//	            Zero value is treated as "use default" by the public
//	            handler; the selector itself rejects zero or
//	            negative with an explicit error.
type SelectionLimits struct {
	MaxItems int `json:"max_items,omitempty"`
}

// DefaultSelectionLimits returns the canonical bounded defaults
// for Stage 2E.1. MaxItems=10, hard max=20.
func DefaultSelectionLimits() SelectionLimits {
	return SelectionLimits{MaxItems: 10}
}

// SelectionPolicy bundles limits only. Band order is FIXED
// (CanonicalBands): the spec rejects configurable BandOrder as a
// way to silently weaken the canonical band hierarchy. Now is NOT
// a policy field; it is an evaluation input.
type SelectionPolicy struct {
	Limits SelectionLimits `json:"limits"`
}

// DefaultSelectionPolicy returns the canonical policy with
// default limits. Band order is canonical by construction.
func DefaultSelectionPolicy() SelectionPolicy {
	return SelectionPolicy{
		Limits: DefaultSelectionLimits(),
	}
}

// SelectionFamily is the closed, typed vocabulary of within-band
// diversity families. A candidate is assigned to exactly one
// family, derived from its WINNING SELECTION SEMANTICS (winning
// rationale / winning combination rule / strongest reason) — NOT
// from arbitrary source ordering.
type SelectionFamily string

const (
	FamilyWake       SelectionFamily = "wake"
	FamilyCascade    SelectionFamily = "cascade"
	FamilyWork       SelectionFamily = "work"
	FamilyExplicit   SelectionFamily = "explicit"
	FamilyHandoff    SelectionFamily = "handoff"
	FamilyEpistemic  SelectionFamily = "epistemic"
	FamilyActivity   SelectionFamily = "activity"
	FamilyScratchpad SelectionFamily = "scratchpad"
	FamilyTopic      SelectionFamily = "topic"
)

// CanonicalSelectionFamilies is the closed ordered list of every
// SelectionFamily. Used for the deterministic within-band diversity
// walk. Order is a deterministic traversal order, NOT a claim of
// intrinsic relevance between families.
var CanonicalSelectionFamilies = []SelectionFamily{
	FamilyWake, FamilyCascade, FamilyWork, FamilyExplicit, FamilyHandoff,
	FamilyEpistemic, FamilyActivity, FamilyScratchpad, FamilyTopic,
}

// policyEntry is one row of the central reason→band+rationale
// policy table.
type policyEntry struct {
	Band      SelectionBand
	Rationale PolicyRationale
}

// selectionPolicyTable is the central, inspectable map from each
// declared CandidateReasonName to its band + rationale. Adding a
// new reason MUST add a new entry here; TestSelection_PolicyCompleteness
// enforces.
var selectionPolicyTable = map[CandidateReasonName]policyEntry{
	ReasonOverdueWake:            {BandObligation, RationaleFireNowDue},
	ReasonUnresolvedWake:         {BandSupporting, RationaleFutureObligation},
	ReasonCascadePending:         {BandChange, RationaleChangeCascadePending},
	ReasonHandoffForContext:      {BandContinuity, RationaleContinuityHandoff},
	ReasonOpenWork:               {BandDirect, RationaleDirectActiveWork},
	ReasonReferencedByActiveWork: {BandDirect, RationaleDirectActiveWork},
	ReasonExplicitReference:      {BandDirect, RationaleDirectExplicit},
	ReasonSameMPMSession:         {BandContinuity, RationaleContinuityIdentity},
	ReasonSameFrameworkSession:   {BandContinuity, RationaleContinuityIdentity},
	ReasonSupersessionChain:      {BandChange, RationaleChangeSupersession},
	ReasonActiveScratchpad:       {BandContinuity, RationaleContinuityWorkingState},
	ReasonRecentCrossAgentChange: {BandChange, RationaleChangeRecent},
	ReasonRecentHumanChange:      {BandChange, RationaleChangeRecent},
	ReasonUnknownSourceChange:    {BandChange, RationaleChangeRecent},
	ReasonConfidenceChanged:      {BandChange, RationaleChangeConfidence},
	ReasonEvidenceAdded:          {BandChange, RationaleChangeEvidence},
	ReasonFoundationSuperseded:   {BandChange, RationaleChangeFoundationMoved},
	ReasonFoundationInvalidated:  {BandChange, RationaleChangeFoundationMoved},
	ReasonCascadeResolved:        {BandChange, RationaleChangeCascadeResolved},
	ReasonSharesTopic:            {BandSupporting, RationaleSupportingTopic},
}

// combinationRule matches a specific reason co-occurrence and
// produces a deterministic band override. The override applies
// AFTER per-reason band resolution; the rule producing the
// strongest band wins. Tie-break: declared rule order.
type combinationRule struct {
	Name      string
	Reasons   []string
	Band      SelectionBand
	Rationale PolicyRationale
}

// combinationRules is the closed, ordered list of material
// combinations. All retained rules have been verified for
// Stage-2D reachability (TestSelection_CombinationReachability).
//
// Strongest-band-wins is the rule-ordering principle: rules are
// sorted strongest-band first so the strongest obligation escalation
// wins when multiple rules match. Rule ordering IS load-bearing; do
// not reorder without rerunning TestSelection_CombinationReachability.
//
// Only rules whose reason combination is generator-reachable
// (TestSelection_CombinationRule_GeneratorBacked) are retained.
// "direct_*" variants referencing referenced_by_active_work +
// foundation_* were considered but rejected: the work source
// (Source A) emits open_work + referenced_by_active_work on a
// (work:*) candidate, while the epistemic source (Source D) emits
// foundation_* on a (theory:*) candidate. They never land on the
// same (kind, artifact_id) so the combination is not reachable.
var combinationRules = []combinationRule{
	// ── Obligation escalation: foundation moved for direct context ──
	{
		Name:      "explicit_ref_on_superseded_foundation",
		Reasons:   []string{"explicit_reference", "foundation_superseded"},
		Band:      BandObligation,
		Rationale: RationaleReconsiderCurrent,
	},
	{
		Name:      "explicit_ref_on_invalidated_foundation",
		Reasons:   []string{"explicit_reference", "foundation_invalidated"},
		Band:      BandObligation,
		Rationale: RationaleReconsiderCurrent,
	},
	{
		Name:      "explicit_ref_with_cascade",
		Reasons:   []string{"explicit_reference", "cascade_pending"},
		Band:      BandObligation,
		Rationale: RationaleReconsiderCurrent,
	},
	// ── Direct-grade co-occurrence ──
	{
		Name:      "explicit_ref_active_work",
		Reasons:   []string{"explicit_reference", "open_work"},
		Band:      BandDirect,
		Rationale: RationaleDirectExplicit,
	},
	// ── Continuity-grade co-occurrence (must NOT lose continuity band) ──
	{
		Name:      "same_session_cross_agent_change",
		Reasons:   []string{"same_mpm_session", "recent_cross_agent_change"},
		Band:      BandContinuity,
		Rationale: RationaleContinuityCrossAgent,
	},
}

// SelectionTrigger is selector-owned metadata that records the
// strongest historical policy trigger absorbed by supersession
// compression. Each suppressed predecessor contributes one trigger
// carrying its winning (band, rationale, combination-rule-name)
// tuple. Triggers are carried on the canonical survivor so the
// downstream agent can see why the historical chain mattered
// without us mutating the canonical candidate's Reasons.
//
// Carrying this as a separate typed slice (rather than overloading
// the candidate's Reasons) is the spec's requirement: canonical
// candidate Reasons describe the live state of the artifact;
// triggers describe selector decisions about its history.
type SelectionTrigger struct {
	// SourceID is the (kind:artifact_id) of the suppressed
	// predecessor. Always present.
	SourceID string `json:"source_id"`
	// Band is the band that the predecessor would have been
	// selected into if it had not been suppressed.
	Band SelectionBand `json:"band"`
	// Rationale is the winning rationale of the predecessor.
	Rationale PolicyRationale `json:"rationale"`
	// CombinationName is the winning combination rule name, if
	// any. Empty when no combination rule fired.
	CombinationName string `json:"combination_name,omitempty"`
}

// SelectedItem is one entry in the bounded selection. Candidate
// is a value copy — the result never aliases mutable input
// state. WhyNow is a deterministic bounded template. Rationale
// is the single winning rationale (singular primary per spec).
// CombinationMatches records every combination rule that fired
// for this candidate (not just the winning one).
// CompressedRelatedIDs preserves the IDs of historical
// predecessors suppressed by chain compression.
// CompressionTriggers preserves the typed historical-policy
// triggers absorbed from each suppressed predecessor.
type SelectedItem struct {
	Candidate            Candidate          `json:"candidate"`
	Band                 string             `json:"band"`
	Rationale            PolicyRationale    `json:"rationale"`
	WhyNow               string             `json:"why_now"`
	SelectionReasons     []string           `json:"selection_reasons"`
	CombinationMatches   []string           `json:"combination_matches,omitempty"`
	CompressedRelatedIDs []string           `json:"compressed_related_ids,omitempty"`
	CompressionTriggers  []SelectionTrigger `json:"compression_triggers,omitempty"`
}

// SelectionResult is the bounded output of the selector.
type SelectionResult struct {
	Items       []SelectedItem       `json:"items"`
	Diagnostics SelectionDiagnostics `json:"diagnostics"`
}

// classified is the internal working type used during the
// classify → sort → diversity walk pipeline. Named at package
// scope so sortClassifiedItems / diversityWalk can take the
// typed slice directly.
type classified struct {
	Candidate            Candidate
	Band                 SelectionBand
	Rationale            PolicyRationale
	WhyNow               string
	SelectionReasons     []string
	CombinationMatches   []string
	Family               SelectionFamily
	CompressedRelatedIDs []string
	CompressionTriggers  []SelectionTrigger
}

// SelectionDiagnostics captures the run metadata. Every
// canonical band / kind / source key is pre-zero-registered so
// consumers can distinguish "ran, found nothing" from "never
// ran" (Stage 2D.2 §21 lesson).
type SelectionDiagnostics struct {
	SelectedAt            int64           `json:"selected_at"`
	InputCount            int             `json:"input_count"`
	AfterCompressionCount int             `json:"after_compression_count"`
	SelectedCount         int             `json:"selected_count"`
	DroppedRedundant      int             `json:"dropped_redundant"`
	DroppedBudget         int             `json:"dropped_budget"`
	SelectedByBand        map[string]int  `json:"selected_by_band"`
	SelectedByKind        map[string]int  `json:"selected_by_kind"`
	SelectedBySource      map[string]int  `json:"selected_by_source"`
	CombinationMatches    []string        `json:"combination_matches,omitempty"`
	UnknownPolicyReasons  []string        `json:"unknown_policy_reasons,omitempty"`
	RequestedLimit        int             `json:"requested_limit"`
	EffectiveLimit        int             `json:"effective_limit"`
	LimitClamped          bool            `json:"limit_clamped"`
	CapApplied            bool            `json:"cap_applied"`
	LimitsUsed            SelectionLimits `json:"limits_used"`
}

// SelectContextualCandidates is the canonical Stage 2E.1
// selector entry. Applies (1) supersession compression, (2)
// per-reason band assignment + combination-rule overrides, (3)
// deterministic tie-break sorting, and (4) limit enforcement.
//
// Pure function: no DB access, no mutation of inputs (the input
// slice itself is not reordered; a fresh slice of Candidate
// values is built from compression). No wall-clock reads inside
// the body.
//
// Deterministic for identical (candidates, policy, now).
func SelectContextualCandidates(
	candidates []Candidate,
	policy SelectionPolicy,
	now int64,
) SelectionResult {
	// Step 0: validate policy and pre-zero diagnostics.
	policy, effectiveLimit, limitClamped, reqLimit, err := validatePolicy(policy)
	if err != nil {
		// Return a structured error result with empty items.
		// The caller (tools handler) should treat this as an error
		// path; the result is a deterministic empty projection.
		bd := preRegisterBandDiagnostics()
		kd := preRegisterKindDiagnostics()
		sd := preRegisterSourceDiagnostics()
		_ = bd
		_ = kd
		_ = sd
		return SelectionResult{
			Items: nil,
			Diagnostics: SelectionDiagnostics{
				SelectedAt:            now,
				InputCount:            len(candidates),
				AfterCompressionCount: len(candidates),
				SelectedCount:         0,
				DroppedRedundant:      0,
				DroppedBudget:         0,
				SelectedByBand:        preRegisterBandDiagnostics(),
				SelectedByKind:        preRegisterKindDiagnostics(),
				SelectedBySource:      preRegisterSourceDiagnostics(),
				RequestedLimit:        reqLimit,
				EffectiveLimit:        0,
				LimitClamped:          limitClamped,
				CapApplied:            false,
				LimitsUsed:            policy.Limits,
			},
		}
	}

	inputCount := len(candidates)
	selectedByBand := preRegisterBandDiagnostics()
	selectedByKind := preRegisterKindDiagnostics()
	selectedBySource := preRegisterSourceDiagnostics()

	// Step 1: collect unknown reason strings (diagnostic scope: INPUT).
	unknownReasons := collectUnknownReasons(candidates)

	// Step 2: supersession compression.
	survivors, suppressedToSurvivor, survivingCount := compressSupersessionChain(candidates)

	// Step 3: collect combination-match names (diagnostic scope:
	// candidates AFTER compression).
	comboMatches := collectCombinationMatches(survivors)

	// Step 4: classify each survivor.
	classifiedItems := make([]classified, 0, len(survivors))
	for i := range survivors {
		c := survivors[i]
		band, rationale := classifyBand(c)
		var comboName string
		var comboRationale PolicyRationale
		band, comboName, comboRationale = applyCombinationRules(c, band, rationale)
		if comboName != "" {
			rationale = comboRationale
		}
		family := deriveFamily(c, band, rationale, comboName)
		reasons := sortedReasons(c.Reasons)
		why := generateWhyNow(band, rationale, comboName, family)
		var cidList []string
		var triggers []SelectionTrigger
		if mapIDs, ok := suppressedToSurvivor[c.ID]; ok {
			cidList = sortedStrings(mapIDs)
			// Build a SelectionTrigger for each suppressed
			// predecessor by re-running the classify pipeline on
			// the predecessor (which we still hold in `candidates`).
			// The trigger captures the strongest historical policy
			// state of the predecessor — band, rationale, combo —
			// without mutating the canonical survivor's Reasons.
			triggers = buildTriggers(c.ID, mapIDs, candidates)
		}
		cm := []string{}
		if comboName != "" {
			cm = []string{comboName}
		}
		classifiedItems = append(classifiedItems, classified{
			Candidate:            c,
			Band:                 band,
			Rationale:            rationale,
			WhyNow:               why,
			SelectionReasons:     reasons,
			CombinationMatches:   cm,
			Family:               family,
			CompressedRelatedIDs: cidList,
			CompressionTriggers:  triggers,
		})
	}

	// Step 5: deterministic tie-break sort within band.
	sortClassifiedItems(classifiedItems)

	// Step 6: enforce diversity walk within band.
	selected := diversityWalk(classifiedItems, effectiveLimit)

	// Step 7: build SelectedItems, populate diagnostics.
	items := make([]SelectedItem, 0, len(selected))
	for _, s := range selected {
		items = append(items, SelectedItem{
			Candidate:            s.Candidate,
			Band:                 s.Band.String(),
			Rationale:            s.Rationale,
			WhyNow:               s.WhyNow,
			SelectionReasons:     s.SelectionReasons,
			CombinationMatches:   s.CombinationMatches,
			CompressedRelatedIDs: s.CompressedRelatedIDs,
			CompressionTriggers:  s.CompressionTriggers,
		})
		selectedByBand[s.Band.String()]++
		selectedByKind[s.Candidate.Kind]++
		for _, src := range strings.Split(s.Candidate.Source, ",") {
			if src == "" {
				continue
			}
			selectedBySource[src]++
		}
	}

	droppedRedundant := inputCount - survivingCount
	if droppedRedundant < 0 {
		droppedRedundant = 0
	}
	droppedBudget := survivingCount - len(items)
	if droppedBudget < 0 {
		droppedBudget = 0
	}
	capApplied := survivingCount > effectiveLimit && droppedBudget > 0

	// Sort combination matches deterministically.
	sortedCombo := sortedStrings(comboMatches)

	return SelectionResult{
		Items: items,
		Diagnostics: SelectionDiagnostics{
			SelectedAt:            now,
			InputCount:            inputCount,
			AfterCompressionCount: survivingCount,
			SelectedCount:         len(items),
			DroppedRedundant:      droppedRedundant,
			DroppedBudget:         droppedBudget,
			SelectedByBand:        selectedByBand,
			SelectedByKind:        selectedByKind,
			SelectedBySource:      selectedBySource,
			CombinationMatches:    sortedCombo,
			UnknownPolicyReasons:  unknownReasons,
			RequestedLimit:        reqLimit,
			EffectiveLimit:        effectiveLimit,
			LimitClamped:          limitClamped,
			CapApplied:            capApplied,
			LimitsUsed:            policy.Limits,
		},
	}
}

// validatePolicy returns the validated policy, effective limit,
// limit-clamped flag, requested limit, and error.
func validatePolicy(p SelectionPolicy) (SelectionPolicy, int, bool, int, error) {
	req := p.Limits.MaxItems
	if req == 0 {
		// Omitted (or default-zero). Use default.
		p.Limits = DefaultSelectionLimits()
		req = p.Limits.MaxItems
	}
	if req < 1 {
		return p, 0, false, req, errInvalidMaxItems{Max: req}
	}
	if req > SelectionHardMaxItems {
		return p, SelectionHardMaxItems, true, req, nil
	}
	return p, req, false, req, nil
}

// errInvalidMaxItems signals that MaxItems was supplied
// explicitly as <= 0. The public contract rejects invalid lower
// values rather than normalizing.
type errInvalidMaxItems struct{ Max int }

func (e errInvalidMaxItems) Error() string {
	return "contextual_selection: selection_limits.max_items must be >= 1 (got 0 or negative)"
}

// classifyBand returns the per-reason band + rationale for the
// STRONGEST mapped reason. Multi-reason candidates land at the
// lowest-band reason. Unknown reasons fall back to
// BandSupporting + RationaleUnmapped.
func classifyBand(c Candidate) (SelectionBand, PolicyRationale) {
	var bestBand = BandSupporting
	var bestRationale PolicyRationale = RationaleUnmapped
	for _, r := range c.Reasons {
		entry, ok := selectionPolicyTable[CandidateReasonName(r)]
		if !ok {
			continue
		}
		if entry.Band < bestBand {
			bestBand = entry.Band
			bestRationale = entry.Rationale
		}
	}
	return bestBand, bestRationale
}

// applyCombinationRules scans combinationRules in declared
// order, returns the first rule whose Reasons set is a subset of
// the candidate's reason set, applied as an override (band +
// rationale) with the matching rule's Name returned for
// diagnostic. If no rule matches, the per-reason band + rationale
// are returned unchanged and the Name is empty.
func applyCombinationRules(c Candidate, band SelectionBand, rationale PolicyRationale) (SelectionBand, string, PolicyRationale) {
	candidateReasons := make(map[string]struct{}, len(c.Reasons))
	for _, r := range c.Reasons {
		candidateReasons[r] = struct{}{}
	}
	bestRule := -1
	for i, rule := range combinationRules {
		match := true
		for _, need := range rule.Reasons {
			if _, ok := candidateReasons[need]; !ok {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// Choose the rule producing the strongest band; tie-break
		// on declared order (first match wins on equal band).
		if bestRule == -1 || rule.Band < combinationRules[bestRule].Band {
			bestRule = i
		}
	}
	if bestRule == -1 {
		return band, "", rationale
	}
	rule := combinationRules[bestRule]
	return rule.Band, rule.Name, rule.Rationale
}

// deriveFamily returns the SelectionFamily for a candidate,
// derived from the WINNING selection semantics (combination rule
// match > rationale > kind). Multi-source candidates get a
// single deterministic family — SelectedBySource still counts
// each source membership separately.
func deriveFamily(c Candidate, band SelectionBand, rationale PolicyRationale, comboName string) SelectionFamily {
	// 1. Combination rule precedence (typed semantics).
	if comboName != "" {
		switch comboName {
		case "explicit_ref_on_superseded_foundation",
			"explicit_ref_on_invalidated_foundation",
			"explicit_ref_with_cascade",
			"explicit_ref_active_work":
			return FamilyExplicit
		}
	}
	// 2. Rationale precedence.
	switch rationale {
	case RationaleFireNowDue:
		return FamilyWake
	case RationaleReconsiderCurrent:
		return FamilyExplicit
	case RationaleDirectActiveWork:
		return FamilyWork
	case RationaleDirectExplicit:
		return FamilyExplicit
	case RationaleContinuityHandoff:
		return FamilyHandoff
	case RationaleContinuityIdentity, RationaleContinuityWorkingState:
		return FamilyHandoff
	case RationaleChangeCascadePending:
		return FamilyCascade
	}
	// 3. Kind fallback (Stage 2D's authoritative kind vocabulary).
	switch c.Kind {
	case "wake":
		return FamilyWake
	case "handoff":
		return FamilyHandoff
	case "work":
		return FamilyWork
	case "memory", "lesson", "theory", "decision", "evidence":
		return FamilyEpistemic
	case "scratchpad":
		return FamilyScratchpad
	case "activity":
		return FamilyActivity
	}
	// 4. Last fallback.
	return FamilyActivity
}

// compressSupersessionChain collapses supersession chains. For
// each connected component (via ReasonSupersessionChain +
// RelatedIDs membership):
//   - exactly one canonical member + every other member
//     historical → compress: survivor=canonical, suppress others.
//   - zero canonical, multiple canonical, or mixed unknown → no
//     compression (fail safe: retain candidates, don't invent
//     canonical truth).
//
// Returns the survivor slice (deterministic order: input order
// filtered), a map[survivorID]→[]suppressedID for the compressed
// related IDs, and the surviving count.
func compressSupersessionChain(candidates []Candidate) ([]Candidate, map[string][]string, int) {
	// Build id→index map for participating candidates.
	idIdx := make(map[string]int, len(candidates))
	part := make([]bool, len(candidates))
	for i := range candidates {
		if hasReasonStrings(candidates[i].Reasons, string(ReasonSupersessionChain)) {
			idIdx[candidates[i].ID] = i
			part[i] = true
		}
	}
	if len(idIdx) == 0 {
		// No chain participants; everything survives unchanged.
		// Defensive copy of slice header.
		out := make([]Candidate, len(candidates))
		copy(out, candidates)
		return out, map[string][]string{}, len(out)
	}
	// Build undirected adjacency among participants via
	// shared RelatedIDs membership.
	var adj [][]int
	idxByID := idIdx
	ids := make([]string, 0, len(idIdx))
	for id := range idIdx {
		ids = append(ids, id)
	}
	// Map id → dense index 0..N-1.
	dense := make(map[string]int, len(ids))
	for i, id := range ids {
		dense[id] = i
	}
	adj = make([][]int, len(ids))
	for _, id := range ids {
		adj[dense[id]] = adj[dense[id]][:0]
	}
	for i, id := range ids {
		c := candidates[idxByID[id]]
		for _, rel := range c.RelatedIDs {
			if j, ok := dense[rel]; ok {
				adj[i] = append(adj[i], j)
				adj[j] = append(adj[j], i)
			}
		}
	}
	// Find connected components via DFS.
	visited := make([]bool, len(ids))
	var comps [][]int
	for i := 0; i < len(ids); i++ {
		if visited[i] {
			continue
		}
		visited[i] = true
		var comp []int
		stack := []int{i}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			comp = append(comp, n)
			for _, nb := range adj[n] {
				if !visited[nb] {
					visited[nb] = true
					stack = append(stack, nb)
				}
			}
		}
		comps = append(comps, comp)
	}
	// Decide compress-or-not per component.
	suppressed := make(map[string]bool, len(candidates))
	suppressedToSurvivor := map[string][]string{}
	for _, comp := range comps {
		canonical := -1
		historicalCount := 0
		unknownCount := 0
		for _, denseIdx := range comp {
			idx := idxByID[ids[denseIdx]]
			switch candidates[idx].LifecycleState {
			case "canonical":
				if canonical == -1 {
					canonical = denseIdx
				} else {
					// Multiple canonical: ambiguous, no compression.
					canonical = -2
				}
			case "historical":
				historicalCount++
			default:
				unknownCount++
			}
		}
		if canonical < 0 || unknownCount > 0 {
			// zero canonical (-1), multiple canonical (-2), or
			// mixed-unknown: DO NOT compress.
			continue
		}
		// Exactly one canonical, every other member historical
		// (historicalCount must equal len(comp)-1).
		if historicalCount != len(comp)-1 {
			continue
		}
		survivorID := ids[canonical]
		var suppressedIDs []string
		for _, denseIdx := range comp {
			if denseIdx == canonical {
				continue
			}
			id := ids[denseIdx]
			suppressed[id] = true
			suppressedIDs = append(suppressedIDs, id)
		}
		sort.Strings(suppressedIDs)
		suppressedToSurvivor[survivorID] = suppressedIDs
	}
	// Emit survivors preserving input order.
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if suppressed[c.ID] {
			continue
		}
		out = append(out, c)
	}
	return out, suppressedToSurvivor, len(out)
}

// diversityWalk selects the final items using PER-BAND
// exhaustion with within-band family diversity.
//
// Band hierarchy is FIXED (CanonicalBands): obligation exhausts
// before direct, direct before continuity, continuity before change,
// change before supporting. Within a band, families are visited in
// declared order (CanonicalSelectionFamilies); within a family,
// sort order is (combination-rule order, timestamp DESC, ID ASC)
// via sortClassifiedItems. The first surviving candidate of each
// (band, family) wins its slot; subsequent (band, family) slots
// drain remaining candidates in the same band before the band
// itself is exhausted.
//
// Practical effect: a budget that fits only obligation-band items
// fills from obligation; once obligation is exhausted, the next
// band (direct) starts drawing; etc. This is the canonical
// structure-beats-recency guarantee at the band granularity the
// spec requires.
func diversityWalk(items []classified, budget int) []classified {
	selected := make([]classified, 0, budget)
	if budget <= 0 || len(items) == 0 {
		return selected
	}
	selectedID := make(map[string]struct{}, budget)

	// PASS 1: per-band exhaustion. Within each band, take items
	// in their sort order (family-rank, combo-rank, timestamp
	// DESC, ID ASC) until the band has no more items OR budget is
	// filled. Family rank still controls tie-breaks inside a band
	// but does NOT interleave across bands.
	for _, band := range CanonicalBands {
		if len(selected) >= budget {
			break
		}
		for _, it := range items {
			if len(selected) >= budget {
				break
			}
			if _, dup := selectedID[it.Candidate.ID]; dup {
				continue
			}
			if it.Band != band {
				continue
			}
			selected = append(selected, it)
			selectedID[it.Candidate.ID] = struct{}{}
		}
	}
	return selected
}

// sortClassifiedItems sorts in place by (band asc, family-order
// asc per CanonicalSelectionFamilies, combination-rule order,
// timestamp DESC, ID ASC).
func sortClassifiedItems(items []classified) {
	familyRank := make(map[SelectionFamily]int, len(CanonicalSelectionFamilies))
	for i, f := range CanonicalSelectionFamilies {
		familyRank[f] = i
	}
	comboRank := make(map[string]int, len(combinationRules))
	for i, r := range combinationRules {
		comboRank[r.Name] = i
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Band != b.Band {
			return a.Band < b.Band
		}
		ra, rb := familyRank[a.Family], familyRank[b.Family]
		if ra != rb {
			return ra < rb
		}
		// Combination rule precedence: rule with declared-order
		// index ranks earlier (smaller). Candidates without a
		// combination match rank after any matched candidate.
		var ai, bi int = 999, 999
		if len(a.CombinationMatches) > 0 {
			if r, ok := comboRank[a.CombinationMatches[0]]; ok {
				ai = r
			}
		}
		if len(b.CombinationMatches) > 0 {
			if r, ok := comboRank[b.CombinationMatches[0]]; ok {
				bi = r
			}
		}
		if ai != bi {
			return ai < bi
		}
		if a.Candidate.Timestamp != b.Candidate.Timestamp {
			return a.Candidate.Timestamp > b.Candidate.Timestamp
		}
		return a.Candidate.ID < b.Candidate.ID
	})
}

// generateWhyNow returns a deterministic bounded template.
func generateWhyNow(band SelectionBand, rationale PolicyRationale, comboName string, family SelectionFamily) string {
	// Combination-rule templates override rationale templates.
	switch comboName {
	case "explicit_ref_on_superseded_foundation",
		"explicit_ref_on_invalidated_foundation",
		"explicit_ref_with_cascade":
		return "Direct current context whose foundation changed; reconsider now."
	case "explicit_ref_active_work":
		return "Caller-supplied artifact referenced by active work."
	case "same_session_cross_agent_change":
		return "Changed by another agent in the current MPM session."
	}
	switch rationale {
	case RationaleFireNowDue:
		return "Overdue unresolved wake."
	case RationaleFutureObligation:
		return "Future scheduled wake — not yet due."
	case RationaleReconsiderCurrent:
		return "Direct current context whose foundation changed; reconsider now."
	case RationaleDirectActiveWork:
		return "Open work item."
	case RationaleDirectExplicit:
		return "Caller-supplied artifact reference."
	case RationaleContinuityIdentity:
		return "Same session continuity."
	case RationaleContinuityHandoff:
		return "Relevant handoff from current context."
	case RationaleContinuityWorkingState:
		return "Active scratchpad in current session."
	case RationaleContinuityCrossAgent:
		return "Changed by another agent in the current MPM session."
	case RationaleChangeRecent:
		return "Recent change recorded."
	case RationaleChangeCascadePending:
		return "Pending cascade — downstream state requires reconsideration."
	case RationaleChangeCascadeResolved:
		return "Cascade resolution recorded."
	case RationaleChangeFoundationMoved:
		return "Foundation state changed."
	case RationaleChangeConfidence:
		return "Confidence was recomputed."
	case RationaleChangeEvidence:
		return "New evidence recorded."
	case RationaleChangeSupersession:
		return "Supersession chain — canonical successor of relevant historical predecessor(s)."
	case RationaleSupportingTopic:
		return "Topic neighbor."
	case RationaleSupportingProvenance:
		return "Unknown provenance."
	case RationaleUnmapped:
		return "Unrecognized reason — see selection_reasons."
	}
	return ""
}

// preRegisterBandDiagnostics returns a zero-filled band-name
// map covering every canonical band.
func preRegisterBandDiagnostics() map[string]int {
	m := make(map[string]int, len(CanonicalBands))
	for _, b := range CanonicalBands {
		m[b.String()] = 0
	}
	return m
}

// preRegisterKindDiagnostics returns a zero-filled kind-name
// map covering every canonical kind.
func preRegisterKindDiagnostics() map[string]int {
	m := make(map[string]int, len(CanonicalKinds))
	for _, k := range CanonicalKinds {
		m[k] = 0
	}
	return m
}

// preRegisterSourceDiagnostics returns a zero-filled source-name
// map covering every canonical source.
func preRegisterSourceDiagnostics() map[string]int {
	m := make(map[string]int, len(CanonicalSources))
	for _, s := range CanonicalSources {
		m[string(s)] = 0
	}
	return m
}

// collectUnknownReasons scans all candidates and returns unique
// reason strings not in the selectionPolicyTable.
func collectUnknownReasons(candidates []Candidate) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, c := range candidates {
		for _, r := range c.Reasons {
			if _, ok := selectionPolicyTable[CandidateReasonName(r)]; ok {
				continue
			}
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// collectCombinationMatches scans the supplied survivors and
// returns unique combination-rule names that fired for them.
func collectCombinationMatches(candidates []Candidate) []string {
	seen := make(map[string]struct{})
	candidateReasons := make(map[string]struct{})
	for _, c := range candidates {
		for _, r := range c.Reasons {
			candidateReasons[r] = struct{}{}
		}
		for _, rule := range combinationRules {
			match := true
			for _, need := range rule.Reasons {
				if _, ok := candidateReasons[need]; !ok {
					match = false
					break
				}
			}
			if match {
				seen[rule.Name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// sortedReasons returns a sorted copy of a reason slice (no
// mutation of input).
func sortedReasons(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// sortedStrings returns a sorted copy.
func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// buildTriggers classifies each suppressed predecessor and returns
// the per-predecessor SelectionTrigger slice for the canonical
// survivor. Triggers preserve the strongest historical policy
// state (band, rationale, winning combination rule) so the
// downstream agent can see why the historical chain mattered
// without the canonical candidate's Reasons being mutated.
//
// Order is deterministic: sorted by SourceID ascending.
func buildTriggers(survivorID string, suppressedIDs []string, all []Candidate) []SelectionTrigger {
	if len(suppressedIDs) == 0 {
		return nil
	}
	byID := make(map[string]Candidate, len(all))
	for _, c := range all {
		byID[c.ID] = c
	}
	ordered := append([]string(nil), suppressedIDs...)
	sort.Strings(ordered)
	out := make([]SelectionTrigger, 0, len(ordered))
	for _, id := range ordered {
		pred, ok := byID[id]
		if !ok {
			continue
		}
		predBand, predRatio := classifyBand(pred)
		predBand, predCombo, predComboRatio := applyCombinationRules(pred, predBand, predRatio)
		if predCombo != "" {
			predRatio = predComboRatio
		}
		out = append(out, SelectionTrigger{
			SourceID:        id,
			Band:            predBand,
			Rationale:       predRatio,
			CombinationName: predCombo,
		})
	}
	return out
}

// hasReason reports whether the slice contains the given string.
func hasReasonStrings(rs []string, r string) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}
