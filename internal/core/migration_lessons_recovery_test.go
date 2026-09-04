package internal

// Stage 7 adversarial-verification regression tests (D2 class).
//
// migrateLessonsToView historically ran as independent auto-commit DDL
// statements. A crash between statements left permanently wedged states:
//
//	State C: rename done, view creation never ran. The next boot's
//	         BaseTables re-created an empty `lessons` TABLE, orphaning the
//	         operator's data inside lessons_base forever.
//	State B': view created, INSTEAD OF triggers never ran. Lesson writes
//	          failed with "cannot modify lessons because it is a view".
//
// Invariants under test:
//  1. Boot heals both interrupted states without data loss.
//  2. A fresh legacy database (lessons as a plain table) migrates cleanly.
//  3. GetMemoriesForExport tolerates NULL tags/metadata/created_at rows —
//     the exact shape a pre-affinity legacy upgrade produces.
import (
	"testing"
)

// newTestDM boots a full DatabaseManager against an isolated workspace.
func newMigrationTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	dm, err := NewDatabaseManager(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

func buildLegacyLessonsBase(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(`DROP VIEW IF EXISTS lessons`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`DROP TABLE IF EXISTS lessons`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`DROP TRIGGER IF EXISTS lessons_instead_of_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`DROP TRIGGER IF EXISTS lessons_instead_of_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`DROP TRIGGER IF EXISTS lessons_instead_of_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`DROP TABLE IF EXISTS lessons_base`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`CREATE TABLE lessons_base (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL DEFAULT 'insight',
		content TEXT NOT NULL,
		tags JSON,
		reinforcement_count INTEGER DEFAULT 1,
		source_session_id TEXT,
		created TEXT NOT NULL,
		content_hash TEXT,
		retrieval_priority REAL NOT NULL DEFAULT 0.5,
		importance REAL NOT NULL DEFAULT 0.5,
		confidence REAL NOT NULL DEFAULT 0.7
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`INSERT INTO lessons_base (id, type, content, created) VALUES
		('les-orphan-1', 'warning', 'interrupted migration survivor', '2025-06-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateLessons_HealsInterruptedState_ShadowTable(t *testing.T) {
	dm := newMigrationTestDM(t)
	buildLegacyLessonsBase(t, dm)

	// State C artifact: BaseTables re-created an empty shadow `lessons`
	// table after the rename; simulate one row written post-crash.
	if _, err := dm.SQLDB().Exec(`CREATE TABLE lessons (
		id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT 'insight', content TEXT NOT NULL,
		tags JSON, reinforcement_count INTEGER DEFAULT 1, source_session_id TEXT,
		created TEXT NOT NULL, content_hash TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.SQLDB().Exec(`INSERT INTO lessons VALUES
		('shadow-row', 'practice', 'written into shadow', NULL, 2, NULL, '2025-06-02T00:00:00Z', NULL)`); err != nil {
		t.Fatal(err)
	}

	// Re-run the migration path exactly as the next boot would.
	dm.migrateLessonsToView()

	objType := objectTypeOf(t, dm, "lessons")
	if objType != "view" {
		t.Fatalf("lessons should be a view after repair, got %q", objType)
	}
	n := countRows(t, dm, "SELECT COUNT(*) FROM lessons_base")
	if n != 2 {
		t.Fatalf("expected 2 salvaged rows in lessons_base, got %d", n)
	}
	ids := map[string]bool{}
	for _, id := range columnStrings(t, dm, "SELECT id FROM lessons_base") {
		ids[id] = true
	}
	if !ids["les-orphan-1"] || !ids["shadow-row"] {
		t.Fatalf("salvage lost data: %v", ids)
	}

	// The repaired surface must accept writes through the view.
	if _, err := dm.AddLesson("post-repair lesson", "insight", []string{}, ""); err != nil {
		t.Fatalf("AddLesson through repaired view: %v", err)
	}
}

func TestMigrateLessons_HealsInterruptedState_MissingTriggers(t *testing.T) {
	dm := newMigrationTestDM(t)
	buildLegacyLessonsBase(t, dm)

	// State B': view exists, no INSTEAD OF triggers.
	if _, err := dm.SQLDB().Exec(`CREATE VIEW lessons AS SELECT rowid, id, type, content, tags,
		reinforcement_count, source_session_id, created, content_hash,
		retrieval_priority, importance, confidence FROM lessons_base`); err != nil {
		t.Fatal(err)
	}

	dm.migrateLessonsToView()

	if objectTypeOf(t, dm, "lessons") != "view" {
		t.Fatalf("lessons should remain a view")
	}
	triggers := countRows(t, dm, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger'
		AND name IN ('lessons_instead_of_insert','lessons_instead_of_update','lessons_instead_of_delete')`)
	if triggers != 3 {
		t.Fatalf("expected 3 INSTEAD OF triggers after repair, got %d", triggers)
	}
	if _, err := dm.AddLesson("post-repair lesson b", "insight", []string{}, ""); err != nil {
		t.Fatalf("AddLesson through repaired view: %v", err)
	}
}

func TestGetMemoriesForExport_NullLegacyColumns(t *testing.T) {
	ws := t.TempDir()
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()

	// Legacy-shaped row: NULL tags/metadata (the genuinely nullable
	// legacy columns). The original fixture also used NULL
	// created_at, but alpha-final's NOT NULL invariant on
	// memories.created_at (commit 1aa8457) made that impossible.
	// The test's semantic intent — verify the export path tolerates
	// NULL legacy columns and COALESCES NULL tags to '[]' — is
	// preserved: we set a real created_at and leave the genuinely
	// nullable legacy columns NULL.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at)
		VALUES ('legacy-null-row', 'memories', 'null columns row', NULL, NULL, 1700000000)`); err != nil {
		t.Fatal(err)
	}

	memories, err := dm.GetMemoriesForExport("", "", "")
	if err != nil {
		t.Fatalf("export must tolerate NULL legacy columns: %v", err)
	}
	found := false
	for _, m := range memories {
		if m["id"] == "legacy-null-row" {
			found = true
			if m["tags"] != "[]" {
				t.Errorf("NULL tags should COALESCE to '[]', got %v", m["tags"])
			}
		}
	}
	if !found {
		t.Fatal("legacy NULL-column row missing from export results")
	}
}

// ── helpers ────────────────────────────────────────────────────────────

func objectTypeOf(t *testing.T, dm *DatabaseManager, name string) string {
	t.Helper()
	var objType string
	if err := dm.SQLDB().QueryRow(
		`SELECT type FROM sqlite_master WHERE name = ?`, name).Scan(&objType); err != nil {
		t.Fatalf("probe %s: %v", name, err)
	}
	return objType
}

func countRows(t *testing.T, dm *DatabaseManager, query string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func columnStrings(t *testing.T, dm *DatabaseManager, query string) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}


// TestLessonReadPaths_TolerateNullLegacyColumns guards the F3-class scan bug
// on the lessons surface: legacy rows with SQL-NULL tags/source_session_id
// must not abort list/search/get.
func TestLessonReadPaths_TolerateNullLegacyColumns(t *testing.T) {
	dm := newMigrationTestDM(t)
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO lessons_base (id, type, content, tags, source_session_id, created)
		VALUES ('nullcols-1', 'insight', 'legacy null columns lesson', NULL, NULL, '2025-06-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.AddLesson("fresh lesson for fts", "insight", nil, ""); err != nil {
		t.Fatal(err)
	}

	lessons, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("ListLessons must tolerate NULL legacy columns: %v", err)
	}
	found := false
	for _, l := range lessons {
		if l.ID == "nullcols-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("NULL-columns lesson missing from ListLessons")
	}

	hits, err := dm.SearchLessons("survivor", 10)
	if err != nil {
		t.Fatalf("SearchLessons: %v", err)
	}
	_ = hits

	if _, err := dm.GetLesson("nullcols-1"); err != nil {
		t.Fatalf("GetLesson on NULL-columns row: %v", err)
	}
}
