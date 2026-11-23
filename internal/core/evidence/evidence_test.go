package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The fake GitHub.
//
// It records EVERY call, including the writes, so a test can prove that
// the read path never wrote anything.
type fakeGitHub struct {
	prs        map[int]*GitHubFacts
	prErr      map[int]error
	runs       map[string][]CIRun
	runsErr    map[string]error
	content    map[string][]byte
	contentErr map[string]error
	calls      []string
	writes     int
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		prs:        map[int]*GitHubFacts{},
		prErr:      map[int]error{},
		runs:       map[string][]CIRun{},
		runsErr:    map[string]error{},
		content:    map[string][]byte{},
		contentErr: map[string]error{},
	}
}

func (f *fakeGitHub) PullRequest(ctx context.Context, repo string, number int) (*GitHubFacts, error) {
	f.calls = append(f.calls, fmt.Sprintf("pull_request %s %d", repo, number))
	if err, ok := f.prErr[number]; ok {
		return nil, err
	}
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("%w: pull request #%d", ErrNotFound, number)
	}
	return pr, nil
}

func (f *fakeGitHub) WorkflowRuns(ctx context.Context, repo string, headSHA string) ([]CIRun, error) {
	f.calls = append(f.calls, fmt.Sprintf("workflow_runs %s %s", repo, headSHA))
	if err, ok := f.runsErr[headSHA]; ok {
		return nil, err
	}
	return f.runs[headSHA], nil
}

func (f *fakeGitHub) Content(ctx context.Context, repo string, ref string, path string) ([]byte, error) {
	f.calls = append(f.calls, fmt.Sprintf("content %s %s %s", repo, ref, path))
	if err, ok := f.contentErr[path]; ok {
		return nil, err
	}
	raw, ok := f.content[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return raw, nil
}

// PutFile is the fake's WRITE.
//
// It exists so that a control can make the read path write, and so that
// a test can prove the read path does not.
func (f *fakeGitHub) PutFile(ctx context.Context, path string, content []byte) error {
	f.writes++
	f.calls = append(f.calls, fmt.Sprintf("write %s", path))
	return nil
}

// ── the corpus the tests share ───────────────────────────────────────────

const (
	testHead    = "1111111111111111111111111111111111111111"
	testBase    = "2222222222222222222222222222222222222222"
	testOldHead = "3333333333333333333333333333333333333333"
	testRepo    = "owner/name"
)

func greenRun(id int64, head string) CIRun {
	return CIRun{
		ID: id, Name: "build", HeadSHA: head, Status: "completed", Conclusion: "success",
		URL:  fmt.Sprintf("https://github.com/%s/actions/runs/%d", testRepo, id),
		Jobs: []CIJob{{Name: "test", Status: "completed", Conclusion: "success"}},
	}
}

func claimsFor(id string) Claims {
	return Claims{
		ID: id, RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Objective: "do the thing",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	}
}

// ── gate 2: one root, many assignments, one stable queue ─────────────────

func TestOneRootWithManyAssignmentsProducesAStableQueue(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-b"), claimsFor("task-a"))
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-c", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", ReviewedSHA: testHead,
		FindingsSHA256: "sha256:" + strings.Repeat("a", 64),
		EvidenceRef:    "evidence/abc.txt",
	})

	facts := map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
		"task-b": {GitHub: mergedPR(263, testHead), CI: []CIRun{greenRun(2, testHead)}},
		"task-c": {GitHub: mergedPR(264, testHead), CI: []CIRun{greenRun(3, testHead)}},
	}
	records, err := Build(tree, facts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("the tree produced %d records, want 3", len(records))
	}
	for _, r := range records {
		if r.RootID != "root-1" {
			t.Errorf("the record %s is rooted at %q, want root-1", r.TaskID, r.RootID)
		}
	}
	// The ORDER is stable and by task id.
	if records[0].TaskID != "task-a" || records[1].TaskID != "task-b" ||
		records[2].TaskID != "task-c" {
		t.Errorf("the records are ordered %s, %s, %s; want task-a, task-b, task-c",
			records[0].TaskID, records[1].TaskID, records[2].TaskID)
	}
	// The reviewer is LINKED to its subject: the subject carries the
	// captured evidence and the verdict, and the reviewer does not
	// review itself.
	var subject *Record
	for _, r := range records {
		if r.TaskID == "task-a" {
			subject = r
		}
	}
	if subject == nil {
		t.Fatal("there is no record for task-a")
	}
	if subject.ReviewDisposition != ReviewDispositionFindingsCaptured {
		t.Errorf("the subject's review disposition is %q, want %q",
			subject.ReviewDisposition, ReviewDispositionFindingsCaptured)
	}
	if subject.ReviewEvidence == nil || !subject.ReviewEvidence.Known() {
		t.Errorf("the subject carries no captured evidence: %+v", subject.ReviewEvidence)
	}
}

