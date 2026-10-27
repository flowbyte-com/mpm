// d7_string_slice_tools_test.go — regression coverage for M5 defect D7 at
// the TOOL SURFACE.
//
// The parser-level coverage is in internal/core/d7_string_slice_parsing_test.go.
// These tests pin what the tool handlers DO with the parsed list — the
// part that matters to a caller:
//
//   - a Go-native []string of source ids produces the SAME typed, resolvable
//     provenance citations as the JSON-decoded array form;
//   - a citation list is accepted WHOLE or not at all, and a rejected list
//     leaves no proposal behind;
//   - the CLI/MCP path and the direct in-process path agree;
//   - the callers that already had a documented strict contract keep it.
//
// Disposable file-backed databases and isolated environment roots only.
package tools

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// d7ToolTestDM opens a disposable, file-backed DatabaseManager inside an
// isolated workspace root.
func d7ToolTestDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("HOME", ws)
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return dm
}

// d7SeedCitableMemory writes a memory the proposal can cite.
func d7SeedCitableMemory(t *testing.T, dm *mpminternal.DatabaseManager, content string) string {
	t.Helper()
	out, _, err := dm.SaveMemoryWithContext(
		content, "memories", []string{"d7"}, 1.0, "0", mpminternal.ActiveContext{})
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no memory id: %+v", out)
	}
	return id
}

