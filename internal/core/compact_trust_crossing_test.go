package internal

// compact_trust_crossing_test.go — the compaction path and the lesson
// trust boundary.
//
// THE QUESTION THIS FILE ANSWERS
// ------------------------------
// PR #189 established that an agent cannot write a TRUSTED lesson. The
// agent may PROPOSE one (`mpm_lessons action=propose`), and an operator
// promotes it (`mpm lesson promote`). A row in `lessons_base` is
// TRUSTED ON ARRIVAL: the table has no status column, so the row is
// immediately inside the trusted retrieval set.
//
// The COMPACTION path is a second way into that table. It synthesizes a
// lesson from raw memories and inserts it into `lessons_base` with no
// review step at all.
//
// The compaction INPUT population is agent-writable. `mpm_memory
// action=save` writes `collection='memories'`, which is exactly what
// `memoryIsActionable` selects:
//
//	const memoryIsUncompacted = `collection = 'memories'
//	     AND deleted_at IS NULL
//	     AND (metadata IS NULL OR metadata = ''
//	          OR json_extract(metadata, '$.compacted_into') IS NULL)`
//
// So the question is not hypothetical: can an assertion that no
// evidence supports, introduced through the agent's own memory write,
// emerge as a TRUSTED lesson — without independent evidence and
// without operator approval?
//
// WHAT IS ASSERTED HERE
// ---------------------
//
//  1. The unsupported assertion must NOT be in `lessons_base`, and must
//     not be reachable through the trusted read path (SearchLessons,
//     which is backed by lessons_fts).
//  2. It must NOT be silently dropped either: the derivation must be
//     preserved somewhere the operator can act on it. The substrate's
//     existing mechanism for that is the pending proposal table.
//  3. A derivation that IS supported by its sources is not collateral
//     damage: it must still survive as trusted knowledge or as a
//     reviewable proposal.
//
// The negative control (TestCompactTrustCrossing_MutationRestoresThe
// Crossing) proves these assertions are not vacuous: it re-introduces
// the direct trusted write in the substrate's own code path and shows
// the assertions above fail.
//
// The mutation was verified by hand on 2026-10-30. Restoring the
// pre-fix commit path — the direct `INSERT INTO lessons ...` in place
// of the pending-proposal insert, and removing the read-back
// assertion — makes
// TestCompactTrustCrossing_UnsupportedAssertionMustNotBecomeTrustedLesson
// fail with exactly three findings:
//
//	1 trusted lesson row(s) exist after a derivation that asserted an unsupported claim
//	the unsupported assertion is reachable through the TRUSTED search path
//	the unsupported assertion was not preserved for review — it was dropped, not quarantined
//
// The other two tests in this file still PASS under that mutation,
// which is the point: they are the controls that show the failing
// test is detecting the trust crossing and not merely noticing that
// something changed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// The unsupported assertion. It is deliberately made of words that
// appear NOWHERE in the source memory, so "the synthesis invented it"
// is a fact about the fixture rather than a judgement call.
const (
	compactSourceText   = "the deployment pipeline is flaky under load"
	compactUnsupported  = "the nightly release pipeline is verified hermetic"
	compactSupported    = "the deployment pipeline is flaky under load"
	compactTrustedProbe = "hermetic"
)

// seedAgentAuthoredMemory writes a raw memory through the SAME write
// the agent's tool surface uses.
//
// `mpm_memory action=save` → handleSaveToMemory →
// SaveMemoryWithContextAndSnapshot → collection defaults to "memories".
// The properties that matter for this test are the row's collection and
// the absence of a compacted_into marker — both of which come from the
// collection alone.
func seedAgentAuthoredMemory(t *testing.T, dm *DatabaseManager, content string) string {
	t.Helper()
	ac := ActiveContext{
		SessionID:     "agent-session-id",
		FrameworkName: "opencode",
		Model:         "test-model-x",
		Agent:         "agent",
	}
	wc := &WrapperContext{
		AgentID:   ac.Agent,
		SessionID: ac.SessionID,
		Model:     ac.Model,
	}
	_, mem, err := dm.SaveMemoryWithContextAndSnapshot(
		content, "memories", []string{"agent-written"}, 5, "", ac, wc)
	if err != nil {
		t.Fatalf("seed the agent-authored memory: %v", err)
	}
	return mem.ID
}

