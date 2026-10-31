// update_safety_test.go — regression tests for the update-safety
// corrections (PR #195 follow-up).
//
// Each test fails against the pre-correction implementation:
//
//   - restart-services was accepted and ignored;
//   - a restore proceeded past non-service writers;
//   - the stable updater was replaced before the health checks;
//   - a migrating candidate's failure silently left old binaries on a
//     new schema;
//   - backup verification compared two different points in time;
//   - dry-run took a persistent backup.
package update

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// --- restart-services ---------------------------------------------------

// TestVerifyServiceExe pins the identity rule: the executable must
// resolve into the new release directory.
func TestVerifyServiceExe(t *testing.T) {
	root := t.TempDir()
	rev := "abc123"
	base := filepath.Join(root, "releases", rev)
	if got := verifyServiceExe(root, rev, "mpm-scheduler", 42, base+"/bin/mpm-scheduler"); got != "" {
		t.Errorf("exact unit binary should match: %q", got)
	}
	if got := verifyServiceExe(root, rev, "mpm-scheduler", 42, base+"/bin/mpm"); got != "" {
		t.Errorf("same-release binary should match: %q", got)
	}
	if got := verifyServiceExe(root, rev, "mpm-scheduler", 42, "/usr/local/bin/mpm-scheduler"); got == "" {
		t.Error("foreign executable must warn")
	} else if !strings.Contains(got, "restart it by hand") {
		t.Errorf("warning must tell the operator what to do: %q", got)
	}
}

// TestRestartServices_ScopesStopStart pins the scoping: only active
// units are stopped and started back; inactive units are untouched.
func TestRestartServices_ScopesStopStart(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	sc := &fakeServices{active: map[string]bool{"mpm-scheduler": true}, pids: map[string]int{"mpm-scheduler": 1}}
	o := Options{
		DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{},
		RestartServices: true, Services: sc,
	}
	out, err := Update(context.Background(), o)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(sc.stopped) != 1 || sc.stopped[0] != "mpm-scheduler" {
		t.Fatalf("stopped = %v, want only the active unit", sc.stopped)
	}
	if len(sc.started) != 1 || sc.started[0] != "mpm-scheduler" {
		t.Fatalf("started = %v, want only the stopped unit back (inactive stays inactive)", sc.started)
	}
	// pid 1 is not the new release: the report must warn, not claim
	// every process runs the new release.
	if !strings.Contains(out.Report, "warning:") {
		t.Errorf("identity mismatch must warn:\\n%s", out.Report)
	}
	if !strings.Contains(out.Report, "restarted: mpm-scheduler") {
		t.Errorf("report must name the restarted unit:\\n%s", out.Report)
	}
}

// TestNoRestartServices_WarnsAboutStaleDaemons pins the unprivileged
// path: without --restart-services an active daemon is reported as
// possibly stale, never touched.
func TestNoRestartServices_WarnsAboutStaleDaemons(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	sc := &fakeServices{active: map[string]bool{"mpm-scheduler": true}}
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, Services: sc}
	out, err := Update(context.Background(), o)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(sc.stopped) != 0 || len(sc.started) != 0 {
		t.Fatalf("without --restart-services nothing may be stopped or started: stopped=%v started=%v", sc.stopped, sc.started)
	}
	if !strings.Contains(out.Report, "may still use the old executable") {
		t.Errorf("report must warn about the unstopped daemon:\\n%s", out.Report)
	}
}

// --- writer quiescence ----------------------------------------------------

// TestRestore_RefusesOpenWriters pins the unbypassable writer check: a
// held SQLite connection and a simulated agent holding the DB file both
// block the restore, even with --with-db-restore --force.
func TestRestore_RefusesOpenWriters(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	if _, err := Update(context.Background(), Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, Services: QuietServices{}}); err != nil {
		t.Fatalf("setup update: %v", err)
	}

	live := NewLayout(root).LiveDB()
	held, err := sql.Open("sqlite3", "file:"+live)
	if err != nil {
		t.Fatal(err)
	}

	var n int
	if err := held.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		t.Fatal(err)
	}

	// A simulated agent: a live process whose open files include the DB.
	agentDB, err := os.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	defer agentDB.Close()
	agent := exec.Command("bash", "-c", "exec -a opencode-agent sleep 60")
	agent.ExtraFiles = []*os.File{agentDB}
	agent.Stdout, agent.Stderr = nil, nil
	if err := agent.Start(); err != nil {
		t.Fatalf("start simulated agent: %v", err)
	}
	defer agent.Process.Kill() //nolint:errcheck
	time.Sleep(200 * time.Millisecond)

	if holders := OpenDBHolders(live); len(holders) == 0 {
		t.Fatal("the held connection and the agent should be observed as holders")
	}

	_, err = Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if err == nil {
		t.Fatal("restore under writers must be refused even with --force")
	}
	if _, ok := err.(*WritersActiveError); !ok {
		t.Fatalf("want *WritersActiveError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "--force") || !strings.Contains(strings.ToLower(err.Error()), "does not bypass") {
		t.Errorf("the refusal must explain that --force does not bypass it:\\n%v", err)
	}

	// Quiesce: close our handle, kill the agent, retry — now it works.
	held.Close()
	agent.Process.Kill() //nolint:errcheck
	agent.Wait()         //nolint:errcheck
	agentDB.Close()      //nolint:errcheck
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(OpenDBHolders(live)) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err = Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}}); err != nil {
		t.Fatalf("quiesced restore: %v", err)
	}
}

