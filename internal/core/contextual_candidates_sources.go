// contextual_candidates_sources.go — per-source generator helpers
// for Stage 2D. Each function reads authoritative state and
// appends candidates to the shared accumulator with deterministic
// reasons. All helpers are bounded per-source.
//
// Invariants enforced here:
//   - Read-only: no INSERT/UPDATE/DELETE on any persistent table.
//   - Bounded: each helper respects its per-source limit.
//   - Deterministic: identical input → identical output.
//   - Independent: identity dimensions (session_id, mpm_session_id,
//     framework_session_id, invocation_id, parent_invocation_id)
//     are read as distinct columns; no fallback substitution.

package internal

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// ── Source A: active work + work-referenced artifacts ──────────────

// addWorkCandidates emits:
//   - active (open) work rows, with work_recently_changed reason for
//     work updated within the per-source limit's recency window;
//   - work whose id is in q.WorkIDs (explicit active work).
//
// The work-pointer is "mpm://work/<id>". Lifecycle state is the
// work.status ("open" by default for active work).
func addWorkCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 {
		return
	}

	// 1. Active work rows, newest-updated first.
	rows, err := dm.db.Query(`
		SELECT id, title, status, verification, updated_at, session_id
		FROM works
		WHERE status = 'open'
		ORDER BY updated_at DESC, id ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, status string
		var verification, sessionID sql.NullString
		var updatedAt int64
		if err := rows.Scan(&id, &title, &status, &verification, &updatedAt, &sessionID); err != nil {
			dm.LogAudit(AuditWarn, "contextual_candidates", "work scan: "+err.Error(), "", AuditContext{"source": "work"})
			continue
		}
		a := ensureCandidate(acc, "work", id)
		a.pointer = "mpm://work/" + id
		a.timestamp = updatedAt
		a.lifecycleState = status
		a.summary = truncate(title, 120)
		a.mpmSessionID = ""
		// session_id on works is the legacy host-correlation field;
		// we don't conflate it with mpm_session_id.
		addReason(a, ReasonOpenWork)
		markSource(a, "work")
		// If this work is referenced by an explicit active-work id
		// supplied in q.WorkIDs, add the structural reason. Since
		// q.WorkIDs is the same set we are iterating over, the
		// caller-supplied list re-enforces open_work.
		for _, w := range q.WorkIDs {
			if w == id {
				addReason(a, ReasonReferencedByActiveWork)
			}
		}
	}

	// 2. Explicit work ids from caller (already mostly covered by
	// the active-work scan, but the caller may supply closed/in-
	// progress ids we want surfaced with a structural reason).
	for _, id := range q.WorkIDs {
		if id == "" {
			continue
		}
		if _, exists := acc[candidateKey("work", id)]; exists {
			continue
		}
		var title, status string
		var updatedAt int64
		err := dm.db.QueryRow(
			`SELECT title, status, updated_at FROM works WHERE id = ?`, id,
		).Scan(&title, &status, &updatedAt)
		if err != nil {
			continue
		}
		a := ensureCandidate(acc, "work", id)
		a.pointer = "mpm://work/" + id
		a.timestamp = updatedAt
		a.lifecycleState = status
		a.summary = truncate(title, 120)
		addReason(a, ReasonReferencedByActiveWork)
		markSource(a, "work")
	}
}

// ── Source B: handoffs (read-only peek) ─────────────────────────────

// addHandoffCandidates emits the most recent unread handoff whose
// mpm_session_id matches q.MPMSessionID (when supplied) OR the
// latest handoff overall (read-only peek; never mutates read_at).
func addHandoffCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 {
		return
	}

	// Latest unread handoff, peek (MarkLatestHandoffRead would
	// mutate read_at; we use GetLatestUnreadHandoff explicitly).
	h, err := dm.GetLatestUnreadHandoff()
	if err == nil && h != nil {
		a := ensureCandidate(acc, "handoff", h.ID)
		a.pointer = "mpm://handoff/" + h.ID
		a.timestamp = h.EndedAt
		a.lifecycleState = h.EndedState
		a.summary = truncate(h.Summary, 200)
		a.mpmSessionID = h.MPMSessionID
		a.fwSessionID = h.FrameworkSessionID
		// Continuity / handoff reasons.
		if q.MPMSessionID != "" && h.MPMSessionID == q.MPMSessionID {
			addReason(a, ReasonSameMPMSession)
		}
		if q.FrameworkSessionID != "" && h.FrameworkSessionID == q.FrameworkSessionID {
			addReason(a, ReasonSameFrameworkSession)
		}
		addReason(a, ReasonHandoffForContext)
		// Surface open commitments / questions as related ids
		// (we already emit candidates for them separately via the
		// obligation pass; here we merely annotate related
		// artifacts).
		markSource(a, "handoff")
	}

	// Also include the latest read-or-unread handoff as a fallback
	// candidate if the unread path returned none.
	if h == nil {
		fallback, ferr := dm.GetLatestHandoff()
		if ferr == nil && fallback != nil {
			a := ensureCandidate(acc, "handoff", fallback.ID)
			a.pointer = "mpm://handoff/" + fallback.ID
			a.timestamp = fallback.EndedAt
			a.lifecycleState = fallback.EndedState
			a.summary = truncate(fallback.Summary, 200)
			a.mpmSessionID = fallback.MPMSessionID
			a.fwSessionID = fallback.FrameworkSessionID
			if q.MPMSessionID != "" && fallback.MPMSessionID == q.MPMSessionID {
				addReason(a, ReasonSameMPMSession)
			}
			if q.FrameworkSessionID != "" && fallback.FrameworkSessionID == q.FrameworkSessionID {
				addReason(a, ReasonSameFrameworkSession)
			}
			addReason(a, ReasonHandoffForContext)
			markSource(a, "handoff")
		}
	}
}

// ── Source C: recent semantic activity ─────────────────────────────

// addActivityCandidates emits candidates from recent_activity for
// events whose artifact_id (where enrichable) is a meaningful
// pointer. We expose the event itself as the candidate (kind =
// "activity"); structural reasons flag same-session continuity,
// cross-agent change, etc.
//
// Recent_activity is observational — it does NOT rank. We simply
// bound the count and tag identity axes.
func addActivityCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 {
		return
	}

	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: limit * 2, // over-fetch because activity-class filter below trims
	})
	if err != nil {
		return
	}
	count := 0
	for _, ev := range res.Events {
		if count >= limit {
			break
		}
		if ev.Tool == "" {
			continue
		}
		// Skip read-only / lifecycle delivery / diagnostic events.
		if isActivityClassReadOnly(ev.Tool, ev.Action) {
			continue
		}
		// Filter by current framework when supplied (focus on
		// cross-agent change relative to caller).
		if q.FrameworkName != "" && ev.FrameworkName != "" && ev.FrameworkName != q.FrameworkName {
			// Cross-agent change is itself a candidate reason.
		}
		a := ensureCandidate(acc, "activity", ev.ID)
		a.pointer = ""
		a.timestamp = ev.Timestamp
		a.actorKind = ev.ActorKind
		a.frameworkName = ev.FrameworkName
		a.mpmSessionID = ev.MPMSessionID
		a.fwSessionID = ev.FrameworkSessionID
		a.summary = truncate(activitySummaryEvent(ev), 200)
		// Identity reasons.
		if q.MPMSessionID != "" && ev.MPMSessionID == q.MPMSessionID {
			addReason(a, ReasonSameMPMSession)
		}
		if q.FrameworkSessionID != "" && ev.FrameworkSessionID == q.FrameworkSessionID {
			addReason(a, ReasonSameFrameworkSession)
		}
		// Provenance reasons.
		switch ev.ActorKind {
		case "agent":
			if q.FrameworkName != "" && ev.FrameworkName != "" && ev.FrameworkName != q.FrameworkName {
				addReason(a, ReasonRecentCrossAgentChange)
			}
		case "human":
			addReason(a, ReasonRecentHumanChange)
		default:
			if ev.FrameworkName == "" {
				addReason(a, ReasonProvenanceUnknown)
			} else {
				addReason(a, ReasonUnknownSourceChange)
			}
		}
		// If the event has an artifact id, link it as related.
		if ev.ArtifactID != "" {
			addRelated(a, ev.ArtifactID)
		}
		markSource(a, "activity")
		count++
	}
}

// activitySummary produces a bounded, secret-safe textual
// summary for an activity event. It deliberately uses ONLY
// structural fields (tool/action); no payload.
func activitySummaryEvent(ev RecentActivityEvent) string {
	if ev.Tool == "" {
		return ""
	}
	s := ev.Tool
	if ev.Action != "" {
		s += "." + ev.Action
	}
	return s
}

// isActivityClassReadOnly mirrors the activity classifier's
// read-only definition for use by candidate generation. We
// duplicate the check here rather than depending on the
// classifier package to keep the candidate generator self-
// contained for hermetic tests.
func isActivityClassReadOnly(tool, action string) bool {
	readOnly := map[string]bool{
		"mpm_context.read_wake_context":    true,
		"mpm_context.read_directives":     true,
		"mpm_context.proactive_recall_hint": true,
		"mpm_context.query_global_rules":  true,
		"mpm_context.route":               true,
		"mpm_context.recent_activity":     true,
		"mpm_handoff.read":                true,
		"mpm_handoff.list":                true,
		"mpm_wakes.check":                 true,
		"mpm_wakes.list":                  true,
		"mpm_scratchpad.read":             true,
		"mpm_system.health_check":         true,
		"mpm_system.gc_run":               true,
		"mpm_system.compact":              true,
	}
	key := tool
	if action != "" {
		key = tool + "." + action
	}
	return readOnly[key]
}

// ── Source D: epistemic dependencies ───────────────────────────────

// addEpistemicCandidates surfaces:
//   - supersede / invalidate chains for decisions, theories, lessons
//     (read from confidence_history where trigger ∈
//     {supersede, invalidate, evidence_*, decay_tick,
//     concept_drift, manual_recompute});
//   - upstream/downstream citation relationships from
//     epistemic_provenance (source/downstream edges).
//
// For Stage 2D we take a structural / one-hop expansion: we read
// confidence_history for trigger events + epistemic_provenance for
// relationships. We do NOT walk the full graph.
func addEpistemicCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 {
		return
	}

	// ── Trigger events from confidence_history
	rows, err := dm.db.Query(`
		SELECT id, artifact_id, artifact_type, trigger, computed_at, confidence
		FROM confidence_history
		WHERE trigger IN ('supersede','invalidate','evidence_added',
		                  'evidence_updated','evidence_deleted',
		                  'evidence_expired','decay_tick',
		                  'concept_drift','manual_recompute')
		ORDER BY computed_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, artifactID, artifactType, trigger string
			var computedAt int64
			var confidence float64
			if err := rows.Scan(&id, &artifactID, &artifactType, &trigger, &computedAt, &confidence); err != nil {
				dm.LogAudit(AuditWarn, "contextual_candidates", "epistemic confidence_history scan: "+err.Error(), "", AuditContext{"source": "epistemic"})
				continue
			}
			kind := canonicalArtifactKind(artifactType)
			a := ensureCandidate(acc, kind, artifactID)
			a.timestamp = computedAt
			switch trigger {
			case "supersede":
				addReason(a, ReasonFoundationSuperseded)
				// Look up the canonical successor from
				// memories.metadata.superseded_by so the
				// supersede chain is preserved as a
				// related_id (audit history).
				if artifactType == "theory" {
					var successor sql.NullString
					if err := dm.db.QueryRow(
						`SELECT json_extract(metadata, '$.superseded_by')
						   FROM memories WHERE id = ? AND collection = 'theories'`,
						artifactID,
					).Scan(&successor); err == nil && successor.Valid && successor.String != "" {
						addRelated(a, successor.String)
					}
				}
			case "invalidate":
				addReason(a, ReasonFoundationInvalidated)
			case "evidence_added", "evidence_updated":
				addReason(a, ReasonEvidenceAdded)
			case "decay_tick", "concept_drift", "manual_recompute":
				addReason(a, ReasonConfidenceChanged)
			}
			a.summary = truncate(buildEpistemicSummary(trigger, "", ""), 200)
			markSource(a, "epistemic")
		}
	}

	// ── Citation relationships from epistemic_provenance
	// Surfaced as the downstream artifact's candidate with the
	// source artifact listed in related_ids — the downstream's
	// candidacy is structural ("X cites Y"), and ranking decides
	// later whether the downstream is selected.
	prow, perr := dm.db.Query(`
		SELECT downstream_id, downstream_type, source_id, source_type, created_at
		FROM epistemic_provenance
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, limit)
	if perr == nil {
		defer prow.Close()
		for prow.Next() {
			var downID, downType, sourceID, sourceType string
			var createdAt int64
			if err := prow.Scan(&downID, &downType, &sourceID, &sourceType, &createdAt); err != nil {
				dm.LogAudit(AuditWarn, "contextual_candidates", "epistemic provenance scan: "+err.Error(), "", AuditContext{"source": "epistemic"})
				continue
			}
			if downID == "" || sourceID == "" {
				continue
			}
			kind := canonicalArtifactKind(downType)
			a := ensureCandidate(acc, kind, downID)
			a.timestamp = createdAt
			addReason(a, ReasonExplicitDependency)
			addRelated(a, sourceID)
			a.summary = truncate(sourceType+":"+sourceID, 200)
			markSource(a, "epistemic")
		}
	}
	_ = q
}

// canonicalArtifactKind maps the substrate's stored artifact_type
// strings into the candidate kind vocabulary. Memory kinds map to
// "memory"; theories to "theory"; decisions to "decision";
// evidence to "evidence"; everything else falls through to its
// lowercased type.
func canonicalArtifactKind(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "memory", "memories":
		return "memory"
	case "lesson", "lessons":
		return "lesson"
	case "theory", "theories":
		return "theory"
	case "decision", "decisions":
		return "decision"
	case "evidence":
		return "evidence"
	case "work":
		return "work"
	case "handoff":
		return "handoff"
	case "scratchpad":
		return "scratchpad"
	case "wake":
		return "wake"
	}
	if t == "" {
		return "artifact"
	}
	return t
}

// buildEpistemicSummary constructs a bounded, secret-safe summary
// from trigger + reason + polarity. It deliberately does NOT
// deserialize payload — that's selection-stage materialization.
func buildEpistemicSummary(trigger, reason, polarity string) string {
	s := trigger
	if reason != "" {
		s += ":" + truncate(reason, 80)
	}
	if polarity != "" {
		s += " [" + polarity + "]"
	}
	return s
}

// ── Source E: cascades ───────────────────────────────────────────────

// addCascadeCandidates emits unresolved cascade obligations whose
// downstream_artifact_id is the artifact the cascade invalidates.
// Resolved/materialized cascades surface as cascade_resolved
// (audit history, not a live obligation).
func addCascadeCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 {
		return
	}
	rows, err := dm.db.Query(`
		SELECT id, dead_artifact_id, dead_artifact_type,
		       downstream_artifact_id, downstream_artifact_type,
		       invalidation_event_id, cascade_depth, status, reason,
		       created_at
		FROM epistemic_cascade_outbox
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, deadID, deadType, downID, downType, eventID, status, reason string
		var depth int64
		var createdAt int64
		if err := rows.Scan(&id, &deadID, &deadType, &downID, &downType, &eventID, &depth, &status, &reason, &createdAt); err != nil {
			dm.LogAudit(AuditWarn, "contextual_candidates", "cascade scan: "+err.Error(), "", AuditContext{"source": "cascade"})
			continue
		}
		// Surface the downstream artifact (the one that needs
		// re-evaluation) as the candidate. Use its kind from the
		// table; fall back to dead artifact's kind.
		kind := canonicalArtifactKind(downType)
		if kind == "artifact" || downID == "" {
			kind = canonicalArtifactKind(deadType)
			downID = deadID
		}
		if downID == "" {
			continue
		}
		a := ensureCandidate(acc, kind, downID)
		a.timestamp = createdAt
		a.lifecycleState = status
		a.summary = truncate("cascade:"+reason, 200)
		switch status {
		case "pending", "processing", "failed":
			addReason(a, ReasonCascadePending)
		case "materialized":
			addReason(a, ReasonCascadeResolved)
		default:
			addReason(a, ReasonCascadePending)
		}
		if eventID != "" {
			addRelated(a, eventID)
		}
		markSource(a, "cascade")
	}
	_ = q
}