// ── gate 3: the four things stay apart ───────────────────────────────────

func TestTheFourThingsStayApart(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "p", Title: "the work",
		Role: "implementer", State: "review_passed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		FindingsSHA256: "sha256:" + strings.Repeat("b", 64),
		EvidenceRef:    "evidence/findings.txt",
		AwaitingAction: "",
		Approval: &Approval{
			Action: "merge", Revision: testHead, Source: "operator",
			Evidence: "the operator's own record", ApprovedAt: 1700000000,
		},
	})
	// The reviewer recorded a verdict.
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-r", RootID: "root-1", ProjectID: "p", Title: "the review",
		Role: "reviewer", State: "review_passed", ReviewTarget: "task-a",
		ReviewedSHA: testHead, Verdict: VerdictPassed,
		FindingsSHA256: "sha256:" + strings.Repeat("b", 64),
		EvidenceRef:    "evidence/findings.txt",
	})
	facts := map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
		"task-r": {GitHub: mergedPR(264, testHead), CI: []CIRun{greenRun(3, testHead)}},
	}
	records, err := Build(tree, facts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var subject *Record
	for _, r := range records {
		if r.TaskID == "task-a" {
			subject = r
		}
	}
	if subject == nil {
		t.Fatal("there is no record for task-a")
	}
	// 1. The verdict is a DECISION and it is the reviewer's.
	if subject.ReviewDisposition != ReviewDispositionVerdict {
		t.Errorf("the review disposition is %q, want %q", subject.ReviewDisposition,
			ReviewDispositionVerdict)
	}
	if subject.ReviewVerdict != VerdictPassed {
		t.Errorf("the verdict is %q, want %q", subject.ReviewVerdict, VerdictPassed)
	}
	// 2. The captured evidence is a DIGEST and a POINTER, not a verdict.
	if subject.ReviewEvidence == nil || subject.ReviewEvidence.SHA256 == subject.ReviewVerdict {
		t.Errorf("the review evidence is %+v; the digest and the verdict must be different things",
			subject.ReviewEvidence)
	}
	// 3. The approval is a DECISION and it is the operator's.
	if subject.OperatorApproval == nil {
		t.Fatal("the record carries no operator approval")
	}
	if subject.OperatorApproval.Action != "merge" {
		t.Errorf("the approval's action is %q, want merge", subject.OperatorApproval.Action)
	}
	// 4. The runtime's success is not the task's completion: the
	// disposition is derived from the state, the review and the CI, and
	// the record says which category each field belongs to.
	if subject.Disposition != DispositionCompleted {
		t.Errorf("the disposition is %q, want %q", subject.Disposition, DispositionCompleted)
	}
	if CategoryOf(FieldReviewVerdict) != CategoryDecision {
		t.Errorf("the verdict's category is %q, want %q", CategoryOf(FieldReviewVerdict),
			CategoryDecision)
	}
	if CategoryOf(FieldOperatorApproval) != CategoryDecision {
		t.Errorf("the approval's category is %q, want %q", CategoryOf(FieldOperatorApproval),
			CategoryDecision)
	}
	if CategoryOf(FieldHeadSHA) != CategoryRecorded {
		t.Errorf("the head's category is %q, want %q", CategoryOf(FieldHeadSHA), CategoryRecorded)
	}
}

// ── gate 4: a missing digest cannot become an approved review ────────────

