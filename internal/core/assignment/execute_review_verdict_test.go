package assignment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	oc "github.com/flowbyte-com/mpm-core/opencode"
)

func reviewResultWith(assistant string) *oc.ReviewResult {
	return &oc.ReviewResult{
		ReviewedRevision:  strings.Repeat("1", 40),
		FindingsSHA256:    strings.Repeat("e", 64),
		Outcome:           "succeeded",
		Agent:             "mpm-reviewer",
		Model:             "minimax-coding-plan/MiniMax-M3.1-Flash",
		Findings:          "the full transcript",
		AssistantFindings: assistant,
		EvidencePath:      "/evidence/review.json",
	}
}

func TestExecuteReviewRecordsApprovedVerdict(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{result: reviewResultWith("the code is correct\nVERDICT: APPROVED")}

	res, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
		ExpectedRevision: strings.Repeat("1", 40),
		ExpectedModel:    "minimax-coding-plan/MiniMax-M3.1-Flash",
	})
	if err != nil {
		t.Fatalf("execute-review: %v", err)
	}
	if res.Verdict != VerdictPassed {
		t.Fatalf("verdict %q, want passed", res.Verdict)
	}
	reviewer, err := store.Get(rev.id)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.State != StateReviewPassed {
		t.Fatalf("reviewer is %q, want review_passed", reviewer.State)
	}
	subject, err := store.Get(implID)
	if err != nil {
		t.Fatal(err)
	}
	if subject.State != StateReviewPassed {
		t.Fatalf("subject is %q, want review_passed", subject.State)
	}
}

func TestExecuteReviewRecordsChangesRequested(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{result: reviewResultWith("F1: fix this\nVERDICT: CHANGES REQUESTED")}

	res, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err != nil {
		t.Fatalf("execute-review: %v", err)
	}
	if res.Verdict != VerdictChangesRequested {
		t.Fatalf("verdict %q, want changes_requested", res.Verdict)
	}
	reviewer, _ := store.Get(rev.id)
	if reviewer.State != StateCompleted {
		t.Fatalf("reviewer is %q, want completed", reviewer.State)
	}
	subject, _ := store.Get(implID)
	if subject.State != StateCompleted {
		t.Fatalf("subject is %q, want completed (changes are a note)", subject.State)
	}
}

func TestExecuteReviewRefusesNoTerminalVerdict(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{result: reviewResultWith("the code is fine")}

	_, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err == nil || !errors.Is(err, ErrReviewVerdict) {
		t.Fatalf("no terminal verdict was not refused: %v", err)
	}
	reviewer, _ := store.Get(rev.id)
	if reviewer.ReviewedSHA != "" || reviewer.FindingsSHA256 != "" {
		t.Fatal("evidence was captured for a review with no verdict")
	}
	attempts, _ := store.AttemptsFor(rev.id)
	if len(attempts) != 1 || attempts[0].LaunchState != SessionAmbiguous {
		t.Fatalf("the unresolved verdict did not leave an ambiguous attempt: %+v", attempts)
	}
}

func TestExecuteReviewIgnoresVerdictsOutsideAssistantText(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")

	// THE TRANSCRIPT WOULD BE AN APPROVAL IF IT WERE READ. The assistant
	// text is verdict-free, so a regression that transcribed `Findings`
	// would record `passed` and this test would fail.
	result := reviewResultWith("the code is fine")
	result.Findings = "VERDICT: APPROVED"
	fake := &fakeReviewRuntime{result: result}
	if _, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	}); err == nil || !errors.Is(err, ErrReviewVerdict) {
		t.Fatalf("a prompt/tool/diagnostic verdict was accepted: %v", err)
	}
	reviewer, _ := store.Get(rev.id)
	if reviewer.State != StateInProgress || reviewer.ReviewedSHA != "" {
		t.Fatalf("a non-assistant verdict changed the reviewer: %+v", reviewer)
	}
}

func TestExecuteReviewRefusesConflictingVerdicts(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{result: reviewResultWith(
		"VERDICT: APPROVED\nVERDICT: CHANGES REQUESTED")}

	if _, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	}); err == nil || !errors.Is(err, ErrReviewVerdict) {
		t.Fatalf("conflicting verdicts were not refused: %v", err)
	}
}

func TestExecuteReviewRefusesWrongSessionIdentity(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{
		result:            reviewResultWith("VERDICT: APPROVED"),
		sessionIDOverride: "ses_not_this_attempt",
	}
	if _, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	}); err == nil || !errors.Is(err, ErrReviewAmbiguous) ||
		!strings.Contains(err.Error(), "session") {
		t.Fatalf("a different native session id was accepted: %v", err)
	}
	assertReviewNotRecorded(t, store, rev.id, implID)
}

