// inmemory_dsn_no_filesystem_test.go — regression coverage for the
// ":memory:"-becomes-a-directory defect.
//
// NewDatabaseManager takes a *workspace root*. It is filesystem-only: it
// derives `<root>/src/db/mpm.db`, MkdirAll's the directory, and hangs the
// watchdog and mirror streams off the same root.
//
// A test passed ":memory:" to it, expecting an in-memory database. What
// actually happened was that a real directory literally named ":memory:"
// was created, containing a real file at ":memory:/src/db/mpm.db",
// relative to the process working directory — i.e. inside the package
// directory of whichever module was under test. That residue
// (internal/core/:memory:/src/db/mpm.db) was found during a production
// recovery, months after it was created, because nothing failed: the
// "in-memory" database was quietly a shared file on disk.
//
// The invariant these tests pin: asking for in-memory SQLite must never
// put anything on the filesystem, and asking for a real workspace must
// keep working exactly as before.

package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memoryDSNForms are the DSN spellings mattn/go-sqlite3 honours as
// in-memory. Each must be recognised as a DSN, never as a path.
var memoryDSNForms = []string{
	":memory:",
	"file::memory:",
	"file::memory:?cache=shared",
	"file:mpm-test?mode=memory&cache=shared",
}

// TestNewDatabaseManager_MemoryDSNCreatesNoFiles is the core regression.
// It runs from a controlled temp cwd so any filesystem side effect is
// observable, then asserts nothing at all was created.
func TestNewDatabaseManager_MemoryDSNCreatesNoFiles(t *testing.T) {
	for _, dsn := range memoryDSNForms {
		t.Run(dsn, func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)

			before, err := os.ReadDir(cwd)
			if err != nil {
				t.Fatalf("read temp cwd: %v", err)
			}
			require0 := func(stage string) {
				after, err := os.ReadDir(cwd)
				if err != nil {
					t.Fatalf("read temp cwd after %s: %v", stage, err)
				}
				if len(after) != len(before) {
					names := make([]string, 0, len(after))
					for _, e := range after {
						names = append(names, e.Name())
					}
					t.Fatalf("%s created filesystem entries in cwd: %v", stage, names)
				}
				// Belt and braces: name the specific artefact the defect
				// produced, so a regression reports the cause and not just
				// a count mismatch.
				if _, err := os.Stat(filepath.Join(cwd, ":memory:")); err == nil {
					t.Fatalf("%s created a directory named \":memory:\" in cwd", stage)
				}
			}

			dm, err := NewDatabaseManager(dsn)
			if err == nil {
				// If a future change teaches this constructor to honour
				// in-memory DSNs, that is a legitimate outcome — but it
				// still must not have written anything.
				dm.Close()
				t.Logf("constructor accepted %q; asserting no filesystem artefacts", dsn)
			} else {
				// The contract today: a DSN is a caller error, reported
				// clearly rather than silently materialised.
				if !strings.Contains(err.Error(), "in-memory DSN") {
					t.Fatalf("error for DSN %q should explain the DSN/workspace "+
						"mismatch, got: %v", dsn, err)
				}
			}
			require0("NewDatabaseManager")
		})
	}
}

// TestNewTestDM_CreatesNoFilesystemArtifacts pins the supported in-memory
// path: DB behaviour succeeds and the filesystem is untouched.
func TestNewTestDM_CreatesNoFilesystemArtifacts(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	dm := NewTestDM(t)

	// DB behaviour must actually work, not merely avoid littering.
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO works (id, title, status, verification, created_at, updated_at, session_id)
		 VALUES ('w-mem-1', 'in-memory work', 'open', 'unverified', 1, 1, '')`,
	); err != nil {
		t.Fatalf("in-memory DB rejected a write: %v", err)
	}
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM works WHERE id = 'w-mem-1'`).Scan(&n); err != nil {
		t.Fatalf("in-memory DB rejected a read: %v", err)
	}
	if n != 1 {
		t.Fatalf("in-memory DB write not visible: got %d rows, want 1", n)
	}

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatalf("read temp cwd: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("in-memory manager created filesystem entries: %v", names)
	}
}

// TestNewDatabaseManager_FileWorkspaceUnchanged pins the normal
// file-backed path, so the guard cannot have been implemented by
// breaking real runtime behaviour.
func TestNewDatabaseManager_FileWorkspaceUnchanged(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	ws := filepath.Join(t.TempDir(), "workspace")

	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("file-backed workspace rejected: %v", err)
	}
	defer dm.Close()

	wantDB := filepath.Join(ws, "src", "db", "mpm.db")
	if got := dm.DBPath(); got != wantDB {
		t.Fatalf("DBPath = %q, want %q", got, wantDB)
	}
	if _, err := os.Stat(wantDB); err != nil {
		t.Fatalf("real DB file was not created at %s: %v", wantDB, err)
	}

	// The DB must be genuinely usable and durable on disk.
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO works (id, title, status, verification, created_at, updated_at, session_id)
		 VALUES ('w-file-1', 'file work', 'open', 'unverified', 1, 1, '')`,
	); err != nil {
		t.Fatalf("file-backed DB rejected a write: %v", err)
	}

	// The derived log surfaces must live under the workspace, not in cwd.
	if dm.watchdogPath != "" {
		if !strings.HasPrefix(dm.watchdogPath, ws) {
			t.Errorf("watchdog path %q escaped the workspace %q", dm.watchdogPath, ws)
		}
	}
}

// TestIsInMemoryDSN pins the classifier itself, including the negative
// cases: a real workspace path that merely contains a colon or the word
// "memory" must NOT be misclassified.
func TestIsInMemoryDSN(t *testing.T) {
	positives := []string{
		":memory:",
		"file::memory:",
		"file::memory:?cache=shared",
		"file:anything?mode=memory&cache=shared",
	}
	negatives := []string{
		"",
		"/home/v/.mpm",
		"./workspace",
		"/tmp/memory-workspace",
		"/tmp/has:colon",
		"file:/tmp/real.db",
		"/home/user/projects/memory",
	}
	for _, s := range positives {
		if !IsInMemoryDSN(s) {
			t.Errorf("IsInMemoryDSN(%q) = false, want true", s)
		}
	}
	for _, s := range negatives {
		if IsInMemoryDSN(s) {
			t.Errorf("IsInMemoryDSN(%q) = true, want false — a real workspace "+
				"path must never be mistaken for a DSN", s)
		}
	}
}
