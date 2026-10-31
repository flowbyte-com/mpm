package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// lastUpdateWithBaseline returns the most recent update attempt to
// the given revision that recorded a pre-update schema fingerprint —
// successful or failed. The failed case matters: a degraded stop
// records a failed update whose backup and fingerprint are exactly
// what a later rollback must find. Without this, the recovery point
// from a failed update would be invisible to the documented recovery
// path.
func lastUpdateWithBaseline(hist []HistoryEntry, toRev string) *HistoryEntry {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Action == "update" && hist[i].ToRev == toRev && hist[i].PreFP != "" {
			e := hist[i]
			return &e
		}
	}
	return nil
}

// lastUpdateWithBackup returns the most recent update attempt to the
// given revision that recorded a verified backup — successful or
// failed, for the same reason as above.
func lastUpdateWithBackup(hist []HistoryEntry, toRev string) *HistoryEntry {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Action == "update" && hist[i].ToRev == toRev && hist[i].BackupDir != "" {
			e := hist[i]
			return &e
		}
	}
	return nil
}

// rollbackBaselineDrift enforces the rollback safety rule against a
// given history snapshot and live fingerprint: a runtime rollback to
// a previous release is safe only while the live database still has
// the shape it had before the update that left that release.
//
// It returns the governing history entry (possibly nil) and whether
// the live shape drifted from the recorded pre-update shape. A nil
// entry means no recorded baseline — there is nothing to compare
// against, so no drift. Callers decide what drift means: without
// --with-db-restore it is a refusal, with it the restore overwrites
// the drift explicitly.
func rollbackBaselineDrift(hist []HistoryEntry, liveFP, fromRev string) (*HistoryEntry, bool) {
	entry := lastUpdateWithBaseline(hist, fromRev)
	if entry == nil {
		return nil, false
	}
	return entry, liveFP != entry.PreFP
}

