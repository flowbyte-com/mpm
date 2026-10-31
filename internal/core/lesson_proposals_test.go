package internal

// lesson_proposals_test.go — the proposal store's contract.
//
// The store is the mechanism the trust boundary rests on, so its
// properties are asserted here rather than only through the tool
// surface:
//
//   - a proposal starts PENDING and stays pending until an operator
//     acts;
//   - the lifecycle has exactly three states, and the storage layer
//     rejects a fourth;
//   - a proposal's content is scanned exactly as a lesson's is;
//   - the proposer is recorded;
//   - a promotion is safe to retry (the content-hash dedup means a
//     retry reinforces rather than duplicates);
//   - a rejected proposal keeps its record.

import (
	"database/sql"
	"testing"
)

func newProposalTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	return NewTestDM(t)
}

// TestLessonProposal_Lifecycle pins the state machine.
func TestLessonProposal_Lifecycle(t *testing.T) {
	dm := newProposalTestDM(t)

	p, err := dm.AddLessonProposal("a proposed lesson", "warning", []string{"alpha"}, nil, ActiveContext{
		SessionID:    "proposal-session",
		FrameworkName: "opencode",
		Model:        "test-model-x",
	})
	if err != nil {
		t.Fatalf("AddLessonProposal: %v", err)
	}
	if p.Status != LessonProposalPending {
		t.Errorf("status = %q, want %q", p.Status, LessonProposalPending)
	}
	if p.PromotedLessonID != "" {
		t.Errorf("promoted_lesson_id = %q, want empty", p.PromotedLessonID)
	}
	if p.ReviewedAt.Valid {
		t.Error("reviewed_at is set on a proposal nobody has reviewed")
	}
	if p.FrameworkName != "opencode" || p.ModelName != "test-model-x" || p.SourceSessionID != "proposal-session" {
		t.Errorf("the proposer was not recorded: %+v", p)
	}
	if len(p.Tags) != 1 || p.Tags[0] != "alpha" {
		t.Errorf("tags = %v, want [alpha]", p.Tags)
	}

	// The status CHECK constraint is the storage-layer backstop: a
	// fourth state cannot be written.
	if _, err := dm.db.Exec(
		`UPDATE lesson_proposals SET status = 'approved' WHERE id = ?`, p.ID); err == nil {
		t.Error("a proposal was moved to a state outside the lifecycle — the CHECK constraint is not doing its job")
	}

	// Promotion.
	lesson, promoted, _, err := dm.PromoteLessonProposal(p.ID)
	if err != nil {
		t.Fatalf("PromoteLessonProposal: %v", err)
	}
	if lesson == nil || lesson.ID == "" {
		t.Fatalf("promotion returned no lesson: %v", lesson)
	}
	if promoted.Status != LessonProposalPromoted {
		t.Errorf("status = %q, want %q", promoted.Status, LessonProposalPromoted)
	}
	if promoted.PromotedLessonID != lesson.ID {
		t.Errorf("promoted_lesson_id = %q, want %q", promoted.PromotedLessonID, lesson.ID)
	}
	if !promoted.ReviewedAt.Valid {
		t.Error("the promotion did not record when it was reviewed")
	}

	// A promoted proposal is not pending any more.
	if _, _, _, err := dm.PromoteLessonProposal(p.ID); err == nil {
		t.Error("promoting an already-promoted proposal succeeded")
	}

	// Rejection keeps the record.
	p2, err := dm.AddLessonProposal("a rejected proposal", "insight", nil, nil)
	if err != nil {
		t.Fatalf("AddLessonProposal: %v", err)
	}
	rejected, err := dm.RejectLessonProposal(p2.ID)
	if err != nil {
		t.Fatalf("RejectLessonProposal: %v", err)
	}
	if rejected.Status != LessonProposalRejected {
		t.Errorf("status = %q, want %q", rejected.Status, LessonProposalRejected)
	}
	if rejected.Content != "a rejected proposal" {
		t.Errorf("the rejected proposal's content was discarded: %q", rejected.Content)
	}
	if _, _, _, err := dm.PromoteLessonProposal(p2.ID); err == nil {
		t.Error("promoting a rejected proposal succeeded")
	}
}

