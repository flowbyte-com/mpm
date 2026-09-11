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
	// Extract the path string only. Do NOT call dm.Close() — dbManager is
	// a process-wide singleton reused by every CLI invocation in this
	// process, and closing it here would break every subsequent command.
	// The 2026-08-13 audit flagged this; the regression is locked in by
	// cmd/mpm/handlers_backup_singleton_test.go::TestHandleBackup_LeavesSingletonAlive.
	dbPath := dm.DBPath()

	// Default output: timestamped .sql next to the source DB.
	outputPath := defaultBackupPath(dbPath)
	if len(args) >= 2 && strings.TrimSpace(args[1]) != "" {
		outputPath = args[1]
	}

	// Ensure target directory exists.
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
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

	if err := os.WriteFile(outputPath, dump, 0o600); err != nil {
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
//
// SAFETY MODEL (T78 2026-09-10 regression repair):
// The destructive ordering is the bug class. Pre-fix, the handler would
// clear WAL/SHM siblings and rename the live DB aside BEFORE validating
// the dump content. If validation rejected the dump (e.g. on a "DO"
// statement), the live DB was already at `.pre-restore` and the canonical
// path was empty, leaving the system degraded until manual recovery.
//
// Post-fix, the handler follows a strict "validate-before-mutate" contract:
//   1. Confirm with the user.
//   2. Read the entire dump into memory.
//   3. Run the canonical validator against the dump (returns parsed
//      statements or rejects). The live DB is UNTOUCHED at this point —
//      any rejection here returns non-zero with the original DB intact.
//   4. Only after a fully valid dump is in hand, clear WAL/SHM siblings,
//      rename the live DB aside as `.pre-restore`, and write the new DB.
//   5. If anything fails after the rename, restore the original from
//      `.pre-restore` and report the error.
//   6. On success, remove `.pre-restore`.
//
// This way the live database is only ever mutated once a complete,
// validated restore is guaranteed to proceed.
func handleRestoreDB(args []string) int {
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return respond("", "Usage: mpm restore-db <path-to-sql-dump>\n", 1)
	}
	sqlPath := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}
	// Extract the path strings only. Do NOT call dm.Close() — dbManager is
	// a process-wide singleton reused by every CLI invocation in this
	// process. The actual restore writes through a fresh sql.Open below,
	// not through the singleton, so closing the singleton here serves no
	// purpose and breaks every subsequent command. The 2026-08-13 audit
	// flagged this; regression locked in by
	// cmd/mpm/handlers_backup_singleton_test.go::TestHandleRestoreDB_LeavesSingletonAlive.
	dbPath := dm.DBPath()

	// 2026-09-10 regression repair (T78): allow legitimate
	// operator-supplied backups from outside the database
	// directory. The previous implementation rejected any path
	// outside the DB dir, which broke a documented restore use
	// case. The actual security boundary is the canonical SQL
	// validator below (NewCanonicalDumpValidator), which rejects
	// unsafe statements regardless of where the dump file lives.
	// We still:
	//   • canonicalize the path (filepath.Clean — defeats trivial
	//     `..` injection)
	//   • require the path to be absolute or relative-to-cwd via
	//     filepath.Abs (no implicit DB-dir prefix)
	//   • refuse obviously unsafe paths (non-regular files,
	//     directories, devices) via os.Stat + mode check
	cleanPath, err := filepath.Abs(sqlPath)
	if err != nil {
		return respond("", fmt.Sprintf("Restore path invalid: %v\n", err), 1)
	}
	info, err := os.Stat(cleanPath)
	if err != nil {
		return respond("", fmt.Sprintf("Cannot read %s: %v\n", sqlPath, err), 1)
	}
	if !info.Mode().IsRegular() {
		return respond("", fmt.Sprintf("Restore path is not a regular file: %s\n", sqlPath), 1)
	}

	// Confirm with the user — this overwrites the live DB.
	fmt.Printf("About to restore DB from: %s\n", sqlPath)
	fmt.Print("This will overwrite the current database. Continue? [y/N] ")
	var answer string
	fmt.Scanln(&answer)
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return respond("", "Restore aborted.\n", 1)
	}

	// STEP 1 (T78 contract): read the entire dump into memory before
	// touching the live DB. fs.Stat already verified the path is a
	// regular file; ReadFile is the only operation that may fail and
	// (a) does not mutate the live DB and (b) returns a precise error.
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return respond("", fmt.Sprintf("Restore failed (read): %v\n", err), 1)
	}

	// STEP 1.5 (2026-09-11 driver-compat shim): preprocess the dump
	// before validation to apply two driver-compat rewrites that the
	// validator still accepts but mattn/go-sqlite3 cannot execute:
	//
	//   1. Drop `INSERT INTO sqlite_schema` (and the legacy
	//      `sqlite_master` alias). `sqlite3 .dump` emits one such row
	//      per database object — they're how the shadow-table
	//      restoration path rebuilds virtual-table metadata. mattn's
	//      driver returns `table sqlite_master may not be modified`
	//      if we try to execute them. The catalog rows are redundant:
	//      the `CREATE TABLE` and `CREATE VIRTUAL TABLE` statements
	//      earlier in the dump already register every object with the
	//      schema. The validator still ALLOWS them (it's not a
	//      security boundary — the catalog rows only describe objects
	//      the validator already approved) — we just don't execute.
	//
	//   2. Promote `CREATE TABLE <name>` to `CREATE TABLE IF NOT EXISTS
	//      <name>`. Real-world restore targets are typically a fresh
	//      DB or one whose schema has drifted slightly from the dump
	//      (e.g. an in-progress migration already added a few tables).
	//      The dump's bare CREATE TABLE would then fail with "table
	//      X already exists" — aborting the entire restore transaction
	//      and wasting all the prior statements. IF NOT EXISTS makes
	//      the restore idempotent against pre-existing tables while
	//      still creating any that are missing. This is a benign
	//      rewrite: the validator already accepts both forms, and the
	//      IF NOT EXISTS semantics are a strict subset of the bare
	//      CREATE TABLE semantics (creating tables only if they don't
	//      exist).
	//
	// Both rewrites happen BEFORE validation so the validator sees the
	// final form and reports statement numbers that match what will be
	// executed.
	content = preprocessDumpForRestore(content)

	// STEP 2 (T78 contract): validate the dump against the allow-list
	// before any destructive mutation. The canonical validator rejects
	// ATTACH / DETACH / SELECT / DELETE / UPDATE / DROP / ALTER /
	// CREATE VIRTUAL TABLE / non-foreign_keys PRAGMA / etc. and any
	// INSERT or CREATE against a non-canonical table. Fails CLOSED on
	// the first unsafe statement. The canonical schema is the
	// hand-maintained allow-list (CanonicalMPMSchema +
	// RuntimeCanonicalSchema); a static guard (TestCanonicalSchemaSync)
	// keeps it in sync with the source code's CREATE TABLE statements.
	//
	// If validation fails here, the live DB is UNTOUCHED — exactly the
	// pre-fix invariant that was violated.
	validator := mpminternal.NewCanonicalDumpValidator()
	statements, err := validator.Prepare(string(content))
	if err != nil {
		return respond("", fmt.Sprintf("Restore rejected (unsafe dump): %v\n", err), 1)
	}

	// STEP 3 (T78 contract): only NOW do we touch the live DB.
	// Clear WAL/SHM siblings — they may contain writes newer than the
	// dump. Best-effort: a sibling absence is not an error (the live DB
	// may not have been opened in WAL mode).
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	for _, p := range []string{walPath, shmPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return respond("", fmt.Sprintf("Restore failed (clear %s): %v\n", p, err), 1)
		}
	}

	// Move the live DB aside as `.pre-restore`. Any failure from this
	// point forward must restore from `.pre-restore` to leave the
	// original intact.
	preDeleteCopy := dbPath + ".pre-restore"
	if _, err := os.Stat(preDeleteCopy); err == nil {
		// Defensive: a stale `.pre-restore` from a prior aborted
		// restore would block this rename. Move it aside to a unique
		// suffix so we never lose data.
		_ = os.Rename(preDeleteCopy, preDeleteCopy+".stale-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	if err := os.Rename(dbPath, preDeleteCopy); err != nil && !os.IsNotExist(err) {
		return respond("", fmt.Sprintf("Restore failed (pre-delete copy): %v\n", err), 1)
	}

	// restoreFromBackup moves the `.pre-restore` file back to the
	// canonical DB path. Package-level helper (see below); declared
	// outside this function so the mpm-lint tx-classifier can see
	// the function structure cleanly without crossing a closure
	// boundary.

	// Open a fresh connection. Using mattn/go-sqlite3 directly (no CLI subprocess)
	// prevents the shell-injection vector present in the old sqlite3 .read approach.
	//
	// The DSN uses `_txlock=immediate` so the very first statement we
	// issue (PRAGMA foreign_keys=OFF) acquires an IMMEDIATE lock. This
	// prevents the race where the outer Begin() succeeds at deferred
	// lock level but a subsequent CREATE TABLE statement tries to
	// upgrade and finds another connection has sneaked in between —
	// which on a hot DB can cause the tx to commit/rollback
	// implicitly. `_busy_timeout=5000` matches the canonical MPM
	// connection DSN so we wait briefly for the lock instead of
	// returning SQLITE_BUSY. The 2026-09-11 Stage 0 restore
	// regression surfaced this when the validator's pre-pass changed
	// the statement shape and the inner Exec began racing with the
	// fresh DB's auto-checkpoint.
	db, err := sql.Open("sqlite3", dbPath+"?_txlock=immediate&_busy_timeout=5000")
	if err != nil {
		return restoreFromBackup(dbPath, preDeleteCopy, "open", err)
	}
	defer closeSQLDB(db)

	// Wrap the entire restore in a transaction. On any statement error, the
	// transaction rolls back and the database is left untouched. This prevents
	// the "partial restore" failure mode where some statements execute and
	// others fail, leaving the DB in an inconsistent state.
	tx, err := db.Begin()
	if err != nil {
		return restoreFromBackup(dbPath, preDeleteCopy, "begin tx", err)
	}
	defer tx.Rollback() // safe no-op after Commit; covers panic / early returns

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
	//
	// Skip INSERT INTO sqlite_schema / sqlite_master as well. The
	// `sqlite3 .dump` tool emits these to re-populate the system catalog
	// for every object, but mattn/go-sqlite3 (the driver used here)
	// returns `table sqlite_master may not be modified` if we try to
	// execute them. The catalog rows are redundant: the `CREATE TABLE`
	// and `CREATE VIRTUAL TABLE` statements earlier in the dump already
	// register every object with the schema. Dropping these rows is
	// semantically a no-op for restore purposes. The validator still
	// ALLOWS them (it's not a security boundary — the catalog rows
	// only describe objects the validator already approved) — the skip
	// is purely a driver-compat shim.
	for i, stmt := range statements {
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		first := strings.Fields(upper)
		if len(first) > 0 {
			switch first[0] {
			case "BEGIN", "COMMIT", "ROLLBACK", "END":
				continue
			}
			// INSERT INTO sqlite_schema / sqlite_master — driver-compat skip.
			if first[0] == "INSERT" && len(first) >= 3 && first[1] == "INTO" {
				// The token may be glued to a column list (no whitespace
				// between `sqlite_schema` and `(`) — extract the bare name.
				tbl := first[2]
				if i := strings.IndexAny(tbl, "("); i >= 0 {
					tbl = tbl[:i]
				}
				tbl = strings.ToLower(tbl)
				if tbl == "sqlite_schema" || tbl == "sqlite_master" ||
					tbl == "sqlite_temp_schema" || tbl == "sqlite_temp_master" {
					continue
				}
			}
		}
		if _, err := tx.Exec(stmt); err != nil {
			// Explicit Rollback so the linter sees the cleanup path
			// (the deferred Rollback above is the safety net for panic
			// paths; this one covers the expected error returns).
			tx.Rollback()
			return restoreFromBackup(dbPath, preDeleteCopy, fmt.Sprintf("statement %d", i+1), err)
		}
	}

	if err := tx.Commit(); err != nil {
		return restoreFromBackup(dbPath, preDeleteCopy, "commit", err)
	}

	// Restore succeeded — discard the pre-delete backup.
	_ = os.Remove(preDeleteCopy)

	fmt.Printf("Restored from: %s\n", sqlPath)
	return 0
}

