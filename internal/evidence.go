package internal

// evidenceType is the shape of a single v1 evidence type entry.
type evidenceType struct {
	Name     string
	Strength float64
}

// evidenceTypeRegistry is the single source of truth for v1 evidence types
// and their default strengths. The order of entries in this slice defines
// the order returned by AllEvidenceTypes().
//
// Spec: docs/superpowers/specs/2026-06-16-confidence-evidence-foundation-design.md
//
// Strengths are starting points; callers can override per-evidence.
// The signs are critical: `challenge` has negative strength, modeling
// "evidence against this artifact."
var evidenceTypeRegistry = []evidenceType{
	{"observation", 0.4},
	{"test", 0.7},
	{"reproduction", 0.85},
	{"challenge", -0.6},
	{"decision_outcome", 0.95},
	{"external_reference", 0.6},
}

// AllEvidenceTypes returns the v1 evidence type names in registry order.
func AllEvidenceTypes() []string {
	out := make([]string, len(evidenceTypeRegistry))
	for i, entry := range evidenceTypeRegistry {
		out[i] = entry.Name
	}
	return out
}

// IsValidEvidenceType reports whether name is a known v1 evidence type.
func IsValidEvidenceType(name string) bool {
	_, ok := lookupEvidenceType(name)
	return ok
}

// DefaultStrength returns the registry default strength for the given
// evidence type. ok is false if name is not a known v1 type; strength
// is zero in that case.
func DefaultStrength(name string) (float64, bool) {
	entry, ok := lookupEvidenceType(name)
	if !ok {
		return 0, false
	}
	return entry.Strength, true
}

// lookupEvidenceType is an internal helper that scans the registry.
// Linear scan is fine for the v1 size (six entries) and keeps the
// registry as a single source of truth.
func lookupEvidenceType(name string) (evidenceType, bool) {
	for _, entry := range evidenceTypeRegistry {
		if entry.Name == name {
			return entry, true
		}
	}
	return evidenceType{}, false
}
