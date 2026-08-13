// internal/scheduler/ingest_test.go
//
// Tests for the OpenClaw memory-flush ingest handler. Each test seeds
// a real file at a temp path (via the newIngestHandlerWithPath test
// seam) and asserts the wake-row insertion (or refusal/quarantine)
// per the safety rail documented in ingest.go.
//
// Tests use core.NewTestDM for a hermetic in-memory DB, isolated from
// the production scheduled_wakes table. The temp-file pattern uses
// t.TempDir() so cleanup is automatic — no global FS state touched.

package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/flowbyte-com/mpm-core"
)

// newSilentLogger discards output so test runs don't spam the console
// with the expected Warn/Info lines from each safety rail.
func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// readFileWithPerm writes content to path with the given mode and
// returns the absolute path. Test helper; the production handler
// expects a real file with restrictive perms.
func writeFileWithPerm(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("writeFile(%s): %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod(%s, %s): %v", path, mode, err)
	}
}

func TestIngest_NoFileIsNoOp(t *testing.T) {
	dm := core.NewTestDM(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "ingest.md")
	h := newIngestHandlerWithPath(dm, newSilentLogger(), target)

	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick returned err: %v", err)
	}
	// Verify no wake was created by checking the canonical
	// CheckPendingWakes / ListScheduledWakes surface. With the
	// hermetic DB the canonical "1 row from our handler" would be
	// the only thing in the table; absence of rows = success.
	rows, err := listAllWakesForTest(t, dm)
	if err != nil {
		t.Fatalf("listAllWakes: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("no-file tick produced %d wake rows; want 0", len(rows))
	}
}

func TestIngest_NormalFileInsertsWake(t *testing.T) {
	dm := core.NewTestDM(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "ingest.md")
	content := []byte("# Lessons learned\n\n- Wake bridge fixed (2026-08-13)\n- Drill orchestrator merged\n")
	writeFileWithPerm(t, target, content, 0o600)

	h := newIngestHandlerWithPath(dm, newSilentLogger(), target)
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick returned err: %v", err)
	}

	// Wake row should be present with metadata.source=openclaw_ingest.
	rows, err := listAllWakesForTest(t, dm)
	if err != nil {
		t.Fatalf("listAllWakes: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 wake row, got %d: %+v", len(rows), rows)
	}
	got := rows[0]
	if got["reason"] != "ephemeral_compaction_ready" {
		t.Errorf("reason = %v, want ephemeral_compaction_ready", got["reason"])
	}
	if got["fired"].(int64) != 0 {
		t.Errorf("fired = %v, want 0 (this wake is what the agent will see on next wake_context)", got["fired"])
	}
	if got["created_by"] != "openclaw-mpm-memory-ingest" {
		t.Errorf("created_by = %v, want openclaw-mpm-memory-ingest", got["created_by"])
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(got["metadata"].(string)), &meta); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	if meta["source"] != "openclaw_ingest" {
		t.Errorf("metadata.source = %v, want openclaw_ingest", meta["source"])
	}
	if int(meta["byte_count"].(float64)) != len(content) {
		t.Errorf("metadata.byte_count = %v, want %d", meta["byte_count"], len(content))
	}
	if meta["content"].(string) != string(content) {
		t.Errorf("metadata.content drift: got %q want %q", meta["content"], string(content))
	}

	// The original file should be gone (atomically renamed + read + deleted).
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("ingest.md still present after success; expected delete (err=%v)", err)
	}
	if _, err := os.Lstat(target + openclawIngestProcessingSuffix); !os.IsNotExist(err) {
		t.Errorf(".processing file left behind; expected cleanup (err=%v)", err)
	}
}

func TestIngest_SymlinkRefused(t *testing.T) {
	dm := core.NewTestDM(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "ingest.md")

	// Create a real file, then symlink it to the target. The handler
	// should refuse to follow the symlink and not ingest.
	real := filepath.Join(dir, "real.md")
	writeFileWithPerm(t, real, []byte("secret content"), 0o600)
	if err := os.Symlink(real, target); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	h := newIngestHandlerWithPath(dm, newSilentLogger(), target)
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick returned err: %v", err)
	}

	// No wake row created.
	rows, err := listAllWakesForTest(t, dm)
	if err != nil {
		t.Fatalf("listAllWakes: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("symlink target produced %d wake rows; want 0 (symlinks must be refused)", len(rows))
	}
	// Real source file must remain untouched.
	got, rerr := os.ReadFile(real)
	if rerr != nil {
		t.Fatalf("readFile(real): %v", rerr)
	}
	if string(got) != "secret content" {
		t.Errorf("real source file content changed; expected untouched")
	}
}