// d7CitationRows reads the citations recorded for one downstream artifact.
func d7CitationRows(t *testing.T, dm *mpminternal.DatabaseManager, downstreamID string) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(`
		SELECT source_id FROM epistemic_provenance
		WHERE downstream_id = ? AND downstream_type = 'theory'`,
		downstreamID)
	if err != nil {
		t.Fatalf("read citations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan citation: %v", err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// d7TheoryCount counts the theory rows — the proof that a rejected
// proposal left nothing behind.
func d7TheoryCount(t *testing.T, dm *mpminternal.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&n); err != nil {
		t.Fatalf("count theories: %v", err)
	}
	return n
}

// ── the end-to-end proof ────────────────────────────────────────────────

// TestD7_NativeStringSliceSourceIDsProduceTypedResolvableCitations is the
// end-to-end proof the brief asks for.
//
// A proposal whose source ids arrive as a Go-native []string must produce
// the SAME typed, resolvable provenance citations as the JSON-decoded
// array form.
//
// Before D7 the native form was dropped at the parse: the proposal was
// created with NO citations, and the caller was told it succeeded.
func TestD7_NativeStringSliceSourceIDsProduceTypedResolvableCitations(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	memA := d7SeedCitableMemory(t, dm, "the first cited observation")
	memB := d7SeedCitableMemory(t, dm, "the second cited observation")

	res, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":          "a proposal citing two memories",
			"validation_criteria": "it can be checked",
			"source_ids":          []string{memA, memB},
			"source_type":         "memory",
		},
	})
	if err != nil {
		t.Fatalf("propose with native []string: %v", err)
	}
	env := res.(map[string]interface{})
	theoryID, _ := env["id"].(string)
	if theoryID == "" {
		t.Fatalf("no theory id: %+v", env)
	}

	// The proposal exists.
	if n := d7TheoryCount(t, dm); n != 1 {
		t.Fatalf("theory rows = %d, want 1", n)
	}

	// And it cites EXACTLY the two memories that were supplied.
	cited := d7CitationRows(t, dm, theoryID)
	want := []string{memA, memB}
	sort.Strings(want)
	if len(cited) != len(want) {
		t.Fatalf("citations = %v, want %v — the supplied ids must all be recorded", cited, want)
	}
	for i := range want {
		if cited[i] != want[i] {
			t.Fatalf("citations = %v, want %v", cited, want)
		}
	}

	// The citations are TYPED and RESOLVABLE — the row names the declared
	// type, and the cited id is a live row of that type.
	var typed, resolvable int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*),
		       SUM(CASE WHEN EXISTS (
		           SELECT 1 FROM memories m
		           WHERE m.id = ep.source_id
		             AND m.collection = 'memories'
		             AND m.deleted_at IS NULL) THEN 1 ELSE 0 END)
		FROM epistemic_provenance ep
		WHERE ep.downstream_id = ? AND ep.source_type = 'memory'`,
		theoryID).Scan(&typed, &resolvable); err != nil {
		t.Fatalf("read typed citations: %v", err)
	}
	if typed != 2 {
		t.Errorf("typed citation rows = %d, want 2", typed)
	}
	if resolvable != 2 {
		t.Errorf("resolvable citation rows = %d, want 2 — a citation must point at a live artifact of the declared type", resolvable)
	}
}

// TestD7_ProposalCarriesEverySuppliedSourceID is the same proof for the
// LEGACY (untyped) citation path, which is the default when no
// source_type is supplied.
func TestD7_ProposalCarriesEverySuppliedSourceID(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	memA := d7SeedCitableMemory(t, dm, "legacy cited observation A")
	memB := d7SeedCitableMemory(t, dm, "legacy cited observation B")

	res, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis": "a proposal citing two memories, untyped",
			"source_ids": []string{memA, memB},
		},
	})
	if err != nil {
		t.Fatalf("propose with native []string: %v", err)
	}
	env := res.(map[string]interface{})
	theoryID, _ := env["id"].(string)

	cited := d7CitationRows(t, dm, theoryID)
	if len(cited) != 2 {
		t.Fatalf("citations = %v, want 2 — the supplied ids must all be recorded", cited)
	}
}

// ── 8. No partially accepted citation list ──────────────────────────────

// TestD7_PartiallyAcceptedCitationListCreatesNoProposal pins
// requirement 8.
//
// A citation list that cannot be represented exactly is REJECTED, and the
// rejection leaves no proposal behind. A proposal resting on fewer
// artifacts than its author declared is an unsupported proposal — and it
// must not exist.
func TestD7_PartiallyAcceptedCitationListCreatesNoProposal(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	memA := d7SeedCitableMemory(t, dm, "a cited observation")
	before := d7TheoryCount(t, dm)

	cases := []struct {
		name  string
		value interface{}
	}{
		{"mixed-type array", []interface{}{memA, 42}},
		{"non-list value", 42},
		{"map", map[string]interface{}{memA: true}},
	}
	for _, tc := range cases {
		_, err := handleMpmTheories(dm, ac, map[string]interface{}{
			"action": "propose",
			"params": map[string]interface{}{
				"hypothesis": "a proposal with a malformed citation list",
				"source_ids": tc.value,
			},
		})
		if err == nil {
			t.Errorf("%s: the malformed citation list was accepted — a partially accepted list must be rejected", tc.name)
		}
		if n := d7TheoryCount(t, dm); n != before {
			t.Errorf("%s: theory rows = %d, want %d — a rejected citation list must leave no proposal behind",
				tc.name, n, before)
		}
	}
}

// ── 9. CLI/MCP and direct in-process parity ─────────────────────────────

// TestD7_CLIMCPAndInProcessParity pins requirement 9.
//
// The CLI and the MCP server both deliver a JSON-decoded payload. A
// direct in-process caller may deliver a native map. The two must produce
// the SAME result for the same logical payload.
func TestD7_CLIMCPAndInProcessParity(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	memID := d7SeedCitableMemory(t, dm, "an observation cited from both paths")

	// The JSON path: what `mpm call` and the MCP server deliver.
	var jsonParams map[string]interface{}
	if err := json.Unmarshal([]byte(`{
		"hypothesis": "a parity proposal",
		"source_ids": ["`+memID+`"],
		"source_type": "memory"
	}`), &jsonParams); err != nil {
		t.Fatalf("decode json params: %v", err)
	}
	jsonRes, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose", "params": jsonParams,
	})
	if err != nil {
		t.Fatalf("json-path propose: %v", err)
	}
	jsonTheoryID, _ := jsonRes.(map[string]interface{})["id"].(string)

	// The in-process path: the same logical payload, natively typed.
	nativeRes, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":  "a parity proposal",
			"source_ids":  []string{memID},
			"source_type": "memory",
		},
	})
	if err != nil {
		t.Fatalf("native-path propose: %v", err)
	}
	nativeTheoryID, _ := nativeRes.(map[string]interface{})["id"].(string)

	// Both proposals exist and cite the same artifact.
	if jsonTheoryID == "" || nativeTheoryID == "" {
		t.Fatalf("missing theory ids: json=%q native=%q", jsonTheoryID, nativeTheoryID)
	}
	if got := d7CitationRows(t, dm, jsonTheoryID); len(got) != 1 || got[0] != memID {
		t.Errorf("json-path citations = %v, want [%s]", got, memID)
	}
	if got := d7CitationRows(t, dm, nativeTheoryID); len(got) != 1 || got[0] != memID {
		t.Errorf("native-path citations = %v, want [%s]", got, memID)
	}
}

// TestD7_DecisionRecordKeepsEverySuppliedSourceID is the decision-side
// half of requirement 7.
//
// The decision path records citations the same way, through the same
// guard.
func TestD7_DecisionRecordKeepsEverySuppliedSourceID(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	memA := d7SeedCitableMemory(t, dm, "a decision's cited observation A")
	memB := d7SeedCitableMemory(t, dm, "a decision's cited observation B")

	res, err := handleMpmDecisions(dm, ac, map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice":     "a decision",
			"rationale":  "because",
			"source_ids": []string{memA, memB},
		},
	})
	if err != nil {
		t.Fatalf("record decision: %v", err)
	}
	env := res.(map[string]interface{})
	decID, _ := env["id"].(string)
	if decID == "" {
		t.Fatalf("no decision id: %+v", env)
	}

	var cited int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM epistemic_provenance
		WHERE downstream_id = ? AND downstream_type = 'decision'`,
		decID).Scan(&cited); err != nil {
		t.Fatalf("count decision citations: %v", err)
	}
	if cited != 2 {
		t.Errorf("decision citations = %d, want 2 — the supplied ids must all be recorded", cited)
	}
}

