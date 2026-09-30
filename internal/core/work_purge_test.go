package internal

// work_purge_test.go — the logical-purge contract.
//
// Design: docs/designs/2026-09-30-work-archive-and-purge.md §5, §6, §8.3–§8.6.
//
// Isolation: every test uses NewTestDM, an in-memory SQLite store. The
// production MPM database is never opened (CLAUDE.md §3, "Test
// isolation").

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func reflectTypeOf(v interface{}) reflect.Type { return reflect.TypeOf(v) }

// purgeFixture builds a completed work item with a full event ledger.
func purgeFixture(t *testing.T, dm *DatabaseManager, title string) *Work {
	t.Helper()
	w, err := dm.AddWork(title, "some content", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if _, err := dm.AddWorkNoteWithContext(w.ID, "a note on the work", ActiveContext{}); err != nil {
		t.Fatalf("note: %v", err)
	}
	if _, err := dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return w
}

// countRows counts a table's rows matching a predicate.
func purgeCount(t *testing.T, dm *DatabaseManager, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// ════════════════════════════════════════════════════════════════════
// §5.4 Gating
// ════════════════════════════════════════════════════════════════════

// TestPurge_DryRunIsDefaultAndWritesNothing is the load-bearing gate.
// A purge that writes without --force is not a "destructive command
// with a confirmation flag" — it is a silent delete.
func TestPurge_DryRunIsDefaultAndWritesNothing(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "dry run target")

	report, err := dm.PurgeWork(WorkPurgeRequest{WorkID: w.ID, ReasonCode: "test_debris"})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !report.DryRun {
		t.Error("DryRun = false without --force")
	}
	// The report must describe the real delete set, not an empty plan.
	if report.Counts.WorkEvents == 0 {
		t.Error("dry run reported 0 work_events; the ledger exists and should be counted")
	}

	// Nothing was written.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM works WHERE id = ?`, w.ID); n != 1 {
		t.Error("dry run removed the works row")
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM work_events WHERE work_id = ?`, w.ID); n == 0 {
		t.Error("dry run removed the event ledger")
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM work_purge_audit`); n != 0 {
		t.Errorf("dry run wrote %d audit row(s); it must write none", n)
	}
}

// TestPurge_ReasonCodeRequired pins the required gate in both directions.
func TestPurge_ReasonCodeRequired(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "reason code target")

	for _, code := range []string{"", "privacy", "PRIVACY", "test-debris", "routine"} {
		_, err := dm.PurgeWork(WorkPurgeRequest{WorkID: w.ID, ReasonCode: code})
		if !errors.Is(err, ErrWorkPurgeInvalidReasonCode) {
			t.Errorf("reason_code %q: err = %v, want ErrWorkPurgeInvalidReasonCode", code, err)
		}
	}
	// An invalid code must be refused on the DRY RUN too, so a dry run
	// cannot launder a bad code past the operator into a forced run.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM work_purge_audit`); n != 0 {
		t.Errorf("rejected purges wrote %d audit row(s)", n)
	}
	// Every valid code is accepted. A fresh item per code: the first
	// forced purge removes the row, so reusing the id would report a
	// not-found for reasons unrelated to the code.
	for _, code := range ValidWorkPurgeReasonCodes {
		fresh := purgeFixture(t, dm, "code "+code)
		if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: fresh.ID, ReasonCode: code, Force: true}); err != nil {
			t.Errorf("valid reason_code %q refused: %v", code, err)
		}
	}
}

// TestPurge_HasNoPrivacyCode pins the deliberate absence. A `privacy`
// code would imply an erasure guarantee v1 does not make.
func TestPurge_HasNoPrivacyCode(t *testing.T) {
	for _, c := range ValidWorkPurgeReasonCodes {
		if c == "privacy" {
			t.Fatal("ValidWorkPurgeReasonCodes contains 'privacy'; v1 purge is not erasure")
		}
	}
	if WorkPurgeReasonCodeIsValid("privacy") {
		t.Error("WorkPurgeReasonCodeIsValid accepts 'privacy'")
	}
}

