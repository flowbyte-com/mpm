package update

// Regression suite for the read-only open path's busy timeout.
//
// THE DEFECT THIS SUITE PINS
// ---------------------------
//
// `openSQLiteRO` used to open the database with `sql.Open` and then run
// `PRAGMA busy_timeout = 10000`. `sql.Open` is LAZY — it opens no
// connection — so that PRAGMA was the FIRST DATABASE OPERATION on the
// connection. The driver installs its own default busy timeout of
// 5000 ms during the connection open (mattn/go-sqlite3 v1.14.37,
// sqlite3.go:1098 and :1491), and the first open-path statement that
// READS the database (`PRAGMA synchronous = NORMAL`, sqlite3.go:1746)
// runs after it. The ten seconds the caller asked for were therefore
// never in force for the operation that needed them: a writer holding
// the lock for more than five seconds made the read fail with
// "database is locked" — after five seconds, not ten.
//
// The observed CI failure was exactly that, reported by the backup's
// self-verification:
//
//	backup self-verification failed: could not list the live tables: database is locked
//
// The correction puts the timeout in the DSN, so the driver installs it
// DURING the connection open and it is in force for the very first
// operation.
//
// DETERMINISTIC PROOF vs PROBABILISTIC TRIAL
// --------------------------------------------
//
// Every test below that contends for the lock is DETERMINISTIC: the
// lock is acquired by this process, the window is controlled, and the
// outcome follows from which timeout is in force — not from how the
// scheduler happened to interleave two goroutines. The two window
// lengths are chosen FAR APART on purpose:
//
//   - the driver's default is 5000 ms;
//   - the requested timeout is 10000 ms;
//
// A lock held for 7000 ms is therefore beyond the default and within
// the request, with a margin of 2 s on one side and 3 s on the other.
// The assertions do not depend on the exact moment the lock is
// released; they depend only on which side of those two thresholds
// the release falls.
//
// `TestBackupLiveDB_StressesConcurrentWriters` is the one PROBABILISTIC
// trial in this file, and it is labelled as such: it cannot prove the
// correction, it can only fail to disprove it under load.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

// lockWindow is how long the contended tests hold the EXCLUSIVE lock
// before releasing it. It sits between the driver's 5000 ms default
// and the 10000 ms the update path asks for.
const lockWindow = 7 * time.Second

// --- the lock holder -------------------------------------------------------

// exclusiveLock holds SQLite's EXCLUSIVE lock on a database until it is
// released.
//
// BEGIN EXCLUSIVE takes the lock immediately in DELETE journal mode —
// the mode the update path's fixture databases use — so a reader's
// SHARED request must wait behind it. The write that follows makes the
// lock unambiguous and leaves a hot journal behind, exactly as a
// crashed or busy writer would.
type exclusiveLock struct {
	db   *sql.DB
	once sync.Once
}

// holdExclusiveLock takes and holds the EXCLUSIVE lock on path.
func holdExclusiveLock(t *testing.T, path string) *exclusiveLock {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("could not open the lock holder: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("BEGIN EXCLUSIVE"); err != nil {
		db.Close()
		t.Fatalf("could not take the exclusive lock: %v", err)
	}
	if _, err := db.Exec("UPDATE memories SET content = 'held' WHERE id = 'm1'"); err != nil {
		db.Close()
		t.Fatalf("could not write under the exclusive lock: %v", err)
	}
	l := &exclusiveLock{db: db}
	t.Cleanup(l.release)
	return l
}

// release drops the lock and closes the holder's connection.
func (l *exclusiveLock) release() {
	l.once.Do(func() {
		l.db.Exec("ROLLBACK") //nolint:errcheck
		l.db.Close()
	})
}

// releaseAfter drops the lock once d has elapsed.
func (l *exclusiveLock) releaseAfter(d time.Duration) {
	go func() {
		time.Sleep(d)
		l.release()
	}()
}

// --- helpers ----------------------------------------------------------------

