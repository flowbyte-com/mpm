package evidence

import (
	"context"
	"strings"
	"testing"
)

// The THIRD REMEDIATION.
//
// The delta review of the second remediation came back CHANGES REQUESTED
// again, and it was right about the merge for the SECOND time: the merge
// was still order-dependent, so the same two accounts published a
// different review depending on the order the caller found them in.
//
// The other findings were about a flag that could never be true, a count
// that was wrong, and counts that did not add up.
//
// Each test here names the invariant it pins.

// ── N1: the merge is a function of the accounts, not of their order ──────

// TestTheMergeDoesNotDependOnTheOrderTheAccountsArrivedIn proves the
// published review does not depend on the order the accounts arrived in.
//
// The order is the caller's accident: the corpus's candidates come from
// a ledger that was read in one order, and the tree's own correlation
// comes from a different walk. Two publishers that saw the same review
// must publish the same review.
//
// So every arrangement of the same accounts must resolve to the same
// review — and the arrangements that broke it are the ones that matter:
// the tree's claim with no verdict beside the ledger's account that has
// one, an older account that says something else, and a reported review
// that names no reviewer at all.
func TestTheMergeDoesNotDependOnTheOrderTheAccountsArrivedIn(t *testing.T) {
	// The tree's own claim: the reviewer exists, the row names the
	// moment, and it carries NO verdict — the verdict lives in the
	// ledger.
	claim := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead, UpdatedAt: 20,
	}
	// The LEDGER's account of the same review, at the same moment.
	ledger := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		Verdict: VerdictPassed, UpdatedAt: 20,
	}
	// An OLDER account of the same review: a decision that has been
	// superseded.
	older := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testOldHead,
		Verdict: VerdictChangesRequested, UpdatedAt: 10,
	}
	// A REPORTED review: no reviewer, no moment, a claim from outside
	// the substrate.
	reported := ReviewInfo{
		Exists: true, ReviewedSHA: testHead,
		Verdict: ReportedApproved, LocallyReported: true,
	}
	// Two accounts of the SAME review at the SAME moment that disagree
	// about the verdict. The substrate contradicts itself, and the
	// published review must still be the same whatever order they
	// arrived in.
	conflictA := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		Verdict: VerdictPassed, UpdatedAt: 20,
	}
	conflictB := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		Verdict: VerdictChangesRequested, UpdatedAt: 20,
	}
	arrangements := [][]ReviewInfo{
		{claim, ledger},
		{ledger, claim},
		{claim, ledger, older},
		{older, ledger, claim},
		{claim, ledger, reported},
		{reported, ledger, claim},
		{claim, older, ledger},
		{older, claim, ledger},
		{conflictA, conflictB},
		{conflictB, conflictA},
	}
	var first string
	for i, arrangement := range arrangements {
		got := resolveForTest(t, arrangement)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("the accounts %v resolved to %q, want %q — the order decided the answer",
				arrangement, got, first)
		}
	}
}

// TestASupersededAccountDoesNotFillTheCurrentAccountsGap proves an
// older account cannot come back through a gap.
//
// The tree's correlation holds the reviewer's CLAIM, and the corpus's
// candidates hold the LEDGER's account. When the claim is the newer of
// the two and carries no verdict, the older account's verdict used to
// fill the gap — and the tie rule could not take it back, because the
// gap was no longer empty.
//
// So the published review carries NO verdict: the older decision is
// superseded, and a superseded decision is not published.
func TestASupersededAccountDoesNotFillTheCurrentAccountsGap(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	// The reviewer's own claim, at the LATER moment, with no verdict.
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-r", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", UpdatedAt: 30,
	})
	// An OLDER account, with a refusal in it.
	candidates := map[string][]ReviewInfo{
		"task-a": {{
			Exists: true, ReviewerID: "task-r", ReviewedSHA: testOldHead,
			Verdict: VerdictChangesRequested, UpdatedAt: 10,
		}},
	}
	records, err := BuildWithReviews(tree, map[string]Facts{}, candidates)
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
		t.Fatal("the subject's record is missing")
	}
	if subject.ReviewVerdict != "" {
		t.Errorf("the subject carries the verdict %q — a superseded decision was published",
			subject.ReviewVerdict)
	}
	if subject.Disposition == DispositionBlocked {
		t.Errorf("the subject is %q — a superseded refusal was published",
			subject.Disposition)
	}
}

