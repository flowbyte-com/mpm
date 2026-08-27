package internal

import (
	"strings"
	"testing"
)

// TestGetMemory_NotFoundIsDomainError is the F7 regression test (2026-08-27).
//
// Bug: GetMemory(id) propagated the raw database/sql sentinel
//   "sql: no rows in result set"
// when no row matched, instead of a domain-specific message. A user
// hitting `mpm call mpm_resolve --payload '{"uri":"mpm://memory/<bogus>"}'`
// saw the raw SQL leak in the error response.
//
// Contract after fix: GetMemory and GetLesson return
//   "memory not found: <id>"
//   "lesson not found: <id>"
// respectively — matching the pattern GetWork already follows at
// db.go:4643 ("work not found: <id>"). The raw `sql.ErrNoRows` is not
// surfaced at the API boundary.
func TestGetMemory_NotFoundIsDomainError(t *testing.T) {
	dm := newTestFileDM(t)

	_, err := dm.GetMemory("0000000000000000")
	if err == nil {
		t.Fatalf("GetMemory(bogus): expected error, got nil")
	}
	if strings.Contains(err.Error(), "sql: no rows in result set") {
		t.Errorf("GetMemory leaked raw SQL error: %v", err)
	}
	if !strings.Contains(err.Error(), "memory not found") {
		t.Errorf("GetMemory(bogus): error %q missing 'memory not found' domain message", err.Error())
	}
	if !strings.Contains(err.Error(), "0000000000000000") {
		t.Errorf("GetMemory(bogus): error %q missing the bogus id", err.Error())
	}
}

func TestGetLesson_NotFoundIsDomainError(t *testing.T) {
	dm := newTestFileDM(t)

	_, err := dm.GetLesson("0000000000000000")
	if err == nil {
		t.Fatalf("GetLesson(bogus): expected error, got nil")
	}
	if strings.Contains(err.Error(), "sql: no rows in result set") {
		t.Errorf("GetLesson leaked raw SQL error: %v", err)
	}
	if !strings.Contains(err.Error(), "lesson not found") {
		t.Errorf("GetLesson(bogus): error %q missing 'lesson not found' domain message", err.Error())
	}
	if !strings.Contains(err.Error(), "0000000000000000") {
		t.Errorf("GetLesson(bogus): error %q missing the bogus id", err.Error())
	}
}

// TestGetMemory_FoundStillWorks is the sanity check: the new
// sql.ErrNoRows → domain error path must not regress the happy path.
func TestGetMemory_FoundStillWorks(t *testing.T) {
	dm := newTestFileDM(t)
	id := f7SeedMemory(t, dm, "f7 sanity probe")
	got, err := dm.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory(%q): %v", id, err)
	}
	if got["id"] != id {
		t.Errorf("GetMemory returned wrong id: %v", got["id"])
	}
}

// TestGetLesson_FoundStillWorks is the sanity check: the new
// sql.ErrNoRows → domain error path must not regress the happy path.
func TestGetLesson_FoundStillWorks(t *testing.T) {
	dm := newTestFileDM(t)
	id := f7SeedLesson(t, dm, "f7 sanity probe", LessonTypeInsight)
	got, err := dm.GetLesson(id)
	if err != nil {
		t.Fatalf("GetLesson(%q): %v", id, err)
	}
	if got.ID != id {
		t.Errorf("GetLesson returned wrong id: %v", got.ID)
	}
}

// f7SeedMemory inserts a row directly via SQL so the F7 test does not
// depend on the SaveMemoryNode scanner (which has its own internal
// contract). Avoids collision with broadcast_test.go's seedMemory.
func f7SeedMemory(t *testing.T, dm *DatabaseManager, content string) string {
	t.Helper()
	id := GenerateID()
	_, err := dm.db.Exec(
		`INSERT INTO memories (id, collection, content, created_at, weight) VALUES (?, 'memories', ?, CAST(strftime('%s','now') AS INTEGER), 1.0)`,
		id, content,
	)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	return id
}

// f7SeedLesson inserts a row directly via SQL — the SaveLesson path
// has its own validation; F7 only cares about the lookup error contract.
func f7SeedLesson(t *testing.T, dm *DatabaseManager, content string, lessonType LessonType) string {
	t.Helper()
	id := GenerateID()
	_, err := dm.db.Exec(
		`INSERT INTO lessons (id, type, content, reinforcement_count, created) VALUES (?, ?, ?, 0, CAST(strftime('%s','now') AS INTEGER))`,
		id, string(lessonType), content,
	)
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	return id
}
