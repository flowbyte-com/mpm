package internal

// workspace_state_isolation_test.go — hermeticity of the workspace-scoped
// runtime state that lives OUTSIDE the database.
//
// # The defect
//
// Three artifacts of the runtime root are reachable from ordinary
// operation and were being written into the operator's real ~/.mpm by
// every test that built a hermetic manager:
//
//	active.json       — the workspace's lifecycle identity
//	active.json.lock  — its cross-process flock handle
//	toxicphrases.txt  — the workspace's phrase list, generated on first
//	                    use by the memory security scanner
//
// active.json and its lock were reached through a *DatabaseManager
// method. EndSessionV2 is the interaction boundary that allocates
// mpm_session_id, and it called the package-level
// AcquireMPMSessionID(), which resolves its workspace from the AMBIENT
// environment (config.GetMPMDir()). A manager wrapping an in-memory test
// database therefore minted a session id inside — and took an exclusive
// flock on — the operator's real files. Isolating the database was not
// enough, and pinning MPM_WORKSPACE in the test helper would only have
// masked the ownership defect rather than fixed it.
//
// toxicphrases.txt was reached through the memory security scanner,
// which resolves its file from the same ambient workspace and creates
// it (with the 20 hardcoded default phrases) if absent. It was also
// cached behind an unkeyed sync.Once, which was a second, separate
// cross-workspace coupling: whichever workspace lost the race set the
// phrases for every other one in the process.
//
// # The invariant these tests pin
//
// A DatabaseManager's workspace-scoped state follows the manager's own
// database. A manager with no file-backed database owns no workspace and
// must not reach for the ambient one. Ambient reads (the CLI, the
// scheduler) still resolve the ambient workspace exactly as before —
// this is an isolation fix, not a product-layout change.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// workspaceStateArtifacts are the runtime-root files whose creation
// outside a test's own tree this package must prevent.
var workspaceStateArtifacts = []string{
	"active.json",
	"active.json.lock",
	"toxicphrases.txt",
}

