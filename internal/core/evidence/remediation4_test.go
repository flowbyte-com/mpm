package evidence

import (
	"context"
	"strings"
	"testing"
)

// The FOURTH REMEDIATION.
//
// The third delta review came back CHANGES REQUESTED, and two of its
// findings were HIGH:
//
//   - the store publication copied the reviewer's LOCAL evidence pointer
//     into the record unscreened, so on a default install the
//     publication refused itself, and elsewhere it published the
//     operator's disk layout and the check then contradicted the
//     publisher about the same capture;
//   - the truncation flag was published and read by nobody, so a reader
//     was shown a partial queue as though it were the whole one.
//
// The others were a union that could publish a revision/verdict pair no
// account asserted, a documented tie-break that was the inverse of the
// code's, an exit status that disagreed with the report, and a record
// path built from an unvalidated index entry.
//
// Each test here names the invariant it pins.

// ── F1: a pointer on the publisher's machine is not published ────────────

// TestAPointerOnThePublishersMachineIsNotPublished proves the review's
// evidence pointer is screened like every other string.
//
// The store records the capture where the HOST wrote it, which is an
// absolute path on the publisher's machine. Publishing it would put the
// operator's disk layout inside the content-addressed bytes, and it
// would make the checker fetch a path the record chose — so the pointer
// is withheld, and the record names the field it withheld.
func TestAPointerOnThePublishersMachineIsNotPublished(t *testing.T) {
	body := []byte("the captured findings")
	record := buildSubjectWithReview(t, ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		FindingsSHA256: DigestBytes(body), CaptureStatus: CaptureVerified,
		EvidenceRef: "/home/operator/.mpm/opencode/reviews/at-1/findings/review-s.json",
		Verdict:     VerdictPassed, UpdatedAt: 20,
	})
	if record.ReviewEvidence == nil {
		t.Fatal("the record carries no review evidence at all")
	}
	if record.ReviewEvidence.Ref != Unknown {
		t.Errorf("the record publishes the pointer %q; a path on the publisher's machine is not evidence a reader can fetch",
			record.ReviewEvidence.Ref)
	}
	// The rest of the capture is NOT lost: the digest was checked, and
	// the status says so.
	if !record.ReviewEvidence.Known() {
		t.Error("withholding the pointer also lost the digest")
	}
	if record.ReviewEvidence.Status != CaptureVerified {
		t.Errorf("the capture status is %q, want %q", record.ReviewEvidence.Status, CaptureVerified)
	}
	if !SameDigest(record.ReviewEvidence.SHA256, DigestBytes(body)) {
		t.Errorf("the record's digest is %q, want the digest of the captured bytes",
			record.ReviewEvidence.SHA256)
	}
	// And the record is publishable: the withheld pointer is a value the
	// redactor chose, not a residue that the last gate refuses.
	if err := AssertPublishable(record); err != nil {
		t.Errorf("the record is not publishable: %v", err)
	}
}

// TestAWithheldPointerIsNotCalledAContradiction proves the publisher's
// answer and the reader's answer about the SAME capture do not
// contradict each other.
//
// The publisher checked the digest against bytes it could read; the
// bytes are not published, so no reader can check them. That is
// MISSING — a check that cannot be made — and not FAILED, which would
// say the record is wrong.
func TestAWithheldPointerIsNotCalledAContradiction(t *testing.T) {
	record := &Record{
		ReviewDisposition: ReviewDispositionVerdict,
		ReviewVerdict:     VerdictPassed,
		ReviewEvidence: &EvidenceRef{
			Ref:    Unknown,
			SHA256: DigestBytes([]byte("the captured findings")),
			Status: CaptureVerified,
		},
	}
	out := &Verification{}
	v := &Verifier{}
	v.checkReviewEvidence(context.Background(), out, record)
	if len(out.Checks) != 1 {
		t.Fatalf("the review evidence produced %d checks, want 1", len(out.Checks))
	}
	check := out.Checks[0]
	if check.Status == CheckFailed {
		t.Errorf("the check calls the publisher a liar: %s", check.Detail)
	}
	if check.Status != CheckMissing {
		t.Errorf("the check's status is %q, want %q", check.Status, CheckMissing)
	}
	if !strings.Contains(check.Detail, "the publisher checked the digest") {
		t.Errorf("the detail does not say what the publisher established: %s", check.Detail)
	}
}

// ── F2: a partial publication says so where the reader looks ─────────────

// TestTheQueueSummarySaysWhenThePublicationIsPartial proves the human
// artefact a reader opens first announces the truncation.
//
// The flag in index.json is a machine file the reader would have to go
// and fetch. The queue summary is where a person starts, so the
// truncation is said HERE too — a partial queue taken for the whole one
// is a fiction.
func TestTheQueueSummarySaysWhenThePublicationIsPartial(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	snapshot := "sha256:" + strings.Repeat("a", 64)
	partial := RenderQueueMarkdown(records, snapshot,
		&Corpus{Truncated: true, Scanned: 500, ScanLimit: 499})
	if !strings.Contains(partial, "PART") {
		t.Errorf("the queue summary does not say the publication is partial:\n%s", partial)
	}
	for _, want := range []string{"499", "more work"} {
		if !strings.Contains(partial, want) {
			t.Errorf("the queue summary does not carry the bound's numbers (%s):\n%s", want, partial)
		}
	}
	// A WHOLE publication does not claim to be partial.
	whole := RenderQueueMarkdown(records, snapshot, &Corpus{Scanned: 1, ScanLimit: 499})
	if strings.Contains(whole, "PART") {
		t.Errorf("a whole publication says it is partial:\n%s", whole)
	}
}

