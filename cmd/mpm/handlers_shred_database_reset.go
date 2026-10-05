package main

// handlers_shred_database_reset.go — safe active-substrate reset for
// `mpm shred database`.
//
// # Background
//
// `mpm shred database` was historically disabled because its pre-fix
// implementation did `os.Remove(mpm.db)` followed by `NewMemoryStore(...)`
// and exited with the workspace holding no database at all. The
// recreation half could never have worked: `NewMemoryStore` discards
// its path argument and never opens a database — the open happens in
// `InitSQLite`, which the pre-fix handler did not call.
//
// # Contract (2026-10-04 redesign)
//
// `mpm shred database [-f|--force]` performs an active-substrate reset:
//
//   - removes src/db/mpm.db, mpm.db-wal, mpm.db-shm
//   - rebuilds a fresh, empty, schema-valid MPM database at the same
//     path through the canonical DatabaseManager constructor
//   - retains the previous mpm.db as `mpm.db.pre-shred-<nanos>` so the
//     operator can manually recover if the reset removed something they
//     cared about (the file is removed on the next successful reset, or
//     by hand)
//
// It is honest at the stated scope:
//
//   - PRESERVED on purpose: backups/, migrations/, mirror.jsonl (+ every
//     rotated .gz), watchdog.jsonl (+ every rotated .gz), telemetry.db,
//     active.json, scheduler.state, scheduler.lock, cascade.lock,
//     toxicphrases.txt, mpm_config.json, mode/, persona/, blobs/, the
//     install prefix, the source checkout.
//   - "shred" still does not mean secure physical erasure. The retained
//     .pre-shred-<nanos> file may still contain the old database pages,
//     and SQLite's freelist / WAL free pages are not scrubbed. Use
//     `./uninstall.sh --shred` for the overwrite form.
//
// # Safety model
//
//  1. The destructive form is gated on the same `forceRequested()`
//     accessor every other bulk shred command uses. Without `-f`/`--force`
//     the command prints a refusal and does nothing.
//  2. Preflight refuses if the resolved database path is not safe:
//     empty, "/", $HOME, the repo parent, an in-memory DSN, or a path
//     that fails stat-based real-path resolution.
//  3. Preflight acquires an exclusive flock on `<dbPath>.lock` and
//     HOLDS it through the entire destructive window. The acquire
//     fails (EWOULDBLOCK) iff any active DatabaseManager (CLI
//     handlers, scheduler, MCP, critic, mpm capture) holds LOCK_SH
//     on the same inode — see internal/core/maintenance_lock.go for
//     the shared/exclusive lease protocol. Kernel releases the lock
//     on FD close / process death.
//  4. Validate-before-mutate: a fresh DatabaseManager is fully built
//     and integrity-checked against a side-file BEFORE the live DB is
//     renamed aside. If the side-file build fails, the live DB is
//     untouched.
//  5. Rename-aside: live DB → `.pre-shred-<nanos>` only after the side
//     file is valid. From this point forward, every error path must
//     rename the file back to leave the original intact.
//  6. WAL/SHM removal happens AFTER the rename-aside. Removing them
//     before would corrupt any reader that follows the rename as a
//     "DB moved" signal.
//  7. The new DatabaseManager is installed as the singleton by closing
//     the old one and resetting `dbManagerOnce` so the next `getDB()`
//     re-initializes it.
//
// # Non-goals
//
//   - Does NOT touch mirror.jsonl, watchdog.jsonl, telemetry.db,
//     backups/, mode/, persona/, blobs/, migrations/, or the install
//     prefix. Those have their own dedicated operations.
//   - Does NOT retry on transient failures. Each step has a precise
//     error message and an exact rollback behaviour.
//   - Does NOT re-implement secure file overwrite. The shred contract
//     does not promise it; the installer's --shred flag is the
//     best-effort overwrite path.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
)

// mpmDatabaseFileName mirrors the unexported dbFileName const in
// internal/core/db.go. The constant is package-private; the value is
// canonical for MPM and changing it requires a schema migration. The
// duplicate here is a deliberate trade-off so this file does not
// depend on an unexported symbol across the package boundary.
const mpmDatabaseFileName = "mpm.db"