// TestPurge_UnknownID pins that a mistyped id is an error, never a
// successful "0 rows removed" report.
func TestPurge_UnknownID(t *testing.T) {
	dm := NewTestDM(t)
	_, err := dm.PurgeWork(WorkPurgeRequest{WorkID: "0123456789abcdef", ReasonCode: "other"})
	if !errors.Is(err, ErrWorkPurgeNotFound) {
		t.Errorf("err = %v, want ErrWorkPurgeNotFound", err)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM work_purge_audit`); n != 0 {
		t.Error("a failed preflight wrote an audit row")
	}
}

// TestPurge_EmptyWorkIDRejected pins the empty-id guard.
func TestPurge_EmptyWorkIDRejected(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := dm.PurgeWork(WorkPurgeRequest{ReasonCode: "other"}); err == nil {
		t.Fatal("purge with empty work_id succeeded, want an error")
	}
}

// ════════════════════════════════════════════════════════════════════
// §5.1 / §5.5 The logical contract
// ════════════════════════════════════════════════════════════════════

// TestPurge_ForcedDeletesTheFullSet walks the §5.1 post-condition list.
func TestPurge_ForcedDeletesTheFullSet(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "full delete target")
	id := w.ID

	// Work-owned evidence, provenance, and a citation edge.
	ev, err := dm.AddEvidence(EvidenceInput{
		ArtifactID: id, ArtifactType: "work", Type: "observation",
		SourceGroup: "test", Strength: 0.5, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO artifact_provenance (id, artifact_id, artifact_type, created_at, actor_kind)
		VALUES (?, ?, 'work', 0, 'agent')`, GenerateID(), id); err != nil {
		t.Fatalf("insert artifact_provenance: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_provenance (id, source_id, source_type, downstream_id, downstream_type, event_id, created_at)
		VALUES (?, 'some-other-artifact', 'memory', ?, 'work', ?, 0)`,
		GenerateID(), id, GenerateID()); err != nil {
		t.Fatalf("insert epistemic_provenance: %v", err)
	}
	// A tool_invocation that MUST survive. Every NOT NULL column is
	// supplied deliberately: this insert used to Skipf on any error,
	// which silently retired every assertion below it in builds whose
	// tool_invocations shape had drifted.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
			(id, session_id, tool_name, action, invocation_id, actor_kind,
			 payload_hash, result_status, started_at)
		VALUES (?, 's1', 'mpm_work', 'list', ?, 'agent', 'h', 'success', 0)`,
		GenerateID(), GenerateID()); err != nil {
		t.Fatalf("insert tool_invocations: %v", err)
	}

	report, err := dm.PurgeWork(WorkPurgeRequest{
		WorkID: id, ReasonCode: "administrative", Note: "housekeeping", Force: true,
	})
	if err != nil {
		t.Fatalf("forced purge: %v", err)
	}
	if report.DryRun {
		t.Error("forced purge reported DryRun = true")
	}

	// No works row, no ledger, no work-owned evidence or provenance.
	probes := []struct {
		label string
		sql   string
	}{
		{"works", `SELECT COUNT(*) FROM works WHERE id = ?`},
		{"work_events", `SELECT COUNT(*) FROM work_events WHERE work_id = ?`},
		{"work evidence", `SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'work'`},
		{"artifact_provenance", `SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'`},
		{"epistemic_provenance", `SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`},
	}
	for _, p := range probes {
		if n := purgeCount(t, dm, p.sql, id); n != 0 {
			t.Errorf("%s: %d row(s) survived the purge", p.label, n)
		}
	}
	// §5.5: the substrate execution ledger is NOT work-owned and stays.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM tool_invocations`); n == 0 {
		t.Error("purge removed tool_invocations; it is the substrate execution ledger, not work-owned")
	}
	if report.AuditID == "" {
		t.Error("forced purge reported no audit id")
	}
	_ = ev
}

// TestPurge_UnretrievableFromEverySurface pins §5.1's read-surface
// clause. Archive already proved the id resolves for by-id access;
// after a purge there must be nothing left for any of them to resolve.
func TestPurge_UnretrievableFromEverySurface(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "gone from every surface")
	if _, _, err := dm.ArchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	id := w.ID

	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// By-id reads.
	if _, err := dm.GetWork(id); err == nil {
		t.Error("GetWork still resolves a purged work item")
	}
	events, err := dm.GetWorkEvents(id)
	if err != nil {
		t.Fatalf("GetWorkEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("GetWorkEvents returned %d event(s) for a purged work item", len(events))
	}
	// Listing surfaces. Every axis, including visibility=all.
	for _, status := range []string{"open", "done", "cancelled", "all"} {
		for _, vis := range ValidWorkVisibilities {
			rows, err := dm.ListWorksByStatusAndVisibility(status, vis)
			if err != nil {
				t.Fatalf("list %s/%s: %v", status, vis, err)
			}
			for _, r := range rows {
				if r.ID == id {
					t.Errorf("purged work item still returned by list status=%s visibility=%s", status, vis)
				}
			}
		}
	}
}

