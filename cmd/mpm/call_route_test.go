package main

import "testing"

func TestCallRoute_RequiresPrompt(t *testing.T) {
	_, err := callRoute(map[string]interface{}{})
	if err == nil {
		t.Fatal("callRoute with empty payload should require prompt field")
	}
}

func TestCallRoute_ReturnsReport(t *testing.T) {
	// Use the live workspace — matches the pattern in cmd/mpm-mcp/tools.go
	// and internal/router_test.go. Sets MPM_ROUTE_WORKSPACE for the test scope.
	t.Setenv("MPM_ROUTE_WORKSPACE", "/home/v/workspace/projects/mpm")

	result, err := callRoute(map[string]interface{}{
		"prompt": "Design the system architecture for our new API gateway",
	})
	if err != nil {
		t.Fatalf("callRoute: %v", err)
	}

	report, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("callRoute result is not a map: %T", result)
	}
	modes, ok := report["selected_modes"].([]string)
	if !ok || len(modes) == 0 {
		t.Errorf("callRoute result missing or empty selected_modes: %v", report["selected_modes"])
	}
}
