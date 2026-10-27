package internal

// p2_citation_test.go — regression coverage for P2: typed, validated and
// idempotent evidence citations on `mpm_theories propose`.
//
// WHAT P2 CHANGES.
//
// mpm-event/0.1 §3.7 (M5) requires that a proposal cite RESOLVABLE evidence
// and that a proposal whose evidence cannot be resolved is REJECTED. The
// historical `source_ids` path could not satisfy that:
//
//   - the cited id was NEVER validated — a proposal citing an id that
//     resolves to nothing was accepted with "success": true; and
//   - the source type was NEVER carried — it was left empty and
//     auto-resolved, so a citation of a tool invocation was stored as a
//     citation of a memory.
//
// P2 adds the typed path on top of `epistemic_provenance`, which already
// provides a typed source, an identifiable event id and an idempotent
// unique key. NO SCHEMA CHANGE.
//
// THE TWO PATHS.
//
//   - TYPED (a citation with a non-empty Type): the type must be in the
//     vocabulary, the cited id must resolve AS THAT TYPE, the declared type
//     is recorded verbatim, and the cited id becomes the event id. A
//     citation that fails is REJECTED, and the rejection leaves no partial
//     proposal record.
//
//   - LEGACY (the historical `source_ids` path): UNCHANGED. Callers that do
//     not supply the optional typed field keep today's behaviour exactly.
//
// ISOLATION.
//
// Every test pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and opens a disposable file-backed database at the canonical
// <workspace>/src/db/mpm.db path, so the production path resolution is
// exercised rather than bypassed. The DB-safety AST guard flags only an
// EMPTY string literal as the first argument to NewDatabaseManager — the
// case that falls through to $HOME/.mpm — so no whitelist entry is needed.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// ── isolation helpers ─────────────────────────────────────────────────────

// p2IsolatedWorkspace pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and returns the disposable workspace root.
//
// HOME is pinned because config.GetMPMDir() falls back to $HOME/.mpm when
// MPM_WORKSPACE is unset, and because workspace-scoped files (active.json,
// toxicphrases.txt) are created under the resolved root.
//
// MPM_DB_PATH is pinned because mpm-critic and mpm-scheduler honour it while
// mpm and mpm-mcp ignore it.
func p2IsolatedWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))
	return ws
}

