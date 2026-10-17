// db_readonly_test.go — focused tests for the read-only DatabaseManager
// constructor (NewDatabaseManagerReadOnly) and the
// HealthCheckReadOnly / HealthCheck cross-mode refusal sentinels.
//
// These are Stage 1 of the MPM #28 staged verification. The
// constructor is the foundational piece the doctor path uses; the
// cross-mode refusal is the structural enforcement of the
// "this code path cannot write" property.
//
// F8 SQLite verification is a separate gate (it exercises ?mode=ro
// against a real DB and a separate writer process). These tests
// pin the constructor's filesystem-mutation contract and the
// HealthCheck shape against a hermetic on-disk fixture.
package internal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestNewDatabaseManagerReadOnly_NoCreateOnMissing pins the
// fail-closed, no-filesystem-mutation contract for the constructor.
// The database file does not exist; the constructor must return an
// error (NOT nil) and must not create the parent directory or any
// file. The specific error is whichever precondition the
// constructor checks first — on a fresh install that has never
// run a write command, both the lock file and the database file
// are missing, and the constructor checks the lock first (the
// coordination primitive). The doctor surfaces
// ErrCoordinatedInspectionUnavailable as a top-level FAIL with
// the "run a write command to establish the lock file" hint.
func TestNewDatabaseManagerReadOnly_NoCreateOnMissing(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	// Pre-condition: no parent directory, no database file, no
	// lock file. The constructor must refuse before any
	// filesystem mutation.
	dbPath := filepath.Join(workspace, "src", "db", "mpm.db")
	lockPath := MaintenanceLockPath(workspace)
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: db file must not exist; got stat err=%v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: lock file must not exist; got stat err=%v", err)
	}

	dm, err := NewDatabaseManagerReadOnly(workspace)
	if dm != nil {
		_ = dm.Close()
		t.Fatalf("expected nil DatabaseManager on missing state, got non-nil")
	}
	if err == nil {
		t.Fatal("expected an error on missing state, got nil")
	}
	// The constructor checks the lock first (the coordination
	// primitive). On a fresh install, ErrMaintenanceLockAbsent is
	// wrapped as ErrCoordinatedInspectionUnavailable. We accept
	// either sentinel; the design only requires a fail-closed
	// refusal and no filesystem mutation.
	if !errors.Is(err, ErrReadOnlyDatabaseNotFound) &&
		!errors.Is(err, ErrCoordinatedInspectionUnavailable) {
		t.Errorf("expected ErrReadOnlyDatabaseNotFound or ErrCoordinatedInspectionUnavailable, got %v", err)
	}

	// Post-condition: nothing was created. The constructor must
	// not have created the db file, the lock file, or the parent
	// directory.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("constructor created the db file: stat err=%v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("constructor created the lock file: stat err=%v", err)
	}
	parentDir := filepath.Dir(dbPath)
	if _, err := os.Stat(parentDir); !os.IsNotExist(err) {
		t.Errorf("constructor created the parent directory: stat err=%v", err)
	}
}

