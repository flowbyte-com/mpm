// d6_provenance_tools_test.go — D6 regression coverage at the TOOL
// SURFACE.
//
// The core-level suite (internal/core/d6_invocation_provenance_test.go)
// pins the provenance bridge itself. These tests pin the two properties
// that only exist at the tool boundary:
//
//  1. The tool handlers actually THREAD the dispatcher's ActiveContext into
//     the recording paths — a bridge in the core is worthless if the
//     handler drops the context on the floor.
//
//  2. `mpm_system event_ingest` — a LIFECYCLE OBSERVATION — must not
//     masquerade as knowledge preservation. It creates no artifact, no
//     provenance row, and it must not make a session look like one that
//     recorded anything.
//
// Disposable file-backed databases and isolated workspace roots only.
package tools

import (
	"path/filepath"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
)

// d6ToolTestDM opens a disposable, file-backed DatabaseManager inside an
// isolated workspace root.
func d6ToolTestDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("HOME", ws)
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))

	dm, err := internal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return dm
}

// d6SeedToolInvocation inserts the invocation-time record an artifact's
// provenance must join to.
func d6SeedToolInvocation(t *testing.T, dm *internal.DatabaseManager, invocationID, toolName, action string) {
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

// ── the handlers thread the dispatcher's context ─────────────────────────

// TestD6_ToolHandlersThreadTheInvocationContext pins that the bridge is
// actually wired at the tool boundary.
//
// Each handler is driven the way the dispatcher drives it — with an
// ActiveContext carrying an invocation id — and the resulting artifact's
// artifact_provenance row must carry that id and join to the
// tool_invocations row of the same call.
func TestD6_ToolHandlersThreadTheInvocationContext(t *testing.T) {
	dm := d6ToolTestDM(t)

	ac := internal.ActiveContext{
		SessionID:     "ses_d6",
		InvocationID:  "inv_handler_1",
		FrameworkName: "opencode",
		Model:         "test-model",
	}
	d6SeedToolInvocation(t, dm, "inv_handler_1", "mpm_decisions", "record")

	// mpm_decisions.record
	res, err := handleRecordDecision(dm, ac, map[string]interface{}{
		"choice":    "the choice",
		"rationale": "the why",
	})
	if err != nil {
		t.Fatalf("handleRecordDecision: %v", err)
	}
	env := res.(map[string]interface{})
	decID, _ := env["id"].(string)
	if decID == "" {
		t.Fatalf("no decision id: %+v", env)
	}
	assertD6Provenance(t, dm, decID, "decision", "inv_handler_1")

	// mpm_lessons.propose → the operator's promotion.
	//
	// The agent-reachable lesson WRITE is refused (a lesson is trusted
	// on arrival), so the lesson artifact is now created by the
	// promotion. The D6 bridge for a lesson is exercised through that
	// path: the proposal records the caller, and the promoted lesson's
	// artifact_provenance row carries the invocation id.
	res, err = handleProposeLesson(dm, ac, map[string]interface{}{
		"fact": "a lesson recorded through the tool surface",
		"type": "insight",
	})
	if err != nil {
		t.Fatalf("handleProposeLesson: %v", err)
	}
	env = res.(map[string]interface{})
	proposalID, _ := env["id"].(string)
	if proposalID == "" {
		t.Fatalf("no proposal id: %+v", env)
	}
	proposal, err := dm.GetLessonProposal(proposalID)
	if err != nil {
		t.Fatalf("GetLessonProposal: %v", err)
	}
	if proposal.FrameworkName != "opencode" {
		t.Errorf("the proposal did not record the caller's framework: %+v", proposal)
	}
	lesson, _, _, err := dm.PromoteLessonProposal(proposalID, ac)
	if err != nil {
		t.Fatalf("PromoteLessonProposal: %v", err)
	}
	assertD6Provenance(t, dm, lesson.ID, "lesson", "inv_handler_1")

	// mpm_theories.propose
	res, err = handleProposeTheory(dm, ac, map[string]interface{}{
		"hypothesis":          "a proposal made through the tool surface",
		"validation_criteria": "it can be checked",
	})
	if err != nil {
		t.Fatalf("handleProposeTheory: %v", err)
	}
	env = res.(map[string]interface{})
	theoryID, _ := env["id"].(string)
	if theoryID == "" {
		t.Fatalf("no theory id: %+v", env)
	}
	assertD6Provenance(t, dm, theoryID, "theory", "inv_handler_1")

	// mpm_handoff.write
	res, err = handleHandoffWrite(dm, ac, map[string]interface{}{
		"session_id": "ses_d6",
		"summary":    "a handoff written through the tool surface",
		"state":      internal.HandoffClean,
	})
	if err != nil {
		t.Fatalf("handleHandoffWrite: %v", err)
	}
	env = res.(map[string]interface{})
	handoffID, _ := env["handoff_id"].(string)
	if handoffID == "" {
		t.Fatalf("no handoff id: %+v", env)
	}
	assertD6Provenance(t, dm, handoffID, "handoff", "inv_handler_1")
}

// assertD6Provenance asserts that one artifact's provenance row carries the
// expected invocation id AND joins to a real tool_invocations row.
func assertD6Provenance(t *testing.T, dm *internal.DatabaseManager, artifactID, artifactType, wantInvocation string) {
	t.Helper()
	var inv string
	if err := dm.SQLDB().QueryRow(`
		SELECT COALESCE(invocation_id, '')
		FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = ?`,
		artifactID, artifactType).Scan(&inv); err != nil {
		t.Fatalf("read %s provenance: %v", artifactType, err)
	}
	if inv != wantInvocation {
		t.Errorf("%s provenance invocation_id = %q, want %q", artifactType, inv, wantInvocation)
	}
	var joined int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*)
		FROM artifact_provenance ap
		JOIN tool_invocations ti ON ti.invocation_id = ap.invocation_id
		WHERE ap.artifact_id = ? AND ap.artifact_type = ?`,
		artifactID, artifactType).Scan(&joined); err != nil {
		t.Fatalf("join %s provenance: %v", artifactType, err)
	}
	if joined != 1 {
		t.Errorf("%s provenance joined to %d tool_invocations rows, want 1", artifactType, joined)
	}
}

// ── 9. Lifecycle ingestion is not knowledge preservation ─────────────────

// TestD6_LifecycleIngestionIsNotKnowledgePreservation pins requirement 9.
//
// `mpm_system event_ingest` records a LIFECYCLE OBSERVATION. It is not a
// knowledge-producing action and it is not a continuity-writing action.
//
// The contract says so ("NOTHING IS PROMOTED. Capturing an observation
// creates no memory, no lesson, no decision and no theory"), and D6 must
// not change that: the correction threads invocation identity into
// RECORDING paths, and the lifecycle ingest path is not one of them.
//
// If the ingest path started writing an artifact_provenance row, a session
// that merely ingested observations would look like a session that
// preserved knowledge — which is exactly the inflation the missed-
// opportunity hunt must not suffer.
func TestD6_LifecycleIngestionIsNotKnowledgePreservation(t *testing.T) {
	dm := d6ToolTestDM(t)

	ac := internal.ActiveContext{
		SessionID:    "ses_d6",
		InvocationID: "inv_ingest_1",
	}
	d6SeedToolInvocation(t, dm, "inv_ingest_1", "mpm_system", "event_ingest")

	env := lifecycleToolEnvelope()
	res, err := handleMpmSystem(dm, ac, map[string]interface{}{
		"action": "event_ingest",
		"params": map[string]interface{}{
			"schema_version": internal.LifecycleEventSchemaVersion,
			"event":          env,
		},
	})
	if err != nil {
		t.Fatalf("event_ingest: %v", err)
	}
	// The ingest itself must have worked — a test that asserts "nothing
	// was recorded" would pass trivially if the ingest had failed.
	out, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("event_ingest result shape: %T", res)
	}
	if _, ok := out["receipt"]; !ok {
		t.Errorf("event_ingest produced no receipt: %+v", out)
	}

	// The observation IS stored — as a lifecycle observation.
	var lifecycleRows int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lifecycle_events`).Scan(&lifecycleRows); err != nil {
		t.Fatalf("count lifecycle_events: %v", err)
	}
	if lifecycleRows == 0 {
		t.Errorf("the lifecycle observation was not stored — the test would be vacuous")
	}

	// But NOTHING was promoted: no memory, no lesson, no decision, no
	// theory, no provenance row.
	//
	// The memories count excludes the seeded baseline directives: the
	// schema's InitSchema seeds five directive rows into the memories
	// table, and those are infrastructure, not something the ingest
	// created.
	for _, q := range []struct {
		label string
		sql   string
	}{
		{"memories", `SELECT COUNT(*) FROM memories WHERE collection <> 'directives'`},
		{"lessons", `SELECT COUNT(*) FROM lessons`},
		{"artifact_provenance", `SELECT COUNT(*) FROM artifact_provenance`},
		{"works", `SELECT COUNT(*) FROM works`},
	} {
		var n int
		if err := dm.SQLDB().QueryRow(q.sql).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.label, err)
		}
		if n != 0 {
			t.Errorf("%s rows = %d, want 0 — a lifecycle observation must not "+
				"masquerade as knowledge preservation", q.label, n)
		}
	}

	// The ingest must not be in the hunt's recording vocabulary. The
	// vocabulary lives in the critic module; the invariant it protects is
	// that an ingest-only session is still a candidate for the
	// missed-opportunity hunt. Asserting it here keeps the two halves of
	// the contract in one place.
	if d6IsRecordingAction("mpm_system", "event_ingest") {
		t.Errorf("mpm_system.event_ingest must not be a recording action")
	}
}

// d6IsRecordingAction mirrors the critic's recording-action vocabulary.
//
// The critic module is a separate Go module (the root module), so the
// vocabulary cannot be imported from here without a cycle. This mirror is
// the tool-surface half of the contract; the critic module pins its own
// copy in the hunt's own tests. If the vocabulary ever changes, BOTH must
// change together — and the critic-side test fails loudly when it does.
func d6IsRecordingAction(tool, action string) bool {
	vocabulary := map[string]map[string]bool{
		"mpm_memory":     {"save": true},
		"mpm_theories":   {"propose": true},
		"mpm_decisions":  {"record": true},
		"mpm_lessons":    {"save": true},
		"mpm_work":       {"create": true, "note": true},
		"mpm_skills":     {"save": true},
		"mpm_handoff":    {"write": true},
		"mpm_context":    {"write_handoff": true},
		"mpm_topics":     {"create": true},
		"mpm_references": {"add": true, "import": true},
		"mpm_evidence":   {"add": true},
	}
	actions, ok := vocabulary[tool]
	if !ok {
		return false
	}
	return actions[action]
}
