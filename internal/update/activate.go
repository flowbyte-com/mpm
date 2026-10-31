package update

import (
	"fmt"
	"os"
	"path/filepath"
)

// installRelease moves a fully-assembled, fully-validated candidate into
// releases/<rev>.
//
// The move is a rename(2): the candidate is assembled under the data
// root's own staging directory, so it shares a filesystem with the
// release directory, and the release either does not exist or is
// complete — never half-populated.
//
// A release is immutable: if releases/<rev> already exists it is NOT
// replaced. When the existing release validates, the candidate is
// discarded and the existing release is used (the recovery case after a
// promotion that failed between install and activation). When it does
// not validate, the update is refused — overwriting it in place would
// silently change what a rollback to that label gives back.
func installRelease(dataRoot, candidateDir, rev string) (installed bool, err error) {
	l := NewLayout(dataRoot)
	final := l.ReleaseDir(rev)
	if _, serr := os.Stat(candidateDir); serr != nil {
		return false, fmt.Errorf("candidate bundle does not exist: %s", candidateDir)
	}
	if err := os.MkdirAll(l.Releases(), 0755); err != nil {
		return false, fmt.Errorf("cannot create %s: %w", l.Releases(), err)
	}
	if fi, err := os.Stat(final); err == nil && fi.IsDir() {
		rep := ValidateBundle(final, rev)
		if rep.Valid {
			return false, nil
		}
		return false, fmt.Errorf("release %s already exists at %s and does not validate; a release is immutable, so it will not be replaced", rev, final)
	}
	if err := os.Rename(candidateDir, final); err != nil {
		return false, fmt.Errorf("could not install the bundle into %s: %w", final, err)
	}
	return true, nil
}

// activateRelease repoints `current` at releases/<rev>.
//
// The swap is a rename(2) of a freshly-created symlink over `current`,
// created in the SAME directory — so a concurrent reader sees either
// the old revision or the new one, never a gap and never a mixture.
// This is the same atomicity install.sh's rb_activate provides, via the
// same operation.
func activateRelease(dataRoot, rev string) (prevTarget string, err error) {
	l := NewLayout(dataRoot)
	if fi, serr := os.Stat(l.ReleaseDir(rev)); serr != nil || !fi.IsDir() {
		return "", fmt.Errorf("cannot activate %q: no such release", rev)
	}
	prevTarget, _, _ = CurrentReleaseTarget(dataRoot)
	tmp := filepath.Join(dataRoot, ".current.swap.update")
	os.Remove(tmp)
	if err := os.Symlink("releases/"+rev, tmp); err != nil {
		return "", fmt.Errorf("could not create the replacement pointer: %w", err)
	}
	if err := os.Rename(tmp, l.Current()); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("could not swap the active pointer: %w", err)
	}
	if prevTarget != "" {
		_ = RecordPreviousRelease(dataRoot, prevTarget)
	}
	return prevTarget, nil
}

// restorePointer points `current` back at a previous target
// ("releases/<rev>"). It is the recovery path when activation
// succeeded but a later step did not.
func restorePointer(dataRoot, target string) error {
	l := NewLayout(dataRoot)
	if target == "" {
		return os.Remove(l.Current())
	}
	tmp := filepath.Join(dataRoot, ".current.swap.update")
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.Current()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// installStableUpdater copies the promoted release's mpm-update to the
// stable top-level path (<data root>/mpm-update, a REAL file).
//
// This is what makes the NEXT recovery independent of the release it
// recovers: the stable copy is state, so version switches cannot reach
// it, and running it never executes the active bundle. It is refreshed
// on every successful activation, so it tracks the newest KNOWN-GOOD
// runtime rather than rotting at whatever revision first installed it.
func installStableUpdater(dataRoot, rev string) error {
	src := filepath.Join(NewLayout(dataRoot).ReleaseDir(rev), "bin", UpdaterBinaryName)
	raw, err := os.ReadFile(src)
	if err != nil {
		// A release built before the updater existed has nothing to
		// install. Reported, not refused: the bundle already validated.
		return nil
	}
	dst := NewLayout(dataRoot).StableUpdater()
	tmp, err := os.CreateTemp(dataRoot, ".mpm-update.*")
	if err != nil {
		return fmt.Errorf("could not stage the stable updater: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0755); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, dst)
}

// LocateStableUpdater finds the updater binary to exec: the stable copy
// when it exists (independent of the active release), else the active
// bundle's copy, else "".
//
// mpm update delegates to whatever this returns. Direct execution of
// the stable copy is the recovery path when the active runtime is
// broken.
func LocateStableUpdater(dataRoot string) string {
	l := NewLayout(dataRoot)
	if fi, err := os.Stat(l.StableUpdater()); err == nil && !fi.IsDir() && fi.Mode().Perm()&0111 != 0 {
		return l.StableUpdater()
	}
	if _, rev, err := CurrentReleaseTarget(dataRoot); err == nil && rev != "" {
		p := filepath.Join(l.ReleaseDir(rev), "bin", UpdaterBinaryName)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode().Perm()&0111 != 0 {
			return p
		}
	}
	return ""
}