// --- migration then failing smoke -------------------------------------------

// migratingBuilder produces a candidate whose binary migrates the LIVE
// database when its health check runs outside the scratch area, then
// fails. Scratch smoke passes; live smoke fails after migrating.
type migratingBuilder struct{ commit string }

func (m migratingBuilder) BuildFromCheckout(srcDir, dstDir string) (string, error) {
	commit := m.commit
	if commit == "" {
		if sha, err := gitHeadCommit(srcDir); err == nil {
			commit = sha
		} else {
			commit = "ffffffffffffffffffffffffffffffffffffffff"
		}
	}
	binDir := filepath.Join(dstDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", err
	}
	for _, bin := range append(append([]string{}, runtimeBinaries...), UpdaterBinaryName) {
		script := "#!/bin/sh\nexit 0\n"
		if bin == "mpm" {
			script = fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then echo "commit: %s"; exit 0; fi
case "$*" in
  *health_check*)
    case "$MPM_DB_PATH" in
      *mpm-update-smoke.*) echo '{"ok":true}'; exit 0;;
      *) python3 -c "import sqlite3; c=sqlite3.connect('$MPM_DB_PATH'); c.execute('ALTER TABLE memories ADD COLUMN evil_migration INTEGER'); c.commit()"; exit 1;;
    esac;;
  *) echo '{}';;
