// maintenance_lock.go — canonical lock identity and primitives for the
// H-4 shared/exclusive maintenance lease.
//
// The active MPM database (`<workspace>/src/db/mpm.db`) is protected by
// a flock-based lease that coordinates destructive operations across
// processes:
//
//   - Normal DatabaseManager users (CLI handlers, scheduler, MCP,
//     critic) acquire LOCK_SH on `<dbPath>.lock` for the lifetime of
//     their *sql.DB. Multiple processes may hold LOCK_SH concurrently,
//     so ordinary operations are not blocked by each other; only
//     destructive operations see the lease.
//   - Destructive operations (mpm restore-db, mpm shred database)
//     acquire LOCK_EX|LOCK_NB on the same lock file. The acquisition
//     fails (EWOULDBLOCK) if any DatabaseManager holds LOCK_SH on the
//     same inode, refusing the operation BEFORE any filesystem
//     mutation.
//
// Crash safety: syscall.Flock is kernel-cleared on FD close and on
// process death. The lock file's content is irrelevant; flock locks
// the open file description, not the pathname or file data.
//
// Symlink resolution: MaintenanceLockPath resolves symlinks on the
// workspace root so /real/workspace and /symlink/to/workspace
// converge on the same lock identity. Two concurrent processes that
// point at the same physical workspace via different path spellings
// coordinate correctly.
//
// Cross-platform note: Linux flock(2) "merges" same-process locks —
// LOCK_SH + LOCK_EX on FDs owned by one process upgrade to LOCK_EX
// without EWOULDBLOCK. macOS/BSD flock is per-FD and would return
// EWOULDBLOCK in the same scenario. The H-4 protocol avoids relying
// on either behaviour: destructive operations RELEASE any
// DatabaseManager singleton before attempting LOCK_EX, so the
// upgrade-or-not distinction is moot.

package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// MaintenanceLockPath returns the canonical lock file path for the
// active MPM database rooted at workspace. Symlinks on workspace are
// resolved so /real/workspace and /symlink/to/workspace converge on
// the same lock identity.
//
// If the workspace does not exist yet (cold install) EvalSymlinks
// returns the absolute path unchanged; the lock file is created on
// first acquisition by the OS OpenFile call.
//
// This is the SINGLE source of truth for the maintenance lock
// identity. Every caller (DatabaseManager constructor, restore-db,
// shred-database) MUST derive its lock path through this function —
// diverging spells (e.g. /workspace vs /real/workspace) would defeat
// the protocol. MPM_DB_LOCK remains a per-test backstop but is not a
// production identity mechanism.
func MaintenanceLockPath(workspace string) string {
	return maintenanceLockPathIn(workspace)
}

// ActiveDBPath returns the canonical database file path for the
// active MPM database rooted at workspace. Symlink resolution
// mirrors MaintenanceLockPath so the lock path and the DB path stay
// in lock-step.
func ActiveDBPath(workspace string) string {
	abs, err := filepath.Abs(workspace)
	if err != nil || abs == "" {
		abs = workspace
	}
	resolved := abs
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil && r != "" {
		resolved = r
	}
	return filepath.Join(resolved, "src", "db", dbFileName)
}

// maintenanceLockPathIn is the workspace-root-parameterised
// implementation of MaintenanceLockPath. Kept as a separate helper
// so test code can pass a temp workspace without going through
// filepath.Abs's cwd resolution (which would otherwise re-canonicalise
// the path).
func maintenanceLockPathIn(workspace string) string {
	abs, err := filepath.Abs(workspace)
	if err != nil || abs == "" {
		abs = workspace
	}
	resolved := abs
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil && r != "" {
		resolved = r
	}
	return filepath.Join(resolved, "src", "db", dbFileName) + ".lock"
}

// AcquireSharedMaintenanceLock acquires LOCK_SH on the maintenance
// lock for the active MPM database rooted at workspace. The returned
// *os.File's FD holds the shared flock until Close is called (kernel
// releases on FD close / process death).
//
// Multiple FDs (in the same or different processes) may hold LOCK_SH
// concurrently. The acquire BLOCKS if any other process holds LOCK_EX
// — the cross-process "destructive window in flight" signal.
//
// Fail loud: a shared-lock failure surfaces as a constructor error so
// callers cannot accidentally produce an unprotected active writer.
func AcquireSharedMaintenanceLock(workspace string) (*os.File, error) {
	lockPath := MaintenanceLockPath(workspace)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("acquire shared maintenance lock: mkdir %s: %w", filepath.Dir(lockPath), err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire shared maintenance lock: open %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire shared maintenance lock: flock %s: %w", lockPath, err)
	}
	return f, nil
}

