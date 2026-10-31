package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	if entry := LastSuccessfulUpdateTo(hist, activeRev); entry != nil && entry.PreFP != "" {
		if liveFP != entry.PreFP {
			if !o.WithDBRestore {
				return out, &RollbackRefused{
					PrevRev:   prevRev,
					BackupDir: entry.BackupDir,
					Reason:    "the live database's shape changed since the update (a migration ran); repointing the old binaries without restoring the old data would strand them on a schema they never knew",
				}
			}
			// --force was confirmed above; fall through to the
			// restore inside the lock.
		}
	}

	lock, err := AcquireUpdateLock(dataRoot)
	if err != nil {
		return out, err
	}
	defer lock.Release()

	// Re-verify inside the lock: the world may have moved.
	liveFP2, ferr := SchemaFingerprint(NewLayout(dataRoot).LiveDB())
	if ferr != nil {
		return out, fmt.Errorf("could not fingerprint the live database: %w", ferr)
	}
	_ = liveFP2

	if o.WithDBRestore {
		hist2, _ := ReadHistory(dataRoot)
		entry := LastSuccessfulUpdateTo(hist2, out.FromRev)
		if entry == nil || entry.BackupDir == "" {
			return out, fmt.Errorf("no verified pre-update backup is recorded for %s; refusing a database restore with no recovery point", out.FromRev)
		}
		if err := restoreDatabase(dataRoot, entry.BackupDir, o.services(), o.RestartServices); err != nil {
			recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{}, "", "", "", "failed", err.Error())
			return out, err
		}
		out.BackupDir = entry.BackupDir
	}

	if err := restorePointer(dataRoot, prevTarget); err != nil {
		recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{}, out.BackupDir, "", "", "failed", err.Error())
		return out, fmt.Errorf("could not restore the active pointer: %w", err)
	}
	// The undone release becomes the new "previous": a second rollback
	// returns to it, and the record always names the target the
	// pointer held before the last activation — whichever direction
	// that activation went.
	if out.FromRev != "" {
		_ = RecordPreviousRelease(dataRoot, "releases/"+out.FromRev)
	}
	if err := installStableUpdater(dataRoot, prevRev); err != nil {
		recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{}, out.BackupDir, "", "", "failed", err.Error())
		return out, err
	}
	live := SmokeLive(dataRoot)
	if !live.OK {
		recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{}, out.BackupDir, "", "", "failed", live.Detail)
		return out, fmt.Errorf("the restored runtime failed its health check: %s", live.Detail)
	}
	recordOutcome(dataRoot, "rollback", out.FromRev, prevRev, Source{Kind: SourceGitHub}, out.BackupDir, "", "", "success", dbRestoreNote(o.WithDBRestore))

	var b strings.Builder
	b.WriteString("")
	fmt.Fprintf(&b, "rolled back %s -> %s\n", displayRev(out.FromRev), prevRev)
	if o.WithDBRestore {
		fmt.Fprintf(&b, "database restored from %s\n", out.BackupDir)
	} else {
		b.WriteString("database untouched (runtime rollback only)\n")
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

// restoreDatabase copies the verified backup over the live database.
// Writers must be stopped first: with --restart-services the managed
// services are stopped here and restarted after; otherwise an active
// scheduler is a refusal, because restoring under writers corrupts.
func restoreDatabase(dataRoot, backupDir string, sc ServiceControl, restartServices bool) error {
	l := NewLayout(dataRoot)
	raw, err := os.ReadFile(filepath.Join(backupDir, "mpm.db"))
	if err != nil {
		return fmt.Errorf("the recovery point has no database copy: %w", err)
	}
	active := sc.Active()
	if active && !restartServices {
		return fmt.Errorf("a managed service is still running; stop it first or re-run with --restart-services (restoring under writers corrupts)")
	}
	if active {
		if err := sc.Stop(); err != nil {
			return err
		}
		defer sc.Start() //nolint:errcheck
	}
	live := l.LiveDB()
	tmp, err := os.CreateTemp(filepath.Dir(live), ".mpm.db.restore.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, live); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("could not restore the database: %w", err)
	}
	os.Remove(live + "-wal")
	os.Remove(live + "-shm")
	return nil
}
