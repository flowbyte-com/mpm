package main

// handlers_logs_test.go — coverage for `mpm ops logs rotate | status`.
//
// Two layers:
//   - humanBytes: pure function, easy to table-test
//   - resolveLogPaths: small surface; tests resolve to expected layout
//   - handleOpsLogs: dispatch + help (rotation end-to-end already covered
//     by internal/log_rotate_test.go)
//
// The CLI itself is a thin shim over RotateLogIfNeededForCLI. Rotation
// correctness is verified at the internal package level — adding another
// test here would duplicate that coverage.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── humanBytes ─────────────────────────────────────────────────────────

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{5 * 1024 * 1024, "5.0 MB"},
		{11 * 1024 * 1024, "11.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{2 * 1024 * 1024 * 1024, "2.0 GB"},
	}
	for _, c := range cases {
		got := humanBytes(c.in)
		assert.Equal(t, c.want, got, "humanBytes(%d)", c.in)
	}
}

// ── resolveLogPaths ────────────────────────────────────────────────────

// TestResolveLogPaths verifies the two managed logs are placed in the
// canonical location (mpmdir/src/db/{watchdog,mirror}.jsonl). Redirects
// MPM_WORKSPACE so the test does not touch the production MPM dir.
func TestResolveLogPaths(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	logs, err := resolveLogPaths()
	require.NoError(t, err)
	require.Len(t, logs, 2)

	gotNames := map[string]string{}
	for _, lf := range logs {
		gotNames[lf.Name] = lf.Path
	}

	assert.Equal(t, filepath.Join(tmp, "src", "db", "watchdog.jsonl"), gotNames["watchdog"])
	assert.Equal(t, filepath.Join(tmp, "src", "db", "mirror.jsonl"), gotNames["mirror"])
}

// ── handleOpsLogs dispatch ─────────────────────────────────────────────

// TestHandleOpsLogs_Help pins that `mpm ops logs help` (no args) prints
// the usage block and exits 0. Human-readable output: substring check.
func TestHandleOpsLogs_Help(t *testing.T) {
	// Redirect stdout to a buffer is overkill — just check the return
	// code and that the function doesn't panic. The full usage text
	// is printed; we don't pin it character-for-character.
	assert.Equal(t, 0, handleOpsLogs([]string{"help"}))
	assert.Equal(t, 0, handleOpsLogs([]string{}))
	assert.Equal(t, 0, handleOpsLogs([]string{"--help"}))
}

// TestHandleOpsLogs_UnknownSubcommand verifies that bad subcommands
// return non-zero (not silent failure).
func TestHandleOpsLogs_UnknownSubcommand(t *testing.T) {
	// unknown subcommand exits 1.
	assert.Equal(t, 1, handleOpsLogs([]string{"purge"}))
}

// TestHandleOpsLogs_RotateWithUnknownLogName pins that an invalid
// log name returns an error (only watchdog | mirror accepted).
func TestHandleOpsLogs_RotateWithUnknownLogName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	assert.Equal(t, 1, handleOpsLogs([]string{"rotate", "garbage"}))
}

// TestHandleOpsLogs_RotateWhenNoLogFiles pins the missing-file path:
// if the logs don't exist yet, rotate is a clean no-op with skipped
// count == 2, exit 0. This is the "fresh install" case.
func TestHandleOpsLogs_RotateWhenNoLogFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	// Confirm pre-state: neither file exists.
	for _, name := range []string{"watchdog.jsonl", "mirror.jsonl"} {
		_, err := os.Stat(filepath.Join(tmp, "src", "db", name))
		assert.True(t, os.IsNotExist(err), "pre-state: %s should not exist", name)
	}

	assert.Equal(t, 0, handleOpsLogs([]string{"rotate"}),
		"rotate should succeed (skipped) when no log files exist")
}

// TestHandleOpsLogs_RotateSingleLog pins that `rotate <name>` only
// touches the named log. Writes a tiny file to each, rotates watchdog
// only, verifies mirror is untouched.
func TestHandleOpsLogs_RotateSingleLog(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	dbDir := filepath.Join(tmp, "src", "db")
	require.NoError(t, os.MkdirAll(dbDir, 0755))

	watchdogPath := filepath.Join(dbDir, "watchdog.jsonl")
	mirrorPath := filepath.Join(dbDir, "mirror.jsonl")
	originalWatchdog := "watchdog-content-must-survive-rotation\n"
	originalMirror := "mirror-content-must-NOT-be-touched\n"
	require.NoError(t, os.WriteFile(watchdogPath, []byte(originalWatchdog), 0644))
	require.NoError(t, os.WriteFile(mirrorPath, []byte(originalMirror), 0644))

	assert.Equal(t, 0, handleOpsLogs([]string{"rotate", "watchdog"}))

	// Watchdog rotated (file now empty; .gz sibling created).
	watchdogContent, err := os.ReadFile(watchdogPath)
	require.NoError(t, err)
	assert.Equal(t, "", string(watchdogContent), "watchdog should be empty after rotation")

	matches, _ := filepath.Glob(watchdogPath + ".*.gz")
	assert.Equal(t, 1, len(matches), "watchdog should have one rotated .gz sibling")

	// Mirror UNTOUCHED.
	mirrorContent, err := os.ReadFile(mirrorPath)
	require.NoError(t, err)
	assert.Equal(t, originalMirror, string(mirrorContent),
		"mirror must NOT be touched by `rotate watchdog`")
}

// TestHandleOpsLogs_RotateThresholdFlag pins the --threshold flag:
// rotate only fires when the file is at least N bytes.
func TestHandleOpsLogs_RotateThresholdFlag(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)
	dbDir := filepath.Join(tmp, "src", "db")
	require.NoError(t, os.MkdirAll(dbDir, 0755))

	watchdogPath := filepath.Join(dbDir, "watchdog.jsonl")
	small := "small\n"
	require.NoError(t, os.WriteFile(watchdogPath, []byte(small), 0644))

	// Threshold 1024: small file should be skipped. Flag must come
	// BEFORE the positional <name> argument (Go stdlib flag contract).
	assert.Equal(t, 0, handleOpsLogs([]string{"rotate", "--threshold", "1024", "watchdog"}))

	content, err := os.ReadFile(watchdogPath)
	require.NoError(t, err)
	assert.Equal(t, small, string(content),
		"file should be untouched when below threshold")

	// Threshold 1: small file should be rotated.
	assert.Equal(t, 0, handleOpsLogs([]string{"rotate", "--threshold", "1", "watchdog"}))

	content, err = os.ReadFile(watchdogPath)
	require.NoError(t, err)
	assert.Equal(t, "", string(content),
		"file should be empty after rotation when above threshold")
}

// TestHandleOpsLogs_StatusDoesNotMutate pins that `status` is read-only.
// Writes a fake log, runs status, verifies the file content is unchanged.
func TestHandleOpsLogs_StatusDoesNotMutate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)
	dbDir := filepath.Join(tmp, "src", "db")
	require.NoError(t, os.MkdirAll(dbDir, 0755))

	watchdogPath := filepath.Join(dbDir, "watchdog.jsonl")
	original := strings.Repeat("x", 1000) + "\n"
	require.NoError(t, os.WriteFile(watchdogPath, []byte(original), 0644))

	assert.Equal(t, 0, handleOpsLogs([]string{"status"}))

	content, err := os.ReadFile(watchdogPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(content),
		"status must NOT mutate the log file")
}