// sentinelAmbientWorkspace installs a throwaway HOME containing a
// complete ~/.mpm tree, unpins MPM_WORKSPACE, and separately pins
// MPM_WORKSPACE to a second throwaway directory.
//
// Two roots, deliberately distinct, is what makes an escape OBSERVABLE.
// A single pinned MPM_WORKSPACE would also redirect the ambient path, so
// a leak that ignored the pin entirely would still be caught — but a
// leak that merely hopped from the operator's home to the pinned
// directory would not. Keeping the sentinels separate means each guard
// below has to name which root it expected a file in.
//
// Returns the ambient workspace (sentinel home's .mpm) and the pinned
// workspace.
func sentinelAmbientWorkspace(t *testing.T) (ambient, pinned string) {
	t.Helper()
	home := t.TempDir()
	// Pre-create the full src/db tree. With a pristine home the first
	// write of active.json would fail on the missing parent and be
	// swallowed by the scanner's non-fatal error path, which would make
	// a leak-injection check indistinguishable from a pass.
	if err := os.MkdirAll(filepath.Join(home, ".mpm", "src", "db"), 0700); err != nil {
		t.Fatalf("mkdir sentinel install: %v", err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("MPM_WORKSPACE")
	ambient = filepath.Join(home, ".mpm")

	pinned = t.TempDir()
	t.Setenv("MPM_WORKSPACE", pinned)
	return ambient, pinned
}

// assertAbsent fails if any of the named artifacts exists under root.
func assertAbsent(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		p := filepath.Join(root, name)
		if st, err := os.Stat(p); err == nil {
			t.Errorf("ESCAPED WRITE: %s was created at %s (%d bytes); "+
				"it belongs to the isolated workspace, not to %s", name, p, st.Size(), root)
		} else if !os.IsNotExist(err) {
			t.Errorf("unexpected error stat %s: %v", p, err)
		}
	}
}

// assertNoDatabaseOrLogs fails if root gained a database or an auxiliary
// log — the pre-21c3d9b leak shape, kept here so a regression in either
// direction is caught by the same guard.
func assertNoDatabaseOrLogs(t *testing.T, root string) {
	t.Helper()
	assertAbsent(t, root, workspaceStateArtifacts...)
	for _, name := range []string{"mpm.db", "mirror.jsonl", "watchdog.jsonl"} {
		p := filepath.Join(root, "src", "db", name)
		if st, err := os.Stat(p); err == nil {
			t.Errorf("ESCAPED WRITE: %s was created at %s (%d bytes)", name, p, st.Size())
		} else if !os.IsNotExist(err) {
			t.Errorf("unexpected error stat %s: %v", p, err)
		}
	}
}

// TestWorkspaceState_InMemoryManagerTouchesNoAmbientFile is the core
// regression. A hermetic in-memory manager must write nothing at all to
// either sentinel root, and must still produce a usable session id and a
// working handoff row.
func TestWorkspaceState_InMemoryManagerTouchesNoAmbientFile(t *testing.T) {
	ambient, pinned := sentinelAmbientWorkspace(t)

	dm := NewTestDM(t)
	// NewTestDM sees MPM_WORKSPACE already set (the pin above) and
	// leaves it — so if the manager reached for ambient state, the
	// pinned root is where it would land, and the sentinel home is what
	// catches a pin-ignoring path.

	h, err := dm.EndSessionV2(
		"", "framework-s1", "",
		"hermetic handoff", "clean",
		[]string{"commit-1"}, []string{"open-1"},
	)
	if err != nil {
		t.Fatalf("EndSessionV2: %v", err)
	}
	if h.MPMSessionID == "" {
		t.Fatal("handoff carried no mpm_session_id; the manager must still mint one, just not into ambient state")
	}

	// A memory save runs the poison scanner, which is the toxicphrases
	// writer. Content deliberately carries no phrase, so the save
	// succeeds and the scan still runs.
	if _, err := dm.SaveMemory("default", "an ordinary sentence about weather", "", nil, nil, nil, false, 1); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	assertNoDatabaseOrLogs(t, ambient)
	// The pinned workspace is where ambient resolution points, so the
	// generated phrase file belongs there — production initialization is
	// unchanged. What must NOT appear is lifecycle state: an in-memory
	// manager owns no workspace, so it has no business minting a session
	// id (or taking a lock) in the ambient one, even a test-pinned one.
	assertAbsent(t, pinned, "active.json", "active.json.lock")
	if _, err := os.Stat(filepath.Join(pinned, "toxicphrases.txt")); err != nil {
		t.Errorf("toxicphrases.txt was not generated under the resolved workspace %s: %v", pinned, err)
	}
}

// TestWorkspaceState_FileBackedManagerUsesItsOwnWorkspace proves the fix
// is an ownership change and not merely suppression: a manager rooted at
// a workspace of its own writes its lifecycle state THERE, even while
// the ambient environment points somewhere else entirely.
func TestWorkspaceState_FileBackedManagerUsesItsOwnWorkspace(t *testing.T) {
	ambient, _ := sentinelAmbientWorkspace(t)

	own := t.TempDir()
	dm, err := NewDatabaseManager(own)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", own, err)
	}
	t.Cleanup(func() { _ = dm.Close() })

	h, err := dm.EndSessionV2(
		"", "framework-s2", "",
		"own-workspace handoff", "clean",
		[]string{"commit-2"}, []string{"open-2"},
	)
	if err != nil {
		t.Fatalf("EndSessionV2: %v", err)
	}
	if h.MPMSessionID == "" {
		t.Fatal("handoff carried no mpm_session_id")
	}

	for _, name := range []string{"active.json", "active.json.lock"} {
		if _, err := os.Stat(filepath.Join(own, name)); err != nil {
			t.Errorf("manager did not create %s in its own workspace %s: %v", name, own, err)
		}
	}
	assertNoDatabaseOrLogs(t, ambient)
}