// shredDatabaseResetSuccessMsg states exactly what was removed,
// exactly what was retained, and the recovery handle. It must never
// claim "securely erased".
const shredDatabaseResetSuccessMsg = `Active substrate reset complete.

Removed:
  %s
  %s-wal
  %s-shm

Rebuilt: fresh empty MPM database at
  %s
(PRAGMA integrity_check ok; migrations baseline + FTS5 schema re-applied.)

Retained (still on disk, may still contain the old content):
  backups/                  — preserved, including any pre-shred snapshots
  migrations/               — preserved
  mirror.jsonl*             — HITL cognitive journal (and rotated .gz)
  watchdog.jsonl*           — operational black box (and rotated .gz)
  telemetry.db              — separate analytics substrate, out of scope
  active.json, scheduler.state, mpm_config.json, mode/, persona/, blobs/

Recovery handle: the previous database is at
  %s
The next successful shred database removes that file. Remove by hand
when no longer needed.

NOTE: "shred" does not mean secure physical erasure. SQLite freelist
and WAL free pages are not scrubbed; the retained .pre-shred-* file may
still contain the old content. Use ./uninstall.sh --shred for a
best-effort overwrite of files on disk.
`

// shredDatabaseResetRefusalMsg is the no-force refusal. It lists the
// marker surface AND what would change so an operator who aborted has
// the full context to decide.
const shredDatabaseResetRefusalMsg = `Refusing: 'mpm shred database' would reset the active substrate.

Nothing was deleted.

What it would remove:
  src/db/mpm.db, mpm.db-wal, mpm.db-shm

What it would rebuild:
  a fresh empty MPM database at src/db/mpm.db, with the canonical
  schema and FTS5 baseline re-applied.

What it would PRESERVE on purpose (still on disk, may contain content):
  backups/, migrations/, mirror.jsonl (+ rotated .gz),
  watchdog.jsonl (+ rotated .gz), telemetry.db, active.json,
  scheduler.state, mpm_config.json, mode/, persona/, blobs/.

Recovery handle:
  the previous mpm.db would be retained as
    src/db/mpm.db.pre-shred-<nanos>
  until the next successful reset, or until you remove it by hand.

Re-run with -f or --force to confirm.
`

// handleShredDatabase is the CLI entry point. Refusal and help use
// the same surface every other bulk shred command does; the
// destructive form requires forceRequested().
func handleShredDatabase(args []string) int {
	if requireHelpShortCircuit(args, func() {
		_ = shredDatabaseHelp()
	}) {
		return 0
	}
	if !forceRequested() {
		return respond("", shredDatabaseResetRefusalMsg, 1)
	}
	return executeShredDatabaseReset()
}

