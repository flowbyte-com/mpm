// p2_citation_mcp_test.go — the MCP-surface proof for P2.
//
// WHY THIS FILE EXISTS.
//
// These tests send a real `tools/call` through the PRODUCTION MCP server
// and read the result back, which is what an MCP client does. The
// in-process tool tests (internal/core/tools/p2_citation_tool_test.go)
// call the handler directly, so they never exercise the MCP adapter, the
// output policy, or the tool filter.
//
// WHAT THIS FILE DOES NOT CLAIM.
//
// It does NOT claim that the MCP server validates arguments against the
// published schema. It does not: mcp-go's input-schema validation is
// OPT-IN via `server.WithInputSchemaValidation()`, which
// `buildMCPServerCore()` does not set, and `mcp.NewToolWithRawSchema`
// (the form this server registers with) bypasses the structured-schema
// helpers entirely. There is no JSON-schema validation anywhere in MPM's
// own Go code.
//
// That is precisely why P2's validation lives in the CORE
// (`ProposeTheoryWithCitations`), not in the schema: a core-level check is
// transport-independent, so it holds for the MCP surface, the `mpm call`
// CLI path and every other caller alike. The registry schema's role is
// DISCOVERY — it is what `mpm_help show mpm_theories` publishes to an
// agent, and what an MCP client receives as the tool's input schema.
//
// The registry declaration is pinned separately, where its effect actually
// is: internal/core/tools/p2_citation_tool_test.go
// (TestP2Tool_RegistryDeclaresSourceType) and
// TestP2Tool_HelpPublishesSourceType.
//
// ISOLATION. The DM is the cmd/mpm-mcp package's sanctioned hermetic
// fixture (newIsolatedTestDM), which routes through t.TempDir() and pins an
// isolated workspace, so nothing reaches the operator's live database.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/flowbyte-com/mpm/internal/blobstore"
	"github.com/mark3labs/mcp-go/server"
)

// p2MCPCall sends one `tools/call` request through the MCP server and
// returns the raw JSON-RPC response.
func p2MCPCall(t *testing.T, s *server.MCPServer, tool string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": tool, "arguments": args},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp := s.HandleMessage(context.Background(), raw)
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return out
}

// p2MCPResult extracts the JSON payload a tool returned in its text content
// block.
func p2MCPResult(t *testing.T, resp map[string]interface{}) map[string]interface{} {
	t.Helper()
	if e, hasErr := resp["error"]; hasErr {
		t.Fatalf("tools/call returned a protocol error: %v", e)
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("no result in the response: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call reported an error: %v", result)
	}
	content, ok := result["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("no content blocks in the result: %v", result)
	}
	block, _ := content[0].(map[string]interface{})
	text, _ := block["text"].(string)
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		// A non-JSON text block is a legitimate failure envelope for a
		// rejected call; hand it back verbatim.
		return map[string]interface{}{"_raw": text}
	}
	return payload
}

// p2BuildMCPServer builds the PRODUCTION MCP server (the same constructor
// main() uses) against a disposable workspace, so the published schemas and
// the tool filter are exactly what a client sees.
func p2BuildMCPServer(t *testing.T, dm *core.DatabaseManager) *server.MCPServer {
	t.Helper()
	workspace := os.Getenv("MPM_WORKSPACE")
	if workspace == "" {
		workspace = t.TempDir()
	}
	for _, subdir := range []string{"mode", "persona", "blobs"} {
		if err := os.MkdirAll(filepath.Join(workspace, subdir), 0o700); err != nil {
			t.Fatalf("bootstrap %s/: %v", subdir, err)
		}
	}
	router, err := core.NewRouter(workspace)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	blobStore, err := blobstore.NewFilesystemBackend(
		dm.SQLDB(), filepath.Join(workspace, "blobs"), 24*time.Hour)
	if err != nil {
		t.Fatalf("build blob store: %v", err)
	}
	return buildMCPServer(dm, core.ActiveContext{}, router, blobStore, tools.DefaultOutputPolicy())
}

// TestP2MCP_SourceTypeIsAcceptedOverMCPToolsCall is the MCP-surface proof.
//
// It sends the call the way a client does, through the production server
// and the production adapter, and asserts the CORE's typed path ran: the
// citation is recorded with the declared source type and the cited event
// id.
//
// It is non-vacuous with respect to the CORE's routing: if the handler
// ignored `source_type` and fell back to the legacy path, the recorded
// event_id would be the DOWNSTREAM id rather than the cited id, and this
// test would fail. (The source_type assertion alone no longer
// discriminates, since D3 made the legacy auto-resolution name what is
// actually cited — the event_id is the legacy path's fingerprint.) It is
// NOT a proof about the registry schema — see the file header.
func TestP2MCP_SourceTypeIsAcceptedOverMCPToolsCall(t *testing.T) {
	// The initial MCP surface is three tools; the full surface is behind
	// MPM_EXPOSE_ALL_TOOLS. mpm_theories is a specialist.
	t.Setenv("MPM_EXPOSE_ALL_TOOLS", "1")

	dm := newIsolatedTestDM(t)
	invocationID := "p2-mcp-invocation-0001"
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		   (id, session_id, tool_name, action, invocation_id, actor_kind,
		    payload_hash, result_status, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		invocationID, "p2-mcp-session", "mpm_memory", "save", "p2-mcp-inv",
		"agent", "sha256:p2", "success", 1793000000); err != nil {
		t.Fatalf("seed tool_invocations row: %v", err)
	}

	s := p2BuildMCPServer(t, dm)

	resp := p2MCPCall(t, s, "mpm_theories", map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":  "an MCP-surface typed citation",
			"source_ids":  []interface{}{invocationID},
			"source_type": "tool_invocation",
		},
	})
	payload := p2MCPResult(t, resp)
	proposalID, _ := payload["id"].(string)
	if proposalID == "" {
		t.Fatalf("the MCP call did not return a proposal id: %v", payload)
	}

	var sourceType, eventID string
	if err := dm.SQLDB().QueryRow(
		`SELECT source_type, event_id FROM epistemic_provenance WHERE downstream_id = ?`,
		proposalID).Scan(&sourceType, &eventID); err != nil {
		t.Fatalf("query epistemic_provenance: %v", err)
	}
	if sourceType != "tool_invocation" {
		t.Errorf("expected source_type %q over the MCP surface, got %q",
			"tool_invocation", sourceType)
	}
	if eventID != invocationID {
		t.Errorf("expected event_id to be the cited id %q, got %q",
			invocationID, eventID)
	}
}

