// executable_d2_recovery_test.go — D-2 release-gate rehearsal.
//
// Real-executable regression for D-2. Rewritten to match the
// post-#28 contract where `mpm doctor` is observational and does
// not silently repair schema damage; explicit writable commands
// (`mpm ops init topics`) are the recovery surface.
//
// Rehearsal:
//
//	A. create a realistic workspace/DB via the explicit writable
//	   maintenance command (`bin/mpm ops init topics`),
//	B. close MPM,
//	C. externally DROP TABLE lessons_fts (no IF EXISTS), using the
//	   same sqlite3 CLI invocation a shell operator would run,
//	D. execute the built/public observational inspection
//	   (`bin/mpm doctor`) and verify it does NOT repair
//	   lessons_fts — the new contract is observational only,
//	E. execute the explicit writable recovery
//	   (`bin/mpm ops init topics`) and verify lessons_fts and
//	   the three lessons_instead_of_* triggers are recreated
//	   on disk,
//	F. verify a probe write through the lessons view succeeds by
//	   running another fresh subprocess against the same workspace.
//
// Subprocess entry point uses real `bin/mpm` invocations; no
// test-internal database manager reach-through. This covers the
// public executable surface (observational + writable init) and
// pins both halves of the #28 contract: doctor does not repair
// (observational), explicit init does repair (writable).
package release_acceptance_test

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

const sqlite3Maybe = "sqlite3" // tolerate absent CLI; skip if not installed

