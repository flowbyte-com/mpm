package internal

// aux_log_isolation_test.go — hermeticity of the auxiliary logs that
// accompany a database (watchdog.jsonl, mirror.jsonl).
//
// # The defect
//
// Two DatabaseManager paths derived a writable auxiliary log location
// from the AMBIENT MPM workspace rather than from the database the
// manager actually wraps:
//
//   NewDatabaseManagerForDB  -> config.GetMPMDir()/src/db/watchdog.jsonl
//   ChallengeMemoryAsync     -> config.GetMPMDir()/src/db/mirror.jsonl,
//                               re-resolved INSIDE the goroutine
//
// NewTestDM — documented as hermetic, "no prod-DB pollution" — routes
// through NewDatabaseManagerForDB for every in-memory test database, so
// the first defect made every ExecTracked/QueryTracked in ~121 test files
// append a line to the operator's real ~/.mpm/src/db/watchdog.jsonl.
//
// The second defect was worse than a misdirected write: because the path
// was read at goroutine EXECUTION time, the destination depended on when
// the goroutine was scheduled. A test that restored the environment in
// t.Cleanup could still see the write land in a directory the test had
// stopped pointing at.
//
// The bug was LATENT on a host with no ~/.mpm/src/db, because logWatchdog
// opens the log without creating parent directories — every write failed
// silently. It fires on any real install, which is why the leak was only
// discovered by comparing live runtime sentinels around a full test run.
//
// # The invariant these tests pin
//
// A manager derives its auxiliary log paths from the directory holding
// ITS OWN database. In-memory has no directory, so it writes no auxiliary
// log. Production construction is unchanged.

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
	_ "github.com/mattn/go-sqlite3"
)

// sentinelInstallHome points HOME at a throwaway directory that already
// contains a complete ~/.mpm/src/db tree, and reports where the ambient
// workspace would point.
//
// The pre-created tree is essential and is the reason these tests can
// detect the defect at all: with a pristine HOME the OpenFile in
// logWatchdog fails on the missing parent directory and the write is
// silently dropped, which makes a leak-injection check look like a pass.
func sentinelInstallHome(t *testing.T) (home, ambientWatchdog, ambientMirror string) {
	t.Helper()
	home = t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".mpm", "src", "db"), 0700); err != nil {
		t.Fatalf("mkdir sentinel install: %v", err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("MPM_WORKSPACE")
	ambientWatchdog = filepath.Join(config.GetMPMDir(), "src", "db", "watchdog.jsonl")
	ambientMirror = filepath.Join(config.GetMPMDir(), "src", "db", "mirror.jsonl")
	return home, ambientWatchdog, ambientMirror
}

func openFileDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// assertNoAmbientWrite fails if either ambient log gained a byte.
func assertNoAmbientWrite(t *testing.T, wd, mirror string) {
	t.Helper()
	for label, p := range map[string]string{"watchdog.jsonl": wd, "mirror.jsonl": mirror} {
		st, err := os.Stat(p)
		if err == nil {
			t.Errorf("ESCAPED WRITE: %s was created at %s (%d bytes) "+
				"outside the isolated database root", label, p, st.Size())
		} else if !os.IsNotExist(err) {
			t.Errorf("unexpected error stat %s: %v", p, err)
		}
	}
}

// TestAuxLogs_InMemoryManagerWritesNothing is the core regression for
// the NewTestDM path: an in-memory database has no directory, so it must
// have no auxiliary logs anywhere.
func TestAuxLogs_InMemoryManagerWritesNothing(t *testing.T) {
	_, ambientWD, ambientMirror := sentinelInstallHome(t)
	dm := NewTestDM(t)

	// Exercise the tracked paths that log to the watchdog.
	if _, err := dm.ExecTracked("CREATE TABLE IF NOT EXISTS aux_probe(x INTEGER)", 3); err != nil {
		t.Fatalf("ExecTracked: %v", err)
	}
	if _, err := dm.ExecTracked("INSERT INTO aux_probe VALUES (1)", 3); err != nil {
		t.Fatalf("ExecTracked: %v", err)
	}
	rows, err := dm.QueryTracked("SELECT x FROM aux_probe")
	if err != nil {
		t.Fatalf("QueryTracked: %v", err)
	}
	rows.Close()
	dm.ChallengeMemoryAsync("mem-1", "evidence-1")
	dm.mirrorWG.Wait()

	if dm.watchdogPath != "" || dm.mirrorPath != "" {
		t.Errorf("in-memory manager must have no auxiliary log paths; "+
			"got watchdogPath=%q mirrorPath=%q", dm.watchdogPath, dm.mirrorPath)
	}
	assertNoAmbientWrite(t, ambientWD, ambientMirror)
}

// TestAuxLogs_FileBackedDBStaysBesideItsOwnDatabase pins the positive
// direction: an explicitly isolated file-backed database keeps its logs
// in its own tree and nowhere else.
func TestAuxLogs_FileBackedDBStaysBesideItsOwnDatabase(t *testing.T) {
	_, ambientWD, ambientMirror := sentinelInstallHome(t)
	root := t.TempDir()
	db := openFileDB(t, filepath.Join(root, "isolated.db"))
	dm := NewDatabaseManagerForDB(db)
	defer dm.Close()

	if _, err := dm.ExecTracked("CREATE TABLE IF NOT EXISTS aux_probe(x INTEGER)", 3); err != nil {
		t.Fatalf("ExecTracked: %v", err)
	}
	dm.ChallengeMemoryAsync("mem-2", "evidence-2")
	dm.mirrorWG.Wait()

	wantWD := filepath.Join(root, "watchdog.jsonl")
	wantMirror := filepath.Join(root, "mirror.jsonl")
	if dm.watchdogPath != wantWD {
		t.Errorf("watchdogPath = %q, want %q", dm.watchdogPath, wantWD)
	}
	if dm.mirrorPath != wantMirror {
		t.Errorf("mirrorPath = %q, want %q", dm.mirrorPath, wantMirror)
	}
	for label, p := range map[string]string{"watchdog": wantWD, "mirror": wantMirror} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s log missing inside the isolated root at %s: %v", label, p, err)
		}
	}
	assertNoAmbientWrite(t, ambientWD, ambientMirror)
}

