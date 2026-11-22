package evidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The FIFTH REMEDIATION.
//
// The fourth delta review came back CHANGES REQUESTED. The HIGH finding
// was not about a field: it was that `check` never applied the PACKAGE'S
// OWN GATES. A record the publisher would refuse was accepted, counted
// and called verified, and a record whose disposition was not the one
// its fields derive could disappear from every section while the report
// said "The queue is empty" and exited 0.
//
// The smaller findings were the same class one level down: a filter that
// matched nothing said the queue was empty, a filtered report hid the
// stale record that caused its ⚠, the human artefacts were bound only to
// the mutable index, the snapshot id was printed but never checked, a
// malformed record aborted the whole check, and the merge still paired a
// capture with another account's revision.
//
// Each test here names the invariant it pins.

// ── R1: the reader applies the package's own gates ───────────────────────

// TestARecordThePublisherWouldRefuseIsNotVerified proves `check` runs
// the same validity gates as the publisher.
//
// A record that is published but wrong is worse than a missing one,
// because it is believed. The reader is the one who believes it.
func TestARecordThePublisherWouldRefuseIsNotVerified(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *Record)
		field  string
	}{
		{
			name:   "a foreign format version",
			mutate: func(r *Record) { r.FormatVersion = "other-format" },
			field:  CheckRecord,
		},
		{
			name:   "a disposition outside the vocabulary",
			mutate: func(r *Record) { r.Disposition = "not_a_disposition" },
			field:  CheckRecord,
		},
		{
			name: "a disposition its own fields do not derive",
			mutate: func(r *Record) {
				r.State = "cancelled"
				r.Disposition = DispositionCompleted
			},
			field: CheckDisposition,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := builtRecord(t, "task-a")
			record.CI = []CIRun{greenRun(1, testHead)}
			record.Disposition = DeriveDisposition(record)
			tc.mutate(record)
			plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{record}, timeFixture())
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			gh := servedPlan(t, plan)
			gh.prs[262] = mergedPR(262, testHead)

			report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if len(report.Records) != 1 {
				t.Fatalf("the report holds %d records, want 1", len(report.Records))
			}
			rec := report.Records[0]
			if rec.Verdict == CheckVerified {
				t.Fatalf("the record is verified although the package would refuse it: %+v", rec.Verification)
			}
			if report.Verified {
				t.Error("the corpus is verified although a record fails its own gates")
			}
			found := false
			for _, c := range rec.Verification.Checks {
				if c.Name == tc.field && c.Status == CheckFailed {
					found = true
				}
			}
			if !found {
				t.Errorf("the report carries no failed %q check: %+v", tc.field, rec.Verification.Checks)
			}
			if len(report.StaleRecords()) != 1 {
				t.Errorf("the report files %d records as stale or contradictory, want 1", len(report.StaleRecords()))
			}
		})
	}
}

// ── R10: a record that cannot be read is reported, not fatal ─────────────

