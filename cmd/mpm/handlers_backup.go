package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ============================================================================
// Handler: backup
// ============================================================================
//
// `mpm backup [path]` exports the SQLite database to a .sql dump (the same
// format produced by `sqlite3 .dump`). Default path is `<db-dir>/mpm-backup-<UTC-timestamp>.sql`.
// Requires the `sqlite3` CLI to be on PATH (see doctor warning otherwise).
func handleBackup(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}
	dbPath := dm.DBPath()
	dm.Close()

	// Default output: timestamped .sql next to the source DB.
	outputPath := defaultBackupPath(dbPath)
	if len(args) >= 2 && strings.TrimSpace(args[1]) != "" {
		outputPath = args[1]
	}

	// Ensure target directory exists.
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return respond("", fmt.Sprintf("Backup failed (mkdir): %v\n", err), 1)
	}

	// Flush any pending WAL writes so the dump captures a consistent snapshot.
	if err := flushWal(dbPath); err != nil {
		return respond("", fmt.Sprintf("Backup failed (wal flush): %v\n", err), 1)
	}

	// Use sqlite3 .dump — portable, matches the format restore-db expects.
	sqlitePath, err := exec.LookPath("sqlite3")
	if err != nil {
		return respond("", "Backup failed: `sqlite3` CLI not found on PATH (see `mpm doctor` for install hint)\n", 1)
	}
	dump, err := exec.Command(sqlitePath, dbPath, ".dump").Output()
	if err != nil {
		return respond("", fmt.Sprintf("Backup failed (sqlite3 .dump): %v\n", err), 1)
	}

	if err := os.WriteFile(outputPath, dump, 0o644); err != nil {
		return respond("", fmt.Sprintf("Backup failed (write): %v\n", err), 1)
	}

	fmt.Printf("Backup written: %s (%d bytes)\n", outputPath, len(dump))
	return 0
}

// ============================================================================
// Handler: restore-db
// ============================================================================
//
// `mpm restore-db <path-to-sql-dump>` imports a .sql dump produced by
// `mpm backup` (or `sqlite3 .dump`) over the current database. Destructive:
// any existing rows are replaced by the dump's content.
//
// Reads the dump file and pipes it through mattn/go-sqlite3 directly instead
// of invoking the sqlite3 CLI. This avoids the shell-injection vector that
// the previous exec.Command(".read " + path) had — a tampered .sql with a
// leading .shell line would execute arbitrary shell commands via sqlite3 CLI.
func handleRestoreDB(args []string) int {
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return respond("", "Usage: mpm restore-db <path-to-sql-dump>\n", 1)
	}
	sqlPath := args[1]

	// Security: canonicalize the path and validate it stays within the
	// database directory. This prevents path traversal (e.g. ../../etc/passwd)
	// and restricts the operation to intentional backup files only.
	dm := getDB()
	if dm == nil {
		return 1
	}
	dbDir := filepath.Dir(dm.DBPath())
	dbPath := dm.DBPath()
	dm.Close()

	cleanPath := filepath.Clean(sqlPath)
	if !strings.HasPrefix(cleanPath, dbDir+string(filepath.Separator)) {
		return respond("", fmt.Sprintf("Restore path must be inside the database directory (%s): %s\n", dbDir, sqlPath), 1)
	}

	if _, err := os.Stat(sqlPath); err != nil {
		return respond("", fmt.Sprintf("Cannot read %s: %v\n", sqlPath, err), 1)
	}

	// Confirm with the user — this overwrites the live DB.
	fmt.Printf("About to restore DB from: %s\n", sqlPath)
	fmt.Print("This will overwrite the current database. Continue? [y/N] ")
	var answer string
	fmt.Scanln(&answer)
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return respond("", "Restore aborted.\n", 1)
	}

	// Close all DB handles BEFORE clearing WAL/SHM so SQLite doesn't fight us.
	// Clear WAL/SHM siblings — they may contain writes newer than the dump.
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	for _, p := range []string{walPath, shmPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return respond("", fmt.Sprintf("Restore failed (clear %s): %v\n", p, err), 1)
		}
	}

	// Read the dump file content
	content, err := os.ReadFile(sqlPath)
	if err != nil {
		return respond("", fmt.Sprintf("Restore failed (read): %v\n", err), 1)
	}

	// SECURITY (C-2): Validate the dump against an allow-list before executing.
	// The canonical validator rejects ATTACH / DETACH / SELECT / DELETE /
	// UPDATE / DROP / ALTER / CREATE VIRTUAL TABLE / non-foreign_keys PRAGMA /
	// etc. and any INSERT or CREATE against a non-canonical table. Fails
	// CLOSED on the first unsafe statement. The canonical schema is the
	// hand-maintained allow-list (CanonicalMPMSchema + RuntimeCanonicalSchema);
	// a static guard (TestCanonicalSchemaSync) keeps it in sync with the
	// source code's CREATE TABLE statements.
	validator := mpminternal.NewCanonicalDumpValidator()
	statements, err := validator.Prepare(string(content))
	if err != nil {
		return respond("", fmt.Sprintf("Restore rejected (unsafe dump): %v\n", err), 1)
	}

	// Open a fresh connection. Using mattn/go-sqlite3 directly (no CLI subprocess)
	// prevents the shell-injection vector present in the old sqlite3 .read approach.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return respond("", fmt.Sprintf("Restore failed (open): %v\n", err), 1)
	}
	defer db.Close()

	// Wrap the entire restore in a transaction. On any statement error, the
	// transaction rolls back and the database is left untouched. This prevents
	// the "partial restore" failure mode where some statements execute and
	// others fail, leaving the DB in an inconsistent state.
	tx, err := db.Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Restore failed (begin tx): %v\n", err), 1)
	}

	// SECURITY (H-3 side-effect): execute statements one at a time. The
	// validator already approved each statement against the allow-list; per-
	// statement execution ensures: (a) a NULL byte or partial write in the
	// dump yields a precise error rather than undefined behaviour from a
	// monolithic Exec; (b) the failure point is observable in the error
	// message; (c) defence-in-depth — even if a future bypass gets past
	// Validate, per-statement execution makes the failure atomic and the
	// transaction rolls back cleanly.
	//
	// Skip BEGIN/COMMIT/ROLLBACK/END statements — they would conflict with
	// the outer Go transaction (database/sql refuses nested transactions).
	// The validator already approved these as safe transaction-control
	// statements; the outer Go tx serves the same atomicity guarantee.
	for i, stmt := range statements {
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		first := strings.Fields(upper)
		if len(first) > 0 {
			switch first[0] {
			case "BEGIN", "COMMIT", "ROLLBACK", "END":
				continue
			}
		}
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return respond("", fmt.Sprintf("Restore failed at statement %d: %v\n", i+1, err), 1)
		}
	}

	if err := tx.Commit(); err != nil {
		return respond("", fmt.Sprintf("Restore failed (commit): %v\n", err), 1)
	}

	fmt.Printf("Restored from: %s\n", sqlPath)
	return 0
}

// ============================================================================
// Helpers
// ============================================================================

func defaultBackupPath(dbPath string) string {
	ts := time.Now().UTC().Format("20060102-150405")
	return filepath.Join(filepath.Dir(dbPath), fmt.Sprintf("mpm-backup-%s.sql", ts))
}

// flushWal runs `PRAGMA wal_checkpoint(TRUNCATE)` against dbPath so the dump
// captures all writes, not just what's in the main file. Uses a transient
// sql.DB connection (caller's connection is presumed closed or about to be).
func flushWal(dbPath string) error {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}