// TestExecuteReviewRecordsSessionPermissionsAsReported pins the
// framework-permission policy for reviewers: a session-level grant the
// framework reports is RECORDED with the review, never refused and never
// granted by MPM. Tool permissions are the agent framework's policy; MPM
// verifies session identity, model, revision and evidence.
func TestExecuteReviewRecordsSessionPermissionsAsReported(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{
		result: reviewResultWith("VERDICT: APPROVED"),
		permissionOverride: []oc.SessionPerm{{
			Action: "read", Resource: "*", Effect: "allow",
		}},
	}
	res, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err != nil {
		t.Fatalf("a review whose framework granted a session permission was refused: %v", err)
	}
	if res.Verdict != VerdictPassed {
		t.Fatalf("the verdict is %q, want passed", res.Verdict)
	}
	got, err := store.Get(rev.id)
	if err != nil {
		t.Fatalf("read the reviewer: %v", err)
	}
	// The evidence row is recorded; the permission list is not an MPM
	// responsibility and is not persisted as an MPM refusal.
	if got.ReviewedSHA == "" || got.FindingsSHA256 == "" {
		t.Fatalf("the review evidence was not recorded: %+v", got)
	}
}

// TestWorkerSessionPermissionsAreFrameworkPolicy pins the same policy for
// the worker: a session-level grant the framework reports does not block
// the assignment, and MPM neither widens nor revokes it.
func TestWorkerSessionPermissionsAreFrameworkPolicy(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	ws := t.TempDir()
	state := okState(ws, "permitted-model", "mpm-worker")
	state.Permissions = []oc.SessionPerm{{Action: "shell", Resource: "*", Effect: "allow"}}
	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}, launchState: state}

	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Assignment == nil || res.Assignment.State != StateCompleted {
		t.Fatalf("the assignment is %+v, want completed", res.Assignment)
	}
	if res.Attempt == nil || res.Attempt.LaunchState != SessionCompleted {
		t.Fatalf("the attempt is %+v, want completed", res.Attempt)
	}
}

// TestFrameworkRefusalIsNotSuccess proves a framework that reports a
// terminal FAILURE is recorded as one: no completion is manufactured from
// a run the framework itself refused.
func TestFrameworkRefusalIsNotSuccess(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	ws := t.TempDir()
	rt := &fakeRuntime{
		allowed:     map[string]bool{"mpm-worker": true},
		launchState: okState(ws, "permitted-model", "mpm-worker"),
	}
	// The framework reports a terminal failure for this session.
	rt.launchState.Outcome = "failed"

	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.RuntimeOutcome != "failed" {
		t.Fatalf("runtime outcome %q, want failed", res.RuntimeOutcome)
	}
	if res.Attempt == nil || res.Attempt.LaunchState != SessionFailed {
		t.Fatalf("the attempt is %+v, want failed", res.Attempt)
	}
	if res.Assignment != nil && res.Assignment.State == StateCompleted {
		t.Fatal("a failed framework run completed the assignment")
	}
}

func TestExecuteReviewRefusesWrongRevision(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{result: reviewResultWith("VERDICT: APPROVED")}
	fake.result.ReviewedRevision = strings.Repeat("2", 40)

	if _, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
		ExpectedRevision: strings.Repeat("1", 40),
	}); err == nil || !errors.Is(err, ErrReviewAmbiguous) {
		t.Fatalf("a different checkout HEAD was accepted: %v", err)
	}
	assertReviewNotRecorded(t, store, rev.id, implID)
}

func TestExecuteReviewRefusesTheWrongModel(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{result: reviewResultWith("VERDICT: APPROVED")}
	fake.result.Model = "deepseek/deepseek-chat"

	if _, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
		ExpectedModel: "minimax-coding-plan/MiniMax-M3.1-Flash",
	}); err == nil || !errors.Is(err, ErrReviewAmbiguous) ||
		!strings.Contains(err.Error(), "model") {
		t.Fatalf("a review under an unauthorised model was accepted: %v", err)
	}
	assertReviewNotRecorded(t, store, rev.id, implID)
}