// TestLessonProposal_ThePromotedLessonIsATrustedLesson pins the
// promotion's output.
func TestLessonProposal_ThePromotedLessonIsATrustedLesson(t *testing.T) {
	dm := newProposalTestDM(t)

	p, err := dm.AddLessonProposal("the promoted content", "practice", nil, nil)
	if err != nil {
		t.Fatalf("AddLessonProposal: %v", err)
	}
	lesson, _, _, err := dm.PromoteLessonProposal(p.ID)
	if err != nil {
		t.Fatalf("PromoteLessonProposal: %v", err)
	}

	// The lesson is in the TRUSTED table and is retrievable through the
	// ordinary lesson read path.
	fetched, err := dm.GetLesson(lesson.ID)
	if err != nil {
		t.Fatalf("the promoted lesson is not in lessons_base: %v", err)
	}
	if fetched.Content != "the promoted content" {
		t.Errorf("content = %q, want %q", fetched.Content, "the promoted content")
	}
	if string(fetched.Type) != "practice" {
		t.Errorf("type = %q, want %q", fetched.Type, "practice")
	}
	found, err := dm.SearchLessons("promoted content", 10)
	if err != nil {
		t.Fatalf("SearchLessons: %v", err)
	}
	if len(found) == 0 {
		t.Error("the promoted lesson is not in the FTS search results")
	}

	// And the proposal's own table still holds the proposal.
	still, err := dm.GetLessonProposal(p.ID)
	if err != nil {
		t.Fatalf("GetLessonProposal: %v", err)
	}
	if still.Status != LessonProposalPromoted {
		t.Errorf("status = %q, want %q", still.Status, LessonProposalPromoted)
	}
}

