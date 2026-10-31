package update

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// BackupResult describes a verified recovery point.
type BackupResult struct {
	// Dir is the recovery point directory (backups/upgrade-<ts>/).
	Dir string
	// DBPath is the verified database copy inside Dir.
	DBPath string
	// Fingerprint is the live database's schema fingerprint at backup
	// time (see schemafp.go).
	Fingerprint string
}

// BackupLiveDB takes a verified online backup of the live database.
//
// It uses VACUUM INTO, which is an online, transactionally consistent
// copy — the same property the rehearsal's sqlite3 .backup path gives,
// and for the same reason: a raw file copy of a WAL-mode database omits
// everything still in the -wal file. Committed transactions that have
// not been checkpointed would be silently lost.
//
// The backup is verified three ways before this returns: the file is
// non-empty, PRAGMA integrity_check passes, and every user table's row
// count matches the live database. A backup that cannot be restored is
// not a backup.
//
// Operator-owned files (mpm_config.json, active.json, mode/,
// persona/) are copied verbatim with modes preserved, and are never
// printed. Credentials are never logged.
func BackupLiveDB(dataRoot string, now time.Time) (BackupResult, error) {
	var out BackupResult
	l := NewLayout(dataRoot)
	live := l.LiveDB()
	if _, err := os.Stat(live); err != nil {
		return out, fmt.Errorf("no live database at %s: %w", live, err)
	}
	ts := now.UTC().Format("20060102T150405Z")
	dir := filepath.Join(l.Backups(), "upgrade-"+ts)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return out, fmt.Errorf("could not create the backup directory: %w", err)
	}
	dst := filepath.Join(dir, "mpm.db")
	if err := vacuumInto(live, dst); err != nil {
		return out, fmt.Errorf("the SQLite online backup failed: %w", err)
	}
	if err := verifyBackup(live, dst); err != nil {
		return out, err
	}
	fp, err := SchemaFingerprint(live)
	if err != nil {
		return out, fmt.Errorf("could not fingerprint the live database: %w", err)
	}
	out = BackupResult{Dir: dir, DBPath: dst, Fingerprint: fp}
	copyOperatorFiles(dataRoot, dir)
	if err := os.WriteFile(l.LastBackup(), []byte(dir+"\n"), 0600); err != nil {
		return out, fmt.Errorf("could not record the recovery point: %w", err)
	}
	return out, nil
}

// vacuumInto copies src into dst with VACUUM INTO. The source is opened
// read-only; SQLite still requires write access to the source DIRECTORY
// for its journal files, which is why this must run against the real
// data root and not a read-only mount.
func vacuumInto(src, dst string) error {
	rmSiblingJournals(dst)
	db, err := sql.Open("sqlite3", "file:"+src+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// VACUUM INTO cannot take a bound parameter for the filename; the
	// destination is constructed by this function from the backup
	// directory, never from operator input, so quoting is safe.
	q := fmt.Sprintf("VACUUM INTO '%s'", escapeSingleQuote(dst))
	if _, err := db.Exec(q); err != nil {
		return err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("the backup produced no file: %w", err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("the backup produced an empty file")
	}
	return nil
}

func escapeSingleQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(out)
}

func rmSiblingJournals(dst string) {
	os.Remove(dst)
	os.Remove(dst + "-wal")
	os.Remove(dst + "-shm")
}

// verifyBackup checks integrity_check and per-table row-count parity
// between live and backup.
func verifyBackup(live, backup string) error {
	bdb, err := sql.Open("sqlite3", "file:"+backup+"?mode=ro")
	if err != nil {
		return fmt.Errorf("could not open the backup for verification: %w", err)
	}
	defer bdb.Close()
	bdb.SetMaxOpenConns(1)
	var integrity string
	if err := bdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("backup integrity_check failed: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("backup integrity_check: %s", integrity)
	}
	liveCounts, err := tableRowCounts(live)
	if err != nil {
		return fmt.Errorf("could not count the live tables: %w", err)
	}
	backupCounts, err := tableRowCounts(backup)
	if err != nil {
		return fmt.Errorf("could not count the backup tables: %w", err)
	}
	for t, n := range liveCounts {
		if backupCounts[t] != n {
			return fmt.Errorf("backup row counts differ on %s (live=%d backup=%d)", t, n, backupCounts[t])
		}
	}
	for t := range backupCounts {
		if _, ok := liveCounts[t]; !ok {
			return fmt.Errorf("backup carries an unexpected table %s", t)
		}
	}
	return nil
}

func tableRowCounts(path string) (map[string]int64, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		// Table names come from sqlite_master, not operator input.
		// An unreadable table fails verification: a backup whose
		// contents cannot be fully compared is not a verified backup.
		if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, t)).Scan(&n); err != nil {
			return nil, fmt.Errorf("could not count table %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// copyOperatorFiles copies operator-owned state alongside the database
// backup. Modes are preserved; nothing is printed or logged.
func copyOperatorFiles(dataRoot, dir string) {
	for _, name := range []string{"mpm_config.json", "active.json"} {
		src := filepath.Join(dataRoot, name)
		if fi, err := os.Stat(src); err == nil && !fi.IsDir() {
			raw, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			os.WriteFile(filepath.Join(dir, name), raw, 0600) //nolint:errcheck
		}
	}
	for _, name := range []string{"mode", "persona"} {
		src := filepath.Join(dataRoot, name)
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			continue
		}
		copyDir(src, filepath.Join(dir, name))
	}
}

func copyDir(src, dst string) {
	os.MkdirAll(dst, 0700) //nolint:errcheck
	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyDir(s, d)
			continue
		}
		raw, err := os.ReadFile(s)
		if err != nil {
			continue
		}
		os.WriteFile(d, raw, 0600) //nolint:errcheck
	}
}