// TestWorkspaceState_PoisonPhrasesLandInThePinnedWorkspace pins the
// toxicphrases half of the contract. The file is still GENERATED on
// first use — production initialization is unchanged — it just lands
// under the workspace the operation actually resolved.
func TestWorkspaceState_PoisonPhrasesLandInThePinnedWorkspace(t *testing.T) {
	ambient, pinned := sentinelAmbientWorkspace(t)

	dm := NewTestDM(t)
	if _, err := dm.SaveMemory("default", "another ordinary sentence", "", nil, nil, nil, false, 1); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	phrases, err := loadPoisonPhrases()
	if err != nil {
		t.Fatalf("loadPoisonPhrases: %v", err)
	}
	if len(phrases) == 0 {
		t.Fatal("poison phrase list is empty; the default seed did not run")
	}
	if _, err := os.Stat(filepath.Join(pinned, "toxicphrases.txt")); err != nil {
		t.Errorf("toxicphrases.txt was not generated under the resolved workspace %s: %v", pinned, err)
	}
	assertAbsent(t, ambient, workspaceStateArtifacts...)
}

// TestWorkspaceState_PoisonPhraseCacheIsKeyedByPath pins the second
// defect: the phrase list was cached behind an unkeyed sync.Once, so the
// first workspace to ask in a process decided the phrases for every
// other workspace in that process. With a keyed cache, a second
// workspace reads its own file.
func TestWorkspaceState_PoisonPhraseCacheIsKeyedByPath(t *testing.T) {
	_, _ = sentinelAmbientWorkspace(t)

	firstPhrases, err := loadPoisonPhrases()
	if err != nil {
		t.Fatalf("loadPoisonPhrases (first workspace): %v", err)
	}
	if len(firstPhrases) == 0 {
		t.Fatal("first workspace produced no phrases")
	}

	// A second, different workspace in the same process.
	second := t.TempDir()
	t.Setenv("MPM_WORKSPACE", second)

	secondPhrases, err := loadPoisonPhrases()
	if err != nil {
		t.Fatalf("loadPoisonPhrases (second workspace): %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "toxicphrases.txt")); err != nil {
		t.Errorf("second workspace did not get its own toxicphrases.txt: %v", err)
	}
	if len(firstPhrases) != len(secondPhrases) {
		t.Errorf("phrase counts differ across workspaces: first=%d second=%d; "+
			"the cache is serving one workspace's data to another",
			len(firstPhrases), len(secondPhrases))
	}
}

// TestWorkspaceState_ScannerGeneratesPhrasesInTheManagersWorkspace
// pins the phrase list's ownership. This one was found by the
// all-module sentinel sweep rather than by reasoning: NewTestDM's
// workspace pin masked it for every test using that helper, and the
// only remaining writer was a test that builds its own file-backed
// manager with NewDatabaseManager(t.TempDir()) — a manager with a
// perfectly good workspace of its own, whose memory save generated the
// phrase list in whatever MPM_WORKSPACE happened to say.
func TestWorkspaceState_ScannerGeneratesPhrasesInTheManagersWorkspace(t *testing.T) {
	ambient, pinned := sentinelAmbientWorkspace(t)

	own := t.TempDir()
	dm, err := NewDatabaseManager(own)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", own, err)
	}
	t.Cleanup(func() { _ = dm.Close() })

	if _, err := dm.SaveMemory("default", "a sentence with nothing suspicious", "", nil, nil, nil, false, 1); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	if _, err := os.Stat(filepath.Join(own, "toxicphrases.txt")); err != nil {
		t.Errorf("phrase list was not generated in the manager's own workspace %s: %v", own, err)
	}
	assertNoDatabaseOrLogs(t, ambient)
	assertAbsent(t, pinned, workspaceStateArtifacts...)
}

// # Production parity
//
// The fixes above change WHO OWNS a file, never WHERE it lives. These
// tests run with MPM_WORKSPACE unset and a sentinel HOME, so the
// canonical ~/.mpm layout is what gets resolved — if any of the
// ownership work had leaked into the path resolution, these would fail.

