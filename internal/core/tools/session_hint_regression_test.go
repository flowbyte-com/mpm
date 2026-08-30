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

// TestHandleHandoffWrite_EmptySessionIDUsesCanonicalHint covers one
// of the routing sites: handleHandoffWrite must surface the canonical
// hint message when called without session_id. This pins the actual
// wire response so a future refactor that reverts to a bare
// fmt.Errorf would fail this test.
func TestHandleHandoffWrite_EmptySessionIDUsesCanonicalHint(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleMpmHandoff(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary": "test",
		},
	})
	if err == nil {
		t.Fatal("expected error on missing session_id")
	}

	for _, want := range []string{"session_id is required", "mpm_context read_wake_context", "MPM_SESSION_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("handoff-write error missing %q; got %q", want, err.Error())
		}
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