// TestAnUnreadableRecordIsReportedNotFatal proves a malformed record is
// a record in the report, not an error that hides the queue.
func TestAnUnreadableRecordIsReportedNotFatal(t *testing.T) {
	record := builtRecord(t, "task-a")
	records := []*Record{record}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	id, err := RecordID(record)
	if err != nil {
		t.Fatalf("the record's id: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)
	gh.content[RecordsDirectory+"/"+id+".json"] = []byte(`{"task_id":`)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("the check aborted on a malformed record: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("the report holds %d records, want 1", len(report.Records))
	}
	rec := report.Records[0]
	if rec.DigestVerified {
		t.Error("the malformed record is reported as digest-verified")
	}
	found := false
	for _, c := range rec.Verification.Checks {
		if c.Name == CheckRecord && c.Status == CheckFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("the malformed record carries no failed %q check: %+v", CheckRecord, rec.Verification.Checks)
	}
	if report.Verified {
		t.Error("the corpus is verified although a record could not be read")
	}
}

// ── R4: a filter may not hide the ⚠'s cause ─────────────────────────────

// TestAFilterCannotHideAStaleRecord proves the record that makes the
// report unverified is shown even when the filter excludes it.
func TestAFilterCannotHideAStaleRecord(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	a := claimsFor("task-a")
	a.ProjectID = "alpha"
	b := claimsFor("task-b")
	b.ProjectID = "beta"
	b.PRRef = "https://github.com/owner/name/pull/263"
	tree.Tasks = append(tree.Tasks, a, b)
	records, err := Build(tree, map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
		"task-b": {GitHub: mergedPR(263, testHead), CI: []CIRun{greenRun(2, testHead)}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)
	// The world moved on after the second record was published.
	gh.prs[263] = mergedPR(263, testOldHead)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch,
		Filters{Project: "alpha"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Verified {
		t.Fatal("the report is verified and a record is stale")
	}
	if report.RecordCount != 2 {
		t.Errorf("the corpus holds %d records, want 2", report.RecordCount)
	}
	if report.ShownCount != 2 {
		t.Errorf("the report shows %d records, want 2: the stale record may not be hidden",
			report.ShownCount)
	}
	stale := report.StaleRecords()
	if len(stale) != 1 || stale[0].TaskID != "task-b" {
		t.Errorf("the report shows %d stale records (%v), want task-b",
			len(stale), taskIDs(stale))
	}
}

// ── R5: the human artefacts are bound to the records ─────────────────────

// TestAForgedRenderingIsNotVerified proves the record's Markdown is
// re-rendered from the record, not merely hashed against the index.
//
// The index is the mutable pointer: a forger who rewrites an artefact
// and its index digest would otherwise be verified.
func TestAForgedRenderingIsNotVerified(t *testing.T) {
	record := builtRecord(t, "task-a")
	record.CI = []CIRun{greenRun(1, testHead)}
	record.Disposition = DeriveDisposition(record)
	plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{record}, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	id, err := RecordID(record)
	if err != nil {
		t.Fatalf("the record's id: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)

	// THE FORGERY: a rendering that says the review passed and the
	// operator approved the merge, with the INDEX's digest updated to
	// match. The record itself is untouched and verifies.
	path := RecordsDirectory + "/" + id + ".md"
	forged := []byte("# The work\n\n- **review**: passed · **operator approved**: merge\n")
	gh.content[path] = forged
	index := parsePlanIndex(t, plan)
	for i := range index.Renderings {
		if index.Renderings[i].Path == path {
			index.Renderings[i].SHA256 = DigestBytes(forged)
		}
	}
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	found := false
	for _, r := range report.Renderings {
		if r.Path == path {
			found = true
			if r.OK {
				t.Error("the forged rendering is verified because its index digest was updated")
			}
		}
	}
	if !found {
		t.Fatalf("the report does not name the record's rendering")
	}
	if report.Verified {
		t.Error("the corpus is verified although a human artefact is forged")
	}
}

// TestAnIndexThatNamesNoArtefactsIsNotVerified proves the required
// artefacts are the RECORDS', not the index's list.
func TestAnIndexThatNamesNoArtefactsIsNotVerified(t *testing.T) {
	record := builtRecord(t, "task-a")
	record.CI = []CIRun{greenRun(1, testHead)}
	record.Disposition = DeriveDisposition(record)
	plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{record}, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)
	index := parsePlanIndex(t, plan)
	index.Renderings = nil
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(report.Renderings) == 0 {
		t.Fatal("the check checked no artefact at all")
	}
	for _, r := range report.Renderings {
		if r.OK {
			t.Errorf("the artefact %s is verified although the index does not name it", r.Path)
		}
	}
	if report.Verified {
		t.Error("the corpus is verified although its artefacts are not named")
	}
}

// ── R11: the snapshot id is checked ──────────────────────────────────────

// TestARenamedSnapshotIsNotVerified proves the index's snapshot id is
// recomputed from the records it names.
func TestARenamedSnapshotIsNotVerified(t *testing.T) {
	record := builtRecord(t, "task-a")
	record.CI = []CIRun{greenRun(1, testHead)}
	record.Disposition = DeriveDisposition(record)
	plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{record}, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)
	index := parsePlanIndex(t, plan)
	index.SnapshotID = "sha256:" + strings.Repeat("f", 64)
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.SnapshotVerified {
		t.Error("the check accepted the index's snapshot id without recomputing it")
	}
	if report.SnapshotDetail == "" {
		t.Error("the report does not say why the snapshot id does not match")
	}
	if report.Verified {
		t.Error("the corpus is verified although its snapshot id is not the records' digest")
	}
}

// ── R12: an index may not list one record twice ──────────────────────────

// TestAnIndexThatListsARecordTwiceIsRefused proves the count in the
// index is a count of DISTINCT records.
func TestAnIndexThatListsARecordTwiceIsRefused(t *testing.T) {
	id := strings.Repeat("a", 64)
	raw := `{"format_version":"` + FormatVersion + `",` +
		`"published_at":"2026-11-22T09:00:00Z",` +
		`"repository":"owner/name","branch":"evidence",` +
		`"snapshot_id":"sha256:` + strings.Repeat("b", 64) + `",` +
		`"record_count":2,` +
		`"records":[{"id":"` + id + `","root_id":"root-1","task_id":"task-a"},` +
		`{"id":"` + id + `","root_id":"root-1","task_id":"task-a"}]}`
	if _, err := ParseIndex([]byte(raw)); err == nil {
		t.Error("the index lists one record twice and the reader accepted it")
	}
}

// ── R6: the capture's revision is the published one ──────────────────────

// TestACaptureIsNotPublishedBesideAnotherAccountsRevision proves the
// digest travels with the revision its account named.
func TestACaptureIsNotPublishedBesideAnotherAccountsRevision(t *testing.T) {
	body := []byte("the captured findings")
	decision := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		Verdict: VerdictPassed, UpdatedAt: 20,
	}
	capture := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testOldHead,
		FindingsSHA256: DigestBytes(body), CaptureStatus: CaptureVerified,
		EvidenceRef: "reviews/findings.json", UpdatedAt: 20,
	}
	for _, accounts := range [][]ReviewInfo{{decision, capture}, {capture, decision}} {
		got := resolveReview(accounts)
		if got.FindingsSHA256 != "" && got.ReviewedSHA != "" && got.findingsSHA != got.ReviewedSHA {
			t.Errorf("the review publishes the digest %q, captured for %q, beside the revision %q",
				got.FindingsSHA256, got.findingsSHA, got.ReviewedSHA)
		}
		if got.FindingsSHA256 != "" && got.ReviewedSHA != testOldHead {
			t.Errorf("the digest is for %q and the published revision is %q",
				testOldHead, got.ReviewedSHA)
		}
	}
}

// ── R8: an explicit unknown is the ABSENCE of a digest ───────────────────

// TestAnUnknownDigestDoesNotShadowAKnownOne proves two accounts that
// disagree about whether the bytes were captured resolve to the capture.
func TestAnUnknownDigestDoesNotShadowAKnownOne(t *testing.T) {
	body := []byte("the captured findings")
	unknown := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		FindingsSHA256: Unknown, CaptureStatus: CaptureNone, UpdatedAt: 20,
	}
	known := ReviewInfo{
		Exists: true, ReviewerID: "task-r", ReviewedSHA: testHead,
		FindingsSHA256: DigestBytes(body), CaptureStatus: CaptureVerified,
		EvidenceRef: "reviews/findings.json", UpdatedAt: 20,
	}
	for _, accounts := range [][]ReviewInfo{{unknown, known}, {known, unknown}} {
		got := resolveReview(accounts)
		if !SameDigest(got.FindingsSHA256, DigestBytes(body)) {
			t.Errorf("the published digest is %q, want the one account that captured the bytes",
				got.FindingsSHA256)
		}
		if got.CaptureStatus != CaptureVerified {
			t.Errorf("the published capture status is %q, want %q",
				got.CaptureStatus, CaptureVerified)
		}
	}
}

// ── R9: the Markdown says what the publisher established ────────────────

// TestTheRecordMarkdownCarriesTheCaptureStatus proves a human reading
// the record can tell a checked digest from an unchecked one.
func TestTheRecordMarkdownCarriesTheCaptureStatus(t *testing.T) {
	record := &Record{
		Title: "the work", TaskID: "task-a", RootID: "root-1",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted, ReviewDisposition: ReviewDispositionFindingsCaptured,
		ReviewEvidence: &EvidenceRef{
			Ref: Unknown, SHA256: DigestBytes([]byte("the bytes")), Status: CaptureVerified,
		},
		Provenance: ProvenanceFor(),
	}
	out := RenderRecordMarkdown(record)
	for _, want := range []string{"status `verified`", "what the publisher established"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Markdown does not say %q:\n%s", want, out)
		}
	}
}

