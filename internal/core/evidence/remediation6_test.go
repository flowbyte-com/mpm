package evidence

import (
	"context"
	"strings"
	"testing"
)

// The SIXTH REMEDIATION.
//
// The fifth delta review came back CHANGES REQUESTED. Its FIRST finding
// was that the re-rendering had REPLACED the digest check instead of
// adding to it: a rendering whose bytes were the record's but whose
// index digest was not was reported `ok` and counted verified.
//
// The others were the same class one level down: the version did not
// move when the renderings and the derivation did, so a legitimate
// older publication was accused of forgery; a misfiled record was still
// FILED under its false disposition; a filtered view could hide the
// cause of its own ⚠; the summary's expectation trusted the index's
// mutable metadata; the index entry's other fields were never checked;
// the record Markdown claimed a checked digest with status `none`;
// duplicate renderings were accepted; `shown_count`'s doc was stale;
// an uncomputable expectation was counted verified; and the snapshot
// detail said more than was recomputed.
//
// Each test here names the invariant it pins.

// ── 1: the artefact's index digest is checked, every time ────────────────

// TestTheArtefactDigestIsCheckedToo proves the digest the index
// published is checked even when the bytes are the right rendering.
//
// A reader may record that digest; an artefact whose bytes do not hash
// to it is not the artefact the index names, however plausible it is.
func TestTheArtefactDigestIsCheckedToo(t *testing.T) {
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
	for i := range index.Renderings {
		if index.Renderings[i].Path == QueueSummaryPath {
			index.Renderings[i].SHA256 = "sha256:" + strings.Repeat("f", 64)
		}
	}
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	found := false
	for _, r := range report.Renderings {
		if r.Path == QueueSummaryPath {
			found = true
			if r.OK {
				t.Error("the summary is verified although its index digest names other bytes")
			}
			if !strings.Contains(r.Detail, "hashes to") {
				t.Errorf("the detail does not say the digest does not match: %s", r.Detail)
			}
		}
	}
	if !found {
		t.Fatal("the report does not name the queue summary")
	}
	if report.Verified {
		t.Error("the corpus is verified although an artefact's published digest is not its bytes'")
	}
}

// ── 2: a version the reader does not understand is refused, not accused ──

// TestAPublicationUnderAnotherVersionIsRefused proves the version is
// the compatibility signal.
//
// The renderings and the derivation are part of the contract. When they
// change, the version changes — otherwise a reader checks an older
// publication against rules it never used and calls legitimate evidence
// forged.
func TestAPublicationUnderAnotherVersionIsRefused(t *testing.T) {
	raw := `{"format_version":"mpm.evidence/v1",` +
		`"published_at":"2026-11-22T09:00:00Z",` +
		`"repository":"owner/name","branch":"evidence",` +
		`"snapshot_id":"sha256:` + strings.Repeat("b", 64) + `",` +
		`"record_count":0,"records":[]}`
	if _, err := ParseIndex([]byte(raw)); err == nil {
		t.Error("the reader accepted a format version it does not understand")
	} else if !strings.Contains(err.Error(), "format version") {
		t.Errorf("the refusal does not name the version: %v", err)
	}
}

// ── 3: a misfiled record is FILED where its fields put it ────────────────

// TestAMisfiledRecordIsFiledByItsDerivation proves the queue's
// sections use the derived disposition, not the record's claim.
//
// A section headed "completed" must not contain a cancelled task, even
// when the record files itself there.
func TestAMisfiledRecordIsFiledByItsDerivation(t *testing.T) {
	record := builtRecord(t, "task-a")
	record.CI = []CIRun{greenRun(1, testHead)}
	record.State = "cancelled"
	record.Disposition = DispositionCompleted
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
	for _, section := range report.Sections {
		if section.Disposition == DispositionCompleted && len(section.Records) > 0 {
			t.Errorf("a cancelled task is filed under %q", DispositionCompleted)
		}
	}
	blocked := false
	for _, section := range report.Sections {
		if section.Disposition == DispositionBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("the derived disposition %q is not a section: %+v", DispositionBlocked, report.Sections)
	}
	// And a filter on ANOTHER disposition still shows it: a failed
	// record is one no filter may hide, and the section it is filed in
	// is the derived one, which the assertions above pin.
	filtered, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch,
		Filters{Disposition: DispositionCompleted})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if filtered.ShownCount != 1 {
		t.Errorf("the filtered view shows %d records, want the failed record a filter may not hide",
			filtered.ShownCount)
	}
}

// ── 4: a filtered view counts what it does not show ──────────────────────

