// p2_citation_tool_test.go — tool-surface coverage for P2.
//
// P2 adds ONE optional parameter to the `mpm_theories` propose action:
// `source_type`. This file pins the tool-surface contract:
//
//   - the registry declares the parameter, with the canonical vocabulary as
//     its enum, so the MCP server (which publishes the registry schema
//     verbatim via mcp.NewToolWithRawSchema) accepts it;
//   - the handler routes to the TYPED path when the parameter is present
//     and to the LEGACY path when it is absent;
//   - a typed citation that does not resolve is rejected at the tool
//     boundary, with no proposal left behind; and
//   - the A-9 correction: the `mpm_evidence` artifact_type enum no longer
//     advertises a value the database's CHECK constraint rejects.
//
// ISOLATION. The tools module's sanctioned hermetic fixture
// (internal.NewTestSharedDM) is used; it routes through t.TempDir() and
// pins an isolated workspace, so nothing reaches the operator's live
// database.

package tools

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// p2SeedToolInvocation inserts a minimal tool_invocations row and returns
// its id.
func p2SeedToolInvocation(t *testing.T, dm *internal.DatabaseManager, id string) string {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		   (id, session_id, tool_name, action, invocation_id, actor_kind,
		    payload_hash, result_status, started_at, completed_at, duration_ms,
		    framework_name, mpm_session_id, framework_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "p2-tool-session", "mpm_memory", "save", "inv-"+id, "agent",
		"sha256:"+id, "success", 1793000000, 1793000000, 1,
		"opencode", "p2-mpm-session", "p2-framework-session")
	if err != nil {
		t.Fatalf("seed tool_invocations row %q: %v", id, err)
	}
	return id
}

// p2ProposeSchema returns the `propose` branch of the mpm_theories params
// schema.
func p2ProposeSchema(t *testing.T) map[string]interface{} {
	t.Helper()
	tool, ok := ByName("mpm_theories")
	if !ok {
		t.Fatal("mpm_theories not found in Registry")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("parse mpm_theories schema: %v", err)
	}
	params, ok := schema["properties"].(map[string]interface{})["params"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_theories schema has no params object")
	}
	branches, ok := params["oneOf"].([]interface{})
	if !ok || len(branches) == 0 {
		t.Fatal("mpm_theories params schema has no oneOf branches")
	}
	for _, b := range branches {
		branch, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		props, ok := branch["properties"].(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := props["hypothesis"]; has {
			return branch
		}
	}
	t.Fatal("mpm_theories schema has no propose branch")
	return nil
}

// TestP2Tool_RegistryDeclaresSourceType pins the registry contract.
//
// The registry schema IS the MCP tool's published input schema, so the
// parameter must be declared there — with the canonical vocabulary as its
// enum, so a caller cannot supply a type the core will reject.
func TestP2Tool_RegistryDeclaresSourceType(t *testing.T) {
	branch := p2ProposeSchema(t)
	props, ok := branch["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("propose branch has no properties")
	}
	declared, ok := props["source_type"].(map[string]interface{})
	if !ok {
		t.Fatal("the propose branch does not declare source_type — nothing " +
			"publishes the parameter to an agent, so it would never be " +
			"discoverable")
	}
	if declared["type"] != "string" {
		t.Errorf("expected source_type to be a string, got %v", declared["type"])
	}
	enum, ok := declared["enum"].([]interface{})
	if !ok {
		t.Fatalf("source_type has no enum: %v", declared)
	}
	got := map[string]bool{}
	for _, e := range enum {
		s, _ := e.(string)
		got[s] = true
	}
	for _, want := range internal.CitationSourceTypes() {
		if !got[want] {
			t.Errorf("source_type enum is missing %q — the registry must offer "+
				"exactly the canonical vocabulary", want)
		}
	}
	if len(got) != len(internal.CitationSourceTypes()) {
		t.Errorf("source_type enum has %d entries but the canonical vocabulary "+
			"has %d — the two must not drift", len(got), len(internal.CitationSourceTypes()))
	}
}

// TestP2Tool_HandleProposeWithSourceTypeIsTyped pins that the handler
// routes to the typed path when the parameter is present: the citation is
// recorded with the DECLARED type, not the "memory" fallback.
func TestP2Tool_HandleProposeWithSourceTypeIsTyped(t *testing.T) {
	dm := newTestSharedDM(t)
	invocationID := p2SeedToolInvocation(t, dm, "p2-tool-typed-invocation")

	res, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":  "a typed proposal through the tool surface",
			"source_ids":  []interface{}{invocationID},
			"source_type": "tool_invocation",
		},
	})
	if err != nil {
		t.Fatalf("typed propose must not error: %v", err)
	}
	result, _ := res.(map[string]interface{})
	proposalID, _ := result["id"].(string)
	if proposalID == "" {
		t.Fatalf("no proposal id returned: %v", res)
	}

	var sourceType string
	if err := dm.SQLDB().QueryRow(
		`SELECT source_type FROM epistemic_provenance WHERE downstream_id = ?`,
		proposalID).Scan(&sourceType); err != nil {
		t.Fatalf("query epistemic_provenance: %v", err)
	}
	if sourceType != "tool_invocation" {
		t.Errorf("expected source_type %q, got %q — the handler must route to "+
			"the typed path when source_type is supplied", "tool_invocation", sourceType)
	}
}