// TestNewDatabaseManagerReadOnly_HealthCheckReadOnly_OnExistingDB
// pins the success path: the constructor opens an existing DB in
// read-only mode, returns a tagged handle, and HealthCheckReadOnly
// runs against it. The doctor service uses this exact shape.
func TestNewDatabaseManagerReadOnly_HealthCheckReadOnly_OnExistingDB(t *testing.T) {
	_ = NewTestDM(t) // verify the writable test path is still reachable
	// We need a real on-disk DB so the read-only constructor has a
	// file to open. Build one at the workspace's canonical
	// location via NewDatabaseManager.
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	// Build a writable DM bound to a fresh on-disk database at
	// the workspace's canonical location. This is the "writable
	// constructor" path the doctor is expected to coexist with.
	writableDM, err := NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("writable constructor: %v", err)
	}
	// Seed one row so the read handle has data to report.
	if _, err := writableDM.db.Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'memories', 'seed', strftime('%s','now'))`,
		"seed-1",
	); err != nil {
		_ = writableDM.Close()
		t.Fatalf("seed memories: %v", err)
	}

	// Now open the same DB read-only. The constructor must
	// succeed because the writable constructor created the lock
	// file as a side effect of NewDatabaseManager.
	roDM, err := NewDatabaseManagerReadOnly(workspace)
	if err != nil {
		_ = writableDM.Close()
		t.Fatalf("read-only constructor: %v", err)
	}
	t.Cleanup(func() { _ = roDM.Close() })

	if !roDM.IsReadOnly() {
		t.Error("IsReadOnly must report true for a read-only DM")
	}

	// HealthCheckReadOnly runs against the read-only handle. It
	// must succeed and report a sane `ok` and at least the seed
	// memory in memories_active.
	res, err := roDM.HealthCheckReadOnly()
	if err != nil {
		t.Fatalf("HealthCheckReadOnly: %v", err)
	}
	if ok, _ := res.Stats["ok"].(bool); !ok {
		t.Errorf("expected ok=true, got %v; warnings=%v", res.Stats["ok"], res.Warnings)
	}
	if n, _ := res.Stats["memories_active"].(int64); n < 1 {
		t.Errorf("expected memories_active >= 1, got %d; warnings=%v", n, res.Warnings)
	}

	// HealthCheck (writable) must REFUSE on the read-only handle.
	if _, err := roDM.HealthCheck(); !errors.Is(err, ErrHealthCheckReadOnlyNotSupported) {
		t.Errorf("HealthCheck on read-only DM must refuse with ErrHealthCheckReadOnlyNotSupported; got %v", err)
	}

	// HealthCheckReadOnly must REFUSE on the writable handle.
	if _, err := writableDM.HealthCheckReadOnly(); !errors.Is(err, ErrHealthCheckNotReadOnly) {
		t.Errorf("HealthCheckReadOnly on writable DM must refuse with ErrHealthCheckNotReadOnly; got %v", err)
	}

	if err := writableDM.Close(); err != nil {
		t.Fatalf("writableDM.Close: %v", err)
	}
}

// TestNewDatabaseManagerReadOnly_RefusesWhenLockFileMissing pins
// the absent-lock fail-closed path. The DB file exists (we created
// it via NewDatabaseManager in a parent test), but we then remove
// the lock file before opening read-only. The constructor must
// refuse with ErrCoordinatedInspectionUnavailable, must NOT create
// the lock file, and must NOT open the SQLite handle.
func TestNewDatabaseManagerReadOnly_RefusesWhenLockFileMissing(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	writableDM, err := NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("writable constructor: %v", err)
	}

	lockPath := MaintenanceLockPath(workspace)
	// Sanity: writable constructor created the lock file.
	if _, err := os.Stat(lockPath); err != nil {
		_ = writableDM.Close()
		t.Fatalf("precondition: writable constructor must create the lock file; stat err=%v", err)
	}

	// Now close the writable DM and remove the lock file. The
	// lock is no longer held by any process; reading the DB
	// without the lock would let a concurrent restore-db race
	// the read.
	if err := writableDM.Close(); err != nil {
		t.Fatalf("writableDM.Close: %v", err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove lock file: %v", err)
	}

	dm, err := NewDatabaseManagerReadOnly(workspace)
	if dm != nil {
		_ = dm.Close()
		t.Fatalf("expected nil DatabaseManager when lock is missing, got non-nil")
	}
	if !errors.Is(err, ErrCoordinatedInspectionUnavailable) {
		t.Fatalf("expected ErrCoordinatedInspectionUnavailable, got %v", err)
	}
	if !errors.Is(err, ErrMaintenanceLockAbsent) {
		t.Errorf("expected ErrCoordinatedInspectionUnavailable to wrap ErrMaintenanceLockAbsent; got %v", err)
	}

	// Post-condition: lock file was NOT created by the failed read.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("constructor recreated the lock file: stat err=%v", err)
	}
}

// TestNewDatabaseManagerReadOnly_RejectsInMemoryDSN pins the
// "this constructor is filesystem-only" contract. An in-memory DSN
// such as ":memory:" or "file::memory:?cache=shared" is not a
// workspace root and the constructor must refuse.
func TestNewDatabaseManagerReadOnly_RejectsInMemoryDSN(t *testing.T) {
	dm, err := NewDatabaseManagerReadOnly(":memory:")
	if dm != nil {
		_ = dm.Close()
		t.Fatalf("expected nil DatabaseManager for in-memory DSN, got non-nil")
	}
	if err == nil {
		t.Fatal("expected an error for in-memory DSN, got nil")
	}
	if !errors.Is(err, ErrCoordinatedInspectionUnavailable) &&
		!errors.Is(err, ErrReadOnlyDatabaseNotFound) {
		// The constructor's first guard is the IsInMemoryDSN check,
		// which returns a wrapped fmt.Errorf (not the read-only
		// sentinels). Verify the error is non-nil and informative.
		// The exact error type is implementation-defined.
		t.Logf("got expected non-sentinel error: %v", err)
	}
}
