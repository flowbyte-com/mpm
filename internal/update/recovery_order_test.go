package update

// Order and recovery-point regressions for the rollback path.
//
// WHAT THESE PIN
// --------------
// Two properties of a database-restoring rollback, both of which are
// about the SEQUENCE rather than about any single step:
//
//  1. THE PAIR MUST BE ALIGNED BEFORE A SERVICE STARTS. A managed unit
//     restarted while the live database has been restored but the
//     active pointer still names the newer release runs the newer
//     binary against the older database — a second outage manufactured
//     by the recovery procedure. So services must stop before any
//     mutation and restart only after the restore AND the pointer swap
//     AND the health check have completed. A failure that leaves those
//     two halves apart must leave the services STOPPED and say so.
//
//  2. A RECOVERY POINT MUST BE RECOGNIZABLE. A structurally valid
//     SQLite database that is not MPM's is not a recovery point.
//     `PRAGMA integrity_check` proves internal consistency and proves
//     nothing about identity: an empty schema reports `ok`. The
//     authoritative identity is the schema fingerprint recorded WITH
//     the backup at backup time, and it is re-checked immediately
//     before the destructive rename.
//
// Every test here runs against a disposable data root. No live service
// is touched: the injected ServiceControl is a recorder, never systemd.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func nowUTC() time.Time { return time.Now().UTC() }

// lastHistoryEntry returns the most recently appended history entry.
func lastHistoryEntry(t *testing.T, root string) (HistoryEntry, bool) {
	t.Helper()
	hist, err := ReadHistory(root)
	if err != nil || len(hist) == 0 {
		return HistoryEntry{}, false
	}
	return hist[len(hist)-1], true
}

// orderLog is the shared, append-only event log. Both the injected
// ServiceControl and the release's fake mpm binary write to it, so one
// ordered sequence covers the service events AND the health check.
type orderLog struct {
	path string
}

func newOrderLog(t *testing.T) *orderLog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "order.log")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MPM_TEST_EVENT_LOG", path)
	return &orderLog{path: path}
}