// TestP2MCP_UnresolvableTypedCitationIsRejectedOverMCP pins the rejection
// on the wire: the MCP call must fail and leave no proposal.
func TestP2MCP_UnresolvableTypedCitationIsRejectedOverMCP(t *testing.T) {
	t.Setenv("MPM_EXPOSE_ALL_TOOLS", "1")

	dm := newIsolatedTestDM(t)
	s := p2BuildMCPServer(t, dm)

	var before int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&before); err != nil {
		t.Fatalf("count theories: %v", err)
	}

	resp := p2MCPCall(t, s, "mpm_theories", map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":  "an MCP-surface citation of something absent",
			"source_ids":  []interface{}{"p2-mcp-missing-invocation"},
			"source_type": "tool_invocation",
		},
	})
	blob, _ := json.Marshal(resp)
	if !strings.Contains(string(blob), "does not resolve") {
		t.Errorf("expected the MCP call to report that the citation does not "+
			"resolve, got: %s", string(blob))
	}

	var after int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&after); err != nil {
		t.Fatalf("count theories: %v", err)
	}
	if after != before {
		t.Errorf("expected no proposal to survive the rejection, count went %d -> %d",
			before, after)
	}
}

// TestP2MCP_LegacyCitationIsUnchangedOverMCP pins backward compatibility
// on the wire: a propose with no source_type still records the historical
// auto-resolved type, with the historical event_id.
//
// D3 changed ONE thing about the auto-resolution: it now finds the two
// canonical artifact kinds the historical resolver could not see — a work
// item and a tool invocation — so the recorded source_type names what is
// actually cited instead of falling back to "memory". The compatibility
// guarantee this test pins ("callers that do not supply source_type must
// be unaffected") is about the CALLER's experience — the call succeeds,
// the citation is recorded, nothing is rejected — and that is unchanged.
//
// The event_id assertion is what keeps this test non-vacuous: on the TYPED
// path the event_id is the CITED id, so a handler that routed a legacy
// call to the typed path would fail here.
func TestP2MCP_LegacyCitationIsUnchangedOverMCP(t *testing.T) {
	t.Setenv("MPM_EXPOSE_ALL_TOOLS", "1")

	dm := newIsolatedTestDM(t)
	invocationID := "p2-mcp-legacy-invocation-0001"
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		   (id, session_id, tool_name, action, invocation_id, actor_kind,
		    payload_hash, result_status, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		invocationID, "p2-mcp-session", "mpm_memory", "save", "p2-mcp-legacy",
		"agent", "sha256:p2", "success", 1793000000); err != nil {
		t.Fatalf("seed tool_invocations row: %v", err)
	}

	s := p2BuildMCPServer(t, dm)

	resp := p2MCPCall(t, s, "mpm_theories", map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis": "an MCP-surface legacy citation",
			"source_ids": []interface{}{invocationID},
		},
	})
	payload := p2MCPResult(t, resp)
	proposalID, _ := payload["id"].(string)
	if proposalID == "" {
		t.Fatalf("the MCP call did not return a proposal id: %v", payload)
	}

	var sourceType, eventID string
	if err := dm.SQLDB().QueryRow(
		`SELECT source_type, event_id FROM epistemic_provenance WHERE downstream_id = ?`,
		proposalID).Scan(&sourceType, &eventID); err != nil {
		t.Fatalf("query epistemic_provenance: %v", err)
	}
	// D3: the auto-resolution now names what is actually cited. Before
	// the correction this was "memory" — a mislabel for a tool
	// invocation.
	if sourceType != "tool_invocation" {
		t.Errorf("expected the legacy path's auto-resolved type %q over the MCP "+
			"surface, got %q — a citation must record the type of what it actually cites",
			"tool_invocation", sourceType)
	}
	// The legacy overloading: the event_id is the DOWNSTREAM id.
	if eventID != proposalID {
		t.Errorf("expected the legacy path's event_id to be the DOWNSTREAM id %q over "+
			"the MCP surface, got %q", proposalID, eventID)
	}
}