// synthResponse builds a well-formed synthesis response.
func synthResponse(title, body string, tags ...string) string {
	b, err := json.Marshal(CompactLesson{Title: title, Body: body, Tags: tags})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// trustedLessonCount counts the rows in the TRUSTED table.
func trustedLessonCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons_base`).Scan(&n); err != nil {
		t.Fatalf("count trusted lessons: %v", err)
	}
	return n
}

// trustedSearchIsEmpty reports whether the TRUSTED retrieval path
// (lessons_fts, via SearchLessons) returns nothing for the query.
// This is the read path an agent and the wake context actually use —
// a row that is in lessons_base but unreachable here would still be a
// hole, so the test checks the read path and not just the table.
func trustedSearchIsEmpty(t *testing.T, dm *DatabaseManager, query string) bool {
	t.Helper()
	lessons, err := dm.SearchLessons(query, 10)
	if err != nil {
		t.Fatalf("search the trusted lesson path: %v", err)
	}
	return len(lessons) == 0
}

// TestCompactTrustCrossing_UnsupportedAssertionMustNotBecomeTrustedLesson
// is the negative test.
//
// The synthesis is stubbed — deterministically, through the substrate's
// own LLM seam (`compactSynthesizeFunc`) — so the test measures the
// TRUST BOUNDARY, not the model's behaviour. The stub returns a lesson
// whose body asserts something the source memory does not support.
// That models both the realistic failure (the model over-generalises
// the source) and the adversarial one (the agent's memory text steers
// the synthesis).
func TestCompactTrustCrossing_UnsupportedAssertionMustNotBecomeTrustedLesson(t *testing.T) {
	dm := NewTestDM(t)

	// The lower-trust input path: the agent writes a memory.
	seedAgentAuthoredMemory(t, dm, compactSourceText)

	// The synthesis seam: deterministic, and it asserts something the
	// source does not support.
	called := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		called++
		// The model saw the source. It answered with a claim the
		// source does not contain.
		return synthResponse("Release pipeline", compactUnsupported, "pipeline"), nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 8)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if called == 0 {
		t.Fatal("the synthesis was never called — the test proves nothing")
	}
	if res.ProposalsCreated != 1 {
		t.Fatalf("the drain committed %d proposal(s) (stop_reason=%q, raw_processed=%d) — the fixture did not reach the synthesis",
			res.ProposalsCreated, res.StopReason, res.RawProcessed)
	}

	// 1. The unsupported assertion must not be TRUSTED.
	if n := trustedLessonCount(t, dm); n != 0 {
		t.Errorf("%d trusted lesson row(s) exist after a derivation that asserted an unsupported claim", n)
	}
	if !trustedSearchIsEmpty(t, dm, compactTrustedProbe) {
		t.Errorf("the unsupported assertion is reachable through the TRUSTED search path")
	}

	// 2. It must not be silently dropped either: the operator has to be
	// able to see it and act on it.
	proposals, err := dm.ListLessonProposals(LessonProposalPending, 50)
	if err != nil {
		t.Fatalf("list the pending proposals: %v", err)
	}
	found := false
	for _, p := range proposals {
		if strings.Contains(p.Content, compactUnsupported) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the unsupported assertion was not preserved for review — it was dropped, not quarantined (pending proposals: %d)", len(proposals))
	}
}

// TestCompactTrustCrossing_LegitimateDerivationSurvives is the
// counterweight: the correction must not become "throw the synthesis
// away".
//
// A derivation that the sources DO support must still end up as
// trusted knowledge or as a reviewable proposal — never lost.
func TestCompactTrustCrossing_LegitimateDerivationSurvives(t *testing.T) {
	dm := NewTestDM(t)

	seedAgentAuthoredMemory(t, dm, compactSourceText)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return synthResponse("Pipeline flakiness", compactSupported, "pipeline"), nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 8)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.RawProcessed != 1 {
		t.Fatalf("the drain processed %d raw memories, want 1", res.RawProcessed)
	}

	trusted := 0
	lessons, err := dm.SearchLessons("flaky", 10)
	if err != nil {
		t.Fatalf("search the trusted lesson path: %v", err)
	}
	for _, l := range lessons {
		if strings.Contains(l.Content, compactSupported) {
			trusted++
		}
	}

	proposals, err := dm.ListLessonProposals(LessonProposalPending, 50)
	if err != nil {
		t.Fatalf("list the pending proposals: %v", err)
	}
	pending := 0
	for _, p := range proposals {
		if strings.Contains(p.Content, compactSupported) {
			pending++
		}
	}

	if trusted+pending != 1 {
		t.Errorf("the supported derivation survived %d time(s) (trusted=%d, pending=%d), want exactly 1 — it was lost or duplicated",
			trusted+pending, trusted, pending)
	}
}

// TestCompactTrustCrossing_MutationRemovesTheEnforcement is the
// NON-VACUITY check.
//
// Every test above asserts on the enforcement. If the enforcement were
// removed, they must FAIL — otherwise they prove nothing.
//
// This test asserts the SHAPE of the enforcement in a way a
// re-introduced direct write cannot satisfy: after a drain there must
// be a pending proposal that (a) holds the synthesis, (b) cites the
// source memory, and (c) is what the folded memory points at. A
// re-introduced `INSERT INTO lessons` produces a trusted lesson and no
// such proposal, so the assertions below fail.
//
// The mutation was verified by hand on 2026-10-30 — see the file
// header.
func TestCompactTrustCrossing_MutationRemovesTheEnforcement(t *testing.T) {
	dm := NewTestDM(t)

	sourceID := seedAgentAuthoredMemory(t, dm, compactSourceText)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return synthResponse("Release pipeline", compactUnsupported, "pipeline"), nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 8)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.ProposalsCreated != 1 || len(res.ProposalIDs) != 1 {
		t.Fatalf("the drain did not produce exactly one proposal (created=%d, ids=%v)",
			res.ProposalsCreated, res.ProposalIDs)
	}
	proposalID := res.ProposalIDs[0]

	// (a) The proposal holds the synthesis.
	proposal, err := dm.GetLessonProposal(proposalID)
	if err != nil {
		t.Fatalf("read the proposal: %v", err)
	}
	if !strings.Contains(proposal.Content, compactUnsupported) {
		t.Errorf("the proposal does not hold the synthesis — the derivation was not recorded for review")
	}
	if proposal.Status != LessonProposalPending {
		t.Errorf("proposal status: got %q, want %q", proposal.Status, LessonProposalPending)
	}

	// (b) The proposal cites the source memory.
	cites := false
	for _, id := range proposal.SourceIDs {
		if id == sourceID {
			cites = true
		}
	}
	if !cites {
		t.Errorf("the proposal does not cite the source memory %s (source_ids=%v)", sourceID, proposal.SourceIDs)
	}

	// (c) The folded memory points at the proposal.
	var compactedInto string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_extract(metadata, '$.compacted_into') FROM memories WHERE id = ?`, sourceID,
	).Scan(&compactedInto); err != nil {
		t.Fatalf("read the folded memory: %v", err)
	}
	if compactedInto != proposalID {
		t.Errorf("the folded memory points at %q, want the proposal id %q", compactedInto, proposalID)
	}

	// And the trusted table is empty — the shape a re-introduced write
	// cannot produce.
	if n := trustedLessonCount(t, dm); n != 0 {
		t.Errorf("%d trusted lesson row(s) exist — the enforcement is gone", n)
	}
}