func TestExecuteReviewPostInferenceErrorNeverReruns(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	fake := &fakeReviewRuntime{err: fmt.Errorf("%w: capture failed", oc.ErrReviewIncomplete)}

	_, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err == nil || !errors.Is(err, ErrReviewAmbiguous) ||
		!errors.Is(err, oc.ErrReviewIncomplete) {
		t.Fatalf("a post-inference capture failure was not ambiguous: %v", err)
	}
	if fake.Calls() != 1 {
		t.Fatalf("the reviewer ran %d time(s), want 1", fake.Calls())
	}
	attempts, _ := store.AttemptsFor(rev.id)
	if len(attempts) != 1 || attempts[0].LaunchState != SessionAmbiguous {
		t.Fatalf("the attempt is not ambiguous: %+v", attempts)
	}

	// A SECOND PASS MUST NOT SPEND ANOTHER REVIEW.
	_, err = store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err == nil {
		t.Fatal("a second pass after a post-inference failure was accepted")
	}
	if fake.Calls() != 1 {
		t.Fatalf("recovery launched a second reviewer: calls=%d", fake.Calls())
	}
}

// assertReviewNotRecorded proves a refused review changed nothing: the
// reviewer is still in_progress, no evidence was written, the subject did not
// move, and the attempt is ambiguous (so it cannot be retried automatically).
func assertReviewNotRecorded(t *testing.T, store *Store, reviewerID, subjectID string) {
	t.Helper()
	reviewer, err := store.Get(reviewerID)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.State != StateInProgress {
		t.Fatalf("the reviewer is %q, want in_progress", reviewer.State)
	}
	if reviewer.ReviewedSHA != "" || reviewer.FindingsSHA256 != "" {
		t.Fatalf("evidence was recorded on a refused review: %+v", reviewer)
	}
	subject, err := store.Get(subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if subject.State != StateCompleted {
		t.Fatalf("the subject is %q, want completed", subject.State)
	}
	attempts, err := store.AttemptsFor(reviewerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].LaunchState != SessionAmbiguous {
		t.Fatalf("the refused review did not leave one ambiguous attempt: %+v", attempts)
	}
}

func TestExecuteReviewDoesNotRerunCapturedEvidenceWithoutVerdict(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")

	// Simulate a crash after an older/partial evidence capture, before the
	// verdict transaction. The review must not be launched again.
	if _, err := store.CaptureReviewEvidence(ReviewEvidenceParams{
		ReviewerID: rev.id, Epoch: rev.epoch,
		ReviewedSHA: strings.Repeat("1", 40), FindingsSHA256: strings.Repeat("e", 64),
		EvidenceRef: "/evidence/partial.json", Actor: Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("seed partial evidence: %v", err)
	}
	fake := &fakeReviewRuntime{result: reviewResultWith("VERDICT: APPROVED")}
	_, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err == nil || !errors.Is(err, ErrReviewVerdict) {
		t.Fatalf("a captured review without a verdict was re-run: %v", err)
	}
	if fake.Calls() != 0 {
		t.Fatalf("the reviewer ran %d time(s) on recovery", fake.Calls())
	}
	// A SECOND recovery pass must also make no inference.
	_, err = store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err == nil || fake.Calls() != 0 {
		t.Fatalf("a repeated recovery ran the reviewer: %v (calls=%d)", err, fake.Calls())
	}
}

func TestExecuteReviewEvidenceAndVerdictRollBackTogether(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")
	store.fault = func(stage string) error {
		if stage == "transition" {
			return errors.New("injected transition failure")
		}
		return nil
	}
	t.Cleanup(func() { store.fault = nil })

	fake := &fakeReviewRuntime{result: reviewResultWith("VERDICT: APPROVED")}
	if _, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	}); err == nil || !errors.Is(err, ErrReviewAmbiguous) {
		t.Fatalf("the injected transition failure was not ambiguous: %v", err)
	}
	attempts, _ := store.AttemptsFor(rev.id)
	if len(attempts) != 1 || attempts[0].LaunchState != SessionAmbiguous {
		t.Fatalf("the failed transaction did not leave an ambiguous attempt: %+v", attempts)
	}
	reviewer, _ := store.Get(rev.id)
	if reviewer.ReviewedSHA != "" || reviewer.FindingsSHA256 != "" {
		t.Fatal("evidence persisted without the verdict")
	}
	if reviewer.State != StateInProgress {
		t.Fatalf("reviewer state is %q after rollback", reviewer.State)
	}
}

func TestAnImplementerCannotRecordAReviewVerdict(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, epoch := claimedAssignment(t, store, projectID, "alpha")

	_, err := store.RecordReviewResult(ReviewResultParams{
		ReviewerID: implID,
		Verdict:    VerdictPassed,
		Epoch:      epoch,
		Actor:      Actor{Address: "alpha", Kind: "agent"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not review") {
		t.Fatalf("an implementer recorded a review verdict: %v", err)
	}
}
