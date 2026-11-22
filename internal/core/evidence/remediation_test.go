package evidence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The REMEDIATION suite.
//
// Every test here exists because a reviewer found a way for the
// published evidence to say something that was not true, or for the
// check to report something it had not established. Each one names the
// invariant it pins.
//
// The tests are grouped by the finding they answer, so a reader can see
// what is protected and why.

// ── F1: the observed side is screened like the claims ───────────────────

// TestTheObservedSideIsScreenedLikeTheClaims proves that the GitHub
// facts and the CI runs go through the same redaction as the store's
// own claims.
//
// The pull request's title is written by whoever opened it. A review's
// body is written by whoever reviewed. A workflow's name is written by
// whoever wrote the workflow. None of that text is the substrate's, and
// none of it has been seen by the publisher — so it is exactly the text
// that must not reach the published bytes.
func TestTheObservedSideIsScreenedLikeTheClaims(t *testing.T) {
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	facts := Facts{
		GitHub: &GitHubFacts{
			Repository: testRepo, Number: 262, State: "merged",
			Title:   "the work " + secret,
			HeadSHA: testHead,
			Reviews: []GitHubReview{{
				Author: "charlie", State: "APPROVED",
				Body: "the findings " + secret,
				SHA:  testHead,
			}},
		},
		CI: []CIRun{{
			ID: 1, Name: "build " + secret, HeadSHA: testHead,
			Status: "completed", Conclusion: "success",
			Jobs: []CIJob{{Name: "test " + secret, Status: "completed", Conclusion: "success"}},
		}},
	}
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{"task-a": facts})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rec := records[0]
	if rec.GitHub == nil {
		t.Fatal("the record carries no GitHub facts")
	}
	if strings.Contains(rec.GitHub.Title, secret) {
		t.Errorf("the pull request's title still carries the credential: %q", rec.GitHub.Title)
	}
	if rec.GitHub.Title != Unknown {
		t.Errorf("the title is %q, want %q", rec.GitHub.Title, Unknown)
	}
	if len(rec.GitHub.Reviews) != 1 {
		t.Fatalf("the record carries %d reviews, want 1", len(rec.GitHub.Reviews))
	}
	if strings.Contains(rec.GitHub.Reviews[0].Body, secret) {
		t.Errorf("the review's body still carries the credential: %q", rec.GitHub.Reviews[0].Body)
	}
	if strings.Contains(rec.CI[0].Name, secret) {
		t.Errorf("the workflow's name still carries the credential: %q", rec.CI[0].Name)
	}
	if strings.Contains(rec.CI[0].Jobs[0].Name, secret) {
		t.Errorf("the job's name still carries the credential: %q", rec.CI[0].Jobs[0].Name)
	}
	// The record NAMES the fields it withheld, and never their content.
	joined := strings.Join(rec.Redactions, ",")
	for _, field := range []string{
		FieldGitHub + ".title", FieldGitHub + ".review.body",
		FieldCI + ".run.name", FieldCI + ".job.name",
	} {
		if !strings.Contains(joined, field) {
			t.Errorf("the record does not name the withheld field %q: %v", field, rec.Redactions)
		}
	}
	for _, withheld := range rec.Redactions {
		if strings.Contains(withheld, secret) {
			t.Errorf("the redaction list carries the content it withheld: %q", withheld)
		}
	}
}

// TestTheObservedSideIsScreenedInThePublishedBytes proves the same thing
// about the BYTES, not the fields.
//
// The record's fields are redacted, but the record's bytes are what a
// stranger downloads — and a digest is over those bytes. So the bytes
// are checked directly.
func TestTheObservedSideIsScreenedInThePublishedBytes(t *testing.T) {
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	facts := Facts{
		GitHub: &GitHubFacts{
			Repository: testRepo, Number: 262, State: "merged",
			Title: "the work " + secret, HeadSHA: testHead,
		},
		CI: []CIRun{{
			ID: 1, Name: "build " + secret, HeadSHA: testHead,
			Status: "completed", Conclusion: "success",
		}},
	}
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{"task-a": facts})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records,
		time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		if strings.Contains(string(f.Content), secret) {
			t.Errorf("the published file %s carries the credential", f.Path)
		}
	}
}

