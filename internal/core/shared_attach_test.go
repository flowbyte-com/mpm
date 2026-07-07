package internal

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSharedAttach_DisabledByDefault verifies that without MPM_SHARED_DB,
// the DatabaseManager starts in local-only mode and SharedAttached() returns "".
func TestSharedAttach_DisabledByDefault(t *testing.T) {
	t.Setenv("MPM_SHARED_DB", "")
	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if got := dm.SharedAttached(); got != "" {
		t.Fatalf("expected SharedAttached() to be empty, got %q", got)
	}
}

// TestSharedAttach_HappyPath verifies that a valid MPM_SHARED_DB is
// attached and SharedAttached() reports the path.
func TestSharedAttach_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	sharedPath := filepath.Join(tmp, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	if got := dm.SharedAttached(); got != sharedPath {
		t.Errorf("SharedAttached() = %q, want %q", got, sharedPath)
	}

	// Verify the shared DB file was created.
	if _, err := os.Stat(sharedPath); err != nil {
		t.Fatalf("shared DB file not created: %v", err)
	}
}

// TestSharedAttach_BadPathFallsBackGracefully verifies that an invalid
// MPM_SHARED_DB path logs a warning but does NOT fail DatabaseManager init.
func TestSharedAttach_BadPathFallsBackGracefully(t *testing.T) {
	// Use a path that will fail ATTACH: directory doesn't exist and
	// cannot be created (we point at a non-directory parent).
	tmp := t.TempDir()
	notADir := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(notADir, []byte("not a dir"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	badPath := filepath.Join(notADir, "subdir", "shared.db")
	t.Setenv("MPM_SHARED_DB", badPath)

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager should succeed even with bad shared path: %v", err)
	}
	defer dm.Close()
	if got := dm.SharedAttached(); got != "" {
		t.Errorf("expected SharedAttached() to be empty on attach failure, got %q", got)
	}
}

// TestSharedAttach_ReadOnlyEnv verifies that MPM_SHARED_READONLY=1 is
// accepted without error (the actual read-only enforcement happens via
// the URI; we just verify the env is parsed).
func TestSharedAttach_ReadOnlyEnv(t *testing.T) {
	tmp := t.TempDir()
	sharedPath := filepath.Join(tmp, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "1")

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if got := dm.SharedAttached(); got != sharedPath {
		t.Errorf("SharedAttached() = %q, want %q", got, sharedPath)
	}
}