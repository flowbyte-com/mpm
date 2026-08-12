// drill_loader_test.go — pin the YAML parsing contract.
//
// Why this test matters: drill specs are user-editable YAML. The loader
// is the single point that translates human input into the Go-typed
// Expect shape that the harness and scorer read from. A silent parse
// regression here breaks every downstream drill.

package internal

import (
	"path/filepath"
	"testing"
)

func TestLoadDrill_ParsesValidYAML(t *testing.T) {
	d, err := LoadDrill(filepath.Join("testdata", "lesson-persistence-001.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d.ID != "lesson-persistence-001" {
		t.Fatalf("id = %q, want lesson-persistence-001", d.ID)
	}
	if d.Framework != "synthetic" {
		t.Fatalf("framework = %q, want synthetic", d.Framework)
	}
	if len(d.Expect.ToolsRequired) != 2 {
		t.Fatalf("got %d tools_required, want 2", len(d.Expect.ToolsRequired))
	}
	if len(d.Expect.Sequence) != 2 {
		t.Fatalf("got %d sequence steps, want 2", len(d.Expect.Sequence))
	}
	if d.TimeoutSecs != 10 {
		t.Fatalf("timeout_secs = %d, want 10", d.TimeoutSecs)
	}
}

func TestLoadDrill_RejectsMissingFile(t *testing.T) {
	if _, err := LoadDrill("/nonexistent/path.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestLoadAllDrills_FindsFixturesOnly verifies that LoadAllDrills
// picks up the fixture files in testdata. The full PASS/FAIL
// harness-based tests for each fixture live in drill_score_test.go
// (TestFixtures_*) and require the harness from drill_harness.go.
func TestLoadAllDrills_FindsFixturesOnly(t *testing.T) {
	drills, err := LoadAllDrills("testdata")
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	// At minimum we have lesson-persistence-001 today (Task 12 adds
	// the other two). Keep the assertion tolerant so this test stays
	// green as fixtures land.
	if len(drills) < 1 {
		t.Fatalf("expected at least one fixture, got %d", len(drills))
	}
	for _, d := range drills {
		if d.ID == "" || d.Framework == "" {
			t.Errorf("incomplete fixture loaded: %+v", d)
		}
	}
}
