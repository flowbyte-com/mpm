// pending_lesson_proposals_test.go — the operator-attention queue must
// be VISIBLE, and must never be mistaken for a fault.
//
// WHAT THIS FILE PINS
// -------------------
// `mpm lesson proposals` has always listed the review queue, but an
// operator had to already know to run it, so proposals accumulated
// silently. Status and Doctor now both surface the backlog from one
// canonical count. Three properties make that safe:
//
//  1. THE COUNT IS EXACT. ListLessonProposals is bounded (default 50);
//     the backlog total must not inherit that bound. A 120-proposal
//     queue reporting "50" would hide exactly the accumulation this
//     row exists to reveal.
//
//  2. A PENDING PROPOSAL IS NOT A FAULT. It is a decision the operator
//     has not made. It must not become a FAIL, must not inflate Doctor's
//     warning count, and must not change a healthy verdict — so an
//     install with 3 proposals waiting is still a healthy install.
//
//  3. UNAVAILABLE IS NOT ZERO. Zero is the healthy answer, so a count
//     that could not be taken must render as "unavailable" on both
//     surfaces and in JSON. Reporting 0 would turn "we could not look"
//     into an all-clear.
//
// And the trust boundary is untouched: reading the queue (in any of
// these surfaces, any number of times) never promotes, rejects or
// rewrites a proposal, and never makes one visible to a trusted read.
//
// Every test runs against mpminternal.NewTestDM(t) — a hermetic
// in-memory database. Nothing here can reach the operator's workspace.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// seedProposalRows inserts n proposals with the given status through
// SQL. Direct inserts keep the >50 truncation cases cheap and
// deterministic; the promote/reject paths are exercised through the
// real API where the behaviour matters.
func seedProposalRows(t *testing.T, dm *mpminternal.DatabaseManager, prefix, status string, n int) {
	t.Helper()
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := 0; i < n; i++ {
		_, err := tx.Exec(
			`INSERT INTO lesson_proposals (id, type, content, created, status)
			 VALUES (?, 'insight', ?, ?, ?)`,
			fmt.Sprintf("%s-%03d", prefix, i),
			fmt.Sprintf("proposal %s #%d", prefix, i),
			"2026-10-31T10:00:00Z",
			status,
		)
		if err != nil {
			t.Fatalf("insert %s #%d: %v", prefix, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// countPending is the canonical read under test.
func countPending(t *testing.T, dm *mpminternal.DatabaseManager) int {
	t.Helper()
	n, err := dm.CountPendingLessonProposals()
	if err != nil {
		t.Fatalf("CountPendingLessonProposals: %v", err)
	}
	return n
}

// captureStdoutFile redirects os.Stdout to a temp file for the duration
// of fn and returns what was written.
//
// A file rather than a pipe: a pipe needs a concurrent reader, and an
// unread pipe would block the renderer. No goroutine, no deadlock, no
// mpm-lint go-detached site.
func captureStdoutFile(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatalf("temp stdout: %v", err)
	}
	orig := os.Stdout
	os.Stdout = f
	fn()
	os.Stdout = orig
	if err := f.Close(); err != nil {
		t.Fatalf("close temp stdout: %v", err)
	}
	body, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read temp stdout: %v", err)
	}
	return string(body)
}

// attentionRow returns the single rendered line carrying `label`. Assertions
// about the lesson-proposal row must be scoped to that row: the
// dashboard also prints "0 pending" for the THEORY queue, and an
// unscoped substring check would pass or fail for the wrong reason.
func attentionRow(t *testing.T, text, label string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	t.Fatalf("no rendered row for %q in:\n%s", label, text)
	return ""
}

// statusText renders the human dashboard for dm.
func statusText(t *testing.T, dm *mpminternal.DatabaseManager) string {
	t.Helper()
	return captureStdoutFile(t, func() {
		printStatusDashboard(dm, time.Now())
	})
}

// statusJSON renders and decodes the machine dashboard for dm.
func statusJSON(t *testing.T, dm *mpminternal.DatabaseManager) map[string]json.RawMessage {
	t.Helper()
	raw := captureStdoutFile(t, func() {
		if code := printStatusJSON(dm, time.Now()); code != 0 {
			t.Errorf("printStatusJSON returned %d", code)
		}
	})
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("status JSON is not valid JSON: %v\n%s", err, raw)
	}
	return out
}

// lessonProposalsFromStatusJSON extracts the additive status key.
func lessonProposalsFromStatusJSON(t *testing.T, doc map[string]json.RawMessage) pendingLessonProposalsJSON {
	t.Helper()
	raw, ok := doc["lesson_proposals"]
	if !ok {
		t.Fatalf("status JSON has no \"lesson_proposals\" key; got keys %v", rawKeys(doc))
	}
	var lp pendingLessonProposalsJSON
	if err := json.Unmarshal(raw, &lp); err != nil {
		t.Fatalf("decode lesson_proposals: %v", err)
	}
	return lp
}

// rawKeys lists the keys of a decoded JSON object. Named distinctly:
// the package already has a keysOf with a different signature.
func rawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// doctorReport runs the Doctor service over dm.
func doctorReport(t *testing.T, dm *mpminternal.DatabaseManager) *DoctorReport {
	t.Helper()
	svc := NewDoctorService(dm)
	if svc == nil {
		t.Fatal("NewDoctorService returned nil")
	}
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}
	return report
}

