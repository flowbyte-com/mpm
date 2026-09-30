package tools

// work_archive_handlers_test.go — MCP-path coverage for the work
// archive lifecycle and the visibility filter.
//
// Design: docs/designs/2026-09-30-work-archive-and-purge.md §4.1, §9.
//
// The MCP path and the CLI path share ListWorkRows and the CoreDB
// methods, so these tests focus on the wire-level contract the handler
// owns: envelope shape, error class, and validation.

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// callWork is a thin wrapper over the dispatcher with the canonical
// {action, params} envelope.
func callWork(t *testing.T, dm internal.CoreDB, action string, params map[string]interface{}) map[string]interface{} {
	t.Helper()
	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": action,
		"params": params,
	})
	if err != nil {
		t.Fatalf("mpm_work %s: %v", action, err)
	}
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("mpm_work %s did not return a map, got %T", action, result)
	}
	return m
}

// listWorkIDs runs mpm_work action=list and returns the ids from the
// envelope.
func listWorkIDs(t *testing.T, dm internal.CoreDB, params map[string]interface{}) ([]string, map[string]interface{}) {
	t.Helper()
	env := callWork(t, dm, "list", params)
	if env["success"] != true {
		t.Fatalf("list envelope success = %v, want true", env["success"])
	}
	rows, _ := env["works"].([]map[string]interface{})
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		id, _ := r["id"].(string)
		ids = append(ids, id)
	}
	return ids, env
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestHandleMpmWork_ArchiveRefusesOpen pins the terminal-only guard at
// the wire boundary. The error must be a caller-facing refusal, not a
// substrate fault.
func TestHandleMpmWork_ArchiveRefusesOpen(t *testing.T) {
	dm := newTestSharedDM(t)
	w, err := typeAssertDBM(dm).AddWork("live commitment", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	_, err = handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "archive",
		"params": map[string]interface{}{"work_id": w.ID},
	})
	if err == nil {
		t.Fatal("archive(open) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error %q does not explain the terminal-only rule", err)
	}
	class, _ := internal.ClassifyError(err)
	if class != internal.OutcomeClassConflict {
		t.Errorf("ClassifyError class = %v, want OutcomeClassConflict", class)
	}
	after, err := typeAssertDBM(dm).GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if after.IsArchived() {
		t.Error("refused archive set archived_at")
	}
}

