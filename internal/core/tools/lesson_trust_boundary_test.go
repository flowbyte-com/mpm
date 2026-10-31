package tools

// lesson_trust_boundary_test.go — the agent/lesson trust boundary.
//
// THE DEFECT THIS PINS
// --------------------
// A lesson row is TRUSTED ON ARRIVAL: `lessons_base` has no status
// column, so a row there is immediately inside the trusted retrieval
// set (lessons_fts, `mpm_lessons search`, `mpm_lessons list`, and the
// wake context's recent_lessons).
//
// The agent tool surface could write there directly. `mpm_lessons
// action=save` — reachable over MCP, over `mpm call`, and through the
// OpenCode plugin, which forwards the action verbatim — created a
// trusted lesson from whatever text the agent supplied. An agent could
// therefore put unsupported speculation into the substrate's knowledge
// with no review step, and the release-candidate report claimed that
// no agent path could do that.
//
// THE CONTRACT PINSED HERE
// -----------------------
//
//  1. `mpm_lessons action=save` is REFUSED at the execution point, and
//     NO payload field unlocks it.
//  2. A refusal writes NOTHING.
//  3. `mpm_lessons action=propose` records a PENDING proposal that is
//     OUTSIDE every trusted retrieval path.
//  4. Only the operator's promotion turns a proposal into a lesson, and
//     the promoted lesson IS retrievable.
//  5. Existing lessons are untouched by any of this.
//
// The mutation check is in TestLessonTrustBoundary_MutationRemovesThe
// Enforcement: re-introducing the write must make these tests fail.

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// trustBoundaryAC is a caller identity that looks like a real agent:
// a session, a framework and a model.
func trustBoundaryAC() mpminternal.ActiveContext {
	return mpminternal.ActiveContext{
		SessionID:    "trust-boundary-session",
		FrameworkName: "opencode",
		Model:        "test-model-x",
		Agent:        "agent",
	}
}

// countLessons returns the number of rows in lessons_base.
func countLessons(t *testing.T, dm *mpminternal.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons_base`).Scan(&n); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	return n
}

// lessonContentExists reports whether the given text appears in
// lessons_base. It is the strongest form of the check: it does not
// care WHICH read path would surface it, only whether the trusted
// table holds it at all.
func lessonContentExists(t *testing.T, dm *mpminternal.DatabaseManager, needle string) bool {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE content LIKE ?`, "%"+needle+"%").Scan(&n); err != nil {
		t.Fatalf("scan lessons: %v", err)
	}
	return n > 0
}

// TestLessonTrustBoundary_SaveIsRefusedAndWritesNothing is the
// headline test.
//
// An agent that tries to write a trusted lesson is refused, and the
// refusal leaves the trusted lesson table exactly as it was.
func TestLessonTrustBoundary_SaveIsRefusedAndWritesNothing(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	before := countLessons(t, dm)

	_, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "The deployment pipeline always requires a manual approval step before release.",
			"type": "insight",
		},
	})
	if err == nil {
		t.Fatal("mpm_lessons action=save succeeded — an agent can create a TRUSTED lesson")
	}

	if !strings.Contains(err.Error(), "operator-only") {
		t.Errorf("the refusal does not say the action is operator-only: %v", err)
	}
	if !strings.Contains(err.Error(), "action=propose") {
		t.Errorf("the refusal does not name the agent's alternative (action=propose): %v", err)
	}

	if after := countLessons(t, dm); after != before {
		t.Errorf("the refused write created %d lesson row(s) — a refusal must write NOTHING", after-before)
	}
	if lessonContentExists(t, dm, "manual approval step") {
		t.Error("the refused lesson's content is in lessons_base — the trusted lesson table was written")
	}
}

