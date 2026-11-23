package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The SEVENTH REMEDIATION.
//
// The sixth delta review came back CHANGES REQUESTED. Its HIGH finding
// was the SAME CLASS as the one before it: an artefact the index names
// that is not a rendering of any record was reported `ok` on its digest
// alone, and the corpus stayed verified — so a branch writer could
// publish an "operator approved" page beside a record that carries no
// approval.
//
// The rest were the same class one level down: two records for one
// assignment were verified and shown twice; the tests' `verified`
// assertions were vacuous because their records were never verified for
// another reason; the derived `--disposition` lookup was unobservable;
// digest-mismatched bytes were filed by their own claim; an index that
// omitted its source was accepted; the Markdown claimed a checked digest
// with an unknown one; and three field docs lagged the semantics.
//
// Each test here names the invariant it pins.

// ── 1: an artefact outside the layout is not part of a publication ──────

// TestAnIndexThatNamesAnArtefactOutsideTheLayoutIsRefused proves the
// published layout is exhaustive.
//
// A digest is not a rendering. An index that names an extra page has no
// record it could be the rendering of, so a reader that verified it on
// its digest alone would believe whatever the page said.
func TestAnIndexThatNamesAnArtefactOutsideTheLayoutIsRefused(t *testing.T) {
	id := strings.Repeat("a", 64)
	cases := []string{
		"records/" + id + "-approved.md",
		"notes/whatever.md",
		"records/" + id + ".txt",
		"records/" + strings.Repeat("b", 64) + ".md", // a record the index does not list
		"summaries/queue.md",
	}
	for i, path := range cases {
		renderings := `[{"path":"` + path + `","sha256":"sha256:` + strings.Repeat("c", 64) + `"}]`
		if path == QueueSummaryPath {
			renderings = `[{"path":"` + QueueSummaryPath + `","sha256":"sha256:` + strings.Repeat("c", 64) + `"}]`
		}
		raw := `{"format_version":"` + FormatVersion + `",` +
			`"published_at":"2026-11-22T09:00:00Z",` +
			`"repository":"owner/name","branch":"evidence",` +
			`"snapshot_id":"sha256:` + strings.Repeat("d", 64) + `",` +
			`"record_count":1,` +
			`"records":[{"id":"` + id + `","root_id":"root-1","task_id":"task-a"}],` +
			`"renderings":` + renderings + `}`
		_, err := ParseIndex([]byte(raw))
		if path == QueueSummaryPath {
			if err != nil {
				t.Errorf("the reader refused the queue summary: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("case %d: the index names %q and the reader accepted it", i, path)
		}
	}
}

// ── 2: one assignment has one record ────────────────────────────────────

// TestAnIndexThatListsTwoRecordsForOneAssignmentIsRefused proves the
// reader applies the publisher's own uniqueness rule.
//
// Two records for one task are not two facts about it, and a queue that
// showed the task twice would tell a reader there were two.
func TestAnIndexThatListsTwoRecordsForOneAssignmentIsRefused(t *testing.T) {
	raw := `{"format_version":"` + FormatVersion + `",` +
		`"published_at":"2026-11-22T09:00:00Z",` +
		`"repository":"owner/name","branch":"evidence",` +
		`"snapshot_id":"sha256:` + strings.Repeat("d", 64) + `",` +
		`"record_count":2,` +
		`"records":[{"id":"` + strings.Repeat("a", 64) + `","root_id":"root-1","task_id":"task-a"},` +
		`{"id":"` + strings.Repeat("b", 64) + `","root_id":"root-1","task_id":"task-a"}]}`
	if _, err := ParseIndex([]byte(raw)); err == nil {
		t.Error("the index lists two records for one assignment and the reader accepted it")
	}
}

// ── 3: a broken artefact makes the corpus unverified ────────────────────

// TestABrokenArtefactMakesTheCorpusUnverified proves the rendering check
// decides the corpus verdict, on a corpus that otherwise verifies.
//
// The earlier tests asserted `report.Verified == false` on corpora that
// were already unverified for another reason; this one is verified until
// the artefact is broken.
func TestABrokenArtefactMakesTheCorpusUnverified(t *testing.T) {
	record, body := verifiedRecord(t, "task-a")
	plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{record}, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)
	gh.runs[testHead] = []CIRun{greenRun(1, testHead)}
	gh.content["reviews/findings.json"] = body

	clean, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !clean.Verified {
		t.Fatalf("the clean corpus is not verified; the test cannot measure the artefact: %+v",
			clean.Records[0].Verification.Checks)
	}

	// THE FORGERY: a page that says the operator approved the merge,
	// with the INDEX's digest updated to match.
	forged := []byte("# The work\n\n- **operator approved**: merge\n")
	gh.content[QueueSummaryPath] = forged
	index := parsePlanIndex(t, plan)
	for i := range index.Renderings {
		if index.Renderings[i].Path == QueueSummaryPath {
			index.Renderings[i].SHA256 = DigestBytes(forged)
		}
	}
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Records[0].Verdict != CheckVerified {
		t.Fatalf("the record is not verified; the test cannot measure the artefact: %+v",
			report.Records[0].Verification.Checks)
	}
	if report.Verified {
		t.Error("the corpus is verified although its queue summary was replaced")
	}
}

// ── 5: bytes that are not the record are not filed by their claim ───────

// TestATamperedRecordIsFiledAsAmbiguous proves the queue does not file a
// record by the claim of bytes the check did not establish, and that the
// page and the summary in such a corpus still have their published
// digests checked.
func TestATamperedRecordIsFiledAsAmbiguous(t *testing.T) {
	record := builtRecord(t, "task-a")
	record.CI = []CIRun{greenRun(1, testHead)}
	record.State = "cancelled"
	record.Disposition = DispositionBlocked
	plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{record}, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	id, err := RecordID(record)
	if err != nil {
		t.Fatalf("the record's id: %v", err)
	}
	// REPLACE THE BYTES with a valid record that claims "completed",
	// while the index still names the cancelled record's digest.
	fake := builtRecord(t, "task-a")
	fake.CI = []CIRun{greenRun(1, testHead)}
	fake.Disposition = DispositionCompleted
	raw, err := CanonicalJSON(fake)
	if err != nil {
		t.Fatalf("encode the forged record: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)
	gh.content[RecordsDirectory+"/"+id+".json"] = raw

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, section := range report.Sections {
		if section.Disposition == DispositionCompleted && len(section.Records) > 0 {
			t.Errorf("a tampered record is filed under %q, which its own bytes claim", DispositionCompleted)
		}
	}
	ambiguous := false
	for _, section := range report.Sections {
		if section.Disposition == DispositionAmbiguous {
			ambiguous = true
		}
	}
	if !ambiguous {
		t.Errorf("the tampered record is not filed as %q: %+v", DispositionAmbiguous, report.Sections)
	}
	// AND THE PAGE FOR THAT RECORD IS NOT VERIFIED. The check could not
	// establish the record, so there is nothing to re-render the page
	// from — a digest alone is not a rendering, and the page is the
	// place a reader would look for the claim the bytes make.
	page := RecordsDirectory + "/" + id + ".md"
	foundPage := false
	for _, r := range report.Renderings {
		if r.Path != page {
			continue
		}
		foundPage = true
		if r.OK {
			t.Error("the page for a tampered record is reported as verified")
		}
		if r.Detail == "" {
			t.Error("the page for a tampered record is not verified and says nothing about it")
		}
	}
	if !foundPage {
		t.Errorf("the report does not name the record's page %s", page)
	}

	// THE SUMMARY IS NOT VERIFIED FOR THE !clean REASON — the record
	// could not be established — and its published digest was checked.
	summaryReason := ""
	for _, r := range report.Renderings {
		if r.Path == QueueSummaryPath {
			if r.OK {
				t.Error("the queue summary is verified while a record could not be established")
			}
			summaryReason = r.Detail
		}
	}
	if !strings.Contains(summaryReason, "while a record could not be established") {
		t.Errorf("the summary's reason is %q, want the unestablished-record reason", summaryReason)
	}

	// AND ITS PUBLISHED DIGEST IS CHECKED: a page whose bytes are not
	// the ones the index names says so, rather than being reported as
	// "could not be established".
	gh.content[page] = []byte("# a different page\n")
	report, err = RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	assertRenderingDigestProblem(t, report, page)

	// THE SUMMARY IS FETCHED AND ITS PUBLISHED DIGEST IS CHECKED TOO,
	// even though it cannot be re-rendered while a record could not be
	// established.
	gh.content[QueueSummaryPath] = []byte("# a different summary\n")
	report, err = RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	assertRenderingDigestProblem(t, report, QueueSummaryPath)
}

// assertRenderingDigestProblem requires the named artefact to be not
// verified because its bytes do not hash to the digest the index
// published.
func assertRenderingDigestProblem(t *testing.T, report *CheckReport, path string) {
	t.Helper()
	for _, r := range report.Renderings {
		if r.Path != path {
			continue
		}
		if r.OK {
			t.Errorf("%s is verified although its bytes are not the ones the index names", path)
		}
		if !strings.Contains(r.Detail, "hashes to") {
			t.Errorf("%s's detail is %q, want it to say the bytes do not hash to the index's digest",
				path, r.Detail)
		}
		return
	}
	t.Errorf("the report does not name the artefact %s", path)
}

// ── 5c: a page the index omits is reported ──────────────────────────────

// TestAPageTheIndexOmitsIsReported proves the required set is the
// RECORDS', not the index's list, even for a record the check could not
// establish.
//
// The page of an established record is required; so is the page of an
// unestablished one, because the reader would otherwise believe a digest
// was checked that was never read.
func TestAPageTheIndexOmitsIsReported(t *testing.T) {
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
	// THE RECORD IS NOT THE ONE THE INDEX NAMES, so its page cannot be
	// re-rendered.
	gh.content[RecordsDirectory+"/"+id+".json"] = []byte(`{"task_id":"not-a-record"}`)
	index := parsePlanIndex(t, plan)
	page := RecordsDirectory + "/" + id + ".md"
	kept := index.Renderings[:0]
	for _, r := range index.Renderings {
		if r.Path != page {
			kept = append(kept, r)
		}
	}
	index.Renderings = kept
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	found := false
	for _, r := range report.Renderings {
		if r.Path != page {
			continue
		}
		found = true
		if r.OK {
			t.Error("the omitted page is reported as verified")
		}
		if !strings.Contains(r.Detail, "does not name") {
			t.Errorf("the page's detail is %q, want it to say the index does not name it", r.Detail)
		}
	}
	if !found {
		t.Errorf("the report does not name the omitted page %s", page)
	}
	if report.Verified {
		t.Error("the corpus is verified although a required page is not named")
	}
}

// ── 5d: the snapshot is not the established subset's digest ─────────────

// TestASnapshotOfTheEstablishedSubsetIsNotVerified proves the snapshot is
// not recomputed over a subset.
//
// An index can name a snapshot that is the digest of the records the
// check COULD establish while another listed record is not the one the
// index names. Recomputing over the subset would call that verified; the
// subset's digest is not the whole corpus's identifier.
func TestASnapshotOfTheEstablishedSubsetIsNotVerified(t *testing.T) {
	tree := Tree{RootID: "root-1"}
	a := claimsFor("task-a")
	b := claimsFor("task-b")
	b.PRRef = "https://github.com/owner/name/pull/263"
	tree.Tasks = append(tree.Tasks, a, b)
	records, err := Build(tree, map[string]Facts{
		"task-a": {GitHub: mergedPR(262, testHead), CI: []CIRun{greenRun(1, testHead)}},
		"task-b": {GitHub: mergedPR(263, testHead), CI: []CIRun{greenRun(2, testHead)}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var recA, recB *Record
	for _, r := range records {
		switch r.TaskID {
		case "task-a":
			recA = r
		case "task-b":
			recB = r
		}
	}
	if recA == nil || recB == nil {
		t.Fatalf("the two records are not both present")
	}
	plan, err := NewPlan(testRepo, EvidenceBranch, records, timeFixture())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	gh := servedPlan(t, plan)
	gh.prs[262] = mergedPR(262, testHead)
	gh.prs[263] = mergedPR(263, testHead)

	// ONE RECORD IS NOT THE ONE THE INDEX NAMES, and the index's
	// snapshot is the digest of the ESTABLISHED SUBSET.
	idB, err := RecordID(recB)
	if err != nil {
		t.Fatalf("the record's id: %v", err)
	}
	gh.content[RecordsDirectory+"/"+idB+".json"] = []byte(`{"task_id":"not-a-record"}`)
	subset, err := SnapshotID([]*Record{recA})
	if err != nil {
		t.Fatalf("the subset's snapshot: %v", err)
	}
	index := parsePlanIndex(t, plan)
	index.SnapshotID = subset
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.SnapshotVerified {
		t.Error("the check verified the snapshot against the established subset")
	}
	if !strings.Contains(report.SnapshotDetail, "could not be established") {
		t.Errorf("the report's snapshot detail is %q, want it to say the snapshot could not be checked",
			report.SnapshotDetail)
	}
	if report.Verified {
		t.Error("the corpus is verified although one record is not the one the index names")
	}
}

// ── 6: an index must name its source, and it must be the source read ─────

// TestTheIndexSourceMustMatchTheReader proves every clause of the source
// check — a missing repository, a wrong one, a missing branch, a wrong
// one — and that a match is accepted.
func TestTheIndexSourceMustMatchTheReader(t *testing.T) {
	cases := []struct {
		name   string
		repo   string
		branch string
		want   string // a substring the contradiction's detail must carry; "" for a match
	}{
		{"the source matches", testRepo, EvidenceBranch, ""},
		{"the repository's case differs", strings.ToUpper(testRepo), EvidenceBranch, ""},
		{"no repository", "", EvidenceBranch, "names no repository"},
		{"another repository", "other/name", EvidenceBranch, "lives in other/name"},
		{"no branch", testRepo, "", "names no branch"},
		{"another branch", testRepo, "evidence/v2", "lives on the branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record, body := verifiedRecord(t, "task-a")
			plan, err := NewPlan(testRepo, EvidenceBranch, []*Record{record}, timeFixture())
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			gh := servedPlan(t, plan)
			gh.prs[262] = mergedPR(262, testHead)
			gh.runs[testHead] = []CIRun{greenRun(1, testHead)}
			gh.content["reviews/findings.json"] = body
			index := parsePlanIndex(t, plan)
			index.Repository = tc.repo
			index.Branch = tc.branch
			writePlanIndex(t, gh, index)

			report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if tc.want != "" {
				// THE DETAIL NAMES THE CLAUSE THAT FIRED, so a table
				// cannot pass because some other contradiction produced
				// a detail.
				if !strings.Contains(report.IndexDetail, tc.want) {
					t.Errorf("the detail is %q, want it to name %q", report.IndexDetail, tc.want)
				}
				if report.Verified {
					t.Error("the corpus is verified although the index contradicts its source")
				}
				return
			}
			if report.IndexDetail != "" {
				t.Errorf("the check reports a contradiction for a matching source: %s", report.IndexDetail)
			}
			if !report.Verified {
				t.Errorf("the matching publication is not verified: %+v",
					report.Records[0].Verification.Checks)
			}
		})
	}
}

// ── 7: the Markdown does not claim a digest that is unknown ─────────────

// TestTheMarkdownDoesNotClaimADigestThatIsUnknown proves the sentence
// needs BOTH the status and a digest.
func TestTheMarkdownDoesNotClaimADigestThatIsUnknown(t *testing.T) {
	record := &Record{
		Title: "the work", TaskID: "task-a", RootID: "root-1",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted, ReviewDisposition: ReviewDispositionFindingsCaptured,
		ReviewEvidence: &EvidenceRef{
			Ref: Unknown, SHA256: Unknown, Status: CaptureVerified,
		},
		Provenance: ProvenanceFor(),
	}
	out := RenderRecordMarkdown(record)
	if strings.Contains(out, "what the publisher established") {
		t.Errorf("the Markdown claims a checked digest, and the digest is unknown:\n%s", out)
	}
}

// ── 5b: the configured target is trimmed before it is validated ─────────

// TestThePublicationTargetIsTrimmed proves the publisher does not write
// an index naming a padded repository.
//
// A configuration with surrounding whitespace would otherwise publish
// " owner/name", and every reader at "owner/name" would be told the index
// contradicts its source.
func TestThePublicationTargetIsTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	raw := []byte(`{"repository":"  owner/name  ","branch":"  evidence  "}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	target, err := LoadTarget(path)
	if err != nil {
		t.Fatalf("load the target: %v", err)
	}
	if target.Repository != testRepo {
		t.Errorf("the target's repository is %q, want %q", target.Repository, testRepo)
	}
	if target.Branch != EvidenceBranch {
		t.Errorf("the target's branch is %q, want %q", target.Branch, EvidenceBranch)
	}
}

// verifiedRecord builds one record that verifies completely, and the
// bytes of the review evidence it names.
func verifiedRecord(t *testing.T, taskID string) (*Record, []byte) {
	t.Helper()
	record := builtRecord(t, taskID)
	record.CI = []CIRun{greenRun(1, testHead)}
	record.ReviewedSHA = testHead
	record.ReviewDisposition = ReviewDispositionVerdict
	record.ReviewVerdict = VerdictPassed
	body := []byte("the captured findings")
	record.ReviewEvidence = &EvidenceRef{
		Ref:    "reviews/findings.json",
		SHA256: DigestBytes(body),
		Status: CaptureVerified,
	}
	record.Disposition = DeriveDisposition(record)
	return record, body
}
