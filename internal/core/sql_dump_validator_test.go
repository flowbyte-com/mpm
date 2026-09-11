package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "../../cmd/mpm/testdata/restore_db"

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(fixtureDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// TestDumpValidator_Fixtures exercises the threat-model fixtures shipped
// under cmd/mpm/testdata/restore_db/. Each fixture is a syntactically-valid
// SQLite dump; the validator must accept the benign ones and reject the
// attack vectors.
func TestDumpValidator_Fixtures(t *testing.T) {
	tables := []string{"memories"}
	v := NewDumpValidator(tables)

	cases := []struct {
		fixture string
		accept  bool
		wantErr string // case-insensitive substring expected in error
	}{
		// Benign — must accept
		{"good_dump.sql", true, ""},
		{"comment_spoof.sql", true, ""},     // ATTACH in comment, INSERT is real
		{"string_with_semicolon.sql", true, ""}, // ; inside string literal

		// Malicious — must reject
		{"bad_attach.sql", false, "ATTACH"},
		{"bad_select.sql", false, "SELECT"},
		{"bad_pragmas.sql", false, "PRAGMA"},
		{"bad_unknown_table.sql", false, "unknown table"},
		{"bad_delete_system.sql", false, "DELETE"},
		{"bad_update_system.sql", false, "UPDATE"},
		{"case_variation.sql", false, "ATTACH"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			content := loadFixture(t, tc.fixture)
			err := v.Validate(content)
			if tc.accept {
				if err != nil {
					t.Fatalf("expected accept, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected reject, got nil error")
			}
			if tc.wantErr != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErr)) {
				t.Fatalf("error %q does not contain expected substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDumpValidator_StripsLineComment verifies that -- line comments are
// removed before validation, so a malicious-looking comment doesn't
// trigger a false-positive rejection.
func TestDumpValidator_StripsLineComment(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	content := "-- ATTACH DATABASE 'evil'\nINSERT INTO memories VALUES(1, 'x');"
	if err := v.Validate(content); err != nil {
		t.Fatalf("line comment should be stripped, got error: %v", err)
	}
}

// TestDumpValidator_StripsBlockComment verifies the same for /* */ blocks.
func TestDumpValidator_StripsBlockComment(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	content := "/* ATTACH DATABASE 'evil' */ INSERT INTO memories VALUES(1, 'x');"
	if err := v.Validate(content); err != nil {
		t.Fatalf("block comment should be stripped, got error: %v", err)
	}
}

// TestDumpValidator_CommentTokenBoundary is the bypass-defense test. A
// naive comment-stripper that replaces comments with empty string would
// turn 'INSE/*fake*/RT INTO ...' into 'INSERT INTO ...', which would
// then be incorrectly accepted. The stripper must replace comments with
// whitespace so 'INSE' and 'RT' remain distinct tokens and the validator
// correctly rejects the malformed statement as 'INSE'.
func TestDumpValidator_CommentTokenBoundary(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	content := "INSE/*fake*/RT INTO memories VALUES(1, 'x');"
	err := v.Validate(content)
	if err == nil {
		t.Fatalf("expected reject for comment-injected token, got nil error")
	}
	if !strings.Contains(err.Error(), "INSE") {
		t.Fatalf("expected error to mention INSE (proving token boundary preserved), got: %v", err)
	}
}

// TestDumpValidator_LineCommentInsideString is the 2026-09-11 regression
// test for the Stage 0 pre-probe restore blocker. The pre-fix
// stripSQLComments was string-literal blind: it treated any `--` as
// comment-start, even inside a single-quoted string value. That caused
// the comment-strip loop to consume the rest of the line — including
// any `''` quote escapes that splitSQLStatements relies on to keep
// string-literal state in sync. The downstream parser would then
// misclassify a `;` later in the string as a statement boundary and
// trip on the orphan fragment.
//
// Pre-fix repro (truncated from the pre-probe dump, lesson about
// `git log --since=<binary-mtime>` inside the lesson content):
//
//	INSERT INTO memories VALUES('id1', 'cmd -- flag and binary''s stuff', ...);
//	INSERT INTO memories VALUES('id2', 'next row', ...);
//
// The first INSERT is one statement. Pre-fix the validator split it on
// the `--` (treating it as a comment) and then on a `;` inside the
// string (because the consumed line also ate the `''` escape, leaving
// the parser's quote state out of sync). The orphan fragment started
// with `s` or whatever came after `--`, but more importantly the next
// row got parsed as a new statement that began with `s` or whatever
// prefix was leaked — none of which started with an allowed keyword.
//
// Post-fix: `--` inside a single-quoted string is treated as literal
// content; only `--` outside strings is treated as a comment marker.
func TestDumpValidator_LineCommentInsideString(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})

	// The minimum reproducer: a single `--` inside a string value, then
	// a `;` that terminates the row, then a second valid row.
	content := "INSERT INTO memories VALUES('id1','cmd -- flag inside string','','','','','','',1.0,0,'',NULL,NULL,NULL);\n" +
		"INSERT INTO memories VALUES('id2','next row','','','','','','',1.0,0,'',NULL,NULL,NULL);\n"
	if err := v.Validate(content); err != nil {
		t.Fatalf("validator must accept `--` inside string literal, got: %v", err)
	}

	// The Stage 0 actual reproducer: a `''` quote escape later in the
	// same string, after the `--`. This is the precise pattern from the
	// pre-probe dump that split the second row from the first.
	content2 := "INSERT INTO memories VALUES('id1','cmd -- flag and binary''s stuff here','','','','','','',1.0,0,'',NULL,NULL,NULL);\n" +
		"INSERT INTO memories VALUES('id2','next row','','','','','','',1.0,0,'',NULL,NULL,NULL);\n"
	if err := v.Validate(content2); err != nil {
		t.Fatalf("validator must accept `''` escape after `--` inside string, got: %v", err)
	}

	// Block comment inside a string: must also be treated as content.
	content3 := "INSERT INTO memories VALUES('id1','cmd /* fake */ flag','','','','','','',1.0,0,'',NULL,NULL,NULL);\n"
	if err := v.Validate(content3); err != nil {
		t.Fatalf("validator must accept `/* */` inside string literal, got: %v", err)
	}

	// And the existing bypass-defense guarantee must still hold: a
	// real `--` line comment OUTSIDE strings must still strip cleanly.
	content4 := "-- ATTACH DATABASE 'evil'\nINSERT INTO memories VALUES('id1','x','','','','','','',1.0,0,'',NULL,NULL,NULL);\n"
	if err := v.Validate(content4); err != nil {
		t.Fatalf("validator must still accept dump with leading line comment, got: %v", err)
	}
}

// TestDumpValidator_EmptyContent confirms that empty/whitespace-only
// dumps don't trigger false positives.
func TestDumpValidator_EmptyContent(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	for _, content := range []string{"", "   ", "\n\n\n", "  \n  \t  \n"} {
		if err := v.Validate(content); err != nil {
			t.Errorf("expected accept for %q, got: %v", content, err)
		}
	}
}

// TestDumpValidator_RejectsUnknownKeyword enumerates the destructive
// SQL keywords the validator must reject.
func TestDumpValidator_RejectsUnknownKeyword(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	cases := []string{
		"DROP TABLE memories",
		"ALTER TABLE memories ADD COLUMN x TEXT",
		"REPLACE INTO memories VALUES(1, 'x')",
		"RENAME TABLE memories TO evil",
		"TRUNCATE memories",
		"VACUUM",
		"REINDEX",
		"ANALYZE",
		"EXEC evil_command",
		"LOAD '/tmp/evil'",
		"DETACH DATABASE evil",
		"CREATE VIRTUAL TABLE vt USING fts5(x)",
		"SELECT * FROM memories",
	}
	for _, content := range cases {
		if err := v.Validate(content); err == nil {
			t.Errorf("expected reject for %q, got nil error", content)
		}
	}
}

// TestDumpValidator_FTS5ShadowTables pins the 2026-09-11 pre-probe restore
// regression. The dump from a database with FTS5 virtual tables contains
// `CREATE TABLE` statements for the shadow tables (`<base>_fts_data`,
// `_fts_idx`, `_fts_content`, `_fts_docsize`, `_fs_config`) that SQLite
// creates internally when the virtual table is built. The pre-fix
// allow-list (a plain `map[string]bool` of explicit table names) did not
// cover them, so any FTS5-enabled restore was rejected.
//
// Post-fix: the validator recognises the shadow-table suffixes and accepts
// them when the corresponding `<base>_fts` virtual table is on the
// explicit allow-list. The base MUST still be allow-listed — i.e. you
// cannot smuggle shadow tables in for an FTS module that wasn't already
// declared in the canonical schema. This test exercises all five
// suffixes plus the security boundary (a shadow for a non-allow-listed
// base must still be rejected).
func TestDumpValidator_FTS5ShadowTables(t *testing.T) {
	v := NewDumpValidator([]string{"memories", "memories_fts"})

	allowedShadowTables := []string{
		"memories_fts_data",
		"memories_fts_idx",
		"memories_fts_content",
		"memories_fts_docsize",
		"memories_fts_config",
	}
	for _, tbl := range allowedShadowTables {
		// CREATE TABLE
		create := "CREATE TABLE '" + tbl + "'(id INTEGER PRIMARY KEY, block BLOB);"
		if err := v.Validate(create); err != nil {
			t.Errorf("CREATE on shadow table %s should be accepted, got: %v", tbl, err)
		}
		// INSERT
		insert := "INSERT INTO " + tbl + " VALUES(1, X'00');"
		if err := v.Validate(insert); err != nil {
			t.Errorf("INSERT into shadow table %s should be accepted, got: %v", tbl, err)
		}
	}

	// Security boundary: a shadow table whose base is NOT on the allow-list
	// must still be rejected. This is what stops the shadow-table pattern
	// from being an injection vector.
	disallowedShadows := []string{
		"CREATE TABLE 'evil_fts_data'(id INTEGER PRIMARY KEY);",
		"CREATE TABLE 'sessions_fts_data'(id INTEGER PRIMARY KEY);", // sessions_fts not on allow-list
		"INSERT INTO evil_fts_content VALUES(1, 'x');",
	}
	for _, content := range disallowedShadows {
		if err := v.Validate(content); err == nil {
			t.Errorf("shadow table for non-allow-listed base must be rejected: %q", content)
		}
	}

	// Make sure the non-shadow tail suffix isn't mistaken for a shadow table.
	if err := v.Validate("CREATE TABLE 'memories_ft_data'(id INTEGER PRIMARY KEY);"); err == nil {
		t.Errorf("table with non-shadow suffix must be rejected")
	}
}

// TestDumpValidator_PRAGMAOnlyForeignKeys verifies the strict PRAGMA
// allow-list. Only `foreign_keys` is permitted (it's emitted by
// `sqlite3 .dump`); every other PRAGMA must be rejected.
func TestDumpValidator_PRAGMAOnlyForeignKeys(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	allowed := []string{
		"PRAGMA foreign_keys",
		"PRAGMA foreign_keys=OFF",
		"PRAGMA foreign_keys = ON",
		"PRAGMA foreign_keys=off",
	}
	denied := []string{
		"PRAGMA writable_schema = 1",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = OFF",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -10000",
	}
	for _, c := range allowed {
		if err := v.Validate(c); err != nil {
			t.Errorf("expected accept for %q, got: %v", c, err)
		}
	}
	for _, c := range denied {
		if err := v.Validate(c); err == nil {
			t.Errorf("expected reject for %q, got nil error", c)
		}
	}
}

// TestDumpValidator_InsertOrReplace allows the SQLite conflict-resolution
// forms: INSERT OR REPLACE/ABORT/IGNORE/FAIL/ROLLBACK INTO <table>.
func TestDumpValidator_InsertOrReplace(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	allowed := []string{
		"INSERT OR REPLACE INTO memories VALUES(1, 'x')",
		"INSERT OR ABORT INTO memories VALUES(1, 'x')",
		"INSERT OR IGNORE INTO memories VALUES(1, 'x')",
		"INSERT OR FAIL INTO memories VALUES(1, 'x')",
		"INSERT OR ROLLBACK INTO memories VALUES(1, 'x')",
	}
	for _, c := range allowed {
		if err := v.Validate(c); err != nil {
			t.Errorf("expected accept for %q, got: %v", c, err)
		}
	}
}

// TestDumpValidator_QuotedTableName verifies that table names wrapped in
// double quotes or square brackets (SQLite identifier quoting) are
// correctly recognized.
func TestDumpValidator_QuotedTableName(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	cases := []string{
		`INSERT INTO "memories" VALUES(1, 'x')`,
		"INSERT INTO [memories] VALUES(1, 'x')",
		`CREATE TABLE "memories" (id INTEGER)`,
	}
	for _, c := range cases {
		if err := v.Validate(c); err != nil {
			t.Errorf("expected accept for %q, got: %v", c, err)
		}
	}
}

// TestDumpValidator_StringWithEscapedQuote verifies the `''` escape for
// embedded single quotes is handled correctly (semicolons inside the
// escaped-string region must not split the statement).
func TestDumpValidator_StringWithEscapedQuote(t *testing.T) {
	v := NewDumpValidator([]string{"memories"})
	content := "INSERT INTO memories VALUES(1, 'O''Reilly');"
	if err := v.Validate(content); err != nil {
		t.Fatalf("expected accept for escaped-quote string, got: %v", err)
	}
}