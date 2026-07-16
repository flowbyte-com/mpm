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