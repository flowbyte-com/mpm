package update

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------
// The fixture root lifecycle
// ---------------------------------------------------------------------
//
// WHY THIS FILE EXISTS
//
// The development-source fixture creates a scratch root, builds a
// checkout and a substrate binary in it, and shares them between the
// tests that need them. Nothing removed it: the root outlived the test
// binary, and a CI host that ran the suite repeatedly accumulated
// scratch directories until /tmp was full.
//
// The rules the fixture now follows:
//
//   - a root is removed after the test PROCESS finishes — after every
//     test, so a shared fixture is never pulled from under a test that
//     still needs it;
//   - a root is removed on the failure path too: a partially prepared
//     fixture is exactly the scratch that accumulates;
//   - cleanup removes ONLY the roots this process created, and it
//     proves ownership before removing anything. A directory that
//     merely resembles an MPM temporary directory is never removed.
//
// The ownership proof is a sentinel file the fixture writes when it
// creates the root. Cleanup refuses to remove a directory without it.

// fixtureRootSentinel is the marker that proves a directory belongs to
// this test process.
//
// The name is deliberately specific: a cleanup that swept /tmp for
// anything resembling an MPM temporary directory would eventually
// delete an operator's directory. Cleanup removes a path it created,
// and can prove it.
const fixtureRootSentinel = ".mpm-update-fixture-root"

// fixtureRootPrefix is the prefix the fixture's roots are created with.
//
// It is a SECOND condition, not the only one: the sentinel is the
// proof, the prefix is a cheap sanity check.
const fixtureRootPrefix = "mpm-devsource-"

// fixtureRoots tracks the roots this process created.
var fixtureRoots = struct {
	mu    sync.Mutex
	paths []string
}{}

// registerFixtureRoot records a root for cleanup.
func registerFixtureRoot(path string) {
	fixtureRoots.mu.Lock()
	defer fixtureRoots.mu.Unlock()
	fixtureRoots.paths = append(fixtureRoots.paths, path)
}

// unregisterFixtureRoot forgets a root that was already removed.
func unregisterFixtureRoot(path string) {
	fixtureRoots.mu.Lock()
	defer fixtureRoots.mu.Unlock()
	kept := fixtureRoots.paths[:0]
	for _, p := range fixtureRoots.paths {
		if p != path {
			kept = append(kept, p)
		}
	}
	fixtureRoots.paths = kept
}

// registeredFixtureRootCount reports how many roots are registered for
// cleanup.
func registeredFixtureRootCount() int {
	fixtureRoots.mu.Lock()
	defer fixtureRoots.mu.Unlock()
	return len(fixtureRoots.paths)
}

// removeFixtureRoot removes one fixture root.
//
// It refuses to remove a directory it cannot prove it owns: the
// sentinel must be inside it. A path that merely LOOKS like an MPM
// temporary directory is never removed.
func removeFixtureRoot(path string) error {
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(abs, fixtureRootSentinel)); err != nil {
		return fmt.Errorf("refusing to remove %s: it does not carry the fixture sentinel, so this process did not create it", abs)
	}
	if base := filepath.Base(abs); len(base) < len(fixtureRootPrefix) || base[:len(fixtureRootPrefix)] != fixtureRootPrefix {
		return fmt.Errorf("refusing to remove %s: its name is not a fixture root name", abs)
	}
	if err := os.RemoveAll(abs); err != nil {
		return err
	}
	unregisterFixtureRoot(abs)
	return nil
}

// cleanupFixtureRoots removes every registered root.
//
// It is called once, after the last test has finished. Cleanup that
// ran per test would remove a SHARED fixture while another test still
// needed it — the fixture is shared precisely because the build it
// holds is expensive.
func cleanupFixtureRoots() {
	fixtureRoots.mu.Lock()
	paths := append([]string(nil), fixtureRoots.paths...)
	fixtureRoots.mu.Unlock()
	for _, p := range paths {
		// Best effort: a root that cannot be removed must not stop the
		// others from being removed, and must not fail the run that
		// just passed.
		_ = removeFixtureRoot(p)
	}
}

// newFixtureRoot creates a disposable root and registers it for
// cleanup.
//
// tmpDir is the directory the root is created in; the empty string
// means the host's default temporary directory. The root carries the
// sentinel that proves ownership.
func newFixtureRoot(tmpDir string) (string, error) {
	root, err := os.MkdirTemp(tmpDir, fixtureRootPrefix)
	if err != nil {
		return "", err
	}
	sentinel := filepath.Join(root, fixtureRootSentinel)
	if err := os.WriteFile(sentinel, []byte("mpm update test fixture root\n"), 0644); err != nil {
		_ = os.RemoveAll(root)
		return "", err
	}
	registerFixtureRoot(root)
	return root, nil
}

// TestMain runs the package's tests and releases the fixture roots.
//
// The cleanup runs in a DEFERRED function inside the closure that
// calls m.Run(), so it happens on the normal path AND while a panic
// unwinds: a crashed run must not leave scratch behind either.
//
// It deliberately does not call os.Exit before the cleanup: os.Exit
// skips deferred functions, so the cleanup would never run.
func TestMain(m *testing.M) {
	exitCode := 0
	func() {
		defer cleanupFixtureRoots()
		exitCode = m.Run()
	}()
	os.Exit(exitCode)
}
