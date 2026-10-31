package update

import (
	"fmt"
	"os"
	"syscall"
)

// UpdateLock serialises concurrent updaters.
//
// The lock is a file lock on <data root>/.mpm-update.lock, held for the
// whole promotion. A second updater that finds the lock held is refused
// with a clear message — it must not queue behind the first, because an
// update that runs against a data root another update is mid-swap
// through would validate a candidate against a moving target.
//
// The lock file is separate from install.sh's .mpm-install.lock and
// .mpm-bundle.lock. Sharing a lock file with the installer would couple
// the two tools' failure modes; the updater must be usable exactly when
// the rest of the installation is in an unknown state.
type UpdateLock struct {
	f *os.File
}

// AcquireUpdateLock opens the lock file (creating the data root with
// 0700 when necessary) and takes an exclusive, non-blocking lock.
// Callers must call Release. A held lock produces an
// *UpdateLockedError, which callers report without retrying.
func AcquireUpdateLock(dataRoot string) (*UpdateLock, error) {
	if err := os.MkdirAll(dataRoot, 0700); err != nil {
		return nil, fmt.Errorf("could not prepare the data root: %w", err)
	}
	path := NewLayout(dataRoot).LockFile()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("could not open the update lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, &UpdateLockedError{Path: path}
	}
	return &UpdateLock{f: f}, nil
}

// UpdateLockedError means another updater holds the lock.
type UpdateLockedError struct {
	Path string
}

func (e *UpdateLockedError) Error() string {
	return fmt.Sprintf("another update holds the lock %s; refusing to run concurrently", e.Path)
}

// Release drops the lock. It never fails loudly: at release time the
// work is done, and a failed unlock must not turn success into failure.
func (l *UpdateLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	l.f.Close()
}