// Rollback moves the active pointer back to the previous release.
//
// Runtime rollback and database rollback are SEPARATE operations, and
// this function performs only the first. The reason is data safety: if
// the update migrated the database into a shape the previous runtime
// never knew, repointing the binaries without restoring the data
// strands the operator on a runtime that cannot read its own database —
// a second outage manufactured by the recovery procedure.
//
// The safety rule: a rollback to R is allowed only while the live
// database still has the shape it had BEFORE the update that left R.
// That shape was recorded in the history entry (pre_fp). If the live
// shape differs, the rollback is REFUSED with the exact recovery
// options — restore the database from the pre-update backup first, or
// re-run with --with-db-restore --force to do both at once (which
// overwrites newer user data and is therefore never automatic).
func Rollback(o Options) (Outcome, error) {
	var out Outcome
	out.Action = "rollback"
	dataRoot := o.root()

	prevTarget := ReadPreviousRelease(dataRoot)
	if prevTarget == "" {
		return out, fmt.Errorf("no previous release is recorded; nothing to roll back to")
	}
	prevRev := previousRevOf(prevTarget)
	out.ToRev = prevRev

	// A database restore overwrites newer user data. It is never
	// implicit and never half-confirmed: without --force it is refused
	// up front, before the lock, before anything moves.
	if o.WithDBRestore && !o.Force {
		return out, &RollbackRefused{
			PrevRev: prevRev,
			Reason:  "--with-db-restore overwrites newer user data; re-run with --force to confirm",
		}
	}

	_, activeRev, cerr := CurrentReleaseTarget(dataRoot)
	out.FromRev = activeRev
	if cerr == nil && activeRev == prevRev {
		out.AlreadyCurrent = true
		out.Report = fmt.Sprintf("already on %s; nothing to roll back", prevRev)
		return out, nil
	}
	if fi, err := os.Stat(NewLayout(dataRoot).ReleaseDir(prevRev)); err != nil || !fi.IsDir() {
		return out, fmt.Errorf("the previous release %s is not installed; rollback needs the release directory (no network is used)", prevRev)
	}

	hist, _ := ReadHistory(dataRoot)
	liveFP, ferr := SchemaFingerprint(NewLayout(dataRoot).LiveDB())
	if ferr != nil {
		return out, fmt.Errorf("could not fingerprint the live database: %w", ferr)
	}
	if entry, drifted := rollbackBaselineDrift(hist, liveFP, activeRev); drifted && !o.WithDBRestore {
		return out, &RollbackRefused{
			PrevRev:   prevRev,
			BackupDir: entry.BackupDir,
			Reason:    "the live database's shape changed since the update (a migration ran); repointing the old binaries without restoring the old data would strand them on a schema they never knew",
		}
	}

	lock, err := AcquireUpdateLock(dataRoot)
	if err != nil {
		return out, err
	}
	defer lock.Release()

	// Re-evaluate the safety condition against the LOCKED state. The
	// pre-lock fingerprint above is advisory (a fast, clear refusal);
	// this is authoritative: the world may have moved between the two
	// reads — another update, a migration, a restore. If the live
	// shape no longer matches the recorded pre-update shape, the
	// baseline the rollback decision rested on is gone: refuse and
	// make the operator re-evaluate, rather than acting on a
	// fingerprint that no longer describes the database.
	lockedHist, _ := ReadHistory(dataRoot)
	lockedFP, ferr := SchemaFingerprint(NewLayout(dataRoot).LiveDB())
	if ferr != nil {
		return out, fmt.Errorf("could not fingerprint the live database: %w", ferr)
	}
	if lockedEntry, lockedDrift := rollbackBaselineDrift(lockedHist, lockedFP, out.FromRev); lockedDrift && !o.WithDBRestore {
		return out, &RollbackRefused{
			PrevRev:   prevRev,
			BackupDir: lockedEntry.BackupDir,
			Reason:    "the live database changed while acquiring the update lock; re-evaluate and retry rather than rolling back against a stale safety check",
		}
	}

	var backupEntry *HistoryEntry
	if o.WithDBRestore {
		hist2, _ := ReadHistory(dataRoot)
		backupEntry = lastUpdateWithBackup(hist2, out.FromRev)
		if backupEntry == nil {
			return out, fmt.Errorf("no verified pre-update backup is recorded for %s; refusing a database restore with no recovery point", out.FromRev)
		}
		if backupEntry.PreFP != "" {
			// Bind the recovery point to its recorded
			// identity BEFORE anything stops or moves: a
			// backup that no longer matches the fingerprint
			// taken at backup time has been replaced or
			// damaged since, and must not become the live
			// database.
			if fp, ferr := SchemaFingerprint(filepath.Join(backupEntry.BackupDir, "mpm.db")); ferr != nil || fp != backupEntry.PreFP {
				detail := "could not fingerprint the recorded backup"
				if ferr == nil {
					detail = "the recorded backup's shape no longer matches its recorded fingerprint"
				} else {
					detail += ": " + ferr.Error()
				}
				recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{}, "", "", "", "failed", detail)
				return out, fmt.Errorf("refusing a database restore: %s (backup: %s)", detail, backupEntry.BackupDir)
			}
		}
		out.BackupDir = backupEntry.BackupDir
	}

	// Service quiescence comes AFTER the lock and the safety checks
	// and BEFORE any mutation: managed units that were active are
	// stopped, and they stay stopped through the restore, the
	// pointer swap and the health verification. They restart only
	// once the intended runtime and database are aligned — or on
	// any failure path, best-effort, so a failed rollback never
	// leaves services silently stopped. Previously inactive units
	// stay inactive: only stopped units are started back.
	sc := o.services()
	var stopped []string
	if o.WithDBRestore && len(sc.ActiveUnits()) > 0 && !o.RestartServices {
		return out, fmt.Errorf("managed service(s) still running (%s); stop them first or re-run with --restart-services (restoring under writers corrupts)", strings.Join(sc.ActiveUnits(), ", "))
	}
	if o.RestartServices {
		stopped = sc.ActiveUnits()
		var startedBack []string
		for _, unit := range stopped {
			if err := sc.Stop(unit); err != nil {
				for _, s := range startedBack {
					_ = sc.Start(s)
				}
				recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{}, out.BackupDir, "", "", "failed", err.Error())
				return out, err
			}
			startedBack = append(startedBack, unit)
		}
	}
	startStopped := func() {
		for _, s := range stopped {
			_ = sc.Start(s)
		}
	}
	fail := func(stage string, err error) (Outcome, error) {
		startStopped()
		recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{}, out.BackupDir, "", "", "failed", stage+": "+err.Error())
		return out, err
	}

	var dbRestoreAccepted []string
	if o.WithDBRestore {
		accepted, err := restoreDatabase(dataRoot, backupEntry.BackupDir, o.Force)
		if err != nil {
			return fail("restore", err)
		}
		for _, u := range accepted {
			dbRestoreAccepted = append(dbRestoreAccepted, fmt.Sprintf("pid %d %s (%s)", u.PID, u.Name, u.Reason))
		}
	}

	if err := restorePointer(dataRoot, prevTarget); err != nil {
		return fail("repoint", fmt.Errorf("could not restore the active pointer: %w", err))
	}
	// The undone release becomes the new "previous": a second rollback
	// returns to it, and the record always names the target the
	// pointer held before the last activation — whichever direction
	// that activation went.
	if out.FromRev != "" {
		_ = RecordPreviousRelease(dataRoot, "releases/"+out.FromRev)
	}
	if err := installStableUpdater(dataRoot, prevRev); err != nil {
		return fail("stable-updater", err)
	}
	live := SmokeLive(dataRoot)
	if !live.OK {
		return fail("health-check", fmt.Errorf("the restored runtime failed its health check: %s", live.Detail))
	}

	// The intended runtime and database are aligned: restart what was
	// stopped and verify the running identities, the same partial-
	// success reporting as the update path. A start failure does not
	// undo the rollback — the pointer and the database are already
	// correct — but it is recorded and reported loudly.
	var identityWarnings []string
	var verifiedUnits []string
	for _, u := range stopped {
		if err := sc.Start(u); err != nil {
			identityWarnings = append(identityWarnings, fmt.Sprintf("%s: restart requested but the start reported an error (%v); confirm it by hand", u, err))
			continue
		}
		if w := verifyServiceIdentity(dataRoot, prevRev, sc, u); w != "" {
			identityWarnings = append(identityWarnings, w)
			continue
		}
		verifiedUnits = append(verifiedUnits, u)
	}
	detail := dbRestoreNote(o.WithDBRestore)
	for _, a := range dbRestoreAccepted {
		detail += "; explicitly accepted unverified process: " + a
	}
	if len(identityWarnings) > 0 {
		detail += "; restart verification warnings: " + strings.Join(identityWarnings, "; ")
	}
	recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{Kind: SourceGitHub}, out.BackupDir, "", "", "success", detail)

	var b strings.Builder
	b.WriteString("")
	fmt.Fprintf(&b, "rolled back %s -> %s\n", displayRev(out.FromRev), prevRev)
	if o.WithDBRestore {
		fmt.Fprintf(&b, "database restored from %s\n", out.BackupDir)
		for _, a := range dbRestoreAccepted {
			fmt.Fprintf(&b, "accepted explicitly-unverified process: %s\n", a)
		}
	} else {
		b.WriteString("database untouched (runtime rollback only)\n")
	}
	if len(stopped) > 0 {
		if len(identityWarnings) == 0 {
			fmt.Fprintf(&b, "restarted: %s (verified against %s)\n", strings.Join(stopped, ", "), prevRev)
		} else {
			fmt.Fprintf(&b, "restarted: %s (with verification warnings — partial success, see below)\n", strings.Join(stopped, ", "))
			if len(verifiedUnits) > 0 {
				fmt.Fprintf(&b, "verified: %s\n", strings.Join(verifiedUnits, ", "))
			}
		}
	} else if len(sc.ActiveUnits()) > 0 {
		fmt.Fprintf(&b, "note: managed service(s) still running (%s) were NOT restarted (no --restart-services); they may still use the old executable until restarted\n", strings.Join(sc.ActiveUnits(), ", "))
	}
	for _, w := range identityWarnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	b.WriteString(staleNote(dataRoot))
	out.Report = b.String()
	return out, nil
}