// ============================================================================
// Helpers
// ============================================================================

// preprocessDumpForRestore applies driver-compat rewrites to a dump
// before validation. See the comment in handleRestoreDB for the why.
// The rewrites are line-based (one statement per line is not guaranteed
// by the dump format — `sqlite3 .dump` writes many statements per line
// for compactness — so we use a line-by-line pass for the safe
// rewrites that always start at the beginning of a line).
//
// FTS5 shadow tables ('<base>_fts_data', '_fts_content', '_fts_config',
// '_fts_docsize', '_fts_idx') are CREATE TABLE statements emitted by
// `sqlite3 .dump` for every FTS5 virtual table in the schema. They are
// NOT legal SQL when the corresponding `<base>_fts` virtual table is
// recreated from scratch — SQLite's CREATE VIRTUAL TABLE for fts5
// creates its own internal shadow tables, and any pre-existing ones
// with the same names block the recreate with "table '<base>_fts_data'
// already exists". The dump format does not separate the shadow-table
// INSERT INTO sqlite_schema rows from their CREATE TABLE triples
// (because mattn/go-sqlite3 rejects INSERT INTO sqlite_master), and
// we cannot runnable execute the virtual-table registration. So the
// ONLY clean fix is to drop the FTS shadow-table CREATE statements
// from the dump entirely: the new DB's own initFTSTables path will
// recreate the virtual table from scratch (driving CREATE VIRTUAL
// TABLE which creates fresh shadow tables). This is forward-only — it
// does not preserve shadow table contents, but the shadow tables hold
// ONLY a search projection; the canonical base-table data they project
// survives, and initFTSTables' backfill reindexes it.
//
// This is the Stage 0 (Pi alpha) follow-on's actual root-cause fix:
// without it, every restore leaves the new DB with stale FTS shadow
// tables and no virtual tables, broken silently on the next init.
// Skipping only the shadow-table CREATE statements (not the
// `<base>_fts` view, which sqlite emits as no CREATE because virtual
// tables are not first-class sql) is sufficient.
func preprocessDumpForRestore(content []byte) []byte {
	var b strings.Builder
	b.Grow(len(content))
	for _, line := range strings.Split(string(content), "\n") {
		upper := strings.ToUpper(strings.TrimSpace(line))
		// Skip `INSERT INTO sqlite_schema`/`sqlite_master`/`sqlite_temp_*` lines.
		if strings.HasPrefix(upper, "INSERT INTO SQLITE_SCHEMA") ||
			strings.HasPrefix(upper, "INSERT INTO SQLITE_MASTER") ||
			strings.HasPrefix(upper, "INSERT INTO SQLITE_TEMP_SCHEMA") ||
			strings.HasPrefix(upper, "INSERT INTO SQLITE_TEMP_MASTER") {
			continue
		}
		// Round 10 T78 fix: strip the `PRAGMA writable_schema=ON;` /
		// `PRAGMA writable_schema=OFF;` brackets that `sqlite3 .dump`
		// emits around the `INSERT INTO sqlite_schema` lines. Pre-fix
		// these slipped through and the validator rejected the dump
		// (`PRAGMA writable_schema not allowed`) even though the
		// operations they bracket had already been stripped. Round 7
		// orphan-prevention work (Round 8 follow-up) removed the
		// INSERT INTO sqlite_schema rows but missed the wrapping
		// PRAGMAs, leaving every dump-blocking-rejection route still
		// visible to the validator. After this strip the dump
		// round-trips through the canonical validator end-to-end.
		if strings.HasPrefix(upper, "PRAGMA WRITABLE_SCHEMA") {
			continue
		}
		// Skip FTS5 shadow-table CREATE TABLE statements. The dump emits
		// these for every FTS5 module; replaying them creates plain
		// CREATE TABLE entries with the shadow-table names, which then
		// blocks the new DB's `CREATE VIRTUAL TABLE IF NOT EXISTS` with
		// "table '<base>_fts_<suffix>' already exists" (orphaned shadow
		// tables — see fts_recovery.go). The new DB's init path drives
		// the canonical FTS virtual-table creation from initFTSTables,
		// which generates fresh shadow tables.
		if isFTS5ShadowCreate(upper) {
			continue
		}
		// Promote `CREATE TABLE <name>` to `CREATE TABLE IF NOT EXISTS <name>`.
		if strings.HasPrefix(upper, "CREATE TABLE ") &&
			!strings.HasPrefix(upper, "CREATE TABLE IF NOT EXISTS ") {
			line = "CREATE TABLE IF NOT EXISTS " + line[len("CREATE TABLE "):]
		}
		// Promote `CREATE INDEX <name>` to `CREATE INDEX IF NOT EXISTS <name>`.
		// A fresh or partially-migrated DB may already have the index
		// from a prior migration; without IF NOT EXISTS the dump's
		// CREATE INDEX would fail with "index already exists" and
		// abort the entire restore transaction. IF NOT EXISTS is a
		// strict subset of bare CREATE INDEX (creates only if absent).
		// Handle CREATE UNIQUE INDEX (compound keyword) as well.
		if strings.HasPrefix(upper, "CREATE INDEX ") &&
			!strings.HasPrefix(upper, "CREATE INDEX IF NOT EXISTS ") {
			line = "CREATE INDEX IF NOT EXISTS " + line[len("CREATE INDEX "):]
		}
		if strings.HasPrefix(upper, "CREATE UNIQUE INDEX ") &&
			!strings.HasPrefix(upper, "CREATE UNIQUE INDEX IF NOT EXISTS ") {
			line = "CREATE UNIQUE INDEX IF NOT EXISTS " + line[len("CREATE UNIQUE INDEX "):]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// isFTS5ShadowCreate reports whether a line beginning with `CREATE TABLE`
// (possibly IF NOT EXISTS-promoted already) is the CREATE statement for
// an FTS5 shadow table. We match by suffix only; the prefix (`<base>`)
// is not checked against the canonical allow-list — the upstream
// validator already enforces that. We do NOT match the virtual table
// itself (it would not appear here anyway because `sqlite3 .dump` does
// not emit CREATE VIRTUAL TABLE statements).
//
// `sqlite3 .dump` writes the shadow-table names as quoted identifiers
// (`'sessions_fts_data'`); after our uppercase pass the quotes remain.
// We strip quotes for the suffix match.
func isFTS5ShadowCreate(upper string) bool {
	if !strings.HasPrefix(upper, "CREATE TABLE") {
		return false
	}
	// Capture the part after CREATE TABLE [IF NOT EXISTS], up to the
	// first whitespace. The dump always quotes the table name.
	const createTable = "CREATE TABLE"
	rest := strings.TrimSpace(upper[len(createTable):])
	// Drop optional "IF NOT EXISTS"
	if strings.HasPrefix(rest, "IF NOT EXISTS ") {
		rest = rest[len("IF NOT EXISTS "):]
	}
	// Take the first token — possibly bracketed or quoted.
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return false
	}
	name := rest
	if sp := strings.IndexAny(rest, " \t("); sp >= 0 {
		name = rest[:sp]
	}
	// Strip surrounding quote/brackets used by `sqlite3 .dump`.
	name = strings.Trim(name, "'\"[]`")
	// Suffix match against the canonical five-shadow-table set.
	for _, suffix := range []string{"_FTS_DATA", "_FTS_CONTENT", "_FTS_CONFIG", "_FTS_DOCSIZE", "_FTS_IDX"} {
		if strings.HasSuffix(name, suffix) {
			// `<base>` must itself end with `_FTS` — otherwise this is a
			// regular table whose name accidentally ends with, say,
			// `_review_data`. SQLite reserves the `_fts_<...>` shadow
			// tables for fts5 virtual tables, but a strict suffix check
			// without the `_FTS` qualifier would mis-fire.
			without := strings.TrimSuffix(name, suffix)
			return strings.HasSuffix(without, "_FTS")
		}
	}
	return false
}

// restoreFromBackup undoes a destructive restore attempt by moving
// the `.pre-restore` file back to the canonical DB path. Invoked
// from every error path that runs after the live DB was renamed.
//
// On success, the operator sees the inner failure reason with a
// note that the original is restored unchanged. If the rename-back
// itself fails, the operator is told to move `.pre-restore` back by
// hand — the assistant cannot lose user data silently.
func restoreFromBackup(dbPath, preDeleteCopy, op string, innerErr error) int {
	// Remove the partial new file (post-rename path).
	_ = os.Remove(dbPath)
	if err := os.Rename(preDeleteCopy, dbPath); err != nil {
		return respond("",
			fmt.Sprintf("Restore failed (%s): %v\nALSO failed to restore original from %s: %v — recover by renaming %s back to %s\n",
				op, innerErr, preDeleteCopy, err, preDeleteCopy, dbPath), 1)
	}
	return respond("",
		fmt.Sprintf("Restore failed (%s): %v\nOriginal database restored unchanged.\n",
			op, innerErr), 1)
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
//
// DSN: must use mpminternal.SqliteWriteDSN so the transient connection
// opens with foreign_keys=ON. A bare path would leave FK enforcement at
// the SQLite default (off), which lets the checkpoint silently drop rows
// that violate foreign-key constraints. The 2026-08-13 audit flagged
// this; locked in by handlers_backup_singleton_test.go::TestFlushWal_PassesForeignKeysPragma.
func flushWal(dbPath string) error {
	db, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(dbPath))
	if err != nil {
		return err
	}
	defer closeSQLDB(db)
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}
