// migrate_lessons_no_warn_test.go — alpha-4.1.2 D-004 regression test.
//
// Audit finding: the auditor flagged that on a fresh database, the first
// call to migrateLessonsToView emitted a WARN-level slog message:
//
//	"WARN migrateLessonsToView: probe lessons_base, treating as absent"
//
// The probe SELECT 1 FROM sqlite_master WHERE name='lessons_base'
// returns sql.ErrNoRows on every fresh install — this is the EXPECTED
// state, not an error. The pre-fix code logged it as WARN anyway,
// polluting the operator's machine-readable slog stream with noise on
// every cold start.
//
// Fix (alpha-4.1.2): the probe now silently treats sql.ErrNoRows as
// "fresh install, expected" and only logs a WARN for actual errors.
//
// This test pins the post-fix contract: building a fresh DatabaseManager
// must not emit the "probe lessons_base, treating as absent" WARN.

package internal

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestMigrateLessonsToView_FreshInstall_NoProbeWarn pins that on a
// fresh database the migrateLessonsToView probe does NOT emit a WARN
// for the expected sql.ErrNoRows path. The test captures slog output
// and asserts on the absence of the specific message.
//
// We don't tear down the global slog default — instead we swap it for
// a buffer-bound handler for the duration of the test, then restore.
func TestMigrateLessonsToView_FreshInstall_NoProbeWarn(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug, // capture everything
	})))
	defer slog.SetDefault(orig)

	// A fresh DM should boot without the legacy probe WARN.
	dm := NewTestDM(t)
	if dm == nil {
		t.Fatal("NewTestDM returned nil")
	}

	output := buf.String()
	// Pin: the specific noisy message must NOT appear.
	if strings.Contains(output, "probe lessons_base, treating as absent") {
		t.Errorf("fresh-boot probe emitted the legacy noisy WARN. Full log:\n%s", output)
	}
	// Sanity: migration DID actually run (positive control).
	if !strings.Contains(output, "migrateLessonsToView") {
		t.Errorf("expected migrateLessonsToView log lines on first boot, got none:\n%s", output)
	}
}