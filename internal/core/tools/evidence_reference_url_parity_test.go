// evidence_reference_url_parity_test.go — schema↔handler parity for
// mpm_evidence's reference_url.
//
// WHY THIS FILE EXISTS INSTEAD OF RELYING ON schema_guard_test.go:
//
// The generic guard (schema_guard_test.go) enforces bidirectional
// schema↔handler parity, but it CANNOT see this field:
//   - extractSchemaProperties reads only the TOP-LEVEL `properties` object,
//     so it sees `action` and `params` but never `params.properties.reference_url`.
//   - handleAddEvidence is an aggregator dispatch target (called from
//     handleMpmEvidence with a `params` identifier), and the guard skips
//     aggregator targets on the under-declaration pass because `params`
//     sets additionalProperties:true.
//
// So neither direction of the generic guard fires whether or not
// reference_url is declared and read. That is exactly the shape of the
// historical "lying contract" that reference_url's predecessor source_url
// had — a property advertised in the schema that nothing honoured, removed
// precisely because nothing caught it. Without this test, the same class of
// drift could recur here undetected.
//
// If the generic guard is ever extended to descend into params.properties
// and to check aggregator targets, this file becomes redundant — check
// before deleting it.

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findTool returns the Registry entry with the given name.
func findTool(t *testing.T, name string) Tool {
	t.Helper()
	for _, tool := range Registry {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q is not in the Registry", name)
	return Tool{}
}

// TestMpmEvidenceSchema_DeclaresReferenceURL pins the declaration half: an
// agent reading the schema must be told the field exists, and told what it
// does and does not mean.
func TestMpmEvidenceSchema_DeclaresReferenceURL(t *testing.T) {
	tool := findTool(t, "mpm_evidence")

	var schema map[string]interface{}
	require.NoError(t, json.Unmarshal(tool.Schema, &schema))

	props, ok := schema["properties"].(map[string]interface{})
	require.True(t, ok, "mpm_evidence schema should have top-level properties")

	params, ok := props["params"].(map[string]interface{})
	require.True(t, ok, "mpm_evidence schema should declare a params object")

	paramProps, ok := params["properties"].(map[string]interface{})
	require.True(t, ok, "mpm_evidence params should declare its own properties")

	refProp, ok := paramProps["reference_url"]
	require.True(t, ok,
		"mpm_evidence params must declare reference_url; the schema is the only "+
			"documentation an agent reads for this field")

	decl, ok := refProp.(map[string]interface{})
	require.True(t, ok, "reference_url should be declared as an object with a type")
	assert.Equal(t, "string", decl["type"])

	// The description is the agent-facing contract. It must state the
	// non-fetch guarantee explicitly, because an agent that assumes MPM
	// validated reachability will make claims MPM cannot support.
	desc, _ := decl["description"].(string)
	require.NotEmpty(t, desc, "reference_url needs a description; agents read it")
	for _, phrase := range []string{"http", "https", "not", "fetch"} {
		assert.Contains(t, strings.ToLower(desc), phrase,
			"the description should mention %q", phrase)
	}
}

// TestMpmEvidenceHandler_ReadsReferenceURL pins the implementation half: the
// handler must actually consume the declared field.
//
// The generic guard skips aggregator dispatch targets, so this assertion is
// the only thing standing between a declared-but-ignored property and a
// second lying contract.
func TestMpmEvidenceHandler_ReadsReferenceURL(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("handlers.go"))
	require.NoError(t, err)

	const fn = "func handleAddEvidence("
	start := strings.Index(string(src), fn)
	require.NotEqual(t, -1, start, "handleAddEvidence should exist in handlers.go")
	body := string(src)[start:]
	if next := strings.Index(body, "\nfunc "); next > 0 {
		body = body[:next]
	}

	assert.Contains(t, body, `getString(payload, "reference_url")`,
		"handleAddEvidence must read reference_url from the payload; a schema "+
			"property the handler ignores is the lying-contract failure mode")
}

// TestMpmEvidenceHandler_ReferenceURLRoundTrip exercises the whole MCP path
// end to end: a declared property, read by the handler, validated, persisted,
// and returned by the corresponding list action.
//
// Parity tests that only assert on source text cannot catch a handler that
// reads the key and then drops the value. This one runs the code.
func TestMpmEvidenceHandler_ReferenceURLRoundTrip(t *testing.T) {
	dm := internal.NewTestDM(t)

	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "refurl-mcp-1")
	require.NoError(t, err)

	const ref = "https://Example.COM/spec?b=2&a=1#Section-4"

	_, err = handleAddEvidence(dm, internal.ActiveContext{}, map[string]interface{}{
		"artifact_id":   "refurl-mcp-1",
		"artifact_type": "memory",
		"type":          "external_reference",
		"source_group":  "external",
		"created_by":    "tester",
		"reference_url": ref,
	})
	require.NoError(t, err)

	var got string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT reference_url FROM evidence WHERE artifact_id = 'refurl-mcp-1'`).Scan(&got))
	assert.Equal(t, ref, got, "the MCP handler must persist the reference verbatim")

	res, err := dm.ListEvidence("refurl-mcp-1", "memory")
	require.NoError(t, err)
	rows, ok := res["evidence"].([]map[string]interface{})
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, ref, rows[0]["reference_url"],
		"the MCP list action must return the reference the add action stored")
}

// TestMpmEvidenceHandler_ReferenceURLValidationRejects pins that the MCP
// surface enforces the same contract as the CLI. An agent must not be able to
// write a rejected URL just because it came in over a different transport.
func TestMpmEvidenceHandler_ReferenceURLValidationRejects(t *testing.T) {
	dm := internal.NewTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "refurl-mcp-bad")
	require.NoError(t, err)

	for _, bad := range []string{"ftp://example.com/x", "example.com/no-scheme", "https://"} {
		_, err := handleAddEvidence(dm, internal.ActiveContext{}, map[string]interface{}{
			"artifact_id":   "refurl-mcp-bad",
			"artifact_type": "memory",
			"type":          "observation",
			"source_group":  "filesystem",
			"created_by":    "tester",
			"reference_url": bad,
		})
		require.Error(t, err, "%q must be rejected on the MCP surface too", bad)
	}

	var count int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = 'refurl-mcp-bad'`).Scan(&count))
	assert.Equal(t, 0, count, "a rejected URL must leave no partial row")
}

// TestMpmEvidenceHandler_ReferenceURLOptional pins backward compatibility on
// the MCP surface: omitting the key must behave exactly as it did before the
// field existed.
func TestMpmEvidenceHandler_ReferenceURLOptional(t *testing.T) {
	dm := internal.NewTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "refurl-mcp-none")
	require.NoError(t, err)

	_, err = handleAddEvidence(dm, internal.ActiveContext{}, map[string]interface{}{
		"artifact_id":   "refurl-mcp-none",
		"artifact_type": "memory",
		"type":          "observation",
		"source_group":  "filesystem",
		"created_by":    "tester",
	})
	require.NoError(t, err, "omitting reference_url must remain valid")

	res, err := dm.ListEvidence("refurl-mcp-none", "memory")
	require.NoError(t, err)
	rows, ok := res["evidence"].([]map[string]interface{})
	require.True(t, ok)
	require.Len(t, rows, 1)

	// Always present as a key so clients can branch on it unconditionally;
	// empty string when absent, mirroring how `notes` is shaped.
	v, present := rows[0]["reference_url"]
	require.True(t, present, "the row map should always carry a reference_url key")
	assert.Equal(t, "", v)
}