func TestIngest_OversizeFileQuarantined(t *testing.T) {
	dm := core.NewTestDM(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "ingest.md")
	// 65KB > 64KB cap.
	content := bytes.Repeat([]byte("x"), openclawIngestMaxBytes+1024)
	writeFileWithPerm(t, target, content, 0o600)

	h := newIngestHandlerWithPath(dm, newSilentLogger(), target)
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick returned err: %v", err)
	}

	// No wake row (oversize is rejected).
	rows, err := listAllWakesForTest(t, dm)
	if err != nil {
		t.Fatalf("listAllWakes: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("oversize produced %d wake rows; want 0", len(rows))
	}

	// Original is gone (renamed to quarantine). Quarantine copy exists
	// with the same content for v inspection.
	qPath := target + openclawIngestRejectedSuffix
	qContent, rerr := os.ReadFile(qPath)
	if rerr != nil {
		t.Fatalf("readFile(quarantine): %v (oversize file should be quarantined, not deleted)", rerr)
	}
	if int64(len(qContent)) != int64(len(content)) {
		t.Errorf("quarantine size = %d, want %d", len(qContent), len(content))
	}
	if _, lerr := os.Lstat(target); !os.IsNotExist(lerr) {
		t.Errorf("oversize should be renamed away from target; still exists (err=%v)", lerr)
	}
}

func TestIngest_WorldReadableFileRefused(t *testing.T) {
	dm := core.NewTestDM(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "ingest.md")
	writeFileWithPerm(t, target, []byte("world-readable content"), 0o644) // 0644 = world-readable

	h := newIngestHandlerWithPath(dm, newSilentLogger(), target)
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick returned err: %v", err)
	}

	rows, err := listAllWakesForTest(t, dm)
	if err != nil {
		t.Fatalf("listAllWakes: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("world-readable file produced %d wake rows; want 0 (perms re-check is a safety rail)", len(rows))
	}
}

func TestIngest_IgnoresDirectoryTarget(t *testing.T) {
	dm := core.NewTestDM(t)
	dir := t.TempDir()
	// Treat the dir itself as the target — it's not a regular file.
	h := newIngestHandlerWithPath(dm, newSilentLogger(), dir)
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick returned err: %v", err)
	}
	rows, _ := listAllWakesForTest(t, dm)
	if len(rows) != 0 {
		t.Errorf("directory target produced %d wake rows; want 0", len(rows))
	}
}

// listAllWakesForTest returns every row in scheduled_wakes. Used by
// the ingest tests to verify exact wake-row insertion behavior without
// re-deriving the canonical ListScheduledWakes semantics. Lives next to
// the tests (not exported) so test changes don't surface in prod API.
func listAllWakesForTest(t *testing.T, dm *core.DatabaseManager) ([]map[string]interface{}, error) {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata
		   FROM scheduled_wakes
		   ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var id, reason, createdBy, metadata string
		var theoryID, recurringRule *string
		var targetTime int64
		var fired int
		if err := rows.Scan(&id, &targetTime, &reason, &theoryID, &recurringRule, &fired, &createdBy, &metadata); err != nil {
			return nil, err
		}
		row := map[string]interface{}{
			"id":        id,
			"target_time": targetTime,
			"reason":    reason,
			"fired":     int64(fired),
			"created_by": createdBy,
			"metadata":  metadata,
		}
		if theoryID != nil {
			row["theory_id"] = *theoryID
		}
		if recurringRule != nil {
			row["recurring_rule"] = *recurringRule
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Test helper: ensure ingest_prefix string check is correct
// (otherwise a one-character typo in the const would silently break
// production cleanup paths).
func TestIngest_SuffixesAreReasonable(t *testing.T) {
	if !strings.HasPrefix(openclawIngestProcessingSuffix, ".") {
		t.Errorf("processing suffix should start with '.', got %q", openclawIngestProcessingSuffix)
	}
	if !strings.HasPrefix(openclawIngestRejectedSuffix, ".") {
		t.Errorf("rejected suffix should start with '.', got %q", openclawIngestRejectedSuffix)
	}
	if openclawIngestProcessingSuffix == openclawIngestRejectedSuffix {
		t.Errorf("processing and rejected suffixes must differ")
	}
}
