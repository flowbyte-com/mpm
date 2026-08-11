package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCallRoute_RequiresPrompt(t *testing.T) {
	dm := newTestDMForCmd(t)
	_, err := runHandler(dm, "mpm_context", map[string]interface{}{
		"action": "route",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("callRoute with empty payload should require prompt field")
	}
}

func TestCallRoute_ReturnsReport(t *testing.T) {
	// Hermetic workspace with synthetic mode/persona files. The previous
	// version of this test hardcoded /home/v/workspace/projects/mpm as the
	// workspace, which passed locally but crashed in CI ("no such file or
	// directory") because the runner has a different checkout path. Build
	// a controlled workspace so the assertions don't depend on the host
	// filesystem layout.
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "mode"))
	mustMkdir(t, filepath.Join(workspace, "persona"))
	mustWriteFile(t, filepath.Join(workspace, "mode", "architect.md"),
		"---\nname: architect\npatterns: architecture\n---\n\n# Architect Mode\n\nArchitect mode body.\n")
	mustWriteFile(t, filepath.Join(workspace, "persona", "venkat.md"),
		"---\nname: venkat\npatterns: architecture\n---\n\n# Venkat Persona\n\nVenkat persona body.\n")

	dm := newTestDMForCmd(t)
	t.Setenv("MPM_ROUTE_WORKSPACE", workspace)
	t.Setenv("MPM_WORKSPACE", workspace)

	result, err := runHandler(dm, "mpm_context", map[string]interface{}{
		"action": "route",
		"params": map[string]interface{}{
			"prompt": "Design the system architecture for our new API gateway",
		},
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

// silence unused-import warnings if os is dropped during future refactors.
var _ = os.Getenv