// TestAuxLogs_AsyncWriteCannotEscapeViaEnvironment is the
// execution-time-resolution defect, pinned.
//
// It restores a *different* workspace after the call has been made and
// before the goroutine can be assumed to have run, then proves the write
// still lands in the manager's own root. Under the old implementation
// the destination was read at goroutine-execution time, so the write
// followed the environment rather than the call.
func TestAuxLogs_AsyncWriteCannotEscapeViaEnvironment(t *testing.T) {
	_, ambientWD, ambientMirror := sentinelInstallHome(t)
	root := t.TempDir()
	db := openFileDB(t, filepath.Join(root, "isolated.db"))
	dm := NewDatabaseManagerForDB(db)
	defer dm.Close()

	// A workspace the manager has no relationship to, pointing somewhere
	// the test will restore to before joining.
	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, "src", "db"), 0700); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}

	t.Setenv("MPM_WORKSPACE", decoy)
	dm.ChallengeMemoryAsync("mem-3", "evidence-3")
	// Restore before joining, exactly as t.Cleanup ordering could.
	t.Setenv("MPM_WORKSPACE", "")
	dm.mirrorWG.Wait()

	if _, err := os.Stat(filepath.Join(root, "mirror.jsonl")); err != nil {
		t.Errorf("mirror.jsonl did not land beside the manager's own database: %v", err)
	}
	decoyMirror := filepath.Join(decoy, "src", "db", "mirror.jsonl")
	if st, err := os.Stat(decoyMirror); err == nil {
		t.Errorf("ESCAPED WRITE: goroutine followed MPM_WORKSPACE to %s (%d bytes)",
			decoyMirror, st.Size())
	}
	assertNoAmbientWrite(t, ambientWD, ambientMirror)
}