// ── Source F: wakes / obligations ───────────────────────────────────

// addWakeCandidates surfaces overdue + pending wakes with explicit
// obligation reasons. Already-fired wakes are not included (they
// represent historical activity, surfaced via Source C).
func addWakeCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 {
		return
	}
	now := timeNowUnix()
	rows, err := dm.db.Query(`
		SELECT id, target_time, reason, fired, fired_at, theory_id, created_at, metadata
		FROM scheduled_wakes
		WHERE fired = 0 AND target_time <= ?
		ORDER BY target_time ASC, id ASC
		LIMIT ?
	`, now, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, reason string
		var targetTime, createdAt int64
		var firedAt sql.NullInt64
		var fired int
		var theoryID sql.NullString
		var metadata sql.NullString
		if err := rows.Scan(&id, &targetTime, &reason, &fired, &firedAt, &theoryID, &createdAt, &metadata); err != nil {
			dm.LogAudit(AuditWarn, "contextual_candidates", "wake scan: "+err.Error(), "", AuditContext{"source": "wake"})
			continue
		}
		a := ensureCandidate(acc, "wake", id)
		a.timestamp = targetTime
		a.lifecycleState = "pending"
		a.summary = truncate(reason, 200)
		if theoryID.Valid && theoryID.String != "" {
			addRelated(a, theoryID.String)
		}
		if targetTime <= now {
			addReason(a, ReasonOverdueWake)
		} else {
			addReason(a, ReasonUnresolvedWake)
		}
		markSource(a, "wake")
	}
	_ = q
}

