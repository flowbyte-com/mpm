// f0910_lesson_lifecycle_regression_test.go — 2026-09-10 lesson
// lifecycle fix.
//
// Pin the soft-vs-hard delete semantics:
//   delete  = reversible soft delete (deleted_at tombstone; row + history persist)
//   restore = clears tombstone; lesson reappears in list/search/get
//   shred   = irreversible hard delete (row removed; no restore path)
//
// The previous CLI cleanup (ee9c4fd9) routed `mpm_lessons delete` and
// `mpm_lessons shred` to the same destructive implementation. That
// was wrong — `delete` should be reversible. This test pins the
// corrected contract at the substrate boundary.
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestF0910_LessonSoftDelete_HidesFromListButPersistsRow pins
// requirement #1 of the contract: soft delete hides the lesson
// from normal list/get, but the underlying row remains in
// lessons_base (so a future restore can bring it back).
func TestF0910_LessonSoftDelete_HidesFromListButPersistsRow(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	// Seed a lesson.
	out, _, err := dm.SaveLesson("f0910-soft-delete-content", "insight", []string{"alpha"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("seed: missing id in %v", out)
	}

	// Verify pre-delete: visible via Get, List, Search.
	if _, err := dm.GetLesson(id); err != nil {
		t.Fatalf("pre-delete GetLesson failed: %v", err)
	}
	if !listContains(t, dm, id) {
		t.Fatalf("pre-delete List must include the seeded lesson")
	}
	if !searchFinds(t, dm, "f0910-soft-delete-content") {
		t.Fatalf("pre-delete Search must find the seeded content")
	}

	// Soft delete via the tool action (the surface that was wrong).
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{"id": id},
	}); err != nil {
		t.Fatalf("delete action failed: %v", err)
	}

	// GetLesson must NOT find the soft-deleted row.
	if _, err := dm.GetLesson(id); err == nil {
		t.Errorf("GetLesson must fail for soft-deleted lesson")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("GetLesson error must say 'not found', got: %v", err)
	}

	// List must NOT include the soft-deleted lesson.
	if listContains(t, dm, id) {
		t.Errorf("List must not include soft-deleted lesson")
	}

	// Search must NOT find the soft-deleted content.
	if searchFinds(t, dm, "f0910-soft-delete-content") {
		t.Errorf("Search must not find soft-deleted content")
	}

	// Underlying row must still exist (recoverable).
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("lessons_base probe: %v", err)
	}
	if n != 1 {
		t.Errorf("lessons_base row must persist after soft delete; got %d rows", n)
	}
	// deleted_at must be set.
	var deletedAt *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT deleted_at FROM lessons_base WHERE id = ?`, id,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("deleted_at probe: %v", err)
	}
	if deletedAt == nil || *deletedAt == 0 {
		t.Errorf("deleted_at must be set after soft delete; got %v", deletedAt)
	}
}

// TestF0910_LessonRestore_BringsBackSoftDeleted pins requirement
// #2: restore clears the tombstone and the lesson is visible again.
func TestF0910_LessonRestore_BringsBackSoftDeleted(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	out, _, err := dm.SaveLesson("f0910-restore-content", "warning", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := out["id"].(string)

	// Soft delete.
	if err := dm.DeleteLesson(id); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := dm.GetLesson(id); err == nil {
		t.Fatal("expected not-found after soft delete")
	}

	// Restore via the tool action.
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "restore",
		"params": map[string]interface{}{"id": id},
	}); err != nil {
		t.Fatalf("restore action failed: %v", err)
	}

	// Lesson is visible again.
	if _, err := dm.GetLesson(id); err != nil {
		t.Errorf("GetLesson must succeed after restore, got: %v", err)
	}
	if !listContains(t, dm, id) {
		t.Errorf("List must include the restored lesson")
	}
	if !searchFinds(t, dm, "f0910-restore-content") {
		t.Errorf("Search must find restored lesson content")
	}

	// deleted_at must be NULL after restore.
	var deletedAt *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT deleted_at FROM lessons_base WHERE id = ?`, id,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("deleted_at probe: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("deleted_at must be NULL after restore; got %v", *deletedAt)
	}
}

// TestF0910_LessonRestore_AlreadyVisible_NoOp pins the idempotent
// surface: restoring an already-visible lesson must return a clean
// error, not silently succeed.
func TestF0910_LessonRestore_AlreadyVisible_NoOp(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	out, _, err := dm.SaveLesson("f0910-already-visible", "insight", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := out["id"].(string)

	_, err = handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "restore",
		"params": map[string]interface{}{"id": id},
	})
	if err == nil {
		t.Fatal("restore on visible lesson must return error")
	}
	if !strings.Contains(err.Error(), "not deleted") {
		t.Errorf("error must say 'not deleted', got: %v", err)
	}
}

