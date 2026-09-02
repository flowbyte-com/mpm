// d041_d081_d101_surface_parity_test.go — alpha-5 regression tests
// for the MCP surface parity fixes:
//
// D-4.1 — `mpm_topics` previously routed only `create`/`search`/`link`;
//         `list` and `show` were missing entirely. The CLI had them
//         (`mpm topic list`, `mpm topic show`) but the agent-facing
//         dispatcher did not. Now both actions route through the same
//         handlers the CLI uses.
//
// D-8.1 — memory-targeted MCP actions (`reinforce`, `weaken`, `snooze`,
//         `shred` for handoffs, `complete` for work) previously read
//         only the family-specific key (`memory_id`, `handoff_id`,
//         `work_id`) and rejected the bare `id` alias. The acceptance
//         pass typed `id` and got back "memory_id is required". Now
//         the helper `memoryIDFromParams` accepts either form, and
//         the same convention is applied to handoffs and work.
//
// D-10.1 — `handleShredHandoff` previously emitted a generic
//         "id is required" error when no id was supplied, hiding the
//         recovery path. Now it surfaces the canonical W-006 hint
//         (mpm_context read_wake_context → session_current_id, or
//         MPM_SESSION_ID env var) via internal.ErrSessionIDRequired().

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestD041_MpmTopicsDispatcherRoutesListAndShow is the alpha-5
// regression for D-4.1. Pre-fix handleMpmTopics returned
// "unknown action" for `list` and `show`. The CLI surface existed
// but the MCP surface did not, so an agent couldn't enumerate or
// inspect topics without dropping to the CLI.
func TestD041_MpmTopicsDispatcherRoutesListAndShow(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	// Seed a topic via the canonical CreateTopic path so list/show have
	// something to return.
	if _, err := dm.CreateTopic("d041-test", "alpha-5 topic", "", ""); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	for _, action := range []string{"list", "show"} {
		t.Run(action, func(t *testing.T) {
			payload := map[string]interface{}{"action": action}
			switch action {
			case "show":
				payload["params"] = map[string]interface{}{"id": "d041-test"}
			default:
				payload["params"] = map[string]interface{}{}
			}

			_, err := handleMpmTopics(dm, ac, payload)
			if err != nil && strings.Contains(err.Error(), "unknown action") {
				t.Errorf("handleMpmTopics(%q) returned unknown-action error: %v", action, err)
			}
		})
	}
}

// TestD041_MpmTopicsDispatcherRejectsUnknownAction still surfaces
// the valid-actions list (including `list` and `show`) when an
// unknown action is supplied.
func TestD041_MpmTopicsDispatcherRejectsUnknownAction(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleMpmTopics(dm, ac, map[string]interface{}{
		"action": "bogus",
	})
	if err == nil {
		t.Fatal("handleMpmTopics with bogus action returned nil error")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("error must say 'unknown action', got: %v", err)
	}
	for _, want := range []string{"create", "search", "link", "list", "show"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must list %q as a valid action, got: %v", want, err)
		}
	}
}

// seedMemory is a tiny helper for the D-8.1 memory-action tests. The
// tool-surface uses `id` (canonical alias) or `memory_id` (canonical
// long form), so we need a real memory row to act on. Avoids the
// full SaveMemory parameter surface.
func seedMemory(t *testing.T, dm *mpminternal.DatabaseManager, id string) {
	t.Helper()
	_, err := dm.SaveMemory("memories", "test content", "", nil,
		map[string]interface{}{"weight": 1.0}, nil, false, 1.0)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
}