// connectionBusyTimeout reads the busy timeout the connection carries.
func connectionBusyTimeout(t *testing.T, db *sql.DB) time.Duration {
	t.Helper()
	var ms int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&ms); err != nil {
		t.Fatalf("could not read the connection's busy timeout: %v", err)
	}
	return time.Duration(ms) * time.Millisecond
}

// isBusy reports whether err is SQLite's "the database is locked".
func isBusy(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.Code == sqlite3.ErrBusy
	}
	return false
}

// openFDsTo returns the open file descriptors of this process that
// point at path.
//
// The update path opens the live database and every copy it makes; a
// handle left open after a backup is a handle the operator's writer
// scan will see, and a handle left open after a FAILED open is a leak.
func openFDsTo(path string) []string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	want := filepath.Clean(path)
	var out []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		target = strings.TrimSuffix(target, " (deleted)")
		if filepath.Clean(target) == want {
			out = append(out, target)
		}
	}
	return out
}

// --- the defect: the timeout that governs the first operation --------------

// TestOpenSQLiteRO_FirstOperationHonoursTheConnectionsBusyTimeout is the
// deterministic proof of the defect and of its correction.
//
// The connection is opened once, uncontended, and asked what busy
// timeout it carries. The database is then locked and the open is
// repeated: the second open's FIRST database operation must be given
// the whole timeout the connection claims to carry.
//
// Before the correction the connection claimed 10000 ms and the first
// operation gave up after 5000 ms — the requested timeout was not in
// force for the operation that needed it.
func TestOpenSQLiteRO_FirstOperationHonoursTheConnectionsBusyTimeout(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()

	probe, err := openSQLiteRO(live)
	if err != nil {
		t.Fatalf("the uncontended open failed: %v", err)
	}
	want := connectionBusyTimeout(t, probe)
	probe.Close()
	if want <= 0 {
		t.Fatalf("the read-only connection must carry a busy timeout, got %s", want)
	}
	t.Logf("the connection carries a %s busy timeout", want)

	// The lock is held for the whole window: the open must wait the
	// timeout out and then fail, bounded.
	holdExclusiveLock(t, live)

	start := time.Now()
	db, err := openSQLiteRO(live)
	elapsed := time.Since(start)
	if err == nil {
		db.Close()
		t.Fatal("the open must fail while the writer holds the lock")
	}
	t.Logf("the open failed after %v: %v", elapsed, err)

	// The requested timeout must be IN FORCE for the first operation.
	// One second of slack absorbs the measurement, not the outcome:
	// the driver's own default is half the requested timeout, so a
	// five-second wait cannot be mistaken for a ten-second one.
	if elapsed+time.Second < want {
		t.Fatalf("the connection carries a %s busy timeout, but the first database operation gave up after %v: the timeout is not in force for the first access", want, elapsed)
	}
	// The failure must stay BOUNDED by that timeout.
	if elapsed > want+5*time.Second {
		t.Fatalf("the failure must stay bounded by the %s timeout, took %v", want, elapsed)
	}
	// And it must be a classified refusal, not a silent success.
	if !isBusy(err) {
		t.Fatalf("the failure must be classified as SQLITE_BUSY, got %T: %v", err, err)
	}
}

// TestOpenSQLiteRO_ToleratesALockReleasedWithinTheTimeout proves the
// other half of the contract: a lock that is released WITHIN the
// timeout must be waited out, not failed on.
//
// The lock is held for 7 s — beyond the driver's 5000 ms default and
// within the 10000 ms the update path asks for. Before the correction
// this failed after 5 s; it now succeeds once the lock is released.
func TestOpenSQLiteRO_ToleratesALockReleasedWithinTheTimeout(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()

	lock := holdExclusiveLock(t, live)
	lock.releaseAfter(lockWindow)

	start := time.Now()
	db, err := openSQLiteRO(live)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a lock released after %s must be waited out, not failed on: the open failed after %v: %v", lockWindow, elapsed, err)
	}
	defer db.Close()
	// The reader really did wait: the lock was held across the first
	// access, so the open cannot have completed before it was released.
	if elapsed < lockWindow-time.Second {
		t.Fatalf("the open returned after %v, before the lock was released: the contention was not exercised", elapsed)
	}
	t.Logf("the open succeeded after %v, once the lock was released", elapsed)
}