func (l *orderLog) note(line string) {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func (l *orderLog) lines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (l *orderLog) reset(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(l.path, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func (l *orderLog) indexOf(t *testing.T, prefix string) int {
	t.Helper()
	for i, line := range l.lines(t) {
		if strings.HasPrefix(line, prefix) {
			return i
		}
	}
	return -1
}

// recordedServices is a ServiceControl that records the observable
// world at the instant of every lifecycle event.
type recordedServices struct {
	root  string
	log   *orderLog
	units []string

	mu       sync.Mutex
	stops    int
	starts   int
	stopErr  map[string]error
	startErr map[string]error
	mainPID  func(unit string) (int, error)
}

func newRecordedServices(root string, log *orderLog, units ...string) *recordedServices {
	return &recordedServices{root: root, log: log, units: units}
}

// snapshot writes one line naming the event and the world state.
//
// The database is opened read-only, queried, and CLOSED before this
// returns: an open file description left behind would be seen by the
// restore's own writer scan as a holder, and the test would be
// measuring its own instrumentation rather than the code.
func (r *recordedServices) snapshot(kind, unit string) {
	_, rev, _ := CurrentReleaseTarget(r.root)
	newer := "unknown"
	if db, err := openSQLiteRO(NewLayout(r.root).LiveDB()); err == nil {
		var n int
		if qerr := db.QueryRow("SELECT COUNT(*) FROM memories WHERE content='newer'").Scan(&n); qerr == nil {
			if n > 0 {
				newer = "yes"
			} else {
				newer = "no"
			}
		}
		db.Close()
	}
	r.log.note(fmt.Sprintf("EVENT %s %s pointer=%s newer=%s", kind, unit, rev, newer))
}

func (r *recordedServices) ActiveUnits() []string {
	r.snapshot("identify", "")
	return r.units
}

func (r *recordedServices) Stop(unit string) error {
	r.mu.Lock()
	r.stops++
	r.mu.Unlock()
	r.snapshot("stop", unit)
	return r.stopErr[unit]
}

func (r *recordedServices) Start(unit string) error {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	r.snapshot("start", unit)
	if r.startErr != nil {
		return r.startErr[unit]
	}
	return nil
}

func (r *recordedServices) MainPID(unit string) (int, error) {
	r.snapshot("identity-check", unit)
	if r.mainPID != nil {
		return r.mainPID(unit)
	}
	return 0, nil
}

// instrumentOldRuntime rewrites the old release's mpm binary so its
// health check is visible in the shared event log (and, when healthy is
// false, so it fails).
func instrumentOldRuntime(t *testing.T, root, rev string, healthy bool) {
	t.Helper()
	dir := NewLayout(root).ReleaseDir(rev)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo \"commit: " + rev + "\"; echo \"version: v\"; exit 0; fi\n" +
		"case \"$*\" in\n" +
		"  *health_check*)\n" +
		"    if [ -n \"$MPM_TEST_EVENT_LOG\" ]; then echo 'EVENT health-check  pointer=health newer=health' >> \"$MPM_TEST_EVENT_LOG\"; fi\n"
	if !healthy {
		script += "    echo 'health_check refused by the instrumented runtime' >&2; exit 1\n"
	}
	script += "    echo '{\"ok\":true}';;\n" +
		"  *) echo '{}';;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "bin", "mpm"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

// rollbackFixture performs a real update on a disposable data root and
// then writes a row the pre-update backup does not contain, so the
// database restore is observable from the service events.
func rollbackFixture(t *testing.T) (root, oldRev, newRev string) {
	t.Helper()
	oldRev = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root = testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	if _, err := Update(context.Background(), Options{
		DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, Services: QuietServices{},
	}); err != nil {
		t.Fatalf("setup update: %v", err)
	}
	_, newRev, _ = CurrentReleaseTarget(root)
	db := openTestDB(t, NewLayout(root).LiveDB())
	if _, err := db.Exec("INSERT INTO memories (id, content) VALUES ('m2','newer')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	return root, oldRev, newRev
}

// TestRollback_ServiceOrderOnSuccess pins the required ORDER of a
// successful database-restoring rollback, observed from the events
// themselves rather than inferred from the end state:
//
//	identify active services
//	stop authorized services
//	verify recovery database      (between stop and health-check)
//	restore database              (between stop and health-check)
//	activate intended runtime     (between stop and health-check)
//	perform required health checks
//	restart previously active services
//	verify executable identity
func TestRollback_ServiceOrderOnSuccess(t *testing.T) {
	root, oldRev, newRev := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler")
	sc.mainPID = func(string) (int, error) { return 0, nil }

	out, err := Rollback(Options{
		DataRoot: root, WithDBRestore: true, Force: true,
		RestartServices: true, Services: sc,
	})
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, out.Report)
	}

	lines := log.lines(t)
	for _, line := range lines {
		t.Logf("  %s", line)
	}

	identify := log.indexOf(t, "EVENT identify")
	stop := log.indexOf(t, "EVENT stop mpm-scheduler")
	health := log.indexOf(t, "EVENT health-check")
	start := log.indexOf(t, "EVENT start mpm-scheduler")
	identity := log.indexOf(t, "EVENT identity-check mpm-scheduler")
	if identify < 0 || stop < 0 || health < 0 || start < 0 || identity < 0 {
		t.Fatalf("a required event is missing: identify=%d stop=%d health=%d start=%d identity=%d\n%s",
			identify, stop, health, start, identity, strings.Join(lines, "\n"))
	}
	if !(identify < stop) {
		t.Errorf("services must be IDENTIFIED before they are stopped: identify=%d stop=%d", identify, stop)
	}
	if !(stop < health) {
		t.Errorf("the health check must run AFTER the stop and the mutations, not before: stop=%d health=%d", stop, health)
	}
	if !(health < start) {
		t.Errorf("services must restart only AFTER the health check: health=%d start=%d", health, start)
	}
	if !(start < identity) {
		t.Errorf("executable identity must be verified AFTER the restart: start=%d identity=%d", start, identity)
	}

	// The world state at each service event: stopped while the newer
	// release still owns the database, started only once the OLD
	// release owns the RESTORED database.
	stopLine := lines[stop]
	if !strings.Contains(stopLine, "pointer="+newRev) || !strings.Contains(stopLine, "newer=yes") {
		t.Errorf("the stop must happen while the newer release still owns the un-restored database: %s", stopLine)
	}
	startLine := lines[start]
	if !strings.Contains(startLine, "pointer="+oldRev) {
		t.Errorf("the service must not start before the pointer names the rolled-back release: %s", startLine)
	}
	if !strings.Contains(startLine, "newer=no") {
		t.Errorf("the service must not start before the database has been restored: %s", startLine)
	}
}

// TestRollback_RepointFailureKeepsServicesStopped is the partial-failure
// regression: the restore commits, the pointer swap does not. The two
// halves of the installation are now apart, so NOTHING may be started —
// a daemon started here would run the newer release against the older
// database.
func TestRollback_RepointFailureKeepsServicesStopped(t *testing.T) {
	root, oldRev, newRev := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler")
	out, err := Rollback(Options{
		DataRoot: root, WithDBRestore: true, Force: true,
		RestartServices: true, Services: sc, FailAt: "restore",
	})
	if err == nil {
		t.Fatalf("an injected failure after the restore must be reported\n%s", out.Report)
	}
	if got := log.indexOf(t, "EVENT start"); got >= 0 {
		t.Errorf("NO service may start while the restored database and the active release disagree; events:\n%s",
			strings.Join(log.lines(t), "\n"))
	}
	if !strings.Contains(err.Error(), "restored") || !strings.Contains(strings.ToLower(err.Error()), "stopped") {
		t.Errorf("the failure must state that the database was restored and the services were left stopped: %v", err)
	}

	// The mismatch is real and must be visible: the database is the
	// restored one, the pointer still names the release that was
	// active. The report must not claim success.
	_, got, _ := CurrentReleaseTarget(root)
	if got != newRev {
		t.Errorf("the pointer must be unchanged by a failed repoint: got %q want %q", got, newRev)
	}
	if _, err := os.Stat(NewLayout(root).LiveDB()); err != nil {
		t.Fatal(err)
	}
	if out.Report != "" && strings.Contains(out.Report, "rolled back") {
		t.Errorf("a failed rollback must not report a completed rollback:\n%s", out.Report)
	}
	last, ok := lastHistoryEntry(t, root)
	if !ok || last.Outcome != "failed" {
		t.Fatalf("the partial failure must be recorded as failed: %+v", last)
	}
	if !strings.Contains(last.Detail, "stopped") {
		t.Errorf("the recorded detail must name the stopped services: %q", last.Detail)
	}
}

// TestRollback_HealthFailureKeepsServicesStopped pins the other half of
// the same rule: the restored runtime failed its health check, so the
// runtime/database pair is aligned on paper but not known-good. The
// required sequence puts the health check BEFORE the restart, so a
// failure there must not restart anything.
func TestRollback_HealthFailureKeepsServicesStopped(t *testing.T) {
	root, oldRev, _ := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, false)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler")
	out, err := Rollback(Options{
		DataRoot: root, WithDBRestore: true, Force: true,
		RestartServices: true, Services: sc,
	})
	if err == nil {
		t.Fatalf("a failed health check must fail the rollback\n%s", out.Report)
	}
	if got := log.indexOf(t, "EVENT start"); got >= 0 {
		t.Errorf("a service must not restart after a FAILED health check; events:\n%s",
			strings.Join(log.lines(t), "\n"))
	}
	if !strings.Contains(strings.ToLower(err.Error()), "stopped") {
		t.Errorf("the error must say the services were left stopped: %v", err)
	}
}

// TestRollback_StopFailureStartsBackOnlyWhatWasStopped pins the
// stop-failure path: nothing was mutated, and the units already stopped
// are started back so a failed attempt never leaves services silently
// down. A unit whose stop FAILED is not started (it was never stopped).
func TestRollback_StopFailureStartsBackOnlyWhatWasStopped(t *testing.T) {
	root, oldRev, newRev := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler", "mpm-telemetry")
	sc.stopErr = map[string]error{"mpm-telemetry": fmt.Errorf("unit is masked")}
	before, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}

	_, rerr := Rollback(Options{
		DataRoot: root, WithDBRestore: true, Force: true,
		RestartServices: true, Services: sc,
	})
	if rerr == nil {
		t.Fatal("a failing stop must abort the rollback")
	}
	if sc.stops != 2 {
		t.Errorf("both units must be attempted: stops=%d", sc.stops)
	}
	if sc.starts != 1 {
		t.Errorf("only the unit that was stopped may be started back: starts=%d events=%v", sc.starts, log.lines(t))
	}
	if got := log.indexOf(t, "EVENT start mpm-telemetry"); got >= 0 {
		t.Errorf("a unit whose stop FAILED must not be started")
	}
	if got := log.indexOf(t, "EVENT health-check"); got >= 0 {
		t.Errorf("nothing may be verified after a failed stop")
	}
	if _, got, _ := CurrentReleaseTarget(root); got != newRev {
		t.Errorf("the pointer must be untouched by a failed stop: %q", got)
	}
	assertLiveUnchanged(t, root, before)
}

// TestRollback_StartFailureIsReported pins honest partial-success
// reporting: the pointer and the database are already correct, so the
// rollback stands — but a restart that failed is reported and recorded,
// never silently swallowed.
func TestRollback_StartFailureIsReported(t *testing.T) {
	root, oldRev, _ := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler")
	sc.startErr = map[string]error{"mpm-scheduler": fmt.Errorf("unit failed to start")}

	out, err := Rollback(Options{
		DataRoot: root, WithDBRestore: true, Force: true,
		RestartServices: true, Services: sc,
	})
	if err != nil {
		t.Fatalf("a restart failure must not undo a completed rollback: %v", err)
	}
	if !strings.Contains(out.Report, "verification warnings") && !strings.Contains(out.Report, "warning:") {
		t.Errorf("the restart failure must be reported in the outcome:\n%s", out.Report)
	}
	last, ok := lastHistoryEntry(t, root)
	if !ok || last.Outcome != "success" {
		t.Fatalf("the rollback itself succeeded: %+v", last)
	}
	if !strings.Contains(last.Detail, "mpm-scheduler") {
		t.Errorf("the recorded detail must name the unit that failed to restart: %q", last.Detail)
	}
}

// TestRollback_IdentityMismatchIsReported pins that a restarted unit
// whose executable is NOT the intended release is reported, not
// assumed. The test's own process stands in for the daemon: its
// executable resolves outside the release directory.
func TestRollback_IdentityMismatchIsReported(t *testing.T) {
	root, oldRev, _ := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler")
	sc.mainPID = func(string) (int, error) { return os.Getpid(), nil }

	out, err := Rollback(Options{
		DataRoot: root, WithDBRestore: true, Force: true,
		RestartServices: true, Services: sc,
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !strings.Contains(out.Report, "runs") || !strings.Contains(out.Report, "not the active release") {
		t.Errorf("an executable outside the intended release must be reported:\n%s", out.Report)
	}
}

// TestRollback_RuntimeOnlyUsesTheSameOrder pins the documented
// runtime-only form (no --with-db-restore) with --restart-services: the
// same stop-before-mutation / restart-after-verification order, and the
// database is never replaced.
func TestRollback_RuntimeOnlyUsesTheSameOrder(t *testing.T) {
	root, oldRev, newRev := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler")
	if _, err := Rollback(Options{DataRoot: root, RestartServices: true, Services: sc}); err != nil {
		t.Fatalf("runtime-only rollback: %v", err)
	}
	lines := log.lines(t)
	stop := log.indexOf(t, "EVENT stop mpm-scheduler")
	health := log.indexOf(t, "EVENT health-check")
	start := log.indexOf(t, "EVENT start mpm-scheduler")
	if stop < 0 || health < 0 || start < 0 {
		t.Fatalf("a required event is missing:\n%s", strings.Join(lines, "\n"))
	}
	if !(stop < health && health < start) {
		t.Errorf("runtime-only rollback must use the same order (stop < health < start): stop=%d health=%d start=%d\n%s",
			stop, health, start, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[stop], "pointer="+newRev) {
		t.Errorf("the stop must precede the pointer swap: %s", lines[stop])
	}
	if !strings.Contains(lines[start], "pointer="+oldRev) {
		t.Errorf("the restart must follow the pointer swap: %s", lines[start])
	}
	// Runtime-only: the newer row is still there. The database was not
	// replaced — that is the whole difference from --with-db-restore.
	if !strings.Contains(lines[start], "newer=yes") {
		t.Errorf("a runtime-only rollback must not replace the database: %s", lines[start])
	}
}

// TestRollback_WithoutAuthorizationTouchesNoService preserves the
// default: without --restart-services the updater reports and never
// stops or starts anything.
func TestRollback_WithoutAuthorizationTouchesNoService(t *testing.T) {
	root, oldRev, _ := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	sc := newRecordedServices(root, log, "mpm-scheduler")
	out, err := Rollback(Options{DataRoot: root, RestartServices: false, Services: sc})
	if err != nil {
		t.Fatalf("runtime-only rollback without service authorization: %v", err)
	}
	if sc.stops != 0 || sc.starts != 0 {
		t.Errorf("no service may be touched without --restart-services: stops=%d starts=%d", sc.stops, sc.starts)
	}
	if !strings.Contains(out.Report, "NOT restarted") {
		t.Errorf("the report must say the running service was not restarted:\n%s", out.Report)
	}
}

// TestRollback_DBRestoreRefusesUnderActiveServicesWithoutAuthorization
// pins the refusal: restoring under a running daemon corrupts, so a
// database restore with active units and no --restart-services stops
// before it does anything at all.
func TestRollback_DBRestoreRefusesUnderActiveServicesWithoutAuthorization(t *testing.T) {
	root, oldRev, newRev := rollbackFixture(t)
	log := newOrderLog(t)
	instrumentOldRuntime(t, root, oldRev, true)
	log.reset(t)

	before, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	sc := newRecordedServices(root, log, "mpm-scheduler")
	_, rerr := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: sc})
	if rerr == nil {
		t.Fatal("a database restore with an active managed service must be refused without --restart-services")
	}
	if sc.stops != 0 || sc.starts != 0 {
		t.Errorf("a refusal must not touch services: stops=%d starts=%d", sc.stops, sc.starts)
	}
	assertLiveUnchanged(t, root, before)
	if _, got, _ := CurrentReleaseTarget(root); got != newRev {
		t.Errorf("a refusal must not move the pointer: %q", got)
	}
}

// TestRestore_ReVerifiesTheRecordedFingerprintImmediatelyBeforeTheRename
// pins the LAST line of defence against a recovery point that is not
// the database that was backed up. The check runs on the staged copy,
// after the writer scan and immediately before the destructive rename,
// so nothing that changed after the backup-time check can be installed
// by accident.
func TestRestore_ReVerifiesTheRecordedFingerprintImmediatelyBeforeTheRename(t *testing.T) {
	root := testLayout(t, "")
	procFSRoot = t.TempDir()
	defer func() { procFSRoot = "/proc" }()
	writeFakeLocks(t, procFSRoot)

	res, err := BackupLiveDB(root, time.Now())
	if err != nil {
		t.Fatalf("setup backup: %v", err)
	}
	before, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}

	// A recovery point whose shape is not the recorded pre-update shape
	// is refused, and the live database is byte-identical afterwards.
	if _, err := restoreDatabase(root, res.Dir, "sha256:deadbeef", false); err == nil {
		t.Fatal("a recovery point that does not match the recorded fingerprint must be refused before the rename")
	}
	assertLiveUnchanged(t, root, before)

	// No recorded fingerprint is a refusal, not a skip.
	if _, err := restoreDatabase(root, res.Dir, "", false); err == nil {
		t.Fatal("a recovery point with no recorded fingerprint must be refused before the rename")
	}
	assertLiveUnchanged(t, root, before)

	// The correctly-fingerprinted recovery point is the one that is
	// installed: the refusal above was about identity, not about
	// refusing to restore at all.
	if _, err := restoreDatabase(root, res.Dir, res.Fingerprint, false); err != nil {
		t.Fatalf("the correctly-fingerprinted recovery point must restore: %v", err)
	}
}

// --- recovery-point identity ----------------------------------------------------

// fakeRecoveryPoint replaces the recorded backup's database with the
// given bytes and records a history entry for it, so a rollback will
// select it as the recovery point.
func fakeRecoveryPoint(t *testing.T, root, fromRev string, db []byte, preFP string) {
	t.Helper()
	dir := filepath.Join(root, "backups", "synthetic-recovery")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mpm.db"), db, 0600); err != nil {
		t.Fatal(err)
	}
	if err := AppendHistory(root, HistoryEntry{
		TS:        nowUTC(),
		Action:    "update",
		FromRev:   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ToRev:     fromRev,
		Outcome:   "failed",
		BackupDir: dir,
		PreFP:     preFP,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRollback_RefusesRecoveryPointWithoutRecordedFingerprint pins the
// fail-closed rule for legacy recovery metadata. The entry names a
// backup but records no pre-update schema fingerprint, so MPM cannot
// prove the file is the database it backed up. A structurally valid
// but unrelated database must not become the live database.
func TestRollback_RefusesRecoveryPointWithoutRecordedFingerprint(t *testing.T) {
	root, _, newRev := rollbackFixture(t)
	foreign := filepath.Join(t.TempDir(), "foreign.db")
	mkDB(t, foreign, map[string]string{
		"memories": "CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)",
	})
	raw, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	fakeRecoveryPoint(t, root, newRev, raw, "")

	before, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if rerr == nil {
		t.Fatal("a recovery point with no recorded fingerprint must be refused, not restored")
	}
	if !strings.Contains(strings.ToLower(rerr.Error()), "fingerprint") {
		t.Errorf("the refusal must name the missing fingerprint: %v", rerr)
	}
	assertLiveUnchanged(t, root, before)
	if _, got, _ := CurrentReleaseTarget(root); got != newRev {
		t.Errorf("the pointer must be untouched by a refused restore: %q", got)
	}
}

// TestRollback_RefusesZeroByteRecoveryPoint pins the degenerate case: a
// zero-byte file is not a database at all, and `integrity_check` cannot
// even run on it.
func TestRollback_RefusesZeroByteRecoveryPoint(t *testing.T) {
	root, _, newRev := rollbackFixture(t)
	fakeRecoveryPoint(t, root, newRev, nil, "")

	before, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}}); rerr == nil {
		t.Fatal("a zero-byte recovery point must be refused")
	}
	assertLiveUnchanged(t, root, before)
	if _, got, _ := CurrentReleaseTarget(root); got != newRev {
		t.Errorf("the pointer must be untouched by a refused restore: %q", got)
	}
}