// TestD081_MemoryActionsAcceptIDAlias verifies the alpha-5 fix:
// reinforce / weaken / snooze all accept either `memory_id` (canonical)
// or `id` (alias). Pre-fix only `memory_id` was read; passing `id`
// returned "memory_id is required".
func TestD081_MemoryActionsAcceptIDAlias(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	seedMemory(t, dm, "mem-1")

	// Use the canonical `id` alias directly. handleReinforceMemory resolves
	// either `id` or `memory_id` via memoryIDFromParams.
	for _, tc := range []struct {
		name   string
		action func(params map[string]interface{}) (interface{}, error)
	}{
		{"reinforce (id alias)", func(p map[string]interface{}) (interface{}, error) {
			return handleReinforceMemory(dm, ac, p)
		}},
		{"weaken (id alias)", func(p map[string]interface{}) (interface{}, error) {
			return handleWeakenMemory(dm, ac, p)
		}},
		{"snooze (id alias)", func(p map[string]interface{}) (interface{}, error) {
			return handleSnoozeMemory(dm, ac, p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// We pass an unknown id; the test only verifies the alias
			// is recognised (the error message should NOT be
			// "memory_id is required" — that's the pre-fix failure mode).
			_, err := tc.action(map[string]interface{}{"id": "missing-id-for-alias-test"})
			if err != nil {
				msg := err.Error()
				if msg == "memory_id is required" {
					t.Errorf("%s with `id` alias: rejected alias (pre-fix failure): %v", tc.name, err)
				}
			}
		})
	}
}

// TestD081_MemoryActionsStillAcceptMemoryID pins the backward-compat
// surface: the canonical `memory_id` key must continue to work after
// the alias was added. Pass an unknown id and confirm we don't get
// the "memory_id is required" rejection (the alias-only failure mode
// would be the inverse: "memory_id is required" even when memory_id
// IS supplied).
func TestD081_MemoryActionsStillAcceptMemoryID(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	for _, tc := range []struct {
		name   string
		action func(params map[string]interface{}) (interface{}, error)
	}{
		{"reinforce (memory_id)", func(p map[string]interface{}) (interface{}, error) {
			return handleReinforceMemory(dm, ac, p)
		}},
		{"weaken (memory_id)", func(p map[string]interface{}) (interface{}, error) {
			return handleWeakenMemory(dm, ac, p)
		}},
		{"snooze (memory_id)", func(p map[string]interface{}) (interface{}, error) {
			return handleSnoozeMemory(dm, ac, p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.action(map[string]interface{}{"memory_id": "missing-id-canonical-test"})
			if err != nil {
				msg := err.Error()
				if msg == "memory_id is required" {
					t.Errorf("%s with canonical `memory_id`: rejected (memory_id was supplied): %v", tc.name, err)
				}
			}
		})
	}
}

// TestD081_MemoryActionsRejectEmptyID confirms the negative contract:
// both keys absent must error. This prevents silent no-ops where a
// caller thinks they sent an id but didn't.
func TestD081_MemoryActionsRejectEmptyID(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	for _, tc := range []struct {
		name   string
		action func(params map[string]interface{}) (interface{}, error)
	}{
		{"reinforce", func(p map[string]interface{}) (interface{}, error) {
			return handleReinforceMemory(dm, ac, p)
		}},
		{"weaken", func(p map[string]interface{}) (interface{}, error) {
			return handleWeakenMemory(dm, ac, p)
		}},
		{"snooze", func(p map[string]interface{}) (interface{}, error) {
			return handleSnoozeMemory(dm, ac, p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.action(map[string]interface{}{})
			if err == nil {
				t.Errorf("%s with no id: expected error, got nil", tc.name)
			} else if err.Error() != "memory_id is required" {
				t.Errorf("%s with no id: error = %q, want %q", tc.name, err.Error(), "memory_id is required")
			}
		})
	}
}

// TestD101_HandoffShredSurfacesSessionIDHint verifies the alpha-5
// D-10.1 fix: when neither `id` nor `handoff_id` nor `session_id`
// is supplied, handleShredHandoff now returns the canonical W-006
// hint via internal.ErrSessionIDRequired() instead of a generic
// "id is required" message. The hint names both recovery paths.
func TestD101_HandoffShredSurfacesSessionIDHint(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleShredHandoff(dm, ac, map[string]interface{}{})
	if err == nil {
		t.Fatal("handleShredHandoff with no id: expected error, got nil")
	}
	msg := err.Error()
	// The canonical W-006 hint names BOTH recovery paths: the
	// session_current_id from mpm_context read_wake_context, and
	// the MPM_SESSION_ID env var. Pre-fix only a bare "id is required"
	// was returned (no recovery path), so the canonical message must
	// start with the canonical shape.
	if !strings.HasPrefix(msg, "session_id is required") {
		t.Errorf("shred error must start with the canonical 'session_id is required' prefix; got: %q", msg)
	}
	if !strings.Contains(msg, "session_current_id") {
		t.Errorf("shred error must surface session_current_id recovery hint; got: %q", msg)
	}
	if !strings.Contains(msg, "MPM_SESSION_ID") {
		t.Errorf("shred error must surface MPM_SESSION_ID env-var recovery hint; got: %q", msg)
	}
}

// TestD081_HandoffShredAcceptsHandoffIDAlias confirms the alpha-5
// alias-convention is consistent with work/memory: handleShredHandoff
// accepts `handoff_id` (legacy) or `id` (alias). The canonical form
// for handoffs is `id` (not `handoff_id`), so the alias is reversed.
func TestD081_HandoffShredAcceptsHandoffIDAlias(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	// Seed a handoff by writing a row directly via the DM.
	// Table is `session_handoffs` (not `handoffs`) — see schema.go.
	const handoffID = "test-handoff-1"
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary, commitments, open_questions)
		VALUES (?, ?, ?, ?, ?, '[]', '[]')
	`, handoffID, "test-session", 1, "clean", "test summary"); err != nil {
		t.Fatalf("seed handoff: %v", err)
	}

	// Use the legacy `handoff_id` alias.
	_, err := handleShredHandoff(dm, ac, map[string]interface{}{"handoff_id": handoffID})
	if err != nil {
		t.Errorf("shred with handoff_id alias: %v", err)
	}

	// Verify the row is gone.
	var count int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM session_handoffs WHERE id = ?`, handoffID).Scan(&count); err != nil {
		t.Fatalf("verify shred: %v", err)
	}
	if count != 0 {
		t.Errorf("handoff not shredded: rows=%d, want 0", count)
	}
}