// TestVerifyBackup_SurvivesALockHeldAcrossTheLiveTableListing is the
// OBSERVED CI FAILURE, reproduced deterministically.
//
// The backup's self-verification lists the LIVE tables, which is the
// read that failed in CI:
//
//	backup self-verification failed: could not list the live tables: database is locked
func TestVerifyBackup_SurvivesALockHeldAcrossTheLiveTableListing(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()

	res, err := BackupLiveDB(root, time.Now())
	if err != nil {
		t.Fatalf("the uncontended backup failed: %v", err)
	}

	lock := holdExclusiveLock(t, live)
	lock.releaseAfter(lockWindow)

	start := time.Now()
	err = verifyBackup(live, res.DBPath)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the backup's self-verification must survive a writer that holds the lock across the live table listing: %v (after %v)", err, elapsed)
	}
	t.Logf("the self-verification completed after %v, once the lock was released", elapsed)
}

// TestBackupLiveDB_SurvivesALockHeldAcrossTheFirstAccess is the
// end-to-end form: the whole backup, taken while a writer holds the
// lock across the first access to the live database.
func TestBackupLiveDB_SurvivesALockHeldAcrossTheFirstAccess(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()

	lock := holdExclusiveLock(t, live)
	lock.releaseAfter(lockWindow)

	start := time.Now()
	res, err := BackupLiveDB(root, time.Now())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the backup must survive a writer that holds the lock across the first access: %v (after %v)", err, elapsed)
	}
	defer os.RemoveAll(res.Dir)
	t.Logf("the backup completed after %v", elapsed)

	// The recovery point it produced must still verify, against itself.
	if err := verifyBackup(live, res.DBPath); err != nil {
		t.Fatalf("the backup did not verify: %v", err)
	}
	if err := verifyRecoveryPoint(res.DBPath, res.Fingerprint); err != nil {
		t.Fatalf("the recovery point did not verify against its recorded fingerprint: %v", err)
	}
}

// --- the read-only guarantee ------------------------------------------------

// TestOpenSQLiteRO_OpensTheExactPathReadOnly proves the open reaches
// the RIGHT file and opens it READ-ONLY.
//
// The path deliberately contains the three characters that delimit a
// SQLite URI filename: `?` ends the path and starts the query, `#`
// ends the path and starts the fragment, and `%` starts a percent
// escape of its own. An unescaped path silently loses `mode=ro` — the
// live database would then be opened WRITABLE — and loses the busy
// timeout with it.
func TestOpenSQLiteRO_OpensTheExactPathReadOnly(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.db")
	mkDB(t, plain, map[string]string{
		"memories": "CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)",
	})
	// Renamed rather than created in place: a fixture created under a
	// URI-hostile name would itself have to be escaped to land there.
	weird := filepath.Join(dir, "we?ird#name%25.db")
	if err := os.Rename(plain, weird); err != nil {
		t.Fatal(err)
	}

	db, err := openSQLiteRO(weird)
	if err != nil {
		t.Fatalf("the open failed: %v", err)
	}
	defer db.Close()

	var seq int
	var name, file string
	rows, err := db.Query("PRAGMA database_list")
	if err != nil {
		t.Fatalf("could not read the database list: %v", err)
	}
	for rows.Next() {
		if err := rows.Scan(&seq, &name, &file); err != nil {
			rows.Close()
			t.Fatal(err)
		}
	}
	rows.Close()
	if file != weird {
		t.Fatalf("opened the wrong database: %q, want %q", file, weird)
	}

	// The live database must be READ-ONLY.
	if _, err := db.Exec("INSERT INTO memories (id, content) VALUES ('x','y')"); err == nil {
		t.Fatal("the live database must be opened read-only")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("the write must be refused as read-only, got: %v", err)
	}

	// And it must be readable.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		t.Fatalf("the database must be readable: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the seeded row, got %d", n)
	}
}