// doctorText renders the human Doctor report.
func doctorText(t *testing.T, report *DoctorReport) string {
	t.Helper()
	var buf bytes.Buffer
	if err := NewDoctorRenderer(&buf, false).Render(report); err != nil {
		t.Fatalf("render doctor: %v", err)
	}
	return buf.String()
}

// breakLessonProposalCount makes the canonical count fail while leaving
// every other query working, by moving the table out from under it. A
// dropped table would be silently re-created by the next
// NewDatabaseManager (CREATE TABLE IF NOT EXISTS), which would turn the
// failure into a legitimate zero — the exact confusion this test must
// not have.
func breakLessonProposalCount(t *testing.T, dm *mpminternal.DatabaseManager) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(
		`ALTER TABLE lesson_proposals RENAME TO lesson_proposals_unavailable`,
	); err != nil {
		t.Fatalf("rename lesson_proposals: %v", err)
	}
	if _, err := dm.CountPendingLessonProposals(); err == nil {
		t.Fatal("the canonical count still succeeded after the table was moved; " +
			"the unavailable path cannot be tested")
	}
}

// ---------------------------------------------------------------------------
// 1. the canonical count is exact and untruncated
// ---------------------------------------------------------------------------

func TestPendingLessonProposals_CountIsExactAtEverySize(t *testing.T) {
	cases := []int{0, 1, 3, 49, 50, 51, 120}
	for _, n := range cases {
		t.Run(fmt.Sprintf("%d_pending", n), func(t *testing.T) {
			dm := mpminternal.NewTestDM(t)
			if n > 0 {
				seedProposalRows(t, dm, "p", "pending", n)
			}
			if got := countPending(t, dm); got != n {
				t.Errorf("CountPendingLessonProposals = %d, want %d", got, n)
			}
		})
	}
}

// TestPendingLessonProposals_CountIsNotTheListLength is the specific
// defect the separate query exists to prevent: ListLessonProposals
// truncates at its limit, so 120 pending proposals would report 50.
func TestPendingLessonProposals_CountIsNotTheListLength(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "p", "pending", 120)

	listed, err := dm.ListLessonProposals("pending", 0)
	if err != nil {
		t.Fatalf("ListLessonProposals: %v", err)
	}
	if len(listed) != 50 {
		t.Fatalf("premise broken: the default list returned %d rows, expected the documented cap of 50", len(listed))
	}

	if got := countPending(t, dm); got != 120 {
		t.Errorf("CountPendingLessonProposals = %d, want the untruncated total 120 "+
			"(the list length is %d)", got, len(listed))
	}
}

func TestPendingLessonProposals_CountExcludesReviewedProposals(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "pending", "pending", 4)
	seedProposalRows(t, dm, "promoted", "promoted", 3)
	seedProposalRows(t, dm, "rejected", "rejected", 5)

	if got := countPending(t, dm); got != 4 {
		t.Errorf("CountPendingLessonProposals = %d, want 4 (promoted and rejected must not count)", got)
	}
}

