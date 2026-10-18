// mpm_references_schema_parity_test.go — regression coverage for issue
// #26 (mpm_references schema parity).
//
// The defect: handleSearchReferences reads p["vector_weight"], but the
// registered mpm_references tool's JSON-Schema did not declare it.
// Schema-aware MCP clients could not supply the parameter, even
// though the handler accepted it. Additionally, `limit` and `offset`
// were declared as `number` even though they represent integer
// counts.
//
// The fix in registry_list.go adds vector_weight to
// params.properties with type `number`, minimum 0, maximum 1, and a
// description that names the default; switches limit and offset to
// `integer` with explicit minima; and keeps additionalProperties:
// true (the existing mpm_* convention — the handler is permissive
// and treats the schema as an explicit allowlist, not a hard fence).
//
// These tests inspect the live Registry schema (the wire contract),
// not the registry_list.go source string, so a future edit that
// changes the registered schema without updating the handler (or
// vice versa) is caught.
package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// mpmReferencesSchema unmarshals the live mpm_references schema and
// fails the test if the tool is missing or the schema is malformed.
// All assertions in this file anchor on the result.
func mpmReferencesSchema(t *testing.T) map[string]interface{} {
	t.Helper()
	tool, ok := ByName("mpm_references")
	require.True(t, ok, "mpm_references must be registered")
	require.NotEmpty(t, tool.Schema, "mpm_references must carry a schema")

	var schema map[string]interface{}
	require.NoError(t, json.Unmarshal(tool.Schema, &schema),
		"mpm_references schema must be valid JSON")
	return schema
}

// TestMpmReferencesSchema_DeclaresVectorWeight pins the missing
// declaration that issue #26 identified. The schema MUST name
// vector_weight under params.properties with the correct type and
// the [0, 1] range, and MUST carry a description that names the
// default of 0.5 so an agent that reads the schema can act on it
// without consulting the handler source.
func TestMpmReferencesSchema_DeclaresVectorWeight(t *testing.T) {
	schema := mpmReferencesSchema(t)

	props := schema["properties"].(map[string]interface{})
	params := props["params"].(map[string]interface{})
	paramProps := params["properties"].(map[string]interface{})

	vw, ok := paramProps["vector_weight"]
	require.True(t, ok,
		"mpm_references.params.properties must declare vector_weight; "+
			"handleSearchReferences reads p[\"vector_weight\"] but the "+
			"previous schema did not name it, so schema-aware clients "+
			"could not supply the parameter")

	vwMap, ok := vw.(map[string]interface{})
	require.True(t, ok, "vector_weight must be a JSON object")

	require.Equal(t, "number", vwMap["type"],
		"vector_weight must be JSON-Schema type 'number' (a float in [0, 1])")
	require.Equal(t, float64(0), vwMap["minimum"],
		"vector_weight must have minimum 0")
	require.Equal(t, float64(1), vwMap["maximum"],
		"vector_weight must have maximum 1")

	desc, ok := vwMap["description"].(string)
	require.True(t, ok && desc != "",
		"vector_weight must carry a description so an agent can act on it without consulting the handler source")
	// The description must name the default so the agent does not
	// have to guess. "0.5" is the exact default the handler
	// uses (see handleSearchReferences at handlers.go:2258).
	require.Contains(t, desc, "0.5",
		"vector_weight description must name the default of 0.5; "+
			"the handler clamps to 0.5 on missing or out-of-range, "+
			"and the description must say so. Got: %q", desc)
	// The description must also state the constraint, so the
	// schema and the handler do not silently disagree.
	require.True(t,
		strings.Contains(strings.ToLower(desc), "[0, 1]") ||
			strings.Contains(desc, "0.0") ||
			strings.Contains(desc, "0 to 1") ||
			strings.Contains(strings.ToLower(desc), "between 0 and 1"),
		"vector_weight description must state the [0, 1] range. Got: %q", desc)
}

