package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	mpminternal "mpm/internal"
)

// ============================================================================
// Handler: backup
// ============================================================================
//
// `mpm backup [path]` exports the SQLite database to a .sql dump (the same
// format produced by `sqlite3 .dump`). Default path is `<db-dir>/mpm-backup-<UTC-timestamp>.sql`.
// Requires the `sqlite3` CLI to be on PATH (see doctor warning otherwise).
func handleBackup(args []string) int {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
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
// Order matters — close DB connections and clear WAL/SHM before invoking
// sqlite3, otherwise SQLite may merge WAL pages on top of the imported data
// and produce an inconsistent state.
func handleRestoreDB(args []string) int {
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return respond("", "Usage: mpm restore-db <path-to-sql-dump>\n", 1)
	}
	sqlPath := args[1]

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

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	dbPath := dm.DBPath()
	// Close all DB handles BEFORE clearing WAL/SHM so SQLite doesn't fight us.
	dm.Close()

	// Clear WAL/SHM siblings — they may contain writes newer than the dump.
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	for _, p := range []string{walPath, shmPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return respond("", fmt.Sprintf("Restore failed (clear %s): %v\n", p, err), 1)
		}
	}

	sqlitePath, err := exec.LookPath("sqlite3")
	if err != nil {
		return respond("", "Restore failed: `sqlite3` CLI not found on PATH (see `mpm doctor` for install hint)\n", 1)
	}
	cmd := exec.Command(sqlitePath, dbPath, ".read "+sqlPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return respond("", fmt.Sprintf("Restore failed (sqlite3 .read): %v\n%s\n", err, string(out)), 1)
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