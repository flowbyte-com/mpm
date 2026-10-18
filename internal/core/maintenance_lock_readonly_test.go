// maintenance_lock_readonly_test.go — focused tests for the read-only
// maintenance lock primitive (AcquireSharedMaintenanceLockExistingOnly).
//
// These are Stage 1 of the MPM #28 staged verification. The primitive
// is the new fail-closed acquisition that the read-only doctor uses:
// it MUST NOT call os.MkdirAll, MUST NOT create the lock file, MUST
// refuse on absent lock, and MUST hold LOCK_SH (so a concurrent
// destructive LOCK_EX|LOCK_NB is refused with EWOULDBLOCK).
//
// Per the brief's H-4 contract: "an absent maintenance lock must fail
// without creating files, and a held shared lock must prevent a
// concurrent destructive operation." These two properties are pinned
// here. The F8 SQLite verification (which exercises ?mode=ro against
// a real DB) is a separate gate; this file pins the kernel-level
// flock coordination and the file-creation contract.
package internal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAcquireSharedMaintenanceLockExistingOnly_NoCreateOnMissing
// pins the fail-closed, no-filesystem-mutation contract for the
// doctor. The lock file does not exist; the primitive must return
// ErrMaintenanceLockAbsent (NOT a wrapped os.PathError, NOT a nil
// error, NOT any kind of created file). After the call, the parent
// directory must remain empty (or whatever state the test set up).
func TestAcquireSharedMaintenanceLockExistingOnly_NoCreateOnMissing(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "src", "db", "mpm.db.lock")

	// Pre-condition: parent dir does not exist; lock file does not
	// exist. The primitive must not bring either into existence.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: lock file must not exist; got stat err=%v", err)
	}

	f, err := AcquireSharedMaintenanceLockExistingOnly(lockPath)
	if f != nil {
		_ = f.Close()
		t.Fatalf("expected nil file on absent lock, got non-nil")
	}
	if !errors.Is(err, ErrMaintenanceLockAbsent) {
		t.Fatalf("expected ErrMaintenanceLockAbsent, got %v", err)
	}

	// Post-condition: nothing was created.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("primitive created the lock file: stat err=%v", err)
	}
	parentDir := filepath.Dir(lockPath)
	if _, err := os.Stat(parentDir); !os.IsNotExist(err) {
		t.Errorf("primitive created the parent directory: stat err=%v", err)
	}
}

// TestAcquireSharedMaintenanceLockExistingOnly_BlocksExclusive is
// the H-4 acceptance: a held LOCK_SH on the same inode refuses a
// concurrent LOCK_EX|LOCK_NB with EWOULDBLOCK.
//
// The lock file is pre-created (the writable constructor would have
// created it; here we create it explicitly so the test does not
// depend on NewDatabaseManager's behaviour).
func TestAcquireSharedMaintenanceLockExistingOnly_BlocksExclusive(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "src", "db"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(tmp, "src", "db", "mpm.db.lock")
	if err := os.WriteFile(lockPath, []byte{}, 0o600); err != nil {
		t.Fatalf("create lock file: %v", err)
	}

	// Acquire the read-only shared lock.
	shFD, err := AcquireSharedMaintenanceLockExistingOnly(lockPath)
	if err != nil {
		t.Fatalf("read-only shared acquire: %v", err)
	}
	t.Cleanup(func() { _ = ReleaseSharedMaintenanceLock(shFD) })

	// Try the destructive (exclusive, non-blocking) acquisition on
	// the same inode. Must fail with EWOULDBLOCK.
	exFD, err := acquireExclusiveMaintenanceLockAt(lockPath)
	if exFD != nil {
		_ = exFD.Close()
		t.Fatal("exclusive acquire succeeded while read-only shared was held — lease is broken")
	}
	if !isEWOULDBLOCK(err) {
		t.Fatalf("exclusive acquire failed but not with EWOULDBLOCK; got %v", err)
	}

	// After releasing the shared lock, exclusive succeeds. Proves
	// the previous failure was the lease, not a stale FD.
	if err := ReleaseSharedMaintenanceLock(shFD); err != nil {
		t.Fatalf("release shared: %v", err)
	}
	exFD, err = acquireExclusiveMaintenanceLockAt(lockPath)
	if err != nil {
		t.Fatalf("exclusive acquire after shared release: %v", err)
	}
	if err := ReleaseExclusiveMaintenanceLock(exFD); err != nil {
		t.Fatalf("release exclusive: %v", err)
	}
}