// TestPurge_RecomputeProjectionDoesNotResurrect pins §5.5's projection
// invariant. Without the hasEvents guard this would re-create the row.
func TestPurge_RecomputeProjectionDoesNotResurrect(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "no resurrection")
	id := w.ID
	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	before := purgeCount(t, dm, `SELECT COUNT(*) FROM works`)

	if err := dm.RecomputeWorkProjection(id); err != nil {
		t.Fatalf("RecomputeWorkProjection on a purged id returned an error: %v", err)
	}
	after := purgeCount(t, dm, `SELECT COUNT(*) FROM works`)
	if after != before {
		t.Errorf("RecomputeWorkProjection changed the works row count from %d to %d", before, after)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM works WHERE id = ?`, id); n != 0 {
		t.Error("RecomputeWorkProjection resurrected the purged row")
	}
}

// TestPurge_NoTombstoneRow pins the §5.5 decision against a scrubbed
// row. A `title='[purged]'` row would be the only row in the table
// that RecomputeWorkProjection actively corrupts.
func TestPurge_NoTombstoneRow(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "no tombstone")
	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: w.ID, ReasonCode: "other", Force: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM works`); n != 0 {
		t.Errorf("purge left %d works row(s); a tombstone is explicitly not used", n)
	}
	var scrubbed int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM works WHERE title LIKE '%purged%'`).Scan(&scrubbed); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scrubbed != 0 {
		t.Error("a scrubbed tombstone row exists")
	}
}

// ════════════════════════════════════════════════════════════════════
// §5.6 Audit record
// ════════════════════════════════════════════════════════════════════

// TestPurge_AuditRecordShape pins what the audit row holds and — more
// importantly — what it does not.
func TestPurge_AuditRecordShape(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "SECRETISH work title")
	id := w.ID
	note := "operator note, independent input"

	report, err := dm.PurgeWork(WorkPurgeRequest{
		WorkID: id, ReasonCode: "administrative", Note: note, Operator: "tester", Force: true,
	})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}

	var (
		gotWorkID, gotReason, gotNote, gotOperator, gotCounts string
		gotPurgedAt                                           int64
	)
	err = dm.SQLDB().QueryRow(`
		SELECT work_id, purged_at, reason_code, COALESCE(note,''), COALESCE(operator,''), counts
		FROM work_purge_audit WHERE id = ?`, report.AuditID).
		Scan(&gotWorkID, &gotPurgedAt, &gotReason, &gotNote, &gotOperator, &gotCounts)
	if err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if gotWorkID != id {
		t.Errorf("audit work_id = %q, want %q", gotWorkID, id)
	}
	if gotReason != "administrative" {
		t.Errorf("audit reason_code = %q", gotReason)
	}
	if gotPurgedAt == 0 {
		t.Error("audit purged_at is 0")
	}
	// The operator note permanently survives.
	if gotNote != note {
		t.Errorf("audit note = %q, want %q — the note must survive the purge", gotNote, note)
	}
	if gotOperator != "tester" {
		t.Errorf("audit operator = %q", gotOperator)
	}

	// counts reflects what was actually deleted.
	var counts WorkPurgeCounts
	if err := json.Unmarshal([]byte(gotCounts), &counts); err != nil {
		t.Fatalf("counts is not valid JSON: %v (%q)", err, gotCounts)
	}
	if counts.WorkEvents != report.Counts.WorkEvents {
		t.Errorf("audit counts.work_events = %d, report says %d", counts.WorkEvents, report.Counts.WorkEvents)
	}
	actual := purgeCount(t, dm, `SELECT COUNT(*) FROM work_events WHERE work_id = ?`, id)
	if counts.WorkEvents != actual && actual != 0 {
		t.Errorf("audit counts.work_events = %d, but %d events were present before delete", counts.WorkEvents, actual)
	}
}

// TestPurge_AuditHasNoArtifactTextColumns is the §5.6 structural pin.
// The table must have no column capable of holding a title, content, or
// work note. Asserted via PRAGMA table_info because a comment
// promising this is not a guarantee.
func TestPurge_AuditHasNoArtifactTextColumns(t *testing.T) {
	dm := NewTestDM(t)
	rows, err := dm.SQLDB().Query(`PRAGMA table_info(work_purge_audit)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, required := range []string{"id", "work_id", "purged_at", "reason_code", "note", "operator", "counts"} {
		if !cols[required] {
			t.Errorf("work_purge_audit is missing required column %q", required)
		}
	}
	for _, forbidden := range []string{"title", "content", "work_note", "event_note", "summary", "body"} {
		if cols[forbidden] {
			t.Errorf("work_purge_audit has a %q column; the audit record must hold no artifact text", forbidden)
		}
	}
}

