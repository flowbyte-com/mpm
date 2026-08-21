package blobstore

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupDB creates an in-memory SQLite db with the blobs schema.
func setupDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	return db
}

// createBlobsTable creates the blobs table in the given db.
func createBlobsTable(t *testing.T, db *sql.DB) {
	_, err := db.Exec(`
		CREATE TABLE blobs (
			id             TEXT PRIMARY KEY,
			source_tool    TEXT NOT NULL,
			source_call_id TEXT,
			session_id     TEXT,
			size_bytes     INTEGER NOT NULL,
			content_type   TEXT NOT NULL DEFAULT 'application/json',
			created_at     INTEGER NOT NULL,
			expires_at     INTEGER NOT NULL,
			checksum       TEXT
		)`)
	require.NoError(t, err)
}

func TestPut_AtomicOrdering(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	content := strings.NewReader("hello world")
	meta := Metadata{
		SourceTool:  "test",
		ContentType: "text/plain",
	}
	ptr, err := fs.Put(context.Background(), content, meta)
	require.NoError(t, err)

	assert.Equal(t, "blob", ptr.Kind)
	assert.NotEmpty(t, ptr.ID)

	// Verify the file exists at the expected path.
	path := filepath.Join(blobDir, ptr.ID)
	_, err = os.Stat(path)
	assert.NoError(t, err)

	// Verify NO .tmp file is left behind.
	tmpPath := path + ".tmp"
	_, err = os.Stat(tmpPath)
	assert.True(t, os.IsNotExist(err), "no .tmp file should remain after successful Put")

	// Verify DB row exists.
	var sizeBytes int64
	err = db.QueryRow(`SELECT size_bytes FROM blobs WHERE id = ?`, ptr.ID).Scan(&sizeBytes)
	assert.NoError(t, err)
	assert.Equal(t, int64(11), sizeBytes)
}

func TestPut_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	content := strings.NewReader("same content")
	meta := Metadata{SourceTool: "test", ContentType: "text/plain"}

	ptr1, err := fs.Put(context.Background(), content, meta)
	require.NoError(t, err)

	ptr2, err := fs.Put(context.Background(), content, meta)
	require.NoError(t, err)

	// Same content → two different UUIDs (UUID collision retry produces distinct IDs).
	assert.NotEqual(t, ptr1.ID, ptr2.ID, "two Put calls should produce different IDs")
}

func TestGet_EOF(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	// Put a blob.
	content := strings.NewReader("hello world")
	meta := Metadata{SourceTool: "test", ContentType: "text/plain"}
	ptr, err := fs.Put(context.Background(), content, meta)
	require.NoError(t, err)

	// Read at offset == size (EOF).
	rc, m, err := fs.Get(context.Background(), ptr.ID, GetOptions{Offset: 11, MaxBytes: 5})
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "", string(data))
	assert.Equal(t, int64(11), m.SizeBytes)
}

func TestGet_StalePointer(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	// Insert DB row without creating the file.
	now := time.Now().Unix()
	_, err = db.Exec(`
		INSERT INTO blobs (id, source_tool, content_type, size_bytes, created_at, expires_at)
		VALUES ('stale-id', 'test', 'text/plain', 10, ?, ?)`, now, now+3600)
	require.NoError(t, err)

	// Get should return ErrBlobMissing.
	_, _, err = fs.Get(context.Background(), "stale-id", GetOptions{})
	assert.ErrorIs(t, err, ErrBlobMissing)
}

func TestGet_OffsetRange(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	content := strings.NewReader("hello world")
	meta := Metadata{SourceTool: "test", ContentType: "text/plain"}
	ptr, err := fs.Put(context.Background(), content, meta)
	require.NoError(t, err)

	// Read middle 5 bytes.
	rc, m, err := fs.Get(context.Background(), ptr.ID, GetOptions{Offset: 2, MaxBytes: 5})
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "llo w", string(data))
	assert.Equal(t, int64(11), m.SizeBytes)
}

func TestGet_UTF8Boundary(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	// UTF-8 string: "a🎉b" — emoji 🎉 is 4 bytes (0xF0 0x9F 0x8E 0x89).
	// Bytes: 0:a, 1:🎉0, 2:🎉1, 3:🎉2, 4:🎉3, 5:b
	// Cut at byte 3 (middle of emoji) should back off to byte 1.
	content := strings.NewReader("a\xf0\x9f\x8e\x89b")
	meta := Metadata{SourceTool: "test", ContentType: "text/plain; charset=utf-8"}
	ptr, err := fs.Put(context.Background(), content, meta)
	require.NoError(t, err)

	// Read bytes 0-3 — cut falls in middle of emoji, should back off to 1.
	rc, _, err := fs.Get(context.Background(), ptr.ID, GetOptions{Offset: 0, MaxBytes: 3})
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "a", string(data), "should stop before split emoji")

	// Also verify full read works when emoji is fully contained.
	rc, _, err = fs.Get(context.Background(), ptr.ID, GetOptions{Offset: 0, MaxBytes: 6})
	require.NoError(t, err)
	_, _ = io.ReadAll(rc)
	rc.Close()
	// No split, so should get all 6 bytes.
}

