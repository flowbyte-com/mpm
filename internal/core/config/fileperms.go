// fileperms.go — one-shot migration + targeted tightening of file
// permissions on existing runtime data files.
//
// v spec 2026-08-04: alpha testers who spun up their databases
// before the 0600 enforcement landed have data-plane files at 0644.
// The dir-level fix (0700 on ~/.mpm) blocks foreign read, but
// defense in depth requires the files themselves to be 0600 too.
// This sweep runs once at startup, before any DB connection is
// opened, and applies 0600 to every known data-plane file. Failures
// are logged at WARN and counted in the report — never fatal, since
// a partial sweep is strictly better than refusing to boot.
//
// Targets (matched by basename glob):
//   src/db/mpm.db*        — main DB + WAL/SHM/journal sidecars
//   src/db/mirror.jsonl*  — audit mirror + rotated/compressed siblings
//   backups/**/*         — every file under the backups tree
//   active.json           — runtime agent state
//   toxicphrases.txt      — poison-phrase list
//   scheduler.lock        — flock singleton
//
// Explicitly NOT touched:
//   mode/*.md, persona/*.md — operator-editable config; parent 0700
//     already blocks foreign read. Leaving files at OS default (0644)
//     avoids friction when operators open them in external editors.
//
// Lives in package config next to GetMPMDir and the dir-level
// permcheck, so the entire security surface is discoverable from
// one directory.

package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// FilepermsReport summarises a tightening run.
//
// Errors is the count of per-file chmod failures; the sweep does
// NOT abort on these. Operators inspect the log lines (logged at
// WARN with path + err) to investigate ownership or filesystem
// anomalies.
type FilepermsReport struct {
	Checked int // files inspected
	Changed int // files whose mode we actually modified
	Skipped int // files already <= 0600
	Errors  int // chmod failures (logged at WARN, not fatal)
}

// TightenFilePerms0600 sweeps the runtime data directory and applies
// 0600 to every known data-plane file. Returns a report with counts;
// never returns an error for per-file failures (those are logged and
// counted in report.Errors). Only returns a non-nil error when the
// sweep itself can't start (e.g., the mpm dir doesn't exist — but
// that's already caught upstream by AssertUserDirPerms0700).
//
// Idempotent. Re-running on an already-tightened install is a no-op
// for every file (Skipped++ for each).
func TightenFilePerms0600(mpmDir string) (*FilepermsReport, error) {
	report := &FilepermsReport{}

	// 1. src/db/ — non-recursive, basename glob match.
	if err := tightenDir(mpmDir, "src/db", false, []globMatcher{
		{match: "mpm.db*", note: "main DB + SQLite sidecars"},
		{match: "mirror.jsonl*", note: "audit mirror + rotations"},
	}, report); err != nil {
		return report, err
	}

	// 2. backups/ — recursive, all files.
	if err := tightenDir(mpmDir, "backups", true, nil, report); err != nil {
		return report, err
	}

	// 3. Root-level explicit files.
	for _, rel := range []string{"active.json", "toxicphrases.txt", "scheduler.lock"} {
		path := filepath.Join(mpmDir, rel)
		tightenOneFile(path, rel, report)
	}

	slog.Info("file perms sweep complete",
		"checked", report.Checked,
		"changed", report.Changed,
		"skipped", report.Skipped,
		"errors", report.Errors)

	return report, nil
}

// globMatcher pairs a glob pattern with a free-text note used in
// log lines so operators can see WHY a file was tightened.
type globMatcher struct {
	match string // basename pattern, e.g. "mpm.db*"
	note  string // human-readable context for log lines
}

// tightenDir walks a subdirectory of mpmDir. If recursive is true,
// all files at any depth are tightened; otherwise only direct
// children. If patterns is non-nil, only files whose basename
// matches one of the patterns are touched (filepath.Match glob
// semantics). nil patterns means "every file".
//
// Missing subdirectory is treated as a no-op (returns nil) so the
// sweep is safe to run on a fresh install where backups/ doesn't
// exist yet.
func tightenDir(mpmDir, sub string, recursive bool, patterns []globMatcher, report *FilepermsReport) error {
	root := filepath.Join(mpmDir, sub)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil // fresh install, nothing to tighten yet
		}
		return fmt.Errorf("stat %s: %w", root, err)
	}

	walkFn := func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Log and continue — partial sweep beats fatal abort.
			slog.Warn("file perms sweep: walk error",
				"path", path, "err", walkErr)
			report.Errors++
			return nil
		}
		if d.IsDir() {
			return nil // only files
		}

		base := filepath.Base(path)
		if !matchesAnyPattern(base, patterns) {
			return nil // pattern filter excluded this file
		}

		rel := path[len(mpmDir)+1:] // for log readability
		tightenOneFile(path, rel, report)
		return nil
	}

	if recursive {
		return filepath.WalkDir(root, walkFn)
	}

	// Non-recursive: read directory entries manually.
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", root, err)
	}
	for _, d := range entries {
		if d.IsDir() {
			continue
		}
		path := filepath.Join(root, d.Name())
		base := d.Name()
		if !matchesAnyPattern(base, patterns) {
			continue
		}
		rel := path[len(mpmDir)+1:]
		tightenOneFile(path, rel, report)
	}
	return nil
}

// tightenOneFile applies 0600 to a single file. Logs at WARN on
// failure (e.g., file owned by another user, chmod EPERM). Updates
// the report counters.
//
// Missing files are a quiet no-op: every target in the sweep
// (active.json, toxicphrases.txt, scheduler.lock, the rotated
// mirror.jsonl siblings, backup snapshots) is optional — created
// only when something writes it. ENOENT means "nothing to tighten
// yet", not a stat failure. Real permission/stat errors (EPERM,
// EIO, EACCES, …) still surface as WARN + Errors++ so genuine
// problems stay visible. Without this distinction the sweep
// emitted two WARN lines on every `mpm status` run before any
// dashboard output, polluting CLI startup on hosts where
// nothing was actually wrong.
func tightenOneFile(path, rel string, report *FilepermsReport) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File simply isn't on disk yet — normal on a fresh
			// install. Nothing to tighten, no diagnostic value in
			// logging it. Skip silently.
			return
		}
		slog.Warn("file perms sweep: stat failed",
			"path", rel, "err", err)
		report.Errors++
		return
	}
	report.Checked++

	mode := info.Mode().Perm()
	if mode <= 0o600 {
		report.Skipped++ // already compliant (or stricter; 0o400 allowed)
		return
	}

	if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
		slog.Warn("file perms sweep: chmod failed (manual fix recommended)",
			"path", rel,
			"current_mode", fmt.Sprintf("%04o", mode),
			"err", chmodErr)
		report.Errors++
		return
	}

	slog.Info("file perms tightened",
		"path", rel,
		"previous_mode", fmt.Sprintf("%04o", mode))
	report.Changed++
}

// matchesAnyPattern tests base against each glob. nil patterns
// (sweep-all) returns true; an empty patterns slice (sweep-nothing)
// returns false.
func matchesAnyPattern(base string, patterns []globMatcher) bool {
	if patterns == nil {
		return true
	}
	for _, p := range patterns {
		if ok, _ := filepath.Match(p.match, base); ok {
			return true
		}
	}
	return false
}