// TestTheCurrentAccountWinsTheDisagreement proves the latest moment is
// the one that is published.
//
// Two accounts of the same review, at different moments, that disagree:
// the later one is the review as it is now, and the earlier one is the
// review as it was.
func TestTheCurrentAccountWinsTheDisagreement(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-r", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", UpdatedAt: 10,
	})
	candidates := map[string][]ReviewInfo{
		"task-a": {{
			Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
			Verdict: VerdictPassed, UpdatedAt: 20,
		}},
	}
	records, err := BuildWithReviews(tree, map[string]Facts{}, candidates)
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
		t.Fatal("the subject's record is missing")
	}
	if subject.ReviewVerdict != VerdictPassed {
		t.Errorf("the subject carries the verdict %q, want %q",
			subject.ReviewVerdict, VerdictPassed)
	}
	if subject.ReviewedSHA != testHead {
		t.Errorf("the subject carries the reviewed revision %q, want %q",
			subject.ReviewedSHA, testHead)
	}
}

// TestAnAccountWithNoMomentStillFillsTheGap proves the reported review
// reaches the reader.
//
// "I do not know when this happened" is not "this happened before the
// newer account". So an account with no moment is never superseded, and
// it still fills the gaps the current account leaves — which is how a
// REPORTED review, which names no reviewer and carries no moment, gets
// to the reader at all.
func TestAnAccountWithNoMomentStillFillsTheGap(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-r", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", UpdatedAt: 10,
	})
	candidates := map[string][]ReviewInfo{
		"task-a": {{
			Exists: true, ReviewedSHA: testHead,
			Verdict: ReportedApproved, LocallyReported: true,
		}},
	}
	records, err := BuildWithReviews(tree, map[string]Facts{}, candidates)
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
		t.Fatal("the subject's record is missing")
	}
	if subject.ReviewVerdict != ReportedApproved {
		t.Errorf("the subject carries the verdict %q, want %q — the reported review was dropped",
			subject.ReviewVerdict, ReportedApproved)
	}
	if subject.ReviewDisposition != ReviewDispositionLocallyReported {
		t.Errorf("the subject's review disposition is %q, want %q",
			subject.ReviewDisposition, ReviewDispositionLocallyReported)
	}
}

// ── N2: the verifier never reads from the record's repository ───────────