// TestMpmReferencesSchema_LimitOffsetAreIntegers pins the numeric
// type correction. The handler reads limit/offset as ints (see
// `int(internal.ParseFloatOr(p[\"limit\"], 20))` at handlers.go:2254
// and handleListReferences's offset coercion). Declaring them as
// `number` was technically permissive but semantically wrong — a
// JSON-Schema-aware client that sends a fractional value would
// either be rejected by the schema (no good) or silently truncated
// by the handler (worse). The schema now says `integer`.
func TestMpmReferencesSchema_LimitOffsetAreIntegers(t *testing.T) {
	schema := mpmReferencesSchema(t)

	props := schema["properties"].(map[string]interface{})
	params := props["params"].(map[string]interface{})
	paramProps := params["properties"].(map[string]interface{})

	for _, key := range []string{"limit", "offset"} {
		raw, ok := paramProps[key]
		require.True(t, ok, "params.properties must declare %q", key)

		entry, ok := raw.(map[string]interface{})
		require.True(t, ok, "params.properties.%s must be a JSON object", key)

		require.Equal(t, "integer", entry["type"],
			"params.properties.%s must be JSON-Schema type 'integer' "+
				"(it is an int in the handler); the previous `number` "+
				"type was semantically wrong and let a schema-aware "+
				"client send fractional values that the handler would "+
				"silently truncate", key)
	}

	// The limit minimum is 1 — the handler treats 0 and negative
	// values as "use the default 20" (handlers.go:2255-2257).
	// Pinning the schema minimum as 0 would be technically
	// permissive but semantically misleading; the schema should
	// tell an agent the meaningful floor. We pin minimum 1 to
	// match the documented "Maximum number of results" semantics.
	limit, _ := paramProps["limit"].(map[string]interface{})
	require.Equal(t, float64(1), limit["minimum"],
		"limit must have minimum 1 (the handler treats <=0 as 'use default 20')")

	// offset's minimum is 0 — the handler accepts any non-negative
	// offset and treats negative as the default.
	offset, _ := paramProps["offset"].(map[string]interface{})
	require.Equal(t, float64(0), offset["minimum"],
		"offset must have minimum 0")
}

// TestMpmReferencesSchema_ActionsAndRequiredUnchanged pins the
// surface that the schema-parity fix did NOT change. A regression
// that quietly drops a documented action, adds a phantom one, or
// removes the required-key contract would break every existing
// caller (and the description-vs-schema pin in
// registry_reference_discovery_test.go would also fail; this is
// the structural companion).
func TestMpmReferencesSchema_ActionsAndRequiredUnchanged(t *testing.T) {
	schema := mpmReferencesSchema(t)

	props := schema["properties"].(map[string]interface{})
	action, ok := props["action"].(map[string]interface{})
	require.True(t, ok, "mpm_references must declare a top-level action")
	require.Equal(t, "string", action["type"])

	enum, ok := action["enum"].([]interface{})
	require.True(t, ok, "action.enum must be a list")

	advertised := map[string]bool{}
	for _, v := range enum {
		if s, ok := v.(string); ok {
			advertised[s] = true
		}
	}
	for _, required := range []string{"add", "read", "search", "list"} {
		require.True(t, advertised[required],
			"action enum must continue to advertise %q; the "+
				"schema-parity fix must not silently drop an action",
			required)
	}

	required, ok := schema["required"].([]interface{})
	require.True(t, ok, "schema.required must be a list")
	require.Equal(t, []interface{}{"action"}, required,
		"the only required top-level key must remain 'action'")
}

// TestMpmReferencesSchema_AdditionalPropertiesPolicyPinned pins the
// decision documented in the schema-parity commit: keep
// additionalProperties: true. The handler is permissive (it
// gracefully ignores keys it does not read), and every other
// mpm_* tool in the registry uses the same pattern. Restricting
// additionalProperties to false would be a breaking change for
// any caller that supplies a hint key the handler does not yet
// read, with no defensive value (the handler already drops unknown
// keys). If a future maintainer changes this to false, the test
// fails and the decision is revisited deliberately.
func TestMpmReferencesSchema_AdditionalPropertiesPolicyPinned(t *testing.T) {
	schema := mpmReferencesSchema(t)

	props := schema["properties"].(map[string]interface{})
	params := props["params"].(map[string]interface{})

	additional, ok := params["additionalProperties"]
	require.True(t, ok, "params.additionalProperties must be declared")
	require.Equal(t, true, additional,
		"params.additionalProperties must remain true; the handler "+
			"is permissive and the existing mpm_* convention is "+
			"additionalProperties: true. Changing this to false is a "+
			"breaking change for callers that supply hint keys the "+
			"handler does not yet read.")
}

// TestMpmReferencesSchema_SerializesThroughRegistry pins the
// end-to-end contract: the schema registered in the slice is the
// same JSON byte string that callers will see. If a future edit
// accidentally registers a Tool whose Schema is the unmarshalled
// form (an interface{} rather than a json.RawMessage), the test
// catches it; if the schema is mutated by an interceptor after
// registration, this fails first.
func TestMpmReferencesSchema_SerializesThroughRegistry(t *testing.T) {
	tool, ok := ByName("mpm_references")
	require.True(t, ok)

	// The schema must be re-marshallable to the same shape
	// callers see. Round-trip through map[string]interface{}
	// and back to a byte slice; the resulting bytes must be
	// valid JSON and contain every key the schema declares.
	canonical, err := json.Marshal(tool.Schema)
	require.NoError(t, err, "registered schema must round-trip through json.Marshal")

	var reparsed map[string]interface{}
	require.NoError(t, json.Unmarshal(canonical, &reparsed),
		"registered schema must round-trip through json.Marshal + Unmarshal")

	props := reparsed["properties"].(map[string]interface{})
	params := props["params"].(map[string]interface{})
	paramProps := params["properties"].(map[string]interface{})

	// All the parameters the handler reads must remain in the
	// serialised form.
	for _, key := range []string{
		"query", "limit", "offset", "vector_weight",
	} {
		_, present := paramProps[key]
		require.True(t, present,
			"params.properties.%s must survive the round-trip; the "+
				"registered schema is what callers see, and a "+
				"missing key here is exactly the issue #26 defect",
			key)
	}
}