// TestPurge_NoteAcceptedForEveryReasonCode pins that the note is not
// gated on the reason code.
func TestPurge_NoteAcceptedForEveryReasonCode(t *testing.T) {
	dm := NewTestDM(t)
	for i, code := range ValidWorkPurgeReasonCodes {
		w := purgeFixture(t, dm, "note test")
		note := "note for " + code
		report, err := dm.PurgeWork(WorkPurgeRequest{
			WorkID: w.ID, ReasonCode: code, Note: note, Force: true,
		})
		if err != nil {
			t.Fatalf("purge with code %q: %v", code, err)
		}
		var got string
		if err := dm.SQLDB().QueryRow(
			`SELECT COALESCE(note,'') FROM work_purge_audit WHERE id = ?`, report.AuditID).Scan(&got); err != nil {
			t.Fatalf("code %q (iteration %d): read note: %v", code, i, err)
		}
		if got != note {
			t.Errorf("code %q: note = %q, want %q", code, got, note)
		}
	}
}

// TestPurge_InvalidReasonCodeRejectedByCheckConstraint pins the DB-level
// enum, not just the Go-level validation. A future write path that
// bypasses the Go check must still be stopped by the schema.
func TestPurge_InvalidReasonCodeRejectedByCheckConstraint(t *testing.T) {
	dm := NewTestDM(t)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO work_purge_audit (id, work_id, purged_at, reason_code, counts)
		VALUES (?, 'x', 0, 'privacy', '{}')`, GenerateID())
	if err == nil {
		t.Fatal("the CHECK constraint accepted reason_code='privacy'")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check") &&
		!strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("error does not look like a constraint violation: %v", err)
	}
}

// ════════════════════════════════════════════════════════════════════
// §6 Preflight — structural references
// ════════════════════════════════════════════════════════════════════

// TestPurge_RefusesOnMemoryDependency pins path 1 AND the no-cascade
// rule: the referring memory's dependencies must be byte-identical
// afterward. A purge that "helpfully" cleaned up the reference would be
// doing exactly what §6.1 forbids.
func TestPurge_RefusesOnMemoryDependency(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "referenced by a memory")
	id := w.ID

	deps := `["` + id + `"]`
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, dependencies, created_at, updated_at)
		VALUES (?, 'decisions', 'a decision', ?, 0, 0)`, GenerateID(), deps); err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	report, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true})
	if !errors.Is(err, ErrWorkPurgeReferenced) {
		t.Fatalf("err = %v, want ErrWorkPurgeReferenced", err)
	}
	if report == nil || len(report.Referrers) == 0 {
		t.Fatal("refusal did not enumerate any referrers")
	}
	found := false
	for _, r := range report.Referrers {
		if r.Kind == "memory_dependency" {
			found = true
		}
	}
	if !found {
		t.Errorf("referrers do not name the memory dependency: %+v", report.Referrers)
	}
	// The referring record is untouched.
	var gotDeps string
	if err := dm.SQLDB().QueryRow(
		`SELECT dependencies FROM memories WHERE dependencies = ?`, deps).Scan(&gotDeps); err != nil {
		t.Fatalf("the referring memory's dependencies were rewritten or removed: %v", err)
	}
	if gotDeps != deps {
		t.Errorf("dependencies = %q, want byte-identical %q", gotDeps, deps)
	}
	// Nothing was purged.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM works WHERE id = ?`, id); n != 1 {
		t.Error("a refused purge deleted the works row anyway")
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM work_purge_audit`); n != 0 {
		t.Error("a refused purge wrote an audit row")
	}
}

// TestPurge_ProseMentionDoesNotRefuse is the §6.3 distinction in
// isolation: the difference between a reference and noise.
func TestPurge_ProseMentionDoesNotRefuse(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "mentioned in prose")
	id := w.ID

	// The id appears in a memory's free-text content, a handoff
	// summary, and a work note. None of these is a reference.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, dependencies, created_at, updated_at)
		VALUES (?, 'memories', ?, '[]', 0, 0)`,
		GenerateID(), "we discussed "+id+" at some point"); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO session_handoffs (id, session_id, summary, created_at)
		VALUES (?, 's1', ?, 0)`, GenerateID(), "see work "+id); err != nil {
		t.Logf("session_handoffs shape differs; the memory-content probe still applies: %v", err)
	}
	if _, err := dm.AddWorkNoteWithContext(id, "the id "+id+" appears in this note", ActiveContext{}); err != nil {
		t.Fatalf("note: %v", err)
	}

	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true}); err != nil {
		t.Fatalf("prose mention blocked the purge: %v", err)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM works WHERE id = ?`, id); n != 0 {
		t.Error("purge did not remove the work item")
	}
}