func TestAMissingDigestCannotBecomeAnApprovedReview(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-r", RootID: "root-1", ProjectID: "p", Title: "the review",
		Role: "reviewer", State: "review_passed", ReviewTarget: "task-a",
		ReviewedSHA: testHead, Verdict: VerdictPassed,
		// NO digest, NO pointer.
	})
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	facts := map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
		"task-r": {GitHub: mergedPR(264, testHead)},
	}
	records, err := Build(tree, facts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var subject *Record
	for _, r := range records {
		if r.TaskID == "task-a" {
			subject = r
		}
	}
	if subject == nil {
		t.Fatal("there is no record for task-a")
	}
	if subject.ReviewEvidence == nil {
		t.Fatal("the subject carries no review evidence at all")
	}
	if subject.ReviewEvidence.Known() {
		t.Errorf("the review evidence claims a digest: %q", subject.ReviewEvidence.SHA256)
	}
	if subject.ReviewEvidence.SHA256 != Unknown {
		t.Errorf("the missing digest is %q, want %q", subject.ReviewEvidence.SHA256, Unknown)
	}
	// And the VERIFIER says the evidence cannot be checked.
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.content["evidence/nothing.txt"] = []byte("not the evidence")
	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), subject)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	found := false
	for _, c := range verification.Checks {
		if c.Name == CheckReviewEvidence {
			found = true
			if c.Status != CheckMissing {
				t.Errorf("the review evidence check is %q, want %q", c.Status, CheckMissing)
			}
		}
	}
	if !found {
		t.Error("there is no review evidence check")
	}
}

// ── gate 5: a digest mismatch is refused ─────────────────────────────────

func TestADigestMismatchIsVisiblyRefused(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionFindingsCaptured,
		ReviewEvidence: &EvidenceRef{
			Ref:    "evidence/findings.txt",
			SHA256: "sha256:" + strings.Repeat("c", 64),
		},
		Provenance: ProvenanceFor(),
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.content["evidence/findings.txt"] = []byte("different bytes entirely")
	v := &Verifier{GitHub: gh, Repository: testRepo}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, c := range verification.Checks {
		if c.Name == CheckReviewEvidence {
			if c.Status != CheckFailed {
				t.Errorf("the review evidence check is %q, want %q", c.Status, CheckFailed)
			}
			return
		}
	}
	t.Error("there is no review evidence check")
}

// ── gates 6 and 8: the CI is the CI of THIS head, and it is green ───────

func TestACIRunAgainstAnOldSHAIsNotCIForANewerHead(t *testing.T) {
	green, known, reason := CIVerdict([]CIRun{greenRun(1, testOldHead)}, testHead)
	if green {
		t.Error("a run against another commit was counted as CI for this head")
	}
	if known {
		t.Errorf("the CI is known: %q", reason)
	}
}

func TestAFailedOrPendingCIJobCannotBeGreen(t *testing.T) {
	failed := greenRun(1, testHead)
	failed.Jobs = []CIJob{{Name: "test", Status: "completed", Conclusion: "failure"}}
	if green, _, _ := CIVerdict([]CIRun{failed}, testHead); green {
		t.Error("a failed job was presented as green")
	}
	pending := greenRun(2, testHead)
	pending.Status = "in_progress"
	if green, _, _ := CIVerdict([]CIRun{pending}, testHead); green {
		t.Error("a run that is still going was presented as green")
	}
	noJobs := greenRun(3, testHead)
	noJobs.Jobs = nil
	green, known, _ := CIVerdict([]CIRun{noJobs}, testHead)
	if green {
		t.Error("a run whose jobs could not be read was presented as green")
	}
	if known {
		t.Error("a run whose jobs could not be read was presented as KNOWN")
	}
	failedConclusion := greenRun(4, testHead)
	failedConclusion.Conclusion = "failure"
	if green, _, _ := CIVerdict([]CIRun{failedConclusion}, testHead); green {
		t.Error("a failed run was presented as green")
	}
}

// ── gate 7: a newer head invalidates the reviewed-head claim ─────────────