// TestAFilteredViewCountsWhatItDoesNotShow proves the report explains
// a ⚠ whose cause the filter excluded.
func TestAFilteredViewCountsWhatItDoesNotShow(t *testing.T) {
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
	gh.prs[263] = mergedPR(263, testHead)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch,
		Filters{Project: "alpha"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Verified {
		t.Fatal("the report is verified and a record outside the view is not")
	}
	if report.UnverifiedOutside != 1 {
		t.Errorf("the report says %d records outside the view are not verified, want 1",
			report.UnverifiedOutside)
	}
}

// ── 5: the summary's expectation does not trust the index's metadata ─────

// TestTheSummaryAndTheIndexMustAgree proves a disagreement between the
// summary and the index's truncation fields is reported as a
// disagreement, not as a forged summary.
func TestTheSummaryAndTheIndexMustAgree(t *testing.T) {
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
	index.Truncated = true
	index.Scanned = 499
	index.ScanLimit = 499
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Verified {
		t.Error("the corpus is verified although the summary and the index disagree about the partial publication")
	}
	for _, r := range report.Renderings {
		if r.Path != QueueSummaryPath {
			continue
		}
		if r.OK {
			t.Error("the summary is verified although it is not the rendering the index's fields imply")
		}
		if !strings.Contains(r.Detail, "index's snapshot and truncation fields") {
			t.Errorf("the detail blames the artefact without naming the index's fields: %s", r.Detail)
		}
	}
}

// ── 6: the index entry must say what the record says ─────────────────────

// TestAnIndexEntryThatContradictsItsRecordIsReported proves the index's
// own fields are checked against the record it names.
//
// A reader who does not run the check reads the index; an entry that
// says another task, title or disposition is a pointer that lies about
// the bytes it points at.
func TestAnIndexEntryThatContradictsItsRecordIsReported(t *testing.T) {
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
	index.Records[0].TaskID = "task-evil"
	index.Records[0].Title = "a lie"
	writePlanIndex(t, gh, index)

	report, err := RunCheck(context.Background(), gh, testRepo, EvidenceBranch, Filters{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("the report holds %d records, want 1", len(report.Records))
	}
	rec := report.Records[0]
	found := false
	for _, c := range rec.Verification.Checks {
		if c.Name == CheckIndex && c.Status == CheckFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("the index contradicts the record and no %q check failed: %+v",
			CheckIndex, rec.Verification.Checks)
	}
	// The report's record is the RECORD's, not the index's lie.
	if rec.TaskID != "task-a" {
		t.Errorf("the report's record says %q, and the record says task-a", rec.TaskID)
	}
	if report.Verified {
		t.Error("the corpus is verified although the index contradicts its record")
	}
}

// ── 7: the Markdown does not claim an unchecked digest ───────────────────

// TestTheMarkdownDoesNotClaimAnUncheckedDigest proves the record page
// says a digest was checked only when the status says it was.
func TestTheMarkdownDoesNotClaimAnUncheckedDigest(t *testing.T) {
	record := &Record{
		Title: "the work", TaskID: "task-a", RootID: "root-1",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		Disposition: DispositionCompleted, ReviewDisposition: ReviewDispositionFindingsCaptured,
		ReviewEvidence: &EvidenceRef{
			Ref: Unknown, SHA256: DigestBytes([]byte("the bytes")), Status: CaptureNone,
		},
		Provenance: ProvenanceFor(),
	}
	out := RenderRecordMarkdown(record)
	if strings.Contains(out, "what the publisher established") {
		t.Errorf("the Markdown claims a digest was checked, and its status is %q:\n%s",
			CaptureNone, out)
	}
}

// ── 8: one artefact is named once ────────────────────────────────────────

// TestAnIndexThatNamesAnArtefactTwiceIsRefused proves the reader does
// not silently pick one of two digests for one artefact.
func TestAnIndexThatNamesAnArtefactTwiceIsRefused(t *testing.T) {
	raw := `{"format_version":"` + FormatVersion + `",` +
		`"published_at":"2026-11-22T09:00:00Z",` +
		`"repository":"owner/name","branch":"evidence",` +
		`"snapshot_id":"sha256:` + strings.Repeat("b", 64) + `",` +
		`"record_count":0,"records":[],` +
		`"renderings":[{"path":"summaries/queue.md","sha256":"sha256:` + strings.Repeat("c", 64) + `"},` +
		`{"path":"summaries/queue.md","sha256":"sha256:` + strings.Repeat("d", 64) + `"}]}`
	if _, err := ParseIndex([]byte(raw)); err == nil {
		t.Error("the index names one artefact twice and the reader accepted it")
	}
}