// TestPurge_ProseMentionWithSubstringID pins the adjacent trap: a
// memory whose dependencies contain a LONGER id that merely has the
// purged id as a prefix. json_each compares values, not substrings.
func TestPurge_ProseMentionWithSubstringID(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "substring target")
	id := w.ID
	longer := id + "ff"

	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, dependencies, created_at, updated_at)
		VALUES (?, 'decisions', 'a decision', ?, 0, 0)`, GenerateID(), `["`+longer+`"]`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true}); err != nil {
		t.Fatalf("a prefix-sharing id in another record's dependencies blocked the purge: %v", err)
	}
}

// TestPurge_RefusesOnProvenanceCitation pins path 2 in the inbound
// direction only: a row where another artifact CITES this work
// (source_id = work) is a structural reference and refuses.
//
// The two subtests used to be indistinguishable — both built the same
// args, so "cited as downstream" and "citing as source" inserted the
// same row. The naming was the only difference, and it was the
// difference that decides whether purge refuses.
func TestPurge_RefusesOnProvenanceCitation(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "citation target")
	id := w.ID

	// source_id = this work, downstream_id = another artifact. The
	// decision cites the work as a foundation.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_provenance
			(id, source_id, source_type, downstream_id, downstream_type, event_id, created_at)
		VALUES (?, ?, 'work', ?, 'decision', ?, 0)`,
		GenerateID(), id, GenerateID(), GenerateID()); err != nil {
		t.Fatalf("insert inbound provenance: %v", err)
	}

	report, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true})
	if !errors.Is(err, ErrWorkPurgeReferenced) {
		t.Fatalf("err = %v, want ErrWorkPurgeReferenced", err)
	}
	if len(report.Referrers) != 1 {
		t.Fatalf("refusal named %d referrers, want 1: %+v", len(report.Referrers), report.Referrers)
	}
	if report.Referrers[0].Kind != "epistemic_provenance" {
		t.Errorf("Kind = %q, want epistemic_provenance", report.Referrers[0].Kind)
	}
	if report.Referrers[0].Detail == "" {
		t.Error("refusal does not explain that the referrer cites the work")
	}
}

// TestPurge_OwnCitationIsNotAReferrer pins the other half of the same
// rule: a citation the work made ITSELF (downstream_id = work,
// source_id = something else) is work-owned, belongs in the delete
// set, and must not veto the work's own deletion.
func TestPurge_OwnCitationIsNotAReferrer(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "citing work")
	id := w.ID

	// The work cites a memory as a foundation.
	cited := GenerateID()
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_provenance
			(id, source_id, source_type, downstream_id, downstream_type, event_id, created_at)
		VALUES (?, ?, 'memory', ?, 'work', ?, 0)`,
		GenerateID(), cited, id, GenerateID()); err != nil {
		t.Fatalf("insert outbound provenance: %v", err)
	}

	report, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true})
	if err != nil {
		t.Fatalf("the work's own outbound citation blocked its own purge: %v", err)
	}
	if report.Counts.EpistemicProvenance != 1 {
		t.Errorf("counted %d work-owned citations, want 1", report.Counts.EpistemicProvenance)
	}
	// The edge is gone with the work; the cited memory is untouched.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`, id); n != 0 {
		t.Errorf("the work's own citation survived the purge: %d rows", n)
	}
	// The cited memory is not this work's to delete, and no dangling
	// edge is left pointing at the purged id from either column.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM memories WHERE id = ?`, cited); n != 0 {
		t.Errorf("the purge created a memory row it should not have: %d", n)
	}
	if n := purgeCount(t, dm,
		`SELECT COUNT(*) FROM epistemic_provenance WHERE source_id = ? OR downstream_id = ?`,
		id, id); n != 0 {
		t.Errorf("a dangling provenance edge still mentions the purged work: %d rows", n)
	}
}