// AcquireExclusiveMaintenanceLock acquires LOCK_EX|LOCK_NB on the
// maintenance lock for the active MPM database rooted at workspace.
// The returned *os.File's FD holds the exclusive flock until Close is
// called (kernel releases on FD close / process death).
//
// Non-blocking: the acquire FAILS IMMEDIATELY with EWOULDBLOCK if any
// process holds LOCK_SH or LOCK_EX on the same inode. The caller
// treats the failure as "another active writer holds the lease" and
// refuses the destructive operation BEFORE any filesystem mutation.
// Destructive callers (restore-db, shred-database) hold the lock
// through the entire destructive window (the cross-process
// serialisation is the entire point — releasing the lock between
// steps would re-open the race).
//
// This function complements AcquireSharedMaintenanceLock. Together
// they implement the H-4 maintenance lease protocol described at the
// top of this file.
func AcquireExclusiveMaintenanceLock(workspace string) (*os.File, error) {
	lockPath := MaintenanceLockPath(workspace)
	return acquireExclusiveMaintenanceLockAt(lockPath)
}

// AcquireExclusiveMaintenanceLockAt is AcquireExclusiveMaintenanceLock
// against an explicit lock path. Used by callers that pin the lock
// location (e.g. tests via MPM_DB_LOCK; matches the shared variant's
// pattern at AcquireSharedMaintenanceLockAt).
func AcquireExclusiveMaintenanceLockAt(lockPath string) (*os.File, error) {
	return acquireExclusiveMaintenanceLockAt(lockPath)
}

// acquireExclusiveMaintenanceLockAt is the shared implementation.
// Extracted so tests can pin the lock location without going through
// MaintenanceLockPath's workspace-root parameterisation.
func acquireExclusiveMaintenanceLockAt(lockPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("acquire exclusive maintenance lock: mkdir %s: %w", filepath.Dir(lockPath), err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire exclusive maintenance lock: open %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire exclusive maintenance lock: flock %s: %w", lockPath, err)
	}
	return f, nil
}

// AcquireSharedMaintenanceLockAt is AcquireSharedMaintenanceLock
// against an explicit lock path. Used by tests that pin the lock
// location via MPM_DB_LOCK (mirrors MPM_CASCADE_LOCK's pattern).
func AcquireSharedMaintenanceLockAt(lockPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("acquire shared maintenance lock: mkdir %s: %w", filepath.Dir(lockPath), err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire shared maintenance lock: open %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire shared maintenance lock: flock %s: %w", lockPath, err)
	}
	return f, nil
}

// ErrMaintenanceLockAbsent is returned by the read-only maintenance lock
// primitive when the lock file does not exist. The read-only doctor
// cannot claim coordination with destructive operations without the
// H-4 lock file, so this is a fail-closed signal: the caller reports
// the absence to the operator and refuses the read.
//
// The error message names the exact remediation ("run a write command
// to establish the lock file") so the operator's failure-mode is
// self-documenting. The doctor does NOT silently bypass the lock or
// open the database without flock.
var ErrMaintenanceLockAbsent = fmt.Errorf(
	"maintenance lock file is absent, refusing to coordinate with destructive operations " +
		"because we cannot verify the absence of writers. Run a write command such as `mpm remember` " +
		"to establish the lock file, then re-run.",
)

// ErrCoordinatedInspectionUnavailable is returned by the read-only
// doctor's entry point when coordinated inspection is unavailable —
// currently only when the maintenance lock file is missing. The
// doctor surfaces this as a top-level "Database" check FAIL with a
// remediation hint, and exits with the existing missing-database exit
// code (preserved per the brief's "preserve existing exit-code
// semantics where applicable" rule).
var ErrCoordinatedInspectionUnavailable = fmt.Errorf(
	"coordinated inspection unavailable: %w",
	ErrMaintenanceLockAbsent,
)