// p2NewDM opens a disposable DatabaseManager rooted at the isolated
// workspace. The database therefore lives at <workspace>/src/db/mpm.db —
// exactly the path MPM_DB_PATH names.
func p2NewDM(t *testing.T) *DatabaseManager {
	t.Helper()
	ws := p2IsolatedWorkspace(t)
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("p2NewDM: NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	return dm
}

// p2SeedToolInvocation inserts a minimal tool_invocations row and returns its
// id. The table is CHECK-constrained to tool invocations and NOT NULL on
// session_id, tool_name, action, invocation_id, actor_kind, payload_hash,
// result_status and started_at.
func p2SeedToolInvocation(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		   (id, session_id, tool_name, action, invocation_id, actor_kind,
		    payload_hash, result_status, started_at, completed_at, duration_ms,
		    framework_name, mpm_session_id, framework_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "p2-session", "mpm_memory", "save", "inv-"+id, "agent",
		"sha256:"+id, "success", 1793000000, 1793000000, 1,
		"opencode", "p2-mpm-session", "p2-framework-session")
	if err != nil {
		t.Fatalf("seed tool_invocations row %q: %v", id, err)
	}
	return id
}

// p2SeedMemory inserts a plain memory row and returns its id.
func p2SeedMemory(t *testing.T, dm *DatabaseManager, content string) string {
	t.Helper()
	id, err := dm.SaveMemoryNodeForInvocation(
		"memories", content, "", []string{"p2"}, nil, nil,
		false, 1.0, "", "0.8", "0.8", "", ActiveContext{},
	)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	return id
}

// p2CitationRows returns the epistemic_provenance rows for a downstream
// artifact.
func p2CitationRows(t *testing.T, dm *DatabaseManager, downstreamID string) []struct {
	SourceID   string
	SourceType string
	EventID    string
} {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT source_id, source_type, event_id FROM epistemic_provenance
		  WHERE downstream_id = ? ORDER BY rowid ASC`, downstreamID)
	if err != nil {
		t.Fatalf("query epistemic_provenance: %v", err)
	}
	defer rows.Close()
	var out []struct {
		SourceID   string
		SourceType string
		EventID    string
	}
	for rows.Next() {
		var r struct {
			SourceID   string
			SourceType string
			EventID    string
		}
		if err := rows.Scan(&r.SourceID, &r.SourceType, &r.EventID); err != nil {
			t.Fatalf("scan epistemic_provenance: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate epistemic_provenance: %v", err)
	}
	return out
}

// p2TheoryCount counts the theory rows in the workspace.
func p2TheoryCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n sql.NullInt64
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&n); err != nil {
		t.Fatalf("count theories: %v", err)
	}
	return int(n.Int64)
}

// ── correct types ─────────────────────────────────────────────────────────

// TestP2Citation_TypedCitationRecordsDeclaredTypeAndCitedEventID pins the
// positive path.
//
// A typed citation of a tool invocation must be stored with the DECLARED
// source type — not the "memory" fallback the legacy path produces — and
// with the CITED id as the event id, so the citation row identifies the
// observation the proposal rests on.
func TestP2Citation_TypedCitationRecordsDeclaredTypeAndCitedEventID(t *testing.T) {
	dm := p2NewDM(t)
	invocationID := p2SeedToolInvocation(t, dm, "p2-typed-invocation-0001")

	res, err := dm.ProposeTheoryWithCitations(
		"A session consulted MPM repeatedly and recorded nothing.",
		"Show the session's invocation ids.",
		nil,
		[]Citation{{ID: invocationID, Type: CitationSourceToolInvocation}},
		[]string{"p2", "typed"},
	)
	if err != nil {
		t.Fatalf("ProposeTheoryWithCitations: %v", err)
	}
	proposalID, _ := res["id"].(string)
	if proposalID == "" {
		t.Fatalf("no proposal id returned: %v", res)
	}

	rows := p2CitationRows(t, dm, proposalID)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 citation row, got %d: %+v", len(rows), rows)
	}
	if rows[0].SourceID != invocationID {
		t.Errorf("expected source_id %q, got %q", invocationID, rows[0].SourceID)
	}
	if rows[0].SourceType != CitationSourceToolInvocation {
		t.Errorf("expected source_type %q, got %q — the declared type must be "+
			"recorded verbatim, not auto-resolved to %q",
			CitationSourceToolInvocation, rows[0].SourceType, "memory")
	}
	if rows[0].EventID != invocationID {
		t.Errorf("expected event_id to be the CITED id %q so the citation is "+
			"self-describing, got %q", invocationID, rows[0].EventID)
	}
}

// TestP2Citation_TypedCitationOfEachArtifactTypeResolves pins that every
// source type in the vocabulary resolves against the RIGHT table — a
// memory id is not a lesson id, and a theory id is not a decision id.
func TestP2Citation_TypedCitationOfEachArtifactTypeResolves(t *testing.T) {
	dm := p2NewDM(t)

	memoryID := p2SeedMemory(t, dm, "a plain memory to cite")
	lesson, err := dm.AddLesson("a lesson to cite", LessonTypeWarning, []string{"p2"}, "")
	if err != nil {
		t.Fatalf("AddLesson: %v", err)
	}
	lessonID := lesson.ID

	proposed, err := dm.ProposeTheory("a theory to cite", "vc", nil, nil, []string{"p2"})
	if err != nil {
		t.Fatalf("ProposeTheory: %v", err)
	}
	theoryID, _ := proposed["id"].(string)

	cases := []struct {
		sourceType string
		id         string
	}{
		{CitationSourceMemory, memoryID},
		{CitationSourceLesson, lessonID},
		{CitationSourceTheory, theoryID},
	}
	for _, tc := range cases {
		t.Run(tc.sourceType, func(t *testing.T) {
			res, err := dm.ProposeTheoryWithCitations(
				"citing "+tc.sourceType, "vc", nil,
				[]Citation{{ID: tc.id, Type: tc.sourceType}},
				[]string{"p2", "typed", tc.sourceType},
			)
			if err != nil {
				t.Fatalf("ProposeTheoryWithCitations(%s): %v", tc.sourceType, err)
			}
			proposalID, _ := res["id"].(string)
			rows := p2CitationRows(t, dm, proposalID)
			if len(rows) != 1 {
				t.Fatalf("expected 1 citation row, got %d", len(rows))
			}
			if rows[0].SourceType != tc.sourceType {
				t.Errorf("expected source_type %q, got %q", tc.sourceType, rows[0].SourceType)
			}
		})
	}
}

// ── invalid sources ───────────────────────────────────────────────────────

// TestP2Citation_UnresolvableSourceIsRejected pins the rejection path.
//
// A cited id that resolves to nothing must REJECT the proposal and leave no
// partial proposal record behind.
func TestP2Citation_UnresolvableSourceIsRejected(t *testing.T) {
	dm := p2NewDM(t)
	before := p2TheoryCount(t, dm)

	_, err := dm.ProposeTheoryWithCitations(
		"A session consulted MPM but recorded nothing.",
		"Show the session's invocation ids.",
		nil,
		[]Citation{{ID: "p2-no-such-invocation", Type: CitationSourceToolInvocation}},
		[]string{"p2", "invalid"},
	)
	if err == nil {
		t.Fatalf("expected the proposal to be REJECTED because its cited source " +
			"does not resolve")
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("error should say the citation does not resolve, got: %v", err)
	}

	after := p2TheoryCount(t, dm)
	if after != before {
		t.Errorf("expected NO theory row to survive the rejection, count went %d -> %d",
			before, after)
	}
	var citations int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance`).Scan(&citations); err != nil {
		t.Fatalf("count epistemic_provenance: %v", err)
	}
	if citations != 0 {
		t.Errorf("expected NO citation row to survive the rejection, got %d", citations)
	}
}