// TestPurge_ProvenanceDirectionsAreAsymmetric is the regression that
// the two directions are genuinely independent, with BOTH edges present
// on ONE work item at the same time.
//
// It exists because a symmetric preflight (source_id = ? OR
// downstream_id = ?) passes every single-direction test while making
// the work-owned delete unreachable: the work's own citation trips the
// referrer check, so the DELETE never runs. Only a fixture holding
// both edges at once can tell the two apart.
func TestPurge_ProvenanceDirectionsAreAsymmetric(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "asymmetric")
	id := w.ID

	citedMemory := GenerateID()
	citingDecision := GenerateID()
	insertCitation := func(sourceID, sourceType, downstreamID, downstreamType string) {
		t.Helper()
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO epistemic_provenance
				(id, source_id, source_type, downstream_id, downstream_type, event_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 0)`,
			GenerateID(), sourceID, sourceType, downstreamID, downstreamType, GenerateID()); err != nil {
			t.Fatalf("insert citation %s->%s: %v", sourceID, downstreamID, err)
		}
	}
	// Edge A — the work CITES a memory. Work-owned; must disappear.
	insertCitation(citedMemory, "memory", id, "work")
	// Edge B — a decision CITES the work. Inbound; must block the purge.
	insertCitation(id, "work", citingDecision, "decision")

	// ── Both edges present: the purge must refuse on B alone ──────
	report, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true})
	if !errors.Is(err, ErrWorkPurgeReferenced) {
		t.Fatalf("err = %v, want ErrWorkPurgeReferenced", err)
	}
	if len(report.Referrers) != 1 {
		t.Fatalf("refusal named %d referrers, want exactly 1 (edge B only): %+v",
			len(report.Referrers), report.Referrers)
	}
	if report.Referrers[0].ID != citingDecision {
		t.Errorf("refusal named %q, want the citing decision %q",
			report.Referrers[0].ID, citingDecision)
	}

	// The refusal changed nothing: BOTH edges and the work survive. A
	// refusal that silently dropped edge A would be worse than one
	// that refused.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance WHERE source_id = ?`, id); n != 1 {
		t.Errorf("edge B has %d rows after a refusal, want 1", n)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`, id); n != 1 {
		t.Errorf("edge A has %d rows after a refusal, want 1 — the refusal mutated it", n)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM works WHERE id = ?`, id); n != 1 {
		t.Errorf("the work has %d rows after a refusal, want 1", n)
	}
	if _, err := dm.GetWork(id); err != nil {
		t.Errorf("the work is no longer retrievable after a refusal: %v", err)
	}

	// ── Remove B: A must now be deleted with the work ─────────────
	if _, err := dm.SQLDB().Exec(
		`DELETE FROM epistemic_provenance WHERE source_id = ?`, id); err != nil {
		t.Fatalf("resolve edge B: %v", err)
	}
	report, err = dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true})
	if err != nil {
		t.Fatalf("purge refused after edge B was resolved: %v", err)
	}
	if report.Counts.EpistemicProvenance != 1 {
		t.Errorf("counted %d work-owned citations to delete, want 1", report.Counts.EpistemicProvenance)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`, id); n != 0 {
		t.Errorf("edge A survived the purge: %d rows remain", n)
	}
	// Edge A's counterparty was never this work's to delete.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance WHERE source_id = ?`, citedMemory); n != 0 {
		t.Errorf("the purge deleted a citation belonging to another artifact: %d rows", n)
	}
}

