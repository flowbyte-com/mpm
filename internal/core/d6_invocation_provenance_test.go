// d6_invocation_provenance_test.go — regression coverage for M5 defect D6:
// incomplete invocation-to-artifact provenance.
//
// THE DEFECT (measured before the fix, on the live workspace):
//
//	artifact_provenance.invocation_id was populated ONLY by the mpm_memory
//	save path. Every other recording path resolved its provenance from
//	the process-wide resolver base, which never carries an invocation id.
//
//	artifact_type  rows  with_invocation  join_to_tool_invocations
//	decision         28        0                   0
//	lesson            5        1                   1
//	memory           54       54                  50
//	work              3        3                   3
//
// So twenty-eight decisions were recorded and NONE of them could be joined
// back to the tool call that produced them. The provenance looked complete
// (a row existed) and was unattributable (the join was empty) — the worst
// of both.
//
// THE CORRECTION: the dispatcher's ActiveContext is threaded into the
// provenance of every recording path, and no path manufactures an
// invocation id it was not given.
//
// These tests use DISPOSABLE FILE-BACKED DATABASES and isolated workspace
// roots (d6TestDM). No live installation or production database is touched.
package internal

import (
	"path/filepath"
	"testing"
)

// ── helpers ─────────────────────────────────────────────────────────────

// d6TestDM opens a disposable, file-backed DatabaseManager inside an
// isolated workspace root. File-backed (not in-memory) so the WAL and
// SAVEPOINT behaviour under test is the production behaviour; disposable so
// nothing leaks into an operator's workspace.
func d6TestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("HOME", ws)
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))

	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return dm
}

// d6SeedInvocation inserts a tool_invocations row — the invocation-time
// record an artifact's provenance must join to.
func d6SeedInvocation(t *testing.T, dm *DatabaseManager, invocationID, toolName, action string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
			(id, session_id, tool_name, action, invocation_id, actor_kind,
			 framework_name, payload_hash, result_status, started_at)
		VALUES (?, 'mpm-sess-d6', ?, ?, ?, 'agent', 'opencode', 'sha256:d6', 'success', 1)`,
		"ti-"+invocationID, toolName, action, invocationID)
	if err != nil {
		t.Fatalf("seed tool_invocations %s: %v", invocationID, err)
	}
}

// d6ProvenanceInvocation reads the invocation_id recorded for one artifact.
// A missing id reads back as the empty string — the honest representation
// of "no invocation is known to have produced this".
func d6ProvenanceInvocation(t *testing.T, dm *DatabaseManager, artifactID, artifactType string) string {
	t.Helper()
	var inv string
	err := dm.SQLDB().QueryRow(`
		SELECT COALESCE(invocation_id, '')
		FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = ?`,
		artifactID, artifactType).Scan(&inv)
	if err != nil {
		t.Fatalf("read provenance for %s/%s: %v", artifactType, artifactID, err)
	}
	return inv
}

// d6ProvenanceRowCount counts the provenance rows for one artifact.
func d6ProvenanceRowCount(t *testing.T, dm *DatabaseManager, artifactID, artifactType string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = ?`,
		artifactID, artifactType).Scan(&n); err != nil {
		t.Fatalf("count provenance rows for %s/%s: %v", artifactType, artifactID, err)
	}
	return n
}