// TestP2Citation_WrongDeclaredTypeIsRejected pins that the declared type
// must be CORRECT, not merely that the id exists.
//
// A memory id declared as a tool invocation resolves as a memory but not as
// a tool invocation — so the citation is rejected.
func TestP2Citation_WrongDeclaredTypeIsRejected(t *testing.T) {
	dm := p2NewDM(t)
	memoryID := p2SeedMemory(t, dm, "a memory that is not a tool invocation")

	for _, wrongType := range []string{
		CitationSourceToolInvocation,
		CitationSourceLesson,
		CitationSourceDecision,
		CitationSourceWork,
	} {
		t.Run(wrongType, func(t *testing.T) {
			before := p2TheoryCount(t, dm)
			_, err := dm.ProposeTheoryWithCitations(
				"citing a memory under the wrong type", "vc", nil,
				[]Citation{{ID: memoryID, Type: wrongType}},
				[]string{"p2", "wrongtype", wrongType},
			)
			if err == nil {
				t.Fatalf("expected the proposal to be REJECTED: a memory id "+
					"does not resolve as %q", wrongType)
			}
			if after := p2TheoryCount(t, dm); after != before {
				t.Errorf("expected no theory row to survive, count went %d -> %d",
					before, after)
			}
		})
	}
}

// TestP2Citation_UnknownSourceTypeIsRejected pins that the vocabulary is
// closed: a type outside it is a caller error, not a silently ignored
// citation.
func TestP2Citation_UnknownSourceTypeIsRejected(t *testing.T) {
	dm := p2NewDM(t)
	before := p2TheoryCount(t, dm)

	_, err := dm.ProposeTheoryWithCitations(
		"citing with an unknown type", "vc", nil,
		[]Citation{{ID: "anything", Type: "lifecycle_event"}},
		[]string{"p2", "unknowntype"},
	)
	if err == nil {
		t.Fatalf("expected the proposal to be REJECTED: %q is not in the "+
			"citation source-type vocabulary", "lifecycle_event")
	}
	if !strings.Contains(err.Error(), "unknown citation source type") {
		t.Errorf("error should name the unknown type, got: %v", err)
	}
	if after := p2TheoryCount(t, dm); after != before {
		t.Errorf("expected no theory row to survive, count went %d -> %d", before, after)
	}
}

