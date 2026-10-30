// missed_opportunity.go — the retrospective missed-opportunity hunt.
//
// WHAT IT DETECTS.
//
// An agent session that did MPM-mediated work and recorded nothing. It is
// the only hunt in the critic that looks at ABSENT knowledge: the other
// three (survival_asymmetry, stale_memory, weak_theory) read `memories`
// and detect DEGRADATION of what is recorded. This one compares agent
// activity against recorded artifacts — the join
// `tool_invocations` × `artifact_provenance` — and asks which sessions
// did work that left no trace.
//
// WHY THE JOIN IS NOT ENOUGH ON ITS OWN.
//
// The audit that proposed this hunt measured the join on live data and
// found a session with 12 invocations and 0 artifacts, and called it "a
// real observed gap". That measurement was WRONG: the session had
// recorded 8 decisions and 3 handoffs.
//
// The reason is a substrate defect, measured here in
// internal/critic/p3_missed_opportunity_test.go:
//
//	artifact_provenance.invocation_id is populated ONLY by the
//	mpm_memory save path.
//
// The per-call provenance bridge (internal/core/memory_tools.go:184) is
// the one place that overlays the ActiveContext's invocation id onto the
// effective provenance. Every other write path — decisions, theories,
// lessons, work — resolves its provenance with an EMPTY invocation id,
// so its artifact_provenance row cannot be joined back to the call that
// produced it. On the live workspace, 26 of 26 decision provenance rows
// have an empty invocation_id.
//
// So the join alone reports a FALSE GAP for any session that recorded
// decisions, theories or lessons. This hunt therefore computes TWO
// independent signals and requires BOTH to be absent before it fires:
//
//  1. the join: no artifact_provenance row is attributable to any of the
//     session's invocations; and
//  2. the tool log: the session made no successful call to a
//     RECORDING ACTION (the declared vocabulary below).
//
// Signal 2 is what closes the hole signal 1 leaves. Both are
// disqualifying: a session is a candidate only when neither fired.
//
// HONESTY OF THE CLAIM.
//
// The proposal this hunt emits asserts only what the cited invocations
// show. It does NOT assert that a particular lesson exists, and it is
// never promoted: it is a PENDING proposal that a reviewer must act on.
//
// THE GAP RULE.
//
// docs/dev/session-a-adjudication.md §6b.2 ("Invocation gaps are candidate
// opportunities, not proof"): a hunt that finds an invocation with no
// corresponding artifact has found the ABSENCE of a record, and absence is
// not evidence. So:
//
//   - the gap is the TRIGGER, never a citation. The citations are the
//     session's tool_invocations rows, which exist and resolve as type
//     'tool_invocation' (P2's typed path validates this); and
//   - where the ONLY thing available is a gap, there is no proposal at
//     all — it is a finding for a human. missedOpportunityProposal
//     returns ok=false in that case, and the hunt logs it instead.
//
// IDEMPOTENCY.
//
// A hunt that emits a fresh proposal every cycle floods the substrate with
// indistinguishable pending theories and inflates the very evidence rows
// the substrate counts as corroboration. The proposal here is therefore a
// PURE FUNCTION of its evidence:
//
//   - the hypothesis names the session and never a count (a count grows,
//     and the content hash is part of the proposal's identity);
//   - the tags carry no cycle marker;
//   - the validation criteria carry no cycle marker — and this is NOT a
//     free choice: validation_criteria is part of the proposal's metadata
//     and therefore part of the F19 identity hash. The audit assumed
//     otherwise; internal/core/p3_identity_test.go pins the truth.
//   - the citations are the session's OLDEST invocations, bounded by
//     MaxCitations. The proposal's IDENTITY never changes; the citations
//     accumulate as evidence while the session keeps working, up to the
//     bound, and then converge on the oldest prefix. The newest
//     invocations are never cited, because a moving citation set would
//     change the proposal's meaning on every cycle.
//
// The cycle number belongs in the finding's Reason, which is logged and
// never persisted.

