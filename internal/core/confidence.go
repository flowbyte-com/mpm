// confidence.go — Pure-Go confidence math.
//
// No DB access. All functions are deterministic given their inputs; time is
// always passed in explicitly so the package is testable. The DB-touching
// wrapper in evidence_store.go is responsible for loading evidence rows and
// calling ComputeFromEvidence(artifactType, evidence, now, lastPositiveAt).
package internal

import (
	"math"
	"time"
)

// InitialConfidence returns the per-artifact-type starting confidence.
// See "Initial Confidence by Type" in the spec.
func InitialConfidence(artifactType string) float64 {
	switch artifactType {
	case "memory":
		return 0.8
	case "theory":
		return 0.5
	case "decision":
		return 0.6
	case "lesson":
		return 0.7
	default:
		return 0.5
	}
}

// artifactTypeFromCollection maps a `memories.collection` discriminator to the
// artifact type used by the evidence system.
func artifactTypeFromCollection(collection string) string {
	switch collection {
	case "memories", "":
		return "memory"
	case "theories":
		return "theory"
	case "decisions":
		return "decision"
	case "lessons":
		return "lesson"
	default:
		return "memory"
	}
}

// decayLambda returns the per-artifact-type exponential decay rate (per day).
// See "Exponential confidence decay with per-collection λ" in the spec.
// Per spec: decisions 0.001, lessons 0.003, memories 0.01, theories 0.02.
func decayLambda(artifactType string) float64 {
	switch artifactType {
	case "decision":
		return 0.001
	case "lesson":
		return 0.003
	case "memory", "":
		return 0.01
	case "theory":
		return 0.02
	default:
		return 0.01
	}
}

// recencyWeight returns e^(-μ × ageDays) for evidence of ageDays. The global
// μ is small (~0.005) so individual evidence rows decay slowly; the
// per-collection decay on the artifact is the dominant factor.
func recencyWeight(ageDays, mu float64) float64 {
	return math.Exp(-mu * ageDays)
}

// decay returns λ × t — the linear-in-time decay term subtracted from log-odds.
// The exponential shape comes from this being inside a sigmoid, not from any
// time-multiplier on decay itself.
func decay(lambda, tDays float64) float64 {
	return lambda * tDays
}

// effectiveEvidence is the per-row contribution to log-odds:
// strength × independence × recency.
func effectiveEvidence(strength, independence, recency float64) float64 {
	return strength * independence * recency
}

// sigmoid maps log-odds to (0, 1). Defined separately so the call site is
// readable.
func sigmoid(logOdds float64) float64 {
	return 1.0 / (1.0 + math.Exp(-logOdds))
}

// evidenceInput is the DB-decoupled view of an evidence row used by the math.
// The DB wrapper constructs these from sql.Rows.
type evidenceInput struct {
	Strength    float64
	Independence float64
	CreatedAt   time.Time
}

// computeConfidence is the core formula from the spec:
//
//	confidence = sigmoid( log(initialOdds) + Σ effective_evidence - decay )
//
// where initialOdds = initial / (1 - initial), recency is e^(-μ × ageDays),
// and decay = λ × tDaysSinceLastPositiveEvidence.
func computeConfidence(artifactType string, ev []evidenceInput, now, lastPositiveAt time.Time, mu float64) float64 {
	initial := InitialConfidence(artifactType)
	logInitialOdds := math.Log(initial / (1 - initial))

	logOdds := logInitialOdds
	for _, e := range ev {
		ageDays := now.Sub(e.CreatedAt).Hours() / 24.0
		logOdds += effectiveEvidence(e.Strength, e.Independence, recencyWeight(ageDays, mu))
	}

	tDays := now.Sub(lastPositiveAt).Hours() / 24.0
	if tDays < 0 {
		tDays = 0
	}
	// decayLambda is keyed on the artifact type (the spec uses collection, but
	// in this codebase collection ↔ artifact type for the four known types).
	logOdds -= decay(decayLambda(artifactType), tDays)

	return sigmoid(logOdds)
}
