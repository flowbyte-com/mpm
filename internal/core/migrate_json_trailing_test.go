// migrate_json_trailing_test.go — Tranche M §1 verification.
//
// Verify the trailing-data implementation in migrate_json.go.  The
// current code uses:
//
//	dec.More()  after the first top-level Decode()
//
// The user wants focused tests that prove this is a reliable
// top-level trailing-data check.  If dec.More() is NOT reliable,
// replace it with the canonical:
//
//	first Decode(value)
//	second Decode(dummy)  // must error with io.EOF
//
// If the implementation is correct, make no change.
//
// All inputs are at the top level.  dec.More() peeks past whitespace
// for the next non-space rune; a top-level value that follows the
// first Decode must be detected.
package internal

import (
	"strings"
	"testing"
)

// mustRejectTrailing pins each input as a top-level trailing-data
// violation: ParseJsonFactsWithReport must return a file-level error
// whose message contains "trailing".
func mustRejectTrailing(t *testing.T, input string) {
	t.Helper()
	_, _, err := ParseJsonFactsWithReport(input, "/tmp/trail.json")
	if err == nil {
		t.Fatalf("input %q must error as trailing data", input)
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("input %q: error must mention 'trailing'; got %q", input, err.Error())
	}
}

// mustAcceptTrailing pins each input as a valid single JSON document:
// ParseJsonFactsWithReport must NOT return a file-level error, and
// must NOT emit a per-entry error for the document itself.
func mustAcceptTrailing(t *testing.T, input string, want int) {
	t.Helper()
	facts, perEntryErrors, err := ParseJsonFactsWithReport(input, "/tmp/solo.json")
	if err != nil {
		t.Fatalf("input %q must succeed; got %v", input, err)
	}
	if len(perEntryErrors) != 0 {
		t.Fatalf("input %q: unexpected per-entry errors: %v", input, perEntryErrors)
	}
	if len(facts) != want {
		t.Fatalf("input %q: expected %d facts, got %d", input, want, len(facts))
	}
}

// TestTrailingData_TopLevelRejections pins the REJECTED cases from the
// brief.  Each input has TWO valid JSON values concatenated at the top
// level — this is what we want to reject.
func TestTrailingData_TopLevelRejections(t *testing.T) {
	t.Run("two-empty-objects", func(t *testing.T) {
		mustRejectTrailing(t, "{} {}")
	})
	t.Run("two-empty-arrays", func(t *testing.T) {
		mustRejectTrailing(t, "[] []")
	})
	t.Run("object-followed-by-bool", func(t *testing.T) {
		mustRejectTrailing(t, `{"a":1} true`)
	})
	t.Run("two-numbers", func(t *testing.T) {
		mustRejectTrailing(t, `42 43`)
	})
	t.Run("two-strings", func(t *testing.T) {
		mustRejectTrailing(t, `"x" "y"`)
	})
}

// TestTrailingData_StandaloneAccepts pins the ACCEPTED cases from the
// brief.  Single valid migration documents (with the schema gate at the
// migrator level) MUST NOT trigger the trailing-data error — even with
// trailing whitespace.
//
// Note: the test inputs here are valid migration documents, not just
// valid JSON.  Bare `{}` and `[1,2,3]` are not migration documents; the
// migrator rejects them at the schema gate (a separate concern from
// the trailing-data check we're verifying).  dec.More() runs BEFORE
// the schema gate, so the schema rejection proves the trailing check
// did not falsely fire.
func TestTrailingData_StandaloneAccepts(t *testing.T) {
	// Wrapped forms (which the migrator accepts) with trailing whitespace.
	t.Run("wrapped-array-alone", func(t *testing.T) {
		mustAcceptTrailing(t, `{"memories":[{"content":"x"}]}`, 1)
	})
	t.Run("wrapped-array-trailing-whitespace", func(t *testing.T) {
		mustAcceptTrailing(t, `{"memories":[{"content":"x"}]}   `, 1)
	})
	t.Run("wrapped-array-trailing-newline", func(t *testing.T) {
		mustAcceptTrailing(t, "{\"memories\":[{\"content\":\"x\"}]}\n\n", 1)
	})
	t.Run("wrapped-array-trailing-tab", func(t *testing.T) {
		mustAcceptTrailing(t, "{\"memories\":[{\"content\":\"x\"}]}\t\t", 1)
	})
	t.Run("wrapped-array-trailing-cr", func(t *testing.T) {
		mustAcceptTrailing(t, "{\"memories\":[{\"content\":\"x\"}]}\r\n", 1)
	})

	// Top-level array forms (which the migrator accepts) with trailing whitespace.
	t.Run("array-alone", func(t *testing.T) {
		mustAcceptTrailing(t, `[{"content":"x"}]`, 1)
	})
	t.Run("array-trailing-whitespace", func(t *testing.T) {
		mustAcceptTrailing(t, `[{"content":"x"}]   `, 1)
	})
	t.Run("array-trailing-newline", func(t *testing.T) {
		mustAcceptTrailing(t, "[{\"content\":\"x\"}]\n", 1)
	})

	// Negative-of-trailing: confirm that bare JSON values NOT in the
	// accepted schema do NOT error with "trailing" — they error with
	// the schema gate instead.  This proves the trailing check ran
	// cleanly even when the schema check rejects.
	t.Run("bare-object-with-trailing-ws", func(t *testing.T) {
		// `{}` is valid JSON, valid trailing, but rejected at the
		// schema gate.  The error must NOT contain "trailing" — that
		// would prove the trailing check fired on valid input.
		_, _, err := ParseJsonFactsWithReport(`{}   `, "/tmp/x.json")
		if err == nil {
			t.Fatal("expected schema error, got nil")
		}
		if strings.Contains(err.Error(), "trailing") {
			t.Errorf("schema rejection must NOT mention 'trailing'; got %q", err.Error())
		}
	})
}