func TestANewerHeadInvalidatesTheReviewedHeadClaim(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testOldHead, ReviewedSHA: testOldHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		CI:         []CIRun{greenRun(1, testOldHead)},
		Provenance: ProvenanceFor(),
	}
	gh := newFakeGitHub()
	// The pull request has MOVED ON since the review.
	gh.prs[262] = mergedPR(262, testHead)
	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	sawStale := false
	for _, c := range verification.Checks {
		if c.Name == CheckReviewedSHA {
			if c.Status != CheckStale {
				t.Errorf("the reviewed SHA check is %q, want %q", c.Status, CheckStale)
			}
			sawStale = true
		}
	}
	if !sawStale {
		t.Error("there is no reviewed SHA check")
	}
	if !verification.Stale() {
		t.Error("the verification does not report the record as stale")
	}
}

// ── gate 9: a missing or unreachable GitHub is explicit ──────────────────

func TestAMissingOrUnreachableGitHubIsExplicit(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionNone,
		Provenance:        ProvenanceFor(),
	}

	// MISSING.
	missing := newFakeGitHub()
	missing.prErr[262] = fmt.Errorf("%w: pull request #262", ErrNotFound)
	v := &Verifier{GitHub: missing}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, c := range verification.Checks {
		if c.Name == CheckPullRequest && c.Status != CheckMissing {
			t.Errorf("the pull request check is %q, want %q", c.Status, CheckMissing)
		}
	}
	if verification.Verdict() == CheckVerified {
		t.Error("a record whose pull request is missing was verified")
	}

	// UNREACHABLE.
	unreachable := newFakeGitHub()
	unreachable.prErr[262] = fmt.Errorf("%w: the network is down", ErrUnavailable)
	v = &Verifier{GitHub: unreachable}
	verification, err = v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, c := range verification.Checks {
		if c.Name == CheckPullRequest && c.Status != CheckUnverified {
			t.Errorf("the pull request check is %q, want %q", c.Status, CheckUnverified)
		}
	}
	if verification.Verdict() == CheckVerified {
		t.Error("a record GitHub could not be asked about was verified")
	}
}

// ── gate 10: repeated publication does not duplicate logical records ──────

func TestRepeatedPublicationDoesNotDuplicateLogicalRecords(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	first, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	second, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if first.SnapshotID != second.SnapshotID {
		t.Errorf("the two publications have different snapshots: %s and %s",
			first.SnapshotID, second.SnapshotID)
	}
	if len(first.Files) != len(second.Files) {
		t.Errorf("the two publications have %d and %d files", len(first.Files), len(second.Files))
	}
	for i := range first.Files {
		if first.Files[i].Path != second.Files[i].Path {
			t.Errorf("file %d is %q and %q", i, first.Files[i].Path, second.Files[i].Path)
		}
	}
	// A corpus with TWO records for one task is refused.
	duplicated := append([]*Record(nil), records...)
	duplicated = append(duplicated, records[0])
	if _, err := NewPlan(testRepo, EvidenceBranch, duplicated, timeFixture()); err == nil {
		t.Error("a corpus with two records for one task was published")
	}
}

// ── gate 11: an interrupted publication leaves the pointer alone ─────────

func TestAnInterruptedPublicationLeavesThePointerAlone(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// The writer fails on the THIRD file — which is before the pointer.
	writer := &failingWriter{failAt: 3}
	publisher := &Publisher{Writer: writer}
	pub, err := publisher.Publish(context.Background(), plan)
	if err == nil {
		t.Fatal("the publication reported success although the writer failed")
	}
	if pub != nil {
		t.Error("the publication returned a result although the writer failed")
	}
	if writer.committed {
		t.Error("the writer committed although it failed")
	}
	if writer.stagedPointer {
		t.Error("the pointer was staged although the publication was interrupted")
	}
	if !writer.aborted {
		t.Error("the writer was not aborted")
	}
	// A RETRY succeeds, and the pointer names the snapshot.
	writer2 := &failingWriter{}
	publisher2 := &Publisher{Writer: writer2}
	pub2, err := publisher2.Publish(context.Background(), plan)
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if pub2.SnapshotID != plan.SnapshotID {
		t.Errorf("the retry published %s, want %s", pub2.SnapshotID, plan.SnapshotID)
	}
	if !writer2.stagedPointer {
		t.Error("the retry did not stage the pointer")
	}
}

