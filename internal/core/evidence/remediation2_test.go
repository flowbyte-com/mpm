package evidence

import (
	"context"
	"strings"
	"testing"
)

// The SECOND REMEDIATION.
//
// These tests exist because the delta review found a way for the
// published evidence to say something that was not true, or for the
// check to report something it had not established. Each one names the
// invariant it pins.

// ── N1: the merge must not publish the OLDER verdict ─────────────────────

// TestTheMergeDoesNotPublishTheOlderVerdict proves the merge respects
// the order of the accounts it merges.
//
// Two accounts of ONE review — the tree's correlation and the corpus's
// candidate — are merged. The merge is not a licence to publish
// whatever the last account said: where the two DISAGREE, the LATER
// one wins, so a superseded verdict cannot come back through it.
//
// And the answer must not depend on the order the candidates arrived
// in, because the order is the caller's accident, not a fact about the
// work.
func TestTheMergeDoesNotPublishTheOlderVerdict(t *testing.T) {
	newer := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		Verdict: VerdictPassed, UpdatedAt: 20,
	}
	older := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testOldHead,
		Verdict: VerdictChangesRequested, UpdatedAt: 10,
	}
	for _, order := range [][]ReviewInfo{
		{newer, older},
		{older, newer},
	} {
		tree := Tree{RootID: "root-1"}
		tree.Tasks = append(tree.Tasks, Claims{
			ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
			Title: "the work", Role: "implementer", State: "completed",
			PRRef:   "https://github.com/owner/name/pull/262",
			BaseSHA: testBase, HeadSHA: testHead,
		})
		// The reviewer's own claim: it names the subject and the
		// moment, and it does not carry the verdict.
		tree.Tasks = append(tree.Tasks, Claims{
			ID: "task-r", RootID: "root-1", ProjectID: "continuity-001",
			Title: "the review", Role: "reviewer", State: "review_passed",
			ReviewTarget: "task-a", UpdatedAt: 10,
		})
		records, err := BuildWithReviews(tree, map[string]Facts{},
			map[string][]ReviewInfo{"task-a": order})
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
			t.Errorf("the subject carries the verdict %q, want %q — the OLDER account won",
				subject.ReviewVerdict, VerdictPassed)
		}
		if subject.ReviewedSHA != testHead {
			t.Errorf("the subject carries the reviewed revision %q, want %q — the OLDER account won",
				subject.ReviewedSHA, testHead)
		}
		if subject.Disposition == DispositionBlocked {
			t.Errorf("the subject is %q — a superseded refusal was published",
				subject.Disposition)
		}
	}
}

// TestAReportedReviewIsNotDropped proves a candidate that names no
// reviewer is MERGED rather than dropped.
//
// A reported review — one that happened outside the substrate — names
// no reviewer, so it cannot be told apart from the account of the same
// review. A dropped candidate is a verdict that never reaches the
// reader, and the subject is filed "awaiting review" forever.
func TestAReportedReviewIsNotDropped(t *testing.T) {
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
		ReviewTarget: "task-a", ReviewedSHA: testHead, UpdatedAt: 10,
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
		t.Errorf("the subject carries the verdict %q, want %q — the candidate was dropped",
			subject.ReviewVerdict, ReportedApproved)
	}
	if subject.ReviewDisposition != ReviewDispositionLocallyReported {
		t.Errorf("the subject's review disposition is %q, want %q",
			subject.ReviewDisposition, ReviewDispositionLocallyReported)
	}
}

// ── N3: a digest the publisher did not establish is not published ────────

// TestAnUnverifiableDigestIsNotPublished proves an UNCHECKED digest is
// published as the explicit unknown.
//
// The store holds a claim; the publisher is what checks it. When the
// publisher could not read the bytes, the digest is a claim it cannot
// stand behind — and publishing it would publish that claim inside the
// content-addressed bytes.
func TestAnUnverifiableDigestIsNotPublished(t *testing.T) {
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
		FindingsSHA256: "sha256:" + strings.Repeat("e", 64),
		CaptureStatus:  CaptureUnverifiable,
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
		t.Errorf("the unestablished digest was published: %q", subject.ReviewEvidence.SHA256)
	}
	if subject.ReviewEvidence.Status != CaptureUnverifiable {
		t.Errorf("the status is %q, want %q", subject.ReviewEvidence.Status,
			CaptureUnverifiable)
	}
}