// d6JoinToInvocation counts the provenance rows for an artifact that join
// to a real tool_invocations row.
func d6JoinToInvocation(t *testing.T, dm *DatabaseManager, artifactID, artifactType string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*)
		FROM artifact_provenance ap
		JOIN tool_invocations ti ON ti.invocation_id = ap.invocation_id
		WHERE ap.artifact_id = ? AND ap.artifact_type = ?`,
		artifactID, artifactType).Scan(&n); err != nil {
		t.Fatalf("join provenance for %s/%s: %v", artifactType, artifactID, err)
	}
	return n
}

// ── 1. Memory saves retain accurate invocation provenance ───────────────

// TestD6_MemorySaveRetainsInvocationProvenance pins requirement 1.
//
// The memory path was the ONE path that worked before D6. It must keep
// working — and it must now do so without the shared, racy per-manager
// provenance overlay that used to carry the identity.
func TestD6_MemorySaveRetainsInvocationProvenance(t *testing.T) {
	dm := d6TestDM(t)
	d6SeedInvocation(t, dm, "inv_mem_1", "mpm_memory", "save")

	ac := ActiveContext{
		SessionID:     "ses_d6",
		InvocationID:  "inv_mem_1",
		FrameworkName: "opencode",
		Model:         "test-model",
	}
	out, _, err := dm.SaveMemoryWithContext(
		"a durable fact", "memories", []string{"d6"}, 1.0, "0", ac)
	if err != nil {
		t.Fatalf("SaveMemoryWithContext: %v", err)
	}
	memID, _ := out["id"].(string)
	if memID == "" {
		t.Fatalf("no memory id in result: %+v", out)
	}

	if got := d6ProvenanceInvocation(t, dm, memID, "memory"); got != "inv_mem_1" {
		t.Errorf("memory provenance invocation_id = %q, want %q", got, "inv_mem_1")
	}
	if got := d6JoinToInvocation(t, dm, memID, "memory"); got != 1 {
		t.Errorf("memory provenance joined to %d tool_invocations rows, want 1", got)
	}

	// The framework/model the caller supplied must still be recorded.
	var fw, model string
	if err := dm.SQLDB().QueryRow(`
		SELECT COALESCE(framework_name,''), COALESCE(model_name,'')
		FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'memory'`,
		memID).Scan(&fw, &model); err != nil {
		t.Fatalf("read memory provenance: %v", err)
	}
	if fw != "opencode" {
		t.Errorf("framework_name = %q, want opencode", fw)
	}
	if model != "test-model" {
		t.Errorf("model_name = %q, want test-model", model)
	}
}

// ── 2. Decision writes retain their producing invocation ID ─────────────

// TestD6_DecisionWriteRetainsProducingInvocationID pins requirement 2.
//
// This is the defect's sharpest edge: 28 of 28 live decision provenance
// rows carried no invocation id, so no decision could be joined to the call
// that recorded it.
func TestD6_DecisionWriteRetainsProducingInvocationID(t *testing.T) {
	dm := d6TestDM(t)
	d6SeedInvocation(t, dm, "inv_dec_1", "mpm_decisions", "record")

	ac := ActiveContext{
		SessionID:    "ses_d6",
		InvocationID: "inv_dec_1",
	}
	res, err := dm.RecordDecision("ctx", "choice", "why", "out", []string{"d6"}, nil, ac)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	decID, _ := res["id"].(string)
	if decID == "" {
		t.Fatalf("no decision id in result: %+v", res)
	}

	if got := d6ProvenanceInvocation(t, dm, decID, "decision"); got != "inv_dec_1" {
		t.Errorf("decision provenance invocation_id = %q, want %q", got, "inv_dec_1")
	}
	if got := d6JoinToInvocation(t, dm, decID, "decision"); got != 1 {
		t.Errorf("decision provenance joined to %d tool_invocations rows, want 1", got)
	}
}

// ── 3. Other supported recording actions preserve available provenance ──

// TestD6_OtherRecordingActionsPreserveAvailableProvenance pins
// requirement 3 across every recording path the correction touches.
func TestD6_OtherRecordingActionsPreserveAvailableProvenance(t *testing.T) {
	dm := d6TestDM(t)

	ac := ActiveContext{
		SessionID:    "ses_d6",
		InvocationID: "inv_all_1",
	}
	d6SeedInvocation(t, dm, "inv_all_1", "mpm_theories", "propose")

	// Theory / pending proposal.
	theory, err := dm.ProposeTheory("a hypothesis", "some criteria", nil, nil, []string{"d6"}, ac)
	if err != nil {
		t.Fatalf("ProposeTheory: %v", err)
	}
	theoryID, _ := theory["id"].(string)

	// Lesson.
	_, lesson, err := dm.SaveLesson("a lesson worth keeping", "insight", []string{"d6"}, ac)
	if err != nil {
		t.Fatalf("SaveLesson: %v", err)
	}

	// Handoff / continuity record.
	handoff, err := dm.EndSession("ses_d6", "a summary", HandoffClean, nil, nil, ac)
	if err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	// Skill (procedural memory).
	skillID, err := dm.SaveSkillWithInvocation(
		"d6skill", "1.0.0",
		"---\nname: d6skill\nversion: 1.0.0\n---\nbody", "test-agent", false, ac)
	if err != nil {
		t.Fatalf("SaveSkillWithInvocation: %v", err)
	}

	// Changelog entry.
	changelogID, err := dm.LogChangelogEntry(
		"a changelog fact", "0123456789abcdef0123456789abcdef01234567", []string{"d6"}, ac)
	if err != nil {
		t.Fatalf("LogChangelogEntry: %v", err)
	}

	cases := []struct {
		name         string
		artifactID   string
		artifactType string
	}{
		{"theory", theoryID, "theory"},
		{"lesson", lesson.ID, "lesson"},
		{"handoff", handoff.ID, "handoff"},
		// A skill is stored in the memories table, so its provenance row
		// carries artifact_type 'memory' — the canonical vocabulary has
		// no 'skill' member and widening it is a schema change.
		{"skill", skillID, "memory"},
		// A changelog entry is likewise a memory row.
		{"changelog", changelogID, "memory"},
	}
	for _, tc := range cases {
		if tc.artifactID == "" {
			t.Fatalf("%s: no artifact id", tc.name)
		}
		if got := d6ProvenanceInvocation(t, dm, tc.artifactID, tc.artifactType); got != "inv_all_1" {
			t.Errorf("%s provenance invocation_id = %q, want %q",
				tc.name, got, "inv_all_1")
		}
		if got := d6JoinToInvocation(t, dm, tc.artifactID, tc.artifactType); got != 1 {
			t.Errorf("%s provenance joined to %d tool_invocations rows, want 1",
				tc.name, got)
		}
	}
}

// ── 4. Two artifacts from the same invocation share the invocation ID ────

// TestD6_TwoArtifactsFromOneInvocationShareTheInvocationID pins
// requirement 4.
//
// One tool call may legitimately produce more than one artifact (a
// decision that also writes a lesson, a handoff that also updates a work).
// Sharing an invocation id across them is CORRECT — it is one turn of one
// agent. The defect was the opposite: an id that belonged to NOBODY.
func TestD6_TwoArtifactsFromOneInvocationShareTheInvocationID(t *testing.T) {
	dm := d6TestDM(t)
	d6SeedInvocation(t, dm, "inv_shared", "mpm_decisions", "record")

	ac := ActiveContext{SessionID: "ses_d6", InvocationID: "inv_shared"}

	dec, err := dm.RecordDecision("ctx", "choice", "why", "", []string{"d6"}, nil, ac)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	decID, _ := dec["id"].(string)

	_, lesson, err := dm.SaveLesson("a shared-invocation lesson", "insight", []string{"d6"}, ac)
	if err != nil {
		t.Fatalf("SaveLesson: %v", err)
	}

	if got := d6ProvenanceInvocation(t, dm, decID, "decision"); got != "inv_shared" {
		t.Errorf("decision invocation_id = %q, want inv_shared", got)
	}
	if got := d6ProvenanceInvocation(t, dm, lesson.ID, "lesson"); got != "inv_shared" {
		t.Errorf("lesson invocation_id = %q, want inv_shared", got)
	}

	// Both join to the SAME invocation row — one call, one id.
	if got := d6JoinToInvocation(t, dm, decID, "decision"); got != 1 {
		t.Errorf("decision joined to %d invocation rows, want 1", got)
	}
	if got := d6JoinToInvocation(t, dm, lesson.ID, "lesson"); got != 1 {
		t.Errorf("lesson joined to %d invocation rows, want 1", got)
	}
}

// ── 5. Distinct invocations are never conflated ──────────────────────────

// TestD6_DistinctInvocationsAreNeverConflated pins requirement 5.
//
// The pre-D6 bridge was a per-MANAGER overlay: a long-lived MCP server
// dispatches concurrently, and two overlapping writes could stamp each
// other's invocation id. The correction passes the identity as an argument,
// so conflation is structurally impossible.
func TestD6_DistinctInvocationsAreNeverConflated(t *testing.T) {
	dm := d6TestDM(t)
	d6SeedInvocation(t, dm, "inv_a", "mpm_decisions", "record")
	d6SeedInvocation(t, dm, "inv_b", "mpm_decisions", "record")

	decA, err := dm.RecordDecision("ctx", "a", "why", "", []string{"d6"}, nil,
		ActiveContext{SessionID: "ses_d6", InvocationID: "inv_a"})
	if err != nil {
		t.Fatalf("RecordDecision A: %v", err)
	}
	decB, err := dm.RecordDecision("ctx", "b", "why", "", []string{"d6"}, nil,
		ActiveContext{SessionID: "ses_d6", InvocationID: "inv_b"})
	if err != nil {
		t.Fatalf("RecordDecision B: %v", err)
	}
	decAID, _ := decA["id"].(string)
	decBID, _ := decB["id"].(string)

	if got := d6ProvenanceInvocation(t, dm, decAID, "decision"); got != "inv_a" {
		t.Errorf("decision A invocation_id = %q, want inv_a", got)
	}
	if got := d6ProvenanceInvocation(t, dm, decBID, "decision"); got != "inv_b" {
		t.Errorf("decision B invocation_id = %q, want inv_b", got)
	}

	// A concurrency-shaped probe: two writes issued back to back on the
	// same manager must not cross their identities.
	for i := 0; i < 20; i++ {
		id := "inv_p" + string(rune('a'+i%20))
		d6SeedInvocation(t, dm, id, "mpm_decisions", "record")
		res, err := dm.RecordDecision("ctx", id, "why", "", []string{"d6"}, nil,
			ActiveContext{SessionID: "ses_d6", InvocationID: id})
		if err != nil {
			t.Fatalf("RecordDecision %s: %v", id, err)
		}
		artifactID, _ := res["id"].(string)
		if got := d6ProvenanceInvocation(t, dm, artifactID, "decision"); got != id {
			t.Fatalf("provenance invocation_id = %q, want %q — invocations were conflated", got, id)
		}
	}
}

// ── 6. Missing provenance stays explicitly missing ───────────────────────

// TestD6_MissingProvenanceStaysMissing pins requirement 6.
//
// The pre-D6 code minted an invocation id when the caller had none:
//
//	if ac.InvocationID != "" { ... } else if base.InvocationID == "" {
//	    base.InvocationID = GenerateID()
//	}
//
// That id joined to no tool_invocations row, so the provenance looked
// complete and attributed nothing. A missing invocation id must stay
// missing.
func TestD6_MissingProvenanceStaysMissing(t *testing.T) {
	dm := d6TestDM(t)

	// A write with NO identity at all — a background caller.
	res, err := dm.RecordDecision("ctx", "choice", "why", "", []string{"d6"}, nil,
		ActiveContext{})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	decID, _ := res["id"].(string)

	got := d6ProvenanceInvocation(t, dm, decID, "decision")
	if got != "" {
		t.Errorf("decision provenance invocation_id = %q, want EMPTY — a missing "+
			"invocation id must not be synthesized", got)
	}

	// The provenance row still exists: the artifact IS recorded, it is
	// simply not attributable.
	if n := d6ProvenanceRowCount(t, dm, decID, "decision"); n != 1 {
		t.Errorf("decision provenance rows = %d, want 1 (the artifact is recorded)", n)
	}
	if n := d6JoinToInvocation(t, dm, decID, "decision"); n != 0 {
		t.Errorf("decision joined to %d invocation rows, want 0 — a synthesized id "+
			"must not appear to be attributable", n)
	}

	// The same must hold for the resolver-base path: an empty ActiveContext
	// must not acquire a fabricated id from the environment either.
	_, lesson, err := dm.SaveLesson("a lesson with no invocation", "insight", []string{"d6"})
	if err != nil {
		t.Fatalf("SaveLesson: %v", err)
	}
	if got := d6ProvenanceInvocation(t, dm, lesson.ID, "lesson"); got != "" {
		t.Errorf("lesson provenance invocation_id = %q, want EMPTY", got)
	}

	// The sharpest form of the rule: a caller that knows SOMETHING but
	// not the invocation. The provenance row records what is known — the
	// framework — and leaves the unknown empty.
	//
	// This is where the fabrication lived: the pre-D6 bridge resolved the
	// context through provenanceFromContext, which minted an id when the
	// context had none. A framework name is not an invocation identity.
	out, _, err := dm.SaveMemoryWithContext(
		"a fact with a framework but no invocation", "memories", []string{"d6"}, 1.0, "0",
		ActiveContext{FrameworkName: "opencode"})
	if err != nil {
		t.Fatalf("SaveMemoryWithContext: %v", err)
	}
	memID, _ := out["id"].(string)
	if memID == "" {
		t.Fatalf("no memory id: %+v", out)
	}
	if got := d6ProvenanceInvocation(t, dm, memID, "memory"); got != "" {
		t.Errorf("memory provenance invocation_id = %q, want EMPTY — a caller that "+
			"knows the framework does not thereby know the invocation", got)
	}
	var fw string
	if err := dm.SQLDB().QueryRow(`
		SELECT COALESCE(framework_name,'') FROM artifact_provenance
		WHERE artifact_id = ? AND artifact_type = 'memory'`, memID).Scan(&fw); err != nil {
		t.Fatalf("read memory provenance: %v", err)
	}
	if fw != "opencode" {
		t.Errorf("framework_name = %q, want opencode — what IS known must still be recorded", fw)
	}
}

// ── 7. Retries and replays do not create false attribution ───────────────

// TestD6_RetriesAndReplaysDoNotCreateFalseAttribution pins requirement 7.
//
// Two shapes of retry exist:
//
//   - A RETRY under the SAME invocation (the transport re-sent the call).
//     The artifact is a NEW artifact with the same invocation id. No
//     conflation: each artifact keeps its own row.
//   - A REPLAY that OVERWRITES an existing artifact in place (a handoff
//     rewritten for the same session, a skill force-overwritten). The
//     artifact keeps its id, so the provenance row must be REFRESHED to the
//     latest writer. Leaving it on the first writer would attribute the
//     current content to an invocation that did not produce it.
func TestD6_RetriesAndReplaysDoNotCreateFalseAttribution(t *testing.T) {
	dm := d6TestDM(t)
	d6SeedInvocation(t, dm, "inv_retry", "mpm_decisions", "record")
	d6SeedInvocation(t, dm, "inv_h1", "mpm_handoff", "write")
	d6SeedInvocation(t, dm, "inv_h2", "mpm_handoff", "write")

	// A retry under the same invocation: two artifacts, one invocation.
	acRetry := ActiveContext{SessionID: "ses_d6", InvocationID: "inv_retry"}
	first, err := dm.RecordDecision("ctx", "one", "why", "", []string{"d6"}, nil, acRetry)
	if err != nil {
		t.Fatalf("RecordDecision (first): %v", err)
	}
	second, err := dm.RecordDecision("ctx", "two", "why", "", []string{"d6"}, nil, acRetry)
	if err != nil {
		t.Fatalf("RecordDecision (retry): %v", err)
	}
	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID == secondID {
		t.Fatalf("the two decisions share an id (%s) — a retry must be a new artifact", firstID)
	}
	for _, id := range []string{firstID, secondID} {
		if got := d6ProvenanceInvocation(t, dm, id, "decision"); got != "inv_retry" {
			t.Errorf("decision %s invocation_id = %q, want inv_retry", id, got)
		}
	}

	// A replay that overwrites the artifact in place: the handoff.
	h1, err := dm.EndSession("ses_replay", "the first summary", HandoffClean, nil, nil,
		ActiveContext{SessionID: "ses_replay", InvocationID: "inv_h1"})
	if err != nil {
		t.Fatalf("EndSession (first): %v", err)
	}
	if got := d6ProvenanceInvocation(t, dm, h1.ID, "handoff"); got != "inv_h1" {
		t.Fatalf("handoff invocation_id = %q, want inv_h1", got)
	}

	h2, err := dm.EndSession("ses_replay", "the second summary", HandoffClean, nil, nil,
		ActiveContext{SessionID: "ses_replay", InvocationID: "inv_h2"})
	if err != nil {
		t.Fatalf("EndSession (replay): %v", err)
	}
	if h2.ID != h1.ID {
		t.Fatalf("handoff id changed on rewrite: %s -> %s", h1.ID, h2.ID)
	}
	if got := d6ProvenanceInvocation(t, dm, h2.ID, "handoff"); got != "inv_h2" {
		t.Errorf("handoff invocation_id after rewrite = %q, want inv_h2 — the row must "+
			"describe the content as it stands, not the first writer", got)
	}
	// Exactly one provenance row: the rewrite refreshed it, it did not
	// accumulate a second one.
	if n := d6ProvenanceRowCount(t, dm, h2.ID, "handoff"); n != 1 {
		t.Errorf("handoff provenance rows = %d, want 1", n)
	}
	// And the latest writer is the one that joins.
	if n := d6JoinToInvocation(t, dm, h2.ID, "handoff"); n != 1 {
		t.Errorf("handoff joined to %d invocation rows, want 1", n)
	}
}

// ── 10. Existing historical records remain accessible without backfill ───

// TestD6_HistoricalRecordsRemainAccessibleWithoutBackfill pins
// requirement 10.
//
// D6 changes no schema and runs no migration. A row written before the
// correction — with an EMPTY invocation id — must still be readable, and
// must still read as EMPTY: no backfill invented an id for it, because an
// invented id would attribute the historical artifact to a call that never
// happened.
func TestD6_HistoricalRecordsRemainAccessibleWithoutBackfill(t *testing.T) {
	dm := d6TestDM(t)

	// A pre-D6 decision: a memories row plus a provenance row with no
	// invocation id, exactly as the pre-correction code wrote it.
	const legacyID = "legacy-decision-0001"
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at)
		VALUES (?, 'decisions', 'a decision from before the correction', '[]', '{}', ?)`,
		legacyID, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed legacy memory: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO artifact_provenance
			(id, artifact_id, artifact_type, created_at, actor_kind, session_id)
		VALUES ('legacy-prov-1', ?, 'decision', 1, 'agent', 'ses_legacy')`,
		legacyID); err != nil {
		t.Fatalf("seed legacy provenance: %v", err)
	}

	// The historical record is still readable.
	mem, err := dm.GetMemory(legacyID)
	if err != nil {
		t.Fatalf("GetMemory(legacy): %v", err)
	}
	if mem["content"] != "a decision from before the correction" {
		t.Errorf("legacy content = %v", mem["content"])
	}

	// And its provenance still reads as EMPTY — nothing backfilled it.
	if got := d6ProvenanceInvocation(t, dm, legacyID, "decision"); got != "" {
		t.Errorf("legacy provenance invocation_id = %q, want EMPTY — D6 must not "+
			"backfill an id it cannot know", got)
	}
	if n := d6JoinToInvocation(t, dm, legacyID, "decision"); n != 0 {
		t.Errorf("legacy decision joined to %d invocation rows, want 0", n)
	}

	// The historical row does not block a NEW decision with the same
	// content — the recording path is unaffected by the presence of
	// legacy rows.
	d6SeedInvocation(t, dm, "inv_new", "mpm_decisions", "record")
	res, err := dm.RecordDecision("ctx", "a fresh choice", "why", "", []string{"d6"}, nil,
		ActiveContext{SessionID: "ses_d6", InvocationID: "inv_new"})
	if err != nil {
		t.Fatalf("RecordDecision after legacy rows: %v", err)
	}
	newID, _ := res["id"].(string)
	if got := d6ProvenanceInvocation(t, dm, newID, "decision"); got != "inv_new" {
		t.Errorf("new decision invocation_id = %q, want inv_new", got)
	}
}

// ── the fabrication itself ──────────────────────────────────────────────

// TestD6_NoInvocationIDIsFabricatedAnywhere pins the rule the whole
// correction rests on: MPM never invents an invocation identity.
//
// It covers the two fabrication sites D6 removed:
//
//   - provenanceFromContext, which minted an id for a context that had none;
//   - AppendWorkEvent, which minted an id for an event with no invocation.
//
// The assertion is deliberately about JOINABILITY rather than about a
// literal value: an id that joins to no tool_invocations row is a
// fabricated id, whatever it looks like.
func TestD6_NoInvocationIDIsFabricatedAnywhere(t *testing.T) {
	dm := d6TestDM(t)

	// A work item created with no invocation identity.
	work, err := dm.CreateWorkWithContext("a work with no invocation", "", "", ActiveContext{})
	if err != nil {
		t.Fatalf("CreateWorkWithContext: %v", err)
	}

	// The work's provenance row must carry no invocation id.
	if got := d6ProvenanceInvocation(t, dm, work.ID, "work"); got != "" {
		t.Errorf("work provenance invocation_id = %q, want EMPTY — no id may be "+
			"manufactured to complete a provenance row", got)
	}
	if n := d6JoinToInvocation(t, dm, work.ID, "work"); n != 0 {
		t.Errorf("work provenance joined to %d invocation rows, want 0", n)
	}

	// The work's events must carry no invocation id either.
	events, err := dm.GetWorkEvents(work.ID)
	if err != nil {
		t.Fatalf("GetWorkEvents: %v", err)
	}
	for _, e := range events {
		if e.InvocationID != "" {
			t.Errorf("work event %s invocation_id = %q, want EMPTY — no id may be "+
				"manufactured to complete an event row", e.ID, e.InvocationID)
		}
	}

	// Nothing anywhere in the table may carry an id that joins to no
	// invocation row while looking like a real one. Any non-empty
	// invocation_id must correspond to a tool_invocations row.
	var orphan int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM artifact_provenance ap
		WHERE COALESCE(ap.invocation_id, '') <> ''
		  AND NOT EXISTS (
			SELECT 1 FROM tool_invocations ti WHERE ti.invocation_id = ap.invocation_id
		  )`).Scan(&orphan); err != nil {
		t.Fatalf("count orphan provenance ids: %v", err)
	}
	if orphan != 0 {
		t.Errorf("%d provenance rows carry an invocation id that joins to no "+
			"tool_invocations row — a fabricated id", orphan)
	}
}
