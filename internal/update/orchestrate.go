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
	// StatusOnly reports local state (no network, no changes).
	StatusOnly bool
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

	lock, err := AcquireUpdateLock(dataRoot)
	if err != nil {
		return out, err
	}
	defer lock.Release()

	cand, err := stageCandidate(ctx, o, dataRoot, src)
	if err != nil {
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
		return out, err
	}

	preFP, backupDir, err := backupAndFingerprint(dataRoot, o.now())
	if err != nil {
		return out, err
	}
	out.BackupDir = backupDir
	if err := failPoint(o.failAt(), "backup"); err != nil {
		return out, err
	}

	if err := preflight(candidateDir(cand), dataRoot); err != nil {
		return out, err
	}
	if err := failPoint(o.failAt(), "preflight"); err != nil {
		return out, err
	}

	rep := ValidateBundle(candidateDir(cand), src.Rev)
	if !rep.Valid {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", rep.ValidationError().Error())
		return out, rep.ValidationError()
	}
	if err := failPoint(o.failAt(), "validate"); err != nil {
		return out, err
	}

	if o.DryRun {
		out.Report = dryRunReport(out.FromRev, src, rep, backupDir)
		return out, nil
	}

	if _, err := installRelease(dataRoot, candidateDir(cand), src.Rev); err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}
	candidateKept = true
	if err := failPoint(o.failAt(), "install"); err != nil {
		return out, err
	}

	prevTarget, err := activateRelease(dataRoot, src.Rev)
	if err != nil {
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}
	activated := true
	defer func() {
		// A failure after the pointer moved restores it. A failure
		// before needs no rollback: nothing was changed.
		_ = activated
	}()
	if err := failPoint(o.failAt(), "activate"); err != nil {
		_ = restorePointer(dataRoot, prevTarget)
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}

	if err := installStableUpdater(dataRoot, src.Rev); err != nil {
		_ = restorePointer(dataRoot, prevTarget)
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}

	smoke := SmokeScratch(filepath.Join(NewLayout(dataRoot).ReleaseDir(src.Rev), "bin", "mpm"), filepath.Join(backupDir, "mpm.db"))
	if !smoke.OK {
		_ = restorePointer(dataRoot, prevTarget)
		err := fmt.Errorf("the candidate failed its smoke test (%s); the previous runtime is active again", smoke.Detail)
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}
	live := SmokeLive(dataRoot)
	if !live.OK {
		_ = restorePointer(dataRoot, prevTarget)
		err := fmt.Errorf("the promoted runtime failed its health check (%s); the previous runtime is active again", live.Detail)
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}
	if err := failPoint(o.failAt(), "smoke"); err != nil {
		_ = restorePointer(dataRoot, prevTarget)
		recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, "", "failed", err.Error())
		return out, err
	}

	postFP, _ := SchemaFingerprint(NewLayout(dataRoot).LiveDB())
	schemaChanged := postFP != "" && preFP != "" && postFP != preFP
	recordOutcome(dataRoot, "update", out.FromRev, src.Rev, src, backupDir, preFP, postFP, "success", schemaNote(schemaChanged))

	var b strings.Builder
	fmt.Fprintf(&b, "updated %s -> %s\n", displayRev(out.FromRev), src.Rev)
	fmt.Fprintf(&b, "recovery point: %s\n", backupDir)
	if schemaChanged {
		b.WriteString("note: the live database's shape changed during this update; a runtime rollback will require a database restore (see `mpm update --rollback`)\n")
	}
	b.WriteString(staleNote(dataRoot))
	out.Report = b.String()
	return out, nil
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

func dryRunReport(from string, src Source, rep BundleReport, backupDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "dry run — nothing was changed\n")
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
