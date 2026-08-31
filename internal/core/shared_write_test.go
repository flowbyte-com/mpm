package internal

import (
	"fmt"
	"testing"
	"time"
)

// TestRecordGlobalRule_RequiresSharedDB verifies that calling
// RecordGlobalRule when MPM_SHARED_DB is unset returns an error
// rather than silently writing to the local DB. This is the
// "no fall-through to local" invariant — global rules have no
// meaning outside the shared store.
func TestRecordGlobalRule_RequiresSharedDB(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	_, err := dm.RecordGlobalRule("test rule", nil, 10, "")
	if err == nil {
		t.Fatal("expected error when shared DB not attached, got nil")
	}
}

// TestRecordGlobalRule_HappyPath attaches a shared DB, writes a rule,
// then reads it back via QueryGlobalRules. Verifies the write/read
// round-trip works.
func TestRecordGlobalRule_HappyPath(t *testing.T) {
	dm := NewTestSharedDM(t)

	id, err := dm.RecordGlobalRule("Never expose API keys to the LLM", []string{"security", "house-rule"}, 10, "test")
	if err != nil {
		t.Fatalf("RecordGlobalRule: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty rule id")
	}

	rules, err := dm.QueryGlobalRules("", 10)
	if err != nil {
		t.Fatalf("QueryGlobalRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0]["id"] != id {
		t.Errorf("id mismatch: got %v, want %s", rules[0]["id"], id)
	}
	if rules[0]["weight"] != 10 {
		t.Errorf("weight mismatch: got %v, want 10", rules[0]["weight"])
	}
}

// TestRecordGlobalRule_RejectsInvalidWeight verifies the weight range
// check (0-100). Out-of-range weights are a programming error; we
// reject at write time rather than accept and clamp.
func TestRecordGlobalRule_RejectsInvalidWeight(t *testing.T) {
	dm := NewTestSharedDM(t)

	for _, bad := range []float64{-1, 101, 9999} {
		_, err := dm.RecordGlobalRule("rule", nil, bad, "")
		if err == nil {
			t.Errorf("expected error for weight=%v, got nil", bad)
		}
	}
}

// TestRecordGlobalRule_RejectsEmptyContent verifies content is required.
func TestRecordGlobalRule_RejectsEmptyContent(t *testing.T) {
	dm := NewTestSharedDM(t)

	_, err := dm.RecordGlobalRule("", nil, 10, "")
	if err == nil {
		t.Fatal("expected error for empty content, got nil")
	}
}

// TestPromoteToGlobal_RequiresSharedDB verifies the same no-fall-through
// invariant for promotion.
func TestPromoteToGlobal_RequiresSharedDB(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	_, err := dm.PromoteToGlobal("any-id")
	if err == nil {
		t.Fatal("expected error when shared DB not attached, got nil")
	}
}

// TestPromoteToGlobal_CopiesLocalMemory verifies the promotion flow:
// local memory is preserved, shared copy is created with lineage metadata.
func TestPromoteToGlobal_CopiesLocalMemory(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Insert a local memory directly. The id is unique-per-run so the
	// test never collides with prior runs (and never relies on stale
	// rows in the workspace DB, which the prior version of this test
	// accidentally did via a hardcoded "local-001" lookup).
	localWriteID := fmt.Sprintf("local-shared-write-%d", time.Now().UnixNano())
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		 VALUES (?, 'memories', 'a useful fact about shell scripts', 5, NULL, '2026-06-26', '2026-06-26')`,
		localWriteID,
	)
	if err != nil {
		t.Fatalf("insert local: %v", err)
	}

	// Promote to global.
	sharedID, err := dm.PromoteToGlobal(localWriteID)
	if err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	if sharedID == "" {
		t.Fatal("expected non-empty shared id")
	}
	if sharedID == localWriteID {
		t.Error("shared id should differ from local id")
	}

	// Local memory is preserved.
	var localContent string
	dm.SQLDB().QueryRow("SELECT content FROM memories WHERE id = ?", localWriteID).Scan(&localContent)
	if localContent != "a useful fact about shell scripts" {
		t.Errorf("local memory was modified: %q", localContent)
	}

	// Shared copy has correct content + lineage.
	rules, err := dm.QueryGlobalRules("", 10)
	if err != nil {
		t.Fatalf("QueryGlobalRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0]["content"] != "a useful fact about shell scripts" {
		t.Errorf("shared copy content mismatch: %v", rules[0]["content"])
	}
	meta, _ := rules[0]["metadata"].(string)
	if !contains(meta, "derived_from_local_id") || !contains(meta, localWriteID) {
		t.Errorf("shared copy missing lineage metadata: %q", meta)
	}
}

// TestPromoteToGlobal_RejectsMissingLocal verifies the not-found path.
func TestPromoteToGlobal_RejectsMissingLocal(t *testing.T) {
	dm := NewTestSharedDM(t)

	_, err := dm.PromoteToGlobal("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for missing local memory, got nil")
	}
}

// contains is a tiny string-contains helper to avoid the strings import.
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}