// TestACheckedDigestIsPublished proves the fix did not swallow the
// digest the publisher DID establish.
//
// A publisher that refuses every digest would be as wrong as one that
// publishes every digest: the record would carry no evidence at all.
func TestACheckedDigestIsPublished(t *testing.T) {
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
		FindingsSHA256: "sha256:" + strings.Repeat("f", 64),
		CaptureStatus:  CaptureVerified,
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
	if subject.ReviewEvidence.SHA256 != "sha256:"+strings.Repeat("f", 64) {
		t.Errorf("the established digest was not published: %q", subject.ReviewEvidence.SHA256)
	}
	if subject.ReviewEvidence.Status != CaptureVerified {
		t.Errorf("the status is %q, want %q", subject.ReviewEvidence.Status, CaptureVerified)
	}
}

// ── N2: the evidence is fetched from the READER's repository ─────────────

// TestTheVerifierFetchesTheEvidenceFromTheReadersRepository proves the
// verifier reads the review's bytes where the PUBLICATION put them.
//
// The record names the repository the WORK is visible in. The operator's
// publication target is a separate choice, and the two are not always
// the same repository — so a verifier that fetched from the record's own
// repository would read from wherever a published record says to read,
// and would report a missing capture for a capture that is right there.
func TestTheVerifierFetchesTheEvidenceFromTheReadersRepository(t *testing.T) {
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

	const evidenceRepo = "owner/evidence"
	v := &Verifier{GitHub: gh, Branch: "evidence", Repository: evidenceRepo}
	verification, err := v.Verify(context.Background(), record)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, c := range verification.Checks {
		if c.Name == CheckReviewEvidence && c.Status != CheckVerified {
			t.Errorf("the review evidence check is %q (%s), want %q",
				c.Status, c.Detail, CheckVerified)
		}
	}
	// And the fetch went to the READER's repository.
	found := false
	for _, call := range gh.calls {
		if strings.Contains(call, "content "+evidenceRepo+" evidence reviews/findings.json") {
			found = true
		}
		if strings.Contains(call, "content "+testRepo+" evidence reviews/findings.json") {
			t.Errorf("the verifier fetched from the RECORD's repository: %s", call)
		}
	}
	if !found {
		t.Errorf("the verifier never fetched from %s: %v", evidenceRepo, gh.calls)
	}
}

// ── N7: the human artefacts are checked, not just named ──────────────────

// TestTheCheckVerifiesTheHumanArtefacts proves the digests the index
// gives the human artefacts are CHECKED.
//
// The index names every Markdown artefact with a digest, so a reader can
// check the Markdown a person reads as exactly as they can check the
// record a machine reads. A digest nobody checks is a claim — and a
// tampered queue summary would be invisible to a reader who trusted it.
func TestTheCheckVerifiesTheHumanArtefacts(t *testing.T) {
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
	gh.prs[262] = mergedPR(262, testHead)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(report.Renderings) == 0 {
		t.Fatal("the report verified no rendering at all")
	}
	for _, r := range report.Renderings {
		if !r.OK {
			t.Errorf("the rendering %s was not verified: %s", r.Path, r.Detail)
		}
	}

	// Now TAMPER with the queue summary, and the check must say so.
	for i, f := range plan.Files {
		if f.Path == QueueSummaryPath {
			gh.content[f.Path] = append([]byte("tampered\n"), f.Content...)
			_ = i
		}
	}
	report, err = RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	tampered := false
	for _, r := range report.Renderings {
		if r.Path == QueueSummaryPath && !r.OK {
			tampered = true
		}
	}
	if !tampered {
		t.Error("the tampered queue summary was reported as verified")
	}
	if report.Verified {
		t.Error("the report says everything verified, and a rendering was tampered with")
	}
}

