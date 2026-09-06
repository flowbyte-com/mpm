// cmd/mpm/d_evidence_default_contract_test.go
//
// Final-pass debt-closure test: re-verify the evidence default
// strength contract that S7 established.
//
// Authoritative defaults (internal/core/evidence.go:20-27):
//
//   observation         0.40
//   test                0.70
//   reproduction        0.85
//   challenge          -0.60
//   decision_outcome    0.95
//   external_reference  0.60
//
// Three-way distinction that S7 nailed down:
//
//   field absent           → registry default
//   field present, zero    → registry default (substrate's
//                              "if Strength == 0, fill from registry"
//                              contract — pre-D.5 the tool handler
//                              pre-coerced nil to 0.5 and bypassed
//                              this contract; the S7 fix restored it)
//   field present, non-zero → honour the value as-is
//
// Tests below pin all six registry defaults and the three-way
// distinction. A regression in handleAddEvidence that re-introduces
// the pre-S7 pre-coercion bug would fail at least one of these
// tests.

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	internal "github.com/flowbyte-com/mpm-core"
)

func s7EvidenceDefault(t *testing.T, dm *internal.DatabaseManager, evidenceType string) float64 {
	t.Helper()
	memID := "ev-def-" + evidenceType
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, tags, metadata, weight, confidence, expires_at, created_at, updated_at)
		 VALUES (?, 'memories', ?, '[]', '{}', 5, 0.8, 3153600000, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))`,
		memID, "evidence default test",
	)
	require.NoError(t, err)

	_, err = runHandler(dm, "mpm_evidence", map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{
			"artifact_id":   memID,
			"artifact_type": "memory",
			"type":          evidenceType,
			"source_group":  "test",
			"created_by":    "test",
		},
	})
	require.NoError(t, err, "tool add for %s", evidenceType)
	got := s6EvidenceStrength(t, dm, memID)
	return got
}

func TestD_EvidenceDefault_AllSixRegistryDefaults(t *testing.T) {
	cases := []struct {
		evidenceType string
		want         float64
	}{
		{"observation", 0.40},
		{"test", 0.70},
		{"reproduction", 0.85},
		{"challenge", -0.60},
		{"decision_outcome", 0.95},
		{"external_reference", 0.60},
	}
	for _, c := range cases {
		c := c
		t.Run(c.evidenceType, func(t *testing.T) {
			dm := internal.NewTestDM(t)
			got := s7EvidenceDefault(t, dm, c.evidenceType)
			assert.InDelta(t, c.want, got, 1e-9, "default strength for %s", c.evidenceType)
		})
	}
}

func TestD_EvidenceDefault_ThreeWayDistinction(t *testing.T) {
	dm := internal.NewTestDM(t)
	seedMemory := func(id string) {
		_, err := dm.SQLDB().Exec(
			`INSERT INTO memories (id, collection, content, tags, metadata, weight, confidence, expires_at, created_at, updated_at)
			 VALUES (?, 'memories', ?, '[]', '{}', 5, 0.8, 3153600000, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))`,
			id, "three-way test",
		)
		require.NoError(t, err)
	}

	// Case 1: field absent → registry default applies (0.85 for reproduction).
	seedMemory("ev-absent")
	_, err := runHandler(dm, "mpm_evidence", map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{
			"artifact_id":   "ev-absent",
			"artifact_type": "memory",
			"type":          "reproduction",
			"source_group":  "test",
			"created_by":    "test",
		},
	})
	require.NoError(t, err)
	assert.InDelta(t, 0.85, s6EvidenceStrength(t, dm, "ev-absent"), 1e-9, "absent → registry default")

	// Case 2: field present, zero → registry default applies (substrate
	// contract: "if in.Strength == 0, fill from registry"). The S7
	// fix preserves this.
	seedMemory("ev-zero")
	_, err = runHandler(dm, "mpm_evidence", map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{
			"artifact_id":   "ev-zero",
			"artifact_type": "memory",
			"type":          "reproduction",
			"source_group":  "test",
			"created_by":    "test",
			"strength":      0.0,
		},
	})
	require.NoError(t, err)
	assert.InDelta(t, 0.85, s6EvidenceStrength(t, dm, "ev-zero"), 1e-9, "explicit 0 → registry default")

	// Case 3: field present, non-zero → honour the value.
	seedMemory("ev-explicit")
	_, err = runHandler(dm, "mpm_evidence", map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{
			"artifact_id":   "ev-explicit",
			"artifact_type": "memory",
			"type":          "reproduction",
			"source_group":  "test",
			"created_by":    "test",
			"strength":      0.42,
		},
	})
	require.NoError(t, err)
	assert.InDelta(t, 0.42, s6EvidenceStrength(t, dm, "ev-explicit"), 1e-9, "explicit non-zero → as supplied")
}