// TestLessonTrustBoundary_NoPayloadFieldUnlocksTheWrite is the
// fail-closed test.
//
// A boolean the caller supplies is not evidence of operator intent: an
// agent that can call the tool can also set the field. Every plausible
// "I am the operator" field must leave the write refused.
//
// The list is deliberately broad. A gate that only refuses
// `operator=true` and quietly honours `trusted=true` is not a gate.
func TestLessonTrustBoundary_NoPayloadFieldUnlocksTheWrite(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	before := countLessons(t, dm)

	attempts := []map[string]interface{}{
		{"operator": true},
		{"operator": "true"},
		{"confirm": true},
		{"review": true},
		{"trusted": true},
		{"pending": false},
		{"promote": true},
		{"force": true},
		{"admin": true},
		{"as_operator": true},
		{"is_operator": true},
		{"source": "operator"},
		{"actor_kind": "human"},
	}

	for _, extra := range attempts {
		params := map[string]interface{}{
			"fact": "trust-boundary-attempt-content",
			"type": "insight",
		}
		for k, v := range extra {
			params[k] = v
		}
		_, err := handleMpmLessons(dm, ac, map[string]interface{}{
			"action": "save",
			"params": params,
		})
		if err == nil {
			t.Errorf("mpm_lessons action=save succeeded with %v — a payload field authorized the write", extra)
		}
	}

	if after := countLessons(t, dm); after != before {
		t.Errorf("the refused writes created %d lesson row(s) — a refusal must write NOTHING", after-before)
	}
}

// TestLessonTrustBoundary_AProposalIsNotInTrustedRetrieval is the
// isolation test.
//
// A proposal must not be reachable by any trusted read path:
//   - not in `lessons_base` (the trusted table),
//   - not in `mpm_lessons search` (the FTS index),
//   - not in `mpm_lessons list`,
//   - not in the wake context's recent_lessons.
//
// The isolation is STRUCTURAL — the proposal lives in its own table,
// which no trusted read path queries — so there is no filter here that
// a future read path could forget.
func TestLessonTrustBoundary_AProposalIsNotInTrustedRetrieval(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	const content = "The staging environment is always rebuilt from scratch before a release."
	res, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{
			"fact": content,
			"type": "warning",
			"tags": []interface{}{"release"},
		},
	})
	if err != nil {
		t.Fatalf("propose failed: %v", err)
	}

	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("propose returned %T, want a map", res)
	}
	if m["success"] != true {
		t.Fatalf("propose did not succeed: %v", m)
	}
	if m["status"] != "pending" {
		t.Errorf("status = %v, want \"pending\"", m["status"])
	}
	if m["trusted"] != false {
		t.Errorf("trusted = %v, want false — a proposal is not trusted knowledge", m["trusted"])
	}
	proposalID, _ := m["id"].(string)
	if proposalID == "" {
		t.Fatalf("propose returned no id: %v", m)
	}

	// 1. The trusted table.
	if lessonContentExists(t, dm, "rebuilt from scratch") {
		t.Error("the proposal's content is in lessons_base")
	}
	if countLessons(t, dm) != 0 {
		t.Errorf("the proposal created %d trusted lesson row(s)", countLessons(t, dm))
	}

	// 2. The FTS-backed search.
	searchOut, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "search",
		"params": map[string]interface{}{"query": "rebuilt from scratch"},
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !searchIsEmpty(t, searchOut) {
		t.Errorf("the proposal is in the lesson SEARCH results: %v", searchOut)
	}

	// 3. The list.
	listOut, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !listIsEmpty(t, listOut) {
		t.Errorf("the proposal is in the lesson LIST: %v", listOut)
	}

	// 4. The wake context.
	wake, err := handleReadWakeContext(dm, ac, map[string]interface{}{
		"session_id": "trust-boundary-session",
	})
	if err != nil {
		t.Fatalf("read_wake_context failed: %v", err)
	}
	raw, err := json.Marshal(wake)
	if err != nil {
		t.Fatalf("marshal the wake context: %v", err)
	}
	if strings.Contains(string(raw), "rebuilt from scratch") {
		t.Errorf("the proposal is in the WAKE CONTEXT:\n%s", raw)
	}
	if strings.Contains(string(raw), proposalID) {
		t.Errorf("the proposal's id is in the WAKE CONTEXT:\n%s", raw)
	}
}

