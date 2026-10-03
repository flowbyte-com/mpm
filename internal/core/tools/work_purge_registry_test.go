package tools

// work_purge_registry_test.go — registry-level coverage that purge is
// NOT an agent-reachable surface.
//
// Design: docs/archive/2026-09-30-work-archive-and-purge.md §5.3.
//
// Purge is deliberately CLI-only. The schema is the whole safety
// argument: an agent that reads an enum advertising `purge` would call
// it, and the dispatcher's "unknown action" error is a usability
// failure, not a safety mechanism. These tests assert purge is absent
// from every place the schema could reintroduce it — the action enum,
// the oneOf branches, and the handler's own unknown-action list.

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestPurge_NotInActionEnum pins the primary boundary.
func TestPurge_NotInActionEnum(t *testing.T) {
	for _, action := range workActionEnum(t) {
		if action == "purge" {
			t.Fatal("mpm_work advertises a `purge` action; purge is CLI-only (design §5.3)")
		}
	}
}

// TestPurge_NotInOneOfBranches pins that no per-action branch
// reintroduces it. The enum and the oneOf are separately maintained,
// and either one alone would be enough to mislead a caller.
func TestPurge_NotInOneOfBranches(t *testing.T) {
	if oneOfActions(t)["purge"] {
		t.Fatal("mpm_work has a oneOf branch for `purge`; purge is CLI-only (design §5.3)")
	}
}

// TestPurge_NotDispatchable pins the defense in depth: even a caller
// that ignores the schema and sends the action anyway gets a refusal.
func TestPurge_NotDispatchable(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{"action": "purge"})
	if err == nil {
		t.Fatal("mpm_work dispatched a `purge` action; purge is CLI-only (design §5.3)")
	}
	// The error must name the action so the caller learns the schema
	// and the dispatcher disagree — that is the actionable part.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "purge") || !strings.Contains(msg, "unknown") {
		t.Fatalf("unknown-action error does not explain the refusal: %v", err)
	}
}
