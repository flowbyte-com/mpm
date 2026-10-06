package internal

// Shared helpers for the artifact_provenance migration regression tests.
//
// The canonical view SQL is READ FROM BaseTables rather than copied into the
// test file. That is deliberate: §15 requires a guard against the migration's
// replacement DDL and schema.go's canonical DDL drifting apart, and a test
// that hardcodes a third copy of the same view text would add a fourth place
// to keep in sync instead of removing one. Reading the canonical source means
// these tests assert against what production actually ships.

import (
	"database/sql"
	"strings"
	"testing"
)

// canonicalProvenanceViewNames are the two analytics views owned by the
// provenance migration. Both are canonical schema objects; neither may be
// special-cased away.
var canonicalProvenanceViewNames = []string{
	"v_model_memory_yield",
	"v_model_theory_utility",
}

// canonicalProvenanceViews extracts the two CREATE VIEW statements from
// BaseTables, in schema.go's order.
func canonicalProvenanceViews() []string {
	var out []string
	for _, stmt := range BaseTables {
		for _, name := range canonicalProvenanceViewNames {
			if strings.Contains(stmt, "CREATE VIEW IF NOT EXISTS "+name) {
				out = append(out, stmt)
			}
		}
	}
	if len(out) != len(canonicalProvenanceViewNames) {
		panic("BaseTables no longer defines exactly the two canonical provenance views; " +
			"the migration and its tests must be updated together")
	}
	return out
}

func provMustExec(t *testing.T, dm *DatabaseManager, stmt string) {
	t.Helper()
	if _, err := dm.db.Exec(stmt); err != nil {
		t.Fatalf("exec %q: %v", provTruncate(stmt, 90), err)
	}
}

func provTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func assertViewExists(t *testing.T, dm *DatabaseManager, view, when string) {
	t.Helper()
	var name string
	err := dm.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='view' AND name=?`, view).Scan(&name)
	if err == sql.ErrNoRows {
		t.Errorf("%s: analytics view %q is MISSING", when, view)
		return
	}
	if err != nil {
		t.Errorf("%s: probing view %q: %v", when, view, err)
	}
}

func assertViewNotReferencingOld(t *testing.T, dm *DatabaseManager, view string) {
	t.Helper()
	var viewSQL string
	if err := dm.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='view' AND name=?`, view).Scan(&viewSQL); err != nil {
		t.Errorf("reading view %q: %v", view, err)
		return
	}
	if strings.Contains(viewSQL, "artifact_provenance_old") {
		t.Errorf("view %q still references artifact_provenance_old", view)
	}
	if !strings.Contains(viewSQL, "artifact_provenance") {
		t.Errorf("view %q does not reference the canonical artifact_provenance table", view)
	}
}

// assertViewQueryable proves the view is not merely present in
// sqlite_master but actually compiles and executes.
func assertViewQueryable(t *testing.T, dm *DatabaseManager, view string) {
	t.Helper()
	if _, err := dm.db.Exec(`SELECT * FROM ` + view + ` LIMIT 0`); err != nil {
		t.Errorf("view %q exists but is not queryable: %v", view, err)
	}
}

// pointViewAtOld reproduces the post-migration corruption where a view was
// left bound to the renamed-aside table.
func pointViewAtOld(t *testing.T, dm *DatabaseManager, view string) {
	t.Helper()
	var sqlText string
	if err := dm.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='view' AND name=?`, view).Scan(&sqlText); err != nil {
		t.Fatalf("reading view %q: %v", view, err)
	}
	provMustExec(t, dm, `DROP VIEW `+view)
	provMustExec(t, dm, strings.Replace(sqlText, "artifact_provenance ", "artifact_provenance_old ", 1))
}