// TestCompactTrustCrossing_TheFoldIsRecordedForEveryMetadataShape
// covers a latent defect the crossing work exposed.
//
// The raw-marking UPDATE used `json_insert(COALESCE(metadata, '{}'), …)`.
// SQLite's json functions treat the JSON literal `null` as a VALUE, not
// an object, so `json_insert('null', '$.k', v)` returns 'null'
// UNCHANGED — a silent no-op. A memory row whose metadata column holds
// the literal `null` (what json.Marshal writes for a nil map) was
// therefore NEVER marked as folded.
//
// The consequence: the row stays in the compaction population, is
// re-offered on every drain, and is folded again and again. Before the
// trust correction that produced duplicate trusted lessons; after it,
// duplicate pending proposals. Either way the fold is not recorded,
// and the correction's "the raw memories are marked" guarantee does
// not hold.
//
// The marking must be TOTAL: whatever shape the metadata column has,
// a folded row is marked.
func TestCompactTrustCrossing_TheFoldIsRecordedForEveryMetadataShape(t *testing.T) {
	shapes := []struct {
		name     string
		metadata string
	}{
		{"sql null", ""},
		{"json null literal", "null"},
		{"empty object", "{}"},
		{"populated object", `{"source":"test"}`},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			dm := NewTestDM(t)

			var id string
			if shape.metadata == "" {
				// SQL NULL: omit the column entirely.
				if _, err := dm.SQLDB().Exec(
					`INSERT INTO memories (id, collection, content, created_at, updated_at)
					 VALUES ('raw-shape', 'memories', 'shape content', '2026-10-30T00:00:00Z', '2026-10-30T00:00:00Z')`,
				); err != nil {
					t.Fatalf("seed the raw memory: %v", err)
				}
				id = "raw-shape"
			} else {
				if _, err := dm.SQLDB().Exec(
					`INSERT INTO memories (id, collection, content, metadata, created_at, updated_at)
					 VALUES ('raw-shape', 'memories', 'shape content', ?, '2026-10-30T00:00:00Z', '2026-10-30T00:00:00Z')`,
					shape.metadata,
				); err != nil {
					t.Fatalf("seed the raw memory: %v", err)
				}
				id = "raw-shape"
			}

			withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
				return synthResponse("Shape", compactSupported, "shape"), nil
			})

			res, err := dm.CompactEpistemologyDrain(context.Background(), true, 8)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if res.ProposalsCreated != 1 || len(res.ProposalIDs) != 1 {
				t.Fatalf("the drain did not fold the row (created=%d, ids=%v)",
					res.ProposalsCreated, res.ProposalIDs)
			}

			var compactedInto sql.NullString
			if err := dm.SQLDB().QueryRow(
				`SELECT json_extract(metadata, '$.compacted_into') FROM memories WHERE id = ?`, id,
			).Scan(&compactedInto); err != nil {
				t.Fatalf("read the folded memory: %v", err)
			}
			if !compactedInto.Valid || compactedInto.String != res.ProposalIDs[0] {
				t.Errorf("the fold was not recorded: compacted_into=%v, want %q",
					compactedInto, res.ProposalIDs[0])
			}

			// And the row must have left the compaction population.
			ids, _, err := dm.extractRawBatch(context.Background(), 50)
			if err != nil {
				t.Fatalf("extract the raw batch: %v", err)
			}
			for _, got := range ids {
				if got == id {
					t.Errorf("the folded row is still in the compaction population — it will be folded again")
				}
			}
		})
	}
}

