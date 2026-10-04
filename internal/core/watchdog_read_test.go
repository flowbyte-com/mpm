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
	// The reader normalises the legacy "synthesize_error" spelling onto
	// the canonical "synthesize_failed" before returning. The on-disk
	// v1 line stays as it was — the alias is a reader-side projection
	// that lets one prefix filter work across both generations of the
	// log. See watchdogOpAliases / normalizeWatchdogOp.
	if got[0]["op"] != "synthesize_failed" {
		t.Errorf("entry 0 op = %v, want synthesize_failed (v1 alias resolved)", got[0]["op"])
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

// TestRecentWatchdogOps_AliasesLegacyOperations pins the read-side aliases
// the v2 redesign requires so existing diagnostics continue to match.
//
// v1 wrote "queryrow" (lowercase, no underscore). The pre-v2 alias
// normalisation projects it onto the canonical "query_row". A reader
// filter on "query_" must therefore pick up legacy lines too. The
// on-disk line is never rewritten.
func TestRecentWatchdogOps_AliasesLegacyOperations(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "watchdog.jsonl")
	lines := `{"op":"queryrow","timestamp":"2026-06-26T10:00:00Z"}
`
	if err := os.WriteFile(logPath, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	dm := &DatabaseManager{watchdogPath: logPath}
	got, err := dm.RecentWatchdogOps(0, "query_")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("legacy queryrow did not match 'query_' prefix: got %d entries", len(got))
	}
	if got[0]["op"] != "query_row" {
		t.Errorf("legacy queryrow projected to %v, want query_row", got[0]["op"])
	}
}

// TestRecentWatchdogOps_AcceptsV1AndV2Mixed pins that a stream containing
// both generations is read end-to-end without error: missing `v` (v1),
// the `operation`/`query` keys, and the `ts` field are all reconciled
// onto the v2 reader surface.
func TestRecentWatchdogOps_AcceptsV1AndV2Mixed(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "watchdog.jsonl")
	lines := `{"operation":"exec","query":"SELECT 1","timestamp":"2026-06-26T10:00:00Z"}
{"v":2,"ts":"2026-06-26T10:00:01Z","level":"warn","op":"exec","sql_shape":"SELECT ?"}
not a json line at all
`
	if err := os.WriteFile(logPath, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	dm := &DatabaseManager{watchdogPath: logPath}
	got, err := dm.RecentWatchdogOps(0, "")
	if err != nil {
		t.Fatal(err)
	}
	// 2 well-formed lines; the corrupt line is skipped, not fatal.
	if len(got) != 2 {
		t.Fatalf("expected 2 readable entries, got %d", len(got))
	}
	if got[0]["op"] != "exec" {
		t.Errorf("v1 operation field not projected to op: %v", got[0]["op"])
	}
	if got[0]["ts"] == "" {
		t.Errorf("v1 timestamp field not copied to ts: %v", got[0])
	}
	if got[1]["op"] != "exec" {
		t.Errorf("v2 op not preserved: %v", got[1]["op"])
	}
}