// TestLessonProposal_APromotionIsSafeToRetry pins the retry property.
//
// A promotion that fails after the lesson write leaves a lesson and a
// pending proposal. The operator retries — and the retry must NOT
// create a second lesson. AddLesson's content-hash dedup finds the
// identical lesson and reinforces it, which is the property that makes
// the two-statement promotion safe.
func TestLessonProposal_APromotionIsSafeToRetry(t *testing.T) {
	dm := newProposalTestDM(t)

	p, err := dm.AddLessonProposal("retry-safe content", "insight", nil, nil)
	if err != nil {
		t.Fatalf("AddLessonProposal: %v", err)
	}
	first, _, _, err := dm.PromoteLessonProposal(p.ID)
	if err != nil {
		t.Fatalf("PromoteLessonProposal: %v", err)
	}

	// Simulate the retry: write the lesson again directly, as a
	// promotion whose state update was lost would.
	second, err := dm.AddLesson("retry-safe content", "insight", nil, "")
	if err != nil {
		t.Fatalf("AddLesson: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("the retry created a SECOND lesson (%s vs %s) — the content-hash dedup is not holding",
			second.ID, first.ID)
	}

	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE content = 'retry-safe content'`).Scan(&n); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	if n != 1 {
		t.Errorf("%d lesson rows carry the promoted content, want 1", n)
	}
}

// TestLessonProposal_TheContentIsScannedLikeALessons pins that a
// proposal is held to the same content rules as a lesson.
//
// A proposal is a candidate for the lesson table. A payload that would
// be blocked at promotion time has no business being stored now, and
// the operator should never be shown a proposal that could not have
// been promoted.
func TestLessonProposal_TheContentIsScannedLikeALessons(t *testing.T) {
	dm := newProposalTestDM(t)

	// A sensitive-content payload is refused.
	if _, err := dm.AddLessonProposal("my api key is sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "insight", nil, nil); err == nil {
		t.Error("a sensitive-content proposal was accepted")
	}

	// An invalid lesson type is refused.
	if _, err := dm.AddLessonProposal("ordinary content", "observation", nil, nil); err == nil {
		t.Error("a proposal with an invalid lesson type was accepted")
	}

	// An empty fact is refused.
	if _, err := dm.AddLessonProposal("   ", "insight", nil, nil); err == nil {
		t.Error("an empty proposal was accepted")
	}
}

// TestLessonProposal_ListPinsTheReviewQueue pins the operator's view.
func TestLessonProposal_ListPinsTheReviewQueue(t *testing.T) {
	dm := newProposalTestDM(t)

	for _, content := range []string{"first proposal", "second proposal", "third proposal"} {
		if _, err := dm.AddLessonProposal(content, "insight", nil, nil); err != nil {
			t.Fatalf("AddLessonProposal: %v", err)
		}
	}
	pending, err := dm.ListLessonProposals(LessonProposalPending, 0)
	if err != nil {
		t.Fatalf("ListLessonProposals: %v", err)
	}
	if len(pending) != 3 {
		t.Errorf("%d pending proposals, want 3", len(pending))
	}

	// The default limit is bounded.
	limited, err := dm.ListLessonProposals("", 2)
	if err != nil {
		t.Fatalf("ListLessonProposals: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("%d proposals with a limit of 2, want 2", len(limited))
	}

	// An unknown status is refused rather than silently meaning "all".
	if _, err := dm.ListLessonProposals("approved", 0); err == nil {
		t.Error("an unknown status was accepted")
	}
}

// TestLessonProposal_TheProposalIsNotInTheTrustedTable is the
// isolation assertion at the storage layer.
//
// The proposal lives in its own table and the trusted lesson table
// never sees it. This is the property that makes the isolation
// STRUCTURAL rather than a filter that a future read path could
// forget.
func TestLessonProposal_TheProposalIsNotInTheTrustedTable(t *testing.T) {
	dm := newProposalTestDM(t)

	const content = "isolation check content"
	if _, err := dm.AddLessonProposal(content, "insight", nil, nil); err != nil {
		t.Fatalf("AddLessonProposal: %v", err)
	}

	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE content LIKE ?`, "%"+content+"%").Scan(&n); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	if n != 0 {
		t.Errorf("%d trusted lesson row(s) carry the proposal's content", n)
	}

	// And it is not in the FTS index either.
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_fts WHERE content LIKE ?`, "%"+content+"%").Scan(&n); err != nil {
		t.Fatalf("count lessons_fts: %v", err)
	}
	if n != 0 {
		t.Errorf("%d FTS row(s) carry the proposal's content", n)
	}
}

// TestLessonProposal_TheProvenanceProxyCreditsAtPromotion pins WHERE the
// cited-source credit lands.
//
// `success_count` means "this node helped produce durable knowledge". A
// pending proposal is NOT durable knowledge, so the credit must not
// land at proposal time: a proposal that is never promoted must not
// inflate the retrieval signal of the nodes it drew on.
//
// The credit therefore lands at the promotion, and the node type is
// resolved from the artifact id against the database.
func TestLessonProposal_TheProvenanceProxyCreditsAtPromotion(t *testing.T) {
	dm := newProposalTestDM(t)

	// A real memory for the proposal to cite.
	store, err := dm.getSharedStore()
	if err != nil {
		t.Fatalf("get the shared store: %v", err)
	}
	mem, err := store.AddMemory("a fact the proposal was distilled from", "memories", nil, nil, "", "test")
	if err != nil {
		t.Fatalf("seed the cited memory: %v", err)
	}
	memID := mem.ID
	if memID == "" {
		t.Fatalf("the seeded memory has no id: %+v", mem)
	}

	p, err := dm.AddLessonProposal("a proposal that cites a node", "insight", nil, []string{memID})
	if err != nil {
		t.Fatalf("AddLessonProposal: %v", err)
	}
	if len(p.SourceIDs) != 1 || p.SourceIDs[0] != memID {
		t.Errorf("the proposal did not carry its citations: %+v", p.SourceIDs)
	}

	// BEFORE the promotion, the node carries no success credit.
	successBefore := proposalSuccessCount(t, dm, memID)
	if successBefore != 0 {
		t.Errorf("the cited node has success_count %d before the promotion — the credit must land "+
			"when the lesson becomes durable knowledge, not when the proposal is recorded",
			successBefore)
	}

	_, _, credited, err := dm.PromoteLessonProposal(p.ID)
	if err != nil {
		t.Fatalf("PromoteLessonProposal: %v", err)
	}
	if credited != 1 {
		t.Errorf("credited = %d, want 1", credited)
	}

	successAfter := proposalSuccessCount(t, dm, memID)
	if successAfter != 1 {
		t.Errorf("the cited node has success_count %d after the promotion, want 1", successAfter)
	}
}

// proposalSuccessCount reads a node's success_count from the retrieval
// observability table.
func proposalSuccessCount(t *testing.T, dm *DatabaseManager, nodeID string) int {
	t.Helper()
	var n int
	err := dm.SQLDB().QueryRow(
		`SELECT COALESCE(success_count, 0) FROM retrieval_metadata WHERE node_id = ?`, nodeID).Scan(&n)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0
		}
		t.Fatalf("read the success count: %v", err)
	}
	return n
}