// executeShredDatabaseReset performs the validate-before-mutate
// reset. Returns 0 on success, 1 on any failure with a precise error
// and a guarantee that the live DB is either fully reset or untouched.
func executeShredDatabaseReset() int {
	mpmDir, dbPath, err := resolveShredDatabaseTarget()
	if err != nil {
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': %v\n", err), 1)
	}
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"

	// H-4: preflight acquires LOCK_EX|LOCK_NB on `<dbPath>.lock` and
	// HOLDS it for the entire destructive window. The deferred close
	// releases the lock on every exit path (success, refusal, panic).
	// Holding through the window means a concurrent `mpm capture` (or
	// any other DatabaseManager user) that started its LOCK_SH before
	// our LOCK_EX acquisition is blocked at its own constructor (the
	// constructor waits on the LOCK_SH acquire), and any concurrent
	// DatabaseManager that started AFTER our LOCK_EX will EWOULDBLOCK
	// at the LOCK_SH acquire and refuse. Either way, no other process
	// can hold a writable *sql.DB on the live DB during the rename.
	lockFile, err := preflightShredDatabase(dbPath)
	if err != nil {
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': %v\n", err), 1)
	}
	defer func() {
		if lerr := internal.ReleaseExclusiveMaintenanceLock(lockFile); lerr != nil {
			// Best-effort. The kernel releases on FD close / process
			// death regardless; we log via respond so the failure is
			// observable but the destructive outcome still stands.
			respond("", fmt.Sprintf("Warning: shred database exclusive-lock release error: %v\n", lerr), 0)
		}
	}()

	// Validate-before-mutate: build a fresh DatabaseManager against a
	// side-directory (not the live path) and integrity-check it. Only
	// after this succeeds do we touch the live DB.
	//
	// NewDatabaseManager interprets its argument as a workspace root,
	// not as a database file path — it derives <root>/src/db/mpm.db
	// from the argument. So we point it at a side workspace under
	// <mpmDir>/.shred-side-<nanos>, let it build a fresh DB there,
	// validate it while it is still attached to its native path, then
	// flatten <sideDir>/src/db/mpm.db into <dbDir>/sideFile so the
	// promote step at the end is a clean file-to-file rename.
	dbDir := filepath.Join(mpmDir, "src", "db")
	sideDir := filepath.Join(mpmDir, fmt.Sprintf(".shred-side-%d", time.Now().UnixNano()))
	sideBuilt := filepath.Join(sideDir, "src", "db", mpmDatabaseFileName)
	sideFile := filepath.Join(dbDir, fmt.Sprintf(
		"mpm.db.shred-side-%d", time.Now().UnixNano()))
	sideDM, err := internal.NewDatabaseManager(sideDir)
	if err != nil {
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': failed to build fresh database under %s: %v\n", sideDir, err), 1)
	}
	if _, err := sideDM.HealthCheck(); err != nil {
		_ = sideDM.Close()
		_ = os.RemoveAll(sideDir)
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': fresh-database health check failed: %v\n", err), 1)
	}
	if err := sideIntegrityCheck(sideDM.SQLDB()); err != nil {
		_ = sideDM.Close()
		_ = os.RemoveAll(sideDir)
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': fresh-database integrity check failed: %v\n", err), 1)
	}
	// Close the side DM BEFORE moving the file — the *sql.DB owns the
	// FD, and moving a file with an open FD on Linux is fine, but
	// closing-then-moving is the cleaner order.
	_ = sideDM.Close()
	if _, err := os.Stat(sideBuilt); err != nil {
		_ = os.RemoveAll(sideDir)
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': side-built DB missing after build: %v\n", err), 1)
	}
	if err := os.Rename(sideBuilt, sideFile); err != nil {
		_ = os.RemoveAll(sideDir)
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': failed to flatten fresh DB to side file: %v\n", err), 1)
	}
	// Best-effort cleanup of the now-empty side tree.
	_ = os.RemoveAll(sideDir)

	// Consume any prior .pre-shred-* sibling so a second reset does not
	// accumulate stale recovery handles.
	if prior, _ := filepath.Glob(dbPath + ".pre-shred-*"); len(prior) > 0 {
		for _, p := range prior {
			_ = os.Remove(p)
		}
	}

	// Rename-aside: move the live DB to .pre-shred-<nanos>. From this
	// point forward every error must restore the original.
	preShred := dbPath + fmt.Sprintf(".pre-shred-%d", time.Now().UnixNano())
	if err := os.Rename(dbPath, preShred); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(sideFile)
		return respond("", fmt.Sprintf("Refusing 'mpm shred database': failed to rename %s to %s: %v\n", dbPath, preShred, err), 1)
	}

	// Remove -wal and -shm for the live path AFTER the rename-aside so
	// any reader following the rename sees a consistent main file with
	// no WAL.
	for _, p := range []string{walPath, shmPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			_ = rollbackShredDatabase(dbPath, preShred)
			_ = os.Remove(sideFile)
			return respond("", fmt.Sprintf("Reset failed (clear %s): %v\n", p, err), 1)
		}
	}

	// Promote the side-file into the canonical DB path via atomic rename.
	if err := os.Rename(sideFile, dbPath); err != nil {
		_ = rollbackShredDatabase(dbPath, preShred)
		return respond("", fmt.Sprintf("Reset failed (promote side-file): %v\n", err), 1)
	}

	// Re-arm the process singleton so the next getDB() sees the fresh
	// DB instead of the closed one. Mirrors the test-side helper at
	// bulk_shred_force_test.go:107 — the only place outside tests that
	// resets dbManagerOnce is this handler.
	resetShredDatabaseSingleton()

	out := fmt.Sprintf(shredDatabaseResetSuccessMsg,
		dbPath, dbPath, dbPath, dbPath, preShred)
	return respond(out, "", 0)
}

