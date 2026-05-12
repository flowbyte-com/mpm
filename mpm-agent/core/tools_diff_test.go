package core

import (
	"os"
	"strings"
	"testing"
)

func TestApplyDiff(t *testing.T) {
	// Create a temp file with known content
	tmp, err := os.CreateTemp("", "test_diff_*.txt")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString("line1\nline2\nline3\n")
	tmp.Close()

	// Diff with context lines matching the actual file content
	diff := `--- a
+++ b
@@ -1,3 +1,4 @@
 line1
 line2
+inserted
 line3
`

	result, err := applyDiff(map[string]interface{}{
		"file": tmp.Name(),
		"diff": diff,
	})
	if err != nil {
		t.Fatalf("applyDiff failed: %v", err)
	}
	if !strings.Contains(result, "Patched") {
		t.Errorf("expected Patched in result, got: %s", result)
	}

	// Verify content
	content, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !strings.Contains(string(content), "inserted") {
		t.Errorf("expected 'inserted' in file after patch, got: %s", string(content))
	}
}

func TestApplyDiffNoChanges(t *testing.T) {
	tmp, _ := os.CreateTemp("", "test_diff_*.txt")
	defer os.Remove(tmp.Name())
	tmp.WriteString("line1\nline2\nline3\n")
	tmp.Close()

	// Diff that matches exactly — no changes needed
	diff := `--- a
+++ b
@@ -1,3 +1,3 @@
 line1
 line2
 line3
`

	result, err := applyDiff(map[string]interface{}{
		"file": tmp.Name(),
		"diff": diff,
	})
	if err != nil {
		t.Fatalf("applyDiff failed: %v", err)
	}
	if result != "No changes needed" {
		t.Errorf("expected 'No changes needed', got: %s", result)
	}
}

func TestApplyDiffInvalidInput(t *testing.T) {
	_, err := applyDiff(map[string]interface{}{"file": "", "diff": "something"})
	if err == nil {
		t.Error("expected error for empty file path")
	}

	_, err = applyDiff(map[string]interface{}{"file": "/nonexistent", "diff": ""})
	if err == nil {
		t.Error("expected error for empty diff")
	}
}