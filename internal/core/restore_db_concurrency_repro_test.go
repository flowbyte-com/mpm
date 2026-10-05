// restore_db_concurrency_repro_test.go — Tranche H-4 §4 race reproducer.
//
// Empirical reproducer for the H-4 concurrency hazard described in
// internal/core/sql_dump_validator.go:
//
//     H-4 concurrent-write `flock` guard deferred
//
// The current restore-db handler (cmd/mpm/handlers_backup.go) renames
// `<workspace>/src/db/mpm` to `<workspace>/src/db/mpm.pre-restore`
// while another MPM writer may still hold an open FD on the same inode.
// Linux rename(2) does NOT invalidate open FDs to the old inode, so a
// concurrent writer continues writing to inode-X-aka-.pre-restore. The
// restore then either:
//   - commits the new DB and removes .pre-restore → writer's pending
//     writes are silently lost;
//   - rolls back from .pre-restore to <dbPath> → writer's writes are
//     preserved but the canonical path now carries content the
//     restore didn't intend to ship.
//
// This file pins the race empirically with deterministic synchronization
// (channels only — NO arbitrary sleeps). All tests use t.TempDir() and
// file-backed SQLite so FD/inode semantics match production.
//
// Each sub-test reports the actual outcome. The pre-fix failure mode
// the chosen guard must prevent is the deterministically-reproducible
// case below.
//
// Build tag: kept OFF the FTS5-required tests so this file does not
// interfere with the FTS5 build pipeline. Uses raw database/sql with
// the standard go-sqlite3 driver, which does not require FTS5.
package internal

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// freshRaceDB creates a file-backed SQLite at <workspace>/src/db/mpm
// with a single "race_log" table. Returns the dbPath and a ready
// connection. t.TempDir() guarantees production state is not touched.
func freshRaceDB(t *testing.T) (workspace, dbPath string, db *sql.DB) {
	t.Helper()
	workspace = t.TempDir()
	dbDir := filepath.Join(workspace, "src", "db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath = filepath.Join(dbDir, "mpm")
	dsn := fmt.Sprintf("file:%s?_journal_mode=wal&_busy_timeout=5000&_foreign_keys=on", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE race_log (n INTEGER NOT NULL, ts INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=wal`); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	// Force checkpoint so the canonical .db file has a baseline.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return workspace, dbPath, db
}

// writerFD opens a SECOND *sql.DB against the same file. Used to
// simulate "another MPM writer" — a CLI invocation, the scheduler, or
// the MCP server. Each FD has its own connection pool and its own
// SQLite POSIX advisory lock on the file's inode.
//
// This FD is what would carry a writer's writes across the rename
// (rename(2) does not invalidate FDs).
func writerFD(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_journal_mode=wal&_busy_timeout=5000&_foreign_keys=on", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("writerFD open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// doRestoreRenameAndReplay performs the exact destructive sequence
// handleRestoreDB executes after validation completes:
//   1. Remove <dbPath>-wal, <dbPath>-shm.
//   2. Rename <dbPath> -> <dbPath>.pre-restore.
//   3. Open a fresh sql.DB on <dbPath> (creates a new inode).
//   4. Replay CREATE TABLE + a single INSERT (simulating a restore).
//   5. (Caller decides when to remove .pre-restore; see
//      removePreRestore helper for the canonical teardown.)
//
// preRenameHook is invoked AFTER the rename (step 2) but BEFORE the
// fresh DB is opened (step 3). This is the deterministic window in
// which concurrent writer FDs still point to the OLD inode, now
// named .pre-restore.
//
// preRemoveHook is invoked AFTER the fresh DB has replayed content
// (step 4) but BEFORE .pre-restore is removed (step 5). This is the
// critical "writer's COMMIT will be lost" window: the old inode is
// about to be unlinked.
func doRestoreRenameAndReplay(t *testing.T, dbPath string, preRenameHook, preRemoveHook func()) {
	t.Helper()
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"

	// (1) Clear WAL/SHM.
	for _, p := range []string{walPath, shmPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", p, err)
		}
	}

	// (2) Rename live DB to .pre-restore.
	preDeleteCopy := dbPath + ".pre-restore"
	if err := os.Rename(dbPath, preDeleteCopy); err != nil && !os.IsNotExist(err) {
		t.Fatalf("rename: %v", err)
	}

	if preRenameHook != nil {
		preRenameHook()
	}

	// (3) Open fresh DB on canonical path (new inode).
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=5000", dbPath)
	restoreDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("restore open: %v", err)
	}
	defer restoreDB.Close()

	if _, err := restoreDB.Exec(`CREATE TABLE race_log (n INTEGER NOT NULL, ts INTEGER NOT NULL)`); err != nil {
		t.Fatalf("restore create: %v", err)
	}

	// (4) Replay one row (the "restored content" — symbolically the dump).
	if _, err := restoreDB.Exec(`INSERT INTO race_log VALUES (?, ?)`, -999, time.Now().Unix()); err != nil {
		t.Fatalf("restore insert: %v", err)
	}

	if preRemoveHook != nil {
		preRemoveHook()
	}
}

// removePreRestore removes the .pre-restore inode. The canonical
// restore path calls this AFTER the replay has committed.
func removePreRestore(t *testing.T, dbPath string) {
	t.Helper()
	preDeleteCopy := dbPath + ".pre-restore"
	if err := os.Remove(preDeleteCopy); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove pre-restore: %v", err)
	}
}

// sumNsAt returns SUM(n) so we can detect partial-writer losses even
// when the row count is unchanged (e.g. the writer wrote n=5 but the
// new DB has the row at n=-999).
func sumNsAt(t *testing.T, db *sql.DB, label string) int {
	t.Helper()
	var s int
	if err := db.QueryRow(`SELECT COALESCE(SUM(n),0) FROM race_log`).Scan(&s); err != nil {
		t.Fatalf("%s sum: %v", label, err)
	}
	return s
}

// countRowsAt returns the count of race_log rows visible from the
// given *sql.DB.
func countRowsAt(t *testing.T, db *sql.DB, label string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM race_log`).Scan(&n); err != nil {
		t.Fatalf("%s count: %v", label, err)
	}
	return n
}

// ScenarioA: writer writes BEFORE rename begins. Restore wins.
// Deterministic: writer's writes land in the OLD inode; restore
// removes the OLD inode, taking the writer's data with it.
//
// This is the trivial pre-fix failure mode: even without concurrency,
// the "validate-before-mutate" ordering means any writer that
// committed before the rename is unceremoniously discarded.
func TestH4Race_ScenarioA_WriterBeforeRename(t *testing.T) {
	workspace, dbPath, _ := freshRaceDB(t)
	_ = workspace

	// Writer's FD writes 100 rows BEFORE the rename. Each write
	// commits to the canonical inode.
	preWriter := writerFD(t, dbPath)
	for i := 0; i < 100; i++ {
		if _, err := preWriter.Exec(`INSERT INTO race_log VALUES (?, ?)`, i, time.Now().UnixNano()); err != nil {
			t.Fatalf("writer pre-rename insert: %v", err)
		}
	}
	// Verify writer sees its 100 rows.
	if got := countRowsAt(t, preWriter, "writer-pre-rename"); got != 100 {
		t.Fatalf("writer's FD should see 100 rows pre-rename; got %d", got)
	}

	// Now run restore (deterministic, no concurrency).
	doRestoreRenameAndReplay(t, dbPath, nil, nil)
	removePreRestore(t, dbPath)

	// The writer's FD still points to the OLD inode (now unlinked).
	// Reopen the canonical path from a fresh FD.
	final := writerFD(t, dbPath)
	got := countRowsAt(t, final, "post-restore-canonical")
	sumN := sumNsAt(t, final, "post-restore-canonical")

	if got != 1 {
		t.Errorf("post-restore canonical DB sees %d rows; want 1 (the restore's row)", got)
	}
	if sumN == 100 || sumN == 0 {
		t.Errorf("writer's pre-rename data is somehow still in the canonical DB: sum(n)=%d", sumN)
	}
	t.Logf("scenario A: writer's 100 pre-rename rows lost; canonical DB has 1 row (sum(n)=%d)", sumN)
}

// ScenarioB: writer's FD survives the rename (Linux rename(2) does not
// invalidate open FDs). Writer continues writing AFTER the rename;
// those writes go to the OLD inode (now named .pre-restore). When the
// restore removes .pre-restore, the writer's later writes are silently
// lost.
//
// Deterministic synchronization:
//   - writer writes one row, signals "ready" on preRenameReady
//   - test calls doRestoreRenameAndReplay; preRenameHook closes
//     preRenameReady and closes renameDone
//   - writer receives renameDone, writes 100 more rows to its FD
//   - test completes the restore and removes .pre-restore
//   - test inspects: did the writer's later writes survive?
func TestH4Race_ScenarioB_FDAcrossRename(t *testing.T) {
	workspace, dbPath, _ := freshRaceDB(t)
	_ = workspace

	// Writer opens FD and seeds a "before" row.
	writerDB := writerFD(t, dbPath)
	if _, err := writerDB.Exec(`INSERT INTO race_log VALUES (?, ?)`, 1, time.Now().UnixNano()); err != nil {
		t.Fatalf("writer seed: %v", err)
	}

	// Channels for deterministic synchronization.
	preRenameReady := make(chan struct{}) // writer -> test: ready
	renameDone := make(chan struct{})     // test -> writer: rename has happened
	postWritesDone := make(chan int)      // writer -> test: count of post-rename writes
	writeErr := make(chan error, 1)       // writer -> test: any error

	go func() {
		// Wait for test to confirm restore is in flight.
		<-preRenameReady

		// Wait until test signals the rename has completed.
		<-renameDone

		// Now write 100 rows to the FD. These go to the OLD inode
		// (now named .pre-restore). The writer's *sql.DB does not
		// know about the rename; it still holds the same FD.
		count := 0
		for i := 0; i < 100; i++ {
			if _, err := writerDB.Exec(`INSERT INTO race_log VALUES (?, ?)`, 2000+i, time.Now().UnixNano()); err != nil {
				writeErr <- err
				postWritesDone <- count
				return
			}
			count++
		}
		writeErr <- nil
		postWritesDone <- count
	}()

	// Run the destructive sequence. The preRenameHook fires AFTER
	// the rename but BEFORE the restore's fresh open.
	doRestoreRenameAndReplay(t, dbPath,
		func() {
			// rename has just happened. Signal writer.
			close(preRenameReady)
			close(renameDone)
		},
		nil,
	)
	<-postWritesDone
	postErr := <-writeErr

	// Remove .pre-restore — this is when writer's post-rename data dies.
	removePreRestore(t, dbPath)

	// The writer's FD (still open) is now pointing to an unlinked
	// inode. Reads from the writer's FD will still succeed (the FD
	// is valid; it just refers to a deleted file). But the writer's
	// data is unreachable from any path the user can name.
	var writerSees int
	if err := writerDB.QueryRow(`SELECT COUNT(*) FROM race_log`).Scan(&writerSees); err != nil {
		t.Logf("writer's FD: SELECT count failed (unlinked inode): %v", err)
		writerSees = -1
	} else {
		t.Logf("writer's FD still reads (unlinked inode): %d rows", writerSees)
	}

	// Canonical path: from a fresh FD. This is what the user sees.
	final := writerFD(t, dbPath)
	finalCount := countRowsAt(t, final, "post-restore-canonical")
	finalSum := sumNsAt(t, final, "post-restore-canonical")

	t.Logf("scenario B: writer's post-rename writes = %d (post-rename), writeErr=%v, writerFD view=%d rows, canonical-view=%d rows sum(n)=%d",
		postWritesDone, postErr, writerSees, finalCount, finalSum)

	if finalCount != 1 {
		t.Errorf("post-restore canonical DB has %d rows; expected 1 (the restore's row). writer's 100 post-rename writes were silently lost", finalCount)
	}
	if finalSum != -999 {
		t.Errorf("post-restore canonical DB has sum(n)=%d; expected -999 (the restore's row)", finalSum)
	}
}

// ScenarioC: writer BEGINs a transaction, holds it across the rename,
// and COMMITs AFTER the rename. SQLite honors the FD-inode model:
// the COMMIT writes to the OLD inode (now named .pre-restore).
// Restore's later Remove(.pre-restore) silently destroys the COMMIT's
// effect.
//
// Deterministic synchronization: same as ScenarioB but with BEGIN
// IMMEDIATE / INSERT / wait / COMMIT.
func TestH4Race_ScenarioC_TxAcrossRename(t *testing.T) {
	workspace, dbPath, _ := freshRaceDB(t)
	_ = workspace

	writerDB := writerFD(t, dbPath)

	preRenameReady := make(chan struct{})
	renameDone := make(chan struct{})
	commitResult := make(chan error, 1)

	go func() {
		<-preRenameReady
		if _, err := writerDB.Exec(`BEGIN IMMEDIATE`); err != nil {
			commitResult <- err
			return
		}
		if _, err := writerDB.Exec(`INSERT INTO race_log VALUES (?, ?)`, 42, time.Now().UnixNano()); err != nil {
			commitResult <- err
			return
		}
		// Wait for the rename to complete, then commit.
		<-renameDone
		_, err := writerDB.Exec(`COMMIT`)
		commitResult <- err
	}()

	doRestoreRenameAndReplay(t, dbPath,
		func() {
			close(preRenameReady)
			close(renameDone)
		},
		nil,
	)
	commitErr := <-commitResult
	removePreRestore(t, dbPath)

	final := writerFD(t, dbPath)
	sumN := sumNsAt(t, final, "post-restore-canonical")
	count := countRowsAt(t, final, "post-restore-canonical")

	t.Logf("scenario C: writer's COMMIT result=%v; canonical DB has %d rows, sum(n)=%d", commitErr, count, sumN)

	// The COMMIT "succeeded" from SQLite's perspective (no error).
	// The actual outcome observed empirically is data CONTAMINATION
	// rather than silent loss: the writer's n=42 row lands in the
	// canonical DB alongside the restore's -999 row.  Either outcome
	// is a contract violation.  Pin both shapes (sum == -999 for
	// silent loss, sum == -957 for contamination) as race evidence.
	//
	// Mechanism (best current hypothesis): SQLite's COMMIT path
	// reopens the file by canonical path; after the rename, the
	// canonical path points to the restore's NEW inode, so the
	// COMMIT's WAL flush lands in the new file.  Even if the writer's
	// user-space FD still points to the old inode, the driver
	// surfaces the COMMITTED row at the canonical path.
	if commitErr != nil {
		t.Logf("scenario C: writer's COMMIT returned %v (writer saw the rename)", commitErr)
	}
	switch sumN {
	case -999:
		t.Logf("scenario C: race outcome = silent data loss; canonical DB has only the restore's row")
	case 42 + (-999):
		t.Logf("scenario C: race outcome = data contamination; canonical DB has restore's row + writer's row")
	default:
		t.Errorf("scenario C: race produced unexpected canonical DB state: count=%d sum(n)=%d", count, sumN)
	}
}

// ScenarioD: writer opens the canonical path AFTER the rename. They
// receive the NEW inode (created by the restore's open). This is
// benign IF the restore has fully replayed by the time the writer
// opens; otherwise the writer sees an empty or partially-populated
// schema.
//
// Deterministic synchronization: rename completes, then writer's FD
// is opened, then restore's CREATE TABLE + INSERT happens.
func TestH4Race_ScenarioD_OpenAfterRename(t *testing.T) {
	workspace, dbPath, _ := freshRaceDB(t)
	_ = workspace

	// Pre-rename seed.
	preWriter := writerFD(t, dbPath)
	if _, err := preWriter.Exec(`INSERT INTO race_log VALUES (?, ?)`, 1, time.Now().UnixNano()); err != nil {
		t.Fatalf("pre-rename seed: %v", err)
	}

	// We can't easily split "rename" from "fresh-open" via the helper,
	// so we run the destructive sequence manually for ScenarioD.
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	for _, p := range []string{walPath, shmPath} {
		_ = os.Remove(p)
	}
	preDeleteCopy := dbPath + ".pre-restore"
	if err := os.Rename(dbPath, preDeleteCopy); err != nil && !os.IsNotExist(err) {
		t.Fatalf("rename: %v", err)
	}

	// Now the canonical path is unlinked. Open a fresh FD — what
	// does it see?
	post := writerFD(t, dbPath)
	var count int
	if err := post.QueryRow(`SELECT COUNT(*) FROM race_log`).Scan(&count); err != nil {
		t.Logf("scenario D: post-rename open sees no schema yet: %v (this is the 'restore hasn't replayed' window)", err)
	} else {
		t.Logf("scenario D: post-rename open sees count=%d (post-rename SQL sees an empty schema, count via CREATE TABLE-less open)", count)
	}

	// Replay the restore.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=5000", dbPath)
	restoreDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("restore open: %v", err)
	}
	defer restoreDB.Close()
	if _, err := restoreDB.Exec(`CREATE TABLE race_log (n INTEGER NOT NULL, ts INTEGER NOT NULL)`); err != nil {
		t.Fatalf("restore create: %v", err)
	}
	if _, err := restoreDB.Exec(`INSERT INTO race_log VALUES (?, ?)`, -999, time.Now().Unix()); err != nil {
		t.Fatalf("restore insert: %v", err)
	}

	// Now the post-rename writer's FD can see the canonical DB.
	postCount := countRowsAt(t, post, "post-replay")
	t.Logf("scenario D: post-rename writer's FD sees %d rows after restore replay", postCount)

	if postCount != 1 {
		t.Errorf("scenario D: post-rename writer FD sees %d rows; want 1 (the restore's row)", postCount)
	}
	_ = os.Remove(preDeleteCopy)
}

// ScenarioE: writer has uncheckpointed WAL writes when restore begins.
// Restore removes the WAL file BEFORE the rename. The writer's
// uncheckpointed data is destroyed.
//
// Deterministic: the writer inserts a row but does NOT checkpoint;
// the test then explicitly removes the WAL file and inspects.
func TestH4Race_ScenarioE_WALUncheckpointed(t *testing.T) {
	workspace, dbPath, _ := freshRaceDB(t)
	_ = workspace

	writerDB := writerFD(t, dbPath)
	if _, err := writerDB.Exec(`INSERT INTO race_log VALUES (?, ?)`, 1, time.Now().UnixNano()); err != nil {
		t.Fatalf("writer seed: %v", err)
	}
	// Confirm WAL exists.
	walPath := dbPath + "-wal"
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("expected WAL at %s: %v", walPath, err)
	}

	// Without the writer's FD doing any further I/O, the WAL still
	// holds the n=1 row. (Checkpoint TRUNCATE would move it to the
	// main file, but we do NOT checkpoint.)
	preDeleteCopy := dbPath + ".pre-restore"

	// Restore sequence: (1) Remove WAL/SHM, (2) rename.
	if err := os.Remove(walPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove wal: %v", err)
	}
	if err := os.Rename(dbPath, preDeleteCopy); err != nil && !os.IsNotExist(err) {
		t.Fatalf("rename: %v", err)
	}

	// Open fresh DB and replay.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=5000", dbPath)
	restoreDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("restore open: %v", err)
	}
	defer restoreDB.Close()
	if _, err := restoreDB.Exec(`CREATE TABLE race_log (n INTEGER NOT NULL, ts INTEGER NOT NULL)`); err != nil {
		t.Fatalf("restore create: %v", err)
	}
	if _, err := restoreDB.Exec(`INSERT INTO race_log VALUES (?, ?)`, -999, time.Now().Unix()); err != nil {
		t.Fatalf("restore insert: %v", err)
	}
	_ = os.Remove(preDeleteCopy)

	// Reopen from a fresh FD. The writer's n=1 row is gone.
	final := writerFD(t, dbPath)
	sum := sumNsAt(t, final, "post-restore-canonical")
	count := countRowsAt(t, final, "post-restore-canonical")
	t.Logf("scenario E: post-restore canonical DB has %d rows, sum(n)=%d (writer's WAL row n=1 destroyed)", count, sum)

	if count != 1 || sum != -999 {
		t.Errorf("scenario E: race NOT reproduced; expected 1 row sum=-999, got count=%d sum=%d", count, sum)
	}
}