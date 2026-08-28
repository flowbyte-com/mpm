package internal

import "fmt"

// evidenceType is the shape of a single v1 evidence type entry.
type evidenceType struct {
	Name     string
	Strength float64
}

// evidenceTypeRegistry is the single source of truth for v1 evidence types
// and their default strengths. The order of entries in this slice defines
// the order returned by AllEvidenceTypes().
//
// Spec: docs/archive/2026-06-16-confidence-evidence-foundation-design.md
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

// AllEvidenceTypes returns a snapshot of the v1 evidence-type registry,
// including the default strength for each entry. Callers — chiefly CLI
// help printers — use this to render discoverable documentation so the
// registry remains the single source of truth (RECOMMENDED 7).
func AllEvidenceTypes() []evidenceType {
	out := make([]evidenceType, len(evidenceTypeRegistry))
	copy(out, evidenceTypeRegistry)
	return out
}

// EvidenceTypeHelp renders a human-readable summary of the v1 evidence
// types, including their default strength and a short semantic gloss.
// The string is suitable for `mpm help evidence` / `mpm evidence --help`
// / CLI usage strings without further formatting.
func EvidenceTypeHelp() string {
	glosses := map[string]string{
		"observation":        "agent observation (default strength too low to verify alone; needs corroboration)",
		"test":               "designated verifier — passing test result (sufficient to verify on its own)",
		"reproduction":       "designated verifier — independent reproduction of the outcome",
		"challenge":          "negative evidence — flags the artifact as contradicted; moves verified → contradicted",
		"decision_outcome":   "designated verifier — observed downstream outcome of a decision",
		"external_reference": "pointer to an external document / API response (moderate weight)",
	}
	out := "v1 evidence types (single source of truth: internal/core/evidence.go):\n"
	for _, e := range AllEvidenceTypes() {
		gloss, ok := glosses[e.Name]
		if !ok {
			gloss = "(no description)"
		}
		out += fmt.Sprintf("  %-20s default strength %+.2f   %s\n", e.Name, e.Strength, gloss)
	}
	return out
}
