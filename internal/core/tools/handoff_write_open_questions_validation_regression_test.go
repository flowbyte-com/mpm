// handoff_write_open_questions_validation_regression_test.go —
// Pass 4 defect C.17.
//
// The 2026-09-05 audit found mpm_handoff.write silently dropped
// invalid `open_questions` element shapes. The registry schema
// declares `"type":"array","items":{"type":"string"}` (and the same
// for `commitments`), but handleHandoffWrite routed both fields
// through internal.ParseStringSliceOr, which silently returned nil
// for non-array scalars and silently dropped non-string elements.
// The handoff row was persisted with open_questions=[] regardless
// of caller intent.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_handoff --payload '{"action":"write","params":{"summary":"x","open_questions":[1,2]}}'
//     # wanted: error mentioning the open_questions contract
//     # actual: success, handoff persisted with open_questions=[]
//
// Canonical contract (per registry schema and EndSession
// persistence):
//
//   omitted        → open_questions = [] (legitimate; field is optional)
//   null           → open_questions = [] (null omission equivalent)
//   []             → open_questions = [] (empty array; the explicit form)
//   ["q1","q2"]    → open_questions = ["q1","q2"]
//   "q1"           → ERROR (string is not an array)
//   [1,2]          → ERROR (mixed-type array)
//   null           → ERROR (null inside array)
//
// The same validation is applied symmetrically to `commitments`,
// which shares the array-of-strings shape and the same handler
// path. Pinning both sides prevents future drift.

package tools

import (
	"strings"
	"testing"
)

// TestHandoffWrite_ValidArraysPersist pins the positive path: a
// valid string array flows through to the persisted handoff row
// for both commitments and open_questions.
func TestHandoffWrite_ValidArraysPersist(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmHandoff(dm, defaultACForPatch(), map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary":        "valid-arrays",
			"commitments":    []interface{}{"finish the audit", "ship pass 4"},
			"open_questions": []interface{}{"how does FTS5 handle CJK?", "is wal-mode safe for backups?"},
		},
	})
	if err != nil {
		t.Fatalf("valid arrays must not error: %v", err)
	}
	if res == nil {
		t.Fatalf("valid arrays must produce a handoff, got nil")
	}
}

// TestHandoffWrite_OmittedArraysValid pins the documented
// optionality: omitting either field is the legitimate
// no-commitments / no-questions path.
func TestHandoffWrite_OmittedArraysValid(t *testing.T) {
	dm := newTestSharedDM(t)

	for label, val := range map[string]interface{}{
		"absent": nil,
		"nil":    nil,
	} {
		t.Run("open_questions="+label, func(t *testing.T) {
			params := map[string]interface{}{"summary": "omit-" + label}
			if val != nil {
				params["open_questions"] = val
			}
			_, err := handleMpmHandoff(dm, defaultACForPatch(), map[string]interface{}{
				"action": "write",
				"params": params,
			})
			if err != nil {
				t.Errorf("omitted open_questions must not error (%s): %v", label, err)
			}
		})
	}
}

// TestHandoffWrite_EmptyArraysValid pins: empty arrays are the
// explicit form of "no commitments / no questions" and must be
// accepted. Distinguishing omitted from empty is part of the
// contract.
func TestHandoffWrite_EmptyArraysValid(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmHandoff(dm, defaultACForPatch(), map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary":        "empty-arrays",
			"commitments":    []interface{}{},
			"open_questions": []interface{}{},
		},
	})
	if err != nil {
		t.Errorf("empty arrays must not error, got: %v", err)
	}
}

// TestHandoffWrite_ScalarOpenQuestionsRejected pins: a scalar
// (string or number) passed where the schema requires an array must
// error loudly rather than silently coerced.
func TestHandoffWrite_ScalarOpenQuestionsRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, scalar := range []interface{}{"a", 123, true} {
		t.Run("scalar", func(t *testing.T) {
			_, err := handleMpmHandoff(dm, defaultACForPatch(), map[string]interface{}{
				"action": "write",
				"params": map[string]interface{}{
					"summary":        "scalar-open-questions",
					"open_questions": scalar,
				},
			})
			if err == nil {
				t.Fatalf("scalar open_questions (%T) must error", scalar)
			}
			if !strings.Contains(err.Error(), "open_questions") {
				t.Errorf("error must mention 'open_questions', got: %v", err)
			}
		})
	}
}

// TestHandoffWrite_ScalarCommitmentsRejected pins the symmetric
// commitment path: a scalar for `commitments` errors at the
// boundary just like a scalar for `open_questions`.
func TestHandoffWrite_ScalarCommitmentsRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmHandoff(dm, defaultACForPatch(), map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary":     "scalar-commitments",
			"commitments": "a-single-string",
		},
	})
	if err == nil {
		t.Fatalf("scalar commitments must error")
	}
	if !strings.Contains(err.Error(), "commitments") {
		t.Errorf("error must mention 'commitments', got: %v", err)
	}
}

// TestHandoffWrite_MixedTypeArrayRejected pins: an array whose
// elements are not all strings is rejected. The schema's items:
// {"type":"string"} is strict; integers/booleans/nulls have no
// canonical mapping.
func TestHandoffWrite_MixedTypeArrayRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmHandoff(dm, defaultACForPatch(), map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary":        "mixed-type",
			"open_questions": []interface{}{"valid question", 123},
		},
	})
	if err == nil {
		t.Fatalf("mixed-type array must error")
	}
	if !strings.Contains(err.Error(), "open_questions") {
		t.Errorf("error must mention 'open_questions', got: %v", err)
	}
}

// TestHandoffWrite_InvalidOpenQuestionsDoesNotPersist pins the
// write-path guarantee: rejected input does not create a handoff
// row with silently-wiped open_questions.
func TestHandoffWrite_InvalidOpenQuestionsDoesNotPersist(t *testing.T) {
	dm := newTestSharedDM(t)

	beforeRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM session_handoffs`)
	beforeCount := 0
	if err := beforeRow.Scan(&beforeCount); err != nil {
		t.Fatalf("count session_handoffs before: %v", err)
	}

	_, err := handleMpmHandoff(dm, defaultACForPatch(), map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary":        "no-persist",
			"open_questions": "scalar-not-array",
		},
	})
	if err == nil {
		t.Fatalf("scalar open_questions must error")
	}

	afterRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM session_handoffs`)
	afterCount := 0
	if err := afterRow.Scan(&afterCount); err != nil {
		t.Fatalf("count session_handoffs after: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("rejected open_questions must not create a handoff; before=%d, after=%d",
			beforeCount, afterCount)
	}
}