// TestP2Tool_HandleProposeWithoutSourceTypeIsLegacy pins that the handler
// routes to the legacy path when the parameter is absent.
//
// The legacy path is still the legacy path: the citation is recorded
// unvalidated, its event_id is the DOWNSTREAM id (the legacy
// overloading), and the proposal is accepted.
//
// D3 changed ONE thing about it: the auto-resolution now finds the two
// canonical artifact kinds the historical resolver could not see — a work
// item and a tool invocation — so the recorded source_type names what is
// actually cited instead of falling back to "memory". The compatibility
// guarantee this test pins ("callers that do not supply source_type must
// be unaffected") is about the CALLER's experience — the call succeeds,
// the citation is recorded, nothing is rejected — and that is unchanged.
//
// The event_id assertion is what keeps this test non-vacuous: on the
// TYPED path the event_id is the CITED id, so a handler that routed a
// legacy call to the typed path would fail here.
func TestP2Tool_HandleProposeWithoutSourceTypeIsLegacy(t *testing.T) {
	dm := newTestSharedDM(t)
	invocationID := p2SeedToolInvocation(t, dm, "p2-tool-legacy-invocation")

	res, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis": "a legacy proposal through the tool surface",
			"source_ids": []interface{}{invocationID},
		},
	})
	if err != nil {
		t.Fatalf("legacy propose must not error: %v", err)
	}
	result, _ := res.(map[string]interface{})
	proposalID, _ := result["id"].(string)

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
		t.Errorf("expected the legacy path's auto-resolved type %q, got %q — a citation "+
			"must record the type of what it actually cites",
			"tool_invocation", sourceType)
	}
	// The legacy overloading: the event_id is the DOWNSTREAM id, not the
	// cited id. This is the legacy path's fingerprint, and it is what
	// distinguishes this row from a typed one.
	if eventID != proposalID {
		t.Errorf("expected the legacy path's event_id to be the DOWNSTREAM id %q, got %q",
			proposalID, eventID)
	}
}

// TestP2Tool_HandleProposeRejectsUnresolvableTypedCitation pins the
// rejection at the tool boundary: the call errors and no proposal survives.
func TestP2Tool_HandleProposeRejectsUnresolvableTypedCitation(t *testing.T) {
	dm := newTestSharedDM(t)

	var before int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&before); err != nil {
		t.Fatalf("count theories: %v", err)
	}

	_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":  "a proposal citing something that is not there",
			"source_ids":  []interface{}{"p2-tool-missing-invocation"},
			"source_type": "tool_invocation",
		},
	})
	if err == nil {
		t.Fatal("expected the tool call to be REJECTED because the cited source " +
			"does not resolve")
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("error should say the citation does not resolve, got: %v", err)
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

// TestP2Tool_HandleProposeRejectsUnknownSourceType pins that an unknown
// type is rejected rather than silently ignored.
func TestP2Tool_HandleProposeRejectsUnknownSourceType(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":  "a proposal citing with a type outside the vocabulary",
			"source_ids":  []interface{}{"p2-tool-anything"},
			"source_type": "lifecycle_event",
		},
	})
	if err == nil {
		t.Fatal("expected the tool call to be REJECTED: lifecycle_event is not " +
			"in the citation source-type vocabulary")
	}
	if !strings.Contains(err.Error(), "unknown citation source type") {
		t.Errorf("error should name the unknown type, got: %v", err)
	}
}