// TestTheCheckCarriesTheTruncationToTheReader proves the machine report
// carries the truncation too.
//
// The index says the publication holds part of the work, and the reader
// is the one who has to know: a flag the report drops is a flag nobody
// sees.
func TestTheCheckCarriesTheTruncationToTheReader(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := Build(tree, map[string]Facts{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	corpus := &Corpus{Truncated: true, Scanned: 500, ScanLimit: 499}
	plan, err := NewPlanFromCorpus(testRepo, EvidenceBranch, corpus, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := newFakeGitHub()
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
		if f.Path == QueueSummaryPath && !strings.Contains(string(f.Content), "PART") {
			t.Errorf("the published queue summary does not say the publication is partial:\n%s", f.Content)
		}
	}
	gh.prs[262] = mergedPR(262, testHead)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !report.Truncated {
		t.Error("the check dropped the truncation the index published")
	}
	if report.Scanned != 500 {
		t.Errorf("the report's scanned count is %d, want 500", report.Scanned)
	}
	if report.ScanLimit != 499 {
		t.Errorf("the report's scan limit is %d, want 499", report.ScanLimit)
	}
}

// ── F3: a verdict travels with the revision it was given for ─────────────

// TestAVerdictIsNeverPublishedWithARevisionNoAccountPairedItWith proves
// the union cannot publish a (revision, verdict) pair that no account
// asserted.
//
// One account knows the revision, another carries the verdict for a
// different revision. Taking the revision from one and the verdict from
// the other would publish a decision about a revision no account ever
// paired with it.
func TestAVerdictIsNeverPublishedWithARevisionNoAccountPairedItWith(t *testing.T) {
	// One half knows the revision and carries no verdict; the other
	// carries the verdict for a DIFFERENT revision.
	revisionOnly := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testOldHead, UpdatedAt: 20,
	}
	verdictOnly := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		Verdict: VerdictPassed, UpdatedAt: 20,
	}
	accounted := map[string]bool{
		testOldHead + "\x00" + "":         true,
		testHead + "\x00" + VerdictPassed: true,
	}

	for _, accounts := range [][]ReviewInfo{
		{revisionOnly, verdictOnly},
		{verdictOnly, revisionOnly},
	} {
		got := resolveReview(accounts)
		if got.Verdict != "" || got.ReviewedSHA != "" {
			if !accounted[got.ReviewedSHA+"\x00"+got.Verdict] {
				t.Errorf("the published review pairs revision %q with verdict %q, and no account asserted that pair",
					got.ReviewedSHA, got.Verdict)
			}
		}
		if got.UpdatedAt != 20 {
			t.Errorf("the published review's moment is %d, want 20", got.UpdatedAt)
		}
	}
}

// ── F6: an index may not name a record by a path ─────────────────────────

// TestAnIndexThatNamesANonRecordIdentifierIsRefused proves the reader
// never builds a path out of an index entry.
//
// A record's identifier is the hex SHA-256 of the record's bytes. An
// entry that is anything else is an index that names a file the format
// does not have, and a reader that followed it would fetch whatever the
// index said.
func TestAnIndexThatNamesANonRecordIdentifierIsRefused(t *testing.T) {
	for _, id := range []string{
		"../../etc/passwd",
		"records/../../etc/passwd",
		"/abs/path",
		"sha256:" + strings.Repeat("a", 64),
		strings.Repeat("A", 64),
		strings.Repeat("a", 63),
		"",
	} {
		raw := `{"format_version":"` + FormatVersion + `",` +
			`"published_at":"2026-11-22T09:00:00Z",` +
			`"repository":"owner/name","branch":"evidence",` +
			`"snapshot_id":"sha256:` + strings.Repeat("b", 64) + `",` +
			`"record_count":1,` +
			`"records":[{"id":"` + id + `","root_id":"root-1","task_id":"task-a"}]}`
		if _, err := ParseIndex([]byte(raw)); err == nil {
			t.Errorf("the index accepted the identifier %q", id)
		}
	}
	// And a real identifier is accepted.
	raw := `{"format_version":"` + FormatVersion + `",` +
		`"published_at":"2026-11-22T09:00:00Z",` +
		`"repository":"owner/name","branch":"evidence",` +
		`"snapshot_id":"sha256:` + strings.Repeat("b", 64) + `",` +
		`"record_count":1,` +
		`"records":[{"id":"` + strings.Repeat("c", 64) + `","root_id":"root-1","task_id":"task-a"}]}`
	if _, err := ParseIndex([]byte(raw)); err != nil {
		t.Errorf("the index refused a real record identifier: %v", err)
	}
}

// buildSubjectWithReview builds one subject with one candidate review.
func buildSubjectWithReview(t *testing.T, review ReviewInfo) *Record {
	t.Helper()
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor("task-a"))
	records, err := BuildWithReviews(tree, map[string]Facts{},
		map[string][]ReviewInfo{"task-a": {review}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, r := range records {
		if r.TaskID == "task-a" {
			return r
		}
	}
	t.Fatal("the subject's record is missing")
	return nil
}
