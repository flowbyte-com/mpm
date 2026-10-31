package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options control one update run. The zero value is a dry check of the
// default source; every mutation requires an explicit field.
type Options struct {
	// DataRoot overrides DataRoot() (tests).
	DataRoot string
	// CheckOnly resolves the candidate and reports, changing nothing.
	CheckOnly bool
	// DryRun plans the promotion without changing anything.
	DryRun bool
	// Rollback repoints at the previous release instead of promoting.
	Rollback bool
	// WithDBRestore pairs with Rollback to restore the pre-update
	// database backup. Requires Force.
	WithDBRestore bool
	// Force authorises the destructive half of --with-db-restore.
	Force bool
	// RestartServices authorises stopping/restarting the managed
	// services. Without it the updater only reports.
	RestartServices bool
	// Services probes managed services. Nil means the production
	// systemctl implementation. Tests inject QuietServices so no
	// test ever touches live services.
	Services ServiceControl
	// AllowDirty permits building from a dirty local checkout
	// (development override). Without it a dirty tree is refused.
	AllowDirty bool
	// WantVersion pins a GitHub release tag ("" means latest stable).
	WantVersion string
	// WantBranch / WantCommit request an explicit git revision.
	WantBranch string
	WantCommit string
	// WantLocal requests a build from a local checkout.
	WantLocal string
	// Fetcher resolves/downloads releases (default: GitHub).
	Fetcher ReleaseFetcher
	// Builder builds git/local checkouts (default: GoBuilder).
	Builder Builder
	// Now supplies timestamps (tests override).
	Now func() time.Time
	// FailAt names a step to fail after, for failure-injection tests
	// (also honoured from MPM_UPDATE_TEST_FAIL_AT).
	FailAt string
}

// Outcome is the machine-readable result of one run.
type Outcome struct {
	// Action is "check", "status", "update" or "rollback".
	Action string
	// AlreadyCurrent is true when no change was needed.
	AlreadyCurrent bool
	// FromRev / ToRev bracket the move.
	FromRev string
	ToRev   string
	// Commit is the exact commit the candidate records.
	Commit string
	// BackupDir is the recovery point ("" when no backup was taken).
	BackupDir string
	// Report is the human rendering.
	Report string
}

// Status is the read-only local state (--status: no network).
type Status struct {
	ActiveRev     string
	ActiveCommit  string
	ActiveVersion string
	PreviousRev   string
	History       []HistoryEntry
	Services      []ServiceHealth
	Stale         StaleReport
	StableUpdater bool
}

// GetStatus reports local state without touching the network.
func GetStatus(dataRoot string) Status {
	var st Status
	if _, rev, err := CurrentReleaseTarget(dataRoot); err == nil {
		st.ActiveRev = rev
		if rel, rerr := ReadRelease(NewLayout(dataRoot).ReleaseDir(rev)); rerr == nil {
			st.ActiveCommit = rel.Commit
			st.ActiveVersion = rel.Version
		}
	}
	st.PreviousRev = previousRevOf(ReadPreviousRelease(dataRoot))
	st.History, _ = ReadHistory(dataRoot)
	st.Services = CheckServices()
	st.Stale, _ = ScanLiveProcesses(dataRoot)
	if LocateStableUpdater(dataRoot) != "" {
		st.StableUpdater = true
	}
	return st
}

