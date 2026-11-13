package internal

// skill_package_schema_test.go — Skill Packages, stage P1: additive,
// local-only storage.
//
// Design of record:
//   docs/dev/skill-package-format-design-2026-11-04.md
//   §9 (the DDL), §6 (the local-only slice and the restore allow-list),
//   §6.1/§6.2 (the two rollbacks), §10 P1 (this stage).
//   Approved: PR #222, comment 6658245185.
//
// P1 creates THREE additive tables and nothing else. There is no reader,
// writer, importer or exporter for them yet — those are later stages. The
// tables are created empty and stay empty.
//
// The load-bearing invariant of this stage is NEGATIVE: the package tables
// must never appear in a shared database. attachShared rewrites every
// `CREATE TABLE` in BaseTables to `shared.<name>`, so a package table
// declared in BaseTables would be created as `shared.skill_packages` in
// EVERY shared database — the exact thing the design forbids. The slice
// `SkillPackageTables` is a sibling of ReferenceTables, not a member of
// BaseTables, and TestSkillPackageTablesAreLocalOnly pins that.
//
// The second load-bearing invariant is the restore allow-list: a dump
// naming the three tables must be ACCEPTED by a new binary, and REFUSED by
// an old one. That refusal is deliberate and loud — it is what makes a
// binary rollback fail closed instead of silently restoring a partial
// database. The allow-list is not weakened to hide it.
//
// Every test here uses a disposable workspace under t.TempDir(). The live
// ~/.mpm database is never opened.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// skillPkgTableNames are the three tables this stage creates.
var skillPkgTableNames = []string{
	"skill_package_blobs",
	"skill_package_files",
	"skill_packages",
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// skillPkgWorkspace points MPM_WORKSPACE at a disposable directory and
// returns it. The live ~/.mpm database is never touched.
func skillPkgWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_SHARED_DB", "")
	return ws
}