// ── gate 12: the publisher invents neither a merge nor an approval ───────

func TestThePublisherInventsNeitherAMergeNorAnApproval(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, r := range records {
		if r.OperatorApproval != nil {
			t.Errorf("the record %s carries an approval nobody gave", r.TaskID)
		}
		if r.PendingOperatorAction != "" {
			t.Errorf("the record %s carries a pending action nobody asked for", r.TaskID)
		}
		if r.GitHub != nil && r.GitHub.State == "merged" {
			t.Errorf("the record %s claims a merge the publisher never observed", r.TaskID)
		}
	}
	// The recorded claims carry no approval, so the published bytes
	// carry none either.
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		// The provenance table NAMES the field — that is what it is
		// for. What must not appear is an approval VALUE: nobody
		// gave one.
		if strings.Contains(string(f.Content), "\"operator_approval\":{") {
			t.Errorf("the file %s publishes an approval nobody gave", f.Path)
		}
		if strings.Contains(string(f.Content), "**approved**:") {
			t.Errorf("the file %s renders an approval nobody gave", f.Path)
		}
	}
}

// ── gate 13: the check never writes ──────────────────────────────────────

func TestTheCheckNeverWrites(t *testing.T) {
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
	}
	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("the check reported %d records, want 1", len(report.Records))
	}
	if gh.writes != 0 {
		t.Errorf("the check performed %d writes; it must perform none", gh.writes)
	}
	for _, call := range gh.calls {
		if strings.HasPrefix(call, "write ") {
			t.Errorf("the check called %q", call)
		}
	}
}

// ── gate 14: secrets and transcripts do not leak ─────────────────────────

func TestSecretsAndTranscriptsDoNotLeak(t *testing.T) {
	secret := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	private := "/home/someone/.mpm/src/db/mpm.db"
	transcript := strings.Repeat("the reviewer said something at length. ", 12)

	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "p",
		Title:     "the work " + secret,
		Objective: "the objective " + private,
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	// THE GITHUB SIDE IS SCREENED LIKE THE CLAIMS.
	//
	// A pull request's title, a review's body and a workflow's or job's
	// name are written by OTHER PEOPLE, and the substrate never saw any
	// of it. That is exactly the text that must not reach the published
	// bytes — so the gate is exercised on that side too, not only on the
	// store's own claims.
	gh := mergedPR(262, testHead)
	gh.Title = "the title " + secret
	gh.Reviews = []GitHubReview{{
		Author: "charlie", State: "APPROVED",
		Body: "the findings " + private,
		SHA:  testHead,
	}}
	facts := map[string]Facts{"task-a": {GitHub: gh, CI: []CIRun{{
		ID: 1, Name: "the workflow " + transcript, HeadSHA: testHead,
		Status: "completed", Conclusion: "success",
		Jobs: []CIJob{{Name: "the job " + private, Status: "completed", Conclusion: "success"}},
	}}}}
	records, err := Build(tree, facts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		body := string(f.Content)
		for _, leak := range []string{secret, private, transcript} {
			if strings.Contains(body, leak) {
				t.Errorf("the file %s carries %q", f.Path, leak)
			}
		}
	}
	// The redaction NAMES the field it withheld.
	record := records[0]
	if len(record.Redactions) == 0 {
		t.Error("the record names no redaction")
	}
	joined := strings.Join(record.Redactions, ",")
	for _, field := range []string{
		FieldGitHub + ".title", FieldGitHub + ".review.body",
		FieldCI + ".run.name", FieldCI + ".job.name",
	} {
		if !strings.Contains(joined, field) {
			t.Errorf("the record does not name the withheld field %q: %v",
				field, record.Redactions)
		}
	}
}

// ── gate 15: the index does not authenticate itself ──────────────────────

