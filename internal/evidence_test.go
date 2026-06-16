package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvidenceTypes_AllHaveStrengths checks that every type registered in
// the v1 evidence type registry has a strength within the valid [-1, 1] range.
func TestEvidenceTypes_AllHaveStrengths(t *testing.T) {
	for _, name := range AllEvidenceTypes() {
		t.Run(name, func(t *testing.T) {
			strength, ok := DefaultStrength(name)
			require.True(t, ok, "DefaultStrength should find %q in registry", name)
			assert.GreaterOrEqual(t, strength, -1.0, "strength must be >= -1")
			assert.LessOrEqual(t, strength, 1.0, "strength must be <= 1")
		})
	}
}

// TestEvidenceTypes_StrengthsMatchSpec pins the v1 default strengths exactly
// to the values in the spec (2026-06-16-confidence-evidence-foundation-design.md).
// If a strength changes intentionally, this test forces a visible update.
func TestEvidenceTypes_StrengthsMatchSpec(t *testing.T) {
	expected := map[string]float64{
		"observation":       0.4,
		"test":              0.7,
		"reproduction":      0.85,
		"challenge":         -0.6,
		"decision_outcome":  0.95,
		"external_reference": 0.6,
	}
	require.Equal(t, len(expected), len(AllEvidenceTypes()),
		"registry size changed; update the expected map and spec")

	for name, want := range expected {
		got, ok := DefaultStrength(name)
		require.True(t, ok, "type %q missing from registry", name)
		assert.InDelta(t, want, got, 1e-9, "strength for %q drifted from spec", name)
	}
}

// TestEvidenceTypes_UnknownReturnsFalse verifies the lookup of an unknown
// evidence type returns ok=false rather than (e.g.) silently returning 0.
func TestEvidenceTypes_UnknownReturnsFalse(t *testing.T) {
	strength, ok := DefaultStrength("not_a_real_type")
	assert.False(t, ok, "unknown type must return ok=false")
	assert.Equal(t, 0.0, strength, "strength for unknown type is zero value")
}

// TestEvidenceTypes_AllReturnsSixV1 pins the count of v1 evidence types.
// Adding a v2 type is fine but requires a deliberate change here.
func TestEvidenceTypes_AllReturnsSixV1(t *testing.T) {
	types := AllEvidenceTypes()
	require.Len(t, types, 6, "v1 evidence registry must contain exactly 6 types")

	// No duplicates.
	seen := make(map[string]struct{}, len(types))
	for _, name := range types {
		_, dup := seen[name]
		assert.False(t, dup, "duplicate type name in registry: %q", name)
		seen[name] = struct{}{}
	}
}

// TestEvidenceTypes_IsValid checks the boolean helper against both a known
// v1 type and a deliberately fake name.
func TestEvidenceTypes_IsValid(t *testing.T) {
	assert.True(t, IsValidEvidenceType("observation"), "observation is a v1 type")
	assert.False(t, IsValidEvidenceType("nope"), "nope is not a registered type")
	assert.False(t, IsValidEvidenceType(""), "empty string is not a valid type")

	// Every type returned by AllEvidenceTypes() must validate.
	for _, name := range AllEvidenceTypes() {
		assert.True(t, IsValidEvidenceType(name), "registry entry %q must be valid", name)
	}
}