// TestP2Citation_SoftDeletedArtifactIsRejected pins that a citation must
// point at something that is still there: a soft-deleted artifact does not
// count.
func TestP2Citation_SoftDeletedArtifactIsRejected(t *testing.T) {
	dm := p2NewDM(t)
	memoryID := p2SeedMemory(t, dm, "a memory that will be deleted")

	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET deleted_at = ? WHERE id = ?`,
		1793000000, memoryID); err != nil {
		t.Fatalf("soft-delete memory: %v", err)
	}

	_, err := dm.ProposeTheoryWithCitations(
		"citing a deleted memory", "vc", nil,
		[]Citation{{ID: memoryID, Type: CitationSourceMemory}},
		[]string{"p2", "deleted"},
	)
	if err == nil {
		t.Fatalf("expected the proposal to be REJECTED: a soft-deleted " +
			"artifact does not resolve")
	}
}

// ── atomic failure ────────────────────────────────────────────────────────

// TestP2Citation_AtomicFailureLeavesNoPartialRecord is the atomicity test.
//
// A batch where the FIRST citation is valid and the SECOND is not must
// leave NOTHING behind: not the valid citation, not the theory row, not the
// dependency patch. A partial write would mean a proposal that exists
// without the evidence it claims to rest on — exactly the state M5 forbids.
func TestP2Citation_AtomicFailureLeavesNoPartialRecord(t *testing.T) {
	dm := p2NewDM(t)
	validInvocation := p2SeedToolInvocation(t, dm, "p2-atomic-valid-invocation")
	before := p2TheoryCount(t, dm)

	_, err := dm.ProposeTheoryWithCitations(
		"A proposal with one good citation and one bad one.",
		"vc",
		[]string{"dep-1"},
		[]Citation{
			{ID: validInvocation, Type: CitationSourceToolInvocation},
			{ID: "p2-atomic-missing-invocation", Type: CitationSourceToolInvocation},
		},
		[]string{"p2", "atomic"},
	)
	if err == nil {
		t.Fatalf("expected the proposal to be REJECTED because one citation " +
			"does not resolve")
	}

	if after := p2TheoryCount(t, dm); after != before {
		t.Errorf("expected NO theory row to survive, count went %d -> %d", before, after)
	}
	var citations int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance`).Scan(&citations); err != nil {
		t.Fatalf("count epistemic_provenance: %v", err)
	}
	if citations != 0 {
		t.Errorf("expected NO citation row to survive — including the VALID one, "+
			"which was written before the failing one — got %d", citations)
	}
}

// ── repeated submissions ──────────────────────────────────────────────────

// TestP2Citation_RepeatedSubmissionIsIdempotent pins idempotency.
//
// Re-submitting the same proposal with the same citations must not
// accumulate citation rows: `epistemic_provenance`'s unique key
// (source_id, downstream_id, event_id) plus its ON CONFLICT DO NOTHING makes
// the second write a no-op.
//
// This matters because the proposal itself is already idempotent (identical
// hypothesis + tags + collection return the same proposal id), so a
// re-submission targets the SAME downstream id. Without the unique key the
// citation would be counted twice.
func TestP2Citation_RepeatedSubmissionIsIdempotent(t *testing.T) {
	dm := p2NewDM(t)
	invocationID := p2SeedToolInvocation(t, dm, "p2-idempotent-invocation")

	citation := []Citation{{ID: invocationID, Type: CitationSourceToolInvocation}}

	first, err := dm.ProposeTheoryWithCitations(
		"A repeated proposal.", "vc", nil, citation, []string{"p2", "idempotent"})
	if err != nil {
		t.Fatalf("first ProposeTheoryWithCitations: %v", err)
	}
	second, err := dm.ProposeTheoryWithCitations(
		"A repeated proposal.", "vc", nil, citation, []string{"p2", "idempotent"})
	if err != nil {
		t.Fatalf("second ProposeTheoryWithCitations: %v", err)
	}

	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID != secondID {
		t.Fatalf("expected the proposal itself to be idempotent, got %q and %q",
			firstID, secondID)
	}

	rows := p2CitationRows(t, dm, firstID)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 citation row after two identical "+
			"submissions, got %d: %+v", len(rows), rows)
	}
}

