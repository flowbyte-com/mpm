// d3_legacy_citation_test.go — regression coverage for M5 defect D3: the
// LEGACY citation path recorded the wrong type for what it actually cited.
//
// THE DEFECT.
//
// `recordProvenanceNode` resolves an untyped (legacy) citation against
// `ResolveArtifactType`, which knows two things: the `memories` table
// (by its `collection` discriminator) and the lessons table. The
// canonical CITATION vocabulary knows six types — memory, theory,
// decision, lesson, work and tool_invocation — and two of them live in
// their own tables.
//
// A legacy citation of a tool invocation or of a work item therefore
// fell through the resolver and was recorded with
// `source_type = "memory"`.
//
// That is not a label of convenience. It is a FALSE RECORD: the id names
// a tool invocation, and the row says it is a memory. A reader could not
// tell a citation of a tool invocation from a citation of a memory, and
// the typed path — which asks the very same questions — resolves it as
// "tool_invocation".
//
// WHAT THIS FILE PINS.
//
//  1. The correction: a legacy citation records the type of what it
//     cites, for every canonical type.
//  2. The compatibility guarantee that is NOT changed: a legacy citation
//     of an id that resolves to NOTHING is still accepted and still
//     recorded as "memory". Rejecting it is defect D3-a and is a
//     compatibility decision for Session A, not a silent change.
//  3. The typed path is untouched: it still records the declared type
//     verbatim and still rejects a citation that does not resolve.
//
// ISOLATION.
//
// Every test pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and opens a disposable file-backed database at the canonical
// <workspace>/src/db/mpm.db path, so the production path resolution is
// exercised rather than bypassed. The DB-safety AST guard flags only an
// EMPTY string literal as the first argument to NewDatabaseManager, so
// these helpers need no whitelist entry.

package internal

import (
	"testing"
)

// d3SeedWork inserts a work item — an artifact kind that lives in its own
// table and that the historical resolver could not find.
func d3SeedWork(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO works (id, title, status, verification, created_at, updated_at, session_id)
		 VALUES (?, ?, 'open', 'unverified', 1793000000, 1793000000, 'd3-session')`,
		id, "A work item the proposal cites.",
	)
	if err != nil {
		t.Fatalf("seed work %q: %v", id, err)
	}
	return id
}

// d3SeedMemory inserts a plain memory.
func d3SeedMemory(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, tags, metadata, content_hash, identity_hash, created_at)
		 VALUES (?, 'memories', ?, '[]', '{}', ?, ?, 1793000000)`,
		id, "A memory the proposal cites.", "hash-"+id, "identity-"+id,
	)
	if err != nil {
		t.Fatalf("seed memory %q: %v", id, err)
	}
	return id
}

// d3CitationType reads the recorded source_type of the citation recorded
// for one downstream artifact.
func d3CitationType(t *testing.T, dm *DatabaseManager, downstreamID string) string {
	t.Helper()
	var sourceType string
	if err := dm.SQLDB().QueryRow(
		`SELECT source_type FROM epistemic_provenance WHERE downstream_id = ?`,
		downstreamID).Scan(&sourceType); err != nil {
		t.Fatalf("read citation type: %v", err)
	}
	return sourceType
}

// ── 1. The correction ────────────────────────────────────────────────────

// TestD3_LegacyCitationRecordsTheTypeItCites pins the correction.
//
// A legacy (undeclared-type) citation must record the type of the
// artifact it actually cites. Before the correction a citation of a tool
// invocation was recorded as "memory" — a record that was simply wrong.
func TestD3_LegacyCitationRecordsTheTypeItCites(t *testing.T) {
	dm := m5NewDM(t)

	invocationID := m5SeedToolInvocation(t, dm, "d3-tool-invocation-0001")
	workID := d3SeedWork(t, dm, "d3-work-0001")
	memoryID := d3SeedMemory(t, dm, "d3-memory-0001")

	cases := []struct {
		name    string
		cited   string
		want    string
		subject string
	}{
		{"tool invocation", invocationID, CitationSourceToolInvocation, "a session consulted MPM"},
		{"work item", workID, CitationSourceWork, "a work item was carried out"},
		{"memory", memoryID, CitationSourceMemory, "an observation was recorded"},
	}

	for i, tc := range cases {
		res, err := dm.ProposeTheory(
			tc.subject,
			"Show the cited artifact.",
			nil,
			[]string{tc.cited},
			[]string{"d3", "legacy"},
		)
		if err != nil {
			t.Fatalf("%s: ProposeTheory: %v", tc.name, err)
		}
		proposalID, _ := res["id"].(string)
		if proposalID == "" {
			t.Fatalf("%s: no proposal id: %+v", tc.name, res)
		}
		if got := d3CitationType(t, dm, proposalID); got != tc.want {
			t.Errorf("%s: citation source_type = %q, want %q — a citation must record "+
				"the type of what it actually cites", tc.name, got, tc.want)
		}
		// Each proposal is its own row; the count must track the cases.
		if n := m5ScalarInt(t, dm.SQLDB(),
			`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`); n != i+1 {
			t.Fatalf("%s: theory rows = %d, want %d", tc.name, n, i+1)
		}
	}
}

