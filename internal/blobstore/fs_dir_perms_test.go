package blobstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestNewFilesystemBackend_EnforcesDirMode pins the disclosure-surface
// invariant: the blob directory must be 0o700 after NewFilesystemBackend
// returns, regardless of whether the directory was freshly created or
// already existed with looser permissions.
//
// Background: prior to this regression test, NewFilesystemBackend relied
// solely on os.MkdirAll(blobDir, 0o700), which does not correct an
// already-present directory's mode. An operationally-permissive directory
// (e.g. 0o755 or 0o775, written by an older release or by a debug
// `chmod` for inspecting contents) survived across upgrades, leaving
// blob files readable to any local user on the box. See
// `docs/security/blob-dir-permissions-incident-2026-09-05.md`.
func TestNewFilesystemBackend_EnforcesDirMode(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	t.Run("fresh dir is created at 0o700", func(t *testing.T) {
		dir := t.TempDir()
		blobDir := filepath.Join(dir, "blobs-fresh")
		// Sanity: the dir does not yet exist.
		if _, err := os.Stat(blobDir); err == nil {
			t.Fatalf("preconditions: %s already exists", blobDir)
		}
		if _, err := NewFilesystemBackend(db, blobDir, 0); err != nil {
			t.Fatalf("NewFilesystemBackend: %v", err)
		}
		assertDirMode(t, blobDir, 0o700)
	})

	t.Run("pre-existing permissive dir is forced to 0o700", func(t *testing.T) {
		dir := t.TempDir()
		blobDir := filepath.Join(dir, "blobs-permissive")
		// Pre-create at 0o755 to simulate the state a restore or
		// older debug session would have left behind.
		if err := os.Mkdir(blobDir, 0o755); err != nil {
			t.Fatalf("setup mkdir: %v", err)
		}
		// Sanity: it's currently permissive.
		assertDirMode(t, blobDir, 0o755)

		if _, err := NewFilesystemBackend(db, blobDir, 0); err != nil {
			t.Fatalf("NewFilesystemBackend: %v", err)
		}
		// The fix must have forced this back to 0o700.
		assertDirMode(t, blobDir, 0o700)
	})
}

func assertDirMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	got := info.Mode().Perm()
	if got != want {
		t.Fatalf("%s: mode = %o (%v), want %o (%v)",
			path, got, info.Mode(), want, os.FileMode(want))
	}
}
