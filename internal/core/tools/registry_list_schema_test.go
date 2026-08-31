// registry_list_schema_test.go — L-5 audit regression coverage
// (post-M3, 2026-08-31).
//
// Pins the JSON Schema enum fixes for the affected tools:
//   - mpm_work: enum must include "resolve_contradiction" (was missing).
//   - mpm_evidence: enum must include "source_groups" (was missing).
//
// The fix is surgical: add the missing enum value to the schema so
// MCP clients (which validate against the schema before sending)
// accept the action. Without the fix, validation rejects the action
// even though the handler supports it.
//
// The test parses each Schema as a generic map[string]interface{} and
// walks the `action.enum` array. If the enum is missing the canonical
// action, the test fails.
package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSchemaActions_ContainResolvedActions pins the L-5 fixes: every
// action that a tool's handler dispatches must also be advertised in
// the JSON Schema enum so MCP validation does not falsely reject it.
//
// Audit L-5 (post-M3, 2026-08-31) found two specific enum gaps:
//   - mpm_work missing "resolve_contradiction"
//   - mpm_evidence missing "source_groups"
//
// Both have been corrected in registry_list.go. This test is a
// regression guard: if a future change drops one of these enum values,
// the test fails and the gap is surfaced before it ships.
func TestSchemaActions_ContainResolvedActions(t *testing.T) {
	cases := []struct {
		tool           string
		requiredAction string
	}{
		{"mpm_work", "resolve_contradiction"},
		{"mpm_evidence", "source_groups"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+"_has_"+tc.requiredAction, func(t *testing.T) {
			tool, ok := ByName(tc.tool)
			require.True(t, ok, "tool %q must be registered", tc.tool)

			var schema map[string]interface{}
			require.NoError(t, json.Unmarshal(tool.Schema, &schema),
				"tool %q schema must be valid JSON", tc.tool)

			props, ok := schema["properties"].(map[string]interface{})
			require.True(t, ok, "tool %q schema.properties must be an object", tc.tool)

			action, ok := props["action"].(map[string]interface{})
			require.True(t, ok, "tool %q schema.properties.action must be an object", tc.tool)

			enum, ok := action["enum"].([]interface{})
			require.True(t, ok, "tool %q schema.properties.action.enum must be an array", tc.tool)

			found := false
			for _, e := range enum {
				if s, ok := e.(string); ok && s == tc.requiredAction {
					found = true
					break
				}
			}
			require.True(t, found,
				"tool %q schema enum must include %q (audit L-5); current enum: [%s]",
				tc.tool, tc.requiredAction, enumStrings(enum))
		})
	}
}

// enumStrings formats an enum array as a comma-separated list for
// readable failure messages.
func enumStrings(enum []interface{}) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		if s, ok := e.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}
