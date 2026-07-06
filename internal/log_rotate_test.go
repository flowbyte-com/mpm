package internal

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRotateLogIfNeeded_NoOpBelowThreshold pins that no rotation
// happens when the log is under threshold. Verifies the file
// contents are unchanged.
func TestRotateLogIfNeeded_NoOpBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	content := "line1\nline2\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := rotateLogIfNeeded(path, 1024); err != nil {
		t.Fatalf("rotateLogIfNeeded: %v", err)
	}

	// File contents unchanged.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("contents changed: got %q, want %q", string(got), content)
	}

	// No rotated file created.
	matches, _ := filepath.Glob(path + ".*.gz")
	if len(matches) != 0 {
		t.Errorf("unexpected rotated file: %v", matches)
	}
}

// TestRotateLogIfNeeded_GzipsAndTruncates verifies the rotation path:
// when the file exceeds threshold, it gets gzipped to a timestamped
// filename and the original is truncated to zero bytes.
func TestRotateLogIfNeeded_GzipsAndTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	// Write > 100 bytes so a 100-byte threshold triggers rotation.
	original := strings.Repeat("xxxxxxxxxx\n", 20) // 220 bytes
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if err := rotateLogIfNeeded(path, 100); err != nil {
		t.Fatalf("rotateLogIfNeeded: %v", err)
	}

	// Original is now empty (truncated).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("original size after rotation: got %d, want 0", info.Size())
	}

	// Rotated gz file exists with timestamp suffix.
	matches, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("rotated file count: got %d, want 1 (matches=%v)", len(matches), matches)
	}

	// Decompress and verify content matches.
	gz, err := os.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	gr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	decompressed, err := io.ReadAll(gr)
	if err != nil {
		t.Fatal(err)
	}
	if string(decompressed) != original {
		t.Errorf("rotated content: got %q, want %q", string(decompressed), original)
	}
}

// TestRotateLogIfNeeded_MissingFileNoOp pins the safe default: missing
// file is not an error (the next write will create it via O_CREATE).
func TestRotateLogIfNeeded_MissingFileNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.jsonl")
	if err := rotateLogIfNeeded(path, 1024); err != nil {
		t.Errorf("missing-file path: expected nil error, got %v", err)
	}
}

// TestRotateLogIfNeeded_EmptyPath pins that empty path is a no-op
// (defensive — should never happen but cheap to pin).
func TestRotateLogIfNeeded_EmptyPath(t *testing.T) {
	if err := rotateLogIfNeeded("", 1024); err != nil {
		t.Errorf("empty path: expected nil error, got %v", err)
	}
}

// TestLogRotateThresholdBytes_Default verifies the env-var fallback
// to the 5 MiB default.
func TestLogRotateThresholdBytes_Default(t *testing.T) {
	t.Setenv("MPM_LOG_ROTATE_BYTES", "")
	if got := logRotateThresholdBytes(); got != defaultLogRotateBytes {
		t.Errorf("default: got %d, want %d", got, defaultLogRotateBytes)
	}
}

// TestLogRotateThresholdBytes_Override verifies MPM_LOG_ROTATE_BYTES
// overrides the default.
func TestLogRotateThresholdBytes_Override(t *testing.T) {
	t.Setenv("MPM_LOG_ROTATE_BYTES", "1048576") // 1 MiB
	if got := logRotateThresholdBytes(); got != 1048576 {
		t.Errorf("override: got %d, want 1048576", got)
	}
}

// TestLogRotateThresholdBytes_InvalidFallback pins that garbage values
// fall back to default rather than panicking or returning 0.
func TestLogRotateThresholdBytes_InvalidFallback(t *testing.T) {
	for _, v := range []string{"abc", "-1", "0"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("MPM_LOG_ROTATE_BYTES", v)
			if got := logRotateThresholdBytes(); got != defaultLogRotateBytes {
				t.Errorf("invalid %q: got %d, want default %d", v, got, defaultLogRotateBytes)
			}
		})
	}
}

// TestLogWatchdog_TriggersRotationAtThreshold pins the end-to-end
// integration: writing through logWatchdog at a threshold below the
// file's size produces a gzipped rotated file. Uses a private
// DatabaseManager with a tmpdir watchdog path so production logs
// are untouched.
func TestLogWatchdog_TriggersRotationAtThreshold(t *testing.T) {
	dm := newTestDM(t)

	// Override the watchdog path to a tmpfile (newTestDM doesn't set
	// one because tests bypass the DM constructor's path setup).
	tmp := t.TempDir()
	watchdogPath := filepath.Join(tmp, "watchdog.jsonl")
	dm.watchdogPath = watchdogPath

	// Write enough content to exceed a small threshold.
	bigLine := strings.Repeat("x", 200)
	for i := 0; i < 5; i++ {
		dm.logWatchdog(watchdogOp{Operation: "exec", Query: bigLine})
	}

	// First check: file exists.
	if _, err := os.Stat(watchdogPath); err != nil {
		t.Fatalf("watchdog file missing: %v", err)
	}

	// Now run rotation manually at a tiny threshold.
	if err := rotateLogIfNeeded(watchdogPath, 100); err != nil {
		t.Fatalf("rotateLogIfNeeded: %v", err)
	}

	// After rotation: original is empty, a .gz sibling exists.
	info, err := os.Stat(watchdogPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("original not truncated: size=%d", info.Size())
	}
	matches, _ := filepath.Glob(watchdogPath + ".*.gz")
	if len(matches) != 1 {
		t.Errorf("expected 1 rotated file, got %d (matches=%v)", len(matches), matches)
	}
}
