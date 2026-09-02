// session_hint_regression_test.go — regression tests for alpha-4 W-006
// (handoff/session discovery hint).
//
// Pins the canonical "session_id is required" error message. Every
// call site that needs session_id must route through
// internal.ErrSessionIDRequired() so the agent gets a single,
// nameable error to grep for, with both recovery paths named.

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestSessionIDRequired_HintNamesBothRecoveryPaths is the W-006
// pin: the canonical error message must name BOTH recovery paths
// (mpm_context read_wake_context → session_current_id, and the
// MPM_SESSION_ID env var). If either path is missing, an agent that
// only knows one will silently fail to recover.
func TestSessionIDRequired_HintNamesBothRecoveryPaths(t *testing.T) {
	err := mpminternal.ErrSessionIDRequired()
	if err == nil {
		t.Fatal("ErrSessionIDRequired returned nil")
	}

	msg := err.Error()

	wantSubstrings := []string{
		"session_id is required",
		"mpm_context read_wake_context",
		"session_current_id",
		"MPM_SESSION_ID",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("canonical error missing %q; got %q", want, msg)
		}
	}
}

// TestHandleHandoffWrite_EmptySessionIDSucceeds pins the alpha-5 contract:
// handoff write with an empty session_id is allowed — the durable identity of
// a handoff is the MPM-generated id + created_at; session_id is opaque
// optional correlation metadata. Production evolved this contract in
// 4b8f8d4 (fix(handoff): make session_id optional) to let callers without a
// stable framework session id (e.g. Claude Code) still persist handoffs.
//
// Summary is the only required input; a successful write echoes a handoff
// with empty session_id and a generated id. If a future change reverts to
// mandating session_id, callers that boot without `MPM_SESSION_ID` would
// silently fail to preserve continuity — this test pins the boundary.
func TestHandleHandoffWrite_EmptySessionIDSucceeds(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmHandoff(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary": "test handoff with no session_id",
		},
	})
	if err != nil {
		t.Fatalf("expected success on empty session_id; got error %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map response; got %T", res)
	}
	if m["handoff_id"] == nil || m["handoff_id"] == "" {
		t.Errorf("expected generated handoff_id; got %v", m["handoff_id"])
	}
	if sid, _ := m["session_id"].(string); sid != "" {
		t.Errorf("expected echoed session_id to be empty; got %q", sid)
	}
}

// TestHandleHandoffWrite_MissingSummaryFails pins that summary remains the
// sole required input for handoff write — even after session_id became
// optional. The wire error must name the missing field so an agent can
// self-correct.
func TestHandleHandoffWrite_MissingSummaryFails(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleMpmHandoff(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error on missing summary")
	}
	if !strings.Contains(err.Error(), "summary") {
		t.Errorf("expected error to mention 'summary'; got %q", err.Error())
	}
}

// TestHandleReadScratchpad_EmptySessionIDUsesCanonicalHint pins the
// scratchpad read-side. Without this, the read surface still emits
// the bare "session_id is required" while the write surface emits
// the canonical hint, splitting the agent's mental model.
func TestHandleReadScratchpad_EmptySessionIDUsesCanonicalHint(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error on missing session_id")
	}
	if !strings.Contains(err.Error(), "mpm_context read_wake_context") {
		t.Errorf("scratchpad-read error missing canonical hint; got %q", err.Error())
	}
}

// TestHandlePromoteScratchpad_EmptySessionIDUsesCanonicalHint pins
// the promote path.
func TestHandlePromoteScratchpad_EmptySessionIDUsesCanonicalHint(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error on missing session_id")
	}
	if !strings.Contains(err.Error(), "mpm_context read_wake_context") {
		t.Errorf("scratchpad-promote error missing canonical hint; got %q", err.Error())
	}
}

// TestHandleDiscardScratchpad_EmptySessionIDUsesCanonicalHint pins
// the discard path.
func TestHandleDiscardScratchpad_EmptySessionIDUsesCanonicalHint(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "discard",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error on missing session_id")
	}
	if !strings.Contains(err.Error(), "mpm_context read_wake_context") {
		t.Errorf("scratchpad-discard error missing canonical hint; got %q", err.Error())
	}
}