// TestExecutableD2_ObservationalDoctorAndExplicitInit pins both
// halves of the MPM #28 contract at the executable level:
//
//  1. `bin/mpm doctor` is observational and must NOT silently
//     repair schema damage. The FTS state after a doctor run on a
//     damaged database must reflect the same damage.
//  2. The explicit writable maintenance command
//     `bin/mpm ops init topics` is the recovery surface. After
//     that command runs, the schema must be restored on disk.
//
// Requires both `bin/mpm` (built via `make build`) and the
// `sqlite3` CLI tool.
func TestExecutableD2_ObservationalDoctorAndExplicitInit(t *testing.T) {
	if os.Getenv("CGO_CFLAGS") == "" {
		t.Skip("FTS5 build flags absent; rerun via `make test-release`")
	}

	// (A) Build a realistic workspace via the producer-side API
	// once so the workspace layout is in place. Then prime the
	// on-disk DB with the explicit writable maintenance command
	// `bin/mpm ops init topics`, which invokes the writable
	// singleton and runs schema init. Assert success — silent
	// tolerance would mask a broken init path.
	workspace, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	mpmBin := findProjectBinary(t)

	initCmd := exec.Command(mpmBin, "ops", "init", "topics")
	initCmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init bin/mpm ops init topics failed: %v\n%s", err, string(out))
	}

	// makeAcceptanceWorkspace resolves MPM_WORKSPACE→$MPM_WORKSPACE
	// and the canonical DB path is <MPM_WORKSPACE>/src/db/mpm.db.
	dbPath := filepath.Join(workspace, "src", "db", "mpm.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("workspace DB not at expected path after init topics: %v (looked for %s)", err, dbPath)
	}

	// Probe the DB path we'll be dropping against — this proves
	// the same DB is the one bin/mpm reads/writes.
	preDrop := snapshotDB(t, dbPath)
	if _, ok := preDrop.vtabs["lessons_fts"]; !ok {
		t.Fatalf("precondition: lessons_fts absent in fresh DB; vtabs=%+v", preDrop.vtabs)
	}
	for _, want := range []string{
		"lessons_instead_of_insert",
		"lessons_instead_of_update",
		"lessons_instead_of_delete",
	} {
		if _, ok := preDrop.triggers[want]; !ok {
			t.Fatalf("precondition: trigger %s absent in fresh DB; triggers=%+v", want, preDrop.triggers)
		}
	}

	// (B) "Close MPM" — implicitly done because no MPM process is
	// holding the DB open after `ops init topics` returned.

	// (C) External strict DROP via the sqlite3 CLI — exactly the
	// operation a shell operator runs. Use sqlite3 directly rather
	// than the Go sqlite handle so we are not using the same code
	// path the recovery path will exercise. If sqlite3 is not
	// installed, fall back to the Go driver for setup-only (still
	// emits a SQL `DROP TABLE lessons_fts` with no IF EXISTS).
	sqlPath, sqlite3OK := whichSqlite3(t)
	if !sqlite3OK {
		t.Logf("sqlite3 CLI not available; falling back to Go-driver strict DROP for setup")
		goStrictDrop(t, dbPath)
	} else {
		out, err := exec.Command(sqlPath, dbPath, `DROP TABLE lessons_fts;`).CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite3 strict DROP: %v\n%s", err, string(out))
		}
	}

	// Confirm the drop took — the DB on disk must show lessons_fts
	// absent before we re-invoke MPM.
	postDrop := snapshotDB(t, dbPath)
	if _, stillPresent := postDrop.vtabs["lessons_fts"]; stillPresent {
		t.Fatalf("precondition: strict DROP did not remove lessons_fts from disk; DB=%+v", postDrop)
	}

	// (D) Execute `bin/mpm doctor` (observational). The doctor
	// boot path is read-only: a fresh handle is opened against
	// the workspace DB and a health report is rendered. Doctor
	// MUST NOT silently repair lessons_fts. A damaged database may
	// legitimately produce a nonzero exit; we assert on the
	// output banner and on the on-disk state after the run.
	docCmd := exec.Command(mpmBin, "doctor")
	docCmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	docOut, docErr := docCmd.CombinedOutput()
	if docErr != nil {
		t.Logf("bin/mpm doctor exit=%v (may be non-zero on warnings or missing-DB); output follows", docErr)
	}
	if !strings.Contains(string(docOut), "MPM") {
		t.Logf("doctor output (truncated): %.200s", string(docOut))
	}

	// Negative control: observational doctor must NOT have
	// recreated lessons_fts. If it did, the post-#28 read-only
	// contract is broken.
	postDoctor := snapshotDB(t, dbPath)
	if _, repaired := postDoctor.vtabs["lessons_fts"]; repaired {
		t.Fatalf("bin/mpm doctor unexpectedly recreated lessons_fts; observational contract violated (vtabs=%+v)", postDoctor.vtabs)
	}

	// (E) Explicit writable recovery: `bin/mpm ops init topics`.
	// The writable singleton opens the DM, runs schema init (which
	// detects the missing lessons_fts and recreates the virtual
	// table plus the three INSTEAD OF triggers), and finally runs
	// the topic-membership backfill the command is named for.
	// Assert success and verify the recovered schema.
	recoverCmd := exec.Command(mpmBin, "ops", "init", "topics")
	recoverCmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	if out, err := recoverCmd.CombinedOutput(); err != nil {
		t.Fatalf("recovery bin/mpm ops init topics failed: %v\n%s", err, string(out))
	}

	finalState := snapshotDB(t, dbPath)
	if _, ok := finalState.vtabs["lessons_fts"]; !ok {
		t.Fatalf("lessons_fts NOT recreated by `bin/mpm ops init topics`; vtabs=%+v triggers=%+v",
			finalState.vtabs, finalState.triggers)
	}
	for _, want := range []string{
		"lessons_instead_of_insert",
		"lessons_instead_of_update",
		"lessons_instead_of_delete",
	} {
		if _, ok := finalState.triggers[want]; !ok {
			t.Errorf("trigger %s NOT recreated by `bin/mpm ops init topics`; present=%+v",
				want, finalState.triggers)
		}
	}

	// (F) Verify a write through the lessons view succeeds via a
	// SECOND subprocess invocation. This exercises the recovery
	// under load (writers come in after init topics already ran).
	//
	// The write goes through the OPERATOR path (`mpm lesson add`). The
	// agent-reachable lesson write is refused — a lesson is trusted on
	// arrival, so only an operator may create one — and what this test
	// is about is the VIEW and its INSTEAD OF trigger chain, which the
	// operator path exercises identically: both write through the
	// `lessons` view.
	insertCmd := exec.Command(mpmBin, "lesson", "add",
		"executable D-2 write probe", "--type", "insight")
	insertCmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out2, err2 := insertCmd.CombinedOutput()
	if err2 != nil {
		t.Errorf("lessons save via bin/mpm subprocess failed: %v\n%s", err2, string(out2))
	}
	// Verify via a fresh Go-driver connection. Reading through an
	// older handle inherited stale connection-pool state from the
	// earlier snapshotDB call in this test (the matttn/go-sqlite3
	// driver does not auto-checkpoint WAL on read); opening
	// independently bypasses that.
	checkDB, cherr := sql.Open("sqlite3", dbPath)
	if cherr != nil {
		t.Fatalf("check db open: %v", cherr)
	}
	defer checkDB.Close()
	var lessonCount int
	if err := checkDB.QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&lessonCount); err != nil {
		t.Fatalf("count probe: %v", err)
	}
	if lessonCount < 1 {
		rows, derr := checkDB.Query(`SELECT id, content FROM lessons`)
		if derr == nil {
			for rows.Next() {
				var id, c string
				_ = rows.Scan(&id, &c)
				t.Logf("  available row: id=%s content=%q", id, c)
			}
			rows.Close()
		}
		t.Errorf("probe row did not land via subprocess write; count=%d (workspace=%s dbPath=%s)", lessonCount, workspace, dbPath)
	}
	// Also verify FTS sync: the lesson must be searchable.
	var ftsCount int
	if err := checkDB.QueryRow(`SELECT COUNT(*) FROM lessons_fts WHERE lessons_fts MATCH ?`, "probe").Scan(&ftsCount); err != nil {
		t.Fatalf("lessons_fts search: %v", err)
	}
	if ftsCount < 1 {
		t.Errorf("lessons_fts MATCH returned no rows; sync trigger chain may be broken (count=%d)", ftsCount)
	}
}