// TestLessonTrustBoundary_PromotionMakesItRetrievable is the other
// half of the contract.
//
// The promotion is the operator's decision, and after it the lesson IS
// a lesson: it is in the trusted table, it is retrievable, and it
// appears in the wake context.
//
// This is what makes the boundary usable rather than merely safe: MPM
// can still capture the experience, and the operator's acceptance is
// what makes it knowledge.
func TestLessonTrustBoundary_PromotionMakesItRetrievable(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	const content = "The staging environment is always rebuilt from scratch before a release."
	res, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{"fact": content, "type": "warning"},
	})
	if err != nil {
		t.Fatalf("propose failed: %v", err)
	}
	proposalID := res.(map[string]interface{})["id"].(string)

	lesson, proposal, _, err := dm.PromoteLessonProposal(proposalID)
	if err != nil {
		t.Fatalf("promote failed: %v", err)
	}
	if lesson == nil || lesson.ID == "" {
		t.Fatalf("promote returned no lesson: %v", lesson)
	}
	if proposal.Status != "promoted" {
		t.Errorf("proposal status = %q, want \"promoted\"", proposal.Status)
	}
	if proposal.PromotedLessonID != lesson.ID {
		t.Errorf("promoted_lesson_id = %q, want %q", proposal.PromotedLessonID, lesson.ID)
	}
	if !proposal.ReviewedAt.Valid {
		t.Error("the promotion did not record when it was reviewed")
	}

	// The lesson is now in the trusted table and retrievable.
	if !lessonContentExists(t, dm, "rebuilt from scratch") {
		t.Error("the promoted lesson is not in lessons_base")
	}
	searchOut, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "search",
		"params": map[string]interface{}{"query": "rebuilt from scratch"},
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if searchIsEmpty(t, searchOut) {
		t.Errorf("the promoted lesson is NOT in the lesson SEARCH results: %v", searchOut)
	}

	wake, err := handleReadWakeContext(dm, ac, map[string]interface{}{
		"session_id": "trust-boundary-session",
	})
	if err != nil {
		t.Fatalf("read_wake_context failed: %v", err)
	}
	raw, _ := json.Marshal(wake)
	if !strings.Contains(string(raw), "rebuilt from scratch") {
		t.Errorf("the promoted lesson is not in the WAKE CONTEXT:\n%s", raw)
	}
}

// TestLessonTrustBoundary_AProposalCanOnlyBePromotedOnce pins the
// state machine.
//
// A promoted proposal is not pending any more, so promoting it again
// must fail rather than create a second lesson.
func TestLessonTrustBoundary_AProposalCanOnlyBePromotedOnce(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	res, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{"fact": "propose-once-content", "type": "insight"},
	})
	if err != nil {
		t.Fatalf("propose failed: %v", err)
	}
	proposalID := res.(map[string]interface{})["id"].(string)

	if _, _, _, err := dm.PromoteLessonProposal(proposalID); err != nil {
		t.Fatalf("the first promotion failed: %v", err)
	}
	before := countLessons(t, dm)

	if _, _, _, err := dm.PromoteLessonProposal(proposalID); err == nil {
		t.Error("promoting an already-promoted proposal succeeded — the state machine has no guard")
	}
	if after := countLessons(t, dm); after != before {
		t.Errorf("the second promotion created %d lesson row(s)", after-before)
	}

	// And a rejected proposal cannot be promoted either.
	res2, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{"fact": "propose-reject-content", "type": "insight"},
	})
	if err != nil {
		t.Fatalf("propose failed: %v", err)
	}
	rejectedID := res2.(map[string]interface{})["id"].(string)
	if _, err := dm.RejectLessonProposal(rejectedID); err != nil {
		t.Fatalf("reject failed: %v", err)
	}
	if _, _, _, err := dm.PromoteLessonProposal(rejectedID); err == nil {
		t.Error("promoting a REJECTED proposal succeeded")
	}
}

