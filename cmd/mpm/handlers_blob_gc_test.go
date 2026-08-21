package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// setupBlobGCHandler creates a temp dir, sets MPM_BLOB_DIR, injects the test
// DatabaseManager into blobGC_DB, and returns the blobDir and a cleanup func.
func setupBlobGCHandler(t *testing.T, dm *mpminternal.DatabaseManager) (blobDir string) {
	tmpDir := t.TempDir()
	blobDir = filepath.Join(tmpDir, "blobs")
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	blobGC_DB = dm
	t.Setenv("MPM_BLOB_DIR", blobDir)
	return blobDir
}

// insertBlob inserts a blob row directly into the database.
func insertBlob(t *testing.T, dm *mpminternal.DatabaseManager, id, sourceTool string, sizeBytes int64, contentType string, createdAt, expiresAt time.Time) {
	_, err := dm.SQLDB().Exec(`
		INSERT INTO blobs (id, source_tool, size_bytes, content_type, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, sourceTool, sizeBytes, contentType, createdAt.Unix(), expiresAt.Unix())
	if err != nil {
		t.Fatalf("insertBlob %s: %v", id, err)
	}
}

// writeBlobFile writes a physical file in the blob directory without a DB row.
func writeBlobFile(t *testing.T, blobDir, id string, content string) {
	path := filepath.Join(blobDir, id)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeBlobFile %s: %v", id, err)
	}
}

// TestBlobGC_DryRun verifies that --dry-run reports without deleting.
func TestBlobGC_DryRun(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	blobDir := setupBlobGCHandler(t, dm)

	now := time.Now()
	insertBlob(t, dm, "expired-blob", "test-tool", 100, "application/json",
		now.Add(-48*time.Hour), now.Add(-1*time.Hour))
	writeBlobFile(t, blobDir, "expired-blob", "payload")

	rc := handleBlobGC([]string{"--dry-run"})
	if rc != 0 {
		t.Errorf("handleBlobGC --dry-run: got exit %d, want 0", rc)
	}

	// File should still exist.
	if _, err := os.Stat(filepath.Join(blobDir, "expired-blob")); os.IsNotExist(err) {
		t.Errorf("expired blob file was deleted on dry-run")
	}

	// DB row should still exist.
	var count int
	if err := dm.SQLDB().QueryRow("SELECT COUNT(*) FROM blobs WHERE id = ?", "expired-blob").Scan(&count); err != nil {
		t.Fatalf("DB query: %v", err)
	}
	if count != 1 {
		t.Errorf("expired blob row was deleted on dry-run: got count=%d, want 1", count)
	}
}

// TestBlobGC_ExpiredPass verifies that expired blobs are deleted on a real run.
func TestBlobGC_ExpiredPass(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	blobDir := setupBlobGCHandler(t, dm)

	now := time.Now()

	// Insert an expired blob.
	insertBlob(t, dm, "already-expired", "test-tool", 42, "text/plain",
		now.Add(-72*time.Hour), now.Add(-2*time.Hour))
	writeBlobFile(t, blobDir, "already-expired", "expired payload")

	// Insert a non-expired blob.
	insertBlob(t, dm, "still-valid", "test-tool", 42, "text/plain",
		now.Add(-1*time.Hour), now.Add(24*time.Hour))
	writeBlobFile(t, blobDir, "still-valid", "valid payload")

	rc := handleBlobGC([]string{})
	if rc != 0 {
		t.Errorf("handleBlobGC: got exit %d, want 0", rc)
	}

	// Expired file should be gone.
	if _, err := os.Stat(filepath.Join(blobDir, "already-expired")); !os.IsNotExist(err) {
		t.Errorf("expired blob file still exists after GC")
	}

	// Valid file should remain.
	if _, err := os.Stat(filepath.Join(blobDir, "still-valid")); os.IsNotExist(err) {
		t.Errorf("valid blob file was deleted prematurely")
	}

	// Expired DB row should be gone.
	var count int
	if err := dm.SQLDB().QueryRow("SELECT COUNT(*) FROM blobs WHERE id = ?", "already-expired").Scan(&count); err != nil {
		t.Fatalf("DB query: %v", err)
	}
	if count != 0 {
		t.Errorf("expired blob row still present: got count=%d, want 0", count)
	}
}

// TestBlobGC_OrphanSweepRespectsGrace verifies that orphan files within the grace
// period are skipped, while older orphans are deleted.
func TestBlobGC_OrphanSweepRespectsGrace(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	blobDir := setupBlobGCHandler(t, dm)

	now := time.Now()

	// Create an orphan file with an old mtime (simulate age > grace).
	oldOrphan := filepath.Join(blobDir, "old-orphan")
	if err := os.WriteFile(oldOrphan, []byte("old orphan"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	os.Chtimes(oldOrphan, now.Add(-2*time.Hour), now.Add(-2*time.Hour))

	// Create an orphan file with a recent mtime (within grace period).
	freshOrphan := filepath.Join(blobDir, "fresh-orphan")
	if err := os.WriteFile(freshOrphan, []byte("fresh orphan"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	origGrace := os.Getenv("MPM_BLOB_ORPHAN_GRACE")
	os.Setenv("MPM_BLOB_ORPHAN_GRACE", "1h")
	defer func() {
		if origGrace != "" {
			os.Setenv("MPM_BLOB_ORPHAN_GRACE", origGrace)
		} else {
			os.Unsetenv("MPM_BLOB_ORPHAN_GRACE")
		}
	}()

	rc := handleBlobGC([]string{})
	if rc != 0 {
		t.Errorf("handleBlobGC: got exit %d, want 0", rc)
	}

	// Old orphan should be deleted.
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Errorf("old orphan file still exists after GC (should have been swept)")
	}

	// Fresh orphan should remain (within grace).
	if _, err := os.Stat(freshOrphan); os.IsNotExist(err) {
		t.Errorf("fresh orphan file was deleted (should have been skipped as within grace period)")
	}
}

// TestBlobGC_CrashRecovery_BothDirections verifies the two crash-recovery scenarios:
// 1. File exists but no DB row (GCSweepOrphans handles the orphan file).
// 2. DB row exists but no file (GCSweepOrphans logs and deletes the orphan row).
func TestBlobGC_CrashRecovery_BothDirections(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	blobDir := setupBlobGCHandler(t, dm)

	now := time.Now()

	// Scenario A: file exists, no DB row → GCSweepOrphans deletes the orphan file.
	orphanFile := filepath.Join(blobDir, "orphan-file-no-db")
	if err := os.WriteFile(orphanFile, []byte("orphan payload"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	os.Chtimes(orphanFile, now.Add(-2*time.Hour), now.Add(-2*time.Hour))

	// Scenario B: DB row exists, no file → GCSweepOrphans deletes the orphan row.
	insertBlob(t, dm, "orphan-db-no-file", "test-tool", 99, "application/json",
		now.Add(-2*time.Hour), now.Add(24*time.Hour))

	rc := handleBlobGC([]string{})
	if rc != 0 {
		t.Errorf("handleBlobGC: got exit %d, want 0", rc)
	}

	// Scenario A: file should be gone.
	if _, err := os.Stat(orphanFile); !os.IsNotExist(err) {
		t.Errorf("orphan file (no DB row) still exists after GC")
	}

	// Scenario B: DB row should be gone.
	var count int
	if err := dm.SQLDB().QueryRow("SELECT COUNT(*) FROM blobs WHERE id = ?", "orphan-db-no-file").Scan(&count); err != nil {
		t.Fatalf("DB query: %v", err)
	}
	t.Logf("orphan-db-no-file count after GC: %d", count)
	if count != 0 {
		t.Errorf("orphan DB row (no file) still present after GC: got count=%d, want 0", count)
	}
}