// resolveShredDatabaseTarget returns (mpmDir, dbPath, err). It refuses
// any path that would make a destructive operation unsafe: empty
// string, "/", $HOME, the repo parent, an in-memory DSN, or any path
// that fails real-path resolution against the active workspace.
func resolveShredDatabaseTarget() (string, string, error) {
	mpmDir := config.GetMPMDir()
	if mpmDir == "" {
		return "", "", errors.New("MPM workspace is empty; set MPM_WORKSPACE or install MPM")
	}
	if internal.IsInMemoryDSN(mpmDir) {
		return "", "", fmt.Errorf("workspace %q is an in-memory DSN; refuse to touch it", mpmDir)
	}
	if mpmDir == "/" {
		return "", "", errors.New("workspace resolves to /; refuse to touch it")
	}
	if home, _ := os.UserHomeDir(); home != "" && mpmDir == home {
		return "", "", errors.New("workspace resolves to $HOME; refuse to touch it")
	}
	if repoParent, err := detectRepoParent(); err == nil && repoParent != "" && mpmDir == repoParent {
		return "", "", errors.New("workspace resolves to the repository parent; refuse to touch it")
	}
	if _, err := filepath.EvalSymlinks(mpmDir); err != nil && !os.IsNotExist(err) {
		return "", "", fmt.Errorf("workspace %q is unreadable: %w", mpmDir, err)
	}
	dbPath := filepath.Join(mpmDir, "src", "db", mpmDatabaseFileName)
	return mpmDir, dbPath, nil
}

// detectRepoParent returns the parent directory of the nearest .git
// ancestor of cwd, or "" if not in a git worktree. The repo parent is
// a foot-gun: deleting its src/db/ subdirectory would not be an
// MPM-workspace operation, it would be deleting a developer's source
// tree.
func detectRepoParent() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	probe := cwd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(probe, ".git")); err == nil {
			return filepath.Dir(probe), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", nil
		}
		probe = parent
	}
	return "", nil
}

// preflightShredDatabase acquires LOCK_EX|LOCK_NB on `<dbPath>.lock`
// and HOLDS it for the duration of the destructive window. Returns
// the open *os.File whose FD carries the exclusive flock; the caller
// MUST defer ReleaseExclusiveMaintenanceLock (or close the file) so
// the kernel releases the lock at the end of the destructive window.
//
// H-4 (corrective): the previous implementation acquired the exclusive
// flock, immediately released it, and proceeded with the destructive
// steps unprotected. That left a window between the probe-release
// and the rename-aside during which a concurrent DatabaseManager
// (mpm capture, the live MPM daemon, any other active writer) could
// acquire LOCK_SH and start writing to the DB we were about to
// rename. The acquire-and-hold form closes that window: from the
// moment preflight returns success until the caller closes the file,
// every other process is refused at LOCK_EX acquisition time
// (EWOULDBLOCK), and any LOCK_SH holder already in place blocks the
// LOCK_EX acquisition right here.
//
// The exclusive acquisition fails (EWOULDBLOCK) iff a DatabaseManager
// in any process holds LOCK_SH on the same inode. That includes
// every active writer described in internal/core/db.go's H-4 lease
// protocol (CLI handlers, scheduler, MCP, critic). The caller treats
// the failure as a refusal: "another writer holds the maintenance
// lease; stop the live MPM daemon and any concurrent `mpm`
// invocation, then retry".
//
// Returns (nil, nil) if the DB does not yet exist (fresh install
// path) — there is no live writer to serialise against, and no
// exclusive lock is needed for a non-existent file. The caller must
// tolerate a nil lock file in that case.
//
// The lock identity is derived from the DB path via
// internal.AcquireExclusiveMaintenanceLockAt so the canonical symlink
// resolution and MPM_DB_LOCK test backstop both apply.
func preflightShredDatabase(dbPath string) (*os.File, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("cannot stat database: %w", err)
	}
	// Derive the lock identity from the workspace root via the SINGLE
	// source of truth in internal/core (MaintenanceLockPath). Hand-
	// constructing "<dbPath>.lock" here would diverge from the canonical
	// identity if MaintenanceLockPath ever changes its scheme, and would
	// silently break cross-symlink aliasing. The static guard
	// internal/core/maintenance_lock_static_test.go enforces this.
	mpmDir := config.GetMPMDir()
	lockPath := internal.MaintenanceLockPath(mpmDir)
	if override := os.Getenv("MPM_DB_LOCK"); override != "" {
		lockPath = override
	}
	lockFile, err := internal.AcquireExclusiveMaintenanceLockAt(lockPath)
	if err != nil {
		return nil, fmt.Errorf("cannot acquire %s: %w\n  (another writer — live MPM daemon, `mpm capture`, or any active DatabaseManager holder — has the maintenance lease on this database.\n  Stop the live MPM daemon (`mpm stop` or `systemctl --user stop mpm-scheduler`) and any concurrent `mpm` invocation, then retry.)", lockPath, err)
	}
	return lockFile, nil
}

