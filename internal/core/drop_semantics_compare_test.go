// drop_semantics_compare_test.go — Item 2 verification.
//
// Reconcile the prior report's claim that `DROP TABLE IF EXISTS
// lessons_fts` behaves materially differently under mattn/go-sqlite3
// than `DROP TABLE lessons_fts` does. Both statements target an
// existing virtual table. Per the SQLite documentation
// (sqlite.org/lang_droptable.html) IF EXISTS only affects behavior
// when the named table does not exist. With an existing table both
// forms should produce identical sqlite_master states.
//
// If the resulting states are identical, the report's claim about
// "IF EXISTS semantics" was wrong; the more useful explanation is
// that the prior `TestReleaseAcceptance_FTS5Invariant` scenario
// exercised a different code path than the pristine rehearsal did
// for unrelated reasons (e.g. schema-state preconditions the test
// did not establish).
//
// This file makes the evidence canonical.
package internal

import (
	"database/sql"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// schemaSnapshot returns a canonical (sorted) view of every
// sqlite_master row in db. Two snapshots are equal iff their
// schemas are identical at the SQL level.
func schemaSnapshot(t *testing.T, db *sql.DB) []schemaRow {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("schema scan: %v", err)
	}
	defer rows.Close()
	var out []schemaRow
	for rows.Next() {
		var r schemaRow
		if err := rows.Scan(&r.Type, &r.Name, &r.TblName, &r.SQL); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

type schemaRow struct {
	Type, Name, TblName, SQL string
}

// setupLessonsFTS seeds a minimal canonical state: lessons_base +
// lessons view + lessons_fts vtab + the three INSTEAD OF triggers.
func setupLessonsFTS(t *testing.T) (db *sql.DB, cleanup func()) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_pragma=fts5(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	stmts := []string{
		`CREATE TABLE lessons_base (rowid INTEGER PRIMARY KEY, id TEXT, type TEXT, content TEXT, tags TEXT)`,
		`CREATE VIRTUAL TABLE lessons_fts USING fts5(content, tags, tokenize='porter unicode61')`,
		`CREATE VIEW lessons AS SELECT rowid, id, type, content, tags FROM lessons_base`,
		`CREATE TRIGGER lessons_instead_of_insert INSTEAD OF INSERT ON lessons BEGIN INSERT INTO lessons_base(rowid, id, type, content, tags) VALUES (NEW.rowid, NEW.id, NEW.type, NEW.content, NEW.tags); INSERT INTO lessons_fts(rowid, content, tags) VALUES (NEW.rowid, NEW.content, NEW.tags); END;`,
		`CREATE TRIGGER lessons_instead_of_update INSTEAD OF UPDATE ON lessons BEGIN UPDATE lessons_base SET id=NEW.id WHERE rowid=OLD.rowid; DELETE FROM lessons_fts WHERE rowid=OLD.rowid; INSERT INTO lessons_fts(rowid, content, tags) VALUES (NEW.rowid, NEW.content, NEW.tags); END;`,
		`CREATE TRIGGER lessons_instead_of_delete INSTEAD OF DELETE ON lessons BEGIN DELETE FROM lessons_base WHERE rowid=OLD.rowid; DELETE FROM lessons_fts WHERE rowid=OLD.rowid; END;`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v\nSQL: %s", err, s)
		}
	}
	return db, func() { _ = db.Close() }
}

func TestDropSemantics_Equality(t *testing.T) {
	dbA, closeA := setupLessonsFTS(t)
	defer closeA()
	if _, err := dbA.Exec(`DROP TABLE IF EXISTS lessons_fts`); err != nil {
		t.Fatalf("A drop: %v", err)
	}
	snapA := schemaSnapshot(t, dbA)

	dbB, closeB := setupLessonsFTS(t)
	defer closeB()
	if _, err := dbB.Exec(`DROP TABLE lessons_fts`); err != nil {
		t.Fatalf("B drop: %v", err)
	}
	snapB := schemaSnapshot(t, dbB)

	// Compare row-by-row. If states diverge, the prior
	// explanation is correct; if they are equal, the IF-EXISTS
	// explanation was wrong.
	diff := func(a, b []schemaRow, format string) string {
		out := ""
		for i := 0; i < len(a) || i < len(b); i++ {
			if i >= len(a) {
				out += format + " B-only: " + b[i].Name + "\n"
				continue
			}
			if i >= len(b) {
				out += format + " A-only: " + a[i].Name + "\n"
				continue
			}
			if a[i] != b[i] {
				out += format + " A: " + a[i].Name + " B: " + b[i].Name + "\n"
			}
		}
		return out
	}

	// Equivalent iff same length and same sorted content.
	equivalent := len(snapA) == len(snapB)
	for i := range snapA {
		if i >= len(snapB) || snapA[i] != snapB[i] {
			equivalent = false
			break
		}
	}

	t.Logf("A row count = %d, B row count = %d", len(snapA), len(snapB))
	if !equivalent {
		t.Errorf("DROP TABLE IF EXISTS and DROP TABLE produce DIFFERENT schema states:\n%s",
			diff(snapA, snapB, "diverge"))
	} else {
		t.Logf("Both operations produce identical sqlite_master state after the drop.")
		t.Logf("State:\n%+v\n", snapA)
	}

	// What does the state look like in BOTH cases?
	lessons_ftsSeen := false
	triggersSeen := map[string]bool{}
	for _, row := range snapA {
		if row.Name == "lessons_fts" {
			lessons_ftsSeen = true
		}
		if row.Type == "trigger" {
			triggersSeen[row.Name] = true
		}
	}
	if lessons_ftsSeen {
		t.Errorf("lessons_fts unexpectedly persists after DROP (A): %v", snapA)
	}
	for _, want := range []string{"lessons_instead_of_insert", "lessons_instead_of_update", "lessons_instead_of_delete"} {
		if !triggersSeen[want] {
			// Under IF EXISTS branch, the IF-EXISTS path may have
			// done nothing — the FTS shadow tables would still be
			// present in DB A. Print the full picture either way.
			t.Logf("trigger %s not seen (unexpected on fresh DB)", want)
		}
	}

	// Always print both states verbatim for the report.
	for _, snap := range []struct {
		label string
		rows  []schemaRow
	}{{"A", snapA}, {"B", snapB}} {
		names := make([]string, len(snap.rows))
		for i, r := range snap.rows {
			names[i] = r.Type + ":" + r.Name
		}
		sort.Strings(names)
		t.Logf("%s final state: %v", snap.label, names)
	}
}

// TestDropSemantics_BehaviorOnMissingObject clarifies the only
// behavior IF EXISTS changes: when the target does not exist.
func TestDropSemantics_BehaviorOnMissingObject(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:?_pragma=fts5(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Strict DROP on a missing table should error.
	if _, err := db.Exec(`DROP TABLE lessons_fts`); err == nil {
		t.Errorf("strict DROP on missing lessons_fts should error; got nil")
	}
	// IF EXISTS DROP must NOT error.
	if _, err := db.Exec(`DROP TABLE IF EXISTS lessons_fts`); err != nil {
		t.Errorf("IF EXISTS DROP on missing lessons_fts should be no-op; got %v", err)
	}
}