// builtRecord builds one finished record with a verdict.
func builtRecord(t *testing.T, taskID string) *Record {
	t.Helper()
	tree := Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, claimsFor(taskID))
	records, err := Build(tree, map[string]Facts{
		taskID: {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, r := range records {
		if r.TaskID == taskID {
			return r
		}
	}
	t.Fatalf("the record %s is missing", taskID)
	return nil
}

// servedPlan publishes a plan into a fake GitHub.
func servedPlan(t *testing.T, plan *Plan) *fakeGitHub {
	t.Helper()
	gh := newFakeGitHub()
	for _, f := range plan.Files {
		gh.content[f.Path] = f.Content
	}
	return gh
}

// parsePlanIndex reads the plan's index.
func parsePlanIndex(t *testing.T, plan *Plan) *Index {
	t.Helper()
	for _, f := range plan.Files {
		if f.Path == IndexPath {
			index, err := ParseIndex(f.Content)
			if err != nil {
				t.Fatalf("parse the plan's index: %v", err)
			}
			return index
		}
	}
	t.Fatal("the plan holds no index")
	return nil
}

// writePlanIndex replaces the served index.
func writePlanIndex(t *testing.T, gh *fakeGitHub, index *Index) {
	t.Helper()
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("encode the index: %v", err)
	}
	gh.content[IndexPath] = raw
}

// taskIDs returns the task ids of a set of verified records.
func taskIDs(records []*VerifiedRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.TaskID)
	}
	return out
}
