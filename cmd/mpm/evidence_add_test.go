package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceAdd_ParsesArgs(t *testing.T) {
	// Construct args. The exact flag set is up to the implementer; this test
	// pins one reasonable shape.
	//   mpm evidence add --artifact <id> --type observation --source "log-x" --strength 0.4 --by "test"
	args := []string{
		"--artifact", "mem-1",
		"--type", "observation",
		"--source", "log-server-01",
		"--strength", "0.4",
		"--by", "test",
	}
	payload, err := parseEvidenceAddArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "mem-1", payload["artifact_id"])
	assert.Equal(t, "observation", payload["type"])
	assert.Equal(t, "log-server-01", payload["source_group"])
	assert.InDelta(t, 0.4, payload["strength"].(float64), 1e-9)
	assert.Equal(t, "test", payload["created_by"])
}

func TestEvidenceAdd_RejectsInvalidType(t *testing.T) {
	args := []string{
		"--artifact", "mem-1",
		"--type", "not_a_real_type",
		"--source", "x",
		"--by", "test",
	}
	_, err := parseEvidenceAddArgs(args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid evidence type")
}

func TestEvidenceAdd_DefaultsStrengthFromRegistry(t *testing.T) {
	// If --strength is omitted, the registry default for the type is used.
	args := []string{
		"--artifact", "mem-1",
		"--type", "reproduction",
		"--source", "x",
		"--by", "test",
	}
	payload, err := parseEvidenceAddArgs(args)
	require.NoError(t, err)
	assert.InDelta(t, 0.85, payload["strength"].(float64), 1e-9)
}

func TestEvidenceAdd_SerializesToJSON(t *testing.T) {
	payload := map[string]interface{}{
		"artifact_id":   "mem-1",
		"artifact_type": "memory",
		"type":          "test",
		"source_group":  "x",
		"strength":      0.7,
		"created_by":    "test",
	}
	out, err := json.Marshal(payload)
	require.NoError(t, err)
	s := string(out)
	assert.True(t, strings.Contains(s, `"artifact_id":"mem-1"`), "expected %s", s)
}