// skillPkgDM opens the workspace's database through the PRODUCTION path
// (NewDatabaseManager), so the DSN's `_foreign_keys=1` applies and foreign
// keys are enforced structurally. NewTestDM would skip that DSN and let FK
// enforcement drop — the exact regression internal/core/foreign_keys_test.go
// guards against.
func skillPkgDM(t *testing.T) *DatabaseManager {
	t.Helper()
	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// skillPkgTableExists reports whether a table is present in the given schema
// ("main" for the local schema, "shared" for the attached shared database).
func skillPkgTableExists(t *testing.T, db *sql.DB, schema, name string) bool {
	t.Helper()
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s.sqlite_master WHERE type='table' AND name=?`, schema)
	var n int
	if err := db.QueryRow(q, name).Scan(&n); err != nil {
		t.Fatalf("query %s.sqlite_master for %q: %v", schema, name, err)
	}
	return n > 0
}

// skillPkgStoredDDL returns the CREATE statement SQLite stored for a table.
func skillPkgStoredDDL(t *testing.T, db *sql.DB, schema, name string) string {
	t.Helper()
	q := fmt.Sprintf(`SELECT sql FROM %s.sqlite_master WHERE type='table' AND name=?`, schema)
	var sqlText string
	if err := db.QueryRow(q, name).Scan(&sqlText); err != nil {
		t.Fatalf("query stored DDL for %q: %v", name, err)
	}
	return sqlText
}

// skillPkgNormaliseDDL collapses whitespace runs so a formatting-only change
// in the source DDL does not fail the comparison, while any difference in
// the statement's substance does.
var skillPkgWS = regexp.MustCompile(`\s+`)

func skillPkgNormaliseDDL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ";")
	s = strings.Replace(s, "CREATE TABLE IF NOT EXISTS", "CREATE TABLE", 1)
	s = strings.Replace(s, "CREATE INDEX IF NOT EXISTS", "CREATE INDEX", 1)
	return strings.TrimSpace(skillPkgWS.ReplaceAllString(s, " "))
}

// skillPkgColumns returns the table's columns as "name|type|notnull|default|pk"
// strings, in declaration order.
func skillPkgColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		d := "nil"
		if dflt != nil {
			d = fmt.Sprintf("%v", dflt)
		}
		out = append(out, fmt.Sprintf("%s|%s|%d|%s|%d", name, typ, notnull, d, pk))
	}
	return out
}

// skillPkgForeignKeys returns the table's foreign keys as
// "from->table.to|on_delete" strings, grouped by the pragma's FK id.
//
// NOTE: PRAGMA foreign_key_list's columns are
// id, seq, table, from, to, on_update, on_delete, match — the composite key
// is the FIRST column (id), not seq. Grouping by seq reads a composite
// foreign key as two single-column ones.
func skillPkgForeignKeys(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", table))
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_list(%s): %v", table, err)
	}
	defer rows.Close()
	type fk struct {
		id                 int
		seq                int
		table, from, to    string
		onUpdate, onDelete string
	}
	var fks []fk
	for rows.Next() {
		var f fk
		var match string
		if err := rows.Scan(&f.id, &f.seq, &f.table, &f.from, &f.to, &f.onUpdate, &f.onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		fks = append(fks, f)
	}
	byID := map[int][]fk{}
	var order []int
	for _, f := range fks {
		if _, seen := byID[f.id]; !seen {
			order = append(order, f.id)
		}
		byID[f.id] = append(byID[f.id], f)
	}
	sort.Ints(order)
	var out []string
	for _, id := range order {
		group := byID[id]
		sort.Slice(group, func(i, j int) bool { return group[i].seq < group[j].seq })
		var froms, tos []string
		del := group[0].onDelete
		for _, g := range group {
			froms = append(froms, g.from)
			tos = append(tos, g.table+"."+g.to)
		}
		out = append(out, fmt.Sprintf("%s->%s|%s",
			strings.Join(froms, ","), strings.Join(tos, ","), del))
	}
	sort.Strings(out)
	return out
}

// skillPkgIndexColumns returns the columns of a named index.
func skillPkgIndexColumns(t *testing.T, db *sql.DB, index string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA index_info(%s)", index))
	if err != nil {
		t.Fatalf("PRAGMA index_info(%s): %v", index, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var seqno, cid int
		var col any
		if err := rows.Scan(&seqno, &cid, &col); err != nil {
			t.Fatalf("scan index_info(%s): %v", index, err)
		}
		out = append(out, fmt.Sprintf("%v", col))
	}
	return out
}

// skillPkgSeedPackage writes one blob, one package row and one file row —
// enough to exercise every foreign key in the schema.
func skillPkgSeedPackage(t *testing.T, dm *DatabaseManager, skillID, sha string) {
	t.Helper()
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'skills', 'a packaged skill')`, 0, skillID); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if _, err := dm.ExecTracked(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES (?, ?, ?)`,
		0, sha, 5, []byte("bytes")); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	if _, err := dm.ExecTracked(
		`INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash) VALUES (?, 'demo', '1.0.0', 'declared',
			'SKILL.md', ?, '{}', ?)`,
		0, skillID, sha, sha); err != nil {
		t.Fatalf("insert package: %v", err)
	}
	if _, err := dm.ExecTracked(
		`INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type,
			role, secret_scanned, poison_scanned) VALUES (?, 'SKILL.md', ?, 5,
			'text/markdown; charset=utf-8', 'entrypoint', 1, 1)`,
		0, skillID, sha); err != nil {
		t.Fatalf("insert file: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 1 — fresh database creation
// ─────────────────────────────────────────────────────────────────────────────

func TestSkillPackageTablesCreatedOnFreshDatabase(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)

	for _, name := range skillPkgTableNames {
		if !skillPkgTableExists(t, dm.SQLDB(), "main", name) {
			t.Errorf("%s was not created on a fresh database", name)
		}
	}

	// The index the design requires.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_skill_package_files_sha256'`).Scan(&n); err != nil {
		t.Fatalf("query index: %v", err)
	}
	if n != 1 {
		t.Errorf("idx_skill_package_files_sha256 was not created (count=%d)", n)
	}

	// The tables are storage-only: they are created EMPTY.
	for _, name := range skillPkgTableNames {
		var rows int
		if err := dm.SQLDB().QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", name)).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if rows != 0 {
			t.Errorf("%s has %d rows on a fresh database; P1 creates storage only", name, rows)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2 — exact schema and index verification
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPackageTablesExactSchema pins the created schema against the
// approved §9 DDL. Two independent checks:
//
//	(a) the CREATE statement SQLite stored is the statement in the
//	    SkillPackageTables slice — so the deployed schema IS the approved
//	    DDL, not an approximation of it;
//	(b) the pragmas report the exact columns, constraints, foreign keys and
//	    index, so a DDL that merely *looks* right cannot pass.
func TestSkillPackageTablesExactSchema(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)

	// (a) the stored DDL is the slice's DDL.
	for _, stmt := range SkillPackageTables {
		head := skillPkgWS.ReplaceAllString(strings.TrimSpace(stmt), " ")
		var name string
		switch {
		case strings.Contains(head, "CREATE TABLE IF NOT EXISTS skill_package_blobs"):
			name = "skill_package_blobs"
		case strings.Contains(head, "CREATE TABLE IF NOT EXISTS skill_packages"):
			name = "skill_packages"
		case strings.Contains(head, "CREATE TABLE IF NOT EXISTS skill_package_files"):
			name = "skill_package_files"
		case strings.Contains(head, "CREATE INDEX IF NOT EXISTS idx_skill_package_files_sha256"):
			name = "idx_skill_package_files_sha256"
		default:
			t.Fatalf("SkillPackageTables holds an unrecognised statement: %s", head)
		}

		kind := "table"
		if strings.HasPrefix(head, "CREATE INDEX") {
			kind = "index"
		}
		var stored string
		if err := dm.SQLDB().QueryRow(
			`SELECT sql FROM sqlite_master WHERE type=? AND name=?`, kind, name).Scan(&stored); err != nil {
			t.Fatalf("query stored DDL for %s: %v", name, err)
		}
		if got, want := skillPkgNormaliseDDL(stored), skillPkgNormaliseDDL(stmt); got != want {
			t.Errorf("%s: the stored DDL is not the approved DDL\n  stored: %s\n  slice : %s",
				name, got, want)
		}
	}

	// (b) the exact column shapes.
	wantBlobs := []string{
		"sha256|TEXT|0|nil|1",
		"size_bytes|INTEGER|1|nil|0",
		"content|BLOB|1|nil|0",
		"created_at|INTEGER|1|CAST(strftime('%s','now') AS INTEGER)|0",
	}
	wantPackages := []string{
		"skill_id|TEXT|0|nil|1",
		"name|TEXT|1|nil|0",
		"version|TEXT|1|nil|0",
		"declared_version|TEXT|0|nil|0",
		"version_source|TEXT|1|nil|0",
		"format|TEXT|1|'mpm-skill-package/1'|0",
		"entrypoint|TEXT|1|nil|0",
		"entrypoint_sha256|TEXT|1|nil|0",
		"manifest|TEXT|1|nil|0",
		"package_hash|TEXT|1|nil|0",
		"source_sha256|TEXT|0|nil|0",
		"is_global|INTEGER|1|0|0",
		"created_at|INTEGER|1|CAST(strftime('%s','now') AS INTEGER)|0",
		"updated_at|INTEGER|0|nil|0",
	}
	wantFiles := []string{
		"skill_id|TEXT|1|nil|1",
		"path|TEXT|1|nil|2",
		"sha256|TEXT|1|nil|0",
		"size_bytes|INTEGER|1|nil|0",
		"content_type|TEXT|1|nil|0",
		"role|TEXT|1|nil|0",
		"executable|INTEGER|1|0|0",
		"secret_scanned|INTEGER|1|0|0",
		"poison_scanned|INTEGER|1|0|0",
		"raw_path|TEXT|0|nil|0",
	}
	for _, tc := range []struct {
		table string
		want  []string
	}{
		{"skill_package_blobs", wantBlobs},
		{"skill_packages", wantPackages},
		{"skill_package_files", wantFiles},
	} {
		got := skillPkgColumns(t, dm.SQLDB(), tc.table)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s columns:\n  got : %v\n  want: %v", tc.table, got, tc.want)
		}
	}

	// (b) the exact foreign keys, with their ON DELETE behaviour.
	wantFK := map[string][]string{
		"skill_package_blobs": {},
		"skill_packages": {
			"skill_id->memories.id|CASCADE",
		},
		"skill_package_files": {
			"sha256->skill_package_blobs.sha256|NO ACTION",
			"skill_id->skill_packages.skill_id|CASCADE",
		},
	}
	for table, want := range wantFK {
		got := skillPkgForeignKeys(t, dm.SQLDB(), table)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s foreign keys:\n  got : %v\n  want: %v", table, got, want)
		}
	}

	// (b) the index and its column.
	cols := skillPkgIndexColumns(t, dm.SQLDB(), "idx_skill_package_files_sha256")
	if len(cols) != 1 || cols[0] != "sha256" {
		t.Errorf("idx_skill_package_files_sha256 covers %v, want [sha256]", cols)
	}

	// The index is on the CHILD side, which is what the collector needs:
	// a foreign key indexes the parent side, so the parent-side index
	// SQLite creates for the blob FK does not answer "which files
	// reference this blob".
	var parentSide int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='sqlite_autoindex_skill_package_files_1'`).Scan(&parentSide); err != nil {
		t.Fatalf("query autoindex: %v", err)
	}
	if parentSide != 1 {
		t.Errorf("the composite primary key's autoindex is missing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3 — foreign-key enforcement
// ─────────────────────────────────────────────────────────────────────────────

func TestSkillPackageForeignKeyEnforcement(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)

	// The connection must actually have FKs on — the DSN's job, not a
	// per-connection PRAGMA that only covers whichever connection it hit.
	var fk int
	if err := dm.SQLDB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 — the DSN's _foreign_keys=1 is not structural", fk)
	}

	// A package row for a skill that does not exist.
	_, err := dm.ExecTracked(
		`INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash) VALUES ('no-such-skill', 'x', '1.0.0',
			'declared', 'SKILL.md', 'sha', '{}', 'sha')`, 0)
	if err == nil {
		t.Error("a package row for a nonexistent skill was accepted — FK enforcement is not on")
	} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("expected a FOREIGN KEY failure, got: %v", err)
	}

	// A file row for a package that does not exist.
	_, err = dm.ExecTracked(
		`INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type, role)
		 VALUES ('no-such-package', 'SKILL.md', 'sha', 1, 'text/markdown', 'entrypoint')`, 0)
	if err == nil {
		t.Error("a file row for a nonexistent package was accepted")
	} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("expected a FOREIGN KEY failure, got: %v", err)
	}

	// A file row naming a blob that does not exist.
	_, err = dm.ExecTracked(
		`INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type, role)
		 VALUES ('some-skill', 'SKILL.md', 'no-such-blob', 1, 'text/markdown', 'entrypoint')`, 0)
	if err == nil {
		t.Error("a file row naming a nonexistent blob was accepted")
	} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("expected a FOREIGN KEY failure, got: %v", err)
	}

	// The valid shape is accepted.
	skillPkgSeedPackage(t, dm, "skill:pkg-ok", "abc123")

	// ON DELETE CASCADE: shredding the skill takes the package rows with it.
	if _, err := dm.ExecTracked(`DELETE FROM memories WHERE id = ?`, 0, "skill:pkg-ok"); err != nil {
		t.Fatalf("delete memory: %v", err)
	}
	var pkgs, files int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_packages`).Scan(&pkgs); err != nil {
		t.Fatal(err)
	}
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_package_files`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if pkgs != 0 || files != 0 {
		t.Errorf("the package rows did not cascade away (packages=%d files=%d)", pkgs, files)
	}

	// ON DELETE NO ACTION: a blob that is still referenced cannot be deleted.
	skillPkgSeedPackage(t, dm, "skill:pkg-2", "def456")
	if _, err := dm.ExecTracked(`DELETE FROM skill_package_blobs WHERE sha256 = ?`, 0, "def456"); err == nil {
		t.Error("a referenced blob was deleted — ON DELETE NO ACTION is not enforced")
	} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("expected a FOREIGN KEY failure, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4 — existing database migration, with all existing rows preserved
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPackageTablesMigrateExistingDatabase is the upgrade path: a
// database created before this stage has no package tables, and opening it
// with the new binary must create them IN PLACE without touching a single
// existing row.
func TestSkillPackageTablesMigrateExistingDatabase(t *testing.T) {
	skillPkgWorkspace(t)

	// 1 — build the database, then remove the package tables so it is a
	//     pre-P1 database in every respect that matters.
	dm := skillPkgDM(t)
	dbPath := dm.dbPath

	skillPkgSeedPackage(t, dm, "skill:pkg-old", "aaa111")
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES ('mem-keep', 'memories', 'keep me')`, 0); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	skillID, err := dm.SaveSkill("legacy-skill", "1.0.0",
		"---\nname: legacy-skill\nversion: 1.0.0\n---\nbody", "test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	// Drop the package tables (children first) to simulate the pre-P1 state.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS skill_package_files`,
		`DROP TABLE IF EXISTS skill_packages`,
		`DROP TABLE IF EXISTS skill_package_blobs`,
	} {
		if _, err := dm.ExecTracked(stmt, 0); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, name := range skillPkgTableNames {
		if skillPkgTableExists(t, dm.SQLDB(), "main", name) {
			t.Fatalf("%s still exists after the DROP — the pre-P1 fixture is not pre-P1", name)
		}
	}
	dm.Close()

	// 2 — reopen with the new binary.
	dm2, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer dm2.Close()
	if dm2.dbPath != dbPath {
		t.Fatalf("the reopen targeted a different database: %q vs %q", dm2.dbPath, dbPath)
	}

	// 3 — the three tables now exist.
	for _, name := range skillPkgTableNames {
		if !skillPkgTableExists(t, dm2.SQLDB(), "main", name) {
			t.Errorf("%s was not created when an existing database was opened", name)
		}
	}

	// 4 — every pre-existing row survived, unchanged.
	var content string
	if err := dm2.SQLDB().QueryRow(`SELECT content FROM memories WHERE id='mem-keep'`).Scan(&content); err != nil {
		t.Fatalf("the pre-existing memory did not survive the migration: %v", err)
	}
	if content != "keep me" {
		t.Errorf("the pre-existing memory's content changed: %q", content)
	}

	var skillContent string
	if err := dm2.SQLDB().QueryRow(`SELECT content FROM memories WHERE id=?`, skillID).Scan(&skillContent); err != nil {
		t.Fatalf("the pre-existing skill did not survive the migration: %v", err)
	}
	if !strings.Contains(skillContent, "legacy-skill") {
		t.Errorf("the pre-existing skill's content changed: %q", skillContent)
	}

	// 5 — the migration must not fabricate package rows.
	var pkgs, files, blobs int
	if err := dm2.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_packages`).Scan(&pkgs); err != nil {
		t.Fatal(err)
	}
	if err := dm2.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_package_files`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := dm2.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_package_blobs`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if pkgs != 0 || files != 0 || blobs != 0 {
		t.Errorf("the migration created rows (packages=%d files=%d blobs=%d); P1 creates storage only",
			pkgs, files, blobs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5 — idempotent reopening
// ─────────────────────────────────────────────────────────────────────────────

func TestSkillPackageTablesIdempotentReopen(t *testing.T) {
	skillPkgWorkspace(t)

	dm := skillPkgDM(t)
	dbPath := dm.dbPath
	skillPkgSeedPackage(t, dm, "skill:pkg-idem", "bbb222")

	// Snapshot the schema so each reopen can be compared against it.
	snapshot := func(dm *DatabaseManager) map[string]string {
		out := map[string]string{}
		for _, name := range skillPkgTableNames {
			out[name] = skillPkgStoredDDL(t, dm.SQLDB(), "main", name)
		}
		var idx string
		if err := dm.SQLDB().QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_skill_package_files_sha256'`).Scan(&idx); err != nil {
			t.Fatalf("query index: %v", err)
		}
		out["idx_skill_package_files_sha256"] = idx
		return out
	}
	want := snapshot(dm)
	dm.Close()

	// Reopen three times. Each must succeed, leave the schema identical and
	// lose no row.
	for i := 0; i < 3; i++ {
		dm, err := NewDatabaseManager("")
		if err != nil {
			t.Fatalf("reopen %d: %v", i+1, err)
		}
		if dm.dbPath != dbPath {
			t.Fatalf("reopen %d targeted a different database: %q vs %q", i+1, dm.dbPath, dbPath)
		}
		got := snapshot(dm)
		for name, wantSQL := range want {
			if got[name] != wantSQL {
				t.Errorf("reopen %d changed the schema of %s:\n  before: %s\n  after : %s",
					i+1, name, wantSQL, got[name])
			}
		}
		var rows int
		if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_package_files`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Errorf("reopen %d changed the file-row count to %d, want 1", i+1, rows)
		}
		dm.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6 — T50: the package tables are LOCAL-ONLY
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPackageTablesAreLocalOnly is T50.
//
// The shared database is a separate attached file that runs "the same table
// schema as the local DB" — but only the tables attachShared propagates,
// which is BaseTables. The package tables are in their own slice and must
// never reach the shared schema.
func TestSkillPackageTablesAreLocalOnly(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	sharedPath := filepath.Join(ws, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	if dm.SharedAttached() == "" {
		t.Fatalf("the shared database was not attached — the test would be vacuous")
	}

	// NEGATIVE CONTROL 1: the probe can see a table that IS shared, and a
	// table that is NOT. If the query could not tell them apart, the
	// assertions below would prove nothing.
	if !skillPkgTableExists(t, dm.SQLDB(), "shared", "memories") {
		t.Error("the shared schema does not carry `memories` — the local-only assertion would be vacuous")
	}
	if skillPkgTableExists(t, dm.SQLDB(), "shared", "reference_docs") {
		t.Error("the shared schema carries `reference_docs`; it is a local-only table and the probe is not distinguishing")
	}

	// The assertion: none of the three package tables is in the shared schema.
	for _, name := range skillPkgTableNames {
		if skillPkgTableExists(t, dm.SQLDB(), "shared", name) {
			t.Errorf("%s IS in the shared schema — the package tables must be local-only", name)
		}
	}

	// And they ARE in the local schema.
	for _, name := range skillPkgTableNames {
		if !skillPkgTableExists(t, dm.SQLDB(), "main", name) {
			t.Errorf("%s is missing from the local schema", name)
		}
	}

	// NEGATIVE CONTROL 2 (static): the slice must not be a member of
	// BaseTables. This is the condition that would cause the propagation,
	// so it is asserted at the source rather than only at the runtime.
	for _, ddl := range BaseTables {
		for _, name := range skillPkgTableNames {
			if strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS "+name+" ") ||
				strings.Contains(ddl, "CREATE TABLE "+name+" ") {
				t.Errorf("BaseTables declares %s — attachShared would create it as shared.%s in every shared database",
					name, name)
			}
		}
	}

	// The slice itself declares all three, exactly once each.
	seen := map[string]int{}
	for _, ddl := range SkillPackageTables {
		for _, name := range skillPkgTableNames {
			if strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS "+name+" (") {
				seen[name]++
			}
		}
	}
	for _, name := range skillPkgTableNames {
		if seen[name] != 1 {
			t.Errorf("SkillPackageTables declares %s %d times, want exactly 1", name, seen[name])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7 — the restore allow-list
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPackageTablesInRestoreAllowList pins the allow-list registration
// in both halves:
//
//	(a) POSITIVE — a dump naming the three package tables is ACCEPTED by the
//	    canonical validator. Without the registration, `mpm restore-db`
//	    would reject every backup taken after this stage.
//	(b) NEGATIVE CONTROL — a validator whose allow-list lacks the three
//	    names REJECTS the same dump. That is what an OLD binary does, and it
//	    is the mechanism that makes a binary rollback fail closed rather
//	    than silently restoring a partial database.
func TestSkillPackageTablesInRestoreAllowList(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)
	skillPkgSeedPackage(t, dm, "skill:pkg-dump", "ccc333")

	// A dump-shaped script: the CREATE statements plus the INSERTs a
	// `sqlite3 .dump` of a package-bearing database would contain.
	var dump strings.Builder
	for _, stmt := range SkillPackageTables {
		dump.WriteString(stmt)
		dump.WriteString(";\n")
	}
	dump.WriteString("INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES ('ccc333', 5, X'6279746573');\n")
	dump.WriteString("INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint, entrypoint_sha256, manifest, package_hash) VALUES ('skill:pkg-dump', 'demo', '1.0.0', 'declared', 'SKILL.md', 'ccc333', '{}', 'ccc333');\n")

	// (a) the canonical validator accepts it.
	validator := NewCanonicalDumpValidator()
	statements, err := validator.Prepare(dump.String())
	if err != nil {
		t.Fatalf("the canonical validator rejected a dump naming the package tables: %v", err)
	}
	if len(statements) == 0 {
		t.Error("the validator approved zero statements")
	}

	// The allow-list carries all three names.
	for _, name := range skillPkgTableNames {
		found := false
		for _, n := range CanonicalMPMSchema {
			if n == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not on CanonicalMPMSchema — restore-db would reject every new-binary backup", name)
		}
	}

	// And they are NOT on the runtime half of the allow-list: they are
	// declared in Go source, so the source-checked half is the right one.
	for _, name := range skillPkgTableNames {
		for _, n := range RuntimeCanonicalSchema {
			if n == name {
				t.Errorf("%s is on RuntimeCanonicalSchema; it is declared in source and belongs on CanonicalMPMSchema", name)
			}
		}
	}

	// (b) NEGATIVE CONTROL: a validator WITHOUT the three names — the old
	// binary's allow-list — must reject the same dump.
	var without []string
	for _, n := range CanonicalMPMSchema {
		skip := false
		for _, pkg := range skillPkgTableNames {
			if n == pkg {
				skip = true
			}
		}
		if !skip {
			without = append(without, n)
		}
	}
	old := NewDumpValidator(without)
	if err := old.Validate(dump.String()); err == nil {
		t.Error("a validator without the package tables ACCEPTED a dump naming them — the allow-list entry is not load-bearing")
	} else if !strings.Contains(strings.ToLower(err.Error()), "skill_package") {
		t.Errorf("expected the rejection to name the package table, got: %v", err)
	}

	// NEGATIVE CONTROL 2: the validator is not vacuous — a dump naming a
	// table nobody declares is still rejected.
	if err := NewCanonicalDumpValidator().Validate(
		"CREATE TABLE definitely_not_an_mpm_table (id TEXT);\n"); err == nil {
		t.Error("the canonical validator accepted a dump naming an unknown table")
	}
}

// TestSkillPackageDumpRestoreRoundTrip checks that a package-bearing
// database survives a dump and a restore: the schema and the rows come
// back, and the foreign keys still hold.
//
// The dump is produced and applied in Go (no sqlite3 CLI dependency), using
// the same statement set the canonical validator approves.
func TestSkillPackageDumpRestoreRoundTrip(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)
	skillPkgSeedPackage(t, dm, "skill:pkg-rt", "ddd444")

	// Dump: the stored DDL plus the rows, as INSERT statements.
	//
	// The dump carries the FOREIGN-KEY CLOSURE of the package tables, in
	// creation order — which is what `sqlite3 .dump` emits, and what makes
	// the restore order-correct:
	//
	//	sessions  ->  memories  ->  skill_package_blobs
	//	                            ->  skill_packages  ->  skill_package_files
	//
	// `memories` is skill_packages' FK parent and `sessions` is memories'
	// own FK parent. A dump that carried only the three package tables
	// would fail on the package INSERT rather than on the package schema,
	// which would test nothing about P1.
	closure := []string{"sessions", "memories"}
	closure = append(closure, skillPkgTableNames...)

	var dump strings.Builder
	for _, name := range closure {
		dump.WriteString(skillPkgStoredDDL(t, dm.SQLDB(), "main", name))
		dump.WriteString(";\n")
	}
	var idxSQL string
	if err := dm.SQLDB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_skill_package_files_sha256'`).Scan(&idxSQL); err != nil {
		t.Fatalf("query index: %v", err)
	}
	dump.WriteString(idxSQL)
	dump.WriteString(";\n")

	dump.WriteString("INSERT INTO memories (id, collection, content) VALUES ('skill:pkg-rt', 'skills', 'a packaged skill');\n")
	dump.WriteString("INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES ('ddd444', 5, X'6279746573');\n")
	dump.WriteString("INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint, entrypoint_sha256, manifest, package_hash) VALUES ('skill:pkg-rt', 'demo', '1.0.0', 'declared', 'SKILL.md', 'ddd444', '{}', 'ddd444');\n")
	dump.WriteString("INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type, role, secret_scanned, poison_scanned) VALUES ('skill:pkg-rt', 'SKILL.md', 'ddd444', 5, 'text/markdown; charset=utf-8', 'entrypoint', 1, 1);\n")

	// The dump must pass the allow-list gate.
	statements, err := NewCanonicalDumpValidator().Prepare(dump.String())
	if err != nil {
		t.Fatalf("the dump failed validation: %v", err)
	}

	// Restore into a disposable database.
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	restored, err := sql.Open("sqlite3", restoredPath+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer restored.Close()
	for i, stmt := range statements {
		if _, err := restored.Exec(stmt); err != nil {
			t.Fatalf("restore statement %d failed: %v\n%s", i+1, err, stmt)
		}
	}

	// The tables and the rows came back.
	for _, name := range skillPkgTableNames {
		if !skillPkgTableExists(t, restored, "main", name) {
			t.Errorf("%s did not survive the restore", name)
		}
	}
	var rows int
	if err := restored.QueryRow(`SELECT COUNT(*) FROM skill_package_files`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("the restored database has %d file rows, want 1", rows)
	}
	var content []byte
	if err := restored.QueryRow(`SELECT content FROM skill_package_blobs WHERE sha256='ddd444'`).Scan(&content); err != nil {
		t.Fatalf("the blob did not survive the restore: %v", err)
	}
	if string(content) != "bytes" {
		t.Errorf("the restored blob's content is %q, want %q", content, "bytes")
	}

	// The foreign keys still hold in the restored database: a file row
	// naming a nonexistent blob is refused.
	if _, err := restored.Exec(
		`INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type, role)
		 VALUES ('skill:pkg-rt', 'other.md', 'no-such-blob', 1, 'text/markdown', 'reference')`); err == nil {
		t.Error("the restored database does not enforce the blob foreign key")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8 — no existing behaviour changed
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPackagesDoNotChangeExistingBehaviour pins the negative half of
// P1's scope: the existing skill readers, writers and pruner are untouched.
//
// The pruning assertion is load-bearing for scope, not just for safety:
// the prune exemption's FORM is an open operator decision (O17). P1 must
// not decide it, and the way to prove it did not decide it is to show that
// an old skill is still removed by age exactly as it was before this
// stage.
func TestSkillPackagesDoNotChangeExistingBehaviour(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)

	// The legacy single-document skill path still works, byte for byte.
	content := "---\nname: agentshell\nversion: 2.0.0\n---\nbody"
	id, err := dm.SaveSkill("agentshell", "2.0.0", content, "test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}
	skills, err := dm.ListSkills("local")
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	found := false
	for _, s := range skills {
		if s.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("the saved skill is not listed — the legacy skill path changed")
	}

	// A legacy skill creates NO package row.
	var pkgs int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_packages`).Scan(&pkgs); err != nil {
		t.Fatal(err)
	}
	if pkgs != 0 {
		t.Errorf("a legacy skill created %d package rows; P1 is storage only", pkgs)
	}

	// The pruner no longer removes an old skill.
	//
	// HISTORY: this assertion used to be the OPPOSITE. At P1, O17 (the
	// prune exemption's FORM) was an open operator decision, and this test
	// pinned that P1 had NOT decided it: the blanket
	// `collection = 'skills'` exclusion "must NOT be here yet". The
	// operator has since approved the BLANKET form explicitly, and P3
	// implements it — so the assertion is inverted here rather than
	// deleted, because the guarantee it makes is now stronger, not
	// weaker: an age-based prune can no longer remove ANY skill.
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, created_at) VALUES ('skill:old', 'skills', 'old', 1)`, 0); err != nil {
		t.Fatalf("insert old skill: %v", err)
	}
	n, err := dm.PruneOlderThan(1000)
	if err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}
	var left int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id='skill:old'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("the age-based prune removed the old skill (removed=%d) — O17's blanket exclusion is in force", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9 — the package foreign key survives the memories-rebuild migrations
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPackageForeignKeySurvivesMemoriesRebuild guards the interaction
// between P1's new foreign key and the two boot migrations that recreate
// the `memories` table with the table-recreate pattern (CREATE new -> copy ->
// DROP old -> RENAME new).
//
// The concern is real and specific: `skill_packages.skill_id` REFERENCES
// memories(id) ON DELETE CASCADE, and `DROP TABLE memories` inside a
// transaction with foreign keys enabled either fails with an FK violation
// or cascades deletes across referencing tables. Both migrations therefore
// manage their own PRAGMA foreign_keys envelope and drop the dependent
// views before the table DROP.
//
// P1 adds a NEW referencing table, so it is a new participant in that
// dance. This test runs both migrations FOR REAL (their sentinels are
// removed so they are not no-ops) against a database that holds package
// rows, and asserts that:
//   - both migrations succeed — a failure here would stop MPM booting;
//   - the package rows survive;
//   - PRAGMA foreign_key_check reports no violation;
//   - the cascade still fires afterwards, so the FK was not silently
//     degraded into a no-op.
func TestSkillPackageForeignKeySurvivesMemoriesRebuild(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)

	skillPkgSeedPackage(t, dm, "skill:pkg-rebuild", "fff666")

	countPackages := func() int {
		t.Helper()
		var n int
		if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM skill_packages`).Scan(&n); err != nil {
			t.Fatalf("count packages: %v", err)
		}
		return n
	}
	before := countPackages()

	// Force the affinity rebuild to run: its sentinel is what makes it a
	// no-op on an already-migrated database.
	for _, sentinel := range []string{
		memoriesColumnAffinitySentinel,
		sharedMemoriesColumnAffinitySentinel,
	} {
		if _, err := dm.ExecTracked(`DELETE FROM schema_migrations WHERE id = ?`, 0, sentinel); err != nil {
			t.Logf("delete sentinel %s: %v", sentinel, err)
		}
	}
	if err := RebuildMemoriesColumnAffinity(dm.db); err != nil {
		t.Fatalf("the memories affinity rebuild failed with package rows present: %v", err)
	}
	if got := countPackages(); got != before {
		t.Errorf("the affinity rebuild changed the package row count: %d -> %d", before, got)
	}

	// And the second memories-recreating migration.
	skillPkgSeedPackage(t, dm, "skill:pkg-rebuild-2", "777888")
	before = countPackages()
	for _, sentinel := range []string{
		memoriesCreatedAtNotNullSentinel,
		sharedMemoriesCreatedAtNotNullSentinel,
	} {
		if _, err := dm.ExecTracked(`DELETE FROM schema_migrations WHERE id = ?`, 0, sentinel); err != nil {
			t.Logf("delete sentinel %s: %v", sentinel, err)
		}
	}
	if err := EnforceMemoriesCreatedAtNotNull(dm.db); err != nil {
		t.Fatalf("EnforceMemoriesCreatedAtNotNull failed with package rows present: %v", err)
	}
	if got := countPackages(); got != before {
		t.Errorf("the created_at migration changed the package row count: %d -> %d", before, got)
	}

	// No dangling references.
	rows, err := dm.SQLDB().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	violations := 0
	for rows.Next() {
		violations++
	}
	if violations != 0 {
		t.Errorf("PRAGMA foreign_key_check reported %d violations after the rebuilds", violations)
	}

	// The cascade still fires — the FK was not degraded into a no-op.
	if _, err := dm.ExecTracked(`DELETE FROM memories WHERE id = ?`, 0, "skill:pkg-rebuild"); err != nil {
		t.Fatalf("delete the memory: %v", err)
	}
	var left int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM skill_packages WHERE skill_id = ?`, "skill:pkg-rebuild").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("the cascade did not fire after the rebuilds (%d package rows left)", left)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10 — the rollback runbook's factual claims
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPackageBinaryRollbackFailsClosed is the §6.2 property, verified
// rather than asserted in prose:
//
//	a backup taken by the NEW binary contains the three package tables, and
//	an OLD binary's restore-db refuses it — because the old binary's
//	allow-list does not name them.
//
// This is the mechanism that makes "export the packages before you roll
// back" a mandatory procedure rather than advice, and it is why the
// allow-list is not weakened to hide the incompatibility.
func TestSkillPackageBinaryRollbackFailsClosed(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)
	skillPkgSeedPackage(t, dm, "skill:pkg-rollback", "eee555")

	// A new binary's backup contains the package tables.
	var dump strings.Builder
	for _, name := range skillPkgTableNames {
		dump.WriteString(skillPkgStoredDDL(t, dm.SQLDB(), "main", name))
		dump.WriteString(";\n")
	}
	if !strings.Contains(dump.String(), "CREATE TABLE skill_package_blobs") {
		t.Fatal("the backup does not contain the package tables")
	}

	// The new binary's own restore accepts it.
	if _, err := NewCanonicalDumpValidator().Prepare(dump.String()); err != nil {
		t.Fatalf("the new binary refuses its own backup: %v", err)
	}

	// An old binary's restore refuses it — the allow-list is the whole
	// mechanism, and it fails CLOSED.
	var oldAllowList []string
	for _, n := range CanonicalMPMSchema {
		isPkg := false
		for _, pkg := range skillPkgTableNames {
			if n == pkg {
				isPkg = true
			}
		}
		if !isPkg {
			oldAllowList = append(oldAllowList, n)
		}
	}
	if err := NewDumpValidator(oldAllowList).Validate(dump.String()); err == nil {
		t.Error("an old binary's restore-db ACCEPTED a package-bearing dump — the rollback would not fail closed")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10 — the operator-checkout hygiene helper
// ─────────────────────────────────────────────────────────────────────────────

// TestSkillPkgNoLiveDatabaseTouched asserts the tests never open the live
// ~/.mpm database. It is cheap insurance against a future edit to a
// helper that drops the MPM_WORKSPACE override.
func TestSkillPkgNoLiveDatabaseTouched(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	live := filepath.Join(home, ".mpm")
	if _, err := os.Stat(live); os.IsNotExist(err) {
		t.Skip("no live ~/.mpm on this host")
	}
	// The live database must not have been modified by this package's tests.
	// We do not assert on its contents — only that our own workspace is
	// somewhere else entirely.
	ws := skillPkgWorkspace(t)
	if strings.HasPrefix(ws, live) {
		t.Errorf("the disposable workspace %q is inside the live ~/.mpm %q", ws, live)
	}
	dm := skillPkgDM(t)
	if strings.HasPrefix(dm.dbPath, live) {
		t.Errorf("the test opened the live database %q", dm.dbPath)
	}
}