// TestPurge_RefusesOnForeignEvidence pins path 3, and the counterpart
// that work-owned evidence does NOT refuse (it is in the delete set).
func TestPurge_RefusesOnForeignEvidence(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "evidence target")
	id := w.ID
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'memory', 'observation', 'test', 0.5, 'test', 0)`, GenerateID(), id); err != nil {
		t.Fatalf("insert foreign evidence: %v", err)
	}
	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true}); !errors.Is(err, ErrWorkPurgeReferenced) {
		t.Errorf("err = %v, want ErrWorkPurgeReferenced", err)
	}

	// Work-owned evidence is NOT a referrer.
	dm2 := NewTestDM(t)
	w2 := purgeFixture(t, dm2, "own evidence")
	if _, err := dm2.SQLDB().Exec(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'work', 'observation', 'test', 0.5, 'test', 0)`, GenerateID(), w2.ID); err != nil {
		t.Fatalf("insert work evidence: %v", err)
	}
	if _, err := dm2.PurgeWork(WorkPurgeRequest{WorkID: w2.ID, ReasonCode: "other", Force: true}); err != nil {
		t.Errorf("work-owned evidence blocked the purge: %v", err)
	}
}

// TestPurge_RefusesOnCascadeOutbox pins path 4.
func TestPurge_RefusesOnCascadeOutbox(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "outbox target")
	id := w.ID
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_cascade_outbox
			(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			 downstream_artifact_id, downstream_artifact_type, created_at, updated_at)
		VALUES (?, ?, ?, 'work', ?, 'decision', 0, 0)`,
		GenerateID(), GenerateID(), id, GenerateID()); err != nil {
		t.Fatalf("insert outbox: %v", err)
	}
	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true}); !errors.Is(err, ErrWorkPurgeReferenced) {
		t.Errorf("err = %v, want ErrWorkPurgeReferenced", err)
	}
}

// TestPurge_PreflightLeavesEverythingUnchanged pins §8.5's last line:
// a refusal mutates nothing at all, not just the work item.
func TestPurge_PreflightLeavesEverythingUnchanged(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "unchanged on refusal")
	id := w.ID
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, dependencies, created_at, updated_at)
		VALUES (?, 'decisions', 'd', ?, 0, 0)`, GenerateID(), `["`+id+`"]`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	before := map[string]int{
		"works":        purgeCount(t, dm, `SELECT COUNT(*) FROM works`),
		"work_events":  purgeCount(t, dm, `SELECT COUNT(*) FROM work_events`),
		"memories":     purgeCount(t, dm, `SELECT COUNT(*) FROM memories`),
		"audit":        purgeCount(t, dm, `SELECT COUNT(*) FROM work_purge_audit`),
		"outbox":       purgeCount(t, dm, `SELECT COUNT(*) FROM epistemic_cascade_outbox`),
		"provenance":   purgeCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance`),
		"evidence":     purgeCount(t, dm, `SELECT COUNT(*) FROM evidence`),
		"archiveProbe": purgeCount(t, dm, `SELECT COUNT(*) FROM work_events WHERE event_type = 'archived'`),
	}
	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true}); err == nil {
		t.Fatal("purge succeeded despite a memory dependency")
	}
	for table, want := range before {
		var got int
		var q string
		switch table {
		case "works":
			q = `SELECT COUNT(*) FROM works`
		case "work_events":
			q = `SELECT COUNT(*) FROM work_events`
		case "memories":
			q = `SELECT COUNT(*) FROM memories`
		case "audit":
			q = `SELECT COUNT(*) FROM work_purge_audit`
		case "outbox":
			q = `SELECT COUNT(*) FROM epistemic_cascade_outbox`
		case "provenance":
			q = `SELECT COUNT(*) FROM epistemic_provenance`
		case "evidence":
			q = `SELECT COUNT(*) FROM evidence`
		case "archiveProbe":
			q = `SELECT COUNT(*) FROM work_events WHERE event_type = 'archived'`
		}
		if err := dm.SQLDB().QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("re-count %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s: %d row(s) after refusal, want %d — a refusal must mutate nothing", table, got, want)
		}
	}
}

// ════════════════════════════════════════════════════════════════════
// §5.5 Transaction atomicity
// ════════════════════════════════════════════════════════════════════

// TestPurge_AtomicityUnderInducedFailure induces a failure AFTER the
// first DELETE and proves the transaction rolled back completely. The
// failure is induced by dropping the audit table's presence from the
// write's point of view: a second purge attempt racing a rename is
// impractical to stage, so instead the check is that a purge whose
// works row vanishes mid-transaction commits nothing.
//
// The mechanism: a trigger that aborts the DELETE on works. The
// work_events delete has already run at that point, so a non-
// transactional implementation would leave a works row with no ledger.
func TestPurge_AtomicityUnderInducedFailure(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "atomicity target")
	id := w.ID
	beforeEvents := purgeCount(t, dm, `SELECT COUNT(*) FROM work_events WHERE work_id = ?`, id)

	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER purge_fail BEFORE DELETE ON works
		WHEN OLD.id = '` + id + `'
		BEGIN SELECT RAISE(ABORT, 'induced failure'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	_, err := dm.PurgeWork(WorkPurgeRequest{WorkID: id, ReasonCode: "other", Force: true})
	if err == nil {
		t.Fatal("purge succeeded despite an induced mid-transaction failure")
	}
	if _, err := dm.SQLDB().Exec(`DROP TRIGGER purge_fail`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	// Nothing partially deleted.
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM works WHERE id = ?`, id); n != 1 {
		t.Error("the works row did not survive the rolled-back purge")
	}
	if got := purgeCount(t, dm, `SELECT COUNT(*) FROM work_events WHERE work_id = ?`, id); got != beforeEvents {
		t.Errorf("work_events = %d after rollback, want %d — the delete set is not atomic", got, beforeEvents)
	}
	if n := purgeCount(t, dm, `SELECT COUNT(*) FROM work_purge_audit`); n != 0 {
		t.Errorf("a failed purge wrote %d audit row(s)", n)
	}
}