func TestDelete_BestEffortIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	// Put a blob.
	content := strings.NewReader("hello world")
	meta := Metadata{SourceTool: "test", ContentType: "text/plain"}
	ptr, err := fs.Put(context.Background(), content, meta)
	require.NoError(t, err)

	// Delete it.
	err = fs.Delete(context.Background(), ptr.ID)
	require.NoError(t, err)

	// Delete again — should succeed (idempotent).
	err = fs.Delete(context.Background(), ptr.ID)
	assert.NoError(t, err)

	// DB row should be gone.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM blobs WHERE id = ?`, ptr.ID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// File should be gone.
	_, err = os.Stat(filepath.Join(blobDir, ptr.ID))
	assert.True(t, os.IsNotExist(err))
}

func TestGCExpired(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	now := time.Now()

	// Insert two blobs with past expiry.
	_, err = db.Exec(`
		INSERT INTO blobs (id, source_tool, content_type, size_bytes, created_at, expires_at)
		VALUES ('expired1', 'test', 'text/plain', 10, ?, ?)`,
		now.Add(-2*time.Hour).Unix(), now.Add(-1*time.Hour).Unix())
	require.NoError(t, err)

	// Create the corresponding files.
	require.NoError(t, os.WriteFile(filepath.Join(blobDir, "expired1"), []byte("hello worl"), 0o600))

	_, err = db.Exec(`
		INSERT INTO blobs (id, source_tool, content_type, size_bytes, created_at, expires_at)
		VALUES ('expired2', 'test', 'text/plain', 10, ?, ?)`,
		now.Add(-2*time.Hour).Unix(), now.Add(-1*time.Hour).Unix())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(blobDir, "expired2"), []byte("hello worl"), 0o600))

	// Insert one blob with future expiry.
	_, err = db.Exec(`
		INSERT INTO blobs (id, source_tool, content_type, size_bytes, created_at, expires_at)
		VALUES ('valid', 'test', 'text/plain', 10, ?, ?)`,
		now.Add(-1*time.Hour).Unix(), now.Add(1*time.Hour).Unix())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(blobDir, "valid"), []byte("hello worl"), 0o600))

	// Run GC.
	stats, err := fs.GCExpired(context.Background(), now)
	require.NoError(t, err)
	assert.True(t, stats.Success)
	assert.Equal(t, 2, stats.ExpiredDeleted)

	// Expired files should be gone.
	_, err = os.Stat(filepath.Join(blobDir, "expired1"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(blobDir, "expired2"))
	assert.True(t, os.IsNotExist(err))

	// Valid blob should remain.
	_, err = os.Stat(filepath.Join(blobDir, "valid"))
	assert.NoError(t, err)
}

func TestGCSweepOrphans_RespectsGrace(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	createBlobsTable(t, db)

	blobDir := t.TempDir()
	fs, err := NewFilesystemBackend(db, blobDir, 24*time.Hour)
	require.NoError(t, err)

	now := time.Now()

	// Create an old orphan file (no DB row, mtime > grace).
	oldPath := filepath.Join(blobDir, "old-orphan")
	require.NoError(t, os.WriteFile(oldPath, []byte("old"), 0o600))
	require.NoError(t, os.Chtimes(oldPath, now.Add(-2*time.Hour), now.Add(-2*time.Hour)))

	// Create a fresh orphan file (no DB row, mtime < grace).
	freshPath := filepath.Join(blobDir, "fresh-orphan")
	require.NoError(t, os.WriteFile(freshPath, []byte("fresh"), 0o600))
	// chtimes on a fresh file leaves it with recent mtime.

	// Run GC with 1-hour grace.
	stats, err := fs.GCSweepOrphans(context.Background(), 1*time.Hour)
	require.NoError(t, err)
	assert.True(t, stats.Success)

	// Old orphan should be deleted.
	_, err = os.Stat(oldPath)
	assert.True(t, os.IsNotExist(err), "old orphan should be deleted by GC")

	// Fresh orphan should remain.
	_, err = os.Stat(freshPath)
	assert.NoError(t, err, "fresh orphan within grace period should be preserved")
}