// TestOpenSQLiteRO_LimitsThePoolAndClosesWhatItOpens pins the pool
// limits and the cleanup.
func TestOpenSQLiteRO_LimitsThePoolAndClosesWhatItOpens(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()

	db, err := openSQLiteRO(live)
	if err != nil {
		t.Fatalf("the open failed: %v", err)
	}
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("the read-only pool must be limited to one connection, got %d", got)
	}
	if got := db.Stats().OpenConnections; got != 1 {
		t.Fatalf("the open must establish exactly one connection, got %d", got)
	}
	db.Close()
	if got := db.Stats().OpenConnections; got != 0 {
		t.Fatalf("closing the pool must close its connection, %d remain", got)
	}
	if open := openFDsTo(live); len(open) != 0 {
		t.Fatalf("the closed pool left %d open handle(s) on the database", len(open))
	}
}

// --- missing and corrupt databases ------------------------------------------

// TestOpenSQLiteRO_RefusesAMissingDatabase proves a missing database
// is refused, and that a read-only open does not CREATE it.
func TestOpenSQLiteRO_RefusesAMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	db, err := openSQLiteRO(path)
	if err == nil {
		db.Close()
		t.Fatal("a missing database must be refused")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("a read-only open must not create the database it could not find")
	}
	if open := openFDsTo(path); len(open) != 0 {
		t.Fatalf("the refused open left %d open handle(s) behind", len(open))
	}
}

// TestOpenSQLiteRO_RefusesACorruptDatabase proves a corrupt database
// is refused at the open, and leaves no handle behind.
func TestOpenSQLiteRO_RefusesACorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("this is not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLiteRO(path)
	if err == nil {
		db.Close()
		t.Fatal("a corrupt database must be refused")
	}
	if open := openFDsTo(path); len(open) != 0 {
		t.Fatalf("the refused open left %d open handle(s) behind", len(open))
	}
}