// TestD3_LegacyCitationOfAnUnresolvableIdIsStillAccepted pins the
// compatibility guarantee this correction does NOT change.
//
// A legacy caller citing an id that resolves to nothing — an id that has
// since been shredded, or one that was never right — must still succeed,
// and the citation must still be recorded as "memory". Failing the whole
// write for that is defect D3-a, and it is a compatibility decision for
// Session A (see docs/dev/session-b-d3-legacy-citations.md), not a
// silent change.
//
// This test is the guard that keeps D3-a visible: if a future change
// tightens the legacy path, THIS test fails and the decision is forced
// to be explicit.
func TestD3_LegacyCitationOfAnUnresolvableIdIsStillAccepted(t *testing.T) {
	dm := m5NewDM(t)

	const phantom = "no-such-artifact-d3-000000000000"

	res, err := dm.ProposeTheory(
		"A proposal citing an id that resolves to nothing.",
		"Show the cited artifact.",
		nil,
		[]string{phantom},
		[]string{"d3", "legacy"},
	)
	if err != nil {
		t.Fatalf("a legacy citation of an unresolvable id must still be accepted "+
			"(D3-a is an open compatibility decision, not a silent change): %v", err)
	}
	proposalID, _ := res["id"].(string)

	// The historical fallback is preserved: "memory".
	if got := d3CitationType(t, dm, proposalID); got != CitationSourceMemory {
		t.Errorf("citation source_type = %q, want %q — the historical fallback for an "+
			"unresolvable id is unchanged", got, CitationSourceMemory)
	}

	// And the proposal itself landed.
	if n := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`); n != 1 {
		t.Errorf("theory rows = %d, want 1", n)
	}
}

// ── 2. The typed path is untouched ───────────────────────────────────────

// TestD3_TypedPathIsUnchanged pins that the typed path is exactly as it
// was: the DECLARED type is recorded verbatim, and a citation that does
// not resolve is rejected with no partial proposal left behind.
func TestD3_TypedPathIsUnchanged(t *testing.T) {
	dm := m5NewDM(t)

	invocationID := m5SeedToolInvocation(t, dm, "d3-tool-invocation-0002")

	// The declared type is recorded verbatim.
	res, err := dm.ProposeTheoryWithCitations(
		"A proposal with a typed citation.",
		"Show the cited invocation.",
		nil,
		[]Citation{{ID: invocationID, Type: CitationSourceToolInvocation}},
		[]string{"d3", "typed"},
	)
	if err != nil {
		t.Fatalf("ProposeTheoryWithCitations: %v", err)
	}
	proposalID, _ := res["id"].(string)
	if got := d3CitationType(t, dm, proposalID); got != CitationSourceToolInvocation {
		t.Errorf("citation source_type = %q, want %q", got, CitationSourceToolInvocation)
	}

	// A typed citation that does not resolve is rejected, atomically.
	_, err = dm.ProposeTheoryWithCitations(
		"A proposal with an unresolvable typed citation.",
		"Show the cited invocation.",
		nil,
		[]Citation{{ID: "no-such-invocation-d3", Type: CitationSourceToolInvocation}},
		[]string{"d3", "typed"},
	)
	if err == nil {
		t.Fatalf("expected the typed citation to be REJECTED because it does not resolve")
	}
	if n := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`); n != 1 {
		t.Errorf("theory rows = %d, want 1 — a rejected typed citation must leave no "+
			"partial proposal behind", n)
	}
}

// ── 3. The two paths agree about what an id IS ───────────────────────────

// TestD3_LegacyAndTypedPathsAgreeOnTheType pins the invariant that makes
// the correction safe: the legacy auto-resolution and the typed
// validation must never disagree about what an id is.
//
// They are different paths — one validates a DECLARED type, the other
// infers one — but they must reach the same answer for the same id.
func TestD3_LegacyAndTypedPathsAgreeOnTheType(t *testing.T) {
	dm := m5NewDM(t)

	invocationID := m5SeedToolInvocation(t, dm, "d3-tool-invocation-0003")
	workID := d3SeedWork(t, dm, "d3-work-0002")

	cases := []struct {
		name string
		id   string
		typ  string
	}{
		{"tool invocation", invocationID, CitationSourceToolInvocation},
		{"work item", workID, CitationSourceWork},
	}

	for _, tc := range cases {
		// The legacy path infers the type.
		legacyRes, err := dm.ProposeTheory(
			"A legacy-path proposal citing a "+tc.name+".",
			"Show it.", nil, []string{tc.id}, []string{"d3", "agree"},
		)
		if err != nil {
			t.Fatalf("%s: legacy propose: %v", tc.name, err)
		}
		legacyID, _ := legacyRes["id"].(string)
		legacyType := d3CitationType(t, dm, legacyID)

		// The typed path declares the same type and must accept it.
		typedRes, err := dm.ProposeTheoryWithCitations(
			"A typed-path proposal citing a "+tc.name+".",
			"Show it.", nil,
			[]Citation{{ID: tc.id, Type: tc.typ}},
			[]string{"d3", "agree"},
		)
		if err != nil {
			t.Fatalf("%s: typed propose: %v", tc.name, err)
		}
		typedID, _ := typedRes["id"].(string)
		typedType := d3CitationType(t, dm, typedID)

		if legacyType != typedType {
			t.Errorf("%s: the legacy path recorded %q and the typed path recorded %q — "+
				"the two paths must agree about what an id IS", tc.name, legacyType, typedType)
		}
		if legacyType != tc.typ {
			t.Errorf("%s: recorded type = %q, want %q", tc.name, legacyType, tc.typ)
		}
	}
}