// TestAPublishableRecordRefusesAnUnscreenedObservation proves the LAST
// gate catches an unscreened observation.
//
// The redaction is the first line of defence. This is the second: a
// record that somehow carries a credential in an observed field is
// REFUSED, not published.
func TestAPublishableRecordRefusesAnUnscreenedObservation(t *testing.T) {
	rec := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: Unknown,
		ReviewDisposition: ReviewDispositionNone,
		Provenance:        ProvenanceFor(),
		GitHub: &GitHubFacts{
			Repository: testRepo, Number: 262, State: "merged",
			Title: "the work ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
		},
	}
	if err := AssertPublishable(rec); err == nil {
		t.Fatal("a record with a credential in its pull request title was publishable")
	}
}

// ── F4: the queue can say what it is waiting on ─────────────────────────

// TestAFinishedTaskWithNoVerdictIsAwaitingAReview proves the queue tells
// the truth about what it is waiting on.
//
// A finished task with no verdict is waiting for a REVIEWER. Filing it
// under "awaiting CI" would tell an operator that the queue is waiting
// on the wrong thing, and the reviewer would never be dispatched.
func TestAFinishedTaskWithNoVerdictIsAwaitingAReview(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	facts := map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
	}
	records, err := Build(tree, facts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := records[0].Disposition; got != DispositionAwaitingReview {
		t.Errorf("the disposition is %q, want %q", got, DispositionAwaitingReview)
	}
}

// TestAFinishedTaskWithGreenCIIsCompleted proves the rule above does not
// swallow the ordinary case.
func TestAFinishedTaskWithGreenCIIsCompleted(t *testing.T) {
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
		ReviewTarget: "task-a", ReviewedSHA: testHead,
		Verdict: VerdictPassed, UpdatedAt: 10,
	})
	facts := map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
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
		t.Fatal("the subject's record is missing")
	}
	if subject.Disposition != DispositionCompleted {
		t.Errorf("the disposition is %q, want %q", subject.Disposition, DispositionCompleted)
	}
}

// TestATaskInAnUnknownStateIsAmbiguous proves the AMBIGUOUS section can
// be produced.
//
// The substrate holds a state this publisher does not know. The record
// must not claim a place for it — the queue's own vocabulary is
// explicit, and a record that guessed would be inventing one.
func TestATaskInAnUnknownStateIsAmbiguous(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "quantum_superposition",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := records[0].Disposition; got != DispositionAmbiguous {
		t.Errorf("the disposition is %q, want %q", got, DispositionAmbiguous)
	}
}

// TestAWorkItemWaitingForTheOperatorIsAwaitingTheOperator proves the
// operator's gate still wins over the review rule.
func TestAWorkItemWaitingForTheOperatorIsAwaitingTheOperator(t *testing.T) {
	rec := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", State: "completed",
		PendingOperatorAction: "merge PR #262",
		HeadSHA:               testHead,
	}
	if got := DeriveDisposition(rec); got != DispositionAwaitingOperator {
		t.Errorf("the disposition is %q, want %q", got, DispositionAwaitingOperator)
	}
}

// ── F5: a captured digest is CHECKED before it is published ─────────────

