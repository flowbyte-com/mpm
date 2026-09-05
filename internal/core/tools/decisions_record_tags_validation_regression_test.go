// decisions_record_tags_validation_regression_test.go — Pass 4 defect C.13.
//
// The 2026-09-05 audit found mpm_decisions record silently drops
// invalid tag shapes. The schema declares `tags` as
// `"type": "array", "items": {"type": "string"}` (optional in the
// required array), but handleRecordDecision pipes p["tags"] through
// internal.ParseStringSliceOr, which returns nil for any non-array
// input and silently filters non-string array elements. The decision
// is then recorded with `tags = []` rather than rejecting the input.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_decisions --payload '{"action":"record","params":{"choice":"X","tags":"a"}}'
//     # wanted: error mentioning the tags contract
//     # actual: success, decision recorded with tags=[]
//
// Canonical contract (per registry schema):
//
//   omitted        → tags = [] (legitimate; field is optional)
//   null           → tags = [] (null omission is equivalent)
//   []             → tags = [] (empty array is the explicit form)
//   ["a","b"]      → tags = ["a","b"]
//   "a"            → ERROR (string is not an array)
//   123            → ERROR (number is not an array)
//   ["a", 123]     → ERROR (mixed-type array; all elements must be strings)
//
// The fix enforces the schema's array-of-strings contract at the
// handler boundary. Silent coercion is the defect.

package tools

import (
	"strings"
	"testing"
)

// TestDecisionsRecord_ValidArrayPersists pins the positive path:
// a valid string array flows through to the persisted decision
// unchanged.
func TestDecisionsRecord_ValidArrayPersists(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "test-choice-valid-tags",
			"tags":   []interface{}{"alpha", "beta", "gamma"},
		},
	})
	if err != nil {
		t.Fatalf("valid tag array must not error: %v", err)
	}
	if res == nil {
		t.Fatalf("valid tag array must produce a decision, got nil")
	}
}

// TestDecisionsRecord_OmittedTagsValid pins the documented
// optionality: omitting tags (key absent or value nil) is the
// legitimate "no tags" path. The fix must not over-correct.
func TestDecisionsRecord_OmittedTagsValid(t *testing.T) {
	dm := newTestSharedDM(t)

	for label, val := range map[string]interface{}{
		"absent": nil,
		"nil":    nil,
	} {
		t.Run(label, func(t *testing.T) {
			params := map[string]interface{}{"choice": "test-omit-" + label}
			if val != nil {
				params["tags"] = val
			}
			_, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
				"action": "record",
				"params": params,
			})
			if err != nil {
				t.Errorf("omitted tags must not error (%s): %v", label, err)
			}
		})
	}
}

// TestDecisionsRecord_EmptyArrayValid pins: empty array is the
// explicit form of "no tags" and must be accepted. Distinguishing
// omitted from empty is part of the contract.
func TestDecisionsRecord_EmptyArrayValid(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "test-empty-array",
			"tags":   []interface{}{},
		},
	})
	if err != nil {
		t.Errorf("empty array must not error, got: %v", err)
	}
}

// TestDecisionsRecord_ScalarRejected pins: a scalar (string or
// number) passed where the schema requires an array must error
// loudly rather than silently coerced.
func TestDecisionsRecord_ScalarRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, scalar := range []interface{}{"a", 123, true} {
		t.Run("scalar", func(t *testing.T) {
			_, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
				"action": "record",
				"params": map[string]interface{}{
					"choice": "test-scalar",
					"tags":   scalar,
				},
			})
			if err == nil {
				t.Fatalf("scalar tags (%T) must error", scalar)
			}
			if !strings.Contains(err.Error(), "tags") {
				t.Errorf("error must mention 'tags', got: %v", err)
			}
		})
	}
}

// TestDecisionsRecord_MixedTypeArrayRejected pins: an array whose
// elements are not all strings is rejected. The schema's `items:
// {"type":"string"}` is strict.
func TestDecisionsRecord_MixedTypeArrayRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "test-mixed",
			"tags":   []interface{}{"a", 123},
		},
	})
	if err == nil {
		t.Fatalf("mixed-type array must error")
	}
	if !strings.Contains(err.Error(), "tags") {
		t.Errorf("error must mention 'tags', got: %v", err)
	}
}

// TestDecisionsRecord_InvalidTagsDoesNotPersist pins the
// write-path guarantee: rejected input does not create a decision.
// Counts decisions (collection='decisions' in the memories table)
// before and after a failed record call.
func TestDecisionsRecord_InvalidTagsDoesNotPersist(t *testing.T) {
	dm := newTestSharedDM(t)

	beforeRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = ?`, "decisions")
	beforeCount := 0
	if err := beforeRow.Scan(&beforeCount); err != nil {
		t.Fatalf("count decisions before: %v", err)
	}

	_, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "test-no-persist",
			"tags":   "scalar-not-array",
		},
	})
	if err == nil {
		t.Fatalf("scalar tags must error")
	}

	afterRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = ?`, "decisions")
	afterCount := 0
	if err := afterRow.Scan(&afterCount); err != nil {
		t.Fatalf("count decisions after: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("rejected tags must not create a decision; before=%d, after=%d",
			beforeCount, afterCount)
	}
}
