package internal

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSigmoid(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0.5},
		{math.Inf(1), 1.0},
		{math.Inf(-1), 0.0},
		{1, 1.0 / (1.0 + math.Exp(-1))},
	}
	for _, tc := range cases {
		got := sigmoid(tc.in)
		assert.InDelta(t, tc.want, got, 1e-9)
	}
}

func TestEffectiveEvidence(t *testing.T) {
	got := effectiveEvidence(0.4, 1.0, 0.5) // strength × independence × recency
	assert.InDelta(t, 0.2, got, 1e-9)
}

func TestRecencyWeight_Decays(t *testing.T) {
	w0 := recencyWeight(0, 0.005)
	w30 := recencyWeight(30, 0.005)
	assert.InDelta(t, 1.0, w0, 1e-9)
	assert.Less(t, w30, w0, "recency weight should decrease with age")
	assert.Greater(t, w30, 0.0)
}

func TestDecay_IncreasesWithTime(t *testing.T) {
	lambda := decayLambda("theories") // 0.02
	d0 := decay(lambda, 0)
	d35 := decay(lambda, 35)
	assert.InDelta(t, 0, d0, 1e-9)
	assert.InDelta(t, lambda*35, d35, 1e-9)
}

func TestInitialConfidence_ByArtifactType(t *testing.T) {
	cases := map[string]float64{
		"memory":   0.8,
		"theory":   0.5,
		"decision": 0.6,
		"lesson":   0.7,
	}
	for artifact, want := range cases {
		t.Run(artifact, func(t *testing.T) {
			got := InitialConfidence(artifact)
			assert.InDelta(t, want, got, 1e-9)
		})
	}
}

func TestInitialConfidence_UnknownTypeDefaultsToPointFive(t *testing.T) {
	got := InitialConfidence("not_a_real_type")
	assert.InDelta(t, 0.5, got, 1e-9)
}

func TestDecayLambda_ByArtifactType(t *testing.T) {
	// Per spec: decisions 0.001, lessons 0.003, memories 0.01, theories 0.02
	cases := map[string]float64{
		"decision": 0.001,
		"lesson":   0.003,
		"memory":   0.01,
		"theory":   0.02,
	}
	for typ, want := range cases {
		t.Run(typ, func(t *testing.T) {
			got := decayLambda(typ)
			assert.InDelta(t, want, got, 1e-9)
		})
	}
}

func TestComputeConfidence_NoEvidenceReturnsInitial(t *testing.T) {
	// No evidence rows → confidence equals the initial value.
	got := computeConfidence("memory", nil, time.Now(), time.Now(), 0.005)
	assert.InDelta(t, 0.8, got, 1e-9)
}

func TestComputeConfidence_PositiveEvidenceRaises(t *testing.T) {
	now := time.Now()
	ev := []evidenceInput{
		{Strength: 0.7, Independence: 1.0, CreatedAt: now},
	}
	got := computeConfidence("memory", ev, now, now, 0.005)
	assert.Greater(t, got, 0.8, "positive evidence should raise confidence above initial 0.8")
}

func TestComputeConfidence_NegativeEvidenceLowers(t *testing.T) {
	now := time.Now()
	ev := []evidenceInput{
		{Strength: -0.6, Independence: 1.0, CreatedAt: now},
	}
	got := computeConfidence("memory", ev, now, now, 0.005)
	assert.Less(t, got, 0.8, "challenge evidence should lower confidence below initial 0.8")
}

func TestComputeConfidence_DecayOverTime(t *testing.T) {
	// Strong evidence now, no evidence later → decay should bring confidence down.
	now := time.Now()
	later := now.Add(200 * 24 * time.Hour) // 200 days
	ev := []evidenceInput{
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
	}
	nowConf := computeConfidence("memory", ev, now, now, 0.005)
	laterConf := computeConfidence("memory", ev, later, now, 0.005)
	assert.Less(t, laterConf, nowConf, "decay should lower confidence over time")
}

func TestComputeConfidence_BoundedBetweenZeroAndOne(t *testing.T) {
	now := time.Now()
	ev := []evidenceInput{
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
	}
	got := computeConfidence("memory", ev, now, now, 0.005)
	assert.Greater(t, got, 0.0)
	assert.Less(t, got, 1.0)
}

func TestArtifactTypeFromCollection(t *testing.T) {
	cases := map[string]string{
		"memories":  "memory",
		"theories":  "theory",
		"decisions": "decision",
		"lessons":   "lesson",
	}
	for coll, want := range cases {
		t.Run(coll, func(t *testing.T) {
			assert.Equal(t, want, artifactTypeFromCollection(coll))
		})
	}
	require.Equal(t, "memory", artifactTypeFromCollection("unknown_collection")) // default
}