// TestBackupLiveDB_RefusesACorruptLiveDatabase proves the backup path
// refuses a corrupt live database and records NO recovery point: a
// failed backup must never be advertised as one.
func TestBackupLiveDB_RefusesACorruptLiveDatabase(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()
	if err := os.WriteFile(live, []byte("this is not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupLiveDB(root, time.Now()); err == nil {
		t.Fatal("a backup of a corrupt live database must fail")
	}
	if _, err := os.Stat(NewLayout(root).LastBackup()); err == nil {
		t.Fatal("a failed backup must not record a recovery point")
	}
}

// --- the backup's own guarantees --------------------------------------------

// TestVerifyBackup_RejectsABackupMissingATable proves the backup is
// not accepted merely because it is a tidy SQLite file: a backup that
// does not carry the live database's tables is not a backup.
func TestVerifyBackup_RejectsABackupMissingATable(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()
	res, err := BackupLiveDB(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(res.Dir)

	// A copy that is a valid database, carries MPM's shape, but has an
	// extra table: the live database's tables and the backup's tables
	// must match exactly.
	other := filepath.Join(t.TempDir(), "other.db")
	odb, err := sql.Open("sqlite3", "file:"+other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := odb.Exec(`CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := odb.Exec(`CREATE TABLE extra (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := odb.Exec(`INSERT INTO memories (id, content) VALUES ('m1','hello')`); err != nil {
		t.Fatal(err)
	}
	odb.Close()

	if err := verifyBackup(live, other); err == nil {
		t.Fatal("a backup that does not carry the live tables must be refused")
	} else if !strings.Contains(err.Error(), "do not match live tables") {
		t.Fatalf("the refusal must name the table mismatch, got: %v", err)
	}
}

// TestBackupLiveDB_LeavesNoDatabaseHandleOpen proves the backup path
// closes every database it opens: a handle left open on the live
// database is a handle the operator's writer scan will see.
func TestBackupLiveDB_LeavesNoDatabaseHandleOpen(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()
	res, err := BackupLiveDB(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(res.Dir)
	if open := openFDsTo(live); len(open) != 0 {
		t.Fatalf("the backup left %d open handle(s) on the live database", len(open))
	}
	if open := openFDsTo(res.DBPath); len(open) != 0 {
		t.Fatalf("the backup left %d open handle(s) on the recovery point", len(open))
	}
}

// --- cancellation ------------------------------------------------------------

// TestOpenSQLiteRO_CancellationDoesNotPoisonThePool covers the
// cancellation surface that exists.
//
// The update path's database helpers take no context, so the backup
// path itself has no cancellation to honour. The pool this function
// returns DOES take one at the statement level, and a cancelled
// statement must fail fast and leave the pool usable.
func TestOpenSQLiteRO_CancellationDoesNotPoisonThePool(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()

	db, err := openSQLiteRO(live)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var n int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories").Scan(&n)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled statement must fail with context.Canceled, got %v", err)
	}
	// The pool must still work afterwards.
	if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		t.Fatalf("the pool must survive a cancelled statement: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the seeded row, got %d", n)
	}
}

// --- the probabilistic trial ------------------------------------------------

// TestBackupLiveDB_StressesConcurrentWriters is a PROBABILISTIC TRIAL,
// not a deterministic proof.
//
// It cannot establish that the correction works; it can only fail to
// disprove it under load. Several writers insert continuously while
// several backups are taken and verified, and EVERY backup must
// succeed — a backup that is silently skipped, or one that verifies
// against a half-written copy, is a failure.
//
// The deterministic proofs are the four contention tests above.
func TestBackupLiveDB_StressesConcurrentWriters(t *testing.T) {
	root := testLayout(t, "")
	live := NewLayout(root).LiveDB()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopWriters := func() { stopOnce.Do(func() { close(stop) }) }
	defer func() { stopWriters(); wg.Wait() }()

	const writers = 3
	// Every writer announces its first successful insert, so the trial
	// starts with three writers already contending — not with a
	// race between the writers and the first backup.
	ready := make(chan struct{}, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			db, err := sql.Open("sqlite3", "file:"+live)
			if err != nil {
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			i := 0
			announced := false
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := db.Exec(fmt.Sprintf("INSERT INTO memories (id, content) VALUES ('s-%d-%d', 'x')", id, i)); err == nil {
					i++
					if !announced {
						announced = true
						ready <- struct{}{}
					}
				}
				// A writer that yields between statements is what a
				// real writer looks like; the lock is still taken
				// and released continuously.
				time.Sleep(time.Millisecond)
			}
		}(w)
	}
	for w := 0; w < writers; w++ {
		select {
		case <-ready:
		case <-time.After(30 * time.Second):
			t.Fatalf("writer %d never wrote: the trial would be vacuous", w)
		}
	}

	for round := 0; round < 4; round++ {
		// Distinct timestamps: the recovery point's directory name has
		// second resolution, and four rounds would otherwise share one
		// directory and overwrite each other's copy.
		res, err := BackupLiveDB(root, time.Now().Add(time.Duration(round)*time.Second))
		if err != nil {
			t.Fatalf("round %d: the backup failed under concurrent writers: %v", round, err)
		}
		if err := verifyBackup(live, res.DBPath); err != nil {
			t.Fatalf("round %d: the backup did not verify: %v", round, err)
		}
		if err := verifyRecoveryPoint(res.DBPath, res.Fingerprint); err != nil {
			t.Fatalf("round %d: the recovery point did not verify: %v", round, err)
		}
		os.RemoveAll(res.Dir)
	}
	stopWriters()
	wg.Wait()

	// The trial must not be vacuous: the writers have to have written,
	// or nothing was contended for.
	live1, err := openSQLiteRO(live)
	if err != nil {
		t.Fatal(err)
	}
	defer live1.Close()
	var rows int64
	if err := live1.QueryRow("SELECT COUNT(*) FROM memories").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows < writers+1 {
		t.Fatalf("the stress trial was vacuous: the writers inserted %d rows in total", rows-1)
	}
	t.Logf("the writers inserted %d rows while %d backups were taken", rows-1, 4)
}
