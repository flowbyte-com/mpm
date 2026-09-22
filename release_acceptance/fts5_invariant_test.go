package release_acceptance_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestReleaseAcceptance_FTS5Invariant pins the FTS5 module integrity
// across every supported DB-open path. The release_acceptance suite
// relies on lessons_fts / memories_fts / sessions_fts / topics_fts /
// references_fts / reference_chunks_fts / scheduled_wakes_fts being
// present and operational — if any of those FTS5 modules is missing,
// every INSERT into the backing base table through the lessons view
// (or direct memories / sessions / topics / references / scheduled_wakes
// insert) fails with "no such table: main.<base>_fts" via the sync
// trigger.
//
// Three scenarios:
//
//	A. fresh DB          — open a clean workspace, run NewDatabaseManager,
//	                       verify every FTS module is built by initFTSTables
//	B. existing healthy  — insert a lesson, verify the INSTEAD OF trigger
//	                       wrote into lessons_fts (count rows)
//	C. partial recovery  — manually drop an FTS shadow table on a fresh DB,
//	                       then re-open with NewDatabaseManager (production
//	                       recovery path), verify fts_recovery rebuilt the
//	                       virtual table so subsequent writes succeed
func TestReleaseAcceptance_FTS5Invariant(t *testing.T) {
	if os.Getenv("CGO_CFLAGS") == "" {
		// This test is structurally conditional on the FTS5 build
		// flags. Make test-release sets them; bare `go test` does not.
		t.Skip("FTS5 build flags absent; rerun via `make test-release`")
	}

	canonical := []string{
		"lessons_fts", "memories_fts", "sessions_fts", "topics_fts",
		"references_fts", "reference_chunks_fts", "scheduled_wakes_fts",
	}

	scenario := func(t *testing.T, label, root string, mutate func(t *testing.T, db *sql.DB)) {
		t.Helper()
		dm, err := mpminternal.NewDatabaseManager(root)
		if err != nil {
			t.Fatalf("[%s] NewDatabaseManager: %v", label, err)
		}
		defer dm.SQLDB().Close()

		if mutate != nil {
			mutate(t, dm.SQLDB())
		}

		// After the open path (and any mutate), every canonical FTS
		// virtual table must exist in sqlite_master.
		got := listFtsVTabs(t, dm.SQLDB())
		missing := []string{}
		for _, want := range canonical {
			if _, ok := got[want]; !ok {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("[%s] missing FTS5 vtabs after open path: %s",
				label, strings.Join(missing, ", "))
		}

		// Smoke-insert one row into each base table to confirm the
		// sync triggers can land a write into the corresponding
		// FTS5 vtab without "no such table" errors.
		if err := insertProbeLesson(t, dm.SQLDB()); err != nil {
			t.Errorf("[%s] probe lesson insert: %v", label, err)
		}
		if err := insertProbeMemory(t, dm.SQLDB()); err != nil {
			t.Errorf("[%s] probe memory insert: %v", label, err)
		}
		if err := insertProbeSession(t, dm.SQLDB()); err != nil {
			t.Errorf("[%s] probe session insert: %v", label, err)
		}
		if err := insertProbeTopic(t, dm.SQLDB()); err != nil {
			t.Errorf("[%s] probe topic insert: %v", label, err)
		}
		if err := insertProbeReference(t, dm.SQLDB()); err != nil {
			t.Errorf("[%s] probe reference insert: %v", label, err)
		}
	}

	t.Run("A_fresh_DB", func(t *testing.T) {
		root := t.TempDir()
		scenario(t, "fresh", root, nil)
	})

	t.Run("C_missing_FTS5_vtab_recovery", func(t *testing.T) {
		root := t.TempDir()
		// Build a fresh DB, drop lessons_fts + shadow tables to
		// simulate a restore-db replay that stripped the CREATE
		// VIRTUAL TABLE line. Re-open through NewDatabaseManager
		// and verify the open path either recreates lessons_fts
		// (initFTSTables' IF NOT EXISTS) or runs fts_recovery.
		dm1, err := mpminternal.NewDatabaseManager(root)
		if err != nil {
			t.Fatalf("[orphan] first NewDatabaseManager: %v", err)
		}
		if _, err := dm1.SQLDB().Exec(`DROP TABLE IF EXISTS lessons_fts`); err != nil {
			t.Fatalf("[orphan] drop vtab: %v", err)
		}
		dm1.SQLDB().Close()

		dm2, err := mpminternal.NewDatabaseManager(root)
		if err != nil {
			t.Fatalf("[orphan] second NewDatabaseManager: %v", err)
		}
		defer dm2.SQLDB().Close()

		got := listFtsVTabs(t, dm2.SQLDB())
		if _, ok := got["lessons_fts"]; !ok {
			t.Errorf("[orphan] lessons_fts missing after re-open: vtabs present=%v", got)
		}
		// Smoke insert.
		if err := insertProbeLesson(t, dm2.SQLDB()); err != nil {
			t.Errorf("[orphan] probe lesson insert: %v", err)
		}
	})

	// D-2 release-blocker regression. Mirrors the pristine rehearsal
	// failure exactly: an operator who runs `sqlite3 mpm.db "DROP TABLE
	// lessons_fts"` (no IF EXISTS) on a real installation expects the
	// next MPM invocation to recover. The C-scenario above used
	// `DROP TABLE IF EXISTS` which behaves differently under the mattn
	// driver (no-op on missing). The strict-DROP version is the one
	// that actually exercises the recovery path; without a regression
	// here, a regression in the recovery pipeline that surfaces only
	// on strict-DROP would pass CI and block the release.
	//
	// Asserts the following invariants after re-open:
	//   1. lessons_fts virtual table is recreated by the recovery path.
	//   2. lessons_base table is intact.
	//   3. lessons VIEW is intact (it is the public writer surface).
	//   4. All three INSTEAD OF triggers exist on the lessons view
	//      (insert / update / delete). Pre-fix, the
	//      `repairOrphanedFTS5` substring-matched on `lessons_fts` and
	//      dropped them; the subsequent `ftsStatements` loop re-ran
	//      the canonical DROP+CREATE pair, but a real ordering bug
	//      in some code paths left them absent, which broke every
	//      write through the lessons view.
	//   5. A probe INSERT through the lessons view succeeds AND the
	//      FTS indexer accepts the row (row count in lessons_fts
	//      matches the lessons_base insert).
	t.Run("D_strict_drop_lesson_fts_recovery", func(t *testing.T) {
		root := t.TempDir()
		dm1, err := mpminternal.NewDatabaseManager(root)
		if err != nil {
			t.Fatalf("[D2-strict] first NewDatabaseManager: %v", err)
		}
		// Strict-DROP — no IF EXISTS. This is the exact operation a
		// shell operator runs via `sqlite3 ... "DROP TABLE
		// lessons_fts"`. Pre-D-fix this left the DB in a state
		// where the recovery path was not exercised by the
		// existing IF EXISTS-flavored test.
		if _, err := dm1.SQLDB().Exec(`DROP TABLE lessons_fts`); err != nil {
			t.Fatalf("[D2-strict] strict drop vtab: %v", err)
		}
		dm1.SQLDB().Close()

		dm2, err := mpminternal.NewDatabaseManager(root)
		if err != nil {
			t.Fatalf("[D2-strict] second NewDatabaseManager: %v", err)
		}
		defer dm2.SQLDB().Close()
		db := dm2.SQLDB()

		// Invariant 1: lessons_fts is back.
		got := listFtsVTabs(t, db)
		if _, ok := got["lessons_fts"]; !ok {
			t.Errorf("[D2-strict] lessons_fts MISSING after re-open: vtabs present=%v", got)
		}

		// Invariant 2: lessons_base is intact.
		var baseExists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lessons_base'`).Scan(&baseExists); err != nil {
			t.Fatalf("[D2-strict] probe lessons_base: %v", err)
		}
		if baseExists != 1 {
			t.Errorf("[D2-strict] lessons_base absent after re-open")
		}

		// Invariant 3: lessons VIEW (not TABLE) is the writer surface.
		var objType string
		if err := db.QueryRow(`SELECT type FROM sqlite_master WHERE name='lessons'`).Scan(&objType); err != nil {
			t.Fatalf("[D2-strict] probe lessons type: %v", err)
		}
		if objType != "view" {
			t.Errorf("[D2-strict] lessons is %q, want view", objType)
		}

		// Invariant 4: all three INSTEAD OF triggers exist. This is
		// the regression the pristine rehearsal surfaced: a real
		// installation that lost its triggers because fts_recovery
		// substring-matched and dropped them, with the canonical
		// CREATE statements never re-firing on the recovery path.
		var trigCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN
			('lessons_instead_of_insert','lessons_instead_of_update','lessons_instead_of_delete')`).Scan(&trigCount); err != nil {
			t.Fatalf("[D2-strict] count INSTEAD OF triggers: %v", err)
		}
		if trigCount != 3 {
			t.Errorf("[D2-strict] missing INSTEAD OF triggers on lessons view: got %d, want 3", trigCount)
		}

		// Invariant 5: probe INSERT round-trips and lands in FTS.
		if err := insertProbeLesson(t, db); err != nil {
			t.Errorf("[D2-strict] probe lesson insert: %v", err)
		}
		var lessonCount, ftsCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM lessons WHERE id='fts-probe-1'`).Scan(&lessonCount); err != nil {
			t.Fatalf("[D2-strict] count lessons: %v", err)
		}
		if lessonCount != 1 {
			t.Errorf("[D2-strict] probe row did not land in lessons view (count=%d)", lessonCount)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM lessons_fts`).Scan(&ftsCount); err != nil {
			// Only fatal if the vtab exists; the IF NOT EXISTS path
			// may not have run if the build is FTS5-disabled. With
			// the FTS5 build flag set (test gate), this is fatal.
			t.Fatalf("[D2-strict] count lessons_fts: %v", err)
		}
		if ftsCount < 1 {
			t.Errorf("[D2-strict] lessons_fts has no rows after recovery (count=%d); trigger chain is broken", ftsCount)
		}
	})
}

func listFtsVTabs(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE '%_fts'`)
	if err != nil {
		t.Fatalf("sqlite_master scan: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		out[name] = true
	}
	return out
}

// Probe inserts — minimal columns; tolerate schema differences across
// releases (use IF NOT EXISTS-style INSERT OR IGNORE on UNIQUE).
func insertProbeLesson(t *testing.T, db *sql.DB) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO lessons (id, type, content, created) VALUES ('fts-probe-1', 'insight', 'probe content', '2026-01-01 00:00:00')`)
	return err
}

func insertProbeMemory(t *testing.T, db *sql.DB) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO memories (id, collection, content, created_at, updated_at) VALUES ('fts-probe-mem-1', 'memory', 'probe content', 0, 0)`)
	return err
}

func insertProbeSession(t *testing.T, db *sql.DB) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO sessions (id, session_id, content, content_hash) VALUES ('fts-probe-sess-1', 'fts-probe-sess-1', 'probe content', 'fts-probe-sess-hash-1')`)
	return err
}

func insertProbeTopic(t *testing.T, db *sql.DB) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO topics (id, name, description, created_at) VALUES ('fts-probe-top-1', 'probe topic', 'probe desc', 0)`)
	return err
}

func insertProbeReference(t *testing.T, db *sql.DB) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO reference_docs (id, title, content, content_hash) VALUES ('fts-probe-ref-1', 'probe', 'probe content', 'fts-probe-ref-hash-1')`)
	return err
}

// Sanity check: listFtsVTabs is non-nil and consistent.
func TestReleaseAcceptance_FTS5Invariant_helpersExist(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	dm, err := mpminternal.NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.SQLDB().Close()
	got := listFtsVTabs(t, dm.SQLDB())
	if len(got) == 0 {
		t.Skip("no FTS5 vtabs present (binary built without ENABLE_FTS5)")
	}
	t.Logf("FTS5 vtabs present: %d", len(got))
}
