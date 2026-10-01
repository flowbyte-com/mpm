// recent_activity.go — semantic recent activity query over tool_invocations.
//
// This module implements the canonical recent_activity surface:
//   - reads from tool_invocations (the only persistent tool-call audit);
//   - applies EffectiveActorKind for historical-normalization correctness;
//   - applies ClassifyAction to include only semantic mutating actions;
//   - enriches with domain tables where deterministic linkage exists;
//   - uses bounded pagination so a sparse semantic feed can always
//     surface the newest matching events regardless of intervening
//     read-only traffic.
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
//
// Identity dimensions exposed on each event (Stage 2C.1):
//
//   SessionID            — legacy per-process dispatcher grouping
//                          (from getOrMakeSessionID). Distinct from
//                          canonical identities. Retained for
//                          back-compat.
//   MPMSessionID         — canonical MPM-owned continuity session.
//                          Empty when no active MPM session existed
//                          at audit time (or pre-Stage-2C.1 row).
//   FrameworkSessionID   — host-owned native session identity.
//                          Empty when host has no native session.
//   InvocationID         — per-call correlation ID. Unique per call.
//   ParentInvocationID   — causal lineage to the spawning invocation,
//                          if any. Empty for root invocations.
type RecentActivityEvent struct {
	ID                  string `json:"id"`
	Timestamp           int64  `json:"timestamp"`     // unix seconds, completed_at
	ActorKind           string `json:"actor_kind"`    // semantic (effective)
	RawActorKind        string `json:"raw_actor_kind,omitempty"`
	ActorID             string `json:"actor_id,omitempty"`
	FrameworkName       string `json:"framework_name,omitempty"`
	SessionID           string `json:"session_id,omitempty"`
	MPMSessionID        string `json:"mpm_session_id,omitempty"`
	FrameworkSessionID  string `json:"framework_session_id,omitempty"`
	InvocationID        string `json:"invocation_id,omitempty"`
	ParentInvocationID  string `json:"parent_invocation_id,omitempty"`
	Tool                string `json:"tool"`
	Action              string `json:"action"`
	Category            string `json:"category"`        // mpm_memory / mpm_work / etc.
	ArtifactType        string `json:"artifact_type,omitempty"`
	ArtifactID          string `json:"artifact_id,omitempty"`
	Status              string `json:"status"`          // success / error
	Summary             string `json:"summary"`         // bounded, secret-safe
	EnrichmentHint      string `json:"enrichment_hint,omitempty"` // e.g. "work_id:abc123"
}

// RecentActivityQueryParams is the bounded query input. Bounded by
// design — no unbounded history, no crafted DSL.
//
// Identity filters (Stage 2C.1) are independent exact-match columns.
// They MUST NOT collapse into one another:
//
//   SessionID           — filters tool_invocations.session_id
//                          (legacy per-process dispatcher grouping).
//   MPMSessionID        — filters tool_invocations.mpm_session_id
//                          (canonical MPM continuity session).
//   FrameworkSessionID  — filters tool_invocations.framework_session_id
//                          (host-owned native session).
//
// A query that supplies only SessionID does NOT consult the
// mpm_session_id column, and vice versa. The contract is exact-match
// per column; no proximity/timestamp/PID/framework-name inference.
type RecentActivityQueryParams struct {
	Limit              int    // default 20, hard max 100
	Since              int64  // unix seconds; 0 = no lower bound
	ActorKind          string // filter by effective actor_kind (human/agent/all/unknown)
	FrameworkName      string // exact match
	SessionID          string // exact match — legacy dispatcher grouping
	MPMSessionID       string // exact match — canonical MPM continuity session
	FrameworkSessionID string // exact match — host-owned native session
	ArtifactType       string // exact match on category (== tool_name)
	// IncludeSystem is retained for legacy callers and is now a no-op:
	// the substrate's tool_invocations does NOT comprehensively cover
	// cascade-materializer / cascade-reconciler / scheduler-retention
	// / GC / migration writes, so a parameter that promised "include
	// all system activity" would be misleading. The accurate surface
	// for system audit is mpm_system (query_audit_log, list_clusters)
	// — recent_activity now answers ONLY "what semantic durable
	// activity happened", which is what agents need for continuity.
	// The field is kept as a struct member so callers that pass it
	// silently compile and run; the handler logs a deprecation note.
	IncludeSystem bool
	// ResultStatus selects which persisted tool outcomes are visible.
	// One of RecentActivityStatusSuccess (the default), ...Error, or
	// ...All; the empty string means Success. See the constants below.
	//
	// The default is Success, and it is the default rather than a choice
	// because this field did not exist until failures were persisted but
	// unreadable: every caller written before it leaves the field at its
	// zero value, and each of them means "recent activity" as "what
	// succeeded". Returning failures to a caller that never asked would
	// change the meaning of the surface for all of them at once.
	ResultStatus string
}

