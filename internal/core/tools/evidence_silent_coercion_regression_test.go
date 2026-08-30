// evidence_silent_coercion_regression_test.go — alpha-4.1.1 audit follow-up.
//
// Bug surfaced by the silent-coercion review (HIGH severity, finding #1):
// handleAddEvidence silently dropped string / wrong-type values for
// `strength` and `independence_factor`. An agent passing `strength:"5"`
// would get an evidence row with strength=0 and no error — the audit
// trail recorded the wrong weight.
//
// Fix mirrors parseWeightStrict's contract: numeric shapes pass through,
// nil returns the documented default, anything else (string, bool,
// object, array) errors with the offending type and the field name.
// Implemented via the new parseFloatStrictOr helper so the error message
// names the field that was malformed.
//
// Pre-fix behavior: silent 0 / 1.0 defaults. The regression below pins
// the post-fix error contract.
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestEvidenceStrict_RejectsStringStrength pins the headline fix:
// string "5" must error, not silently coerce to 0.
func TestEvidenceStrict_RejectsStringStrength(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleAddEvidence(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"artifact_id":   "mem-test-1",
		"artifact_type": "memory",
		"type":          "test",
		"source_group":  "test",
		"created_by":    "test-agent",
		"strength":      "5", // string — must error
	})
	if err == nil {
		t.Fatalf("handleAddEvidence accepted string strength — silent-coercion regression")
	}
	if !strings.Contains(err.Error(), "strength") {
		t.Errorf("error message must name the malformed field `strength`, got: %v", err)
	}
	if !strings.Contains(err.Error(), "must be a number") {
		t.Errorf("error message must explain the type requirement, got: %v", err)
	}
}

// TestEvidenceStrict_RejectsStringIndependence pins the same contract
// for the second numeric field. Independence factor is the canonical
// 1.0 default — wrong-type input still gets the explicit error rather
// than silently absorbing the default.
func TestEvidenceStrict_RejectsStringIndependence(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleAddEvidence(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"artifact_id":         "mem-test-2",
		"artifact_type":       "memory",
		"type":                "test",
		"source_group":        "test",
		"created_by":          "test-agent",
		"strength":            0.5,
		"independence_factor": "1.0", // string — must error
	})
	if err == nil {
		t.Fatalf("handleAddEvidence accepted string independence_factor — silent-coercion regression")
	}
	if !strings.Contains(err.Error(), "independence_factor") {
		t.Errorf("error message must name the malformed field, got: %v", err)
	}
}

// TestEvidenceStrict_RejectsBoolAndObject pins the broader wrong-type
// contract: bool, map, array all error. Pre-fix they would silently
// coerce to 0 / 1.0.
//
// These three cases hit the strict-parser path before any DB call, so
// they don't need a seeded artifact — the error surfaces at parse time.
func TestEvidenceStrict_RejectsBoolAndObject(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
	}{
		{"bool", true},
		{"map", map[string]interface{}{"x": 1}},
		{"array", []interface{}{1, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestIsolatedDM(t)

			_, err := handleAddEvidence(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"artifact_id":   "mem-test-" + tc.name,
				"artifact_type": "memory",
				"type":          "test",
				"source_group":  "test",
				"created_by":    "test-agent",
				"strength":      tc.value,
			})
			if err == nil {
				t.Fatalf("handleAddEvidence accepted %T strength — silent-coercion regression", tc.value)
			}
		})
	}
}

// TestEvidenceStrict_ParserReturnsBeforeDBWrite pins the strict-parser
// path's contract: a wrong-type numeric input is rejected before any DB
// validation runs. This is what makes the negative tests above
// (RejectsStringStrength, RejectsStringIndependence, RejectsBoolAndObject)
// safe — the parser is the first thing that runs after payload ingress.
//
// We exercise the parser indirectly: a wrong-type strength with a
// deliberately malformed artifact_id (no row exists) should still fail
// at the parser, not at the DB layer. If a future refactor moves the
// DB lookup before the parse, the assertion order changes — this test
// surfaces that.
func TestEvidenceStrict_ParserReturnsBeforeDBWrite(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleAddEvidence(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"artifact_id":   "this-memory-does-not-exist",
		"artifact_type": "memory",
		"type":          "test",
		"source_group":  "test",
		"created_by":    "test-agent",
		"strength":      "not-a-number", // parser error must beat DB error
	})
	if err == nil {
		t.Fatalf("handleAddEvidence accepted string strength on a missing artifact")
	}
	if !strings.Contains(err.Error(), "strength") {
		t.Errorf("error must be the parser error (names `strength`), got: %v", err)
	}
	if strings.Contains(err.Error(), "does not exist") {
		t.Errorf("DB-layer error leaked through before parser error — ordering regression: %v", err)
	}
}

// TestEvidenceStrict_PositivePathSeedsArtifact is the only test in this
// file that reaches the DB write path. It seeds a memory row first so
// handleAddEvidence can complete the round-trip with a numeric strength.
// The negative cases above are sufficient for the parser contract;
// this one pins that numeric shapes still actually write through.
func TestEvidenceStrict_PositivePathSeedsArtifact(t *testing.T) {
	dm := newTestIsolatedDM(t)

	memID := "mem-evidence-positive"
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'evidence strict test')`,
		memID,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	res, err := handleAddEvidence(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"artifact_id":   memID,
		"artifact_type": "memory",
		"type":          "test",
		"source_group":  "test",
		"created_by":    "test-agent",
		"strength":      float64(0.75),
	})
	if err != nil {
		t.Fatalf("positive numeric path rejected: %v", err)
	}
	_ = res // row was written; confidence recompute is verified elsewhere
}