// ── N10: a filter may not hide a tampered record ─────────────────────────

// TestAFilterCannotHideATamperedRecord proves the filters cannot hide
// a record whose bytes do not match its name.
//
// A filter narrows what a reader LOOKS at; it may not hide what the
// reader needs to KNOW. A reader who filtered to one project must still
// learn that another project's evidence was tampered with.
func TestAFilterCannotHideATamperedRecord(t *testing.T) {
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
		RootID: "root-1", TaskID: "task-b", Title: "the tampered work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/263",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionVerdict, ReviewVerdict: VerdictPassed,
		Provenance: ProvenanceFor(),
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{good, bad}, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := newFakeGitHub()
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
	}
	gh.prs[262] = mergedPR(262, testHead)
	gh.prs[263] = mergedPR(263, testHead)
	gh.runs[testHead] = []CIRun{greenRun(1, testHead)}

	// TAMPER with the second record's bytes, so they no longer hash to
	// the identifier the index gave them.
	badID, err := RecordID(bad)
	if err != nil {
		t.Fatalf("the record's id: %v", err)
	}
	gh.content[RecordsDirectory+"/"+badID+".json"] = []byte(`{"task_id":"task-b","tampered":true}`)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch,
		Filters{Project: "alpha"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.RecordCount != 2 {
		t.Errorf("the report says the corpus holds %d records, want 2", report.RecordCount)
	}
	found := false
	for _, r := range report.Records {
		if r.TaskID == "task-b" {
			found = true
			if r.DigestVerified {
				t.Error("the tampered record was reported as digest-verified")
			}
		}
	}
	if !found {
		t.Error("the filter hid the tampered record — a reader would never learn of it")
	}
}

// ── the check's own reporting of the capture's status ────────────────────

// TestTheCheckNamesWhyTheReviewEvidenceCannotBeChecked proves the
// check distinguishes a REFUTED digest from an unpublished one.
//
// They are different facts: one says the publisher found the bytes and
// they did not match, and the other says the publisher could not read
// them. A reader deserves to know which.
func TestTheCheckNamesWhyTheReviewEvidenceCannotBeChecked(t *testing.T) {
	record := &Record{
		FormatVersion: FormatVersion, Repository: testRepo, Project: "p",
		RootID: "root-1", TaskID: "task-a", Title: "the work",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted,
		PRRef:       "https://github.com/owner/name/pull/262",
		BaseSHA:     testBase, HeadSHA: testHead, ReviewedSHA: testHead,
		ReviewDisposition: ReviewDispositionFindingsCaptured,
		Provenance:        ProvenanceFor(),
	}
	for _, status := range []string{CaptureContradicted, CaptureUnverifiable} {
		record.ReviewEvidence = &EvidenceRef{
			Ref: "reviews/findings.json", SHA256: Unknown, Status: status,
		}
		gh := newFakeGitHub()
		gh.prs[262] = mergedPR(262, testHead)
		v := &Verifier{GitHub: gh}
		verification, err := v.Verify(context.Background(), record)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		detail := ""
		for _, c := range verification.Checks {
			if c.Name == CheckReviewEvidence {
				detail = c.Detail
			}
		}
		switch status {
		case CaptureContradicted:
			if !strings.Contains(detail, "refuted") {
				t.Errorf("the detail %q does not say the digest was refuted", detail)
			}
		case CaptureUnverifiable:
			if !strings.Contains(detail, "could not read") {
				t.Errorf("the detail %q does not say the bytes could not be read", detail)
			}
		}
	}
}

// ── the truncation is a fact, not a guess ────────────────────────────────