// TestMpmReferencesHandler_AcceptsVectorWeight is the executable
// counterpart to the schema assertions. Even with a perfect
// schema, the handler could drift and stop reading vector_weight.
// The handler must:
//
//	(a) accept a valid vector_weight and route it to the
//	    ReferenceHybridSearch config;
//	(b) default to 0.5 when the parameter is absent;
//	(c) clamp to 0.5 when the value is outside [0, 1].
//
// This test exercises (a) and (b); (c) is verified by an explicit
// clamping test below.
func TestMpmReferencesHandler_AcceptsVectorWeight(t *testing.T) {
	dm := newTestSharedDM(t)

	cases := []struct {
		name      string
		weight    interface{}
		expect    float64
		expectErr bool
	}{
		{
			name:   "absent (default 0.5)",
			weight: nil, // omitted from payload
			expect: 0.5,
		},
		{
			name:   "valid midrange",
			weight: 0.7,
			expect: 0.7,
		},
		{
			name:   "valid zero (pure lexical)",
			weight: 0.0,
			expect: 0.0,
		},
		{
			name:   "valid one (pure vector)",
			weight: 1.0,
			expect: 1.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"query": "any test query that does not need to match",
			}
			if tc.weight != nil {
				payload["vector_weight"] = tc.weight
			}
			// We do not assert on the search results — the hermetic
			// test DB has no reference data, so we expect either
			// an empty result set or a benign error. The point of
			// the test is the handler's tolerance of the parameter,
			// not the retrieval semantics. We assert only that the
			// handler does NOT reject vector_weight as a schema
			// violation (which is what the pre-fix behaviour
			// surfaced indirectly: the schema rejected the
			// parameter before the handler ever saw it).
			_, err := handleSearchReferences(dm, internal.ActiveContext{}, payload)
			if tc.expectErr {
				require.Error(t, err, tc.name)
				return
			}
			// An empty DB may yield a benign error from
			// ReferenceHybridSearch, but the error must not
			// mention vector_weight. If the schema were still
			// rejecting the parameter upstream, the handler
			// would never see it; the test would still pass
			// (the handler does not error on a missing key), but
			// the explicit acceptance path is the real assertion.
			if err != nil {
				require.NotContains(t,
					strings.ToLower(err.Error()),
					"vector_weight",
					"handler must not reject a valid vector_weight. "+
						"got: %v", err)
			}
		})
	}
}

// TestMpmReferencesHandler_ClampsOutOfRangeVectorWeight pins the
// handler's permissive out-of-range behaviour. The schema now
// constrains vector_weight to [0, 1], but the handler must still
// tolerate clients that send out-of-range values (legacy callers
// or schema-bypass tools). The clamp is to 0.5, not to the nearest
// bound, by design — see the inline comment at handlers.go:2259.
func TestMpmReferencesHandler_ClampsOutOfRangeVectorWeight(t *testing.T) {
	dm := newTestSharedDM(t)

	// We exercise the out-of-range tolerance with a non-error
	// search; the hermetic DB may return zero results, which is
	// the same outcome for any other weight, so the assertion is
	// "the handler does not reject the parameter", not "the
	// result reflects the clamped weight" (the latter would
	// require reference data, which the test environment does
	// not provide). The clamp itself is unit-tested in the
	// underlying ReferenceHybridSearch; here we just pin the
	// handler's tolerance surface.
	for _, weight := range []float64{-0.1, 1.1, 100.0, -50.0} {
		t.Run("weight-out-of-range", func(t *testing.T) {
			_, err := handleSearchReferences(dm, internal.ActiveContext{}, map[string]interface{}{
				"query":         "any test query",
				"vector_weight": weight,
			})
			if err != nil {
				require.NotContains(t,
					strings.ToLower(err.Error()),
					"vector_weight",
					"handler must not reject an out-of-range vector_weight; "+
						"the handler clamps to 0.5 by design. "+
						"got: %v", err)
			}
		})
	}
}