// TestAuxLogs_ConcurrentAsyncWritesAllStayLocal runs many concurrent
// mirror writers against one isolated manager. All writes must land in
// the manager's own root, and none may escape. This also exercises the
// shared watchdogMu/rotation path under -race.
func TestAuxLogs_ConcurrentAsyncWritesAllStayLocal(t *testing.T) {
	_, ambientWD, ambientMirror := sentinelInstallHome(t)
	root := t.TempDir()
	db := openFileDB(t, filepath.Join(root, "isolated.db"))
	dm := NewDatabaseManagerForDB(db)
	defer dm.Close()

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dm.ChallengeMemoryAsync("mem-concurrent", "evidence")
			if _, err := dm.ExecTracked("CREATE TABLE IF NOT EXISTS aux_probe(x INTEGER)", 3); err != nil {
				t.Errorf("concurrent ExecTracked: %v", err)
			}
		}(i)
	}
	wg.Wait()
	dm.mirrorWG.Wait()

	data, err := os.ReadFile(filepath.Join(root, "mirror.jsonl"))
	if err != nil {
		t.Fatalf("read local mirror: %v", err)
	}
	if got := countLines(data); got != n {
		t.Errorf("local mirror has %d lines, want %d", got, n)
	}
	assertNoAmbientWrite(t, ambientWD, ambientMirror)
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// TestAuxLogs_ProductionConstructionUnchanged is the anti-regression for
// the real runtime: NewDatabaseManager must still place BOTH logs under
// the workspace it was given, exactly as before this change.
func TestAuxLogs_ProductionConstructionUnchanged(t *testing.T) {
	// A real, complete ~/.mpm/src/db in a throwaway HOME, so this models
	// an actual install rather than a synthetic directory.
	home, _, _ := sentinelInstallHome(t)
	ws := filepath.Join(home, ".mpm")

	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	wantWD := filepath.Join(ws, "src", "db", "watchdog.jsonl")
	wantMirror := filepath.Join(ws, "src", "db", "mirror.jsonl")
	if dm.watchdogPath != wantWD {
		t.Errorf("production watchdogPath = %q, want %q", dm.watchdogPath, wantWD)
	}
	if dm.mirrorPath != wantMirror {
		t.Errorf("production mirrorPath = %q, want %q", dm.mirrorPath, wantMirror)
	}

	// And the logs must actually be written there, with 0600.
	if _, err := dm.ExecTracked("CREATE TABLE IF NOT EXISTS aux_probe(x INTEGER)", 3); err != nil {
		t.Fatalf("ExecTracked: %v", err)
	}
	dm.ChallengeMemoryAsync("mem-prod", "evidence-prod")
	dm.mirrorWG.Wait()

	for _, p := range []string{wantWD, wantMirror} {
		st, err := os.Stat(p)
		if err != nil {
			t.Errorf("production log missing at %s: %v", p, err)
			continue
		}
		if perm := st.Mode().Perm(); perm != 0600 {
			t.Errorf("production log %s has mode %o, want 0600", p, perm)
		}
	}
}

// TestAuxLogDir_ReportsTheDatabaseOwnDirectory pins the derivation
// helper directly, including the in-memory and nil cases.
func TestAuxLogDir_ReportsTheDatabaseOwnDirectory(t *testing.T) {
	if got := auxLogDir(nil); got != "" {
		t.Errorf("auxLogDir(nil) = %q, want \"\"", got)
	}

	mem, err := sql.Open("sqlite3", "file:aux-logdir-mem?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open in-memory: %v", err)
	}
	defer mem.Close()
	if got := auxLogDir(mem); got != "" {
		t.Errorf("auxLogDir(in-memory) = %q, want \"\"", got)
	}

	root := t.TempDir()
	db := openFileDB(t, filepath.Join(root, "x.db"))
	if got := auxLogDir(db); got != root {
		t.Errorf("auxLogDir(file-backed) = %q, want %q", got, root)
	}
}