// ---------------------------------------------------------------------------
// 2. status text and JSON agree
// ---------------------------------------------------------------------------

func TestStatus_SurfacesPendingProposalsInTextAndJSON(t *testing.T) {
	for _, n := range []int{0, 1, 3, 120} {
		t.Run(fmt.Sprintf("%d_pending", n), func(t *testing.T) {
			dm := mpminternal.NewTestDM(t)
			if n > 0 {
				seedProposalRows(t, dm, "p", "pending", n)
			}

			text := statusText(t, dm)
			if !strings.Contains(text, "Attention") {
				t.Errorf("status text has no Attention section:\n%s", text)
			}
			if !strings.Contains(text, "Lesson proposals") {
				t.Errorf("status text does not mention lesson proposals:\n%s", text)
			}

			lp := lessonProposalsFromStatusJSON(t, statusJSON(t, dm))
			if lp.Unavailable {
				t.Fatalf("status JSON reports lesson proposals unavailable with %d pending", n)
			}
			if lp.Pending == nil {
				t.Fatal("status JSON has a null pending count but is not marked unavailable")
			}
			if *lp.Pending != n {
				t.Errorf("status JSON pending = %d, want %d", *lp.Pending, n)
			}

			// Text and JSON must not disagree.
			wantText := fmt.Sprintf("%d pending", n)
			if n > 0 {
				wantText = fmt.Sprintf("%d pending review", n)
			}
			if !strings.Contains(text, wantText) {
				t.Errorf("status text does not contain %q:\n%s", wantText, text)
			}
		})
	}
}

func TestStatus_ZeroPendingIsStatedAndQuiet(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	text := statusText(t, dm)
	if row := attentionRow(t, text, "Lesson proposals"); !strings.Contains(row, "0 pending") {
		t.Errorf("zero pending must be stated on the lesson-proposal row, got %q", row)
	}
	if strings.Contains(text, "mpm lesson proposals") {
		t.Errorf("zero pending must not print the review command as a call to action:\n%s", text)
	}
}

