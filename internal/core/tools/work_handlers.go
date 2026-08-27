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
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_work. Valid actions include create, list, show, update, complete, cancel, history, note, reopen", action)
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
	return workToMapWork(w), nil
}

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
	var works []*mpminternal.Work
	var err error
	if status == "all" {
		works, err = dm.ListAllWorks()
	} else {
		works, err = dm.ListWorksByStatus(status)
	}
	if err != nil {
		return nil, err
	}
	// Optional limit param.
	if limVal, ok := p["limit"]; ok {
		var limit int
		switch v := limVal.(type) {
		case float64:
			limit = int(v)
		case int:
			limit = v
		case int64:
			limit = int(v)
		}
		if limit > 0 && limit < len(works) {
			works = works[:limit]
		}
	}
	result := make([]map[string]interface{}, len(works))
	for i, w := range works {
		result[i] = workToMapWork(w)
	}
	// F14: enveloped response. The bare-array shape made error vs success
	// indistinguishable at the wire and diverged from every sibling list
	// (mpm_handoff list, lessons search, memory query — all envelopes).
	return map[string]interface{}{
		"success": true,
		"works":   result,
		"count":   len(result),
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
	return workToMapWork(w), nil
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
	return workToMapWork(w), nil
}

func handleCompleteWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
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
	return workToMapWork(w), nil
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
	return workToMapWork(w), nil
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
	return workToMapWork(w), nil
}

func workToMapWork(w *mpminternal.Work) map[string]interface{} {
	m := map[string]interface{}{
		"id":           w.ID,
		"title":        w.Title,
		"status":       w.Status,
		"verification": w.Verification,
		"created_at":   w.CreatedAt,
		"updated_at":   w.UpdatedAt,
	}
	if w.Content != "" {
		m["content"] = w.Content
	}
	if w.CompletedAt != nil {
		m["completed_at"] = *w.CompletedAt
	}
	if w.SessionID != "" {
		m["session_id"] = w.SessionID
	}
	return m
}

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