// RecentActivityResultStatus is the closed vocabulary for
// RecentActivityQueryParams.ResultStatus.
//
// The two persisted values are not invented here: tool_invocations
// declares CHECK (result_status IN ('success','error')) and both
// writers derive the value from `err != nil`. "all" is the query-side
// wildcard that suppresses the predicate, matching the "all" spelling
// already used by the ActorKind filter in this same struct.
const (
	// RecentActivityStatusSuccess shows successful invocations only.
	// This is what an unset ResultStatus means.
	RecentActivityStatusSuccess = "success"
	// RecentActivityStatusError shows failed invocations only.
	RecentActivityStatusError = "error"
	// RecentActivityStatusAll shows both, suppressing the predicate.
	RecentActivityStatusAll = "all"
)

// appendResultStatusFilter adds the result_status predicate for the
// requested selector, or suppresses it entirely for StatusAll.
//
// An unrecognised value is an error rather than a fallback. Silently
// treating a typo as "all" would return failures to a caller that asked
// for something narrower, which is the very failure mode this parameter
// exists to let callers opt out of — and it would do so invisibly.
//
// The empty string resolves to Success rather than to All: it is the
// zero value every pre-existing caller carries, and the historical
// behaviour of this surface is the successful view. Empty is therefore a
// documented default, not a fourth spelling of All.
func appendResultStatusFilter(where []string, args []interface{}, status string) ([]string, []interface{}, error) {
	switch status {
	case "", RecentActivityStatusSuccess:
		return append(where, "result_status = ?"), append(args, RecentActivityStatusSuccess), nil
	case RecentActivityStatusError:
		return append(where, "result_status = ?"), append(args, RecentActivityStatusError), nil
	case RecentActivityStatusAll:
		return where, args, nil
	default:
		return nil, nil, fmt.Errorf(
			"recent_activity: result_status must be %q, %q, or %q; got %q",
			RecentActivityStatusSuccess, RecentActivityStatusError, RecentActivityStatusAll, status)
	}
}

// RecentActivityDefaults holds the canonical default/hard limits.
const (
	RecentActivityDefaultLimit = 20
	RecentActivityHardMaxLimit = 100
)

// Bounded-pagination constants for RecentActivityWithMeta.
//
// recentActivityPageSize: rows visited per iteration. Sized so a single
// page is well under the SQLite practical step cost while still
// amortising query-plan overhead.
//
// recentActivityScanCap: hard upper bound on raw rows visited per
// request. Even with 0.1% semantic density this surfaces ≥2 events
// per typical limit=100 query, and the worst case is honestly
// reported via Truncated=true.
//
// recentActivityScanMultiplier: per-request scan budget scales with
// the requested limit so small queries stay cheap and large queries
// still find their target.
const (
	recentActivityPageSize         = 200
	recentActivityScanCap          = 2000
	recentActivityScanMultiplier    = 20
)

// RecentActivityResult extends RecentActivity with scan metadata so
// callers can distinguish a true short history from a truncated scan.
//
//   - Events           : the matching semantic events, newest-first.
//   - HistoryExhausted : true when the scan walked every row in the
//                        bounded WHERE filter and stopped because the
//                        table ran out (not because a ceiling hit).
//   - Truncated        : true when the internal scan budget was hit
//                        before the requested number of matching
//                        events was collected; more matching events
//                        MAY exist further back in history.
//   - ScannedRows      : total raw tool_invocations rows visited
//                        across all pages.
//   - ScanLimit        : the safety ceiling applied (raw rows).
//
// Count is ALWAYS len(Events) — the wire envelope derives the count
// from the slice length to prevent the count/length drift class.
type RecentActivityResult struct {
	Events           []RecentActivityEvent `json:"events"`
	HistoryExhausted bool                  `json:"history_exhausted"`
	Truncated        bool                  `json:"truncated"`
	ScannedRows      int                   `json:"scanned_rows"`
	ScanLimit        int                   `json:"scan_limit"`
}

// RecentActivity returns the bounded recent semantic activity
// stream for the calling agent. Read-only: never mutates DB state
// other than its own tool_invocations row (which is excluded by
// ClassifyAction). Returns just the events slice; callers needing
// scan metadata use RecentActivityWithMeta.
func (dm *DatabaseManager) RecentActivity(p RecentActivityQueryParams) ([]RecentActivityEvent, error) {
	res, err := dm.RecentActivityWithMeta(p)
	if err != nil {
		return nil, err
	}
	return res.Events, nil
}

