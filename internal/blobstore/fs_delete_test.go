// fs_delete_test.go — M-3 audit regression coverage (post-M3, 2026-08-31).
//
// The audit found that FilesystemBackend.Delete swallowed the DB
// DELETE error with `_, _ = f.db.ExecContext(...)`. A non-nil driver
// error (lock failure, FK violation, closed conn) would silently
// leave the row in place while returning nil. The fix surfaces the
// error so the caller learns the persistence guarantee failed.
//
// Tests:
//   - TestFilesystemBackend_Delete_DBErrorSurfaced: close the DB
//     mid-call; Delete must propagate the driver error rather than
//     swallow it.
//   - TestFilesystemBackend_Delete_MissingFileOK: idempotent miss
//     (no row, no file) returns nil.
//   - TestFilesystemBackend_Delete_IdempotentRepeated: deleting the
//     same id twice yields nil on both calls (the second is a clean
//     zero-rows-affected case).
//   - TestFilesystemBackend_Delete_HappyPath: row + file removed.
package blobstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilesystemBackend_Delete_DBErrorSurfaced(t *testing.T) {
	db := setupDB(t)
	createBlobsTable(t, db)
	fs, err := NewFilesystemBackend(db, t.TempDir(), 24*60*60*1000)
	require.NoError(t, err)

	// Close the DB. Subsequent Exec must return a "sql: database is
	// closed" error which the post-fix Delete surfaces instead of
	// swallowing.
	require.NoError(t, db.Close())

	err = fs.Delete(context.Background(), "any-id-doesnt-matter")
	require.Error(t, err, "DB-closed Delete must propagate the driver error")
	require.Contains(t, err.Error(), "delete blob db row",
		"error should identify the DB delete failure")
}

func TestFilesystemBackend_Delete_MissingFileOK(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	createBlobsTable(t, db)
	fs, err := NewFilesystemBackend(db, t.TempDir(), 24*60*60*1000)
	require.NoError(t, err)

	// No row, no file: the idempotent-miss path. Delete must return nil.
	err = fs.Delete(context.Background(), "never-existed")
	require.NoError(t, err, "missing id must be idempotent")
}

func TestFilesystemBackend_Delete_HappyPath(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	createBlobsTable(t, db)
	fs, err := NewFilesystemBackend(db, t.TempDir(), 24*60*60*1000)
	require.NoError(t, err)

	// Put one blob, then delete it.
	body := "hello world"
	ptr, err := fs.Put(context.Background(), strings.NewReader(body), Metadata{
		SourceTool: "test", ContentType: "text/plain", SizeBytes: int64(len(body)),
	})
	require.NoError(t, err)

	// File should exist before delete.
	payloadPath := filepath.Join(fs.blobDir, ptr.ID)
	_, statErr := os.Stat(payloadPath)
	require.NoError(t, statErr, "payload file should exist after Put")

	err = fs.Delete(context.Background(), ptr.ID)
	require.NoError(t, err, "happy-path Delete must succeed")

	// File should be gone after delete.
	_, statErr = os.Stat(payloadPath)
	require.True(t, os.IsNotExist(statErr), "payload file should be removed post-Delete")
}

func TestFilesystemBackend_Delete_IdempotentRepeated(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	createBlobsTable(t, db)
	fs, err := NewFilesystemBackend(db, t.TempDir(), 24*60*60*1000)
	require.NoError(t, err)

	body := "idempotent delete"
	ptr, err := fs.Put(context.Background(), strings.NewReader(body), Metadata{
		SourceTool: "test", ContentType: "text/plain", SizeBytes: int64(len(body)),
	})
	require.NoError(t, err)

	// First delete: row + file removed.
	require.NoError(t, fs.Delete(context.Background(), ptr.ID))

	// Second delete: zero rows affected (idempotent), file already gone.
	// Must still return nil — the documented best-effort/idempotent
	// contract is preserved.
	require.NoError(t, fs.Delete(context.Background(), ptr.ID),
		"repeated Delete must remain idempotent (returns nil)")
}
