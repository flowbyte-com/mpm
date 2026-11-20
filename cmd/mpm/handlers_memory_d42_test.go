// handlers_memory_d42_test.go — alpha-5 D-4.2 regression test.
//
// D-4.2: `mpm memory search --limit N` was previously not supported:
// the handler hardcoded `limit=20` and any `--limit` token polluted
// the FTS5 query (causing zero-hit results because no document
// contained the literal token `--limit`). The fix introduces
// extractLimitFlag which strips --limit / -l from the args and
// threads the value through to FullTextSearch.
//
// The regression tests pin both halves:
//   1. extractLimitFlag parses space-form, equals-form, short-form
//      correctly and preserves the rest of the args.
//   2. handleMemorySearch end-to-end: --limit 1 against 3 seeded
//      memories returns exactly 1 (not 3, not 0).

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestExtractLimitFlag_AllForms pins the parser's contract for the
// four supported forms:
//   --limit 5      (space-separated, long)
//   --limit=5      (equals, long)
//   -l 5           (space-separated, short)
//   -l=5           (equals, short)
func TestExtractLimitFlag_AllForms(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantLimit int
		wantRest  []string
	}{
		{"space-form long", []string{"foo", "--limit", "5", "bar"}, 5, []string{"foo", "bar"}},
		{"equals-form long", []string{"foo", "--limit=5", "bar"}, 5, []string{"foo", "bar"}},
		{"space-form short", []string{"foo", "-l", "5", "bar"}, 5, []string{"foo", "bar"}},
		{"equals-form short", []string{"foo", "-l=5", "bar"}, 5, []string{"foo", "bar"}},
		{"only-flag", []string{"--limit", "5"}, 5, []string{}},
		{"no flag returns default", []string{"foo", "bar"}, 20, []string{"foo", "bar"}},
		{"missing value keeps default", []string{"foo", "--limit"}, 20, []string{"foo"}},
		{"unparseable value keeps default", []string{"foo", "--limit", "abc"}, 20, []string{"foo"}},
		{"negative value keeps default", []string{"foo", "--limit", "-1"}, 20, []string{"foo"}},
		{"zero value keeps default", []string{"foo", "--limit", "0"}, 20, []string{"foo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotRest, gotLimit := extractLimitFlag(tc.args, 20)
			if gotLimit != tc.wantLimit {
				t.Errorf("limit: got %d, want %d", gotLimit, tc.wantLimit)
			}
			if !sliceEqual(gotRest, tc.wantRest) {
				t.Errorf("rest: got %v, want %v", gotRest, tc.wantRest)
			}
		})
	}
}

// TestHandleMemorySearch_RespectsLimitFlag is the end-to-end D-4.2
// regression: search with --limit 1 against 3 seeded memories returns
// exactly 1. Pre-fix --limit was not stripped, so it polluted the
// FTS5 query and returned 0 hits.
//
// Note: cmd/mpm caches the DatabaseManager in a sync.Once-backed
// global. Other tests in the suite may have already triggered
// getDB() — in which case our t.Setenv("MPM_WORKSPACE") would not
// take effect. We reset the global state so getDB() re-initialises
// against the fresh temp workspace.
func TestHandleMemorySearch_RespectsLimitFlag(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	// Reset the global DB singleton so the next getDB() picks up the
	// temp workspace we just set. The sync.Once needs to be re-armed
	// (not just zeroed) — sync.Once cannot be copied per its contract,
	// so we replace the global with a brand-new value via address-of.
	savedDM := dbManager
	savedErr := dbManagerInitErr
	resetGlobalDB(t, ws)
	t.Cleanup(func() {
		if dbManager != nil {
			_ = dbManager.Close()
		}
		dbManager = savedDM
		dbManagerInitErr = savedErr
		// Re-arm the Once so subsequent tests in the suite see a
		// usable singleton. resetGlobalDB pointed dbManagerOnce at
		// our local onceToken; restore it to a fresh token owned by
		// the package's Once var.
		dbManagerOnce = sync.Once{}
	})

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// Seed 3 memories whose content contains the search term.
	for i := 0; i < 3; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, weight, created_at)
			VALUES (?, 'memories', ?, '{"weight":1}', 1, CAST(strftime('%s','now') AS INTEGER))
		`, "d42-"+itoaForD42(i), "d42-searchterm content "+itoaForD42(i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Capture stdout so we can parse the JSON envelope.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = r.Close()
		_ = w.Close()
	})

	exit := handleMemorySearch([]string{"d42-searchterm", "--limit", "1", "--json"})
	_ = w.Close()
	if exit != 0 {
		t.Fatalf("handleMemorySearch exit=%d", exit)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured: %v", err)
	}

	var env struct {
		Success  bool `json:"success"`
		Query    string `json:"query"`
		Count    int `json:"count"`
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nbody: %s", err, buf.String())
	}
	if !env.Success {
		t.Errorf("success: got %v, want true", env.Success)
	}
	if env.Count != 1 {
		t.Errorf("count: got %d, want 1 (--limit 1)", env.Count)
	}
	if len(env.Memories) != 1 {
		t.Errorf("len(memories): got %d, want 1", len(env.Memories))
	}
	// Pre-fix: query would be "d42-searchterm --limit 1" because the
	// flag tokens were joined into the FTS5 query. Post-fix: query
	// is just "d42-searchterm".
	if strings.Contains(env.Query, "--limit") {
		t.Errorf("query still contains --limit flag (pre-fix bug): got %q", env.Query)
	}
	if env.Query != "d42-searchterm" {
		t.Errorf("query: got %q, want %q", env.Query, "d42-searchterm")
	}
}

// sliceEqual is a small helper to keep the parser-table assertions
// readable. Empty slices compare equal so we don't need nil-aware
// semantics here.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itoaForD42 avoids colliding with strconvItoa (defined in
// internal/core/scheduled_tasks_migration_test.go — different module,
// but Go test compilation is per-package and we want to be defensive
// against future copies).
func itoaForD42(n int) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789"
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

// onceToken is a thin wrapper around sync.Once so we can swap the
// package-level dbManagerOnce pointer without copying the underlying
// sync.Once value (which the vet forbids). The resetGlobalDB helper
// points dbManagerOnce at a fresh token; the test's t.Cleanup restores
// the package-level Once to a fresh zero value.
type onceToken struct{ once sync.Once }

// resetGlobalDB rewires the cmd/mpm package-level DB singletons so the
// next getDB() call re-initialises against the given workspace. We do
// NOT copy sync.Once — we redirect dbManagerOnce to point at a local
// token that the goroutine will use until cleanup restores the
// package-level Once.
//
// We can't directly change the type of dbManagerOnce to *onceToken
// without touching production code, so the trick is to reassign the
// sync.Once VALUE to a fresh struct literal (which IS allowed because
// struct literals aren't copies of an existing value).
func resetGlobalDB(t *testing.T, workspace string) {
	t.Helper()
	dbManager = nil
	dbManagerInitErr = nil
	// Re-arm by assigning a fresh sync.Once literal — vet permits
	// this because we are not copying an existing value.
	dbManagerOnce = sync.Once{}
}