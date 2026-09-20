// session_persistence.go — cross-process-locked atomic read-or-create
// and rotation helpers for the mpm_session_id field on active.json.
//
// Why this file exists
// ────────────────────
// active.json is the AUTHORITATIVE source of truth for the current
// MPM lifecycle identity. Multiple processes (CLI invocations, MCP
// server, scheduler, drill harness) read and write it concurrently.
// Without locking, two processes starting simultaneously against an
// empty active state can each independently mint a different ID and
// clobber each other:
//
//   process A reads active.json (no mpm_session_id)
//   process B reads active.json (no mpm_session_id)
//   process A allocates mpm-A, writes
//   process B allocates mpm-B, writes (clobbers mpm-A)
//
// The fix is a cross-process advisory lock on a sibling file
// (`active.json.lock`) wrapping the entire read-modify-write cycle.
// Linux/macOS get this from syscall.Flock; flock is per-FD — a fresh
// FD always succeeds after the prior holder releases — so the locking
// primitive is the standard cross-process advisory lock used by every
// Unix daemon that coordinates via file locks.
//
// Atomic write is already provided by SaveActiveJSON (tmp + rename).
// Together: an outside observer either sees the old content or the
// new content, never a hybrid, and two concurrent acquirers
// serialize on the lock.

package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// flockLockPath is the sibling file used as the cross-process lock
// handle. Lives next to active.json; deleting it is unnecessary
// (flock is per-FD, not per-file).
func flockLockPath() string {
	return filepath.Join(config.GetMPMDir(), "active.json.lock")
}

// flockUnlock is the function returned by acquireFlock that releases
// the lock when called. Calling it more than once is a no-op
// (idempotent — closing an already-closed FD is a no-op on Linux).
type flockUnlock func()

// acquireFlock acquires an exclusive advisory lock on the active.json
// sibling lock file. Blocks if another process holds the lock.
// Returns an unlock function the caller MUST call when done.
//
// The lock file is created on first use if it does not exist; the
// file content is irrelevant (flock locks the inode, not the file
// data). Subsequent acquireFlock calls from the same process always
// succeed because each call opens a fresh FD — flock is per-FD.
func acquireFlock(lockPath string) (flockUnlock, error) {
	// Ensure the lock file exists. O_CREATE + O_RDWR opens for write
	// but does not require content.
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("acquireFlock: open %s: %w", lockPath, err)
	}

	// syscall.Flock is per-FD and blocking. On Linux/macOS this
	// blocks until the kernel grants the lock.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquireFlock: flock %s: %w", lockPath, err)
	}

	var released bool
	unlock := func() {
		if released {
			return
		}
		released = true
		// Release the flock before closing the FD; either order works
		// on Linux, but the unlock-then-close pattern matches the
		// canonical sysv-locks-on-tmpfiles example.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return unlock, nil
}

// withActiveJSONFlock acquires the active.json cross-process lock
// for the duration of fn. This is the canonical wrapper — every
// read-modify-write cycle on the mpm_session_id field MUST go
// through this helper to prevent split-brain between concurrent
// processes. The lock is released even if fn panics.
func withActiveJSONFlock(fn func() error) error {
	unlock, err := acquireFlock(flockLockPath())
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// loadOrAllocateMPMSessionIDLocked is the canonical first-use
// allocator. Caller MUST hold the active.json flock (via
// withActiveJSONFlock). Returns the active mpm_session_id from
// active.json; if absent, allocates a fresh one, writes it, and
// returns it.
//
// The ID format is "mpm-" + 32 hex chars (128 bits from
// crypto/rand). The "mpm-" prefix lets operators distinguish MPM-
// owned IDs from framework-supplied IDs at a glance.
func loadOrAllocateMPMSessionIDLocked() (string, error) {
	state, err := LoadActiveJSON()
	if err != nil {
		return "", fmt.Errorf("loadOrAllocateMPMSessionID: load: %w", err)
	}
	if state.MPMSessionID != "" {
		return state.MPMSessionID, nil
	}
	newID, err := generateNewMPMSessionID()
	if err != nil {
		return "", fmt.Errorf("loadOrAllocateMPMSessionID: generate: %w", err)
	}
	state.MPMSessionID = newID
	state.MPMSessionIDCreatedAt = time.Now().Unix()
	if err := SaveActiveJSON(state); err != nil {
		return "", fmt.Errorf("loadOrAllocateMPMSessionID: save: %w", err)
	}
	return newID, nil
}

// rotateMPMSessionIDLocked allocates a fresh mpm_session_id,
// persists it, and returns it. Caller MUST hold the active.json
// flock. The previous value (if any) is overwritten — there is no
// "keep old + start new" semantic. Explicit rotation is a fresh
// lifecycle boundary.
func rotateMPMSessionIDLocked() (string, error) {
	state, err := LoadActiveJSON()
	if err != nil {
		return "", fmt.Errorf("rotateMPMSessionID: load: %w", err)
	}
	newID, err := generateNewMPMSessionID()
	if err != nil {
		return "", fmt.Errorf("rotateMPMSessionID: generate: %w", err)
	}
	state.MPMSessionID = newID
	state.MPMSessionIDCreatedAt = time.Now().Unix()
	if err := SaveActiveJSON(state); err != nil {
		return "", fmt.Errorf("rotateMPMSessionID: save: %w", err)
	}
	return newID, nil
}