esac
`, commit)
		}
		if err := os.WriteFile(filepath.Join(binDir, bin), []byte(script), 0755); err != nil {
			return "", err
		}
	}
	if err := WriteRelease(dstDir, commit, commit, 1700000000, "v-mig"); err != nil {
		return "", err
	}
	for _, p := range []string{
		"agent_installation/mpm-opencode/server.js",
		"agent_installation/mpm-opencode/dist/index.v2.js",
	} {
		full := filepath.Join(dstDir, p)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte("// payload"), 0644)
	}
	return commit, nil
}

// TestFailedActivationAfterMigration_KeepsEverythingConsistent pins the
// worst failure path: the new binary migrates the live database, THEN
// fails its health check.
func TestFailedActivationAfterMigration_KeepsEverythingConsistent(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: migratingBuilder{}, Services: QuietServices{}}
	_, err := Update(context.Background(), o)
	if err == nil {
		t.Fatal("the migrating candidate must fail its live smoke test")
	}
	// The pointer is back on the old release — never left pointing at
	// a release that never proved itself.
	if _, got, _ := CurrentReleaseTarget(root); got != oldRev {
		t.Fatalf("pointer = %q, want restored %q", got, oldRev)
	}
	// The history records the migration explicitly. A silent restore
	// would pretend the database is untouched.
	hist, _ := ReadHistory(root)
	if len(hist) != 1 || hist[0].Outcome != "failed" {
		t.Fatalf("history = %+v", hist)
	}
	if !hist[0].SchemaChanged {
		t.Error("the history must record that the schema changed during the failed smoke")
	}
	if !strings.Contains(hist[0].Detail, "migration") {
		t.Errorf("the failure record must name the migration:\\n%s", hist[0].Detail)
	}
	if hist[0].BackupDir == "" {
		t.Error("the failure record must name the recovery point")
	}
	// The database is NOT silently restored: the migration is still
	// there, awaiting an explicit operator decision.
	cols, err := tableNames(NewLayout(root).LiveDB())
	_ = cols
	db, _ := sql.Open("sqlite3", "file:"+NewLayout(root).LiveDB()+"?mode=ro")
	var hasEvil bool
	rows, _ := db.Query(`PRAGMA table_info(memories)`)
	for rows.Next() {
		var cid int
		var name, ctype string
		var nn int
		var dflt sql.NullString
		var pk int
		rows.Scan(&cid, &name, &ctype, &nn, &dflt, &pk)
		if name == "evil_migration" {
			hasEvil = true
		}
	}
	rows.Close()
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvil {
		t.Error("the migrated column must still be present: a failed activation never overwrites newer database writes")
	}
	// The stable updater was never replaced by the unproven release.
	if _, err := os.Stat(filepath.Join(root, StableUpdaterName)); !os.IsNotExist(err) {
		t.Error("the stable updater must not be replaced before the health checks pass")
	}
}

// --- backup under concurrent writes --------------------------------------------

func TestBackup_ToleratesConcurrentWriters(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()
	stop := make(chan struct{})
	var stopOnce sync.Once
	closeStop := func() { stopOnce.Do(func() { close(stop) }) }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		db, _ := sql.Open("sqlite3", "file:"+live)
		defer db.Close()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				db.Exec(fmt.Sprintf("INSERT INTO memories (id, content) VALUES ('w-%d-%d', 'x')", time.Now().UnixNano(), i))
				i++
			}
		}
	}()
	defer func() { closeStop(); wg.Wait() }()
	for i := 0; i < 3; i++ {
		res, err := BackupLiveDB(root, time.Now())
		if err != nil {
			t.Fatalf("backup under concurrent writes failed: %v", err)
		}
		// The backup verifies against itself even while live moves.
		if err := verifyBackup(live, res.DBPath); err != nil {
			t.Fatalf("backup self-verification failed: %v", err)
		}
	}
}

func TestCorruptBackupDetected(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()
	res, err := BackupLiveDB(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	truncated := res.DBPath + ".truncated"
	if err := os.WriteFile(truncated, raw[:len(raw)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(live, truncated); err == nil {
		t.Fatal("a truncated backup must fail verification")
	}
	empty := res.DBPath + ".empty"
	os.WriteFile(empty, []byte{}, 0600) //nolint:errcheck
	if err := verifyBackup(live, empty); err == nil {
		t.Fatal("an empty backup must fail verification")
	}
}

// --- dry-run and check side effects -----------------------------------------------

func snapshotDataRoot(t *testing.T, root string) (files map[string]string, dbBytes []byte) {
	t.Helper()
	files = map[string]string{}
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasSuffix(rel, ".db-wal") || strings.HasSuffix(rel, ".db-shm") || strings.HasSuffix(rel, ".db-journal") {
			return nil
		}
		raw, _ := os.ReadFile(p)
		files[rel] = string(raw)
		return nil
	})
	dbBytes, _ = os.ReadFile(filepath.Join(root, "src", "db", "mpm.db"))
	return files, dbBytes
}

func assertDataRootUnchanged(t *testing.T, root string, beforeFiles map[string]string, beforeDB []byte, what string) {
	t.Helper()
	afterFiles, afterDB := snapshotDataRoot(t, root)
	if len(afterFiles) != len(beforeFiles) {
		t.Errorf("%s changed the file set: before=%d after=%d", what, len(beforeFiles), len(afterFiles))
		for k := range afterFiles {
			if _, ok := beforeFiles[k]; !ok {
				t.Errorf("%s created: %s", what, k)
			}
		}
	}
	for k, v := range beforeFiles {
		if afterFiles[k] != v {
			t.Errorf("%s modified: %s", what, k)
		}
	}
	if string(afterDB) != string(beforeDB) {
		t.Errorf("%s modified the live database", what)
	}
}

// TestDryRun_Nonmutating pins the dry-run contract: validation without
// backup, staging, history, pointer moves or database writes.
func TestDryRun_Nonmutating(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	beforeFiles, beforeDB := snapshotDataRoot(t, root)
	out, err := Update(context.Background(), Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, DryRun: true, Services: QuietServices{}})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if out.AlreadyCurrent {
		t.Fatal("fixture should not already be current")
	}
	assertDataRootUnchanged(t, root, beforeFiles, beforeDB, "dry-run")
	if _, _, err := CurrentReleaseTarget(root); err != nil {
		t.Fatal("pointer must still resolve")
	} else if _, rev, _ := CurrentReleaseTarget(root); rev != oldRev {
		t.Fatalf("pointer moved to %q", rev)
	}
}

// TestCheck_CreatesNothing pins that --check resolves only.
func TestCheck_CreatesNothing(t *testing.T) {
	root := t.TempDir()
	beforeFiles, beforeDB := snapshotDataRoot(t, root)
	out, err := Check(context.Background(), Options{DataRoot: root, Fetcher: stubFetcher{rev: "rev-1"}})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if out.AlreadyCurrent {
		t.Fatal("empty root cannot already be current")
	}
	assertDataRootUnchanged(t, root, beforeFiles, beforeDB, "check")
}