// ── 10. Existing callers retain their documented behaviour ───────────────

// TestD7_CitationSitesStillAcceptTheCompactStringForms pins requirement
// 10 for the COMPACT string forms at the citation sites.
//
// The MCP server does not validate arguments against the schema it
// publishes, so a comma-separated string really does reach the handler.
// The strict form keeps that shape — it is a WHOLE parse, so no element
// is lost — and the JSON-encoded array string with it. A working caller
// that sends either form must not be broken by the correction.
func TestD7_CitationSitesStillAcceptTheCompactStringForms(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	memA := d7SeedCitableMemory(t, dm, "a cited observation for the compact form")
	memB := d7SeedCitableMemory(t, dm, "another cited observation for the compact form")

	cases := []struct {
		name  string
		value interface{}
	}{
		{"comma-separated string", memA + "," + memB},
		{"json-encoded array string", `["` + memA + `","` + memB + `"]`},
	}

	for _, tc := range cases {
		res, err := handleMpmTheories(dm, ac, map[string]interface{}{
			"action": "propose",
			"params": map[string]interface{}{
				"hypothesis": "a proposal citing via the " + tc.name,
				"source_ids": tc.value,
			},
		})
		if err != nil {
			t.Fatalf("%s: propose: %v", tc.name, err)
		}
		env := res.(map[string]interface{})
		theoryID, _ := env["id"].(string)
		if theoryID == "" {
			t.Fatalf("%s: no theory id: %+v", tc.name, env)
		}

		cited := d7CitationRows(t, dm, theoryID)
		want := []string{memA, memB}
		sort.Strings(want)
		if len(cited) != len(want) {
			t.Fatalf("%s: citations = %v, want %v", tc.name, cited, want)
		}
		for i := range want {
			if cited[i] != want[i] {
				t.Fatalf("%s: citations = %v, want %v", tc.name, cited, want)
			}
		}
	}
}

// TestD7_ExistingCallersRetainTheirDocumentedBehaviour pins requirement 10.
//
// The callers that already had a strict contract keep it, including the
// EXACT error strings. The one thing they gain is the native []string
// shape, which they used to reject.
func TestD7_ExistingCallersRetainTheirDocumentedBehaviour(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	// mpm_decisions.record: a scalar `tags` is still an error, with the
	// original message.
	_, err := handleMpmDecisions(dm, ac, map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "a decision",
			"tags":   "not-an-array",
		},
	})
	if err == nil {
		t.Fatalf("a scalar tags value was accepted — the strict contract must hold")
	}
	if got, want := err.Error(), "tags must be an array of strings, got string"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}

	// mpm_theories.propose: a scalar `dependencies` is still an error.
	_, err = handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":   "a proposal",
			"dependencies": "not-an-array",
		},
	})
	if err == nil {
		t.Fatalf("a scalar dependencies value was accepted — the strict contract must hold")
	}
	if got, want := err.Error(), "dependencies must be an array of strings, got string"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}

	// And a valid JSON array still works exactly as before.
	res, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":   "a proposal with an array dependency",
			"dependencies": []interface{}{"dep-1"},
		},
	})
	if err != nil {
		t.Fatalf("propose with a JSON dependency array: %v", err)
	}
	env := res.(map[string]interface{})
	if id, _ := env["id"].(string); id == "" {
		t.Errorf("no theory id: %+v", env)
	}

	// The native []string shape is now accepted by the SAME strict
	// contract — it used to be rejected.
	res, err = handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis":   "a proposal with a native dependency array",
			"dependencies": []string{"dep-1"},
		},
	})
	if err != nil {
		t.Fatalf("propose with a native dependency array: %v", err)
	}
	env = res.(map[string]interface{})
	if id, _ := env["id"].(string); id == "" {
		t.Errorf("no theory id: %+v", env)
	}
}

// TestD7_NativeTagsAndSourceIDsAreAcceptedEverywhere pins the parity of
// the LENIENT path: a native []string is now accepted everywhere a
// JSON-decoded array was, including the tag lists.
func TestD7_NativeTagsAndSourceIDsAreAcceptedEverywhere(t *testing.T) {
	dm := d7ToolTestDM(t)
	ac := mpminternal.ActiveContext{}

	res, err := handleMpmTheories(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"hypothesis": "a proposal with native tags",
			"tags":       []string{"alpha", "beta"},
		},
	})
	if err != nil {
		t.Fatalf("propose with native tags: %v", err)
	}
	env := res.(map[string]interface{})
	theoryID, _ := env["id"].(string)
	if theoryID == "" {
		t.Fatalf("no theory id: %+v", env)
	}

	var tags string
	if err := dm.SQLDB().QueryRow(
		`SELECT tags FROM memories WHERE id = ?`, theoryID).Scan(&tags); err != nil {
		t.Fatalf("read theory tags: %v", err)
	}
	if !containsAll(tags, "alpha", "beta") {
		t.Errorf("theory tags = %q, want it to contain alpha and beta", tags)
	}
}

// containsAll reports whether s contains every needle.
func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