// TestRollback_RefusesFingerprintMismatchedRecoveryPoint is the
// regression guard for the binding that already exists: a recovery
// point whose shape does not match the fingerprint recorded for it has
// been replaced or damaged since, and is refused.
func TestRollback_RefusesFingerprintMismatchedRecoveryPoint(t *testing.T) {
	root, _, newRev := rollbackFixture(t)
	foreign := filepath.Join(t.TempDir(), "foreign.db")
	mkDB(t, foreign, map[string]string{
		"memories": "CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)",
	})
	raw, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	fakeRecoveryPoint(t, root, newRev, raw, "sha256:0000000000000000000000000000000000000000000000000000000000000000")

	before, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if rerr == nil {
		t.Fatal("a recovery point whose shape does not match its recorded fingerprint must be refused")
	}
	assertLiveUnchanged(t, root, before)
	if _, got, _ := CurrentReleaseTarget(root); got != newRev {
		t.Errorf("the pointer must be untouched by a refused restore: %q", got)
	}
}

// TestVerifyBackupStandalone_RejectsForeignShape pins the recognizable-
// MPM rule directly: a database whose `memories` table is not MPM's
// shape is refused, however tidy its schema is.
func TestVerifyBackupStandalone_RejectsForeignShape(t *testing.T) {
	cases := map[string]string{
		"a lone id column":    "CREATE TABLE memories (id TEXT PRIMARY KEY)",
		"an unrelated column": "CREATE TABLE memories (payload BLOB)",
	}
	for name, ddl := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			db, err := sql.Open("sqlite3", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ddl); err != nil {
				db.Close()
				t.Fatal(err)
			}
			db.Close()
			if err := verifyBackupStandalone(path); err == nil {
				t.Fatalf("a memories table without MPM's columns must not be a recognizable MPM database (%s)", ddl)
			}
		})
	}
}