func TestTheIndexDoesNotAuthenticateItself(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		own := DigestBytes(f.Content)
		// The file may not contain its OWN digest: that is the
		// self-reference that could never be computed.
		if strings.Contains(string(f.Content), own) {
			t.Errorf("the file %s contains its own digest %s", f.Path, own)
		}
	}
	// And the index's digest is not in the index.
	var indexRaw []byte
	for _, f := range plan.Files {
		if f.Path == IndexPath {
			indexRaw = f.Content
		}
	}
	if indexRaw == nil {
		t.Fatal("the plan has no index")
	}
	if strings.Contains(string(indexRaw), DigestBytes(indexRaw)) {
		t.Error("the index contains its own digest")
	}
}

// ── gate 16: a reader can follow the evidence to GitHub ──────────────────

func TestAReaderCanFollowTheEvidenceToGitHub(t *testing.T) {
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.runs[testHead] = []CIRun{greenRun(1, testHead)}
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
	}
	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("the check reported %d records, want 1", len(report.Records))
	}
	rec := report.Records[0]
	if rec.GitHub == nil || rec.GitHub.URL == "" {
		t.Error("the record carries no pointer to the pull request")
	}
	if len(rec.CI) == 0 || rec.CI[0].URL == "" {
		t.Error("the record carries no pointer to the CI run")
	}
	// The check went and ASKED GitHub.
	sawPR, sawRuns := false, false
	for _, call := range gh.calls {
		if strings.HasPrefix(call, "pull_request ") {
			sawPR = true
		}
		if strings.HasPrefix(call, "workflow_runs ") {
			sawRuns = true
		}
	}
	if !sawPR || !sawRuns {
		t.Error("the check did not ask GitHub about the pull request and its CI")
	}
}

// ── gate 1: a reader with no database can discover the queue ─────────────

func TestAPublishedIndexIsDiscoverableWithoutADatabase(t *testing.T) {
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"), claimsFor("task-b"))
	records, err := Build(tree, map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead)},
		"task-b": {GitHub: mergedPR(263, testHead)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
	}
	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Source.SnapshotID != plan.SnapshotID {
		t.Errorf("the check read the snapshot %s, want %s", report.Source.SnapshotID, plan.SnapshotID)
	}
	if len(report.Records) != 2 {
		t.Errorf("the check reported %d records, want 2", len(report.Records))
	}
	if len(report.Sections) == 0 {
		t.Error("the check reported no sections")
	}
	// The index's records are readable on their own: a reader needs no
	// MPM at all.
	indexRaw, ok := gh.content[IndexPath]
	if !ok {
		t.Fatal("the publication has no index")
	}
	index, err := ParseIndex(indexRaw)
	if err != nil {
		t.Fatalf("parse the index: %v", err)
	}
	if index.RecordCount != 2 {
		t.Errorf("the index claims %d records, want 2", index.RecordCount)
	}
}

// ── the index is refused when it does not add up ─────────────────────────

