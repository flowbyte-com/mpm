package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestWork_NotFoundHint is the F5 regression test (2026-08-27).
//
// Bug: `mpm work item complete "<some title>"` returned
//   work not found: <some title>
// which left a user confused about whether the work item genuinely didn't
// exist or whether they were addressing it wrong. The work-item contract is
// ID-only — titles are intentionally not unique and not a valid lookup
// key — but the error did not communicate that.
//
// Contract after fix: every `mpm_work` action that resolves a work_id and
// gets "not found" must surface a hint pointing at `mpm work item list` as
// the discovery surface. The internal GetWork error stays unchanged
// (other callers don't need the hint); only the tool-handler boundary
// wraps the user-facing error.
func TestWork_NotFoundHint(t *testing.T) {
	dm := newTestSharedDM(t)

	probe := createTestWork(t, dm, "f5 hint probe")

	actions := []struct {
		name   string
		action string
		params map[string]interface{}
	}{
		// show / complete / cancel / update / reopen all gate on GetWork
		// and surface "work not found: <id>" when the id is unknown.
		// history and note are event-sourced append paths and do not
		// require a pre-existing work row; their error contract is
		// "always succeed silently" and is out of scope for F5.
		{"show", "show", map[string]interface{}{"work_id": "definitely-does-not-exist"}},
		{"complete", "complete", map[string]interface{}{"work_id": "definitely-does-not-exist", "note": "f5"}},
		{"cancel", "cancel", map[string]interface{}{"work_id": "definitely-does-not-exist"}},
		{"update", "update", map[string]interface{}{"work_id": "definitely-does-not-exist", "status": "done"}},
		{"reopen", "reopen", map[string]interface{}{"work_id": "definitely-does-not-exist"}},
	}

	for _, tc := range actions {
		t.Run(tc.name, func(t *testing.T) {
			_, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": tc.action, "params": tc.params,
			})
			if err == nil {
				t.Fatalf("%s with bogus id: expected error, got nil", tc.action)
			}
			msg := err.Error()
			if !strings.Contains(msg, "work not found") {
				t.Errorf("%s: error %q missing 'work not found'", tc.action, msg)
			}
			if !strings.Contains(msg, "list") {
				t.Errorf("%s: error %q missing the 'list' discovery hint", tc.action, msg)
			}
		})
	}

	// Sanity: a real work_id still works after the hint-wrapping change.
	if _, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "show", "params": map[string]interface{}{"work_id": probe},
	}); err != nil {
		t.Errorf("show with real work_id failed: %v", err)
	}
}

// createTestWork seeds a work item and returns its id. Helper for the F5
// regression suite; mirrors the inline create pattern in adjacent work
// tests without depending on the full payload envelope.
func createTestWork(t *testing.T, dm mpminternal.CoreDB, title string) string {
	t.Helper()
	res, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{"title": title},
	})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	m, _ := res.(map[string]interface{})
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("create %q returned empty id: %v", title, res)
	}
	return id
}
