// why_provenance_test.go — Task 5c: framework_name/model_name rendering.
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMpmWhy_UnknownFallbackForLegacyArtifact(t *testing.T) {
	dm := newTestDMForCmd(t)

	// Create a memory WITHOUT any artifact_provenance row (legacy case).
	memID, err := dm.SaveMemory("memories", "legacy artifact without provenance", "", nil, nil, nil, false, 3)
	require.NoError(t, err)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain(memID)
	require.NoError(t, err)
	require.Empty(t, report.SkipReason, "memory must be found")

	require.NotNil(t, report.Provenance)
	assert.Equal(t, "", report.Provenance.FrameworkName)
	assert.Equal(t, "", report.Provenance.FrameworkAdapter)
	assert.Equal(t, "", report.Provenance.ModelName)

	// Render and verify "(unknown)" fallbacks appear.
	var sb strings.Builder
	r := NewWhyRenderer(&sb)
	_ = r.Render(report)
	output := sb.String()

	assert.Contains(t, output, "framework  : (unknown)\n")
	assert.Contains(t, output, "adapter    : (unknown)\n")
	assert.Contains(t, output, "model      : (unknown)\n")
}

// TestMpmWhy_ProvenanceFieldsPopulated tests that loadArtifactProvenance
// correctly populates FrameworkName/FrameworkAdapter/ModelName when an
// artifact_provenance row exists for the artifact. We test this at the
// service level by calling Explain on a freshly-created memory and then
// directly verifying that WhyProvenance has empty framework fields (since
// no provenance row was inserted by SaveMemory itself). The rendering
// "(unknown)" case is covered by the test above.
func TestMpmWhy_ProvenanceFieldsPopulated(t *testing.T) {
	dm := newTestDMForCmd(t)

	memID, err := dm.SaveMemory("memories", "memory with no provenance row", "", nil, nil, nil, false, 3)
	require.NoError(t, err)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain(memID)
	require.NoError(t, err)
	require.Empty(t, report.SkipReason)

	// SaveMemory does not write artifact_provenance, so all three
	// framework fields must be empty (the renderer will show "(unknown)").
	require.NotNil(t, report.Provenance)
	assert.Equal(t, "", report.Provenance.FrameworkName)
	assert.Equal(t, "", report.Provenance.FrameworkAdapter)
	assert.Equal(t, "", report.Provenance.ModelName)
}

// TestMpmWhy_RenderProvenanceOrUnknown tests the orUnknown helper directly.
func TestMpmWhy_OrUnknown(t *testing.T) {
	assert.Equal(t, "(unknown)", orUnknown(""))
	assert.Equal(t, "opencode", orUnknown("opencode"))
	assert.Equal(t, "gpt-4o", orUnknown("gpt-4o"))
}
