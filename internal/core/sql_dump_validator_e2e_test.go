package internal

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestDumpValidator_E2E_RestoreAbortsOnTamperedDump proves the security
// guarantee end-to-end against a real SQLite database:
//
//  1. A real DB exists with baseline data.
//  2. A tampered dump is loaded from disk.
//  3. The validator is built from the live DB's sqlite_master and runs
//     Prepare() on the tampered dump.
//  4. Prepare() rejects — no statements returned.
//  5. The DB is verified untouched: baseline data intact, no
//     'evil' attachment created, no mass overwrite happened.
//
// This is the realistic threat scenario: an attacker drops a tampered
// .sql file in the DB directory, the user runs `mpm restore-db`, and the
// validator catches the malicious content before any SQL reaches SQLite.
func TestDumpValidator_E2E_RestoreAbortsOnTamperedDump(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// 1. Set up a real DB with baseline data.
	setupDB := func() {
		db, err := sql.Open("sqlite3", dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE memories (id INTEGER PRIMARY KEY, content TEXT, weight INTEGER)`); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO memories VALUES (1, 'baseline data', 50)`); err != nil {
			t.Fatalf("insert baseline: %v", err)
		}
	}
	setupDB()

	// 2. Load a tampered dump from disk.
	for _, fixture := range []string{"bad_attach.sql", "bad_update_system.sql", "bad_delete_system.sql", "bad_pragmas.sql"} {
		t.Run(fixture, func(t *testing.T) {
			tampered, err := os.ReadFile(filepath.Join(fixtureDir, fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			// 3. Build validator from the live DB.
			validator, err := NewDumpValidatorFromDB(dbPath)
			if err != nil {
				t.Fatalf("build validator: %v", err)
			}

			// 4. Prepare should reject — no statements returned.
			statements, err := validator.Prepare(string(tampered))
			if err == nil {
				t.Fatalf("expected reject for %s, got nil error (would have executed %d statements)", fixture, len(statements))
			}
			if len(statements) != 0 {
				t.Fatalf("expected 0 statements on rejection, got %d", len(statements))
			}

			// 5. Verify DB is untouched: baseline data intact.
			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("verify open: %v", err)
			}
			defer db.Close()
			var content string
			var weight int
			err = db.QueryRow(`SELECT content, weight FROM memories WHERE id=1`).Scan(&content, &weight)
			if err != nil {
				t.Fatalf("baseline data missing (DB was modified despite rejection): %v", err)
			}
			if content != "baseline data" || weight != 50 {
				t.Fatalf("baseline data corrupted: content=%q weight=%d", content, weight)
			}

			// 5b. Verify no attachment was created (for bad_attach specifically).
			if fixture == "bad_attach.sql" {
				var attached int
				err = db.QueryRow(`SELECT COUNT(*) FROM pragma_database_list WHERE name NOT IN ('main', 'temp')`).Scan(&attached)
				if err != nil {
					t.Fatalf("check attachments: %v", err)
				}
				if attached != 0 {
					t.Fatalf("ATTACH succeeded despite rejection: %d external databases", attached)
				}
			}

			// 5c. Verify no mass overwrite happened (for bad_update specifically).
			if fixture == "bad_update_system.sql" {
				var w int
				db.QueryRow(`SELECT weight FROM memories WHERE id=1`).Scan(&w)
				if w != 50 {
					t.Fatalf("UPDATE succeeded despite rejection: weight=%d (expected 50)", w)
				}
			}

			// 5d. Verify no mass delete happened (for bad_delete specifically).
			if fixture == "bad_delete_system.sql" {
				var count int
				db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count)
				if count != 1 {
					t.Fatalf("DELETE succeeded despite rejection: %d rows (expected 1)", count)
				}
			}
		})
	}
}

// TestDumpValidator_E2E_GoodDumpRestoresRoundtrip proves the happy path:
// a valid dump is accepted, statements are returned, executing them
// restores the expected state to a fresh DB. Uses the canonical validator
// so the empty target DB doesn't matter — the canonical allow-list knows
// about `memories` regardless of whether it's in the live DB yet.
func TestDumpValidator_E2E_GoodDumpRestoresRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "restore-target.db")

	// Create the target DB file (empty schema). The dump's CREATE TABLE
	// and CREATE INDEX statements will populate it.
	setupDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	setupDB.Close()

	// Load good_dump.sql.
	good, err := os.ReadFile(filepath.Join(fixtureDir, "good_dump.sql"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Build the canonical validator — works regardless of live DB state.
	validator := NewCanonicalDumpValidator()
	statements, err := validator.Prepare(string(good))
	if err != nil {
		t.Fatalf("expected accept for good_dump, got: %v", err)
	}
	if len(statements) == 0 {
		t.Fatal("expected non-empty statements for good_dump")
	}

	// Execute per-statement (mirrors the handler's per-statement flow).
	// The dump's own BEGIN TRANSACTION / COMMIT provides atomicity — we
	// don't wrap in a Go tx because database/sql refuses nested
	// transactions.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open for exec: %v", err)
	}
	defer db.Close()
	for i, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec statement %d: %v", i+1, err)
		}
	}

	// Verify the restored state.
	verifyDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	defer verifyDB.Close()
	var count int
	if err := verifyDB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows after restore, got %d", count)
	}
	var content string
	if err := verifyDB.QueryRow(`SELECT content FROM memories WHERE id=1`).Scan(&content); err != nil {
		t.Fatalf("query: %v", err)
	}
	if content != "hello world" {
		t.Fatalf("expected 'hello world', got %q", content)
	}
}