// TestACapturedDigestIsCheckedBeforeItIsPublished proves the publisher's
// answer about a capture is the right one.
//
// A digest in the store is a CLAIM. The publisher checks it against the
// bytes, and there are three different answers — and the difference
// between them is the whole point.
func TestACapturedDigestIsCheckedBeforeItIsPublished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "findings.json")
	body := []byte(`{"findings":"the reviewer said something"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the capture: %v", err)
	}
	digest := DigestBytes(body)

	// VERIFIED: the bytes are there and they hash to the digest.
	status, reason := VerifyCapturedEvidence(digest, path)
	if status != CaptureVerified {
		t.Errorf("the status is %q (%s), want %q", status, reason, CaptureVerified)
	}

	// CONTRADICTED: the bytes are there and they do NOT.
	other := DigestBytes([]byte("something else entirely"))
	status, reason = VerifyCapturedEvidence(other, path)
	if status != CaptureContradicted {
		t.Errorf("the status is %q (%s), want %q", status, reason, CaptureContradicted)
	}

	// UNVERIFIABLE: the bytes cannot be read.
	status, reason = VerifyCapturedEvidence(digest, filepath.Join(dir, "absent.json"))
	if status != CaptureUnverifiable {
		t.Errorf("the status is %q (%s), want %q", status, reason, CaptureUnverifiable)
	}

	// NONE: no digest was recorded at all.
	status, _ = VerifyCapturedEvidence("", path)
	if status != CaptureNone {
		t.Errorf("the status is %q, want %q", status, CaptureNone)
	}
}

// TestARefutedDigestIsNotPublished proves the refusal reaches the bytes.
//
// A digest that was REFUTED is not published as though it were true: it
// becomes the explicit unknown, and the record says it was refuted.
func TestARefutedDigestIsNotPublished(t *testing.T) {
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
		ReviewTarget: "task-a", ReviewedSHA: testHead,
		FindingsSHA256: "sha256:" + strings.Repeat("b", 64),
		CaptureStatus:  CaptureContradicted,
		EvidenceRef:    "findings/review.json",
		UpdatedAt:      10,
	})
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var subject *Record
	for _, r := range records {
		if r.TaskID == "task-a" {
			subject = r
		}
	}
	if subject == nil || subject.ReviewEvidence == nil {
		t.Fatal("the subject carries no review evidence")
	}
	if subject.ReviewEvidence.SHA256 != Unknown {
		t.Errorf("the refuted digest was published: %q", subject.ReviewEvidence.SHA256)
	}
	if subject.ReviewEvidence.Status != CaptureContradicted {
		t.Errorf("the status is %q, want %q", subject.ReviewEvidence.Status, CaptureContradicted)
	}
}

// ── F3: the newest review of a subject is the one that is published ─────

// TestTheLedgerAccountOfAReviewIsNotLostToTheTreesOwn proves two
// accounts of ONE review are merged.
//
// The tree's correlation reads the reviewer's CLAIM; the corpus's
// candidates read the LEDGER. They are the same review, so they are
// merged — and before this they competed, with the claim winning the
// tie. The subject's record then carried NO verdict, which reads as
// "no review happened" and files finished work under "awaiting
// review" forever.
func TestTheLedgerAccountOfAReviewIsNotLostToTheTreesOwn(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	// The reviewer's OWN CLAIM. It names the subject and the revision
	// it examined; it does NOT carry the verdict, because the verdict
	// lives in the ledger.
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-r", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", ReviewedSHA: testHead,
		UpdatedAt: 10,
	})
	// The LEDGER's account of the SAME review.
	candidates := map[string][]ReviewInfo{
		"task-a": {{
			Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
			FindingsSHA256: "sha256:" + strings.Repeat("d", 64),
			CaptureStatus:  CaptureVerified,
			EvidenceRef:    "findings/review.json",
			Verdict:        VerdictPassed,
			UpdatedAt:      10,
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
		t.Errorf("the subject carries the verdict %q, want %q — the ledger's account was lost",
			subject.ReviewVerdict, VerdictPassed)
	}
	if subject.ReviewEvidence == nil {
		t.Fatal("the subject carries no review evidence")
	}
	if subject.ReviewEvidence.Status != CaptureVerified {
		t.Errorf("the capture's status is %q, want %q — the publisher's answer was lost",
			subject.ReviewEvidence.Status, CaptureVerified)
	}
	if subject.ReviewEvidence.SHA256 != "sha256:"+strings.Repeat("d", 64) {
		t.Errorf("the subject carries the digest %q, want the ledger's",
			subject.ReviewEvidence.SHA256)
	}
}

// TestAFinishedReviewIsNotFiledAsWaitingForAReview proves a reviewer's
// own record is filed by the REVIEW.
//
// The rules about CI and about an outstanding verdict are the SUBJECT's
// rules. A reviewer's assignment IS the review, so a finished review is
// done work — and filing it under "awaiting review" tells an operator
// the queue is waiting for a reviewer who has already finished.
func TestAFinishedReviewIsNotFiledAsWaitingForAReview(t *testing.T) {
	rec := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-r", Title: "the review",
		Recipient: "mpm-reviewer", Role: "reviewer", State: "review_passed",
		Disposition: DispositionCompleted,
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: Unknown,
		ReviewDisposition: ReviewDispositionNone,
		Provenance:        ProvenanceFor(),
	}
	if got := DeriveDisposition(rec); got != DispositionCompleted {
		t.Errorf("the disposition is %q, want %q", got, DispositionCompleted)
	}
}

// TestALiveReviewIsActive proves the reviewer rule does not file a
// review that is still running as finished.
func TestALiveReviewIsActive(t *testing.T) {
	rec := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-r", Title: "the review",
		Recipient: "mpm-reviewer", Role: "reviewer", State: "in_progress",
		Disposition: DispositionActive,
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: Unknown,
		ReviewDisposition: ReviewDispositionNone,
		Provenance:        ProvenanceFor(),
	}
	if got := DeriveDisposition(rec); got != DispositionActive {
		t.Errorf("the disposition is %q, want %q", got, DispositionActive)
	}
}

// TestTheNewestReviewOfASubjectIsPublished proves a superseded verdict
// is not published as the current one.
//
// Two reviewers, one subject: the head advanced and the work was
// re-reviewed. The later review is the one that says what the state is
// now.
func TestTheNewestReviewOfASubjectIsPublished(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	// The OLD review.
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "review-old", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the first review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", ReviewedSHA: testOldHead,
		Verdict: VerdictChangesRequested, UpdatedAt: 10,
	})
	// The NEW review.
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "review-new", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the second review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", ReviewedSHA: testHead,
		Verdict: VerdictPassed, UpdatedAt: 20,
	})
	facts := map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
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
		t.Fatal("the subject's record is missing")
	}
	if subject.ReviewVerdict != VerdictPassed {
		t.Errorf("the subject carries the verdict %q, want %q — the OLD review was published",
			subject.ReviewVerdict, VerdictPassed)
	}
	if subject.Disposition != DispositionCompleted {
		t.Errorf("the disposition is %q, want %q — a superseded refusal was published",
			subject.Disposition, DispositionCompleted)
	}
}

// TestTheNewestCandidateWinsOverTheTreesOwn proves the corpus's
// candidates and the tree's own correlation are compared by the same
// rule.
func TestTheNewestCandidateWinsOverTheTreesOwn(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
	})
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "review-old", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the first review", Role: "reviewer", State: "review_passed",
		ReviewTarget: "task-a", ReviewedSHA: testOldHead,
		Verdict: VerdictChangesRequested, UpdatedAt: 10,
	})
	candidates := map[string][]ReviewInfo{
		"task-a": {{
			Exists: true, ReviewerID: "review-new", ReviewedSHA: testHead,
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
		t.Errorf("the subject carries the verdict %q, want %q", subject.ReviewVerdict, VerdictPassed)
	}
}

// ── F6: a stale review is not hidden behind an unverifiable check ───────

// TestAStaleReviewIsNotHiddenBehindAnUnverifiedCheck proves the stale
// section asks the CHECKS, not the aggregate.
//
// The aggregate verdict is the worst check, and "unverified" outranks
// "stale". So a record whose review is stale and whose CI could not be
// read aggregates to "unverified" — and a reader would never learn that
// the review is stale.
func TestAStaleReviewIsNotHiddenBehindAnUnverifiedCheck(t *testing.T) {
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
	// The CI cannot be read.
	gh.runsErr[testOldHead] = fmt.Errorf("%w: the network is down", ErrUnavailable)
	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verification.Verdict() != CheckUnverified {
		t.Errorf("the aggregate verdict is %q, want %q", verification.Verdict(), CheckUnverified)
	}
	if !verification.Stale() {
		t.Error("the verification does not report anything as stale")
	}
	rec := &VerifiedRecord{Record: record, Verification: verification,
		Verdict: verification.Verdict()}
	stale := StaleRecords([]*VerifiedRecord{rec})
	if len(stale) != 1 {
		t.Errorf("the stale section holds %d records, want 1 — the stale review is hidden", len(stale))
	}
}

// ── F7: the evidence is looked for on the branch the reader was told ────

// TestTheEvidenceIsLookedForOnTheBranchTheReaderWasTold proves the
// verifier looks for the review's bytes where the publication put them.
//
// The branch is the READER's configuration — a record must not name its
// own branch — so the verifier has to be told it. A verifier that
// hard-coded the default would report a missing capture for a capture
// that is right there.
func TestTheEvidenceIsLookedForOnTheBranchTheReaderWasTold(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		Provenance: ProvenanceFor(),
	}
	body := []byte("the captured findings")
	record.ReviewEvidence = &EvidenceRef{
		Ref: "reviews/findings.json", SHA256: DigestBytes(body), Status: CaptureVerified,
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.content["reviews/findings.json"] = body
	v := &Verifier{GitHub: gh, Branch: "evidence/v2", Repository: testRepo}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	found := false
	for _, c := range verification.Checks {
		if c.Name == CheckReviewEvidence {
			found = true
			if c.Status != CheckVerified {
				t.Errorf("the review evidence check is %q (%s), want %q",
					c.Status, c.Detail, CheckVerified)
			}
		}
	}
	if !found {
		t.Fatal("there is no review evidence check")
	}
}

// ── F8: the decision checks are falsifiable ─────────────────────────────

// TestAVerdictForAnotherRevisionIsStale proves a verdict for a revision
// the pull request has moved past is STALE.
//
// Saying "verified" would tell a reader that the work in front of them
// was approved.
func TestAVerdictForAnotherRevisionIsStale(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testOldHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		Provenance: ProvenanceFor(),
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	status := ""
	for _, c := range verification.Checks {
		if c.Name == CheckReviewVerdict {
			status = c.Status
		}
	}
	if status != CheckStale {
		t.Errorf("the review verdict check is %q, want %q", status, CheckStale)
	}
}

// TestAMergeApprovalForAnUnmergedPullRequestIsRefused proves an
// approval to merge is checked against the pull request.
func TestAMergeApprovalForAnUnmergedPullRequestIsRefused(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "operator_approved",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		Provenance: ProvenanceFor(),
		OperatorApproval: &Approval{
			Action: "merge PR #262", Revision: testHead,
			Source: "operator", Evidence: "the gate",
		},
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.prs[262].State = "open"
	gh.prs[262].MergedAt = ""
	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	status := ""
	for _, c := range verification.Checks {
		if c.Name == CheckApproval {
			status = c.Status
		}
	}
	if status != CheckFailed {
		t.Errorf("the approval check is %q, want %q", status, CheckFailed)
	}
}

// ── F9: the verified flag covers the WHOLE corpus ───────────────────────

// TestTheVerifiedFlagCoversTheWholeCorpusNotTheFilteredView proves the
// flag is not computed after the filters.
//
// A reader who filtered to one project must not be told "verified" while
// another project's records are stale.
func TestTheVerifiedFlagCoversTheWholeCorpusNotTheFilteredView(t *testing.T) {
	good := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "alpha",
		RootID: "root-1", TaskID: "task-a", Title: "the good work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		Provenance: ProvenanceFor(),
	}
	bad := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "beta",
		RootID: "root-1", TaskID: "task-b", Title: "the stale work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/263",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		Provenance: ProvenanceFor(),
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.prs[263] = mergedPR(263, testOldHead)
	gh.runs[testHead] = []CIRun{greenRun(1, testHead)}
	gh.runs[testOldHead] = []CIRun{greenRun(2, testOldHead)}

	plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{good, bad},
		time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
	}
	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch,
		Filters{Project: "alpha"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Verified {
		t.Error("the report says everything verified, and another project's record is stale")
	}
	if report.RecordCount != 2 {
		t.Errorf("the report says the corpus holds %d records, want 2", report.RecordCount)
	}
	if report.ShownCount != 1 {
		t.Errorf("the report says %d records were shown, want 1", report.ShownCount)
	}
}

// ── F14: a record cannot name a run for another commit ──────────────────

// TestARecordThatNamesACIRunForAnotherCommitIsRefused proves the record
// cannot contradict itself about its own CI.
//
// The two halves of the package disagreed: the disposition matched a run
// against the head, and the verifier did not. Now both do.
func TestARecordThatNamesACIRunForAnotherCommitIsRefused(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		CI:         []CIRun{greenRun(1, testOldHead)},
		Provenance: ProvenanceFor(),
	}
	gh := newFakeGitHub()
	gh.prs[262] = mergedPR(262, testHead)
	gh.runs[testHead] = []CIRun{greenRun(1, testHead)}
	v := &Verifier{GitHub: gh}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	status := ""
	for _, c := range verification.Checks {
		if c.Name == CheckCI {
			status = c.Status
		}
	}
	if status != CheckFailed {
		t.Errorf("the CI check is %q, want %q", status, CheckFailed)
	}
}

// ── F17: the verifier does not fetch a path it was told to fetch ────────

// TestTheVerifierDoesNotFetchAPathOutsideTheEvidenceRepository proves the
// pointer is treated as DATA.
//
// A published record may have been fetched from anywhere, so the
// verifier fetches a path inside the evidence repository and nothing
// else.
func TestTheVerifierDoesNotFetchAPathOutsideTheEvidenceRepository(t *testing.T) {
	for _, ref := range []string{
		"../secrets.txt",
		"/etc/passwd",
		"https://elsewhere.example/bytes",
		"reviews/../../etc/passwd",
	} {
		record := &Record{
			FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
			RootID: "root-1", TaskID: "task-a", Title: "the work",
			Recipient: "mpm-implementer", Role: "implementer", State: "completed",
			Disposition: DispositionCompleted,
			PRRef:       "https://github.com/owner/name/pull/262",
			BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
			ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
			Provenance: ProvenanceFor(),
			ReviewEvidence: &EvidenceRef{
				Ref: ref, SHA256: "sha256:" + strings.Repeat("c", 64), Status: CaptureVerified,
			},
		}
		gh := newFakeGitHub()
		gh.prs[262] = mergedPR(262, testHead)
		v := &Verifier{GitHub: gh}
		verification, err := v.Verify(context.Background(), record)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, c := range verification.Checks {
			if c.Name == CheckReviewEvidence && c.Status == CheckVerified {
				t.Errorf("the pointer %q was accepted", ref)
			}
		}
		for _, call := range gh.calls {
			if strings.Contains(call, "content "+testRepo+" "+EvidenceBranch+" "+ref) {
				t.Errorf("the verifier fetched %q", ref)
			}
		}
	}
}

// ── F13: the human artefacts are named and checkable ────────────────────

// TestTheHumanArtefactsAreNamedAndCheckable proves the Markdown a
// person reads is covered by a digest.
//
// Before this, a reader who verified a record had verified nothing
// about the artefacts a human actually reads.
func TestTheHumanArtefactsAreNamedAndCheckable(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records,
		time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	index, err := ParseIndex(indexBytes(plan))
	if err != nil {
		t.Fatalf("parse the index: %v", err)
	}
	if len(index.Renderings) != 2 {
		t.Fatalf("the index names %d renderings, want 2 (the record and the queue)",
			len(index.Renderings))
	}
	for _, r := range index.Renderings {
		found := false
		for _, f := range plan.Files {
			if f.Path == r.Path {
				found = true
				if got := DigestBytes(f.Content); got != r.SHA256 {
					t.Errorf("the rendering %s is digest %s and the index says %s",
						r.Path, got, r.SHA256)
				}
			}
		}
		if !found {
			t.Errorf("the index names a rendering that was not published: %s", r.Path)
		}
	}
}

// ── F15: a truncated publication says so ────────────────────────────────

// TestThePublicationSaysWhenItIsTruncated proves the truncation is
// published.
//
// A reader that took a partial queue for the whole one would be misled,
// and a silent truncation is worse than a visible one.
func TestThePublicationSaysWhenItIsTruncated(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	corpus := &Corpus{Truncated: true, Scanned: 500, ScanLimit: 500}
	plan, err := NewPlanFromCorpus(testRepo, EvidenceBranch, corpus, records,
		time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	index, err := ParseIndex(indexBytes(plan))
	if err != nil {
		t.Fatalf("parse the index: %v", err)
	}
	if !index.Truncated {
		t.Error("the index does not say the publication was truncated")
	}
	if index.ScanLimit != 500 || index.Scanned != 500 {
		t.Errorf("the index says scanned=%d limit=%d, want 500 and 500",
			index.Scanned, index.ScanLimit)
	}
}

// ── F16: an action that was taken is not pending ───────────────────────

// TestAnApprovedTaskIsNotAlsoPending proves the store's normal state
// after an approval is publishable.
//
// The store keeps the awaited action on the row after the operator acts
// on it, so a record that published both would say the same action is
// pending and taken — and the whole publication was refused.
func TestAnApprovedTaskIsNotAlsoPending(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Role: "implementer", State: "operator_approved",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: testBase, HeadSHA: testHead,
		AwaitingAction: "merge PR #262",
		Approval: &Approval{
			Action: "merge PR #262", Revision: testHead,
			Source: "operator", Evidence: "the gate",
		},
	})
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rec := records[0]
	if rec.PendingOperatorAction != "" {
		t.Errorf("the record still says %q is pending", rec.PendingOperatorAction)
	}
	if rec.OperatorApproval == nil {
		t.Fatal("the record lost the approval")
	}
}

// ── F18: the reported verdicts are not the recorded ones ───────────────

// TestTheReportedVerdictsAreNotTheRecordedOnes proves the two
// vocabularies cannot be confused.
//
// A reader that saw "changes_requested" could not tell a reported one
// from a recorded one without reading the disposition too — and the
// whole point of keeping them apart is that a reader should not have
// to.
func TestTheReportedVerdictsAreNotTheRecordedOnes(t *testing.T) {
	if ReportedChangesRequested == VerdictChangesRequested {
		t.Errorf("the reported vocabulary %q is the recorded one",
			ReportedChangesRequested)
	}
	if ReportedApproved == VerdictPassed {
		t.Errorf("the reported vocabulary %q is the recorded one", ReportedApproved)
	}
	if !strings.HasPrefix(ReportedApproved, "reported_") {
		t.Errorf("the reported verdict %q does not say it is reported", ReportedApproved)
	}
	if !strings.HasPrefix(ReportedChangesRequested, "reported_") {
		t.Errorf("the reported verdict %q does not say it is reported", ReportedChangesRequested)
	}
}

// indexBytes returns the index a plan publishes.
func indexBytes(plan *Plan) []byte {
	for _, f := range plan.Files {
		if f.Path == IndexPath {
			return f.Content
		}
	}
	return nil
}