// RecentActivityWithMeta is the canonical implementation. It walks
// tool_invocations newest-first in fixed-size pages, applying the
// SQL-level filters that are safe to push down (result_status,
// since, framework_name, session_id, tool_name) and post-classifying
// each row for action class and effective actor. The scan terminates
// when one of:
//
//   - the requested number of matching events is collected,
//   - the table runs out of rows (HistoryExhausted=true),
//   - the safety ceiling is reached (Truncated=true).
//
// The scan budget is bounded by recentActivityScanCap; a request
// asking for limit=N can scan at most recentActivityScanCap raw
// rows. This means a sparse semantic feed can always find the
// newest N matching events (subject to the absolute scan cap), but
// also that extremely sparse histories honestly report Truncated.
func (dm *DatabaseManager) RecentActivityWithMeta(p RecentActivityQueryParams) (RecentActivityResult, error) {
	if dm == nil || dm.db == nil {
		return RecentActivityResult{}, fmt.Errorf("RecentActivity: db not initialized")
	}

	// Bound the limit.
	limit := p.Limit
	if limit <= 0 {
		limit = RecentActivityDefaultLimit
	}
	if limit > RecentActivityHardMaxLimit {
		limit = RecentActivityHardMaxLimit
	}

	// Compute scan budget: scale with limit but bound absolutely.
	scanLimit := limit * recentActivityScanMultiplier
	if scanLimit < recentActivityPageSize {
		scanLimit = recentActivityPageSize
	}
	if scanLimit > recentActivityScanCap {
		scanLimit = recentActivityScanCap
	}

	// Build the SQL WHERE clause from push-down-safe filters only.
	// result_status is pushed so the scan does not waste time on
	// invocations the caller did not ask for; classification/actor
	// filters are computed post-row since they depend on map lookups,
	// not raw column equality.
	where, args, err := appendResultStatusFilter(nil, nil, p.ResultStatus)
	if err != nil {
		return RecentActivityResult{}, err
	}
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
	if p.MPMSessionID != "" {
		where = append(where, "mpm_session_id = ?")
		args = append(args, p.MPMSessionID)
	}
	if p.FrameworkSessionID != "" {
		where = append(where, "framework_session_id = ?")
		args = append(args, p.FrameworkSessionID)
	}
	if p.ArtifactType != "" {
		where = append(where, "tool_name = ?")
		args = append(args, p.ArtifactType)
	}

	// The WHERE keyword is emitted only when there is at least one
	// predicate. This is not defensive tidiness: ResultStatus=All with
	// no other filters legitimately yields an empty predicate list, and
	// splicing an unconditional `WHERE ` in front of `ORDER BY` is a
	// syntax error — the caller would see a broken query rather than
	// the unfiltered stream they asked for.
	fromClause := `FROM tool_invocations`
	if len(where) > 0 {
		fromClause += ` WHERE ` + joinWhere(where)
	}

	baseQuery := `
		SELECT id, session_id, tool_name, action, invocation_id,
		       actor_kind, framework_name, payload_hash, result_status,
		       started_at, completed_at, duration_ms, error_message,
		       mpm_session_id, framework_session_id
		` + fromClause + `
		ORDER BY completed_at DESC, id DESC`

	out := make([]RecentActivityEvent, 0, limit)
	scanned := 0
	offset := 0
	truncated := false
	historyExhausted := false

	// Bounded pagination: newest-first walk. Terminates when the
	// matching-event quota is filled, the table runs out, or the
	// scan budget is hit (latter two are reported honestly via
	// the result flags).
	for {
		pageArgs := append(append([]interface{}{}, args...), recentActivityPageSize, offset)
		rows, err := dm.db.Query(baseQuery+` LIMIT ? OFFSET ?`, pageArgs...)
		if err != nil {
			return RecentActivityResult{}, fmt.Errorf("RecentActivity query: %w", err)
		}

		pageEvents, pageCount, scanErr := dm.filterActivityPage(rows, p)
		rows.Close()
		if scanErr != nil {
			return RecentActivityResult{}, scanErr
		}
		scanned += pageCount
		out = append(out, pageEvents...)

		if len(out) >= limit {
			// Trim to exact limit; may have collected more from the
			// last page than was needed.
			if len(out) > limit {
				out = out[:limit]
			}
			break
		}

		if pageCount < recentActivityPageSize {
			// Page was short — table exhausted for this WHERE.
			historyExhausted = true
			break
		}

		if scanned >= scanLimit {
			truncated = true
			break
		}

		offset += pageCount
	}

	return RecentActivityResult{
		Events:           out,
		HistoryExhausted: historyExhausted,
		Truncated:        truncated,
		ScannedRows:      scanned,
		ScanLimit:        scanLimit,
	}, nil
}

