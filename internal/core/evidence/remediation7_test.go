package evidence

import (
	"context"
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
// record by the claim of bytes the check did not establish.
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
}

// ── 6: an index must name its source ────────────────────────────────────

// TestAnIndexWithoutItsSourceIsNotVerified proves the source check is
// not skipped when the index omits the fields.
func TestAnIndexWithoutItsSourceIsNotVerified(t *testing.T) {
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
	index.Repository = ""
	index.Branch = ""
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.IndexDetail == "" {
		t.Error("the check accepted an index that names no source")
	}
	if report.Verified {
		t.Error("the corpus is verified although the index names no source")
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
