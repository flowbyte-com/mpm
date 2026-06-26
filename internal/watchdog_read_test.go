package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecentWatchdogOps_FiltersByPrefix verifies that RecentWatchdogOps only
// returns entries whose "op" field starts with the given prefix.
func TestRecentWatchdogOps_FiltersByPrefix(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "watchdog.jsonl")

	// Write three lines: two synthesize, one unrelated.
	lines := []string{
		`{"op":"synthesize_error","timestamp":"2026-06-26T10:00:00Z","error":"boom"}`,
		`{"op":"query_slow","timestamp":"2026-06-26T10:00:01Z","query":"SELECT 1"}`,
		`{"op":"synthesize_skip","timestamp":"2026-06-26T10:00:02Z","reason":"all candidates already seen this session"}`,
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dm := &DatabaseManager{watchdogPath: logPath}

	got, err := dm.RecentWatchdogOps(0, "synthesize_")
	if err != nil {
		t.Fatalf("RecentWatchdogOps: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 synthesize entries, got %d", len(got))
	}
	if got[0]["op"] != "synthesize_error" {
		t.Errorf("entry 0 op = %v, want synthesize_error", got[0]["op"])
	}
	if got[1]["op"] != "synthesize_skip" {
		t.Errorf("entry 1 op = %v, want synthesize_skip", got[1]["op"])
	}
}

// TestRecentWatchdogOps_RespectsLimit verifies that n caps the result.
func TestRecentWatchdogOps_RespectsLimit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "watchdog.jsonl")

	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString(`{"op":"synthesize_skip","timestamp":"2026-06-26T10:00:00Z"}` + "\n")
	}
	if err := os.WriteFile(logPath, []byte(sb.String()), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dm := &DatabaseManager{watchdogPath: logPath}
	got, err := dm.RecentWatchdogOps(10, "synthesize_")
	if err != nil {
		t.Fatalf("RecentWatchdogOps: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("expected 10 entries (limit), got %d", len(got))
	}
}

// TestRecentWatchdogOps_MissingFileReturnsNil verifies that a missing log
// file is not an error — the watchdog may not have been written yet.
func TestRecentWatchdogOps_MissingFileReturnsNil(t *testing.T) {
	t.Parallel()
	dm := &DatabaseManager{watchdogPath: "/nonexistent/watchdog.jsonl"}
	got, err := dm.RecentWatchdogOps(10, "synthesize_")
	if err != nil {
		t.Fatalf("RecentWatchdogOps on missing file: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice for missing file, got %v", got)
	}
}

// TestRecentWatchdogOps_EmptyPrefixReturnsAll verifies that an empty prefix
// returns every entry (the prefix filter is optional).
func TestRecentWatchdogOps_EmptyPrefixReturnsAll(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "watchdog.jsonl")
	lines := `{"op":"a","timestamp":"2026-06-26T10:00:00Z"}
{"op":"b","timestamp":"2026-06-26T10:00:01Z"}
{"op":"c","timestamp":"2026-06-26T10:00:02Z"}
`
	if err := os.WriteFile(logPath, []byte(lines), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dm := &DatabaseManager{watchdogPath: logPath}
	got, err := dm.RecentWatchdogOps(0, "")
	if err != nil {
		t.Fatalf("RecentWatchdogOps: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries with empty prefix, got %d", len(got))
	}
}