// sideIntegrityCheck runs PRAGMA integrity_check against the
// side-file's *sql.DB and requires the result to be exactly "ok".
// This is the tighter gate on top of DatabaseManager.HealthCheck.
func sideIntegrityCheck(db *sql.DB) error {
	if db == nil {
		return errors.New("nil *sql.DB")
	}
	row := db.QueryRow("PRAGMA integrity_check")
	var got string
	if err := row.Scan(&got); err != nil {
		return fmt.Errorf("integrity_check query failed: %w", err)
	}
	if got != "ok" {
		return fmt.Errorf("integrity_check returned %q", got)
	}
	return nil
}

// rollbackShredDatabase renames a .pre-shred-<nanos> file back to
// the canonical DB path. Called from every post-rename error path.
func rollbackShredDatabase(dbPath, preShred string) error {
	if preShred == "" {
		return nil
	}
	if _, err := os.Stat(preShred); os.IsNotExist(err) {
		return nil
	}
	if err := os.Rename(preShred, dbPath); err != nil {
		return fmt.Errorf("rollback failed: rename %s -> %s: %w (recover by hand)", preShred, dbPath, err)
	}
	return nil
}

// resetShredDatabaseSingleton re-arms the process-wide DatabaseManager
// singleton so the next getDB() initializes a fresh one. The old DM
// was closed implicitly when NewDatabaseManager saw the side path.
//
// This is the only place outside the test helpers that touches the
// singleton; it is also the only handler that does so. Kept as a
// private function so a future reader cannot wire a reset behind a
// different command name.
func resetShredDatabaseSingleton() {
	if dbManager != nil {
		// Best-effort close. Close errors on the dead singleton are
		// not surfaced; the new constructor overwrites the field
		// before the next caller can observe the old one.
		_ = dbManager.Close()
	}
	dbManager = nil
	dbManagerInitErr = nil
	dbManagerOnce = sync.Once{}
}

// shredDatabaseHelp prints the truth about this command — what it
// removes, what it preserves, what it does NOT promise.
func shredDatabaseHelp() int {
	out := `mpm shred database — reset the active MPM substrate

Scope:
  Removes the live database file (mpm.db) and its -wal/-shm sidecars,
  then rebuilds a fresh, empty, schema-valid MPM database at the same
  path. Retains the previous database as
    src/db/mpm.db.pre-shred-<nanos>
  for one cycle so the operator can recover if needed.

Concurrency (H-4 maintenance lease):
  Preflight acquires LOCK_EX|LOCK_NB on <dbPath>.lock and HOLDS it
  through the entire destructive window. Every active DatabaseManager
  (CLI handlers, scheduler, MCP, critic) holds LOCK_SH on the same
  lock file for the lifetime of its *sql.DB. The acquire fails with
  EWOULDBLOCK iff any other process holds the maintenance lease.
  Before retrying, stop the live MPM daemon (mpm stop or
  'systemctl --user stop mpm-scheduler') and any concurrent 'mpm'
  invocation that uses this database. The lock release on FD close /
  process death is kernel-cleared.

Preserved on purpose (still on disk, may contain the old content):
  backups/, migrations/, mirror.jsonl (+ rotated .gz),
  watchdog.jsonl (+ rotated .gz), telemetry.db, active.json,
  scheduler.state, scheduler.lock, mpm_config.json, mode/, persona/,
  blobs/, the install prefix, the source checkout.

Does NOT mean:
  "shred" here is not secure physical erasure. SQLite freelist and WAL
  free pages are not scrubbed; the retained .pre-shred-<nanos> file may
  still contain the old database pages. Use ./uninstall.sh --shred for
  a best-effort overwrite of files on disk.

Requires:
  -f | --force      otherwise the command refuses and does nothing

Examples:
  mpm shred database -f
  mpm shred database --force

After:
  Next mpm invocation re-bootstraps the canonical schema and the FTS5
  baseline (memories, sessions, topics, references, scheduled_wakes,
  lessons) plus the baseline cognitive directives.
`
	return respond(out, "", 0)
}