// filterActivityPage scans a single page of tool_invocations rows,
// applies the per-row classification + actor filters, and returns
// the matching events plus the raw row count actually read.
//
// The returned slice is bounded by the requested limit so a single
// oversized page does not blow past the cap before the caller can
// terminate the outer pagination loop.
func (dm *DatabaseManager) filterActivityPage(rows *sql.Rows, p RecentActivityQueryParams) ([]RecentActivityEvent, int, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = RecentActivityDefaultLimit
	}
	if limit > RecentActivityHardMaxLimit {
		limit = RecentActivityHardMaxLimit
	}

	out := make([]RecentActivityEvent, 0, recentActivityPageSize)
	read := 0
	for rows.Next() {
		read++
		var (
			id, sessID, tool, act, invID, actKind, fwk, pHash, status, startedAt, completedAt string
			dur                                                                                sql.NullInt64
			errMsg                                                                             sql.NullString
			mpmSID, fwSID                                                                      sql.NullString
		)
		if err := rows.Scan(&id, &sessID, &tool, &act, &invID,
			&actKind, &fwk, &pHash, &status,
			&startedAt, &completedAt, &dur, &errMsg,
			&mpmSID, &fwSID); err != nil {
			return nil, read, fmt.Errorf("RecentActivity scan: %w", err)
		}

		// The SQL already pushed result_status down, so in the
		// success/all-default path this branch is unreachable. It exists
		// because a future caller could reuse this helper without that
		// filter.
		//
		// It is conditioned on the selector rather than applied
		// unconditionally: an unconditional `status == "error"`
		// continue would silently discard exactly the rows a caller
		// asked for by passing ResultStatus=error or =all, making the
		// new parameter look accepted but do nothing. That is the same
		// invisible-widening failure the parameter is meant to end.
		//
		// Unsupported selectors never reach here: RecentActivityWithMeta
		// validates before the first query and returns the error, so the
		// default arm below is only reachable for "" and "success".
		wantsErrors := p.ResultStatus == RecentActivityStatusError ||
			p.ResultStatus == RecentActivityStatusAll
		if !wantsErrors && status == "error" {
			continue
		}

		// Classification filter. recent_activity is the factual
		// semantic-activity surface: it answers "what semantic
		// durable activity happened?" and excludes everything
		// else by design — read_only queries, lifecycle delivery
		// side effects (handoff/read, wakes/check, context/read_wake_context),
		// diagnostic probes (mpm_system/health_check), and substrate
		// maintenance (mpm_system/gc_run, mpm_system/compact).
		//
		// The legacy IncludeSystem switch is now a strict no-op:
		// the substrate cannot truthfully provide "all system
		// activity" coverage (cascade-materializer, scheduler
		// retention, GC, migration writes do not all flow through
		// tool_invocations). Agents wanting structured system audit
		// should use mpm_system query_audit_log / list_clusters
		// instead. The handler logs a deprecation row when a
		// legacy caller passes IncludeSystem=true.
		cls := ClassifyAction(tool, act)
		if cls != ActionClassMutating {
			continue
		}

		// Effective actor classification.
		effectiveKind := EffectiveActorKind(actKind, fwk)

		// Actor filter. Default (empty ActorKind) admits agent +
		// human + unknown; system and drill are excluded because
		// they are substrate bookkeeping, not agency-authored
		// activity. Explicit ActorKind "all" admits every class.
		if p.ActorKind != "" && p.ActorKind != "all" {
			if p.ActorKind != effectiveKind {
				continue
			}
		} else if p.ActorKind == "" {
			switch effectiveKind {
			case ActorKindAgent, ActorKindHuman, ActorKindUnknown:
				// keep
			default:
				continue
			}
		}

		// Build the event.
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
		if mpmSID.Valid && mpmSID.String != "" {
			ev.MPMSessionID = mpmSID.String
		}
		if fwSID.Valid && fwSID.String != "" {
			ev.FrameworkSessionID = fwSID.String
		}
		if errMsg.Valid && errMsg.String != "" {
			ev.Status = "error"
		}

		// Deterministic enrichment: pull artifact id from the
		// per-tool tables where correlation is by invocation_id.
		if artifactID, hint := dm.enrichActivityEvent(ev); artifactID != "" {
			ev.ArtifactID = artifactID
			ev.ArtifactType = tool
			ev.EnrichmentHint = hint
		}

		out = append(out, ev)
		if len(out) >= limit {
			// Caller may stop paginating; cap the page contribution.
			break
		}
	}
	return out, read, nil
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