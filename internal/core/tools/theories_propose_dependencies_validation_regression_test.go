// theories_propose_dependencies_validation_regression_test.go —
// Pass 4 defect C.14.
//
// The 2026-09-05 audit found mpm_theories.propose silently dropped
// invalid `dependencies` shapes. The schema declares
// `"type":"array","items":{"type":"string"}` (a list of theory ids
// persisted by encodeDependencyList), but handleProposeTheory routed
// the value through internal.ParseStringSliceOr, which silently
// returned nil for non-array scalars and silently dropped non-string
// elements from arrays. The theory row was persisted with
// dependencies=[] and the wake-on-delete scan treated it as a
// standalone theory — caller intent (forward-edge declaration) was
// lost without any error.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_theories --payload '{"action":"propose","params":{"hypothesis":"H","dependencies":"the-1"}}'
//     # wanted: error mentioning the dependencies contract
//     # actual: success, theory persisted with dependencies=[]
//
// Canonical contract (per registry schema and
// encodeDependencyList):
//
//   omitted        → dependencies = [] (legitimate; field is optional)
//   null           → dependencies = [] (null omission equivalent)
//   []             → dependencies = [] (empty array; the explicit form)
//   ["the-1","the-2"] → dependencies = ["the-1","the-2"]
//   "the-1"        → ERROR (string is not an array)
//   123            → ERROR (number is not an array)
//   ["the-1", 123] → ERROR (mixed-type array)
//
// The fix enforces the schema's array-of-strings contract at the
// handler boundary.

package tools

import (
	"strings"
	"testing"
)

// TestTheoriesPropose_ValidArrayPersists pins the positive path: a
// valid string array flows through to the persisted theory row.
func TestTheoriesPropose_ValidArrayPersists(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":   "valid-deps-hypothesis",
			"dependencies": []interface{}{"the-1", "the-2"},
		},
	})
	if err != nil {
		t.Fatalf("valid deps array must not error: %v", err)
	}
	if res == nil {
		t.Fatalf("valid deps array must produce a theory, got nil")
	}
}

// TestTheoriesPropose_OmittedDepsValid pins the documented
// optionality: omitting dependencies (key absent or value nil) is
// the legitimate "no dependencies" path.
func TestTheoriesPropose_OmittedDepsValid(t *testing.T) {
	dm := newTestSharedDM(t)

	for label, val := range map[string]interface{}{
		"absent": nil,
		"nil":    nil,
	} {
		t.Run(label, func(t *testing.T) {
			params := map[string]interface{}{"hypothesis": "omit-" + label}
			if val != nil {
				params["dependencies"] = val
			}
			_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
				"action": "propose",
				"params": params,
			})
			if err != nil {
				t.Errorf("omitted deps must not error (%s): %v", label, err)
			}
		})
	}
}

// TestTheoriesPropose_EmptyArrayValid pins: empty array is the
// explicit form of "no dependencies" and must be accepted.
// Distinguishing omitted from empty is part of the contract.
func TestTheoriesPropose_EmptyArrayValid(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":   "empty-array-hypothesis",
			"dependencies": []interface{}{},
		},
	})
	if err != nil {
		t.Errorf("empty deps array must not error, got: %v", err)
	}
}

// TestTheoriesPropose_ScalarRejected pins: a scalar (string or
// number) passed where the schema requires an array must error
// loudly rather than silently coerced.
func TestTheoriesPropose_ScalarRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, scalar := range []interface{}{"the-1", 123, true} {
		t.Run("scalar", func(t *testing.T) {
			_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
				"action": "propose",
				"params": map[string]interface{}{
					"hypothesis":   "scalar-hypothesis",
					"dependencies": scalar,
				},
			})
			if err == nil {
				t.Fatalf("scalar deps (%T) must error", scalar)
			}
			if !strings.Contains(err.Error(), "dependencies") {
				t.Errorf("error must mention 'dependencies', got: %v", err)
			}
		})
	}
}

// TestTheoriesPropose_MixedTypeArrayRejected pins: an array whose
// elements are not all strings is rejected. Theory ids are strings;
// integers/booleans have no canonical mapping.
func TestTheoriesPropose_MixedTypeArrayRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":   "mixed-hypothesis",
			"dependencies": []interface{}{"the-1", 123},
		},
	})
	if err == nil {
		t.Fatalf("mixed-type array must error")
	}
	if !strings.Contains(err.Error(), "dependencies") {
		t.Errorf("error must mention 'dependencies', got: %v", err)
	}
}

// TestTheoriesPropose_InvalidDepsDoesNotPersist pins the
// write-path guarantee: rejected input does not create a theory row
// with silently-wiped dependencies. Theories are stored in memories
// with collection='theories'.
func TestTheoriesPropose_InvalidDepsDoesNotPersist(t *testing.T) {
	dm := newTestSharedDM(t)

	beforeRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = ?`, "theories")
	beforeCount := 0
	if err := beforeRow.Scan(&beforeCount); err != nil {
		t.Fatalf("count theories before: %v", err)
	}

	_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":   "no-persist-hypothesis",
			"dependencies": "scalar-not-array",
		},
	})
	if err == nil {
		t.Fatalf("scalar deps must error")
	}

	afterRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = ?`, "theories")
	afterCount := 0
	if err := afterRow.Scan(&afterCount); err != nil {
		t.Fatalf("count theories after: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("rejected deps must not create a theory; before=%d, after=%d",
			beforeCount, afterCount)
	}
}
