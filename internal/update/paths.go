package update

import (
	"os"
	"path/filepath"
)

// Layout paths. The data root is resolved WITHOUT creating anything:
// read-only surfaces (--check, --status) must not materialise
// directories as a side effect. Creation happens explicitly, with 0700,
// at the point a mutation is authorised.
const (
	// StableUpdaterName is the stable updater copy at the top of the
	// data root.
	//
	// WHY HERE AND NOT IN bin/: in the versioned layout bin/ is a
	// symlink into the ACTIVE bundle (bin -> current/bin). Anything
	// placed there resolves into a release and dies with it — which is
	// exactly what must not happen to the recovery tool. The top level
	// of the data root is state, so a real file there survives version
	// switches and a broken `current` pointer alike. The operator runs
	// it directly as ~/.mpm/mpm-update when the active runtime is
	// broken.
	StableUpdaterName = "mpm-update"

	// UpdateLockName serialises concurrent updaters. It is separate
	// from install.sh's .mpm-install.lock and .mpm-bundle.lock on
	// purpose: the updater must never contend with the installer's
	// lock discipline, and the installer is out of this PR's touch
	// boundary.
	UpdateLockName = ".mpm-update.lock"

	// PreviousReleaseName records the pointer target before the last
	// activation. It is the same name install.sh uses
	// ($DATA_ROOT/.previous-release), so either tool can read what the
	// other wrote.
	PreviousReleaseName = ".previous-release"

	// HistoryName is the append-only record of update outcomes.
	HistoryName = "update-history.jsonl"

	// LastBackupName records the newest recovery point.
	LastBackupName = ".last-update-backup"

	// ReleasesDirName, CurrentLinkName and StagingDirName are the
	// versioned-layout names shared with runtime_bundle.sh.
	ReleasesDirName = "releases"
	CurrentLinkName = "current"
	StagingDirName  = "staging"
	BackupsDirName  = "backups"
)

// DataRoot resolves the MPM data root without creating anything.
//
// Priority: $MPM_WORKSPACE (explicit override), then $HOME/.mpm. This
// mirrors config.GetMPMDir's resolution minus its MkdirAll side effect,
// because --check and --status are read-only and must stay that way.
func DataRoot() string {
	if env := os.Getenv("MPM_WORKSPACE"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if cwd, cerr := os.Getwd(); cerr == nil {
			return cwd
		}
		return "."
	}
	return filepath.Join(home, ".mpm")
}

// Layout derives every path the updater needs from a data root.
type Layout struct {
	Root string
}

// NewLayout returns the layout for a data root.
func NewLayout(root string) Layout { return Layout{Root: root} }

func (l Layout) join(elem ...string) string {
	return filepath.Join(append([]string{l.Root}, elem...)...)
}

// Releases is <root>/releases.
func (l Layout) Releases() string { return l.join(ReleasesDirName) }

// Current is <root>/current.
func (l Layout) Current() string { return l.join(CurrentLinkName) }

// Staging is <root>/staging.
func (l Layout) Staging() string { return l.join(StagingDirName) }

// Backups is <root>/backups.
func (l Layout) Backups() string { return l.join(BackupsDirName) }

// StableUpdater is <root>/mpm-update.
func (l Layout) StableUpdater() string { return l.join(StableUpdaterName) }

// LockFile is <root>/.mpm-update.lock.
func (l Layout) LockFile() string { return l.join(UpdateLockName) }

// PreviousRelease is <root>/.previous-release.
func (l Layout) PreviousRelease() string { return l.join(PreviousReleaseName) }

// History is <root>/update-history.jsonl.
func (l Layout) History() string { return l.join(HistoryName) }

// LastBackup is <root>/.last-update-backup.
func (l Layout) LastBackup() string { return l.join(LastBackupName) }

// LiveDB is <root>/src/db/mpm.db.
func (l Layout) LiveDB() string { return l.join("src", "db", "mpm.db") }

// ReleaseDir is <root>/releases/<rev>.
func (l Layout) ReleaseDir(rev string) string { return filepath.Join(l.Releases(), rev) }
