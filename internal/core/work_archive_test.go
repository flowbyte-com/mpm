package internal

// work_archive_test.go — Phase A test matrix for the work archive
// lifecycle and the independent status/visibility axes.
//
// Design: docs/designs/2026-09-30-work-archive-and-purge.md §8, §9.
//
// Every test here uses NewTestDM (in-memory, per-test shared-cache
// namespace) so the production MPM database is never touched
// (CLAUDE.md §3, "Test isolation").

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// archivedAtOf reads works.archived_at straight from SQL so the
// projection is verified at the authoritative source, not through the
// Go accessor that a bug could be hiding behind.
func archivedAtOf(t *testing.T, dm *DatabaseManager, workID string) *int64 {
	t.Helper()
	var v sql.NullInt64
	if err := dm.db.QueryRow(`SELECT archived_at FROM works WHERE id = ?`, workID).Scan(&v); err != nil {
		t.Fatalf("read archived_at for %s: %v", workID, err)
	}
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// eventCount counts ledger rows for a work item.
func eventCount(t *testing.T, dm *DatabaseManager, workID, eventType string) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM work_events WHERE work_id = ? AND event_type = ?`,
		workID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

func statusOf(t *testing.T, dm *DatabaseManager, workID string) string {
	t.Helper()
	var s string
	if err := dm.db.QueryRow(`SELECT status FROM works WHERE id = ?`, workID).Scan(&s); err != nil {
		t.Fatalf("read status for %s: %v", workID, err)
	}
	return s
}

// wakeWorkIDs projects a wake-context work slice down to ids.
func wakeWorkIDs(works []WakeContextWork) []string {
	ids := make([]string, len(works))
	for i, w := range works {
		ids[i] = w.ID
	}
	return ids
}

// ─── §1.3 Terminal-only archive ───────────────────────────────────

// TestArchive_RefusesOpenWork is the core guard: archiving an open item
// must write NOTHING. Not just "return an error" — no works row change,
// no ledger event. A partially-applied archive would silently remove a
// live commitment from wake context.
func TestArchive_RefusesOpenWork(t *testing.T) {
	dm := NewTestDM(t)

	w, err := dm.AddWork("live commitment", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	beforeEvents := eventCount(t, dm, w.ID, string(WorkEventTypeArchived))

	_, _, err = dm.ArchiveWorkWithContext(w.ID, "should not apply", ActiveContext{})
	if !errors.Is(err, ErrWorkNotTerminal) {
		t.Fatalf("archive(open) error = %v, want ErrWorkNotTerminal", err)
	}
	// The error must be classifiable as a conflict, not an internal fault.
	if class, _ := ClassifyError(err); class != OutcomeClassConflict {
		t.Errorf("ClassifyError class = %v, want OutcomeClassConflict", class)
	}
	if got := archivedAtOf(t, dm, w.ID); got != nil {
		t.Errorf("archived_at = %v, want NULL (refusal must write nothing)", *got)
	}
	if got := statusOf(t, dm, w.ID); got != "open" {
		t.Errorf("status = %q, want open (unchanged)", got)
	}
	if got := eventCount(t, dm, w.ID, string(WorkEventTypeArchived)); got != beforeEvents {
		t.Errorf("archived event count = %d, want %d (no event on refusal)", got, beforeEvents)
	}
	// And it must still be in the operational view.
	rows, err := dm.ListWorksByStatusAndVisibility("open", "active")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("open/active rows = %d, want 1 (refused archive must not hide work)", len(rows))
	}
}

// TestArchive_AllowsDone covers the supported flow: complete then archive.
func TestArchive_AllowsDone(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("shipped", "", "")
	if _, err := dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	archived, already, err := dm.ArchiveWorkWithContext(w.ID, "shipped in alpha-final", ActiveContext{})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if already {
		t.Error("alreadyArchived = true on a first archive, want false")
	}
	if !archived.IsArchived() {
		t.Error("IsArchived() = false after archive, want true")
	}
	if archived.ArchivedAt == nil {
		t.Fatal("ArchivedAt = nil after archive, want a timestamp")
	}
	if got := eventCount(t, dm, w.ID, string(WorkEventTypeArchived)); got != 1 {
		t.Errorf("archived event count = %d, want 1", got)
	}
	// status is untouched by archive.
	if got := statusOf(t, dm, w.ID); got != "done" {
		t.Errorf("status = %q after archive, want done (archive must not change status)", got)
	}
}

// TestArchive_AllowsCancelled pins that the guard is on "open", not on
// "done".
func TestArchive_AllowsCancelled(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("abandoned", "", "")
	if _, err := dm.CancelWorkWithContext(w.ID, "not doing this", ActiveContext{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive(cancelled): %v", err)
	}
	if got := statusOf(t, dm, w.ID); got != "cancelled" {
		t.Errorf("status = %q, want cancelled", got)
	}
}

// TestArchive_IsIdempotent pins §1.5: a second archive succeeds with
// already_archived=true and appends NO second ledger event. The event
// count assertion is the load-bearing half — a "success" that appended
// a duplicate event would corrupt the projection replay.
func TestArchive_IsIdempotent(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("shipped", "", "")
	if _, err := dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	first, already1, err := dm.ArchiveWorkWithContext(w.ID, "first", ActiveContext{})
	if err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if already1 {
		t.Error("first archive reported alreadyArchived=true")
	}
	second, already2, err := dm.ArchiveWorkWithContext(w.ID, "second", ActiveContext{})
	if err != nil {
		t.Fatalf("second archive should succeed idempotently, got: %v", err)
	}
	if !already2 {
		t.Error("second archive alreadyArchived = false, want true")
	}
	if got := eventCount(t, dm, w.ID, string(WorkEventTypeArchived)); got != 1 {
		t.Errorf("archived event count = %d after two archives, want 1 (idempotent: no second event)", got)
	}
	if first.ArchivedAt == nil || second.ArchivedAt == nil {
		t.Fatal("ArchivedAt unexpectedly nil")
	}
	if *first.ArchivedAt != *second.ArchivedAt {
		t.Errorf("archived_at changed on re-archive: %d → %d", *first.ArchivedAt, *second.ArchivedAt)
	}
}

// TestArchive_UnknownID_NoWrites pins that an unknown id errors and
// creates nothing (the SoftDeleteMemory "substrate never lies" contract).
func TestArchive_UnknownID_NoWrites(t *testing.T) {
	dm := NewTestDM(t)

	if _, _, err := dm.ArchiveWorkWithContext("work-does-not-exist", "", ActiveContext{}); err == nil {
		t.Fatal("archive(unknown id) succeeded, want error")
	}
	if _, err := dm.UnarchiveWorkWithContext("work-does-not-exist", "", ActiveContext{}); err == nil {
		t.Fatal("unarchive(unknown id) succeeded, want error")
	}
	// No work row may be conjured by a failed archive.
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM works WHERE id = ?`, "work-does-not-exist").Scan(&n); err != nil {
		t.Fatalf("count works: %v", err)
	}
	if n != 0 {
		t.Errorf("works rows for unknown id = %d, want 0 (failed archive must not create a work row)", n)
	}
}