// TestP2Tool_EvidenceArtifactTypeEnumMatchesDatabase pins the A-9
// correction.
//
// The registry previously advertised `artifact_type: "skill"`, which the
// evidence table's CHECK constraint rejects — a caller following the
// registry got a CHECK violation. The enum must now be exactly the
// database's vocabulary.
func TestP2Tool_EvidenceArtifactTypeEnumMatchesDatabase(t *testing.T) {
	tool, ok := ByName("mpm_evidence")
	if !ok {
		t.Fatal("mpm_evidence not found in Registry")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("parse mpm_evidence schema: %v", err)
	}
	params, ok := schema["properties"].(map[string]interface{})["params"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_evidence schema has no params object")
	}
	props, ok := params["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_evidence params has no properties")
	}
	declared, ok := props["artifact_type"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_evidence declares no artifact_type")
	}
	enum, ok := declared["enum"].([]interface{})
	if !ok {
		t.Fatalf("artifact_type has no enum: %v", declared)
	}
	for _, e := range enum {
		if s, _ := e.(string); s == "skill" {
			t.Error("the artifact_type enum still advertises \"skill\", which the " +
				"evidence table's CHECK constraint rejects — a caller following " +
				"the registry gets a CHECK violation")
		}
	}

	// The enum must be a subset of what the database accepts. The CHECK
	// constraint is the authority, so read it back out of the live schema
	// rather than duplicating it here.
	dm := newTestSharedDM(t)
	var ddl string
	if err := dm.SQLDB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'evidence'`).
		Scan(&ddl); err != nil {
		t.Fatalf("read evidence DDL: %v", err)
	}
	check := p2ExtractCheckVocabulary(ddl, "artifact_type")
	if len(check) == 0 {
		t.Fatalf("could not extract the artifact_type CHECK vocabulary from %q", ddl)
	}
	for _, e := range enum {
		s, _ := e.(string)
		if s == "" {
			continue
		}
		if !check[s] {
			t.Errorf("the registry advertises artifact_type %q, which the "+
				"evidence table's CHECK constraint rejects (accepted: %v)",
				s, p2SortedKeys(check))
		}
	}
}

// p2ExtractCheckVocabulary pulls the IN-list of a column's CHECK constraint
// out of a CREATE TABLE statement.
func p2ExtractCheckVocabulary(ddl, column string) map[string]bool {
	out := map[string]bool{}
	marker := "CHECK (" + column + " IN ("
	idx := strings.Index(ddl, marker)
	if idx < 0 {
		return out
	}
	rest := ddl[idx+len(marker):]
	end := strings.Index(rest, ")")
	if end < 0 {
		return out
	}
	for _, part := range strings.Split(rest[:end], ",") {
		v := strings.Trim(strings.TrimSpace(part), "'")
		if v != "" {
			out[v] = true
		}
	}
	return out
}

// p2SortedKeys returns a map's keys in sorted order, for stable messages.
func p2SortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestP2Tool_HelpPublishesSourceType pins the DISCOVERY effect of the
// registry change.
//
// The registry schema is not enforced anywhere in MPM's Go code — there is
// no JSON-schema validation in the CLI dispatcher, the runtime dispatcher
// or the MCP server (mcp-go's input-schema validation is opt-in via
// server.WithInputSchemaValidation, which the server does not set). What
// the schema IS, is the tool's published contract:
//
//   - it is what `mpm_help show mpm_theories` returns to an agent as the
//     tool's input schema; and
//   - it is what an MCP client receives as the tool's input schema.
//
// So the registry declaration is what makes the new parameter DISCOVERABLE.
// An agent that has never read the source learns the parameter and its
// vocabulary from this surface. This test pins that.
//
// It is the test that makes the registry change non-vacuously pinned:
// removing the declaration breaks this test even though no call is
// rejected.
func TestP2Tool_HelpPublishesSourceType(t *testing.T) {
	// Help is the discovery surface: it returns the registered schema
	// verbatim, so this is exactly what an agent reads.
	out, err := Help("show", "mpm_theories")
	if err != nil {
		t.Fatalf("mpm_help show mpm_theories: %v", err)
	}
	schema, ok := out[inputSchemaField].(map[string]interface{})
	if !ok {
		t.Fatalf("mpm_help show returned no input_schema: %v", out)
	}

	props, ok := schema["properties"].(map[string]interface{})["params"].(map[string]interface{})
	if !ok {
		t.Fatal("the published schema has no params object")
	}
	branches, ok := props["oneOf"].([]interface{})
	if !ok || len(branches) == 0 {
		t.Fatal("the published schema has no oneOf branches")
	}
	var found map[string]interface{}
	for _, b := range branches {
		branch, _ := b.(map[string]interface{})
		branchProps, _ := branch["properties"].(map[string]interface{})
		if _, has := branchProps["hypothesis"]; has {
			found = branchProps
			break
		}
	}
	if found == nil {
		t.Fatal("the published schema has no propose branch")
	}
	declared, ok := found["source_type"].(map[string]interface{})
	if !ok {
		t.Fatal("the published schema does NOT declare source_type — an agent " +
			"reading mpm_help would never learn that the parameter exists")
	}
	enum, ok := declared["enum"].([]interface{})
	if !ok {
		t.Fatalf("the published source_type has no enum: %v", declared)
	}
	got := map[string]bool{}
	for _, e := range enum {
		s, _ := e.(string)
		got[s] = true
	}
	for _, want := range internal.CitationSourceTypes() {
		if !got[want] {
			t.Errorf("the published source_type enum is missing %q — an agent "+
				"must be able to learn the whole vocabulary from discovery", want)
		}
	}
}