// TestTheVerifierDoesNotFallBackToTheRecordsRepository proves the
// verifier reads the evidence from where the READER was told, and from
// nowhere else.
//
// A published record is DATA: it may have been fetched from anywhere,
// and its repository is a field in it. A verifier that fell back to
// that field would read from whatever repository a forged record named
// — with the reader's own credential.
//
// So a verifier that was not told where the evidence lives says that
// the check could not be made, and asks for nothing.
func TestTheVerifierDoesNotFallBackToTheRecordsRepository(t *testing.T) {
	body := []byte("the captured findings")
	record := &Record{
		FormatVersion: FormatVersion, Repository: "somebody/else", Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionFindingsCaptured,
		Provenance:        ProvenanceFor(),
	}
	record.ReviewEvidence = &EvidenceRef{
		Ref: "reviews/findings.json", SHA256: DigestBytes(body), Status: CaptureVerified,
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.content["reviews/findings.json"] = body

	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	status := ""
	detail := ""
	for _, c := range verification.Checks {
		if c.Name == CheckReviewEvidence {
			status = c.Status
			detail = c.Detail
		}
	}
	if status != CheckUnverified {
		t.Errorf("the review evidence check is %q (%s), want %q — the verifier read from the record's repository",
			status, detail, CheckUnverified)
	}
	for _, call := range gh.calls {
		if strings.Contains(call, "reviews/findings.json") {
			t.Errorf("the verifier fetched the evidence although it was not told where it lives: %s", call)
		}
	}
}

// reviewShape is one shape of review account.
type reviewShape struct {
	name     string
	verdict  string
	sha      string
	reviewer string
	at       int64
}

// TestTheMergeIsOrderIndependentAcrossTheAccounts proves the order-
// independence over the WHOLE space of shapes, not over the shapes one
// thought of.
//
// The arrangements that break a merge are the ones nobody imagines: an
// account with no moment beside one with, two accounts of the same
// moment that disagree, a reported account beside a recorded one. So
// every combination of them is built, in both orders, and every pair of
// orders must publish the same review.
func TestTheMergeIsOrderIndependentAcrossTheAccounts(t *testing.T) {
	// The accounts of one review, in the shapes that matter.
	shapes := []reviewShape{
		{"newer", VerdictPassed, testHead, "task-r", 20},
		{"older", VerdictChangesRequested, testOldHead, "task-r", 10},
		{"same-moment", VerdictPassed, testHead, "task-r", 10},
		{"no-moment", VerdictPassed, testHead, "task-r", 0},
		{"reported", ReportedApproved, testHead, "", 0},
		{"reported-later", ReportedApproved, testHead, "", 20},
	}
	// The tree's own account of the review, at every moment it could
	// be at.
	for _, a := range shapes {
		for _, b := range shapes {
			for _, treeAt := range []int64{0, 5, 10, 20, 30} {
				for _, withReviewer := range []bool{false, true} {
					forward := resolveWithTree(t, a, b, treeAt, withReviewer)
					reverse := resolveWithTree(t, b, a, treeAt, withReviewer)
					if forward != reverse {
						t.Errorf("the accounts %s and %s (tree at %d, reviewer %v) resolved to %q "+
							"in one order and %q in the other — the order decided the answer",
							a.name, b.name, treeAt, withReviewer, forward, reverse)
					}
				}
			}
		}
	}
}

// resolveWithTree builds a subject's record from two accounts and the
// tree's own account, and renders what the review came out as.
func resolveWithTree(t *testing.T, a, b reviewShape, treeAt int64, withReviewer bool) string {
	t.Helper()
	account := func(s reviewShape) ReviewInfo {
		info := ReviewInfo{
			Exists: true, ReviewerID: s.reviewer, ReviewedSHA: s.sha,
			Verdict: s.verdict, UpdatedAt: s.at,
		}
		if s.reviewer == "" {
			info.LocallyReported = true
		}
		return info
	}
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	if withReviewer {
		tree.Tasks = append(tree.Tasks, Claims{
			ID: "task-r", RootID: "root-1", ProjectID: "continuity-001",
			Title: "the review", Role: "reviewer", State: "review_passed",
			ReviewTarget: "task-a", ReviewedSHA: testHead, UpdatedAt: treeAt,
		})
	}
	records, err := BuildWithReviews(tree, map[string]Facts{},
		map[string][]ReviewInfo{"task-a": {account(a), account(b)}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, r := range records {
		if r.TaskID == "task-a" {
			return strings.Join([]string{
				r.ReviewVerdict, r.ReviewedSHA, r.Disposition, r.ReviewDisposition,
			}, "|")
		}
	}
	t.Fatal("the subject's record is missing")
	return ""
}

// resolveForTest builds a subject's record from a set of review accounts
// and renders what the review came out as.
//
// The rendering is what the comparison needs: the verdict, the reviewed
// revision and the disposition, so two arrangements that differ in any
// of them are not the same published review.
func resolveForTest(t *testing.T, accounts []ReviewInfo) string {
	t.Helper()
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	// The tree's own account of the review: the reviewer exists, and
	// the claim carries no verdict.
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-r", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", UpdatedAt: 20,
	})
	records, err := BuildWithReviews(tree, map[string]Facts{},
		map[string][]ReviewInfo{"task-a": accounts})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, r := range records {
		if r.TaskID == "task-a" {
			return strings.Join([]string{
				r.ReviewVerdict, r.ReviewedSHA, r.Disposition, r.ReviewDisposition,
			}, "|")
		}
	}
	t.Fatal("the subject's record is missing")
	return ""
}
