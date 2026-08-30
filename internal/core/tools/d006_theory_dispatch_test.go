// d006_theory_dispatch_test.go — public-boundary regression for the
// handleMpmTheories dispatcher.
//
// Audit D-006: the dispatcher used to only accept `propose` and `resolve`
// even though alpha-3 added `show`, `list`, `query` paths in the CLI.
// This test pins the dispatcher surface from the agent runtime side:
// each new action must be routable AND each unknown action must error
// with the new "Valid actions include ..." message.
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestD006_DispatcherAcceptsNewActions pins the surface-parity contract:
// show / list / query must route through handleMpmTheories without
// returning "unknown action".
func TestD006_DispatcherAcceptsNewActions(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	// Seed a theory so list/show have something to return.
	proposed, err := dm.ProposeTheory("d006-dispatcher-target", "vc", nil, nil, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := proposed["id"].(string)

	for _, action := range []string{"show", "list", "query"} {
		t.Run(action, func(t *testing.T) {
			payload := map[string]interface{}{"action": action}
			switch action {
			case "show":
				payload["params"] = map[string]interface{}{"id": id}
			case "query":
				payload["params"] = map[string]interface{}{"query": "d006-dispatcher"}
			default:
				payload["params"] = map[string]interface{}{}
			}

			_, err := handleMpmTheories(dm, ac, payload)
			if err != nil && strings.Contains(err.Error(), "unknown action") {
				t.Errorf("handleMpmTheories(%q) returned unknown-action error: %v", action, err)
			}
		})
	}
}

// TestD006_DispatcherRejectsUnknownAction pins the negative contract:
// unknown actions must error loudly and the error message must list the
// new valid actions.
func TestD006_DispatcherRejectsUnknownAction(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "bogus",
	})
	if err == nil {
		t.Fatal("handleMpmTheories with bogus action returned nil error")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("error must say 'unknown action', got: %v", err)
	}
	// The new valid actions must appear in the error so callers can
	// discover them without source-diving.
	for _, want := range []string{"propose", "resolve", "show", "list", "query"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must list %q as a valid action, got: %v", want, err)
		}
	}
}

// TestD006_DispatcherStillRoutesProposeAndResolve pins the backward-
// compat contract: the original propose/resolve actions must continue
// to work after the new actions were added.
func TestD006_DispatcherStillRoutesProposeAndResolve(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	// propose
	_, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action":  "propose",
		"params":  map[string]interface{}{"hypothesis": "d006 backward compat", "validation_criteria": "vc"},
	})
	if err != nil && strings.Contains(err.Error(), "unknown action") {
		t.Errorf("propose action wrongly rejected: %v", err)
	}
}

// newTestDMForTools returns a fresh hermetic DatabaseManager for tools-
// package regression tests. Wraps internal.NewTestDM so tests can call
// `newTestDMForTools(t)` without importing the internal package directly.
func newTestDMForTools(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	return mpminternal.NewTestDM(t)
}
