package tools

import (
	"fmt"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// workNotFoundHint wraps the canonical "work not found: <id>" error with a
// hint that explains the work-item lookup contract: titles are not a valid
// lookup key (they are intentionally not unique), the discovery surface is
// `mpm work item list`. Without this hint, a user typing
// `mpm work item complete "ship parser fix"` (passing the title) gets back
// `work not found: ship parser fix` and cannot tell whether the work item
// genuinely doesn't exist or whether they addressed it incorrectly.
//
// F5 (2026-08-27): work completion contract is ID-only. The hint makes
// that contract visible at the user-facing error boundary. The internal
// GetWork error is unchanged — non-UI callers (audit, scheduler) get the
// raw form.
func workNotFoundHint(err error, id string) error {
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "work not found") {
		return err
	}
	return fmt.Errorf("%w\nhint: mpm_work addresses work items by id, not by title; titles are not unique. Run `mpm work item list` to find the id for a given title", err)
}

// handleMpmWork dispatches work actions: create, list, show, update, complete, cancel,
// and the new v2 event-sourced actions: history, note, reopen.
func handleMpmWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_work", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)

	switch action {
	case "create":
		return handleCreateWork(dm, ac, params)
	case "list":
		return handleListWorks(dm, params)
	case "show":
		return handleShowWork(dm, params)
	case "update":
		return handleUpdateWork(dm, ac, params)
	case "complete":
		return handleCompleteWork(dm, ac, params)
	case "cancel":
		return handleCancelWork(dm, ac, params)
	case "history":
		return handleHistoryWork(dm, params)
	case "note":
		return handleNoteWork(dm, ac, params)
	case "reopen":
		return handleReopenWork(dm, ac, params)
	case "archive":
		return handleArchiveWork(dm, ac, params)
	case "unarchive":
		return handleUnarchiveWork(dm, ac, params)
	case "resolve_contradiction":
		// F6-1 (alpha-final): agent-facing first-class contradiction
		// resolution. The recovery path for T20-1's invariant — a
		// previously-verified work that was downgraded by an
		// unsubstantiated dispute can be restored without deleting
		// the dispute rows (audit trail preserved).
		return handleResolveContradictionWork(dm, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_work. Valid actions include create, list, show, update, complete, cancel, history, note, reopen, archive, unarchive, resolve_contradiction", action)
	}
}

func handleCreateWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	title, _ := p["title"].(string)
	if title == "" {
		return nil, fmt.Errorf("title is required for create")
	}
	content, _ := p["content"].(string)
	sessionID, _ := p["session_id"].(string)
	w, err := dm.CreateWorkWithContext(title, content, sessionID, ac)
	if err != nil {
		return nil, err
	}
	return mpminternal.WorkRowToMap(w), nil
}

// validWorkStatuses enumerates the accepted values for the `status`
// filter on mpm_work action=list. W-010: invalid statuses used to
// silently return zero rows, indistinguishable from "no items
// exist". The enum is the single source of truth — keep it sorted.
var validWorkStatuses = []string{"all", "cancelled", "done", "open"}

// validWorkVisibilities mirrors mpminternal.ValidWorkVisibilities on
// the wire. Same W-010 rationale: a typo in `visibility` must produce a
// clear error, never a zero-row result that reads as "no work exists".
var validWorkVisibilities = mpminternal.ValidWorkVisibilities

func handleListWorks(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	// Support status filtering: "open" (default), "done", "cancelled", "all".
	// The legacy ListWorks only returned open, which prevented reliable
	// listing of completed work (validation suite P2).
	status, _ := p["status"].(string)
	if status == "" {
		// Maintain backward compatibility: default to open when no status supplied.
		// Callers that want all should pass status="all".
		status = "open"
	}
	// W-010: validate the status enum explicitly. Previously a typo
	// like "openx" silently fell through to ListWorksByStatus which
	// returned zero rows — indistinguishable from "no items exist".
	if !isValidWorkStatus(status) {
		return nil, fmt.Errorf(
			"field `status` must be one of [%s], got %q",
			strings.Join(validWorkStatuses, ", "), status,
		)
	}
	// visibility is the SECOND, independent axis (2026-09-30). It is not
	// derived from status: `done` + `archived` is a coherent query, and
	// `all` + `all` is the complete inventory.
	visibility, _ := p["visibility"].(string)
	if visibility == "" {
		visibility = string(mpminternal.WorkVisibilityActive)
	}
	if !isValidWorkVisibility(visibility) {
		return nil, fmt.Errorf(
			"field `visibility` must be one of [%s], got %q",
			strings.Join(validWorkVisibilities, ", "), visibility,
		)
	}
	// Optional limit param.
	limit := 0
	if limVal, ok := p["limit"]; ok {
		switch v := limVal.(type) {
		case float64:
			limit = int(v)
		case int:
			limit = v
		case int64:
			limit = int(v)
		}
	}
	// 2026-09-14 release-pass: rows come from the canonical
	// mpminternal.ListWorkRows helper (in internal/core/work_rows.go).
	// The same helper is called by the human-mode CLI handler in
	// cmd/mpm/handlers_work.go — both paths consume identical data.
	rows, err := mpminternal.ListWorkRows(dm, status, visibility, limit)
	if err != nil {
		return nil, err
	}
	// F14: enveloped response. The bare-array shape made error vs success
	// indistinguishable at the wire and diverged from every sibling list
	// (mpm_handoff list, lessons search, memory query — all envelopes).
	return map[string]interface{}{
		"success":    true,
		"works":      rows,
		"count":      len(rows),
		"status":     status,
		"visibility": visibility,
	}, nil
}

func handleShowWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for show")
	}
	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	return mpminternal.WorkRowToMap(w), nil
}

// Thin handlers: parse payload, delegate to db.go event-sourced API.
// No UPDATE works queries, no git logic, no provenance duplication.

func handleUpdateWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	statusStr, _ := p["status"].(string)
	title, _ := p["title"].(string)
	content, _ := p["content"].(string)
	w, err := dm.UpdateWorkWithContext(workID, title, content, statusStr, ac)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	return mpminternal.WorkRowToMap(w), nil
}

func handleCompleteWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// D-8.1: accept either `work_id` (canonical) or `id` (alias), matching
	// the same surface convention used by mpm_memory (memory_id) and
	// mpm_handoff (id).
	workID, _ := p["work_id"].(string)
	if workID == "" {
		if v, ok := p["id"].(string); ok && v != "" {
			workID = v
		}
	}
	note, _ := p["note"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for complete")
	}
	_, err := dm.CompleteWorkWithContext(workID, note, ac)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	// P3 fix: do not auto-record git evidence on complete — it inflated
	// verification to 'partial' for false completions (claim without outcome).
	// Git evidence is still recorded via explicit observation paths and via
	// external git commit detection; auto-inflation on every lifecycle
	// transition is removed so verification reflects only outcome evidence.
	if _, err := dm.DeriveWorkVerification(workID); err != nil {
		// Non-fatal: verification stays at its last known value.
	}
	// Re-fetch so the response reflects the verification derived by THIS
	// command rather than the pre-derivation snapshot (D3 fix,
	// 2026-08-25). Lifecycle mutation and verification derivation remain
	// orthogonal; only the returned payload changes.
	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	return mpminternal.WorkRowToMap(w), nil
}

func handleCancelWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	note, _ := p["note"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for cancel")
	}
	if _, err := dm.CancelWorkWithContext(workID, note, ac); err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	// P3 fix: same as complete — do not auto-inflate verification via git
	// on cancel. Cancellation already locks verification below verified via
	// DeriveWorkVerification's lifecycle gate; audit evidence remains in
	// history but does not promote verification.
	if _, err := dm.DeriveWorkVerification(workID); err != nil {
		// Non-fatal.
	}
	// Re-fetch post-derivation (see handleCompleteWork).
	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	return mpminternal.WorkRowToMap(w), nil
}

// handleHistoryWork returns the full event history for a work item,
// ordered by event_index ASC, enveloped (F14).
//
// F16: each event is enriched with framework/model metadata resolved from
// the authoritative provenance records. The work's artifact_provenance row
// is joined via invocation_id — one batched query for the whole history,
// no per-event lookups. Missing optional metadata stays absent rather than
// fabricated.
func handleHistoryWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for history")
	}

	// D-006: verify the work exists before listing its events. Without
	// this check, a missing work_id returns `count=0, events=[]` with
	// `success=true` — indistinguishable from a freshly-created work
	// that genuinely has no events yet. Operators typing a title (or
	// any typo'd id) get a silent lie about the work's existence.
	if _, err := dm.GetWork(workID); err != nil {
		return nil, workNotFoundHint(err, workID)
	}

	events, err := dm.GetWorkEvents(workID)
	if err != nil {
		return nil, fmt.Errorf("get work events: %w", err)
	}

	// Batch-resolve framework/model per distinct invocation_id from
	// tool_invocations (the invocation-time record). artifact_provenance
	// is the artifact-level fallback for the create event.
	provMeta := dm.ResolveFrameworkModelForInvocations(workID, collectInvocationIDs(events))

	result := make([]map[string]interface{}, len(events))
	for i, e := range events {
		m := workEventToMap(e)
		if fm, ok := provMeta[e.InvocationID]; ok {
			if fm.FrameworkName != "" {
				m["framework_name"] = fm.FrameworkName
			}
			if fm.Model != "" {
				m["model"] = fm.Model
			}
		}
		result[i] = m
	}
	return map[string]interface{}{
		"success": true,
		"work_id": workID,
		"events":  result,
		"count":   len(result),
	}, nil
}