// ─── §1.4 Unarchive restores visibility only ──────────────────────

// TestUnarchive_CancelledStaysCancelled is the single most important
// regression pin in the whole design (§1.4: "the single most likely
// contract to regress"). Unarchiving must never reopen work.
func TestUnarchive_CancelledStaysCancelled(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("abandoned", "", "")
	if _, err := dm.CancelWorkWithContext(w.ID, "dropping", ActiveContext{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	restored, err := dm.UnarchiveWorkWithContext(w.ID, "", ActiveContext{})
	if err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if restored.Status != WorkStatusCancelled {
		t.Errorf("status after unarchive = %q, want cancelled (unarchive must NOT reopen work)", restored.Status)
	}
	if restored.IsArchived() {
		t.Error("IsArchived() = true after unarchive, want false")
	}
	// The cancelled item must NOT appear in open_works. Re-entering the
	// operational open view requires an explicit reopen.
	open, err := dm.ListWorksByStatusAndVisibility("open", "active")
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("open/active rows = %d after unarchive of a cancelled item, want 0", len(open))
	}
	cancelled, err := dm.ListWorksByStatusAndVisibility("cancelled", "active")
	if err != nil {
		t.Fatalf("list cancelled: %v", err)
	}
	if len(cancelled) != 1 {
		t.Errorf("cancelled/active rows = %d, want 1 (unarchived item is visible again)", len(cancelled))
	}
}

// TestUnarchive_DoneStaysDone is the same guard on the other terminal
// state.
func TestUnarchive_DoneStaysDone(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("shipped", "", "")
	if _, err := dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	restored, err := dm.UnarchiveWorkWithContext(w.ID, "", ActiveContext{})
	if err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if restored.Status != WorkStatusDone {
		t.Errorf("status after unarchive = %q, want done", restored.Status)
	}
	if restored.CompletedAt == nil {
		t.Error("CompletedAt cleared by unarchive, want preserved")
	}
}

// TestUnarchive_RequiresArchived pins §1.5: unarchiving a non-archived
// item is an error, not a silent success, and writes nothing.
func TestUnarchive_RequiresArchived(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("shipped", "", "")
	if _, err := dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	before := eventCount(t, dm, w.ID, string(WorkEventTypeUnarchived))
	_, err := dm.UnarchiveWorkWithContext(w.ID, "", ActiveContext{})
	if !errors.Is(err, ErrWorkNotArchived) {
		t.Fatalf("unarchive(non-archived) error = %v, want ErrWorkNotArchived", err)
	}
	if class, _ := ClassifyError(err); class != OutcomeClassConflict {
		t.Errorf("ClassifyError class = %v, want OutcomeClassConflict", class)
	}
	if got := eventCount(t, dm, w.ID, string(WorkEventTypeUnarchived)); got != before {
		t.Errorf("unarchived event count = %d, want %d (no event on refusal)", got, before)
	}
}

// ─── §2 The two axes compose independently ───────────────────────

// TestVisibility_StatusAxesCompose pins the full 3x2 cross-product from
// §2.2 on a fixture with one item in each of the three statuses, one of
// which is archived.
func TestVisibility_StatusAxesCompose(t *testing.T) {
	dm := NewTestDM(t)

	open, _ := dm.AddWork("still live", "", "")
	done, _ := dm.AddWork("finished", "", "")
	cancelled, _ := dm.AddWork("dropped", "", "")

	if _, err := dm.CompleteWorkWithContext(done.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := dm.CancelWorkWithContext(cancelled.ID, "dropped", ActiveContext{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Archive the done item only. Cancelled stays active so the fixture
	// exercises archived-only-on-one-status.
	if _, _, err := dm.ArchiveWorkWithContext(done.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	type want struct {
		status     string
		visibility string
		ids        []string
	}
	cases := []want{
		// §2.2 row 1: the default — today's unchanged result.
		{"open", "active", []string{open.ID}},
		{"done", "active", nil},
		{"cancelled", "active", []string{cancelled.ID}},
		{"all", "active", []string{open.ID, cancelled.ID}},
		// §2.2 row 2: the review view.
		{"open", "archived", nil},
		{"done", "archived", []string{done.ID}},
		{"cancelled", "archived", nil},
		{"all", "archived", []string{done.ID}},
		// §2.2 row 3: the complete inventory.
		{"open", "all", []string{open.ID}},
		{"done", "all", []string{done.ID}},
		{"cancelled", "all", []string{cancelled.ID}},
		{"all", "all", []string{done.ID, open.ID, cancelled.ID}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s", tc.status, tc.visibility), func(t *testing.T) {
			got, err := dm.ListWorksByStatusAndVisibility(tc.status, tc.visibility)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			ids := make([]string, len(got))
			for i, w := range got {
				ids[i] = w.ID
			}
			if len(ids) != len(tc.ids) {
				t.Fatalf("got %v, want %v", ids, tc.ids)
			}
			for _, wantID := range tc.ids {
				found := false
				for _, id := range ids {
					if id == wantID {
						found = true
					}
				}
				if !found {
					t.Errorf("missing %s in %v", wantID, ids)
				}
			}
		})
	}
}

// TestVisibility_InvalidRejected pins the W-010 contract: an unknown
// axis value is a clear error, never an indistinguishable zero-row
// result.
func TestVisibility_InvalidRejected(t *testing.T) {
	dm := NewTestDM(t)

	if _, err := dm.ListWorksByStatusAndVisibility("open", "archivd"); err == nil {
		t.Fatal("typo visibility returned no error, want a validation error")
	} else if !strings.Contains(err.Error(), "active|archived|all") {
		t.Errorf("error %q does not list the valid values", err)
	}
	if _, err := dm.ListWorksByStatusAndVisibility("donee", "all"); err == nil {
		t.Fatal("typo status returned no error, want a validation error")
	}
	if _, err := NormalizeWorkVisibility("nope"); err == nil {
		t.Fatal("NormalizeWorkVisibility accepted an unknown value")
	}
	// Empty means the documented default, not an error.
	got, err := NormalizeWorkVisibility("")
	if err != nil || got != WorkVisibilityActive {
		t.Errorf("NormalizeWorkVisibility(\"\") = %v, %v; want active, nil", got, err)
	}
}

// TestVisibility_StatusAll_UnchangedOnEmptyArchiveSet pins §2.3: for a
// database with no archived items, `status=all, visibility=active`
// returns exactly what `status=all` returned before the feature existed.
func TestVisibility_StatusAll_UnchangedOnEmptyArchiveSet(t *testing.T) {
	dm := NewTestDM(t)

	a, _ := dm.AddWork("a", "", "")
	b, _ := dm.AddWork("b", "", "")
	if _, err := dm.CompleteWorkWithContext(b.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// ListAllWorks is the historical `status=all` path; it must now
	// equal `status=all, visibility=active` when nothing is archived.
	viaAllWorks, err := dm.ListAllWorks()
	if err != nil {
		t.Fatalf("ListAllWorks: %v", err)
	}
	viaTwoAxes, err := dm.ListWorksByStatusAndVisibility("all", "active")
	if err != nil {
		t.Fatalf("two-axis list: %v", err)
	}
	if len(viaAllWorks) != len(viaTwoAxes) {
		t.Fatalf("ListAllWorks=%d, two-axis=%d, want equal when nothing is archived", len(viaAllWorks), len(viaTwoAxes))
	}
	for i := range viaAllWorks {
		if viaAllWorks[i].ID != viaTwoAxes[i].ID {
			t.Errorf("row %d: ListAllWorks=%s, two-axis=%s", i, viaAllWorks[i].ID, viaTwoAxes[i].ID)
		}
	}
	seen := map[string]bool{}
	for _, w := range viaAllWorks {
		seen[w.ID] = true
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Errorf("status=all missing unarchived items: have %v, want both %s and %s", seen, a.ID, b.ID)
	}
}

// ─── §3.1 Every operational surface excludes archived work ────────

// TestSurfaces_AllSevenExcludeArchived is the anti-partial-filter pin.
// The design's stated risk is a filter applied to gatherOpenWorks but
// not gatherCompletedWorks, so an archived item leaks into exactly one
// surface. This test drives all seven sites against the SAME archived
// item and asserts every one of them is clean.
//
// Design §3.1.
func TestSurfaces_AllSevenExcludeArchived(t *testing.T) {
	dm := NewTestDM(t)

	// One archived DONE item (so it would surface in gatherCompletedWorks
	// and the session-surface completed refs) plus one archived CANCELLED
	// item, so the fixture exercises more than one status.
	archivedDone, _ := dm.AddWork("archived finished", "", "")
	if _, err := dm.CompleteWorkWithContext(archivedDone.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(archivedDone.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archivedCancelled, _ := dm.AddWork("archived dropped", "", "")
	if _, err := dm.CancelWorkWithContext(archivedCancelled.ID, "dropped", ActiveContext{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(archivedCancelled.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	archivedIDs := map[string]bool{archivedDone.ID: true, archivedCancelled.ID: true}
	assertNoArchived := func(site string, ids []string) {
		t.Helper()
		for _, id := range ids {
			if archivedIDs[id] {
				t.Errorf("surface %q leaked archived work %s", site, id)
			}
		}
	}

	// Site 1: wake open_works.
	assertNoArchived("wake_context.gatherOpenWorks", wakeWorkIDs(dm.gatherOpenWorks()))
	// Site 2: wake completed_works.
	assertNoArchived("wake_context.gatherCompletedWorks", wakeWorkIDs(dm.gatherCompletedWorks()))
	// Site 3: focus structural input.
	assertNoArchived("contextual_focus.gatherActiveWorkIDs", dm.gatherActiveWorkIDs())
	// Site 4: contextual candidates open-scan.
	acc := map[string]*candidateAccumulator{}
	addWorkCandidates(dm, ContextQuery{}, 50, acc)
	for key := range acc {
		if archivedIDs[strings.TrimPrefix(key, "work:")] {
			t.Errorf("surface contextual_candidates.addWorkCandidates leaked archived work (key %q)", key)
		}
	}
	// Sites 5/6: the two legacy list paths.
	listWorks, err := dm.ListWorks()
	if err != nil {
		t.Fatalf("ListWorks: %v", err)
	}
	ids := make([]string, len(listWorks))
	for i, w := range listWorks {
		ids[i] = w.ID
	}
	assertNoArchived("db.ListWorks", ids)
	listAll, err := dm.ListAllWorks()
	if err != nil {
		t.Fatalf("ListAllWorks: %v", err)
	}
	ids = ids[:0]
	for _, w := range listAll {
		ids = append(ids, w.ID)
	}
	assertNoArchived("db.ListAllWorks", ids)
	// Site 7 (the `mpm wake` CLI completed_refs projection) lives in
	// package main and is covered by TestWakeCompletedRefs_ExcludesArchived
	// in cmd/mpm/work_archive_visibility_test.go, which drives the real
	// queryCompletedWorkRefs function rather than a copy of its SQL.
}

// TestSurfaces_ExplicitByIDRetainsArchived pins §3.2: archived work
// stays fully reachable once its id is known. An archive that made the
// artifact unreachable would be a purge wearing a different name.
func TestSurfaces_ExplicitByIDRetainsArchived(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("archived finished", "with content", "")
	if _, err := dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// GetWork.
	got, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork on archived item: %v", err)
	}
	if !got.IsArchived() {
		t.Error("GetWork lost the archived flag")
	}
	// GetWorkEvents. The archive event itself must be readable through
	// the history surface — that is the whole point of archive being
	// non-destructive.
	events, err := dm.GetWorkEvents(w.ID)
	if err != nil {
		t.Fatalf("GetWorkEvents: %v", err)
	}
	if len(events) < 2 {
		t.Errorf("event ledger has %d events for archived work, want the full ledger", len(events))
	}
	var sawArchiveEvent bool
	for _, e := range events {
		if e.EventType == WorkEventTypeArchived {
			sawArchiveEvent = true
		}
	}
	if !sawArchiveEvent {
		t.Error("history of an archived item does not include the `archived` event")
	}
	// WorkRowToMap still carries archived_at.
	row := WorkRowToMap(got)
	if _, ok := row["archived_at"]; !ok {
		t.Error("WorkRowToMap dropped archived_at for an archived item")
	}
	// The explicit WorkIDs contextual path (§3.2: naming a work id is
	// already a deliberate query, so it must not be filtered).
	acc := map[string]*candidateAccumulator{}
	addWorkCandidates(dm, ContextQuery{WorkIDs: []string{w.ID}}, 50, acc)
	if _, ok := acc[candidateKey("work", w.ID)]; !ok {
		t.Errorf("explicit WorkIDs lookup dropped archived work %s", w.ID)
	}
	// resolveExplicitArtifact still classifies the id.
	if kind, ok := resolveExplicitArtifact(dm, w.ID); !ok || kind != "work" {
		t.Errorf("resolveExplicitArtifact(%s) = %q, %v; want \"work\", true", w.ID, kind, ok)
	}
}

// TestWorkRowToMap_ArchivedAtOmittedWhenActive pins the single-source-
// of-truth wire contract (§4.3): absent archived_at means "not
// archived", so there is never a second boolean in disagreement.
func TestWorkRowToMap_ArchivedAtOmittedWhenActive(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("live", "", "")
	row := WorkRowToMap(w)
	if _, present := row["archived_at"]; present {
		t.Error("archived_at present on an active work item, want omitted")
	}
	if _, present := row["archived"]; present {
		t.Error("row carries a redundant `archived` boolean, want archived_at as the only signal")
	}
}

// ─── Migration ────────────────────────────────────────────────────

// TestWorkEventsCheck_MigratedOnExistingDB is the migration invariant
// the design flags as highest risk: an existing database must actually
// RECEIVE the updated CHECK constraint. Constructing a DB with the
// pre-feature DDL and then running migrations is the only way to prove
// the probe works — asserting on a fresh DB proves nothing, because a
// fresh DB gets the new DDL from schema.go and never exercises
// migrateWorkEventsCheck at all.
func TestWorkEventsCheck_MigratedOnExistingDB(t *testing.T) {
	dm := NewTestDM(t)

	// Rebuild work_events with the LEGACY (9-value) CHECK and an
	// existing row, simulating an install created before the feature.
	if _, err := dm.db.Exec(`DROP TABLE work_events`); err != nil {
		t.Fatalf("drop work_events: %v", err)
	}
	legacyDDL := `CREATE TABLE work_events (
		id                    TEXT PRIMARY KEY,
		work_id               TEXT NOT NULL,
		event_index           INTEGER NOT NULL,
		event_type            TEXT NOT NULL
		                      CHECK (event_type IN (
		                        'created','note_appended','completed',
		                        'cancelled','reopened',
		                        'title_updated','content_updated',
		                        'claimed_complete','evidence_observed'
		                      )),
		created_at            INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		invocation_id         TEXT,
		parent_invocation_id TEXT,
		note                  TEXT,
		title                 TEXT,
		content               TEXT,
		directive_ids         TEXT DEFAULT '[]',
		UNIQUE(work_id, event_index)
	);`
	if _, err := dm.db.Exec(legacyDDL); err != nil {
		t.Fatalf("create legacy work_events: %v", err)
	}
	// A pre-existing ledger row must survive the migration.
	if _, err := dm.db.Exec(
		`INSERT INTO work_events (id, work_id, event_index, event_type, created_at, title)
		 VALUES ('evt-legacy', 'work-legacy', 0, 'created', 1700000000, 'legacy work')`,
	); err != nil {
		t.Fatalf("seed legacy event: %v", err)
	}

	// The legacy DB rejects `archived`. Prove the premise before
	// migrating — otherwise a vacuous test would pass later.
	if _, err := dm.db.Exec(
		`INSERT INTO work_events (id, work_id, event_index, event_type) VALUES ('evt-x','work-legacy',1,'archived')`,
	); err == nil {
		t.Fatal("premise broken: legacy CHECK already accepts 'archived'")
	}

	// Run the migration.
	if err := dm.migrateWorkEventsCheck(); err != nil {
		t.Fatalf("migrateWorkEventsCheck: %v", err)
	}

	// The legacy row survived.
	var title string
	if err := dm.db.QueryRow(`SELECT title FROM work_events WHERE id = 'evt-legacy'`).Scan(&title); err != nil {
		t.Fatalf("legacy row lost during migration: %v", err)
	}
	if title != "legacy work" {
		t.Errorf("legacy row title = %q, want %q", title, "legacy work")
	}
	// And `archived` is now accepted — the actual assertion.
	if _, err := dm.db.Exec(
		`INSERT INTO work_events (id, work_id, event_index, event_type) VALUES ('evt-x','work-legacy',1,'archived')`,
	); err != nil {
		t.Fatalf("migrated CHECK still rejects 'archived': %v", err)
	}
	// Idempotency: a second migration pass must not fail or double-copy.
	if err := dm.migrateWorkEventsCheck(); err != nil {
		t.Fatalf("second migrateWorkEventsCheck: %v", err)
	}
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM work_events WHERE id = 'evt-legacy'`).Scan(&n); err != nil {
		t.Fatalf("count after second migration: %v", err)
	}
	if n != 1 {
		t.Errorf("legacy row count = %d after re-migration, want 1", n)
	}
}

// TestWorksArchivedAt_MigratedOnExistingDB pins the SafeMigrations
// column-add on an existing install. SafeMigrations runs
// ALTER TABLE ADD COLUMN, so this is the "old DB gains the column"
// path; without the entry, every archived_at query in the codebase
// errors with "no such column" on an upgraded install.
func TestWorksArchivedAt_MigratedOnExistingDB(t *testing.T) {
	dm := NewTestDM(t)

	if _, err := dm.db.Exec(`ALTER TABLE works DROP COLUMN archived_at`); err != nil {
		t.Skipf("sqlite build cannot DROP COLUMN (%v); SafeMigrations path covered by the canonical-schema test", err)
	}
	var hasArchived string
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('works') WHERE name = 'archived_at'`).Scan(&hasArchived); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if hasArchived != "0" {
		t.Fatal("premise broken: archived_at still present after DROP COLUMN")
	}

	// SafeMigrations is applied by the schema init path; re-running it
	// is the production path an upgraded install takes.
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema re-run: %v", err)
	}
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('works') WHERE name = 'archived_at'`).Scan(&hasArchived); err != nil {
		t.Fatalf("probe after migration: %v", err)
	}
	if hasArchived != "1" {
		t.Error("SafeMigrations did not add works.archived_at to an existing database")
	}
	// Every existing row is correctly non-archived with no backfill.
	var nonNull int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM works WHERE archived_at IS NOT NULL`).Scan(&nonNull); err != nil {
		t.Fatalf("count: %v", err)
	}
	if nonNull != 0 {
		t.Errorf("%d rows backfilled with archived_at, want 0 (no backfill)", nonNull)
	}
}

// ─── Projection replay ────────────────────────────────────────────

// TestRecomputeWorkProjection_ReplaysArchive pins that the archive
// state survives projection rebuild from the ledger. If recompute
// ignored the archived events, every drift-repair would silently
// un-archive a large batch of work.
func TestRecomputeWorkProjection_ReplaysArchive(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("finished", "", "")
	if _, err := dm.CancelWorkWithContext(w.ID, "dropped", ActiveContext{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Corrupt the projection so the recompute has something to fix.
	if _, err := dm.db.Exec(`UPDATE works SET archived_at = NULL, status = 'open' WHERE id = ?`, w.ID); err != nil {
		t.Fatalf("corrupt projection: %v", err)
	}
	if err := dm.RecomputeWorkProjection(w.ID); err != nil {
		t.Fatalf("RecomputeWorkProjection: %v", err)
	}

	rebuilt, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if !rebuilt.IsArchived() {
		t.Error("recompute lost the archive state — archived_at should be replayed from the ledger")
	}
	if rebuilt.Status != WorkStatusCancelled {
		t.Errorf("status after recompute = %q, want cancelled", rebuilt.Status)
	}

	// And the unarchive event replays too.
	if _, err := dm.UnarchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if _, err := dm.db.Exec(`UPDATE works SET archived_at = 12345 WHERE id = ?`, w.ID); err != nil {
		t.Fatalf("corrupt projection: %v", err)
	}
	if err := dm.RecomputeWorkProjection(w.ID); err != nil {
		t.Fatalf("second RecomputeWorkProjection: %v", err)
	}
	if archivedAtOf(t, dm, w.ID) != nil {
		t.Error("recompute did not clear archived_at after the unarchive event")
	}
	if got := statusOf(t, dm, w.ID); got != "cancelled" {
		t.Errorf("status after second recompute = %q, want cancelled (replay must not reopen)", got)
	}
}

// TestRecomputeWorkProjection_NoEventsLeavesRowAlone pins the
// hasEvents guard. Phase-B purge deletes the ledger rows, and this
// guard is what makes a later recompute a no-op instead of a resurrect.
func TestRecomputeWorkProjection_NoEventsLeavesRowAlone(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("no ledger", "", "")
	if _, err := dm.db.Exec(`DELETE FROM work_events WHERE work_id = ?`, w.ID); err != nil {
		t.Fatalf("delete events: %v", err)
	}
	if err := dm.RecomputeWorkProjection(w.ID); err != nil {
		t.Fatalf("RecomputeWorkProjection: %v", err)
	}
	// The works row is untouched — not reset to defaults.
	got, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if got.Title != "no ledger" {
		t.Errorf("title = %q after recompute with no events, want the original %q", got.Title, "no ledger")
	}
}

// ─── Archive does not touch verification ──────────────────────────

// TestArchive_DoesNotDeriveVerification pins §3.5: archive is
// orthogonal to verification. The completed item's verification must be
// unchanged by the archive call.
func TestArchive_DoesNotDeriveVerification(t *testing.T) {
	dm := NewTestDM(t)

	w, _ := dm.AddWork("finished", "", "")
	if _, err := dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	before, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if _, _, err := dm.ArchiveWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	after, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if after.Verification != before.Verification {
		t.Errorf("verification changed on archive: %q → %q", before.Verification, after.Verification)
	}
}