// TestAuxLogs_SharedStoreInheritsTheManagerMirror pins the third path:
// getSharedStore builds a MemoryStore through NewMemoryStore(""), which
// discards its argument and derives MirrorFile from the AMBIENT workspace.
//
// This was a larger hole than either DatabaseManager leak. SaveMemory,
// UpdateMemory, DeleteMemory and ChallengeMemoryWithTheory all reach the
// store through getSharedStore, so every ordinary memory write in ~121
// test files appended a full audit row — content, metadata and all — to
// the operator's real ~/.mpm/src/db/mirror.jsonl. It was invisible to the
// DatabaseManager-level tests because those only exercise ExecTracked and
// ChallengeMemoryAsync, neither of which touches the store.
func TestAuxLogs_SharedStoreInheritsTheManagerMirror(t *testing.T) {
	_, ambientWD, ambientMirror := sentinelInstallHome(t)
	dm := NewTestDM(t)

	id, err := dm.SaveMemory("memories", "shared store must not reach the live mirror", "",
		[]string{"aux"}, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if _, err := dm.ChallengeMemoryWithTheory(id, "contradicted by the aux sweep"); err != nil {
		t.Fatalf("ChallengeMemoryWithTheory: %v", err)
	}
	if err := dm.UpdateMemory(id, "edited", nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	store, err := dm.getSharedStore()
	if err != nil {
		t.Fatalf("getSharedStore: %v", err)
	}
	if store.MirrorFile != "" {
		t.Errorf("shared store MirrorFile = %q, want \"\" for an in-memory manager",
			store.MirrorFile)
	}
	assertNoAmbientWrite(t, ambientWD, ambientMirror)
}

// TestAuxLogs_SharedStoreWritesBesideAFileBackedDatabase is the positive
// direction for the same path: a file-backed manager's ordinary memory
// writes must still produce mirror rows, in its own directory.
func TestAuxLogs_SharedStoreWritesBesideAFileBackedDatabase(t *testing.T) {
	_, ambientWD, ambientMirror := sentinelInstallHome(t)
	// NewDatabaseManager (not NewDatabaseManagerForDB) so the database has
	// a real schema — SaveMemory needs the memories table.
	root := t.TempDir()
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	id, err := dm.SaveMemory("memories", "this row belongs to the isolated database", "",
		[]string{"aux"}, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	// ChallengeMemoryWithTheory is the store-reaching path: it calls
	// getSharedStore().AddMemory, which is what appendToMirror sits behind.
	if _, err := dm.ChallengeMemoryWithTheory(id, "contradicted by the aux sweep"); err != nil {
		t.Fatalf("ChallengeMemoryWithTheory: %v", err)
	}

	local := filepath.Join(root, "src", "db", "mirror.jsonl")
	data, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("read local mirror: %v", err)
	}
	if !bytes.Contains(data, []byte("this row belongs to the isolated database")) {
		t.Errorf("mirror row missing from %s; got:\n%s", local, data)
	}
	assertNoAmbientWrite(t, ambientWD, ambientMirror)
}

// TestAuxLogs_SessionSharesTheParentMirror pins NewSession: a session is
// a second connection to the SAME database, so it must reuse the parent's
// mirror rather than re-deriving one. Re-deriving from dbPath would put
// the log in the process working directory when the parent is in-memory,
// because filepath.Dir("") is ".".
func TestAuxLogs_SessionSharesTheParentMirror(t *testing.T) {
	_, ambientWD, ambientMirror := sentinelInstallHome(t)
	dm := NewTestDM(t)
	if _, err := dm.ExecTracked("CREATE TABLE IF NOT EXISTS aux_probe(x INTEGER)", 3); err != nil {
		t.Fatalf("ExecTracked: %v", err)
	}

	sess, err := dm.NewSession()
	if err != nil {
		t.Skipf("NewSession requires a reopenable file-backed database: %v", err)
	}
	defer sess.Close()

	sdm, ok := sess.(*DatabaseManager)
	if !ok {
		t.Fatalf("NewSession returned %T, want *DatabaseManager", sess)
	}
	if sdm.mirrorPath != dm.mirrorPath {
		t.Errorf("session mirrorPath = %q, want the parent's %q", sdm.mirrorPath, dm.mirrorPath)
	}
	assertNoAmbientWrite(t, ambientWD, ambientMirror)
}

// TestAuxLogs_BlockedAttemptWithoutMirrorIsSilentNoOp pins the empty-path
// contract on the second writer. Before the guard, an empty MirrorFile
// reached os.OpenFile("") and returned an error, which every call site
// surfaces as a warning — turning "no mirror directory" into noise on
// every blocked-content rejection under an in-memory manager.
func TestAuxLogs_BlockedAttemptWithoutMirrorIsSilentNoOp(t *testing.T) {
	store := &MemoryStore{MirrorFile: ""}
	if err := store.appendBlockedAttempt("secret", "blocked: ghp_", "save"); err != nil {
		t.Errorf("appendBlockedAttempt with no mirror must be a silent no-op, got %v", err)
	}
	if err := store.appendToMirror(&Memory{ID: "x", Collection: "memories", Content: "y"}); err != nil {
		t.Errorf("appendToMirror with no mirror must be a silent no-op, got %v", err)
	}
}

// TestAuxLogs_ProductionSharedStoreUsesCanonicalMirror is the second
// half of the production-parity proof. The first
// (TestAuxLogs_ProductionConstructionUnchanged) checks the manager's own
// mirrorPath; this checks the store it hands out, because getSharedStore
// now OVERWRITES MirrorFile and that overwrite must be a no-op in
// production. If it were not, every memory write in a real install would
// silently lose its audit row.
func TestAuxLogs_ProductionSharedStoreUsesCanonicalMirror(t *testing.T) {
	home, _, _ := sentinelInstallHome(t)
	ws := filepath.Join(home, ".mpm")

	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	store, err := dm.getSharedStore()
	if err != nil {
		t.Fatalf("getSharedStore: %v", err)
	}
	want := filepath.Join(ws, "src", "db", "mirror.jsonl")
	if store.MirrorFile != want {
		t.Errorf("production shared store MirrorFile = %q, want %q", store.MirrorFile, want)
	}
	// And the row really lands there, through the store, with 0600.
	id, err := dm.SaveMemory("memories", "canonical production mirror row", "",
		[]string{"aux"}, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if _, err := dm.ChallengeMemoryWithTheory(id, "production challenge evidence"); err != nil {
		t.Fatalf("ChallengeMemoryWithTheory: %v", err)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read canonical mirror: %v", err)
	}
	if !bytes.Contains(data, []byte("CHALLENGED_MEMORY_ID: "+id)) {
		t.Errorf("challenge audit row missing from %s; got:\n%s", want, data)
	}
	st, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat canonical mirror: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0600 {
		t.Errorf("canonical mirror has mode %o, want 0600", perm)
	}
}

// TestAuxLogs_MirrorStreamStillRotates proves the ChallengeMemoryAsync
// rotation step survived the path change. Rotation is applied to the
// RESOLVED path inside the goroutine, so a regression here would mean the
// mirror grows without bound in production.
func TestAuxLogs_MirrorStreamStillRotates(t *testing.T) {
	home, _, _ := sentinelInstallHome(t)
	ws := filepath.Join(home, ".mpm")
	t.Setenv("MPM_LOG_ROTATE_BYTES", "512")

	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	mirror := filepath.Join(ws, "src", "db", "mirror.jsonl")
	for i := 0; i < 64; i++ {
		dm.ChallengeMemoryAsync("mem-rotate", "rotation evidence")
	}
	dm.mirrorWG.Wait()

	rotated, err := filepath.Glob(mirror + ".*.gz")
	if err != nil {
		t.Fatalf("glob rotated mirrors: %v", err)
	}
	if len(rotated) == 0 {
		t.Errorf("mirror.jsonl never rotated at a 512-byte threshold; size now %d",
			fileSize(t, mirror))
	}
}

// TestAuxLogs_LogFailureIsNonFatal pins the pre-existing contract that an
// unwritable audit log never fails the memory write that produced it. The
// empty-MirrorFile no-op added alongside this change must not be mistaken
// for an error path, and a genuine IO failure must still be swallowed at
// the call site.
func TestAuxLogs_LogFailureIsNonFatal(t *testing.T) {
	home, _, _ := sentinelInstallHome(t)
	ws := filepath.Join(home, ".mpm")
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// Replace the log with a directory so every append fails with EISDIR.
	mirror := filepath.Join(ws, "src", "db", "mirror.jsonl")
	if err := os.RemoveAll(mirror); err != nil {
		t.Fatalf("remove mirror: %v", err)
	}
	if err := os.MkdirAll(mirror, 0700); err != nil {
		t.Fatalf("mkdir in place of mirror: %v", err)
	}

	id, err := dm.SaveMemory("memories", "row written with an unwritable mirror", "",
		[]string{"aux"}, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory must succeed even when the mirror is unwritable: %v", err)
	}
	if _, err := dm.ChallengeMemoryWithTheory(id, "evidence with an unwritable mirror"); err != nil {
		t.Fatalf("ChallengeMemoryWithTheory must succeed with an unwritable mirror: %v", err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}