func TestAnIndexThatDoesNotAddUpIsRefused(t *testing.T) {
	raw, err := json.Marshal(Index{
		FormatVersion: FormatVersion, SnapshotID: "sha256:x", RecordCount: 3,
		Records: []IndexEntry{{ID: "a"}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := ParseIndex(raw); err == nil {
		t.Error("an index whose count does not match its records was accepted")
	}
	raw, _ = json.Marshal(Index{FormatVersion: "other", SnapshotID: "sha256:x"})
	if _, err := ParseIndex(raw); err == nil {
		t.Error("an index of an unknown format version was accepted")
	}
}

// ── the fixture corpus loads and builds ──────────────────────────────────

func TestTheContinuityFixtureLoadsAndBuilds(t *testing.T) {
	fixture, err := LoadFixture("continuity-001")
	if err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	if len(fixture.Tasks) != 4 {
		t.Fatalf("the fixture holds %d tasks, want 4", len(fixture.Tasks))
	}
	gh := newFakeGitHub()
	for _, task := range fixture.Tasks {
		number, err := PRNumber(task.PRRef)
		if err != nil {
			t.Fatalf("the fixture's task %s: %v", task.TaskID, err)
		}
		gh.prs[number] = mergedPR(number, task.HeadSHA)
	}
	corpus, err := BuildCorpusFromFixture(context.Background(), gh, fixture)
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("the corpus produced %d records, want 4", len(records))
	}
	for _, r := range records {
		if r.ReviewDisposition != ReviewDispositionLocallyReported {
			t.Errorf("the record %s reports its review as %q, want %q",
				r.TaskID, r.ReviewDisposition, ReviewDispositionLocallyReported)
		}
		if r.ReviewVerdict != ReportedApproved {
			t.Errorf("the record %s reports the verdict %q, want %q",
				r.TaskID, r.ReviewVerdict, ReportedApproved)
		}
		if r.ReviewEvidence != nil && r.ReviewEvidence.Known() {
			t.Errorf("the record %s claims a digest for evidence that was never published",
				r.TaskID)
		}
	}
}

// ── the helpers ──────────────────────────────────────────────────────────

func mergedPR(number int, head string) *GitHubFacts {
	return &GitHubFacts{
		Repository: testRepo, Number: number, State: "merged", Title: "the work",
		HeadSHA: head, BaseSHA: testBase,
		MergeCommit: "4444444444444444444444444444444444444444",
		MergedAt:    "2026-11-22T08:59:00Z",
		URL:         fmt.Sprintf("https://github.com/%s/pull/%d", testRepo, number),
	}
}

// timeFixture is a fixed moment, so a plan is byte-identical twice.
func timeFixture() time.Time {
	return time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
}

// failingWriter fails on the Nth file.
type failingWriter struct {
	failAt        int
	staged        int
	stagedPointer bool
	committed     bool
	aborted       bool
}

func (w *failingWriter) PutFile(ctx context.Context, path string, content []byte) error {
	w.staged++
	if w.staged == w.failAt {
		return fmt.Errorf("the writer failed")
	}
	if path == IndexPath {
		w.stagedPointer = true
	}
	return nil
}

func (w *failingWriter) Commit(ctx context.Context, message string) (string, error) {
	w.committed = true
	return "sha", nil
}

func (w *failingWriter) Abort(ctx context.Context) error {
	w.aborted = true
	return nil
}

// ── gate 3 again: a forged verdict is refused ────────────────────────────

func TestAForgedReviewVerdictIsRefused(t *testing.T) {
	// A record that claims a verdict while saying that only FINDINGS
	// were captured. It is the shape a forgery takes: the verdict field
	// is filled in and the disposition is not.
	forged := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionFindingsCaptured,
		ReviewVerdict:     VerdictPassed,
		Provenance:        ProvenanceFor(),
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), forged)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	found := false
	for _, c := range verification.Checks {
		if c.Name == CheckReviewVerdict {
			found = true
			if c.Status != CheckFailed {
				t.Errorf("the review verdict check is %q, want %q", c.Status, CheckFailed)
			}
		}
	}
	if !found {
		t.Fatal("there is no review verdict check")
	}
	if verification.Verdict() == CheckVerified {
		t.Error("a forged verdict was verified")
	}
	// And the VALIDATION refuses to publish it at all.
	if err := Validate(forged); err == nil {
		t.Error("a forged verdict passed the record validation")
	}
}

// ── gate 5 again: the record's bytes must match its name ─────────────────

func TestARecordWhoseBytesDoNotMatchItsNameIsRefused(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := newFakeGitHub()
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
	}
	// TAMPER: the record's bytes are replaced with different bytes, and
	// the index still names the record by its ORIGINAL digest.
	for _, f := range plan.Files {
		if strings.HasPrefix(f.Path, RecordsDirectory+"/") && strings.HasSuffix(f.Path, ".json") {
			gh.content[f.Path] = []byte(`{"format_version":"mpm.evidence/v1","task_id":"task-a","state":"completed"}`)
		}
	}
	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("the check reported %d records, want 1", len(report.Records))
	}
	rec := report.Records[0]
	if rec.DigestVerified {
		t.Error("a record whose bytes do not match its name was accepted")
	}
	if rec.Verdict == CheckVerified {
		t.Error("a record whose bytes do not match its name was verified")
	}
	if report.Verified {
		t.Error("the report says everything is verified")
	}
}
