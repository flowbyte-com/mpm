package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecuteSteps(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	os.WriteFile(testFile, []byte("hello world"), 0644)

	steps := []map[string]interface{}{
		{"tool": "shell", "args": map[string]interface{}{"command": "echo hello"}},
		{"tool": "read_file", "args": map[string]interface{}{"path": testFile}},
		{"checkpoint": "read file contents"},
	}
	results, err := ExecuteSteps(steps)
	if err != nil {
		t.Fatalf("ExecuteSteps failed: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
	// Check checkpoint was preserved
	if results[2]["checkpoint"] != "read file contents" {
		t.Error("expected checkpoint 'read file contents'")
	}
}

func TestExecuteStepsError(t *testing.T) {
	steps := []map[string]interface{}{
		{"tool": "shell", "args": map[string]interface{}{"command": "exit 1"}},
		{"tool": "shell", "args": map[string]interface{}{"command": "echo should not run"}},
	}
	results, _ := ExecuteSteps(steps)
	// Should stop on error
	if len(results) != 2 {
		t.Errorf("expected 2 results (stopped at error), got %d", len(results))
	}
}

func TestReadFileSemantic(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	os.WriteFile(testFile, []byte("Line 1\nLine 2\nLine 3\nLine 4\nLine 5\nLine 6"), 0644)

	// Test full mode
	result := ReadFileSemantic(testFile, "full")
	if result == "" {
		t.Error("expected non-empty result")
	}

	// Test summary mode
	result = ReadFileSemantic(testFile, "summary")
	if !containsString(result, "File has") {
		t.Error("expected summary with line count")
	}

	// Test code mode (no code structures)
	result = ReadFileSemantic(testFile, "code")
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestWebSynthesize(t *testing.T) {
	result, err := WebSynthesize("Go language")
	if err != nil {
		t.Fatalf("WebSynthesize failed: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	if !containsString(result, "Synthesis for:") {
		t.Error("expected synthesis header in result")
	}
}

func containsString(text, substr string) bool {
	for i := 0; i+len(substr) <= len(text); i++ {
		if text[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}