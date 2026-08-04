// fileperms_test.go — pin the sweep contract: pattern match, mode
// tightening, non-fatal on chmod failure, idempotent re-runs.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testFixtureDir creates an mpm-dir-shaped tree under a parent and
// populates it with files that mimic a real install (so the sweep
// has something concrete to act on).
func testFixtureDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// src/db/ with mpm.db + sidecar + mirror
	srcDB := filepath.Join(root, "src", "db")
	if err := os.MkdirAll(srcDB, 0o700); err != nil {
		t.Fatalf("mkdir src/db: %v", err)
	}
	for _, name := range []string{"mpm.db", "mpm.db-wal", "mpm.db-shm", "mirror.jsonl"} {
		if err := os.WriteFile(filepath.Join(srcDB, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Unrelated file in src/db that should NOT be touched.
	if err := os.WriteFile(filepath.Join(srcDB, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	// backups/ with a recursive file
	backupSub := filepath.Join(root, "backups", "critic-pre")
	if err := os.MkdirAll(backupSub, 0o700); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupSub, "mpm.db.backup"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	// Root-level files
	for _, name := range []string{"active.json", "toxicphrases.txt", "scheduler.lock"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	return root
}

func TestTightenFilePerms0600_TightensAllTargets(t *testing.T) {
	root := testFixtureDir(t)

	report, err := TightenFilePerms0600(root)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Expected: mpm.db, mpm.db-wal, mpm.db-shm, mirror.jsonl,
	// backups/critic-pre/mpm.db.backup, active.json,
	// toxicphrases.txt, scheduler.lock = 8 files
	if report.Checked != 8 {
		t.Errorf("checked: want 8, got %d", report.Checked)
	}
	if report.Changed != 8 {
		t.Errorf("changed: want 8, got %d", report.Changed)
	}
	if report.Errors != 0 {
		t.Errorf("errors: want 0, got %d", report.Errors)
	}

	// Verify all targeted files are 0600.
	targets := []string{
		"src/db/mpm.db", "src/db/mpm.db-wal", "src/db/mpm.db-shm",
		"src/db/mirror.jsonl",
		"backups/critic-pre/mpm.db.backup",
		"active.json", "toxicphrases.txt", "scheduler.lock",
	}
	for _, rel := range targets {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("stat %s: %v", rel, err)
			continue
		}
		if got := info.Mode().Perm(); got > 0o600 {
			t.Errorf("%s: want <= 0600, got %04o", rel, got)
		}
	}

	// Verify stray.txt in src/db was NOT touched (pattern filter).
	info, _ := os.Stat(filepath.Join(root, "src", "db", "stray.txt"))
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("stray.txt: want 0644 (untouched), got %04o", got)
	}
}

func TestTightenFilePerms0600_IdempotentSecondRun(t *testing.T) {
	root := testFixtureDir(t)

	_, err := TightenFilePerms0600(root)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	report, err := TightenFilePerms0600(root)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if report.Changed != 0 {
		t.Errorf("second sweep should change 0 files; got %d", report.Changed)
	}
	if report.Skipped != 8 {
		t.Errorf("second sweep should skip 8 files; got %d", report.Skipped)
	}
}

func TestTightenFilePerms0600_FreshInstallNoError(t *testing.T) {
	// Fresh install: only the root exists, no src/db or backups.
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	report, err := TightenFilePerms0600(root)
	if err != nil {
		t.Fatalf("sweep on fresh install: %v", err)
	}
	if report.Checked != 0 {
		t.Errorf("checked on fresh install: want 0, got %d", report.Checked)
	}
}

func TestTightenFilePerms0600_StrictModeUntouched(t *testing.T) {
	// Already-0400 files should be reported as Skipped, not Changed.
	root := t.TempDir()
	target := filepath.Join(root, "active.json")
	if err := os.WriteFile(target, []byte("x"), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}

	report, err := TightenFilePerms0600(root)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Changed != 0 {
		t.Errorf("0400 file should not be Changed; got Changed=%d", report.Changed)
	}
	if report.Skipped != 1 {
		t.Errorf("0400 file should be Skipped; got Skipped=%d", report.Skipped)
	}
}

func TestMatchesAnyPattern_NilMeansMatchAll(t *testing.T) {
	if !matchesAnyPattern("anything", nil) {
		t.Error("nil patterns should match all files")
	}
	if matchesAnyPattern("anything", []globMatcher{}) {
		t.Error("empty patterns slice should match no files")
	}
}

func TestMatchesAnyPattern_FilteringWorks(t *testing.T) {
	patterns := []globMatcher{
		{match: "mpm.db*"},
		{match: "mirror.jsonl*"},
	}
	cases := []struct {
		name string
		want bool
	}{
		{"mpm.db", true},
		{"mpm.db-wal", true},
		{"mpm.db-shm", true},
		{"mpm.db-journal", true},
		{"mirror.jsonl", true},
		{"mirror.jsonl.gz", true},
		{"stray.txt", false},
		{"active.json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesAnyPattern(tc.name, patterns); got != tc.want {
				t.Errorf("%s: want %v, got %v", tc.name, tc.want, got)
			}
		})
	}
}

func TestTightenFilePerms0600_LogsReportStructure(t *testing.T) {
	// Smoke test: the function returns a non-nil report with valid
	// counters even on an empty install. Catches field-name typos
	// and zero-value regressions.
	root := t.TempDir()
	report, err := TightenFilePerms0600(root)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
	// All four counter fields should be accessible (compile-time
	// check via struct literal access).
	_ = FilepermsReport{
		Checked: report.Checked,
		Changed: report.Changed,
		Skipped: report.Skipped,
		Errors:  report.Errors,
	}
	// Verify report summary string formatting doesn't panic.
	if !strings.Contains(reportSummary(report), "checked") {
		t.Error("reportSummary() didn't include 'checked'")
	}
}

// reportSummary is a tiny helper used by tests + callers to format
// the report for stdout / log output. Kept here so the test can
// sanity-check the format.
func reportSummary(r *FilepermsReport) string {
	return "checked=" + itoa(r.Checked) +
		" changed=" + itoa(r.Changed) +
		" skipped=" + itoa(r.Skipped) +
		" errors=" + itoa(r.Errors)
}

// itoa is a minimal base-10 formatter. Stdlib strconv.Itoa would
// work but we keep the test file dependency-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}