package critic

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// recordingActions is the declared vocabulary of (tool, action) pairs that,
// when they SUCCEED, mean the agent RECORDED durable state.
//
// It is the second of the hunt's two disqualifying signals, and it exists
// because the join cannot see every recording (see the file header).
//
// It is deliberately CONSERVATIVE: it lists every action that persists or
// changes durable MPM state, so that a session which did anything lasting
// is not reported as having recorded nothing. The failure mode of an
// incomplete vocabulary is a FALSE GAP — a pending proposal claiming a
// missed opportunity that did not happen — which is exactly the kind of
// noise the trust work is trying to remove.
//
// The asymmetry is deliberate and worth stating: a newly added recording
// action that is MISSING from this map produces a false gap, so the map
// must be extended in the same change that adds the action. A newly added
// action that IS listed but did not really record produces a missed
// opportunity, which is merely conservative.
//
// This map is a single source of truth. The hunt's SQL predicate is
// GENERATED from it (recordingActionPredicate), so the detection and the
// documentation cannot drift apart, and
// TestP3RecordingActionVocabulary_IsDeclaredAndComplete pins it.
var recordingActions = map[string]map[string]bool{
	// Knowledge artifacts.
	"mpm_memory":    {"save": true},
	"mpm_theories":  {"propose": true},
	"mpm_decisions": {"record": true},
	"mpm_lessons":   {"propose": true},
	"mpm_work":      {"create": true, "note": true},
	"mpm_skills":    {"save": true},

	// Continuity and navigation: durable, and therefore recording.
	"mpm_handoff":    {"write": true},
	"mpm_context":    {"write_handoff": true},
	"mpm_topics":     {"create": true},
	"mpm_references": {"add": true, "import": true},
	"mpm_evidence":   {"add": true},
}

// recordingActionKeys returns the flattened "tool.action" keys of the
// recording vocabulary, sorted, so the SQL predicate and the error
// messages are deterministic.
func recordingActionKeys() []string {
	keys := make([]string, 0, len(recordingActions))
	for tool, actions := range recordingActions {
		for action := range actions {
			keys = append(keys, tool+"."+action)
		}
	}
	sort.Strings(keys)
	return keys
}

// recordingActionPredicate returns the SQL fragment and its bound
// arguments that select the recording calls.
//
// The fragment is `ti.tool_name || '.' || ti.action IN (?, ?, ...)` — the
// CALLER supplies the surrounding parentheses, because the fragment is
// embedded in a CASE expression and a stray ')' would close it early.
//
// SQLite's `||` binds tighter than IN, so the expression parses as
// `(ti.tool_name || '.' || ti.action) IN (...)`.
func recordingActionPredicate() (string, []interface{}) {
	keys := recordingActionKeys()
	placeholders := make([]string, len(keys))
	args := make([]interface{}, len(keys))
	for i, k := range keys {
		placeholders[i] = "?"
		args[i] = k
	}
	return "ti.tool_name || '.' || ti.action IN (" + strings.Join(placeholders, ",") + ")", args
}

// MissedOpportunityHunt detects sessions that did MPM-mediated work
// without recording anything durable.
type MissedOpportunityHunt struct {
	// MinInvocations is the least number of invocations a session must
	// have before it can be a candidate. Zero means the default.
	MinInvocations int
	// MaxCitations bounds how many invocations are cited as evidence.
	// The OLDEST are cited, so the citation set stays stable as a session
	// grows. Zero means the default.
	MaxCitations int
	// Limit bounds how many candidate sessions are reported per cycle.
	// Zero means the default.
	Limit int
}

const (
	// defaultMinInvocations is the least activity a session must show
	// before "it recorded nothing" is worth a reviewer's attention. A
	// single call is not a pattern.
	defaultMinInvocations = 5
	// defaultMaxCitations bounds the evidence payload. It is generous:
	// the live workspace's largest session has 32 invocations. The bound
	// exists so a pathological session cannot produce an unbounded
	// payload, not because the expected size is large.
	defaultMaxCitations = 200
	// defaultCandidateLimit bounds how many candidates one cycle
	// reports, so one busy workspace cannot flood the substrate.
	defaultCandidateLimit = 20
)

// Name identifies the hunt in logs and in the cycle report.
func (h *MissedOpportunityHunt) Name() string { return "missed_opportunity" }

