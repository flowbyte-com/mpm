package internal

import (
	"os"
	"path/filepath"
	"testing"
)

// Refactor 2026-08-12: replaced direct NewDatabaseManager("") calls with
// NewTestLocalOnlyDM / NewTestSharedDM. The previous pattern relied on
// ambient MPM_SHARED_DB to route the shared DB to a per-test tmpfile but
// left the local DB pointed at config.GetMPMDir() — which under a normal
// `go test ./...` invocation defaults to ~/.mpm/src/db/mpm.db and
// pollutes the live production database with per-test fixtures. The
// hermetic helpers set MPM_WORKSPACE to t.TempDir() so neither the
// local DB nor the watchdog.jsonl ever touches production state.

// TestSharedAttach_DisabledByDefault verifies that without MPM_SHARED_DB,
// the DatabaseManager starts in local-only mode and SharedAttached() returns "".
func TestSharedAttach_DisabledByDefault(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	if got := dm.SharedAttached(); got != "" {
		t.Fatalf("expected SharedAttached() to be empty, got %q", got)
	}
}

// TestSharedAttach_HappyPath verifies that a valid MPM_SHARED_DB is
// attached and SharedAttached() reports the path.
func TestSharedAttach_HappyPath(t *testing.T) {
	dm := NewTestSharedDM(t)
	sharedPath := dm.SharedAttached()
	if sharedPath == "" {
		t.Fatal("shared DB did not attach")
	}

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
	// cannot be created (we point at a non-directory parent). MPM_WORKSPACE
	// is set to a sibling tmpdir so the local DB also lives under
	// t.TempDir() and never touches the production database — even though
	// this test only exercises the attach-failure path.
	tmp := dmSharedTmpForBadPath(t)
	t.Setenv("MPM_WORKSPACE", tmp)
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
	t.Cleanup(func() { dm.Close() })
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
	t.Setenv("MPM_WORKSPACE", tmp)
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "1")

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	if got := dm.SharedAttached(); got != sharedPath {
		t.Errorf("SharedAttached() = %q, want %q", got, sharedPath)
	}
}

// dmSharedTmpForBadPath returns the temp directory NewTestSharedDM would
// use, so the bad-path test can drop a blocker file there before
// constructing a fresh DatabaseManager with the failing attach path.
func dmSharedTmpForBadPath(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}