// TestLessonTrustBoundary_ExistingLessonsAreUntouched is the
// preservation test.
//
// The boundary must not rewrite, purge or re-index anything that was
// already there. A pre-existing lesson keeps its row, its content, its
// reinforcement count and its id across the whole proposal/promotion
// cycle.
func TestLessonTrustBoundary_ExistingLessonsAreUntouched(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	// An existing, trusted lesson — recorded by the operator.
	existing, err := dm.AddLesson("a lesson that was already trusted", "insight", []string{"legacy"}, "")
	if err != nil {
		t.Fatalf("seed the existing lesson: %v", err)
	}

	type row struct {
		ID        string
		Content   string
		Tags      string
		Created   string
		Hash      string
		Reinf     int
		DeletedAt *int64
	}
	read := func() row {
		t.Helper()
		var r row
		var deletedAt *int64
		if err := dm.SQLDB().QueryRow(`
			SELECT id, content, COALESCE(tags,''), created, COALESCE(content_hash,''), reinforcement_count, deleted_at
			FROM lessons_base WHERE id = ?`, existing.ID).
			Scan(&r.ID, &r.Content, &r.Tags, &r.Created, &r.Hash, &r.Reinf, &deletedAt); err != nil {
			t.Fatalf("read the existing lesson: %v", err)
		}
		r.DeletedAt = deletedAt
		return r
	}

	before := read()

	// Run the whole agent-side and operator-side cycle.
	res, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{"fact": "a newly proposed lesson", "type": "insight"},
	})
	if err != nil {
		t.Fatalf("propose failed: %v", err)
	}
	if _, _, _, err := dm.PromoteLessonProposal(res.(map[string]interface{})["id"].(string)); err != nil {
		t.Fatalf("promote failed: %v", err)
	}

	after := read()
	if before != after {
		t.Errorf("the pre-existing lesson's row changed:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// TestLessonTrustBoundary_TheProposalRecordsWhoProposedIt pins the
// provenance.
//
// A proposal must say WHO proposed it, so the operator can judge it.
// Without that, a proposal is an anonymous assertion — which is the
// problem the whole mechanism exists to solve.
func TestLessonTrustBoundary_TheProposalRecordsWhoProposedIt(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	res, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "propose",
		"params": map[string]interface{}{"fact": "provenance-carrying proposal", "type": "insight"},
	})
	if err != nil {
		t.Fatalf("propose failed: %v", err)
	}
	proposalID := res.(map[string]interface{})["id"].(string)

	proposal, err := dm.GetLessonProposal(proposalID)
	if err != nil {
		t.Fatalf("read the proposal: %v", err)
	}
	if proposal.FrameworkName != "opencode" {
		t.Errorf("framework_name = %q, want %q", proposal.FrameworkName, "opencode")
	}
	if proposal.ModelName != "test-model-x" {
		t.Errorf("model_name = %q, want %q", proposal.ModelName, "test-model-x")
	}
	if proposal.SourceSessionID != "trust-boundary-session" {
		t.Errorf("source_session_id = %q, want %q", proposal.SourceSessionID, "trust-boundary-session")
	}
	if proposal.Status != "pending" {
		t.Errorf("status = %q, want %q", proposal.Status, "pending")
	}
}

