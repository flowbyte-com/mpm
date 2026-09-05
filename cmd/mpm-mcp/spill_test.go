// spill_test.go — Additional integration tests for MCP spill path.
// New tests added: buildSpillPreview, spill envelope bounds.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildSpillPreview is tested directly here (package-level, no mock needed).
func TestBuildSpillPreview_FirstKeysLimit(t *testing.T) {
	result := map[string]interface{}{}
	for i := 0; i < 10; i++ {
		result[fmt.Sprintf("key%d", i)] = i
	}
	jsonBytes, err := json.Marshal(result)
	require.NoError(t, err)

	preview := buildSpillPreview(jsonBytes)
	keys, ok := preview["first_keys"].([]string)
	require.True(t, ok)
	// Exactly 5, not "at most 5": a regression that returned zero keys would
	// satisfy LessOrEqual and hide the bug.
	assert.Equal(t, 5, len(keys), "first_keys must be capped at exactly 5")
	assert.Equal(t, "json", preview["kind"])
}

func TestBuildSpillPreview_ApproxItems(t *testing.T) {
	// Array result.
	arrBytes, err := json.Marshal([]int{1, 2, 3, 4, 5})
	require.NoError(t, err)
	p := buildSpillPreview(arrBytes)
	assert.Equal(t, 5, p["approx_items"])

	// Map result.
	mapBytes, err := json.Marshal(map[string]int{"a": 1, "b": 2})
	require.NoError(t, err)
	p = buildSpillPreview(mapBytes)
	assert.Equal(t, 2, p["approx_items"])

	// String result (primitive).
	strBytes, err := json.Marshal("hello")
	require.NoError(t, err)
	p = buildSpillPreview(strBytes)
	assert.Equal(t, 0, p["approx_items"])
}

func TestBuildSpillPreview_MalformedJSON(t *testing.T) {
	preview := buildSpillPreview([]byte("not json"))
	assert.Equal(t, "unknown", preview["kind"])
	assert.Equal(t, 0, preview["approx_items"])
}

func TestSpillEnvelope_Bounded(t *testing.T) {
	// A large result produces an envelope much smaller than the MCP result ceiling.
	// This is structural: the envelope contains pointers and metadata, not payload.
	//
	// The payload is a repeating recognizable marker, NOT zero bytes: the leak
	// assertion at the bottom is only meaningful if the payload contains
	// something that would actually show up in the marshalled envelope.
	const marker = "SPILLMARKER"
	largeResult := map[string]interface{}{
		"data": strings.Repeat(marker, 4096), // ~45 KB, comfortably over threshold
	}
	jsonBytes, err := json.Marshal(largeResult)
	require.NoError(t, err)

	// Confirm the fixture is genuinely on the spill side of the boundary —
	// otherwise this test would silently be exercising the inline-return path.
	policy := tools.DefaultOutputPolicy()
	decision, n, err := policy.Apply(context.Background(), jsonBytes)
	require.NoError(t, err)
	require.Equal(t, tools.DecisionSpill, decision,
		"fixture must exceed the spill threshold (got %d bytes)", n)

	preview := buildSpillPreview(jsonBytes)
	envelope := map[string]interface{}{
		"status":       "spilled",
		"pointer":      "mpm://blob/test-id",
		"size_bytes":   int64(len(jsonBytes)),
		"content_type": "application/json",
		"source_tool":  "test",
		"preview":      preview,
		"expires_at":   time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	envBytes, err := json.Marshal(envelope)
	require.NoError(t, err)

	// The envelope must land back on the pass side of the same policy.
	envDecision, _, err := policy.Apply(context.Background(), envBytes)
	require.NoError(t, err)
	assert.Equal(t, tools.DecisionPass, envDecision,
		"spill envelope must itself fit under the threshold")

	// The payload must not appear in the envelope.
	assert.NotContains(t, string(envBytes), marker)
}