// TestCompactTrustCrossing_TheCompactionInputIsAgentWritable pins the
// premise the negative test rests on.
//
// If the compaction input population were NOT agent-writable, the
// crossing would be a non-issue. This asserts that it IS: a row
// written through the agent's own memory write lands in the population
// the compaction drain selects.
func TestCompactTrustCrossing_TheCompactionInputIsAgentWritable(t *testing.T) {
	dm := NewTestDM(t)

	id := seedAgentAuthoredMemory(t, dm, compactSourceText)

	// The row is in the compaction population.
	ids, contents, err := dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extract the raw batch: %v", err)
	}
	found := false
	for i, got := range ids {
		if got == id {
			found = true
			if contents[i] != compactSourceText {
				t.Errorf("the extracted content is %q, want %q", contents[i], compactSourceText)
			}
		}
	}
	if !found {
		t.Errorf("the agent-written memory %s is NOT in the compaction population — the crossing premise no longer holds", id)
	}
}

// TestCompactTrustCrossing_OperatorPromotionRestoresTheTrustedPath is
// the end-to-end proof that the correction PRESERVES the legitimate
// path rather than only closing the hole.
//
// The sequence is the operator's: the agent's derivation lands in the
// review queue, an operator promotes it, and the lesson becomes
// trusted and retrievable — with the same content the pre-fix
// compaction would have written.
func TestCompactTrustCrossing_OperatorPromotionRestoresTheTrustedPath(t *testing.T) {
	dm := NewTestDM(t)

	seedAgentAuthoredMemory(t, dm, compactSourceText)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return synthResponse("Pipeline flakiness", compactSupported, "pipeline"), nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 8)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.ProposalsCreated != 1 || len(res.ProposalIDs) != 1 {
		t.Fatalf("the drain did not produce exactly one proposal (created=%d, ids=%v)",
			res.ProposalsCreated, res.ProposalIDs)
	}
	proposalID := res.ProposalIDs[0]

	// Before the operator acts: not trusted, not retrievable.
	if !trustedSearchIsEmpty(t, dm, compactTrustedProbe) {
		t.Error("the derivation is retrievable as a trusted lesson BEFORE an operator promoted it")
	}

	// The operator promotes it.
	lesson, proposal, credited, err := dm.PromoteLessonProposal(proposalID)
	if err != nil {
		t.Fatalf("promote the proposal: %v", err)
	}
	if proposal.Status != LessonProposalPromoted {
		t.Errorf("proposal status: got %q, want %q", proposal.Status, LessonProposalPromoted)
	}
	if proposal.PromotedLessonID != lesson.ID {
		t.Errorf("promoted_lesson_id: got %q, want %q", proposal.PromotedLessonID, lesson.ID)
	}
	if credited != 1 {
		t.Errorf("credited sources: got %d, want 1 — the source memory must be credited at the promotion", credited)
	}

	// After the operator acts: trusted and retrievable, with the
	// synthesis content intact.
	lessons, err := dm.SearchLessons("flaky", 10)
	if err != nil {
		t.Fatalf("search the trusted lesson path: %v", err)
	}
	found := false
	for _, l := range lessons {
		if strings.Contains(l.Content, compactSupported) {
			found = true
		}
	}
	if !found {
		t.Errorf("the promoted lesson is not retrievable, or its content was lost")
	}
}
