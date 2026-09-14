// internal/core/work_rows.go — Shared work-list row helpers.
//
// 2026-09-14 release-pass: ListWorkRows + WorkRowToMap live in the
// main core package (not internal/core/tools/) so the CLI handler
// (cmd/mpm/handlers_work.go) and the substrate's mpm_work
// JSON handler (internal/core/tools/work_handlers.go) both consume
// the same structured row data. The CLI does NOT invoke another
// CLI surface and parse its JSON; both paths share this helper.

package internal

// ListWorkRows returns the structured work-list rows for the given
// status filter and limit. The rows are a []map[string]interface{}
// with the canonical work-row shape (id / title / content /
// status / verification / created_at / updated_at / completed_at /
// session_id).
//
// The same helper is called from:
//   - handleListWorks in internal/core/tools/work_handlers.go (the
//     JSON wire path that handleCall mpm_work uses)
//   - handleWorkItemList in cmd/mpm/handlers_work.go (the human-
//     mode CLI path)
//
// Both paths emit identical rows; the CLI handler renders them via
// the canonical visual grammar (render.Label + render.Hint) while
// the JSON path serialises them via json.Marshal.
//
// Status values: "all" / "open" / "done" / "cancelled". The legacy
// singular "ListWorks" call (status default open) is preserved by
// mapping an empty status to "open".
//
// Limit values <= 0 mean "no cap".
func ListWorkRows(dm CoreDB, status string, limit int) ([]map[string]interface{}, error) {
	if status == "" {
		status = "open"
	}
	var works []*Work
	var err error
	if status == "all" {
		works, err = dm.ListAllWorks()
	} else {
		works, err = dm.ListWorksByStatus(status)
	}
	if err != nil {
		return nil, err
	}
	if limit > 0 && limit < len(works) {
		works = works[:limit]
	}
	rows := make([]map[string]interface{}, len(works))
	for i, w := range works {
		rows[i] = WorkRowToMap(w)
	}
	return rows, nil
}

// WorkRowToMap builds the canonical work-row shape consumed by
// both the JSON envelope (mpm_work action=list) and the human-mode
// CLI renderer. The map keys are stable wire keys; do not rename
// without updating downstream consumers.
func WorkRowToMap(w *Work) map[string]interface{} {
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