// TestAcquireSharedMaintenanceLockExistingOnly_DoesNotCreateParent
// pins the second part of the no-filesystem-mutation contract: even
// if the parent directory and the lock file both exist, the
// primitive must not modify them. The lock file's content, mode and
// mtime are unchanged after the primitive opens and closes the file.
func TestAcquireSharedMaintenanceLockExistingOnly_DoesNotCreateParent(t *testing.T) {
	tmp := t.TempDir()
	lockDir := filepath.Join(tmp, "src", "db")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(lockDir, "mpm.db.lock")
	if err := os.WriteFile(lockPath, []byte("do-not-modify"), 0o600); err != nil {
		t.Fatalf("create lock file: %v", err)
	}

	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	beforeContent, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	f, err := AcquireSharedMaintenanceLockExistingOnly(lockPath)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := ReleaseSharedMaintenanceLock(f); err != nil {
		t.Fatalf("release: %v", err)
	}

	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	afterContent, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}

	if string(beforeContent) != string(afterContent) {
		t.Errorf("lock file content changed: before=%q after=%q", beforeContent, afterContent)
	}
	if before.Size() != after.Size() {
		t.Errorf("lock file size changed: before=%d after=%d", before.Size(), after.Size())
	}
	// mtime is best-effort: on some filesystems (tmpfs, ext4 with
	// relatime) the kernel may update atime but mtime is preserved
	// for a read-only open. We compare mtime specifically.
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("lock file mtime changed: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

// TestAcquireSharedMaintenanceLockExistingOnly_RefusesAfterRemoval
// pins the closed-FD-after-stat edge case: if the file existed at
// Stat time but cannot be opened (e.g. the parent directory was
// removed in the window), the primitive must NOT silently treat
// the open failure as ErrMaintenanceLockAbsent — that sentinel is
// for files that did not exist. A real open failure must surface
// as a wrapped error so the operator sees why the lock could not
// be acquired.
//
// The mechanism: chmod the parent directory to 0 so that the
// open is denied with EACCES. os.Stat still reports the file
// (the stat call does not require read permission on the parent
// once the path components have been resolved by the test setup
// step), but the subsequent open refuses.
//
// On a fresh tmp dir: write the lock file, then chmod the parent
// to 0. The kernel will stat the file by inode (since the test
// process owns it), but open on the path will fail.
func TestAcquireSharedMaintenanceLockExistingOnly_RefusesAfterRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 is bypassed by DAC_OVERRIDE; the " +
			"permission-denied path is not exercisable on this kernel")
	}
	tmp := t.TempDir()
	lockDir := filepath.Join(tmp, "src", "db")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(lockDir, "mpm.db.lock")
	if err := os.WriteFile(lockPath, []byte{}, 0o600); err != nil {
		t.Fatalf("create lock file: %v", err)
	}
	// Snapshot the file's inode state before denying access so we
	// can be sure the open path is the one being tested.
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("precondition: lock file must be statable: %v", err)
	}
	// Deny access to the parent directory. Subsequent open
	// attempts on lockPath will fail with EACCES.
	if err := os.Chmod(lockDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockDir, 0o700) })

	f, err := AcquireSharedMaintenanceLockExistingOnly(lockPath)
	if f != nil {
		_ = f.Close()
		t.Fatal("expected nil file when parent directory is unreadable")
	}
	if err == nil {
		t.Fatal("expected an error when the lock path is unopenable, got nil")
	}
	if errors.Is(err, ErrMaintenanceLockAbsent) {
		t.Fatalf("ErrMaintenanceLockAbsent is for absent files, not for unopenable paths; got %v", err)
	}
	// The error must be a wrapped open failure, not nil, not the
	// absent-file sentinel. The exact wrapping is platform-
	// dependent; we only assert it is non-nil and not the
	// absent-file sentinel.
}