// AcquireSharedMaintenanceLockExistingOnly opens the maintenance lock
// file O_RDONLY (no O_CREATE) and acquires LOCK_SH on it. Unlike
// AcquireSharedMaintenanceLock, this primitive:
//
//   - Does NOT call os.MkdirAll. The lock file is treated as
//     pre-existing state managed by the writable constructor; the
//     read-only doctor must not create new files in the database
//     directory (the no-filesystem-mutation contract for non-fix
//     doctor invocations).
//   - Opens the file with O_RDONLY (no O_CREATE). If the file does
//     not exist, the call returns ErrMaintenanceLockAbsent
//     immediately. The doctor cannot fall back to a no-flock read;
//     the design rejects that as a silent bypass of H-4.
//   - Issues LOCK_SH on success. As with the writable variant, the
//     kernel still provides cross-process coordination: a destructive
//     LOCK_EX|LOCK_NB attempt on the same inode will see EWOULDBLOCK
//     while the read-only doctor holds LOCK_SH.
//
// The returned *os.File's FD holds LOCK_SH until Close is called.
// Callers should defer the release to bound the lease window to the
// shortest useful inspection span.
//
// The lock file's inode identity is the coordination primitive; the
// file's content is irrelevant (flock locks the open file description,
// not the path or the data). Because the writable constructor creates
// the lock file when it opens the database, the read-only doctor can
// rely on the lock file's existence iff a writable DatabaseManager
// has previously been opened against this workspace. A fresh install
// that has never run a write command will see ErrMaintenanceLockAbsent
// — that is the correct, fail-closed behaviour.
func AcquireSharedMaintenanceLockExistingOnly(lockPath string) (*os.File, error) {
	// Fail closed: absent lock file is not a recoverable condition
	// for the doctor. The doctor cannot claim coordination with
	// destructive operations without the H-4 lock; opening the
	// database without flock would let a concurrent restore-db or
	// shred-database race the read.
	//
	// os.Stat is cheaper than os.OpenFile+IsNotExist and the only
	// thing the doctor needs to know up front is whether the
	// coordination primitive is present.
	if _, err := os.Stat(lockPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMaintenanceLockAbsent
		}
		return nil, fmt.Errorf("acquire shared maintenance lock (read-only): stat %s: %w", lockPath, err)
	}

	f, err := os.OpenFile(lockPath, os.O_RDONLY, 0)
	if err != nil {
		// The file existed at Stat time but disappeared (or became
		// unreadable) before we could open it. Treat that as the
		// same fail-closed condition: the coordination primitive is
		// not safely usable.
		if os.IsNotExist(err) {
			return nil, ErrMaintenanceLockAbsent
		}
		return nil, fmt.Errorf("acquire shared maintenance lock (read-only): open %s: %w", lockPath, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire shared maintenance lock (read-only): flock %s: %w", lockPath, err)
	}
	return f, nil
}

// ReleaseSharedMaintenanceLock closes the LOCK_SH FD. Safe to call
// with a nil file (no-op). The kernel auto-releases on FD close /
// process death, so callers may also rely on finalizers; the explicit
// close keeps the release deterministic for tests.
func ReleaseSharedMaintenanceLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}

// ReleaseExclusiveMaintenanceLock closes the LOCK_EX FD. Safe to call
// with a nil file (no-op). Same kernel-release semantics as the shared
// variant.
func ReleaseExclusiveMaintenanceLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}

// ---------------------------------------------------------------------------
// Schema-initialisation lock
// ---------------------------------------------------------------------------
//
// Separate from the H-4 maintenance lease, on a separate file: it serialises
// the boot migration transaction across processes that open the same fresh or
// just-upgraded database at the same time. The lease guards destructive
// operations against live DatabaseManagers; this lock guards the one-time
// schema transition. Sharing the lease file would conflate the two protocols
// (every opener already holds LOCK_SH, so an exclusive acquisition on the same
// inode could never succeed from a peer).

// schemaInitLockTimeout bounds the wait for a peer mid-migration. Schema
// initialisation is short; a longer wait means something is genuinely stuck
// and the caller should see an error rather than hang forever.
const schemaInitLockTimeout = 30 * time.Second

// SchemaInitLockPath returns the lock file used to serialise schema
// initialisation for the database at dbPath. It lives beside the database and
// is derived from the resolved dbPath so every opener of the same file
// converges on the same identity.
func SchemaInitLockPath(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	return dbPath + ".schema-init.lock"
}

// AcquireSchemaInitLock takes the exclusive schema-init lock, waiting up to
// schemaInitLockTimeout for a peer. Returns the locked file; the caller MUST
// pass it to ReleaseSchemaInitLock. In-memory databases (test fixtures) have
// no cross-process peers and return (nil, nil).
func AcquireSchemaInitLock(dbPath string) (*os.File, error) {
	lockPath := SchemaInitLockPath(dbPath)
	if lockPath == "" || IsInMemoryDSN(dbPath) {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("schema-init lock: mkdir %s: %w", filepath.Dir(lockPath), err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("schema-init lock: open %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(schemaInitLockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = f.Close()
			return nil, fmt.Errorf("schema-init lock: flock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("schema-init lock: %s held for more than %s; another process may be stuck", lockPath, schemaInitLockTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f, nil
}

// ReleaseSchemaInitLock releases and closes the schema-init lock file.
func ReleaseSchemaInitLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