// TestProductionParity_AmbientPathsUnchanged pins the canonical layout.
func TestProductionParity_AmbientPathsUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv("MPM_WORKSPACE")

	if err := os.MkdirAll(filepath.Join(home, ".mpm"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	want := filepath.Join(home, ".mpm")
	if got := ActiveJSONPath(); got != filepath.Join(want, "active.json") {
		t.Errorf("ActiveJSONPath() = %q, want %q", got, filepath.Join(want, "active.json"))
	}
	if got := flockLockPath(); got != filepath.Join(want, "active.json.lock") {
		t.Errorf("flockLockPath() = %q, want %q", got, filepath.Join(want, "active.json.lock"))
	}
	if got := config.GetToxicPhrasesPath(); got != filepath.Join(want, "toxicphrases.txt") {
		t.Errorf("toxic phrases path = %q, want %q", got, filepath.Join(want, "toxicphrases.txt"))
	}
}

// TestProductionParity_AcquireWritesTheAmbientActiveJSON proves the
// package-level allocator — the one the CLI and the scheduler use —
// still reads and writes ~/.mpm/active.json under its cross-process
// lock, and that a second acquisition returns the SAME id rather than
// minting a new one.
func TestProductionParity_AcquireWritesTheAmbientActiveJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv("MPM_WORKSPACE")
	invalidateSessionCache()
	t.Cleanup(invalidateSessionCache)

	if err := os.MkdirAll(filepath.Join(home, ".mpm"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	first := AcquireMPMSessionID()
	if first == "" {
		t.Fatal("AcquireMPMSessionID returned empty")
	}
	if _, err := os.Stat(filepath.Join(home, ".mpm", "active.json")); err != nil {
		t.Fatalf("ambient active.json was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".mpm", "active.json.lock")); err != nil {
		t.Fatalf("ambient active.json.lock was not created: %v", err)
	}
	if second := AcquireMPMSessionID(); second != first {
		t.Errorf("second acquisition minted a new id: first=%q second=%q", first, second)
	}
	if got := CurrentMPMSessionID(); got != first {
		t.Errorf("CurrentMPMSessionID() = %q, want %q", got, first)
	}
}

// TestProductionParity_ExplicitSessionIDStillWins proves the ownership
// change did not disturb the caller-supplied path: an explicit id
// bypasses allocation entirely and must not create a lock file.
func TestProductionParity_ExplicitSessionIDStillWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv("MPM_WORKSPACE")
	if err := os.MkdirAll(filepath.Join(home, ".mpm"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ws := t.TempDir()
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })

	h, err := dm.EndSessionV2(
		"", "framework-explicit", "mpm-caller-supplied",
		"explicit identity", "clean",
		[]string{"c"}, []string{"o"},
	)
	if err != nil {
		t.Fatalf("EndSessionV2: %v", err)
	}
	if h.MPMSessionID != "mpm-caller-supplied" {
		t.Errorf("mpm_session_id = %q, want the caller-supplied value", h.MPMSessionID)
	}
	if _, err := os.Stat(filepath.Join(ws, "active.json")); !os.IsNotExist(err) {
		t.Errorf("an explicit session id must not allocate: active.json exists in %s (err=%v)", ws, err)
	}
}

// TestProductionParity_ScannerUsesTheAmbientPhraseFile proves the
// scanner still reads the canonical list under the canonical path when
// no manager is in play — the shape the CLI's ScanContentForWrite and
// skill frontmatter validation use.
func TestProductionParity_ScannerUsesTheAmbientPhraseFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv("MPM_WORKSPACE")
	if err := os.MkdirAll(filepath.Join(home, ".mpm"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if got := config.GetToxicPhrasesPath(); got != filepath.Join(home, ".mpm", "toxicphrases.txt") {
		t.Fatalf("GetToxicPhrasesPath() = %q, want %q",
			got, filepath.Join(home, ".mpm", "toxicphrases.txt"))
	}

	// A clean sentence generates the file and reports no poison.
	if blocked, reason := ScanContentForWrite("an entirely benign sentence"); blocked {
		t.Fatalf("benign content blocked: %s", reason)
	}
	if _, err := os.Stat(filepath.Join(home, ".mpm", "toxicphrases.txt")); err != nil {
		t.Fatalf("scanner did not generate the canonical phrase file: %v", err)
	}

	// A seeded phrase is still caught — generation must not weaken the
	// scanner.
	phrases, err := loadPoisonPhrases()
	if err != nil || len(phrases) == 0 {
		t.Fatalf("loadPoisonPhrases: %v (len=%d)", err, len(phrases))
	}
	if blocked, reason := ScanContentForWrite(phrases[0]); !blocked {
		t.Errorf("seeded phrase %q was not blocked; the scanner is inert against its own list", phrases[0])
	} else if !strings.Contains(reason, "poison phrase") {
		t.Errorf("blocked with the wrong reason: %q", reason)
	}
}
