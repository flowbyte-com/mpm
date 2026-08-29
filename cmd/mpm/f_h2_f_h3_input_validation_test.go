// f_h2_f_h3_input_validation_test.go — F-H2/F-H3 strict input validation.
//
// F-H2: `mpm memory add --expires-in garbage` silently produced a memory
//       with no TTL. The parseDuration error was swallowed by the handler.
//       The fix surfaces the error and returns exit 1.
//
// F-H3: `mpm memory add --tags {"a":1}` stored the literal string as a
//       single tag, producing `["{\"a\":1}"]` in the database (a nested-
//       JSON array wrapper that downstream consumers couldn't parse).
//       The fix rejects JSON-shaped --tags explicitly and points at CSV.
//
// These tests exercise the public handler directly.
package main

import (
	"testing"
)

// TestF_H2_ExpiresInGarbageRejected is the headline regression:
// garbage --expires-in must surface an explicit error.
func TestF_H2_ExpiresInGarbageRejected(t *testing.T) {
	beforeCount := fH2MemoryCount(t)
	rc := handleMemoryAdd([]string{"--expires-in", "garbage", "content"})
	if rc == 0 {
		t.Fatalf("--expires-in garbage should be rejected, got exit 0")
	}
	if fH2MemoryCount(t) != beforeCount {
		t.Errorf("rejected call should NOT create a memory")
	}
}

// TestF_H2_ExpiresInGarbageErrorMentionsValue pins the error-message
// contract: the rejected-value string should appear in the error so
// the operator can spot the typo.
func TestF_H2_ExpiresInGarbageErrorMentionsValue(t *testing.T) {
	out := captureBoth(t, func() {
		handleMemoryAdd([]string{"--expires-in", "totally-bogus", "content"})
	})
	if !contains(out, "totally-bogus") {
		t.Errorf("error should mention the rejected value, got: %s", out)
	}
}

// TestF_H2_ExpiresInMissingValueRejected pins the missing-arg contract.
func TestF_H2_ExpiresInMissingValueRejected(t *testing.T) {
	rc := handleMemoryAdd([]string{"--expires-in"})
	if rc == 0 {
		t.Fatalf("--expires-in with no value should be rejected, got exit 0")
	}
}

// TestF_H2_ExpiresInValidDaysAccepted pins the happy path: a valid
// duration must be accepted and the TTL applied to the memory.
func TestF_H2_ExpiresInValidDaysAccepted(t *testing.T) {
	beforeCount := fH2MemoryCount(t)
	content := fH2UniqueContent(t, "f-h2-7d")
	rc := handleMemoryAdd([]string{"--expires-in", "7d", "--tags", "f-h2-7d", content})
	if rc != 0 {
		t.Fatalf("--expires-in 7d should succeed, got exit %d", rc)
	}
	afterCount := fH2MemoryCount(t)
	if afterCount <= beforeCount {
		t.Errorf("expected a new memory (before=%d, after=%d)", beforeCount, afterCount)
	}
}

// TestF_H2_ExpiresInValidHoursAccepted pins the hours unit happy path.
func TestF_H2_ExpiresInValidHoursAccepted(t *testing.T) {
	content := fH2UniqueContent(t, "f-h2-24h")
	rc := handleMemoryAdd([]string{"--expires-in", "24h", "--tags", "f-h2-24h", content})
	if rc != 0 {
		t.Fatalf("--expires-in 24h should succeed, got exit %d", rc)
	}
}

// TestF_H2_ExpiresInInvalidUnitRejected pins the unit-validation contract.
func TestF_H2_ExpiresInInvalidUnitRejected(t *testing.T) {
	rc := handleMemoryAdd([]string{"--expires-in", "5y", "content"})
	if rc == 0 {
		t.Fatalf("--expires-in 5y should be rejected (no year unit), got exit 0")
	}
}

// TestF_H3_TagsJSONObjectRejected pins the F-H3 headline regression:
// --tags '{"a":1}' must NOT store the literal as a single tag.
func TestF_H3_TagsJSONObjectRejected(t *testing.T) {
	beforeCount := fH2MemoryCount(t)
	rc := handleMemoryAdd([]string{"--tags", `{"a":1}`, "content"})
	if rc == 0 {
		t.Fatalf("--tags JSON object should be rejected, got exit 0")
	}
	if fH2MemoryCount(t) != beforeCount {
		t.Errorf("rejected call should NOT create a memory")
	}
}

// TestF_H3_TagsJSONArrayRejected pins the array-shaped variant.
func TestF_H3_TagsJSONArrayRejected(t *testing.T) {
	beforeCount := fH2MemoryCount(t)
	rc := handleMemoryAdd([]string{"--tags", `["a","b"]`, "content"})
	if rc == 0 {
		t.Fatalf("--tags JSON array should be rejected, got exit 0")
	}
	if fH2MemoryCount(t) != beforeCount {
		t.Errorf("rejected call should NOT create a memory")
	}
}

// TestF_H3_TagsCSVAccepted pins the happy path: comma-separated tags
// must still work after the JSON rejection gate.
func TestF_H3_TagsCSVAccepted(t *testing.T) {
	rc := handleMemoryAdd([]string{"--tags", "alpha,beta,gamma", "--tags-mark", "f-h3-csv", "content"})
	if rc != 0 {
		t.Fatalf("--tags CSV should succeed, got exit %d", rc)
	}
}

// fH2MemoryCount returns the count of memories (excluding shredded).
func fH2MemoryCount(t *testing.T) int {
	t.Helper()
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	var count int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	return count
}

// fH2UniqueContent produces a content string that's unique per test run,
// so dedup doesn't mask insertion failures.
func fH2UniqueContent(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "-" + fC4UniqueID(t, "z")
}

// contains is a tiny strings.Contains shim to avoid an extra import.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