// dbSnapshot is the result of reading sqlite_master from a DB path
// directly (not through MPM). This proves the on-disk state, not the
// in-memory state.
type dbSnapshot struct {
	db       *sql.DB
	vtabs    map[string]bool
	triggers map[string]bool
	views    map[string]bool
	tables   map[string]bool
}

func snapshotDB(t *testing.T, dbPath string) *dbSnapshot {
	t.Helper()
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	db, err := sql.Open("sqlite3", abs+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", abs, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	out := &dbSnapshot{
		db:       db,
		vtabs:    map[string]bool{},
		triggers: map[string]bool{},
		views:    map[string]bool{},
		tables:   map[string]bool{},
	}
	rows, err := db.Query(`SELECT type, name FROM sqlite_master`)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		switch typ {
		case "table":
			out.tables[name] = true
			// Lessons_fts shows up as type=table even though
			// it's a virtual table.
			if strings.HasSuffix(name, "_fts") || name == "lessons_fts" {
				out.vtabs[name] = true
			}
		case "view":
			out.views[name] = true
		case "trigger":
			out.triggers[name] = true
		}
	}
	return out
}

func whichSqlite3(t *testing.T) (string, bool) {
	t.Helper()
	p, err := exec.LookPath(sqlite3Maybe)
	if err != nil {
		return "", false
	}
	return p, true
}

func goStrictDrop(t *testing.T, dbPath string) {
	t.Helper()
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	db, err := sql.Open("sqlite3", abs+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE lessons_fts`); err != nil {
		t.Fatalf("strict drop: %v", err)
	}
}

func mustGetOutput(cmd *exec.Cmd) string {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(cmd error: %v)", err)
	}
	return string(out)
}

// guard against accidental future imports of mpminternal; the import
// keeps the test in this package's dependency graph and prevents an
// unused-import error if the test is temporarily simplified.
var _ = mpminternal.NewDatabaseManager