func TestStatus_NonZeroPendingNamesTheReviewCommand(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "p", "pending", 3)
	text := statusText(t, dm)
	if !strings.Contains(text, "mpm lesson proposals") {
		t.Errorf("a non-empty queue must tell the operator how to act on it:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// 3. doctor agrees with status, and pending is not a fault
// ---------------------------------------------------------------------------

func TestDoctor_SurfacesPendingProposalsAndAgreesWithStatus(t *testing.T) {
	for _, n := range []int{0, 1, 3, 120} {
		t.Run(fmt.Sprintf("%d_pending", n), func(t *testing.T) {
			dm := mpminternal.NewTestDM(t)
			if n > 0 {
				seedProposalRows(t, dm, "p", "pending", n)
			}

			report := doctorReport(t, dm)
			if report.Attention == nil {
				t.Fatal("Doctor produced no Attention section")
			}
			if report.Attention.LessonProposals == nil {
				t.Fatal("Doctor Attention has no lesson_proposals field")
			}
			if report.Attention.LessonProposals.Unavailable {
				t.Fatalf("Doctor reports the count unavailable with %d pending", n)
			}
			if report.Attention.LessonProposals.Pending == nil {
				t.Fatal("Doctor lesson_proposals.pending is null without being marked unavailable")
			}
			if got := *report.Attention.LessonProposals.Pending; got != n {
				t.Errorf("Doctor pending = %d, want %d", got, n)
			}

			// Doctor and Status must report the same number.
			if statusN := *lessonProposalsFromStatusJSON(t, statusJSON(t, dm)).Pending; statusN != n {
				t.Errorf("status reports %d pending, Doctor reports %d", statusN, n)
			}

			text := doctorText(t, report)
			if !strings.Contains(text, "Lesson proposals") {
				t.Errorf("Doctor text does not mention lesson proposals:\n%s", text)
			}
			if n == 0 && !strings.Contains(attentionRow(t, text, "Lesson proposals"), "0 pending") {
				t.Errorf("Doctor must state 0 pending, got %q", attentionRow(t, text, "Lesson proposals"))
			}
			if n > 0 && !strings.Contains(attentionRow(t, text, "Lesson proposals"), fmt.Sprintf("%d pending review", n)) {
				t.Errorf("Doctor must state %d pending review, got %q",
					n, attentionRow(t, text, "Lesson proposals"))
			}
			if n > 0 && !strings.Contains(text, "mpm lesson proposals") {
				t.Errorf("Doctor text must name the review command:\n%s", text)
			}
			if n == 0 && strings.Contains(text, "mpm lesson proposals") {
				t.Errorf("zero pending must not be a call to action:\n%s", text)
			}
		})
	}
}

// TestDoctor_PendingProposalsAreNotAFailure is the core safety
// property: operator work must never make a healthy install look ill.
func TestDoctor_PendingProposalsAreNotAFailure(t *testing.T) {
	base := mpminternal.NewTestDM(t)
	baseline := doctorReport(t, base)
	baseline.Tally()

	withQueue := mpminternal.NewTestDM(t)
	seedProposalRows(t, withQueue, "p", "pending", 7)
	report := doctorReport(t, withQueue)
	report.Tally()

	if report.Failed != 0 {
		t.Errorf("pending proposals produced %d FAIL check(s); they are not faults", report.Failed)
	}
	if report.Warnings != baseline.Warnings {
		t.Errorf("pending proposals changed the warning count: %d -> %d; "+
			"they must not inflate it", baseline.Warnings, report.Warnings)
	}
	if report.Passed != baseline.Passed {
		t.Errorf("pending proposals changed the pass count: %d -> %d",
			baseline.Passed, report.Passed)
	}
	// The verdict itself must be unchanged: a clean install with a queue
	// is still clean.
	if exitCodeFor(report) != exitCodeFor(baseline) {
		t.Errorf("exit code changed from %d to %d because of pending proposals",
			exitCodeFor(baseline), exitCodeFor(report))
	}
}

// exitCodeFor mirrors handleDoctor's exit-code rule.
func exitCodeFor(report *DoctorReport) int {
	switch {
	case report.Failed > 0:
		return 2
	case report.Warnings > 0:
		return 1
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// 4. unavailable is never reported as zero
// ---------------------------------------------------------------------------

func TestStatus_UnavailableIsNotZero(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "p", "pending", 4)
	breakLessonProposalCount(t, dm)

	text := statusText(t, dm)
	row := attentionRow(t, text, "Lesson proposals")
	if !strings.Contains(row, "unavailable") {
		t.Errorf("the lesson-proposal row must say unavailable, got %q", row)
	}
	if strings.Contains(row, "0 pending") {
		t.Errorf("the lesson-proposal row reports 0 pending for a count that failed: %q", row)
	}

	lp := lessonProposalsFromStatusJSON(t, statusJSON(t, dm))
	if !lp.Unavailable {
		t.Error("status JSON must mark the count unavailable")
	}
	if lp.Pending != nil {
		t.Errorf("status JSON must report a null pending count when unavailable, got %d", *lp.Pending)
	}
}

func TestDoctor_UnavailableIsNotZero(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "p", "pending", 4)
	breakLessonProposalCount(t, dm)

	report := doctorReport(t, dm)
	if report.Attention == nil || report.Attention.LessonProposals == nil {
		t.Fatal("Doctor dropped the Attention section when the count failed")
	}
	if !report.Attention.LessonProposals.Unavailable {
		t.Error("Doctor must mark the count unavailable")
	}
	if report.Attention.LessonProposals.Pending != nil {
		t.Errorf("Doctor must report a null pending count when unavailable, got %d",
			*report.Attention.LessonProposals.Pending)
	}

	// A failed COUNT of operator work must not take down the operational
	// and cluster sections alongside it.
	if report.Attention.Unavailable != nil {
		t.Errorf("a lesson-count failure nilled the whole Attention section (component %q); "+
			"the failure must be confined to its own field",
			report.Attention.Unavailable.Component)
	}

	text := doctorText(t, report)
	row := attentionRow(t, text, "Lesson proposals")
	if !strings.Contains(row, "unavailable") {
		t.Errorf("Doctor's lesson-proposal row must say unavailable, got %q", row)
	}
	if strings.Contains(row, "0 pending") {
		t.Errorf("Doctor's lesson-proposal row reports 0 pending for a count that failed: %q", row)
	}

	// And it is still not a fault.
	report.Tally()
	if report.Failed != 0 {
		t.Errorf("an unavailable count produced %d FAIL check(s)", report.Failed)
	}
}

// ---------------------------------------------------------------------------
// 5. the trust boundary and the data are untouched by reading
// ---------------------------------------------------------------------------

// proposalSnapshot is everything a read must not change.
type proposalSnapshot struct {
	statuses     map[string]string
	reviewedAt   map[string]int64
	trustedCount int
}

func snapshotProposalState(t *testing.T, dm *mpminternal.DatabaseManager) proposalSnapshot {
	t.Helper()
	snap := proposalSnapshot{
		statuses:   map[string]string{},
		reviewedAt: map[string]int64{},
	}
	rows, err := dm.SQLDB().Query(
		`SELECT id, status, COALESCE(reviewed_at, -1) FROM lesson_proposals`)
	if err != nil {
		t.Fatalf("snapshot proposals: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		var reviewed int64
		if err := rows.Scan(&id, &status, &reviewed); err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		snap.statuses[id] = status
		snap.reviewedAt[id] = reviewed
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	trusted, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("count trusted lessons: %v", err)
	}
	snap.trustedCount = len(trusted)
	return snap
}

func TestSurfacingTheQueueDoesNotMutateProposalsOrLessons(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "pending", "pending", 3)
	seedProposalRows(t, dm, "promoted", "promoted", 1)
	seedProposalRows(t, dm, "rejected", "rejected", 1)

	before := snapshotProposalState(t, dm)

	// Read every surface several times. A display must never decide.
	for i := 0; i < 3; i++ {
		_ = countPending(t, dm)
		_ = statusText(t, dm)
		_ = lessonProposalsFromStatusJSON(t, statusJSON(t, dm))
		report := doctorReport(t, dm)
		report.Tally()
		_ = doctorText(t, report)
	}

	after := snapshotProposalState(t, dm)

	if len(before.statuses) != len(after.statuses) {
		t.Fatalf("proposal count changed: %d -> %d", len(before.statuses), len(after.statuses))
	}
	for id, want := range before.statuses {
		if got := after.statuses[id]; got != want {
			t.Errorf("proposal %s status changed %q -> %q; reading the queue must not decide it", id, want, got)
		}
	}
	for id, want := range before.reviewedAt {
		if got := after.reviewedAt[id]; got != want {
			t.Errorf("proposal %s reviewed_at changed %d -> %d", id, want, got)
		}
	}
	if before.trustedCount != after.trustedCount {
		t.Errorf("trusted lesson count changed %d -> %d; a pending proposal must never become trusted by being displayed",
			before.trustedCount, after.trustedCount)
	}
}

// TestPendingProposalsStayOutOfTrustedReads pins the boundary from the
// other side: a pending proposal is not a lesson, and reading the queue
// does not change that.
func TestPendingProposalsStayOutOfTrustedReads(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "p", "pending", 3)
	// One trusted lesson, written the only way a lesson is written.
	if _, err := dm.AddLesson("a trusted operator lesson", "insight", nil, ""); err != nil {
		t.Fatalf("AddLesson: %v", err)
	}

	report := doctorReport(t, dm)
	report.Tally()
	_ = statusText(t, dm)

	lessons, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	if len(lessons) != 1 {
		t.Fatalf("trusted lesson count = %d, want 1 (the operator lesson only)", len(lessons))
	}
	for _, l := range lessons {
		if strings.HasPrefix(l.Content, "proposal ") {
			t.Errorf("a pending proposal leaked into the trusted lesson surface: %q", l.Content)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. the count follows the lifecycle
// ---------------------------------------------------------------------------

func TestCountDecreasesAfterPromotionAndRejection(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	seedProposalRows(t, dm, "p", "pending", 3)
	if got := countPending(t, dm); got != 3 {
		t.Fatalf("initial pending = %d, want 3", got)
	}

	// Promotion is the operator path, and it is the ONLY path from a
	// proposal to a trusted lesson.
	lesson, _, _, err := dm.PromoteLessonProposal("p-000")
	if err != nil {
		t.Fatalf("PromoteLessonProposal: %v", err)
	}
	if lesson == nil {
		t.Fatal("promotion produced no trusted lesson")
	}
	if got := countPending(t, dm); got != 2 {
		t.Errorf("after promotion pending = %d, want 2", got)
	}

	if _, err := dm.RejectLessonProposal("p-001"); err != nil {
		t.Fatalf("RejectLessonProposal: %v", err)
	}
	if got := countPending(t, dm); got != 1 {
		t.Errorf("after rejection pending = %d, want 1", got)
	}

	// Both surfaces follow on the next read.
	if lp := lessonProposalsFromStatusJSON(t, statusJSON(t, dm)); lp.Pending == nil || *lp.Pending != 1 {
		t.Errorf("status JSON did not follow the lifecycle: %+v", lp)
	}
	if att := doctorReport(t, dm).Attention; att == nil || att.LessonProposals == nil ||
		att.LessonProposals.Pending == nil || *att.LessonProposals.Pending != 1 {
		t.Errorf("Doctor did not follow the lifecycle: %+v", att)
	}
}

// ---------------------------------------------------------------------------
// 7. existing contracts are preserved
// ---------------------------------------------------------------------------

func TestStatusJSON_ExistingKeysPreserved(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	doc := statusJSON(t, dm)

	// Every key the status JSON published before this change must still
	// be present with its meaning intact.
	for _, k := range []string{
		"uptime", "mode", "persona", "memories", "directives",
		"theories", "decisions", "synthesis",
	} {
		if _, ok := doc[k]; !ok {
			t.Errorf("status JSON lost the pre-existing key %q; keys now: %v", k, rawKeys(doc))
		}
	}
	// The new key is additive, not a replacement.
	if _, ok := doc["lesson_proposals"]; !ok {
		t.Errorf("status JSON is missing the additive key %q", "lesson_proposals")
	}

	// Nested shapes are unchanged.
	var theories map[string]int
	if err := json.Unmarshal(doc["theories"], &theories); err != nil {
		t.Fatalf("decode theories: %v", err)
	}
	for _, k := range []string{"total", "pending", "resolved"} {
		if _, ok := theories[k]; !ok {
			t.Errorf("theories lost the key %q", k)
		}
	}
}

func TestDoctorAttention_ExistingFieldsPreserved(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	report := doctorReport(t, dm)
	if report.Attention == nil {
		t.Fatal("Attention section absent")
	}
	raw, err := json.Marshal(report.Attention)
	if err != nil {
		t.Fatalf("marshal attention: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal attention: %v", err)
	}
	for _, k := range []string{
		"operational_issues_7d", "operational_events", "audit_clusters",
		"security_events_7d", "lesson_proposals",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("Doctor Attention JSON lost/missed the key %q; keys now: %v", k, rawKeys(got))
		}
	}
}

// TestStatusJSON_IsReachableFromTheCLI is the regression for a
// pre-existing defect found while building this feature: handleStatus
// implemented --json, printStatusJSON rendered it, and the router's
// option registry declared NO options for `status` — so the guard
// rejected the flag before dispatch and the machine-readable status was
// unreachable. The route test lives here because the JSON contract this
// change extends is the one that was broken.
func TestStatusJSON_IsReachableFromTheCLI(t *testing.T) {
	if _, ok := cliOptionRegistry["status"]; !ok {
		t.Fatal("`status` is missing from cliOptionRegistry")
	}
	accepted := map[string]bool{}
	for _, name := range cliOptionRegistry["status"] {
		accepted[name] = true
	}
	for _, want := range []string{"json", "j"} {
		if !accepted[want] {
			t.Errorf("`status` does not declare %q; `mpm status --json` would be rejected by the router", want)
		}
	}
	// And the handler really does read the flag.
	src, err := os.ReadFile(filepath.Join(".", "handlers_status.go"))
	if err != nil {
		t.Fatalf("read handlers_status.go: %v", err)
	}
	if !strings.Contains(string(src), "ExtractJSONFlag") {
		t.Error("handleStatus no longer reads the JSON flag; the registry entry would be dead")
	}
}
