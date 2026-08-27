// Regression tests for the empty-content acceptance defect.
//
// The validation report found that `mpm remember ""` and `mpm add "   "`
// both create memories. A persisted memory must contain meaningful
// non-whitespace content. This invariant must hold at the canonical
// INSERT primitive (`saveMemoryRow`) so no caller can bypass it.
package internal

import (
	"strings"
	"testing"
)

// TestSaveMemory_RejectsEmptyContent pins the canonical primitive's
// rejection of empty content. Fails before the fix; passes after.
func TestSaveMemory_RejectsEmptyContent(t *testing.T) {
	dm := NewTestDM(t)
	cases := []struct {
		name    string
		content string
	}{
		{"empty string", ""},
		{"spaces only", "   "},
		{"newlines only", "\n\n\n"},
		{"tabs only", "\t\t"},
		{"mixed whitespace", "\n\t  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dm.SaveMemoryWithExtras("memories", tc.content, "", nil, nil, nil, false, 1, "", "0.5", "0.5", "")
			if err == nil {
				t.Fatalf("expected error for content=%q, got nil", tc.content)
			}
			if !strings.Contains(err.Error(), "empty") && !strings.Contains(err.Error(), "whitespace") && !strings.Contains(err.Error(), "non-empty") {
				t.Errorf("error message should mention empty/whitespace (got: %v)", err)
			}
			// Confirm no artifact was persisted as a side effect.
			var n int
			if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = 'memories'`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("rejected content leaked a row (count=%d)", n)
			}
		})
	}
}

// TestSaveMemory_AcceptsValidContent pins that valid content adjacent
// to the rejected cases still lands in the table.
func TestSaveMemory_AcceptsValidContent(t *testing.T) {
	dm := NewTestDM(t)
	cases := []struct {
		name    string
		content string
	}{
		{"single char", "x"},
		{"single leading space then text", " hello"},
		{"single non-whitespace", "a"},
		{"valid unicode", "café"},
		{"emoji", "📌"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := dm.SaveMemoryWithExtras("memories", tc.content, "", nil, nil, nil, false, 1, "", "0.5", "0.5", "")
			if err != nil {
				t.Fatalf("valid content should succeed (got: %v)", err)
			}
			if id == "" {
				t.Fatal("expected non-empty id")
			}
			// Cleanup so each case starts fresh.
			_, _ = dm.db.Exec(`DELETE FROM memories WHERE id = ?`, id)
		})
	}
}

// TestSaveMemory_EmptyContentDoesNotPersist fences the no-side-effect
// contract: a rejected insert must not leave ANY row in the table.
// This is the structural counterpart to the scanner/rejection pair at
// the canonical primitive.
func TestSaveMemory_EmptyContentDoesNotPersist(t *testing.T) {
	dm := NewTestDM(t)
	for _, content := range []string{"", "  ", "\n\n", "\t\t\t", " \t \n"} {
		_, _ = dm.SaveMemoryWithExtras("memories", content, "", nil, nil, nil, false, 1, "", "0.5", "0.5", "")
	}
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = 'memories'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rejected inserts leaked %d row(s)", n)
	}
}

// TestAddMemoryDirect_EmptyContentRejected pins the fallback path
// (used by MemoryStore.AddMemory when DM is nil) so the validation
// invariant is enforced on both write paths.
func TestAddMemoryDirect_EmptyContentRejected(t *testing.T) {
	dm := NewTestDM(t)

	// Insert one valid memory so the test has something to read.
	if _, err := dm.SaveMemoryWithExtras("memories", "control row", "", nil, nil, nil, false, 1, "", "0.5", "0.5", ""); err != nil {
		t.Fatal(err)
	}

	cases := []string{"", "   ", "\n\t"}
	for _, content := range cases {
		_, err := dm.SaveMemoryWithExtras("memories", content, "", nil, nil, nil, false, 1, "", "0.5", "0.5", "")
		if err == nil {
			t.Errorf("empty content should be rejected (case=%q), got nil", content)
		}
	}

	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = 'memories'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected exactly the control row, got count=%d", n)
	}
}