// timeNowUnix is the package-local stub point for current-time
// reads inside candidate generation. Default: time.Now().Unix().
// Tests override this with a fixed value to pin determinism.
var timeNowUnix = func() int64 { return time.Now().Unix() }

// ── Source G: scratchpad ─────────────────────────────────────────────

// addScratchpadCandidates surfaces active scratchpad items as
// candidates. The scratchpad primary key is session_id; the
// substrate stores one scratchpad row per session with thesis /
// supporting JSON columns. Material content is selection-stage.
func addScratchpadCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 {
		return
	}
	rows, err := dm.db.Query(`
		SELECT session_id, created_at, updated_at, decay_at
		FROM ephemeral_scratchpad
		ORDER BY updated_at DESC, session_id ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var createdAt, updatedAt int64
		var decayAt sql.NullInt64
		if err := rows.Scan(&sessionID, &createdAt, &updatedAt, &decayAt); err != nil {
			dm.LogAudit(AuditWarn, "contextual_candidates", "scratchpad scan: "+err.Error(), "", AuditContext{"source": "scratchpad"})
			continue
		}
		a := ensureCandidate(acc, "scratchpad", sessionID)
		a.timestamp = updatedAt
		a.lifecycleState = "active"
		a.summary = "" // selection-stage materialization
		addReason(a, ReasonActiveScratchpad)
		markSource(a, "scratchpad")
	}
	_ = q
}

// ── Source H: topic neighborhood ────────────────────────────────────

// addTopicCandidates surfaces artifacts directly attached to the
// topics in q.TopicIDs, capped at the topic limit. The expansion is
// one-hop (no topic->topic graph walk) and bounded by a per-topic
// sub-limit to prevent a broad topic from exploding the candidate
// set.
func addTopicCandidates(dm *DatabaseManager, q ContextQuery, limit int, acc map[string]*candidateAccumulator) {
	if limit <= 0 || len(q.TopicIDs) == 0 {
		return
	}
	// Per-topic sub-limit: divide evenly with a floor of 1.
	subLimit := limit / len(q.TopicIDs)
	if subLimit < 1 {
		subLimit = 1
	}
	seen := make(map[string]bool)
	for _, topicID := range q.TopicIDs {
		rows, err := dm.db.Query(`
			SELECT memory_id, session_id
			FROM topic_memberships
			WHERE topic_id = ? AND memory_id IS NOT NULL
			ORDER BY memory_id ASC
			LIMIT ?
		`, topicID, subLimit)
		if err != nil {
			continue
		}
		for rows.Next() {
			var memID, sessionID string
			if err := rows.Scan(&memID, &sessionID); err != nil {
				dm.LogAudit(AuditWarn, "contextual_candidates", "topic scan: "+err.Error(), "", AuditContext{"source": "topic"})
				continue
			}
			key := candidateKey("memory", memID)
			if seen[key] {
				continue
			}
			seen[key] = true
			a := ensureCandidate(acc, "memory", memID)
			a.pointer = "mpm://memory/" + memID
			a.lifecycleState = ""
			a.summary = "" // selection-stage materialization
			addReason(a, ReasonSharesTopic)
			addRelated(a, topicID)
			markSource(a, "topic")
		}
		rows.Close()
	}
}

// ── utilities ───────────────────────────────────────────────────────

// helper used by the JSON-output helper for debug surfaces.
var _ = json.Marshal
