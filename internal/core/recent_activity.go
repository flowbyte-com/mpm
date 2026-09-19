// recent_activity.go — semantic recent activity query over tool_invocations.
//
// This module implements the canonical recent_activity surface:
//   - reads from tool_invocations (the only persistent tool-call audit);
//   - applies EffectiveActorKind for historical-normalization correctness;
//   - applies ClassifyAction to include only semantic mutating actions;
//   - enriches with domain tables where deterministic linkage exists.
//
// It is read-only. No DB writes except for its own tool_invocations
// row (recorded by the audit hook, classified as read_only so it
// never appears recursively).
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RecentActivityEvent is the public wire shape for one semantic
// activity record. Stable field names — do not rename without
// bumping ContextVersion.
type RecentActivityEvent struct {
	ID              string `json:"id"`
	Timestamp       int64  `json:"timestamp"`     // unix seconds, completed_at
	ActorKind       string `json:"actor_kind"`    // semantic (effective)
	RawActorKind    string `json:"raw_actor_kind,omitempty"`
	ActorID         string `json:"actor_id,omitempty"`
	FrameworkName   string `json:"framework_name,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	InvocationID    string `json:"invocation_id,omitempty"`
	ParentInvocationID string `json:"parent_invocation_id,omitempty"`
	Tool            string `json:"tool"`
	Action          string `json:"action"`
	Category        string `json:"category"`        // mpm_memory / mpm_work / etc.
	ArtifactType    string `json:"artifact_type,omitempty"`
	ArtifactID      string `json:"artifact_id,omitempty"`
	Status          string `json:"status"`          // success / error
	Summary         string `json:"summary"`         // bounded, secret-safe
	EnrichmentHint  string `json:"enrichment_hint,omitempty"` // e.g. "work_id:abc123"
}

// RecentActivityQueryParams is the bounded query input. Bounded by
// design — no unbounded history, no crafted DSL.
type RecentActivityQueryParams struct {
	Limit         int    // default 20, hard max 100
	Since         int64  // unix seconds; 0 = no lower bound
	ActorKind     string // filter by effective actor_kind (human/agent/all)
	FrameworkName string // exact match
	SessionID     string // exact match
	ArtifactType  string // exact match on category
	IncludeSystem bool   // if true, also include maintenance/diagnostic actions
}

// RecentActivityDefaults holds the canonical default/hard limits.
const (
	RecentActivityDefaultLimit = 20
	RecentActivityHardMaxLimit = 100
)

// RecentActivity returns the bounded recent semantic activity
// stream for the calling agent. Read-only: never mutates DB state
// other than its own tool_invocations row (which is excluded by
// ClassifyAction).
func (dm *DatabaseManager) RecentActivity(p RecentActivityQueryParams) ([]RecentActivityEvent, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("RecentActivity: db not initialized")
	}

	// Bound the limit.
	limit := p.Limit
	if limit <= 0 {
		limit = RecentActivityDefaultLimit
	}
	if limit > RecentActivityHardMaxLimit {
		limit = RecentActivityHardMaxLimit
	}

	// Build WHERE clause incrementally.
	where := []string{"1=1"}
	args := []interface{}{}
	if p.Since > 0 {
		where = append(where, "started_at >= ?")
		args = append(args, p.Since)
	}
	if p.FrameworkName != "" {
		where = append(where, "framework_name = ?")
		args = append(args, p.FrameworkName)
	}
	if p.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, p.SessionID)
	}

	// Fetch a generous raw candidate pool: tool_invocations may
	// contain reads that we filter post-classification. We over-fetch
	// by a bounded factor so we still have mutating rows after the
	// filter, then trim to limit. Hard cap at 4x to keep the read
	// bounded regardless of caller-supplied limit.
	fetchCap := limit * 4
	if fetchCap > RecentActivityHardMaxLimit*4 {
		fetchCap = RecentActivityHardMaxLimit * 4
	}

	q := `
		SELECT id, session_id, tool_name, action, invocation_id,
		       actor_kind, framework_name, payload_hash, result_status,
		       started_at, completed_at, duration_ms, error_message
		FROM tool_invocations
		WHERE ` + joinWhere(where) + `
		ORDER BY completed_at DESC, id DESC
		LIMIT ?`
	args = append(args, fetchCap)

	rows, err := dm.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("RecentActivity query: %w", err)
	}
	defer rows.Close()

	out := make([]RecentActivityEvent, 0, limit)
	for rows.Next() {
		var (
			id, sessID, tool, act, invID, actKind, fwk, pHash, status, startedAt, completedAt string
			dur                                                                                sql.NullInt64
			errMsg                                                                             sql.NullString
		)
		if err := rows.Scan(&id, &sessID, &tool, &act, &invID,
			&actKind, &fwk, &pHash, &status,
			&startedAt, &completedAt, &dur, &errMsg); err != nil {
			return nil, fmt.Errorf("RecentActivity scan: %w", err)
		}

		// 1. Filter failed invocations out of the default semantic
		//    activity feed. A failed save is a non-event — no
		//    semantic durable change happened. Callers wanting
		//    failures can join tool_invocations directly.
		if status == "error" {
			continue
		}

		// 2. Classify the action.
		cls := ClassifyAction(tool, act)

		// 3. Exclude by default: read_only, lifecycle delivery,
		//    diagnostic, maintenance. include_system opts into
		//    diagnostic + maintenance but never read_only.
		switch cls {
		case ActionClassMutating:
			// keep
		case ActionClassLifecycle, ActionClassDiagnostic, ActionClassMaintenance:
			if !p.IncludeSystem {
				continue
			}
		case ActionClassReadOnly:
			continue
		default:
			continue
		}

		// 3. Effective actor classification (historical normalization).
		effectiveKind := EffectiveActorKind(actKind, fwk)

		// 4. Actor_kind filter (post-classification).
		if p.ActorKind != "" && p.ActorKind != "all" {
			if p.ActorKind != effectiveKind {
				continue
			}
		} else if p.ActorKind == "" {
			// Default: agent + human only. System excluded.
			if effectiveKind != ActorKindAgent && effectiveKind != ActorKindHuman {
				continue
			}
		}

		// 5. ArtifactType filter (post-classification; == tool category).
		if p.ArtifactType != "" && p.ArtifactType != tool {
			continue
		}

		// 6. Build the event.
		ts, _ := parseUnixSec(completedAt)
		ev := RecentActivityEvent{
			ID:            id,
			Timestamp:     ts,
			ActorKind:     effectiveKind,
			RawActorKind:  actKind,
			FrameworkName: fwk,
			SessionID:     sessID,
			InvocationID:  invID,
			Tool:          tool,
			Action:        act,
			Category:      tool,
			Status:        status,
			Summary:       activitySummary(tool, act, effectiveKind),
		}
		if errMsg.Valid && errMsg.String != "" {
			ev.Status = "error"
		}

		// 7. Deterministic enrichment: pull artifact id from the
		//    per-tool tables where correlation is by invocation_id.
		if artifactID, hint := dm.enrichActivityEvent(ev); artifactID != "" {
			ev.ArtifactID = artifactID
			ev.ArtifactType = tool
			ev.EnrichmentHint = hint
		}

		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// activitySummary produces a bounded, secret-safe semantic summary
// from (tool, action). NEVER includes payload content; the summary
// is structural metadata only.
func activitySummary(tool, action, actor string) string {
	switch {
	case tool == "mpm_memory" && action == "save":
		return "saved memory"
	case tool == "mpm_memory" && action == "shred":
		return "shredded memory"
	case tool == "mpm_memory" && action == "delete":
		return "soft-deleted memory"
	case tool == "mpm_memory" && action == "restore":
		return "restored memory"
	case tool == "mpm_memory" && action == "challenge":
		return "challenged memory"
	case tool == "mpm_memory" && action == "commit_milestone":
		return "committed milestone"
	case tool == "mpm_lessons" && action == "save":
		return "saved lesson"
	case tool == "mpm_lessons" && action == "shred":
		return "shredded lesson"
	case tool == "mpm_theories" && action == "propose":
		return "proposed theory"
	case tool == "mpm_theories" && action == "resolve":
		return "resolved theory"
	case tool == "mpm_decisions" && action == "record":
		return "recorded decision"
	case tool == "mpm_decisions" && action == "supersede":
		return "superseded decision"
	case tool == "mpm_decisions" && action == "invalidate":
		return "invalidated decision"
	case tool == "mpm_topics" && action == "create":
		return "created topic"
	case tool == "mpm_topics" && action == "link":
		return "linked topic"
	case tool == "mpm_topics" && action == "unlink":
		return "unlinked topic"
	case tool == "mpm_references" && action == "save":
		return "saved reference"
	case tool == "mpm_references" && action == "delete":
		return "deleted reference"
	case tool == "mpm_evidence" && action == "save":
		return "saved evidence"
	case tool == "mpm_evidence" && action == "attach":
		return "attached evidence"
	case tool == "mpm_confidence" && action == "update":
		return "updated confidence"
	case tool == "mpm_handoff" && action == "write":
		return "wrote session handoff"
	case tool == "mpm_handoff" && action == "shred":
		return "shredded handoff"
	case tool == "mpm_scratchpad" && action == "flush":
		return "flushed scratchpad"
	case tool == "mpm_scratchpad" && action == "promote":
		return "promoted scratchpad"
	case tool == "mpm_scratchpad" && action == "discard":
		return "discarded scratchpad"
	case tool == "mpm_work" && action == "create":
		return "created work item"
	case tool == "mpm_work" && action == "update":
		return "updated work item"
	case tool == "mpm_work" && action == "complete":
		return "completed work item"
	case tool == "mpm_work" && action == "cancel":
		return "cancelled work item"
	case tool == "mpm_work" && action == "note":
		return "appended work note"
	case tool == "mpm_work" && action == "reopen":
		return "reopened work item"
	case tool == "mpm_work" && action == "resolve_contradiction":
		return "resolved work contradiction"
	case tool == "mpm_wakes" && action == "schedule":
		return "scheduled wake"
	case tool == "mpm_wakes" && action == "resolve":
		return "resolved wake"
	case tool == "mpm_wakes" && action == "upsert_task":
		return "upserted wake task"
	case tool == "mpm_wakes" && action == "delete_task":
		return "deleted wake task"
	case tool == "mpm_skills" && action == "save":
		return "saved skill"
	case tool == "mpm_skills" && action == "workshop":
		return "ran skill workshop"
	case tool == "mpm_context" && action == "record_global_rule":
		return "recorded global rule"
	case tool == "mpm_context" && action == "retire_global_rule":
		return "retired global rule"
	case tool == "mpm_context" && action == "promote_to_global":
		return "promoted to global"
	case tool == "mpm_context" && action == "write_handoff":
		return "wrote session handoff"
	}
	return tool + "/" + action
}

// enrichActivityEvent looks up domain-table linkage by invocation_id
// where deterministic. Returns (artifact_id, hint) or ("", "") when
// no linkage can be found without timestamp heuristics.
func (dm *DatabaseManager) enrichActivityEvent(ev RecentActivityEvent) (string, string) {
	if ev.InvocationID == "" {
		return "", ""
	}
	// work_events: append-only ledger; first hit per invocation_id wins.
	if ev.Tool == "mpm_work" {
		var workID, eventType string
		err := dm.db.QueryRow(
			`SELECT work_id, event_type FROM work_events
			 WHERE invocation_id = ? ORDER BY event_index ASC LIMIT 1`,
			ev.InvocationID,
		).Scan(&workID, &eventType)
		if err == nil && workID != "" {
			hint := "work_event:" + eventType
			return workID, hint
		}
	}
	return "", ""
}

// joinWhere concatenates WHERE predicates with AND.
func joinWhere(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}

// parseUnixSec parses a unix-seconds integer string. Returns 0 on
// parse failure (defensive — renderers tolerate zero timestamps).
func parseUnixSec(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RecentActivityJSON is a convenience helper for callers that want
// the activity stream as compact JSON. Never returns payload content.
func (dm *DatabaseManager) RecentActivityJSON(p RecentActivityQueryParams) (string, error) {
	events, err := dm.RecentActivity(p)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(events)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RecentActivityMaxAge returns the canonical default lookback
// duration when no Since is supplied. 30 days is the substrate's
// common retention window.
const RecentActivityMaxAge = 30 * 24 * time.Hour