func dbRestoreNote(withRestore bool) string {
	if withRestore {
		return "runtime and database restored together (--with-db-restore --force)"
	}
	return "runtime rollback only; the database was untouched"
}

// RollbackRefused explains why a one-command rollback was refused and
// lists the exact recovery options. It is a refusal, not a failure: the
// installation is untouched.
type RollbackRefused struct {
	PrevRev   string
	BackupDir string
	Reason    string
}

func (e *RollbackRefused) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing a one-command rollback to %s: %s.\n", e.PrevRev, e.Reason)
	b.WriteString("Recovery options (runtime rollback and database rollback are separate operations):\n")
	if e.BackupDir != "" {
		fmt.Fprintf(&b, "  1. Restore the database from the pre-update backup, then roll back the runtime:\n")
		fmt.Fprintf(&b, "       cp %s/mpm.db <data-root>/src/db/mpm.db   # stop writers first\n", e.BackupDir)
		fmt.Fprintf(&b, "       mpm-update --rollback\n")
		fmt.Fprintf(&b, "  2. Do both at once (OVERWRITES newer user data — never automatic):\n")
		fmt.Fprintf(&b, "       mpm-update --rollback --with-db-restore --force\n")
	}
	b.WriteString("  3. Stay on the current release and investigate the migration instead.\n")
	return b.String()
}

// WritersActiveError refuses a database restore while writers remain.
// It is unbypassable: --force confirms DATA LOSS, never the integrity
// of restoring under writers. The installation is untouched.
type WritersActiveError struct {
	Holders []DBHolder
	// Unverifiable names processes the scan could neither inspect
	// nor rule out. It is set only when the caller did NOT pass
	// --force: with --force the operator explicitly accepts the named
	// residual instead (recorded, never silent).
	Unverifiable []UnverifiableProc
	// Inconclusive means quiescence could not even be established.
	Inconclusive bool
	Detail       string
}