// TestHandleMpmWork_ArchiveRoundTrip drives the supported flow through
// the MCP wire and asserts the visibility filter actually changes the
// result set.
func TestHandleMpmWork_ArchiveRoundTrip(t *testing.T) {
	dm := newTestSharedDM(t)
	w, err := typeAssertDBM(dm).AddWork("finished", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if _, err := typeAssertDBM(dm).CompleteWorkWithContext(w.ID, "done", internal.ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Visible before archive.
	ids, _ := listWorkIDs(t, dm, map[string]interface{}{"status": "all"})
	if !hasID(ids, w.ID) {
		t.Fatalf("work missing from the default listing before archive: %v", ids)
	}

	archived := callWork(t, dm, "archive", map[string]interface{}{"work_id": w.ID})
	if archived["success"] != true {
		t.Errorf("archive envelope success = %v, want true", archived["success"])
	}
	if _, ok := archived["archived_at"]; !ok {
		t.Error("archive response omits archived_at")
	}
	if _, ok := archived["already_archived"]; ok {
		t.Error("first archive reported already_archived; that key is reserved for the idempotent case")
	}

	// Hidden from the default view, visible under the archived filter.
	ids, _ = listWorkIDs(t, dm, map[string]interface{}{"status": "all"})
	if hasID(ids, w.ID) {
		t.Errorf("archived work still in the default listing: %v", ids)
	}
	ids, _ = listWorkIDs(t, dm, map[string]interface{}{"status": "all", "visibility": "archived"})
	if !hasID(ids, w.ID) {
		t.Errorf("archived work missing from visibility=archived: %v", ids)
	}
	ids, _ = listWorkIDs(t, dm, map[string]interface{}{"status": "all", "visibility": "all"})
	if !hasID(ids, w.ID) {
		t.Errorf("archived work missing from visibility=all: %v", ids)
	}

	// Unarchive restores visibility and never reopens.
	restored := callWork(t, dm, "unarchive", map[string]interface{}{"work_id": w.ID})
	if restored["success"] != true {
		t.Errorf("unarchive envelope success = %v, want true", restored["success"])
	}
	if workStatus(restored, "status") != "done" {
		t.Errorf("status after unarchive = %q, want done (unarchive must never reopen)", workStatus(restored, "status"))
	}
	if _, ok := restored["archived_at"]; ok {
		t.Error("unarchive response still carries archived_at")
	}
	ids, _ = listWorkIDs(t, dm, map[string]interface{}{"status": "all"})
	if !hasID(ids, w.ID) {
		t.Errorf("unarchived work missing from the default listing: %v", ids)
	}
}

// TestHandleMpmWork_ArchiveAcceptsIDAlias pins the `id` alias the
// schema advertises. A schema that documents an alias the handler
// ignores is the §4.4 drift defect in miniature.
func TestHandleMpmWork_ArchiveAcceptsIDAlias(t *testing.T) {
	dm := newTestSharedDM(t)
	w, err := typeAssertDBM(dm).AddWork("finished", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if _, err := typeAssertDBM(dm).CompleteWorkWithContext(w.ID, "done", internal.ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	callWork(t, dm, "archive", map[string]interface{}{"id": w.ID})
	after, err := typeAssertDBM(dm).GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if !after.IsArchived() {
		t.Error("the `id` alias did not reach the archive path")
	}
	callWork(t, dm, "unarchive", map[string]interface{}{"id": w.ID})
	after, err = typeAssertDBM(dm).GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if after.IsArchived() {
		t.Error("the `id` alias did not reach the unarchive path")
	}
}

// TestHandleMpmWork_ArchiveIdempotent pins §1.5 on the wire: the second
// archive succeeds and reports already_archived rather than erroring.
func TestHandleMpmWork_ArchiveIdempotent(t *testing.T) {
	dm := newTestSharedDM(t)
	w, err := typeAssertDBM(dm).AddWork("finished", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if _, err := typeAssertDBM(dm).CompleteWorkWithContext(w.ID, "done", internal.ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	callWork(t, dm, "archive", map[string]interface{}{"work_id": w.ID})
	second := callWork(t, dm, "archive", map[string]interface{}{"work_id": w.ID})
	if second["already_archived"] != true {
		t.Errorf("second archive already_archived = %v, want true", second["already_archived"])
	}
}

// TestHandleMpmWork_ArchiveMissingWorkID pins the missing-param error
// path for both new actions.
func TestHandleMpmWork_ArchiveMissingWorkID(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, action := range []string{"archive", "unarchive"} {
		t.Run(action, func(t *testing.T) {
			_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
				"action": action,
				"params": map[string]interface{}{},
			})
			if err == nil {
				t.Fatalf("mpm_work %s with no work_id succeeded, want an error", action)
			}
			if !strings.Contains(err.Error(), "work_id is required") {
				t.Errorf("error %q does not name the missing parameter", err)
			}
		})
	}
}

// TestHandleMpmWork_UnarchiveNonArchived pins that unarchiving an
// active item errors rather than silently succeeding.
func TestHandleMpmWork_UnarchiveNonArchived(t *testing.T) {
	dm := newTestSharedDM(t)
	w, err := typeAssertDBM(dm).AddWork("never archived", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	_, err = handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "unarchive",
		"params": map[string]interface{}{"work_id": w.ID},
	})
	if err == nil {
		t.Fatal("unarchive(non-archived) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "not archived") {
		t.Errorf("error %q does not explain the precondition", err)
	}
}

// TestHandleMpmWork_ShowRetainsArchived pins that archive is a
// visibility operation, not a deletion: `show` still resolves the item
// by id.
func TestHandleMpmWork_ShowRetainsArchived(t *testing.T) {
	dm := newTestSharedDM(t)
	w, err := typeAssertDBM(dm).AddWork("finished", "with content", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if _, err := typeAssertDBM(dm).CompleteWorkWithContext(w.ID, "done", internal.ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	callWork(t, dm, "archive", map[string]interface{}{"work_id": w.ID})

	shown := callWork(t, dm, "show", map[string]interface{}{"work_id": w.ID})
	if shown["id"] != w.ID {
		t.Errorf("show id = %v, want %s", shown["id"], w.ID)
	}
	if shown["content"] != "with content" {
		t.Errorf("show content = %v, want the original text", shown["content"])
	}
	if _, ok := shown["archived_at"]; !ok {
		t.Error("show on archived work omits archived_at")
	}
	// history is also an explicit by-id access path and must still
	// resolve — the ledger retains the archive event rather than being
	// filtered by it.
	history := callWork(t, dm, "history", map[string]interface{}{"work_id": w.ID})
	events, _ := history["events"].([]map[string]interface{})
	var sawArchive bool
	for _, e := range events {
		// event_type carries the typed WorkEventType, so compare
		// through the declared constant rather than a raw string.
		if e["event_type"] == interface{}(internal.WorkEventTypeArchived) {
			sawArchive = true
		}
	}
	if !sawArchive {
		t.Errorf("history for archived work has no 'archived' event: %v", events)
	}
}

// TestHandleMpmWork_ListVisibilityValidation pins the W-010 contract:
// both axes produce a clear error on a typo, never a silent zero-row
// result. `count: 0` with `success: true` would be indistinguishable
// from "no work exists".
func TestHandleMpmWork_ListVisibilityValidation(t *testing.T) {
	dm := newTestSharedDM(t)
	typeAssertDBM(dm).AddWork("something", "", "")

	for _, params := range []map[string]interface{}{
		{"visibility": "archivd"},
		{"visibility": "yes"},
		{"status": "donee"},
	} {
		_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "list",
			"params": params,
		})
		if err == nil {
			t.Errorf("list with params %v succeeded, want a validation error", params)
		}
	}
}

// TestHandleMpmWork_ListEnvelopeEchoesBothAxes pins that the envelope
// reports the filters it applied. Without this a caller cannot tell an
// empty result set from a filtered one.
func TestHandleMpmWork_ListEnvelopeEchoesBothAxes(t *testing.T) {
	dm := newTestSharedDM(t)
	cases := []struct {
		params     map[string]interface{}
		wantStatus string
		wantVis    string
	}{
		{map[string]interface{}{}, "open", "active"},
		{map[string]interface{}{"status": "all"}, "all", "active"},
		{map[string]interface{}{"visibility": "archived"}, "open", "archived"},
		{map[string]interface{}{"status": "done", "visibility": "all"}, "done", "all"},
	}
	for _, tc := range cases {
		_, env := listWorkIDs(t, dm, tc.params)
		if env["status"] != tc.wantStatus {
			t.Errorf("params %v: envelope status = %v, want %q", tc.params, env["status"], tc.wantStatus)
		}
		if env["visibility"] != tc.wantVis {
			t.Errorf("params %v: envelope visibility = %v, want %q", tc.params, env["visibility"], tc.wantVis)
		}
	}
}

// TestHandleMpmWork_UnknownActionListsArchive pins that the
// dispatcher's "Valid actions include …" string names the new actions.
// This is one of the five surfaces §4.4 requires to change together; a
// stale list is a message that lies to every agent that reads it.
func TestHandleMpmWork_UnknownActionListsArchive(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "definitely-not-an-action",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("unknown action succeeded, want an error")
	}
	for _, want := range []string{"archive", "unarchive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-action error does not list %q: %q", want, err)
		}
	}
}