func previousRevOf(target string) string {
	if target == "" {
		return ""
	}
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

// failAt resolves the failure-injection point.
func (o Options) failAt() string {
	if o.FailAt != "" {
		return o.FailAt
	}
	return os.Getenv("MPM_UPDATE_TEST_FAIL_AT")
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) root() string {
	if o.DataRoot != "" {
		return o.DataRoot
	}
	return DataRoot()
}

func (o Options) fetcher() ReleaseFetcher { return o.FetcherOrDefault() }

// FetcherOrDefault returns the configured fetcher or GitHub.
func (o Options) FetcherOrDefault() ReleaseFetcher {
	if o.Fetcher != nil {
		return o.Fetcher
	}
	return GitHubFetcher{}
}

func (o Options) services() ServiceControl {
	if o.Services != nil {
		return o.Services
	}
	return SystemdServices{}
}

func (o Options) builderOrDefault() Builder {
	if o.Builder != nil {
		return o.Builder
	}
	return GoBuilder{}
}

// Check resolves the candidate for the requested source and reports
// whether an update is available. It changes nothing.
func Check(ctx context.Context, o Options) (Outcome, error) {
	src, err := resolveSource(ctx, o)
	if err != nil {
		return Outcome{}, err
	}
	_, activeRev, cerr := CurrentReleaseTarget(o.root())
	out := Outcome{Action: "check", ToRev: src.Rev, Commit: src.Commit, FromRev: activeRev}
	if cerr != nil {
		out.Report = fmt.Sprintf("no active release; candidate %s (%s) is available", src.Rev, ShortSHA(src.Commit))
		return out, nil
	}
	if activeRev == src.Rev {
		out.AlreadyCurrent = true
		out.Report = fmt.Sprintf("already up to date at %s", activeRev)
		return out, nil
	}
	out.Report = fmt.Sprintf("update available: %s -> %s", activeRev, src.Rev)
	return out, nil
}

// Update runs the full safety workflow:
//
//  1. identify the current release and the requested candidate;
//  2. acquire the update lock (reject concurrent updates);
//  3. validate the candidate's identity and compatibility;
//  4. stage the candidate outside the active release;
//  5. take and verify a SQLite online backup;
//  6. check schema compatibility and migration requirements;
//  7. validate all binaries, plugin payloads, manifests and metadata;
//  8. atomically activate the candidate;
//  9. verify installed build identities and service health;
//  10. record the previous release and the outcome;
//  11. preserve rollback material.
//
// Steps 1 and the reporting half of 10 happen outside the lock; every
// mutation from staging onward holds it.
func Update(ctx context.Context, o Options) (Outcome, error) {
	var out Outcome
	out.Action = "update"
	dataRoot := o.root()

	if err := failPoint(o.failAt(), "resolve"); err != nil {
		return out, err
	}
	src, err := resolveSource(ctx, o)
	if err != nil {
		return out, err
	}
	out.ToRev = src.Rev
	out.Commit = src.Commit

	_, activeRev, cerr := CurrentReleaseTarget(dataRoot)
	out.FromRev = activeRev
	if cerr == nil && activeRev == src.Rev && !schemaDriftSuspected(dataRoot) {
		out.AlreadyCurrent = true
		out.Report = fmt.Sprintf("already up to date at %s", activeRev)
		return out, nil
	}

	// --dry-run is nonmutating: the candidate is staged in a temp
	// directory outside the data root, no backup is taken, no lock is
	// held and nothing is recorded. It answers "would this validate?"
	// and keeps nothing. --check is even cheaper (resolve only).
	if o.DryRun {
		return dryRunUpdate(ctx, o, dataRoot, src, &out)
	}

	lock, err := AcquireUpdateLock(dataRoot)
	if err != nil {
		return out, err
	}
	defer lock.Release()

	cand, err := stageCandidate(ctx, o, NewLayout(dataRoot).Staging(), src)
	if err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, "", "", "", "failed", err.Error())
		return out, err
	}
	// The staging area is disposable: a failure anywhere below discards
	// the candidate and leaves the installation exactly as it was.
	// Removal is best-effort with a permission repair first: a source
	// build runs the Go toolchain, whose module cache files are
	// read-only, and a plain RemoveAll would leave them behind.
	candidateKept := false
	defer func() {
		if !candidateKept {
			removeWorkDir(cand.WorkDir)
		}
	}()
	if err := failPoint(o.failAt(), "stage"); err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, "", "", "", "failed", err.Error())
		return out, err
	}

	preFP, backupDir, err := backupAndFingerprint(dataRoot, o.now())
	if err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, "", preFP, "", "failed", err.Error())
		return out, err
	}
	out.BackupDir = backupDir
	if err := failPoint(o.failAt(), "backup"); err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}

	if err := preflight(candidateDir(cand), dataRoot); err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}
	if err := failPoint(o.failAt(), "preflight"); err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}

	rep := ValidateBundle(candidateDir(cand), src.Rev)
	if !rep.Valid {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", rep.ValidationError().Error())
		return out, rep.ValidationError()
	}
	if err := failPoint(o.failAt(), "validate"); err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}

	if o.DryRun {
		out.Report = dryRunReport(out.FromRev, src, rep)
		return out, nil
	}

	// Quiescence. With --restart-services, the managed units that are
	// active are stopped before the pointer moves, so no old-binary
	// writer is mid-flight during activation, and started back
	// afterwards. Only stopped units are started back: previously
	// inactive services stay inactive.
	sc := o.services()
	var stoppedUnits []string
	var stoppedSoFar []string
	if o.RestartServices {
		stoppedUnits = sc.ActiveUnits()
		for _, u := range stoppedUnits {
			if err := sc.Stop(u); err != nil {
				for _, s := range stoppedSoFar {
					_ = sc.Start(s)
				}
				recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
				return out, err
			}
			stoppedSoFar = append(stoppedSoFar, u)
		}
	}
	restoreStopped := func() {
		for _, s := range stoppedSoFar {
			_ = sc.Start(s)
		}
	}

	failAfterStop := func(stage string, err error) (Outcome, error) {
		restoreStopped()
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}

	if _, err := installRelease(dataRoot, candidateDir(cand), src.Rev); err != nil {
		return failAfterStop("install", err)
	}
	candidateKept = true
	if err := failPoint(o.failAt(), "install"); err != nil {
		return failAfterStop("install", err)
	}

	prevTarget, err := activateRelease(dataRoot, src.Rev)
	if err != nil {
		return failAfterStop("activate", err)
	}
	// A failure after the pointer moved either falls back or stops
	// degraded (see below). A failure before needs no rollback:
	// nothing was changed.
	failAfterActivate := func(postFP string, schemaChanged bool, err error) (Outcome, error) {
		if !schemaChanged {
			// The live schema is untouched: falling back restores a
			// consistent state (old binaries on the old shape).
			_ = restorePointer(dataRoot, prevTarget)
			restoreStopped()
			recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, postFP, "failed", err.Error())
			return out, err
		}
		// The new runtime changed the live schema before failing.
		// Restoring the old pointer blindly would strand old binaries
		// on a shape they never knew — a second outage manufactured
		// by the recovery procedure. Verify the previous release can
		// actually read the migrated shape (against a scratch copy,
		// never the live file) before falling back.
		if cerr := oldRuntimeReadsNewSchema(dataRoot, out.FromRev); cerr != nil {
			// DEGRADED STOP. Keep the new pointer: the binaries match
			// the migrated data. Restart what was stopped so the
			// managed services are consistent with the KEPT pointer.
			// Record everything, touch nothing else — in particular,
			// never silently restore the database.
			restoreStopped()
			degraded := fmt.Errorf("%v\nDEGRADED: the live database's shape changed and the previous release cannot read it (%v). The active pointer stays at %s (binaries match the data). Recover with: mpm-update --rollback --with-db-restore --force (restores the pre-update backup %s, overwriting newer data), or fix %s forward", err, cerr, src.Rev, backupDir, src.Rev)
			recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, postFP, "failed", degraded.Error())
			return out, degraded
		}
		_ = restorePointer(dataRoot, prevTarget)
		restoreStopped()
		verified := fmt.Errorf("%v\nNote: the live database's shape changed during this update, but the previous release verified clean against the migrated schema, so the fallback was restored", err)
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, postFP, "failed", verified.Error())
		return out, verified
	}
	if err := failPoint(o.failAt(), "activate"); err != nil {
		return failAfterActivate("", false, err)
	}

	smoke := SmokeScratch(filepath.Join(NewLayout(dataRoot).ReleaseDir(src.Rev), "bin", "mpm"), filepath.Join(backupDir, "mpm.db"))
	if !smoke.OK {
		err := fmt.Errorf("the candidate failed its smoke test (%s); the previous runtime is active again", smoke.Detail)
		return failAfterActivate("", false, err)
	}

	// The live smoke is the point of no return for the SCHEMA: the new
	// binary opens the live database, and a newer binary may apply
	// pending migrations just by opening it. The fingerprint watch
	// observes that, so a later failure cannot pretend the database is
	// untouched.
	live, postFP, schemaChanged := smokeLiveWatched(dataRoot, preFP)
	if !live.OK {
		err := fmt.Errorf("the promoted runtime failed its health check (%s)", live.Detail)
		return failAfterActivate(postFP, schemaChanged, err)
	}
	if err := failPoint(o.failAt(), "smoke"); err != nil {
		return failAfterActivate(postFP, schemaChanged, err)
	}

	// The promoted release passed. Only now is the stable updater
	// replaced: every earlier failure leaves the known-good copy in
	// place, so recovery never depends on a release that never proved
	// itself.
	if err := installStableUpdater(dataRoot, src.Rev); err != nil {
		return failAfterActivate(postFP, schemaChanged, err)
	}

	// Restart what was stopped, then verify the running identities:
	// a restarted daemon must resolve into the new release, not just
	// report active. Only units that verify cleanly count as
	// "verified" — a warning anywhere means partial success at best,
	// and the report must say so instead of claiming a clean restart.
	var identityWarnings []string
	var verifiedUnits []string
	for _, u := range stoppedSoFar {
		if err := sc.Start(u); err != nil {
			identityWarnings = append(identityWarnings, fmt.Sprintf("%s: restart requested but the start reported an error (%v); confirm it by hand", u, err))
			continue
		}
		if w := verifyServiceIdentity(dataRoot, src.Rev, sc, u); w != "" {
			identityWarnings = append(identityWarnings, w)
			continue
		}
		verifiedUnits = append(verifiedUnits, u)
	}
	recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, postFP, "success", successDetail(schemaChanged, identityWarnings))

	var b strings.Builder
	fmt.Fprintf(&b, "updated %s -> %s\n", displayRev(out.FromRev), src.Rev)
	fmt.Fprintf(&b, "recovery point: %s\n", backupDir)
	if schemaChanged {
		b.WriteString("note: the live database's shape changed during this update; a runtime rollback will require a database restore (see `mpm update --rollback`)\n")
	}
	if len(stoppedSoFar) > 0 {
		if len(identityWarnings) == 0 {
			fmt.Fprintf(&b, "restarted: %s (verified against %s)\n", strings.Join(stoppedSoFar, ", "), src.Rev)
		} else {
			fmt.Fprintf(&b, "restarted: %s (with verification warnings — partial success, see below)\n", strings.Join(stoppedSoFar, ", "))
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

// dryRunUpdate validates a candidate without changing or keeping
// anything.
//
// Nothing persistent happens here: the candidate is staged in a temp
// directory (never the data root's staging area), no backup is taken
// (there is nothing to recover), no lock is held (no shared state is
// touched) and nothing is recorded. The preflight reads the live
// database through a temp snapshot, never the live file. What costs
// something is time and network — resolving, downloading or building
// the candidate — which is exactly what --check avoids and --dry-run
// spends on purpose.
func dryRunUpdate(ctx context.Context, o Options, dataRoot string, src Source, out *Outcome) (Outcome, error) {
	work, err := os.MkdirTemp("", "mpm-update-dryrun.")
	if err != nil {
		return *out, err
	}
	defer removeWorkDir(work)
	cand, err := stageCandidate(ctx, o, work, src)
	if err != nil {
		return *out, err
	}
	if err := preflight(candidateDir(cand), dataRoot); err != nil {
		return *out, err
	}
	rep := ValidateBundle(candidateDir(cand), src.Rev)
	if !rep.Valid {
		return *out, rep.ValidationError()
	}
	out.Report = dryRunReport(out.FromRev, src, rep)
	return *out, nil
}

// smokeLiveWatched runs the live health check and fingerprints the
// database before and after, so a migration applied by the new binary
// is observed rather than assumed away.
func smokeLiveWatched(dataRoot string, preFP string) (smoke SmokeResult, postFP string, changed bool) {
	smoke = SmokeLive(dataRoot)
	post, err := SchemaFingerprint(NewLayout(dataRoot).LiveDB())
	if err != nil {
		return smoke, "", false
	}
	return smoke, post, preFP != "" && post != "" && post != preFP
}

// oldRuntimeReadsNewSchema verifies the previous release can read
// the migrated live database — against a scratch copy, never the live
// file. A nil error means falling back to the old pointer is safe;
// anything else means the fallback itself would strand the operator
// and the update must stop degraded instead.
func oldRuntimeReadsNewSchema(dataRoot, fromRev string) error {
	if fromRev == "" {
		return fmt.Errorf("no previous release is installed to fall back to")
	}
	oldMPM := filepath.Join(NewLayout(dataRoot).ReleaseDir(fromRev), "bin", "mpm")
	if fi, err := os.Stat(oldMPM); err != nil || fi.IsDir() {
		return fmt.Errorf("previous release %s has no mpm binary", fromRev)
	}
	smoke := SmokeScratch(oldMPM, NewLayout(dataRoot).LiveDB())
	if !smoke.OK {
		return fmt.Errorf("previous release failed its health check against the migrated schema: %s", smoke.Detail)
	}
	return nil
}

// verifyServiceIdentity confirms a restarted unit resolves into the
// new release. It returns "" when the identity checks out, or a
// warning the operator must see.
func verifyServiceIdentity(dataRoot, rev string, sc ServiceControl, unit string) string {
	pid, err := sc.MainPID(unit)
	if err != nil || pid <= 0 {
		return fmt.Sprintf("%s: restarted but its process could not be identified; confirm it runs %s by hand", unit, rev)
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return fmt.Sprintf("%s (pid %d): executable unreadable; confirm it runs %s by hand", unit, pid, rev)
	}
	return verifyServiceExe(dataRoot, rev, unit, pid, exe)
}

// verifyServiceExe is the pure identity rule: the executable must
// resolve into the new release directory. Split out so tests can pin
// the matching without live processes.
func verifyServiceExe(dataRoot, rev, unit string, pid int, exe string) string {
	want := filepath.Join(dataRoot, ReleasesDirName, rev)
	if exe == want+"/bin/"+unit || strings.HasPrefix(exe, want+"/") {
		return ""
	}
	return fmt.Sprintf("%s (pid %d) runs %s, not the active release %s; restart it by hand", unit, pid, exe, rev)
}

func successDetail(schemaChanged bool, warnings []string) string {
	if schemaChanged {
		return "the live database's shape changed during this update"
	}
	if len(warnings) > 0 {
		return "update succeeded with warnings: " + strings.Join(warnings, "; ")
	}
	return ""
}

func dryRunReport(from string, src Source, rep BundleReport) string {
	var b strings.Builder
	b.WriteString("dry run — nothing was changed and nothing was kept:\n")
	b.WriteString("(no backup was taken, nothing was staged in the data root, nothing was recorded)\n")
	fmt.Fprintf(&b, "would update %s -> %s (%s)\n", displayRev(from), src.Rev, string(src.Kind))
	for _, c := range rep.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %s: %s\n", mark, c.Name, c.Detail)
	}
	return b.String()
}

func candidateDir(cand Candidate) string { return cand.Dir }

// removeWorkDir removes a staging work directory, repairing permissions
// first so read-only build-cache files do not survive cleanup.
func removeWorkDir(dir string) {
	if dir == "" {
		return
	}
	_ = os.Chmod(dir, 0700)
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && fi != nil {
			_ = os.Chmod(p, 0700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}

func schemaDriftSuspected(dataRoot string) bool { return false }

func schemaNote(changed bool) string {
	if changed {
		return "the live database's shape changed during this update"
	}
	return ""
}

func recordOutcome(dataRoot, action, from, to string, src Source, backupDir, preFP, postFP, outcome, detail string) {
	_ = AppendHistory(dataRoot, HistoryEntry{
		Action: action, FromRev: from, ToRev: to,
		Source: string(src.Kind), SourceRef: src.Ref, Commit: src.Commit,
		Outcome: outcome, Detail: detail, BackupDir: backupDir,
		PreFP: preFP, PostFP: postFP, SchemaChanged: preFP != "" && postFP != "" && preFP != postFP,
	})
}

func staleNote(dataRoot string) string {
	rep, err := ScanLiveProcesses(dataRoot)
	if err != nil {
		return "process inventory unavailable on this platform; check running processes by hand\n"
	}
	if len(rep.Stale) == 0 && len(rep.OpenCodeSessions) == 0 {
		return "no stale processes: nothing is running an older release\n"
	}
	return FormatStaleReport(rep)
}

func failPoint(name, step string) error {
	if name == step {
		return fmt.Errorf("injected failure after %s (MPM_UPDATE_TEST_FAIL_AT=%s)", step, name)
	}
	return nil
}