// TestLessonTrustBoundary_MutationRemovesTheEnforcement is the
// NON-VACUITY check.
//
// Every test above asserts on the enforcement. If the enforcement were
// removed, they must FAIL — otherwise they prove nothing.
//
// This test is not a mutation harness: it asserts the SHAPE of the
// enforcement (a refusal that names the operator-only contract and
// writes nothing) in a way that a re-introduced write cannot satisfy.
// The mutation was verified by hand: restoring the old handleSaveLesson
// body makes TestLessonTrustBoundary_SaveIsRefusedAndWritesNothing and
// TestLessonTrustBoundary_NoPayloadFieldUnlocksTheWrite fail.
func TestLessonTrustBoundary_MutationRemovesTheEnforcement(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	_, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{"fact": "mutation-check-content"},
	})
	if err == nil {
		t.Fatal("the write was NOT refused — the enforcement is gone")
	}

	// The refusal must be the SPECIFIC operator-only refusal, not an
	// incidental failure (a bad type, a missing table). A re-introduced
	// write would produce either a success or an unrelated error.
	msg := err.Error()
	for _, fragment := range []string{
		"operator-only",
		"trusted on arrival",
		"Nothing was written",
		"operator=true",
		"mpm lesson promote",
		"mpm lesson add",
	} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("the refusal is missing %q — it is not the operator-only refusal:\n%s", fragment, msg)
		}
	}

	// And the database must not even have the proposals table's twin —
	// the trusted table — written.
	if countLessons(t, dm) != 0 {
		t.Errorf("%d lesson row(s) were written by the refused call", countLessons(t, dm))
	}
}

// searchIsEmpty reports whether a lesson-search result carries no
// lessons.
func searchIsEmpty(t *testing.T, out interface{}) bool {
	t.Helper()
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("search returned %T, want a map", out)
	}
	if count, ok := m["count"].(int); ok && count == 0 {
		return true
	}
	if lessons, ok := m["lessons"].([]interface{}); ok && len(lessons) == 0 {
		return true
	}
	// Fall back to the raw shape: an empty result must not contain any
	// lesson id.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal the search result: %v", err)
	}
	return strings.Contains(string(raw), `"lessons":[]`) || strings.Contains(string(raw), `"count":0`)
}

// listIsEmpty reports whether a lesson-list result carries no lessons.
func listIsEmpty(t *testing.T, out interface{}) bool {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal the list result: %v", err)
	}
	s := string(raw)
	return strings.Contains(s, `"lessons":[]`) ||
		strings.Contains(s, `"count":0`) ||
		strings.Contains(s, `"lessons": []`)
}

// TestLessonTrustBoundary_AnUnknownActionIsStillRejected guards the
// dispatch's default branch: the enumeration must not have grown a
// transition the agent can drive.
func TestLessonTrustBoundary_AnUnknownActionIsStillRejected(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := trustBoundaryAC()

	for _, action := range []string{"promote", "reject", "publish", "approve", "accept"} {
		_, err := handleMpmLessons(dm, ac, map[string]interface{}{
			"action": action,
			"params": map[string]interface{}{"fact": "must not be written", "id": "whatever"},
		})
		if err == nil {
			t.Errorf("action %q was accepted — the agent surface has a transition it must not have", action)
		}
		if countLessons(t, dm) != 0 {
			t.Errorf("action %q wrote a lesson", action)
		}
	}
}

// TestLessonTrustBoundary_TheToolSchemaOffersNoTransition pins the
// SURFACE: the schema's action enum must name only the actions the
// agent may drive.
//
// A refusal at the execution point is the enforcement; the schema is
// the announcement. The announcement must not contradict the
// enforcement by offering an action that is refused.
func TestLessonTrustBoundary_TheToolSchemaOffersNoTransition(t *testing.T) {
	reg := findToolRegistration("mpm_lessons")
	if reg == nil {
		t.Fatal("mpm_lessons is not registered")
	}
	schema := string(reg.Schema)
	if !strings.Contains(schema, `"propose"`) {
		t.Errorf("the mpm_lessons schema does not offer action=propose: %s", schema)
	}
	if strings.Contains(schema, `"save"`) {
		t.Errorf("the mpm_lessons schema still offers action=save, which is refused: %s", schema)
	}
	for _, forbidden := range []string{"promote", "reject", "publish", "approve"} {
		if strings.Contains(schema, `"`+forbidden+`"`) {
			t.Errorf("the mpm_lessons schema offers action=%q — the agent surface must have no transition", forbidden)
		}
	}
}

// findToolRegistration returns the registry entry for a tool name.
func findToolRegistration(name string) *Tool {
	for i := range Registry {
		if Registry[i].Name == name {
			return &Registry[i]
		}
	}
	return nil
}