func (e *WritersActiveError) Error() string {
	var b strings.Builder
	if e.Inconclusive {
		fmt.Fprintf(&b, "refusing to restore the database: writer quiescence cannot be established: %s\n", e.Detail)
		b.WriteString("Resolve the observation failure and retry; --force does not bypass this check.")
		return b.String()
	}
	if len(e.Unverifiable) > 0 {
		fmt.Fprintf(&b, "refusing to restore the database: %d process(es) could not be verified as writers or non-writers:\n", len(e.Unverifiable))
		for _, u := range e.Unverifiable {
			fmt.Fprintf(&b, "  pid %d %s (%s)\n", u.PID, u.Name, u.Reason)
		}
		b.WriteString("Re-run with --force to explicitly accept this NAMED residual, or stop the processes and retry.\n")
		b.WriteString("--force confirms overwriting newer data AND accepting the named unknowns; it does not bypass hard evidence.")
		return b.String()
	}
	fmt.Fprintf(&b, "refusing to restore the database: %d process(es) still hold it open:\n", len(e.Holders))
	for _, h := range e.Holders {
		fmt.Fprintf(&b, "  pid %d %s (%s)\n", h.PID, h.Name, h.File)
	}
	b.WriteString("Stop the writers, then retry.\n")
	b.WriteString("--force confirms overwriting newer data; it does not bypass this check.")
	return b.String()
}

// restoreDatabase copies the verified backup over the live database.
//
// WRITER QUIESCENCE. Restoring under writers corrupts: an SQLite
// connection holding the live file (or its WAL) will keep writing to
// the replaced inode or replay the old WAL over the restored copy.
// So the restore is refused while any process holds an open file
// description on the live database or its sidecars — daemons, agent
// sessions, MCP servers, CLIs alike. That check cannot be overridden:
// --force confirms DATA LOSS (overwriting newer user data), not the
// integrity of restoring under writers.
//
// Managed services are stoppable with --restart-services (only the
// units that were active are stopped, and only those are started
// back, so previously inactive services stay inactive). Anything else
// holding the database must be stopped by its owner first.
// checkWriters enforces the restore's writer rule against one scan:
// holders and inconclusive observation always refuse; named-but-
// unverifiable processes refuse unless the operator explicitly
// accepted them with --force.
func checkWriters(scan HolderScan, force bool) ([]UnverifiableProc, error) {
	if scan.Inconclusive {
		return nil, &WritersActiveError{Inconclusive: true, Detail: scan.Detail}
	}
	if len(scan.Holders) > 0 {
		return nil, &WritersActiveError{Holders: scan.Holders}
	}
	if len(scan.Unverifiable) > 0 && !force {
		return nil, &WritersActiveError{Unverifiable: scan.Unverifiable}
	}
	return scan.Unverifiable, nil
}

func restoreDatabase(dataRoot, backupDir string, force bool) ([]UnverifiableProc, error) {
	l := NewLayout(dataRoot)
	live := l.LiveDB()
	raw, err := os.ReadFile(filepath.Join(backupDir, "mpm.db"))
	if err != nil {
		return nil, fmt.Errorf("the recovery point has no database copy: %w", err)
	}
	// The caller stops authorized managed services FIRST (Rollback
	// owns the service lifecycle around the whole operation) and only
	// then calls here: by this point every stoppable writer is quiet,
	// so a remaining holder is unstoppable-by-us and refuses the
	// restore. Named-but-unverifiable processes are accepted only
	// with --force (recorded, never silent).
	accepted, err := checkWriters(ScanDBHolders(live, live+"-wal", live+"-shm"), force)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(live), ".mpm.db.restore.*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	// Verify the staged copy immediately before the destructive
	// rename: the recovery point may have been damaged after it was
	// verified at backup time (bit rot, a half-written copy, operator
	// error). A damaged recovery point must never replace the live
	// database. This deliberately does NOT compare against the live
	// shape — after a migration they are supposed to differ, and that
	// difference is the reason for the restore.
	if err := verifyBackupStandalone(tmpName); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("the recovery point failed pre-restore verification: %w", err)
	}
	// Re-check immediately before the swap: a connection opened after
	// the first scan must abort the restore rather than race it. This
	// narrows the check-then-act window to the rename syscall itself.
	// It cannot close the window — a one-time /proc scan is not an
	// atomic all-writers guarantee — so the --with-db-restore --force
	// confirmation and the recorded outcome remain the backstop, not
	// the scan.
	accepted, err = checkWriters(ScanDBHolders(live, live+"-wal", live+"-shm"), force)
	if err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	if err := os.Rename(tmpName, live); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("could not restore the database: %w", err)
	}
	os.Remove(live + "-wal")
	os.Remove(live + "-shm")
	return accepted, nil
}