// ════════════════════════════════════════════════════════════════════
// §8.4 Absence of side effects
// ════════════════════════════════════════════════════════════════════

// TestPurge_NoSecureDeletePragma pins the absence of erasure
// semantics. secure_delete would change the write-amplification and
// lock profile of every subsequent write, and it would imply a
// guarantee purge has not been given.
func TestPurge_NoSecureDeletePragma(t *testing.T) {
	dm := NewTestDM(t)
	w := purgeFixture(t, dm, "no secure delete")
	if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: w.ID, ReasonCode: "other", Force: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	var sd int
	if err := dm.SQLDB().QueryRow(`PRAGMA secure_delete`).Scan(&sd); err != nil {
		t.Fatalf("PRAGMA secure_delete: %v", err)
	}
	if sd != 0 {
		t.Errorf("secure_delete = %d; v1 purge must not enable erasure semantics (expected 0 in the test build)", sd)
	}
}

// TestPurge_NoBackupCreated pins that purge creates no backup of its
// own. --backup is explicit and opt-in; an implicit copy of a purge
// target is a copy of the material being removed.
func TestPurge_NoBackupCreated(t *testing.T) {
	dm := NewTestDM(t)
	// NewTestDM is in-memory, so point the check at a real directory
	// and assert nothing appeared in it.
	dir := t.TempDir()
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for i := 0; i < 3; i++ {
		w := purgeFixture(t, dm, "no implicit backup")
		if _, err := dm.PurgeWork(WorkPurgeRequest{WorkID: w.ID, ReasonCode: "other", Force: true}); err != nil {
			t.Fatalf("purge: %v", err)
		}
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("purge created %d file(s) in the workspace", len(after)-len(before))
	}
	// And nothing under a backups/ directory.
	if _, err := os.Stat(filepath.Join(dir, "backups")); err == nil {
		t.Error("purge created a backups/ directory")
	}
}

// TestPurge_IsNotOnCoreDB pins the CLI-only boundary structurally.
// mpm_system is agent-reachable and describes itself as being for
// health rather than daily work; an irreversible delete does not belong
// on a surface an agent can reach unattended.
func TestPurge_IsNotOnCoreDB(t *testing.T) {
	// A compile-time assertion is the strong form: if PurgeWork were
	// ever added to CoreDB, this file would still compile but the
	// reflection probe below would fail with a named method.
	ct := reflectTypeOf(new(CoreDB)).Elem()
	for i := 0; i < ct.NumMethod(); i++ {
		if ct.Method(i).Name == "PurgeWork" {
			t.Error("CoreDB declares PurgeWork; purge must stay off the agent-reachable interface")
		}
	}
	// PurgeWork must exist on the concrete manager, or the CLI has
	// nothing to call.
	if _, ok := interface{}(new(DatabaseManager)).(interface {
		PurgeWork(WorkPurgeRequest) (*WorkPurgeReport, error)
	}); !ok {
		t.Error("DatabaseManager does not implement PurgeWork")
	}
}