// TestP2Citation_RepeatedSubmissionOfDifferentCitationsAccumulates pins the
// other half of idempotency: two DIFFERENT cited ids are two DIFFERENT
// citations, and both are kept.
//
// The idempotency must come from the cited identity, not from a blanket
// "one citation per proposal" rule — otherwise a proposal that genuinely
// rests on two observations would lose one of them.
func TestP2Citation_RepeatedSubmissionOfDifferentCitationsAccumulates(t *testing.T) {
	dm := p2NewDM(t)
	first := p2SeedToolInvocation(t, dm, "p2-two-citations-a")
	second := p2SeedToolInvocation(t, dm, "p2-two-citations-b")

	res, err := dm.ProposeTheoryWithCitations(
		"A proposal resting on two observations.", "vc", nil,
		[]Citation{
			{ID: first, Type: CitationSourceToolInvocation},
			{ID: second, Type: CitationSourceToolInvocation},
		},
		[]string{"p2", "twocitations"},
	)
	if err != nil {
		t.Fatalf("ProposeTheoryWithCitations: %v", err)
	}
	proposalID, _ := res["id"].(string)

	rows := p2CitationRows(t, dm, proposalID)
	if len(rows) != 2 {
		t.Fatalf("expected 2 citation rows, got %d: %+v", len(rows), rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.SourceType != CitationSourceToolInvocation {
			t.Errorf("expected source_type %q, got %q",
				CitationSourceToolInvocation, r.SourceType)
		}
		seen[r.SourceID] = true
	}
	if !seen[first] || !seen[second] {
		t.Errorf("expected both cited ids to be recorded, got %v", seen)
	}
}

// ── backward compatibility ────────────────────────────────────────────────

// TestP2Citation_LegacyPathIsUnchanged pins backward compatibility.
//
// A caller that does NOT supply a typed citation must get exactly the
// historical behaviour:
//
//   - the cited id is NOT validated — an id that resolves to nothing is
//     accepted;
//   - the source type is auto-resolved, and falls back to "memory" for a
//     non-artifact id (the mislabelling P2 does NOT change on this path);
//   - the event id is the DOWNSTREAM id, not the cited id.
//
// This is the behaviour the cascade materializer and every existing caller
// depends on, and it is deliberately left alone: tightening it is a
// separate, separately-ruled change (tracked as D3-b).
func TestP2Citation_LegacyPathIsUnchanged(t *testing.T) {
	dm := p2NewDM(t)

	res, err := dm.ProposeTheory("A legacy proposal.", "vc", nil,
		[]string{"p2-legacy-unresolvable-id"}, []string{"p2", "legacy"})
	if err != nil {
		t.Fatalf("the legacy path must still accept an unresolvable id: %v", err)
	}
	proposalID, _ := res["id"].(string)

	rows := p2CitationRows(t, dm, proposalID)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 citation row, got %d: %+v", len(rows), rows)
	}
	if rows[0].SourceID != "p2-legacy-unresolvable-id" {
		t.Errorf("expected source_id %q, got %q",
			"p2-legacy-unresolvable-id", rows[0].SourceID)
	}
	if rows[0].SourceType != "memory" {
		t.Errorf("expected the legacy path's auto-resolved fallback %q, got %q — "+
			"the legacy path must be unchanged", "memory", rows[0].SourceType)
	}
	if rows[0].EventID != proposalID {
		t.Errorf("expected the legacy path's event_id to be the DOWNSTREAM id "+
			"%q, got %q", proposalID, rows[0].EventID)
	}
}

// TestP2Citation_LegacyAndTypedPathsAreDistinct pins that the two paths do
// not collide in the unique key: the same cited id recorded through the
// legacy path and through the typed path are two different citations,
// because their event ids differ.
//
// This is deliberate. The legacy path's event id is the downstream id and
// the typed path's is the cited id; keeping them apart means a caller that
// migrates from one path to the other does not silently collide with a row
// written by the other.
func TestP2Citation_LegacyAndTypedPathsAreDistinct(t *testing.T) {
	dm := p2NewDM(t)
	memoryID := p2SeedMemory(t, dm, "a memory cited through both paths")

	legacy, err := dm.ProposeTheory("A legacy proposal citing a memory.", "vc", nil,
		[]string{memoryID}, []string{"p2", "legacy", "bothpaths"})
	if err != nil {
		t.Fatalf("legacy ProposeTheory: %v", err)
	}
	legacyID, _ := legacy["id"].(string)

	typed, err := dm.ProposeTheoryWithCitations("A typed proposal citing a memory.", "vc",
		nil, []Citation{{ID: memoryID, Type: CitationSourceMemory}},
		[]string{"p2", "typed", "bothpaths"})
	if err != nil {
		t.Fatalf("typed ProposeTheoryWithCitations: %v", err)
	}
	typedID, _ := typed["id"].(string)

	legacyRows := p2CitationRows(t, dm, legacyID)
	typedRows := p2CitationRows(t, dm, typedID)
	if len(legacyRows) != 1 || len(typedRows) != 1 {
		t.Fatalf("expected 1 citation row per proposal, got legacy=%d typed=%d",
			len(legacyRows), len(typedRows))
	}
	if legacyRows[0].EventID == typedRows[0].EventID {
		t.Errorf("expected the two paths' event ids to differ (legacy=%q typed=%q) "+
			"so the two citation shapes cannot collide",
			legacyRows[0].EventID, typedRows[0].EventID)
	}
}
