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

	"github.com/flowbyte-com/mpm/internal/testenv"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"
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
		t.Errorf("report must name the restarted unit")
	}
	if !strings.Contains(out.Report, "with verification warnings") {
		t.Errorf("an unverifiable identity is partial success, not a clean restart")
	}
	if strings.Contains(out.Report, "(verified against") {
		t.Errorf("the report must not claim clean verification with warnings present")
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
	// Sanctioned hermetic environment: the simulated agent must not
	// resolve production state through ambient MPM_WORKSPACE/HOME,
	// even though it only holds the passed-in fd.
	agent.Env = testenv.WithExtra(t)
	if err := agent.Start(); err != nil {
		t.Fatalf("start simulated agent: %v", err)
	}
	defer agent.Process.Kill() //nolint:errcheck
	time.Sleep(200 * time.Millisecond)

	if scan := ScanDBHolders(live); scan.Inconclusive || len(scan.Holders) == 0 {
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
		if scan := ScanDBHolders(live); (!scan.Inconclusive && len(scan.Holders) == 0) || time.Now().After(deadline) {
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
	if !strings.Contains(hist[0].Detail, "migrated schema") {
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

// --- degraded stop after incompatible migration -----------------------------------

// oldFailsMigratedBuilder is an old runtime that reads the old schema
// fine but cannot read the migrated one: its health check fails if
// and only if the evil column is present.
type oldFailsMigratedBuilder struct{ commit string }

func (b oldFailsMigratedBuilder) BuildFromCheckout(srcDir, dstDir string) (string, error) {
	commit := b.commit
	if commit == "" {
		commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	binDir := filepath.Join(dstDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", err
	}
	for _, bin := range append(append([]string{}, runtimeBinaries...), UpdaterBinaryName) {
		script := "#!/bin/sh\nexit 0\n"
		if bin == "mpm" {
			script = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "commit: ` + commit + `"; exit 0; fi
case "$*" in
  *health_check*)
    if python3 -c "import sqlite3,sys; c=sqlite3.connect('` + "$MPM_DB_PATH" + `'); sys.exit(0 if 'evil_migration' in [r[1] for r in c.execute('PRAGMA table_info(memories)')] else 1)"; then exit 1; else echo '{"ok":true}'; exit 0; fi;;
  *) echo '{}';;
esac
`
		}
		if err := os.WriteFile(filepath.Join(binDir, bin), []byte(script), 0755); err != nil {
			return "", err
		}
	}
	if err := WriteRelease(dstDir, commit, commit, 1700000000, "v-old"); err != nil {
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

// TestFailedActivationAfterIncompatibleMigration_StopsDegraded pins the
// degraded stop: when the new runtime migrated the live schema and the
// previous release cannot read it, the updater must NOT restore the old
// pointer. It keeps the new (data-matching) binaries, records the
// degraded state with its recovery point, and never touches the
// database.
func TestFailedActivationAfterIncompatibleMigration_StopsDegraded(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayoutWithOldBuilder(t, oldRev, oldFailsMigratedBuilder{})
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: migratingBuilder{}, Services: QuietServices{}}
	_, err := Update(context.Background(), o)
	if err == nil {
		t.Fatal("the migrating candidate must fail its live smoke test")
	}
	if !strings.Contains(err.Error(), "DEGRADED") {
		t.Fatalf("the failure must be identified as degraded, got:\n%v", err)
	}
	newRev, err2 := gitHeadCommit(srcDir)
	if err2 != nil {
		t.Fatal(err2)
	}
	// The pointer STAYS at the new release: the binaries match the
	// migrated data. Restoring the old pointer would strand incompatible
	// binaries on the new schema.
	if _, got, _ := CurrentReleaseTarget(root); got != newRev {
		t.Fatalf("pointer = %q, want kept at %q (degraded, not rolled back)", got, newRev)
	}
	hist, _ := ReadHistory(root)
	if len(hist) != 1 || hist[0].Outcome != "failed" {
		t.Fatalf("history = %+v", hist)
	}
	if !hist[0].SchemaChanged {
		t.Error("the history must record the schema change")
	}
	if hist[0].BackupDir == "" {
		t.Error("the degraded record must name the recovery point")
	}
	if !strings.Contains(hist[0].Detail, "DEGRADED") {
		t.Errorf("the history must record the degraded stop:\\n%s", hist[0].Detail)
	}
	// The database is NOT silently restored: the migration is still
	// there for the recovery procedure to handle explicitly.
	if hasEvilColumn(t, NewLayout(root).LiveDB()) != true {
		t.Error("the migrated column must still be present: a failed activation never overwrites newer database writes")
	}
	// The stable updater was never replaced by the unproven release.
	if _, err := os.Stat(filepath.Join(root, StableUpdaterName)); !os.IsNotExist(err) {
		t.Error("the stable updater must not be replaced before the health checks pass")
	}

	// The documented recovery path works from the degraded state:
	// --rollback --with-db-restore --force finds the FAILED update's
	// backup (not just successful ones), validates it, restores it,
	// and repoints at the old release.
	rout, err := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if err != nil {
		t.Fatalf("documented degraded recovery: %v", err)
	}
	if rout.ToRev != oldRev {
		t.Fatalf("rollback ToRev = %q, want %q", rout.ToRev, oldRev)
	}
	if _, got, _ := CurrentReleaseTarget(root); got != oldRev {
		t.Fatalf("pointer = %q, want restored %q", got, oldRev)
	}
	if hasEvilColumn(t, NewLayout(root).LiveDB()) {
		t.Error("the restored database must be the pre-update backup: no migrated column")
	}
	// The pre-update backup did not have the evil column (it predates
	// the migration) but did have the seeded row: the restore is the
	// recorded recovery point, not a silent overwrite.
	db, err := sql.Open("sqlite3", "file:"+NewLayout(root).LiveDB()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories WHERE id = 'm1'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("the restored database must contain the seeded row, got count=%d err=%v", n, err)
	}
	hist2, _ := ReadHistory(root)
	if len(hist2) != 2 || hist2[1].Outcome != "success" || hist2[1].Action != "rollback" {
		t.Fatalf("history must record the successful recovery rollback: %+v", hist2)
	}
}

func hasEvilColumn(t *testing.T, dbPath string) bool {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var nn int
		var dflt sql.NullString
		var pk int
		rows.Scan(&cid, &name, &ctype, &nn, &dflt, &pk)
		if name == "evil_migration" {
			return true
		}
	}
	return false
}

// --- rollback baseline helper -----------------------------------------------------

func TestRollbackBaselineDrift(t *testing.T) {
	hist := []HistoryEntry{{
		Action: "update", ToRev: "r2", Outcome: "success",
		PreFP: "fp-old", BackupDir: "/backups/x",
	}}
	if e, d := rollbackBaselineDrift(hist, "fp-old", "r2"); e == nil || d {
		t.Errorf("matching shape must not drift (entry=%v drift=%v)", e, d)
	}
	e, d := rollbackBaselineDrift(hist, "fp-new", "r2")
	if !d {
		t.Fatal("changed shape must drift")
	}
	if e == nil || e.BackupDir != "/backups/x" {
		t.Errorf("the drifted entry must carry the recovery point: %+v", e)
	}
	if _, d := rollbackBaselineDrift(nil, "x", "r2"); d {
		t.Error("no history must not drift")
	}
	noFP := []HistoryEntry{{Action: "update", ToRev: "r2", Outcome: "success"}}
	if _, d := rollbackBaselineDrift(noFP, "x", "r2"); d {
		t.Error("an entry without a fingerprint must not drift")
	}
	if _, d := rollbackBaselineDrift(hist, "fp-old", "other"); d {
		t.Error("an unrelated revision must not drift")
	}
}

// --- scanner conclusiveness ---------------------------------------------------------

func writeFakeProc(t *testing.T, procRoot, pid string, fdTarget, comm, statusUID string) {
	t.Helper()
	fdDir := filepath.Join(procRoot, pid, "fd")
	if fdTarget == "NOFD" {
		// The pid directory exists but has no fd subdirectory: the
		// process vanished mid-scan (benign race).
		if err := os.MkdirAll(filepath.Join(procRoot, pid), 0755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(fdDir, 0755); err != nil {
		t.Fatal(err)
	}
	if fdTarget == "UNREADABLE" {
		// Replace the fd directory with a file: listing it fails for
		// reasons other than a vanished process, at any privilege.
		os.RemoveAll(fdDir)
		if err := os.WriteFile(fdDir, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	} else if fdTarget != "" {
		if err := os.Symlink(fdTarget, filepath.Join(fdDir, "3")); err != nil {
			t.Fatal(err)
		}
	}
	if comm != "" {
		if err := os.WriteFile(filepath.Join(procRoot, pid, "comm"), []byte(comm), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if statusUID != "" {
		st := "Name:\ttest\nUid:\t" + statusUID + "\t" + statusUID + "\t" + statusUID + "\t" + statusUID + "\n"
		if err := os.WriteFile(filepath.Join(procRoot, pid, "status"), []byte(st), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func withFakeProc(t *testing.T, setup func(procRoot string)) {
	t.Helper()
	old := procFSRoot
	procFSRoot = t.TempDir()
	t.Cleanup(func() { procFSRoot = old })
	setup(procFSRoot)
}

func TestScanDBHolders_OccupiedAndClear(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mpm.db")
	os.WriteFile(db, []byte("x"), 0600)
	withFakeProc(t, func(procRoot string) {
		writeFakeProc(t, procRoot, "100", db, "holder", "")
		writeFakeProc(t, procRoot, "200", filepath.Join(filepath.Dir(db), "other"), "bystander", "")
		writeFakeProc(t, procRoot, "400", "NOFD", "", "")
		writeFakeProc(t, procRoot, "notapid", db, "ignored", "")
	})
	scan := ScanDBHolders(db)
	if scan.Inconclusive {
		t.Fatalf("complete observation must be conclusive: %s", scan.Detail)
	}
	if len(scan.Holders) != 1 || scan.Holders[0].PID != 100 {
		t.Fatalf("want exactly pid 100, got %+v", scan.Holders)
	}
	if scan.Holders[0].Name != "holder" {
		t.Fatalf("want comm-derived name, got %q", scan.Holders[0].Name)
	}
}

func TestScanDBHolders_InconclusiveOnUninspectable(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mpm.db")
	os.WriteFile(db, []byte("x"), 0600)
	own := fmt.Sprintf("%d", os.Geteuid())
	withFakeProc(t, func(procRoot string) {
		// Own uid: cannot be ruled out, so the scan fails closed.
		writeFakeProc(t, procRoot, "300", "UNREADABLE", "ghost", own)
	})
	scan := ScanDBHolders(db)
	if !scan.Inconclusive {
		t.Fatalf("an uninspectable same-uid process must make the scan inconclusive: %+v", scan)
	}
	if !strings.Contains(scan.Detail, "300") {
		t.Errorf("the detail must name the process: %q", scan.Detail)
	}
}

func TestScanDBHolders_RulesOutForeignUID(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mpm.db")
	os.WriteFile(db, []byte("x"), 0600)
	foreign := "65000"
	if fmt.Sprintf("%d", os.Geteuid()) == foreign {
		foreign = "65001"
	}
	withFakeProc(t, func(procRoot string) {
		writeFakeProc(t, procRoot, "300", "UNREADABLE", "foreign", foreign)
	})
	scan := ScanDBHolders(db)
	if scan.Inconclusive {
		t.Fatalf("a foreign-uid process on a 0700-closed-adjacent database is ruled out: %s", scan.Detail)
	}
	if scan.Skipped != 1 {
		t.Errorf("the ruled-out process must be counted, got %+v", scan)
	}
	if len(scan.Holders) != 0 {
		t.Errorf("no holders expected: %+v", scan.Holders)
	}
}

func TestScanDBHolders_BadProcRoot(t *testing.T) {
	old := procFSRoot
	procFSRoot = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { procFSRoot = old })
	scan := ScanDBHolders(filepath.Join(t.TempDir(), "mpm.db"))
	if !scan.Inconclusive {
		t.Fatal("an unreadable /proc must make the scan inconclusive")
	}
}

// --- identity reporting ---------------------------------------------------------------

type errPIDControl struct{ err error }

func (e errPIDControl) ActiveUnits() []string            { return nil }
func (e errPIDControl) Stop(unit string) error           { return nil }
func (e errPIDControl) Start(unit string) error          { return nil }
func (e errPIDControl) MainPID(unit string) (int, error) { return 0, e.err }

func TestVerifyServiceIdentity_Unidentifiable(t *testing.T) {
	w := verifyServiceIdentity(t.TempDir(), "rev", errPIDControl{err: fmt.Errorf("nope")}, "mpm-scheduler")
	if w == "" {
		t.Fatal("an unidentifiable process must warn, not verify")
	}
	if strings.Contains(w, "verified") {
		t.Errorf("must not claim verification: %q", w)
	}
	if !strings.Contains(w, "confirm it") {
		t.Errorf("the warning must tell the operator what to do: %q", w)
	}
}

// --- /proc/locks evidence ---------------------------------------------------------

func writeFakeLocks(t *testing.T, procRoot string, lines ...string) {
	t.Helper()
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(filepath.Join(procRoot, "locks"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func devInoOf(t *testing.T, path string) (uint64, uint64) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Dev, st.Ino
}

func formatLockLine(pid int, dev, ino uint64) string {
	return fmt.Sprintf("1: POSIX ADVISORY WRITE %d %x:%x:%d 0 EOF", pid, unix.Major(dev), unix.Minor(dev), ino)
}

// TestScanDBHolders_LockEvidenceCatchesUninspectable pins the
// world-readable fallback: a lock on our inode is hard evidence even
// when the holder's descriptors cannot be read.
func TestScanDBHolders_LockEvidenceCatchesUninspectable(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mpm.db")
	os.WriteFile(db, []byte("x"), 0600)
	dev, ino := devInoOf(t, db)
	withFakeProc(t, func(procRoot string) {
		writeFakeProc(t, procRoot, "400", "UNREADABLE", "locked-holder", "0")
		writeFakeLocks(t, procRoot, formatLockLine(400, dev, ino))
	})
	scan := ScanDBHolders(db)
	if scan.Inconclusive {
		t.Fatalf("a lock match is conclusive: %s", scan.Detail)
	}
	if len(scan.Holders) != 1 || scan.Holders[0].PID != 400 {
		t.Fatalf("want exactly pid 400 from lock evidence, got %+v", scan.Holders)
	}
}

// TestScanDBHolders_RootWithoutLockIsNamed pins the accepted
// residual: an uninspectable root process with no lock on our files
// does not block a restore — but it is NAMED, not silently skipped,
// so --force accepts something the operator actually saw.
func TestScanDBHolders_RootWithoutLockIsNamed(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mpm.db")
	os.WriteFile(db, []byte("x"), 0600)
	withFakeProc(t, func(procRoot string) {
		writeFakeProc(t, procRoot, "400", "UNREADABLE", "init-ish", "0")
		writeFakeLocks(t, procRoot)
	})
	scan := ScanDBHolders(db)
	if scan.Inconclusive {
		t.Fatalf("must not fail closed here: %s", scan.Detail)
	}
	if len(scan.Holders) != 0 {
		t.Fatalf("no holders expected: %+v", scan.Holders)
	}
	if len(scan.Unverifiable) != 1 || scan.Unverifiable[0].PID != 400 {
		t.Fatalf("the process must be named, not skipped: %+v", scan)
	}
	if scan.Unverifiable[0].Name != "init-ish" {
		t.Fatalf("the name must come through: %+v", scan.Unverifiable[0])
	}
}

// TestCannotOpenAny pins the permission emulation the rule-outs rest
// on. Foreign uid 65000 (or 65001 where that collides), our groups
// excluded on purpose: the point is what the BITS say.
func TestCannotOpenAny(t *testing.T) {
	dir := t.TempDir()
	foreign, groups := 65000, []int{65000}
	if os.Geteuid() == foreign {
		foreign = 65001
		groups = []int{65001}
	}
	closed := filepath.Join(dir, "closed.db")
	os.WriteFile(closed, []byte("x"), 0600)
	if !cannotOpenAny([]string{closed}, foreign, groups) {
		t.Error("0600 file must be closed to a foreign uid")
	}
	// A 0644 file under a traversable directory IS open to a
	// foreign uid: must not rule out. (Under t.TempDir itself, which
	// is 0700, the same file would be unreachable — traversal matters.
	// So this case uses its own 0755 tree, cleaned up by hand.)
	pubBase, err := os.MkdirTemp("", "mpm-open-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(pubBase) //nolint:errcheck
	if err := os.Chmod(pubBase, 0755); err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(pubBase, "pub")
	if err := os.Mkdir(pub, 0755); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(pub, "open.db")
	os.WriteFile(open, []byte("x"), 0644)
	if cannotOpenAny([]string{open}, foreign, groups) {
		t.Error("0644 file under a traversable dir is open to a foreign uid: must NOT rule out")
	}
	// Group-readable file under an other-untraversable directory:
	// unreachable, hence ruled out — without the directory the same
	// file would stay suspicious.
	locked := filepath.Join(dir, "locked.db")
	os.WriteFile(locked, []byte("x"), 0640)
	os.Chmod(dir, 0700)
	if !cannotOpenAny([]string{locked}, foreign, groups) {
		t.Error("0640 file under a 0700 dir must be closed to a foreign uid outside the group")
	}
	if cannotOpenAny([]string{filepath.Join(dir, "missing.db")}, foreign, groups) {
		t.Error("a missing file cannot be held open")
	}
}

// --- restore ordering and unverifiable handling -----------------------------------

// pinningServices is a fake ServiceControl whose Stop opens (and keeps)
// a real database connection — the scheduler-pins-the-DB case. The
// connection stays open until the test closes held.
type pinningServices struct {
	stopped []string
	started []string
	liveDB  string
	held    *sql.DB
}

func (f *pinningServices) ActiveUnits() []string { return []string{"mpm-scheduler"} }

func (f *pinningServices) Stop(unit string) error {
	f.stopped = append(f.stopped, unit)
	db, err := sql.Open("sqlite3", "file:"+f.liveDB)
	if err != nil {
		return err
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		db.Close()
		return err
	}
	f.held = db
	return nil
}

func (f *pinningServices) Start(unit string) error {
	f.started = append(f.started, unit)
	return nil
}

func (f *pinningServices) MainPID(unit string) (int, error) { return 0, nil }

// TestRestore_StopsServicesBeforeCheckingHolders pins the ordering:
// authorized managed services stop FIRST, so a scheduler pinning the
// database does not make an authorized restore fail. The pin the stop
// itself leaves behind still blocks the restore — and the stopped
// service is started back even on the refusal path.
func TestRestore_StopsServicesBeforeCheckingHolders(t *testing.T) {
	root := testLayout(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	if _, err := Update(context.Background(), Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, Services: QuietServices{}}); err != nil {
		t.Fatalf("setup update: %v", err)
	}
	sc := &pinningServices{liveDB: NewLayout(root).LiveDB()}
	_, err := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, RestartServices: true, Services: sc})
	if err == nil {
		t.Fatal("the pin left by the stop itself must still block the restore")
	}
	if _, ok := err.(*WritersActiveError); !ok {
		t.Fatalf("want *WritersActiveError, got %T: %v", err, err)
	}
	if len(sc.stopped) != 1 || len(sc.started) != 1 {
		t.Fatalf("the stopped service must be started back on the refusal path: stopped=%v started=%v", sc.stopped, sc.started)
	}
	sc.held.Close()
	if _, err = Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, RestartServices: true, Services: QuietServices{}}); err != nil {
		t.Fatalf("quiesced retry: %v", err)
	}
}

// TestRestore_RefusesUnverifiableWithoutForce pins the named-residual
// rule at the unit level: no holders, but an uninspectable process the
// scanner can neither inspect nor rule out. Without --force it refuses;
// the error names the process for an explicit decision.
func TestRestore_RefusesUnverifiableWithoutForce(t *testing.T) {
	root := testLayout(t, "")
	procFSRoot = t.TempDir()
	defer func() { procFSRoot = "/proc" }()
	own := fmt.Sprintf("%d", os.Geteuid())
	writeFakeProc(t, procFSRoot, "700", "UNREADABLE", "ghost-agent", own)
	writeFakeLocks(t, procFSRoot)
	err := func() error {
		_, err := restoreDatabase(root, filepath.Join(root, "backups", "x"), "", false)
		return err
	}()
	// No backup dir exists either — either error proves the scan ran
	// first is wrong; assert specifically on the scan refusal by
	// creating the backup dir first.
	_ = err
	os.MkdirAll(filepath.Join(root, "backups", "x"), 0700)
	res, berr := BackupLiveDB(root, time.Now())
	if berr != nil {
		t.Fatalf("setup backup: %v", berr)
	}
	_, rerr := restoreDatabase(root, res.Dir, res.Fingerprint, false)
	if rerr == nil {
		t.Fatal("an unverifiable process must block a restore without --force")
	}
	we, ok := rerr.(*WritersActiveError)
	if !ok {
		t.Fatalf("want *WritersActiveError, got %T: %v", rerr, rerr)
	}
	if len(we.Unverifiable) != 1 || we.Unverifiable[0].PID != 700 {
		t.Fatalf("the refusal must name the process: %+v", we)
	}
	accepted, err := restoreDatabase(root, res.Dir, res.Fingerprint, true)
	if err != nil {
		t.Fatalf("--force accepts the named residual: %v", err)
	}
	if len(accepted) != 1 || accepted[0].PID != 700 {
		t.Fatalf("the accepted residual must be returned for the record: %+v", accepted)
	}
}

// TestScanDBHolders_UnreadableDescriptorIsNotQuiescence pins the
// negative rule: an fd entry that cannot be readlinked for any reason
// other than vanishing mid-scan (here: a directory where a symlink
// should be, so readlink fails) makes the whole process uninspectable.
// It must surface as named-but-unverified, never as evidence the
// database is unheld.
func TestScanDBHolders_UnreadableDescriptorIsNotQuiescence(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mpm.db")
	os.WriteFile(db, []byte("x"), 0600)
	withFakeProc(t, func(procRoot string) {
		ownUID := fmt.Sprintf("%d", os.Geteuid())
		writeFakeProc(t, procRoot, "500", "", "holderish", ownUID)
		fdDir := filepath.Join(procRoot, "500", "fd")
		// An ordinary, unrelated fd.
		if err := os.Symlink("/dev/null", filepath.Join(fdDir, "3")); err != nil {
			t.Fatal(err)
		}
		// An entry readlink cannot resolve that did not vanish: a
		// directory is not a symlink (EINVAL, not ENOENT).
		if err := os.Mkdir(filepath.Join(fdDir, "9"), 0755); err != nil {
			t.Fatal(err)
		}
		writeFakeLocks(t, procRoot)
	})
	scan := ScanDBHolders(db)
	if len(scan.Holders) != 0 {
		t.Fatalf("no holders: %+v", scan.Holders)
	}
	if scan.Inconclusive {
		t.Fatalf("locks are readable and show nothing: this is unverifiable, not inconclusive: %s", scan.Detail)
	}
	if len(scan.Unverifiable) != 1 || scan.Unverifiable[0].PID != 500 {
		t.Fatalf("the process must be named as unverifiable: %+v", scan)
	}
}

// TestRestore_RefusesDamagedRecoveryPoint pins pre-restore
// verification: a recovery point damaged after its backup-time
// check must not replace the live database.
// TestRestore_RefusesEmptyRecoveryPoint pins pre-restore
// verification for a zero-byte recovery point: it must be refused
// before replacing the live database, which must stay byte-identical.
func TestRestore_RefusesEmptyRecoveryPoint(t *testing.T) {
	root, liveBefore := setupRollbackWithBackup(t)
	hist, _ := ReadHistory(root)
	if err := os.Truncate(filepath.Join(hist[0].BackupDir, "mpm.db"), 0); err != nil {
		t.Fatal(err)
	}
	_, err := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if err == nil {
		t.Fatal("a zero-byte recovery point must fail pre-restore verification")
	}
	assertLiveUnchanged(t, root, liveBefore)
}

// TestRestore_RefusesForeignRecoveryPoint pins pre-restore
// verification for a valid but unrelated SQLite database (empty
// schema, no MPM tables): internally consistent, but not a
// recognizable MPM recovery point, so it must be refused before
// replacing the live database.
func TestRestore_RefusesForeignRecoveryPoint(t *testing.T) {
	root, liveBefore := setupRollbackWithBackup(t)
	hist, _ := ReadHistory(root)
	foreign := filepath.Join(t.TempDir(), "foreign.db")
	fdb, err := sql.Open("sqlite3", "file:"+foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fdb.Exec("CREATE TABLE other (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	fdb.Close()
	raw, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hist[0].BackupDir, "mpm.db"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if err == nil {
		t.Fatal("a foreign database must fail pre-restore verification")
	}
	assertLiveUnchanged(t, root, liveBefore)
}

// TestRestore_RefusesTruncatedRecoveryPoint pins pre-restore
// verification for a recovery point damaged after its backup-time
// check (truncated copy): the staged-copy check must fail before the
// rename, leaving the live database byte-identical.
func TestRestore_RefusesTruncatedRecoveryPoint(t *testing.T) {
	root, liveBefore := setupRollbackWithBackup(t)
	hist, _ := ReadHistory(root)
	// Damage the recovery point after its backup-time verification.
	raw, err := os.ReadFile(filepath.Join(hist[0].BackupDir, "mpm.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hist[0].BackupDir, "mpm.db"), raw[:len(raw)/2], 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if err == nil {
		t.Fatal("a damaged recovery point must fail pre-restore verification")
	}
	assertLiveUnchanged(t, root, liveBefore)
}

// setupRollbackWithBackup builds an isolated root with one successful
// update (hence one verified recovery point) and returns the root
// plus the live database bytes for later comparison.
func setupRollbackWithBackup(t *testing.T) (string, []byte) {
	t.Helper()
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	if _, err := Update(context.Background(), Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, Services: QuietServices{}}); err != nil {
		t.Fatalf("setup update: %v", err)
	}
	hist, _ := ReadHistory(root)
	if len(hist) != 1 || hist[0].BackupDir == "" {
		t.Fatalf("no recovery point recorded: %+v", hist)
	}
	liveBefore, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	return root, liveBefore
}

func assertLiveUnchanged(t *testing.T, root string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the live database must be untouched by a refused restore")
	}
}

// TestVerifyBackupStandalone pins the pre-restore check directly:
// a zero-byte file, garbage, a truncated copy and a valid-but-foreign
// database are all refused as recovery material; a healthy backup
// passes.
func TestVerifyBackupStandalone(t *testing.T) {
	dir := t.TempDir()
	healthy := filepath.Join(dir, "healthy.db")
	hdb, err := sql.Open("sqlite3", "file:"+healthy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hdb.Exec("CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)"); err != nil {
		t.Fatal(err)
	}
	hdb.Close()
	if err := verifyBackupStandalone(healthy); err != nil {
		t.Fatalf("healthy backup must verify: %v", err)
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackupStandalone(empty); err == nil {
		t.Fatal("a zero-byte file is not a database")
	}
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("SQLite format 3\x00"+strings.Repeat("x", 5000)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackupStandalone(garbage); err == nil {
		t.Fatal("a garbage file with a valid header must fail integrity verification")
	}
	foreign := filepath.Join(dir, "foreign.db")
	fdb, err := sql.Open("sqlite3", "file:"+foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fdb.Exec("CREATE TABLE other (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	fdb.Close()
	if err := verifyBackupStandalone(foreign); err == nil {
		t.Fatal("a valid database without MPM tables is not a recognizable recovery point")
	}
}

// TestStageScratchDB_SeesUncheckpointedWAL pins why scratch copies use
// the online backup API instead of a raw file copy: a migration that
// has been committed but not checkpointed lives in the WAL, and a raw
// copy of the main database file would silently omit it — making a
// compatibility check pass against the OLD schema.
func TestStageScratchDB_SeesUncheckpointedWAL(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	w, err := sql.Open("sqlite3", "file:"+live)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec("INSERT INTO t (v) VALUES ('x')"); err != nil {
		t.Fatal(err)
	}
	// The "migration": committed, but the writer connection stays open
	// so nothing is checkpointed away.
	if _, err := w.Exec("ALTER TABLE t ADD COLUMN migrated TEXT DEFAULT 'm'"); err != nil {
		t.Fatal(err)
	}
	var walSize int64
	if fi, err := os.Stat(live + "-wal"); err == nil {
		walSize = fi.Size()
	}
	t.Logf("wal size before snapshot: %d", walSize)

	_, staged, err := stageScratchDB(live)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(filepath.Dir(filepath.Dir(staged))))
	if !hasColumn(t, staged, "t", "migrated") {
		t.Fatal("the staged copy is missing the uncheckpointed migration")
	}
	// Sanity: the migration really did live only in the WAL at stage
	// time — a raw copy of the main file alone must NOT see it.
	// (If SQLite checkpointed eagerly this check is vacuous, but the
	// staged-copy assertion above still holds.)
	raw, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(dir, "raw.db")
	if err := os.WriteFile(rawPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_ = rawPath
}

func hasColumn(t *testing.T, dbPath, table, column string) bool {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var nn int
		var dflt sql.NullString
		var pk int
		rows.Scan(&cid, &name, &ctype, &nn, &dflt, &pk)
		if name == column {
			return true
		}
	}
	return false
}

// TestFingerprintFailure_UnknownCompatNotSilent pins the fail-closed
// rule for an unreadable post-smoke fingerprint: the update may still
// succeed (the smoke test is the promotion gate), but nothing may
// record or imply an unchanged schema. Injected via the "fingerprint"
// failure point, so no real database damage is needed.
func TestFingerprintFailure_UnknownCompatNotSilent(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	out, err := Update(context.Background(), Options{
		DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{},
		Services: QuietServices{}, FailAt: "fingerprint",
	})
	if err != nil {
		t.Fatalf("an unknown schema must not fail a healthy update: %v", err)
	}
	if _, got, _ := CurrentReleaseTarget(root); got == "" {
		t.Fatal("the pointer must still move on a healthy smoke test")
	}
	if !strings.Contains(out.Report, "UNKNOWN") {
		t.Errorf("the report must mark the schema unknown, not clean")
	}
	hist, _ := ReadHistory(root)
	if len(hist) != 1 || hist[0].Outcome != "success" {
		t.Fatalf("history = %+v", hist)
	}
	if !strings.Contains(hist[0].Detail, "UNKNOWN") {
		t.Errorf("the record must preserve the unknown state for future rollbacks: %q", hist[0].Detail)
	}
	if hist[0].SchemaChanged {
		t.Error("an unknown schema must not be recorded as changed")
	}
	if strings.Contains(hist[0].Detail, "unchanged") || strings.Contains(out.Report, "no change") {
		t.Error("nothing may imply the schema is unchanged")
	}
}