func collectInvocationIDs(events []*mpminternal.WorkEvent) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e.InvocationID != "" && !seen[e.InvocationID] {
			seen[e.InvocationID] = true
			out = append(out, e.InvocationID)
		}
	}
	return out
}

func handleNoteWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	note, _ := p["note"].(string)
	ev, err := dm.AddWorkNoteWithContext(workID, note, ac)
	if err != nil {
		return nil, err
	}
	return workEventToMap(ev), nil
}

func handleReopenWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	w, err := dm.ReopenWorkWithContext(workID, ac)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	return mpminternal.WorkRowToMap(w), nil
}

// handleResolveContradictionWork is the F6-1 / T20-1 agent-facing recovery
// path: withdraw unsubstantiated dispute evidence from a work item and
// re-derive verification.
//
// Inputs:
//   - work_id  : required (which work item to recover)
//   - reason   : required (audit-trail reason — recorded into the dispute
//                row's notes as resolved_reason)
//
// Returns the re-derived work row (verification reflects the cleared
// evidence picture).
//
// Idempotency: works on whatever dispute rows are currently live. Already-
// resolved rows (expires_at <= now) are not touched.
func handleResolveContradictionWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for resolve_contradiction")
	}
	reason, _ := p["reason"].(string)
	if reason == "" {
		return nil, fmt.Errorf("reason is required for resolve_contradiction (audit trail)")
	}
	if err := dm.ResolveWorkContradiction(workID, reason); err != nil {
		return nil, err
	}
	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, err
	}
	return mpminternal.WorkRowToMap(w), nil
}

// workToMapWork was removed in the 2026-09-14 release-pass. The
// canonical implementation lives in internal/core/work_rows.go
// (mpminternal.WorkRowToMap) so both the substrate's mpm_work JSON
// path and the human-mode CLI handler consume identical structured
// rows.

func workEventToMap(e *mpminternal.WorkEvent) map[string]interface{} {
	m := map[string]interface{}{
		"id":          e.ID,
		"work_id":     e.WorkID,
		"event_index": e.EventIndex,
		"event_type":  e.EventType,
		"created_at":  e.CreatedAt,
	}
	if e.InvocationID != "" {
		m["invocation_id"] = e.InvocationID
	}
	if e.ParentInvocationID != "" {
		m["parent_invocation_id"] = e.ParentInvocationID
	}
	if e.Note != "" {
		m["note"] = e.Note
	}
	if e.Title != "" {
		m["title"] = e.Title
	}
	if e.Content != "" {
		m["content"] = e.Content
	}
	if len(e.DirectiveIDs) > 0 {
		m["directive_ids"] = e.DirectiveIDs
	}
	return m
}

// isValidWorkStatus reports whether s is one of the accepted work-status
// filter values. W-010: a typo like "openx" used to silently fall
// through to ListWorksByStatus, which returned zero rows indistinguishable
// from "no items exist". Now an invalid status errors at the API boundary
// before the DB call, with the valid options listed.
func isValidWorkStatus(s string) bool {
	for _, v := range validWorkStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// isValidWorkVisibility is the visibility-axis twin of
// isValidWorkStatus. Same W-010 rationale — an unknown visibility is a
// caller error, never an empty result set.
func isValidWorkVisibility(s string) bool {
	for _, v := range validWorkVisibilities {
		if v == s {
			return true
		}
	}
	return false
}

// workIDFromParams resolves the canonical `work_id` plus the `id` alias
// used by the lifecycle commands.
func workIDFromParams(p map[string]interface{}) string {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		if v, ok := p["id"].(string); ok {
			workID = v
		}
	}
	return workID
}

// handleArchiveWork takes a terminal work item out of the operational
// view. Refuses an `open` item with no writes, and is idempotent: an
// already-archived item reports already_archived=true and appends no
// second ledger event.
func handleArchiveWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID := workIDFromParams(p)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for archive")
	}
	note, _ := p["note"].(string)
	w, already, err := dm.ArchiveWorkWithContext(workID, note, ac)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	out := mpminternal.WorkRowToMap(w)
	out["success"] = true
	if already {
		out["already_archived"] = true
	}
	return out, nil
}

// handleUnarchiveWork restores an archived item to the operational
// view. It never reopens: an item archived while cancelled returns as
// cancelled. Unarchiving a non-archived item is an error, not a no-op.
func handleUnarchiveWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID := workIDFromParams(p)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for unarchive")
	}
	note, _ := p["note"].(string)
	w, err := dm.UnarchiveWorkWithContext(workID, note, ac)
	if err != nil {
		return nil, workNotFoundHint(err, workID)
	}
	out := mpminternal.WorkRowToMap(w)
	out["success"] = true
	return out, nil
}