// TestTheCorpusKnowsWhetherAnythingWasDropped proves the corpus's
// truncation flag means what it says.
//
// A scan that merely REACHED the bound has dropped nothing, and
// publishing it as truncated would be a false alarm in every report
// that carries the flag.
func TestTheCorpusKnowsWhetherAnythingWasDropped(t *testing.T) {
	corpus := &Corpus{Truncated: false, Scanned: 500, ScanLimit: 500}
	plan, err := NewPlanFromCorpus(testRepo, EvidenceBranch, corpus, nil, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	index, err := ParseIndex(indexBytes(plan))
	if err != nil {
		t.Fatalf("parse the index: %v", err)
	}
	if index.Truncated {
		t.Error("the index says the publication was truncated although nothing was dropped")
	}
	corpus.Truncated = true
	plan, err = NewPlanFromCorpus(testRepo, EvidenceBranch, corpus, nil, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	index, err = ParseIndex(indexBytes(plan))
	if err != nil {
		t.Fatalf("parse the index: %v", err)
	}
	if !index.Truncated {
		t.Error("the index does not say the publication was truncated")
	}
}

// ── the index names the artefacts the check reads ────────────────────────

// TestTheIndexNamesEveryArtefactTheCheckReads proves the check and the
// index agree about which files are the human artefacts.
//
// The check reads what the index names. If the index named a file the
// check never read, the digest would be a claim nobody checked.
func TestTheIndexNamesEveryArtefactTheCheckReads(t *testing.T) {
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
	index, err := ParseIndex(indexBytes(plan))
	if err != nil {
		t.Fatalf("parse the index: %v", err)
	}
	published := map[string]bool{}
	for _, f := range plan.Files {
		published[f.Path] = true
	}
	for _, r := range index.Renderings {
		if !published[r.Path] {
			t.Errorf("the index names %s, which was not published", r.Path)
		}
	}
	if !published[QueueSummaryPath] {
		t.Errorf("the queue summary %s was not published", QueueSummaryPath)
	}
	found := false
	for _, r := range index.Renderings {
		if r.Path == QueueSummaryPath {
			found = true
		}
	}
	if !found {
		t.Errorf("the index does not name the queue summary: %v", index.Renderings)
	}
}

// the fixture's reviews are the fixture's own.
//
// The fixture holds one review per task, and the fixture's tree holds
// no reviewer assignment, so the corpus's candidates are the only
// account of the review there is. The package applies them as given.
func TestTheFixtureReviewsAreAppliedAsGiven(t *testing.T) {
	fixture, err := LoadFixture("continuity-001")
	if err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	corpus := &Corpus{
		Trees:   []Tree{fixture.Tree()},
		Reviews: fixture.Reviews(),
		Facts:   map[string]Facts{},
	}
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	if len(records) != len(fixture.Tasks) {
		t.Fatalf("the corpus holds %d records, want %d", len(records), len(fixture.Tasks))
	}
	for _, rec := range records {
		if rec.ReviewVerdict == "" {
			t.Errorf("the record %s carries no verdict — the fixture's review was dropped", rec.TaskID)
		}
		if rec.ReviewVerdict != ReportedApproved {
			t.Errorf("the record %s carries the verdict %q, want %q",
				rec.TaskID, rec.ReviewVerdict, ReportedApproved)
		}
		if rec.ReviewDisposition != ReviewDispositionLocallyReported {
			t.Errorf("the record %s carries the review disposition %q, want %q",
				rec.TaskID, rec.ReviewDisposition, ReviewDispositionLocallyReported)
		}
	}
}

// the reported verdict and the recorded one are different vocabularies,
// and the fixture uses the reported one.
func TestTheFixtureUsesTheReportedVocabulary(t *testing.T) {
	fixture, err := LoadFixture("continuity-001")
	if err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	for _, task := range fixture.Tasks {
		if task.ReviewDisposition != ReviewDispositionLocallyReported {
			continue
		}
		if task.ReviewVerdict != ReportedApproved {
			t.Errorf("the task %s carries the verdict %q, want %q",
				task.TaskID, task.ReviewVerdict, ReportedApproved)
		}
	}
}
