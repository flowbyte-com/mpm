package main

import (
	"encoding/json"
	"testing"
)

func TestCallRoute_RequiresPrompt(t *testing.T) {
	dm := newTestDMForCmd(t)
	_, err := runHandler(dm, "route", map[string]interface{}{})
	if err == nil {
		t.Fatal("callRoute with empty payload should require prompt field")
	}
}

func TestCallRoute_ReturnsReport(t *testing.T) {
	// Use the live workspace — matches the pattern in cmd/mpm-mcp/tools.go
	// and internal/router_test.go. Sets MPM_ROUTE_WORKSPACE for the test scope.
	dm := newTestDMForCmd(t)
	t.Setenv("MPM_ROUTE_WORKSPACE", "/home/v/workspace/projects/mpm")

	result, err := runHandler(dm, "route", map[string]interface{}{
		"prompt": "Design the system architecture for our new API gateway",
	})
	if err != nil {
		t.Fatalf("callRoute: %v", err)
	}

	// Round-trip through JSON to verify the wire shape — the actual contract.
	// The in-memory return type is internal.RoutingReport; the JSON envelope
	// at call.go:91 handles serialization via struct tags.
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var report map[string]interface{}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	modes, ok := report["selected_modes"].([]interface{})
	if !ok || len(modes) == 0 {
		t.Errorf("callRoute JSON missing or empty selected_modes: %v", report["selected_modes"])
	}
	if _, ok := report["selected_persona"].(string); !ok {
		t.Errorf("callRoute JSON missing selected_persona: %v", report["selected_persona"])
	}
	if _, ok := report["scores"].(map[string]interface{}); !ok {
		t.Errorf("callRoute JSON missing scores: %v", report["scores"])
	}
}