// TestF0910_LessonShred_Permanent pins requirement #3: shred is
// irreversible hard delete. Row is gone; restore fails cleanly.
func TestF0910_LessonShred_Permanent(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	out, _, err := dm.SaveLesson("f0910-shred-content", "insight", []string{"to-be-shredded"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := out["id"].(string)

	// Shred via the tool action.
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{"id": id},
	}); err != nil {
		t.Fatalf("shred action failed: %v", err)
	}

	// Row is gone from lessons_base.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("lessons_base probe: %v", err)
	}
	if n != 0 {
		t.Errorf("lessons_base row must be gone after shred; got %d rows", n)
	}

	// FTS row is gone too.
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_fts WHERE rowid IN (SELECT rowid FROM lessons_base WHERE id = ?)`, id,
	).Scan(&n); err != nil {
		t.Fatalf("lessons_fts probe: %v", err)
	}
	if n != 0 {
		t.Errorf("lessons_fts row must be gone after shred; got %d", n)
	}

	// List/Search/Get all return not-found.
	if _, err := dm.GetLesson(id); err == nil {
		t.Errorf("GetLesson must fail after shred")
	}
	if listContains(t, dm, id) {
		t.Errorf("List must not include shredded lesson")
	}
	if searchFinds(t, dm, "f0910-shred-content") {
		t.Errorf("Search must not find shredded content")
	}

	// Restore on a shredded lesson fails cleanly.
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "restore",
		"params": map[string]interface{}{"id": id},
	}); err == nil {
		t.Errorf("restore on shredded lesson must fail")
	}
}

// TestF0910_LessonLifecycleIsolation pins requirement #4: lifecycle
// operations on one lesson don't affect another.
func TestF0910_LessonLifecycleIsolation(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	outA, _, err := dm.SaveLesson("isolation-A", "insight", nil)
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	idA, _ := outA["id"].(string)
	outB, _, err := dm.SaveLesson("isolation-B", "insight", nil)
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}
	idB, _ := outB["id"].(string)

	// Delete A; B must remain visible.
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "delete", "params": map[string]interface{}{"id": idA},
	}); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if _, err := dm.GetLesson(idA); err == nil {
		t.Errorf("A must be soft-deleted")
	}
	if _, err := dm.GetLesson(idB); err != nil {
		t.Errorf("B must remain visible; got: %v", err)
	}

	// Restore A; B still visible.
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "restore", "params": map[string]interface{}{"id": idA},
	}); err != nil {
		t.Fatalf("restore A: %v", err)
	}
	if _, err := dm.GetLesson(idA); err != nil {
		t.Errorf("A must be restored")
	}
	if _, err := dm.GetLesson(idB); err != nil {
		t.Errorf("B must still be visible")
	}

	// Shred A; B must remain visible AND content searchable.
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "shred", "params": map[string]interface{}{"id": idA},
	}); err != nil {
		t.Fatalf("shred A: %v", err)
	}
	if _, err := dm.GetLesson(idA); err == nil {
		t.Errorf("A must be gone after shred")
	}
	if _, err := dm.GetLesson(idB); err != nil {
		t.Errorf("B must remain visible after shredding A")
	}
}

// TestF0910_LessonDeleteThenShred_WorksAsExpected pins the
// chained lifecycle: delete (soft) → shred (irreversible) → restore
// fails. The shred on a soft-deleted lesson must still hit the
// base table directly (the view would otherwise filter it out).
func TestF0910_LessonDeleteThenShred_WorksAsExpected(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	out, _, err := dm.SaveLesson("chain-delete-shred", "insight", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := out["id"].(string)

	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "delete", "params": map[string]interface{}{"id": id},
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Shred on a soft-deleted lesson must succeed (the shred path
	// bypasses the view filter).
	if _, err := handleMpmLessons(dm, ac, map[string]interface{}{
		"action": "shred", "params": map[string]interface{}{"id": id},
	}); err != nil {
		t.Fatalf("shred after soft delete failed: %v", err)
	}

	// Row is gone; restore fails.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n != 0 {
		t.Errorf("row must be gone after delete→shred chain")
	}
}

// TestF0910_LessonDispatcher_AdvertisesLifecycleActions pins the
// JSON Schema enum parity lock: the dispatcher's claimed action set
// must include delete, restore, and shred.
func TestF0910_LessonDispatcher_AdvertisesLifecycleActions(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleMpmLessons(dm, ac, map[string]interface{}{"action": "bogus"})
	if err == nil {
		t.Fatal("bogus action must error")
	}
	for _, want := range []string{"delete", "restore", "shred"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("dispatcher unknown-action error must list %q, got: %v", want, err)
		}
	}
}

// listContains checks whether ListLessons returns the lesson id.
func listContains(t *testing.T, dm mpminternal.CoreDB, id string) bool {
	t.Helper()
	lessons, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	for _, l := range lessons {
		if l.ID == id {
			return true
		}
	}
	return false
}

// searchFinds runs a content-only query and reports whether the
// FTS path returned any lesson (the test DM includes the query
// token in the lesson content for this to match).
func searchFinds(t *testing.T, dm mpminternal.CoreDB, query string) bool {
	t.Helper()
	results, err := dm.SearchLessons(query, 20)
	if err != nil {
		t.Fatalf("SearchLessons(%q): %v", query, err)
	}
	return len(results) > 0
}