// Run finds the candidate sessions and emits one pending proposal each.
//
// The hunt is READ-ONLY against `tool_invocations` and
// `artifact_provenance`; it writes nothing itself. Its findings are
// emitted through the critic's normal `mpm call` path, which is the only
// supported write path for domain content.
func (h *MissedOpportunityHunt) Run(ctx context.Context, a *Audit) ([]Finding, error) {
	minInvocations := h.MinInvocations
	if minInvocations <= 0 {
		minInvocations = defaultMinInvocations
	}
	maxCitations := h.MaxCitations
	if maxCitations <= 0 {
		maxCitations = defaultMaxCitations
	}
	limit := h.Limit
	if limit <= 0 {
		limit = defaultCandidateLimit
	}

	candidates, err := h.findCandidateSessions(ctx, a, minInvocations, limit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Fetch the citable invocations for the candidate sessions in ONE
	// query, then take each session's OLDEST ones (a stable set).
	invocationIDs, err := h.citableInvocations(ctx, a, candidates, maxCitations)
	if err != nil {
		return nil, err
	}

	findings := make([]Finding, 0, len(candidates))
	for _, sessionID := range candidates {
		ids := invocationIDs[sessionID]
		finding, ok := missedOpportunityProposal(sessionID, ids)
		if !ok {
			// The gap rule: where the only thing available is a gap,
			// there is no proposal. It is a finding for a human.
			a.log.Warn("missed_opportunity: candidate with nothing citable; "+
				"reporting as a human finding, not a proposal",
				"session_id", sessionID,
				"cycle", a.Cycle())
			continue
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

// missedOpportunitySession is one candidate session.
type missedOpportunitySession struct {
	SessionID string
	// Invocations is the session's total invocation count.
	Invocations int
	// AttributedArtifacts is how many artifact_provenance rows join to
	// the session's invocations.
	AttributedArtifacts int
	// RecordingCalls is how many of the session's calls were successful
	// recording calls.
	RecordingCalls int
}

// findCandidateSessions returns the session ids that qualify as
// missed-opportunity candidates.
//
// A session qualifies when it has enough invocations, AND no artifact is
// attributable to it by the join, AND it made no successful recording
// call. All three conditions are disqualifying when MET.
func (h *MissedOpportunityHunt) findCandidateSessions(ctx context.Context, a *Audit, minInvocations, limit int) ([]string, error) {
	predicate, args := recordingActionPredicate()

	// The recording-call condition, built once and used twice (in the
	// SELECT list and in the HAVING clause). The parentheses around the
	// predicate are supplied HERE, because the fragment is a bare
	// expression that must not close the CASE's own parenthesis early.
	recordingCond := "ti.result_status = 'success' AND (" + predicate + ")"

	// The recording-call count is a bound parameter, so the query text is
	// built with a fixed number of placeholders. The `||` operator is
	// SQLite's string concatenation.
	//
	// PLACEHOLDER ORDER MATTERS. SQLite binds placeholders in the order
	// they appear in the TEXT, and this query's text order is:
	//
	//	1. the recording condition in the SELECT list  (N placeholders)
	//	2. the recording condition in the HAVING clause (N placeholders)
	//	3. the minimum invocation count               (1 placeholder)
	//	4. the limit                                   (1 placeholder)
	//
	// so the arguments are bound in exactly that order below. The HAVING
	// conditions are written recording-first for that reason.
	query := `
		SELECT ti.framework_session_id,
		       COUNT(DISTINCT ti.id)         AS invocations,
		       COUNT(DISTINCT ap.artifact_id) AS attributed_artifacts,
		       SUM(CASE WHEN ` + recordingCond + `
		                THEN 1 ELSE 0 END)      AS recording_calls
		FROM tool_invocations ti
		LEFT JOIN artifact_provenance ap ON ap.invocation_id = ti.invocation_id
		WHERE coalesce(ti.framework_session_id, '') <> ''
		GROUP BY ti.framework_session_id
		HAVING SUM(CASE WHEN ` + recordingCond + `
		                THEN 1 ELSE 0 END) = 0
		   AND COUNT(DISTINCT ap.artifact_id) = 0
		   AND COUNT(DISTINCT ti.id) >= ?
		ORDER BY invocations DESC, ti.framework_session_id ASC
		LIMIT ?`

	// The predicate appears twice, so its arguments are bound twice.
	bound := make([]interface{}, 0, len(args)*2+2)
	bound = append(bound, args...)
	bound = append(bound, args...)
	bound = append(bound, minInvocations, limit)

	rows, err := a.DB().QueryContext(ctx, query, bound...)
	if err != nil {
		return nil, fmt.Errorf("missed_opportunity: query candidate sessions: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			sessionID          string
			invocations        int
			attributedArtifact int
			recordingCalls     int
		)
		if err := rows.Scan(&sessionID, &invocations, &attributedArtifact, &recordingCalls); err != nil {
			return nil, fmt.Errorf("missed_opportunity: scan candidate session: %w", err)
		}
		out = append(out, sessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("missed_opportunity: iterate candidate sessions: %w", err)
	}
	return out, nil
}

// citableInvocations returns, per candidate session, the ids of the
// invocations the proposal will cite.
//
// The OLDEST invocations are taken, up to maxCitations, so the citation
// SET is stable as the session grows: a later cycle re-cites the same
// rows, the proposal's identity is unchanged, and the typed citation
// write is a no-op. The citations are `tool_invocations.id` values —
// that is the column P2's 'tool_invocation' source type resolves
// against.
func (h *MissedOpportunityHunt) citableInvocations(ctx context.Context, a *Audit, sessions []string, maxCitations int) (map[string][]string, error) {
	out := make(map[string][]string, len(sessions))
	if len(sessions) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(sessions))
	args := make([]interface{}, len(sessions))
	for i, s := range sessions {
		placeholders[i] = "?"
		args[i] = s
	}

	query := `
		SELECT framework_session_id, id
		FROM tool_invocations
		WHERE framework_session_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY framework_session_id ASC, started_at ASC, id ASC`

	rows, err := a.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("missed_opportunity: query session invocations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID, id string
		if err := rows.Scan(&sessionID, &id); err != nil {
			return nil, fmt.Errorf("missed_opportunity: scan session invocation: %w", err)
		}
		if len(out[sessionID]) >= maxCitations {
			continue
		}
		out[sessionID] = append(out[sessionID], id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("missed_opportunity: iterate session invocations: %w", err)
	}
	return out, nil
}

// missedOpportunityProposal builds the pending proposal for a candidate
// session.
//
// It returns ok=false when there is nothing to cite. That is the gap
// rule's explicit case: a proposal whose only evidence would be a gap is
// not promotable under M5 as written, so it is a finding for a human
// rather than a proposal.
//
// Every identity-bearing field is a PURE FUNCTION of the session id:
//
//   - the hypothesis names the session and never a count (a count grows,
//     and the content hash is part of the identity);
//   - the tags carry no cycle marker;
//   - the validation criteria carry no cycle marker.
//
// The cited invocations are what the proposal RESTS ON. They are not the
// gap: the gap is the trigger, and it is never cited.
func missedOpportunityProposal(sessionID string, invocationIDs []string) (Finding, bool) {
	if sessionID == "" {
		return Finding{}, false
	}
	if len(invocationIDs) == 0 {
		return Finding{}, false
	}

	hypothesis := fmt.Sprintf(
		"Session %s performed MPM-mediated work whose tool invocations are cited here. "+
			"None of the cited calls recorded anything durable, and MPM attributed no artifact to any of them, "+
			"so a knowledge-capture opportunity may exist for this session. "+
			"This proposal asserts only what the cited invocations show: it does not assert that any particular "+
			"lesson exists, it is not evidence that one does, and it is never promoted automatically.",
		sessionID)

	validationCriteria := fmt.Sprintf(
		"Before this proposal is promoted, a reviewer must determine what knowledge session %s did not capture, "+
			"and must cite that knowledge as resolvable evidence. Until then it stays pending at the base confidence: "+
			"the absence of a record is a candidate opportunity, never evidence that a lesson exists.",
		sessionID)

	return Finding{
		Tool:   "mpm_theories",
		Action: "propose",
		Reason: fmt.Sprintf(
			"session %s performed %d MPM-mediated calls with no recording call and no attributable artifact",
			sessionID, len(invocationIDs)),
		Payload: map[string]interface{}{
			"hypothesis":          hypothesis,
			"validation_criteria": validationCriteria,
			"tags": []string{
				"critic",
				"missed_opportunity",
				"session:" + sessionID,
			},
			// The typed citation: P2's 'tool_invocation' source type
			// resolves each cited id against tool_invocations, so an id
			// that does not exist REJECTS the proposal and leaves no
			// partial record behind.
			"source_ids":  invocationIDs,
			"source_type": "tool_invocation",
		},
		Priority: 1,
	}, true
}
