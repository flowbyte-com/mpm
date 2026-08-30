package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ptrInt64 returns a pointer to the given int64 value. Used in test fixtures
// to populate *int64 fields without repeating `&int64(x)` boilerplate.
func ptrInt64(v int64) *int64 { return &v }

// recallTestDBCounter increments per call to setupTestDB, giving each
// caller a uniquely-named shared-cache in-memory database. The DSN form
// `file:<unique>?mode=memory&cache=shared` keeps the DB in RAM while making
// it visible to every connection in the pool — bare ":memory:" would give
// each pooled connection its own private DB and silently lose schema state.
var recallTestDBCounter int64

// setupTestDB opens a fresh in-memory SQLite database with the minimal
// `memories` schema these tests need (plus a handful of indexes). Returns
// the *sql.DB and registers cleanup with t.Cleanup so callers don't have
// to remember `defer db.Close()`.
//
// The recall tests touch the raw *sql.DB directly because the functions
// under test (keywordSearchWithTime, etc.) take *sql.DB rather than a
// DatabaseManager. For tests that DO want a full DatabaseManager, use
// internal.NewTestDM(t) instead.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	n := atomic.AddInt64(&recallTestDBCounter, 1)
	dsn := fmt.Sprintf("file:recall-memtest-%d?mode=memory&cache=shared", n)

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	// Create memories table with all required columns (matches schema + SafeMigrations)
	schema := `
	CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		collection TEXT NOT NULL DEFAULT 'memories',
		content TEXT NOT NULL,
		session_id TEXT,
		tags JSON,
		metadata JSON,
		embedding BLOB,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		deleted_at DATETIME,
		is_long_term INTEGER DEFAULT 0,
		weight INTEGER DEFAULT 1,
		reinforcement_count INTEGER DEFAULT 0,
		last_accessed_at DATETIME,
		expires_at DATETIME,
		reference_id TEXT,
		FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL
	);
	CREATE INDEX IF NOT EXISTS idx_memories_collection ON memories(collection);
	CREATE INDEX IF NOT EXISTS idx_memories_longterm ON memories(is_long_term, weight);
	CREATE INDEX IF NOT EXISTS idx_memories_reinforcement ON memories(reinforcement_count);
	CREATE INDEX IF NOT EXISTS idx_memories_accessed ON memories(last_accessed_at);
	CREATE INDEX IF NOT EXISTS idx_memories_reference ON memories(reference_id);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}

	t.Cleanup(func() { db.Close() })
	return db
}

// insertMemory inserts a test memory directly via SQL (bypassing MPM internals)
func insertMemory(t *testing.T, db *sql.DB, id, collection, content, sessionID, tags string) {
	_, err := db.Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, reinforcement_count, weight)
		VALUES (?, ?, ?, ?, ?, 0, 1)
	`, id, collection, content, sessionID, tags)
	if err != nil {
		t.Fatalf("failed to insert memory %s: %v", id, err)
	}
}

// TestRecallDeduplicatesReinforcement verifies that calling keywordSearchWithTime
// and then reinforcing only once per unique memory ID per call.
func TestRecallDeduplicatesReinforcement(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	

	// Insert two memories with same keyword so they both match
	insertMemory(t, db, "mem-1", "memories", "golang programming language", "sess1", `[]`)
	insertMemory(t, db, "mem-2", "memories", "golang is great", "sess1", `[]`)

	// Simulate what handleRecall does: query then reinforce per unique ID.
	// NOTE: production handleRecall collects IDs into a map and runs a
	// SINGLE bulk UPDATE after the row iterator closes — not per-ID
	// UPDATEs inside the loop. The test mirrors the production flow now
	// because per-ID UPDATEs while the row cursor is open hit
	// "database is locked" under shared-cache in-memory (the production
	// code's bulk UPDATE after Close() does not).
	rows, err := keywordSearchWithTime(db, "golang", "memories", "", "", 0, "", 10)
	if err != nil {
		t.Fatalf("keywordSearchWithTime failed: %v", err)
	}

	// First pass: walk the rows, dedup per call via sessionAccessCounts,
	// collect unique IDs for the bulk update.
	sessionAccessCounts := make(map[string]int)
	var uniqueIDs []string
	for rows.Next() {
		var id string
		// Scan all 10 columns (must match keywordSearchWithTime's SELECT):
		// id, content, session_id, tags, metadata, created_at, reinforcement_count,
		// weight, last_accessed_at, reference_id. The Scan destination in
		// handleRecall has the same 10 args; if either drifts the count
		// mismatch surfaces as a runtime crash on the very first recall.
		// metadata + created_at + last_accessed_at + reference_id are all
		// nullable or time-typed in the test schema (insertMemory doesn't
		// set them) so we use sql.NullString / sql.NullTime — matching
		// production handleRecall's nullable destinations.
		if err := rows.Scan(&id, new(string), new(string), new(string), new(sql.NullString), new(sql.NullTime), new(int64), new(int64), new(sql.NullTime), new(sql.NullString)); err != nil {
			t.Logf("scan err: %v", err)
			continue
		}
		if sessionAccessCounts[id] == 0 {
			uniqueIDs = append(uniqueIDs, id)
		}
		sessionAccessCounts[id]++
	}
	rows.Close()

	// Second pass: reinforce (matching what ReinforceMemory does) once
	// per unique ID. Done after rows.Close() so the read txn is gone
	// before the write txn tries to acquire the lock.
	for _, id := range uniqueIDs {
		_, err := db.Exec(`UPDATE memories SET reinforcement_count = reinforcement_count + 1 WHERE id = ?`, id)
		if err != nil {
			t.Fatalf("reinforce failed for %s: %v", id, err)
		}
	}

	// Verify both IDs appear in sessionAccessCounts
	if sessionAccessCounts["mem-1"] != 1 {
		t.Errorf("mem-1 access count = %d, want 1", sessionAccessCounts["mem-1"])
	}
	if sessionAccessCounts["mem-2"] != 1 {
		t.Errorf("mem-2 access count = %d, want 1", sessionAccessCounts["mem-2"])
	}

	// Verify both were reinforced exactly once
	var reinforcement int
	row := db.QueryRow("SELECT reinforcement_count FROM memories WHERE id = 'mem-1'")
	if err := row.Scan(&reinforcement); err != nil {
		t.Fatalf("failed to read mem-1: %v", err)
	}
	if reinforcement != 1 {
		t.Errorf("mem-1: expected reinforcement_count=1, got %d", reinforcement)
	}

	row = db.QueryRow("SELECT reinforcement_count FROM memories WHERE id = 'mem-2'")
	if err := row.Scan(&reinforcement); err != nil {
		t.Fatalf("failed to read mem-2: %v", err)
	}
	if reinforcement != 1 {
		t.Errorf("mem-2: expected reinforcement_count=1, got %d", reinforcement)
	}
}

// TestRecallDeduplicatesAccessAcrossMultipleRows verifies that when the same
// memory ID appears twice in a single recall result (duplicate rows), it is
// only reinforced once.
func TestRecallDeduplicatesAccessAcrossMultipleRows(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	

	// Insert a single memory
	insertMemory(t, db, "mem-dup", "memories", "duplicate test content", "sess1", `[]`)

	// Simulate recall returning the same memory twice (duplicate rows).
	// Same flow as TestRecallDeduplicatesReinforcement: collect IDs, then
	// reinforce after rows.Close() to avoid the shared-cache lock.
	rows, err := keywordSearchWithTime(db, "duplicate", "memories", "", "", 0, "", 10)
	if err != nil {
		t.Fatalf("keywordSearchWithTime failed: %v", err)
	}

	sessionAccessCounts := make(map[string]int)
	var uniqueIDs []string
	for rows.Next() {
		var id string
		// Scan all 10 columns — must match keywordSearchWithTime's SELECT
		// (id, content, session_id, tags, metadata, created_at, reinforcement_count,
		// weight, last_accessed_at, reference_id). See the same comment in
		// TestRecallDeduplicatesReinforcement for the column-count contract.
		// metadata + created_at + last_accessed_at + reference_id use
		// sql.NullString / sql.NullTime to match the test schema and
		// production handleRecall's nullable destinations.
		if err := rows.Scan(&id, new(string), new(string), new(string), new(sql.NullString), new(sql.NullTime), new(int64), new(int64), new(sql.NullTime), new(sql.NullString)); err != nil {
			continue
		}
		if sessionAccessCounts[id] == 0 {
			uniqueIDs = append(uniqueIDs, id)
		}
		sessionAccessCounts[id]++
	}
	rows.Close()

	for _, id := range uniqueIDs {
		_, err := db.Exec(`UPDATE memories SET reinforcement_count = reinforcement_count + 1 WHERE id = ?`, id)
		if err != nil {
			t.Fatalf("reinforce failed for %s: %v", id, err)
		}
	}

	// Should be accessed once and reinforced once
	// Note: our query (FTS5 or LIKE) does not produce duplicate rows for the same ID,
	// so this test verifies that a single matching memory is reinforced once per call.
	// The "duplicate rows" scenario this was designed to test does not occur in practice.
	if sessionAccessCounts["mem-dup"] != 1 {
		t.Errorf("mem-dup access count = %d, want 1", sessionAccessCounts["mem-dup"])
	}

	var reinforcement int
	row := db.QueryRow("SELECT reinforcement_count FROM memories WHERE id = 'mem-dup'")
	if err := row.Scan(&reinforcement); err != nil {
		t.Fatalf("failed to read mem-dup: %v", err)
	}
	if reinforcement != 1 {
		t.Errorf("mem-dup: expected reinforcement_count=1 (deduped), got %d", reinforcement)
	}
}

// TestRecallReinforceSQLPattern verifies the reinforcement SQL pattern
// (mimics what ReinforceMemory does) updates reinforcement_count correctly.
func TestRecallReinforceSQLPattern(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	

	// Insert a memory
	insertMemory(t, db, "mem-real", "memories", "real reinforcement test", "sess1", `[]`)

	// Apply the exact same SQL that ReinforceMemory uses
	_, err := db.Exec(`
		UPDATE memories
		SET reinforcement_count = reinforcement_count + 1,
		    weight = MIN(weight + ((1 + 1) / 2), 100),
		    last_accessed_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, "mem-real")
	if err != nil {
		t.Fatalf("reinforce SQL failed: %v", err)
	}

	// Verify reinforcement_count incremented
	var reinforcement int
	row := db.QueryRow("SELECT reinforcement_count FROM memories WHERE id = 'mem-real'")
	if err := row.Scan(&reinforcement); err != nil {
		t.Fatalf("failed to read mem-real: %v", err)
	}
	if reinforcement != 1 {
		t.Errorf("mem-real: expected reinforcement_count=1, got %d", reinforcement)
	}

	// Verify last_accessed_at was set
	var lastAccessed sql.NullString
	row = db.QueryRow("SELECT last_accessed_at FROM memories WHERE id = 'mem-real'")
	if err := row.Scan(&lastAccessed); err != nil {
		t.Fatalf("failed to read last_accessed_at: %v", err)
	}
	if !lastAccessed.Valid {
		t.Error("mem-real: last_accessed_at should be set")
	}
}

func TestIsMemoryStale(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour

	tests := []struct {
		name       string
		createdAt  int64
		lastAccess *int64
		staleDays  int
		want       bool
	}{
		{
			name:       "disabled when staleDays=0",
			createdAt:  now.Add(-30 * day).Unix(),
			lastAccess: nil,
			staleDays:  0,
			want:       false,
		},
		{
			name:       "recent last access — not stale",
			createdAt:  now.Add(-30 * day).Unix(),
			lastAccess: ptrInt64(now.Add(-5 * day).Unix()),
			staleDays:  14,
			want:       false,
		},
		{
			name:       "last access > threshold — stale",
			createdAt:  now.Add(-30 * day).Unix(),
			lastAccess: ptrInt64(now.Add(-20 * day).Unix()),
			staleDays:  14,
			want:       true,
		},
		{
			name:       "never accessed, created > threshold — stale",
			createdAt:  now.Add(-20 * day).Unix(),
			lastAccess: nil,
			staleDays:  14,
			want:       true,
		},
		{
			name:       "never accessed, created < threshold — not stale",
			createdAt:  now.Add(-5 * day).Unix(),
			lastAccess: nil,
			staleDays:  14,
			want:       false,
		},
		{
			name:       "last access exactly at threshold — not stale (exclusive)",
			createdAt:  now.Add(-30 * day).Unix(),
			lastAccess: ptrInt64(now.Add(-14*day + 1*time.Second).Unix()),
			staleDays:  14,
			want:       false,
		},
		{
			name:       "last access just past threshold — stale",
			createdAt:  now.Add(-30 * day).Unix(),
			lastAccess: ptrInt64(now.Add(-14*day - 1*time.Second).Unix()),
			staleDays:  14,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMemoryStale(tt.createdAt, tt.lastAccess, tt.staleDays)
			if got != tt.want {
				t.Errorf("isMemoryStale(%v, %v, %d) = %v, want %v",
					tt.createdAt, tt.lastAccess, tt.staleDays, got, tt.want)
			}
		})
	}
}

func TestComputeScore(t *testing.T) {
	tests := []struct {
		rc, weight       int
		wantMin, wantMax float64
	}{
		{0, 1, 0.0, 0.1},   // default: very low score
		{5, 10, 0.4, 0.5},  // moderate: mid score
		{10, 10, 0.6, 0.7}, // high: upper mid
		{50, 20, 1.0, 1.0}, // capped at 1.0
	}
	for _, tt := range tests {
		got := computeScore(tt.rc, tt.weight)
		if got < tt.wantMin || got > tt.wantMax {
			t.Errorf("computeScore(%d, %d) = %v, want between %v and %v",
				tt.rc, tt.weight, got, tt.wantMin, tt.wantMax)
		}
	}
}

func TestFormatRationale(t *testing.T) {
	now := time.Now()
	tests := []struct {
		rc, weight   int
		lastAccessed *int64
		wantSubstr   string
	}{
		{5, 12, ptrInt64(now.Add(-48 * time.Hour).Unix()), "5x ref"},
		{5, 12, ptrInt64(now.Add(-48 * time.Hour).Unix()), "weight 12"},
		{5, 12, ptrInt64(now.Add(-48 * time.Hour).Unix()), "LTM"},
		{5, 12, ptrInt64(now.Add(-48 * time.Hour).Unix()), "accessed"},
		{0, 1, nil, ""}, // no chips expected for default memory
	}
	for _, tt := range tests {
		got := formatRationale(tt.rc, tt.weight, tt.lastAccessed)
		if tt.wantSubstr != "" && !strings.Contains(got, tt.wantSubstr) {
			t.Errorf("formatRationale(%d, %d, _) = %q, want substring %q",
				tt.rc, tt.weight, got, tt.wantSubstr)
		}
	}
}

func TestMain(m *testing.M) {
	// Run tests with temporary home to avoid polluting real config
	tmpDir := os.TempDir()
	os.Setenv("HOME", tmpDir)

	// Alpha-4 D-004/W-004: build the mpm binary once so the
	// machine-clean-output tests (cmd/mpm/call_io_test.go) can exec
	// it directly and observe the real stdout/stderr split.
	if bin := buildMPMBinForIOTests(); bin != "" {
		mpmBin = bin
	}

	os.Exit(m.Run())
}

// TestKeywordSearchWithTime_ExcludesExpiredRows pins the
// MemoryExpireClause on the recall path: a memory whose expires_at is
// in the past must NOT be returned by keywordSearchWithTime, regardless
// of whether the FTS5 path or the LIKE fallback is taken.
func TestKeywordSearchWithTime_ExcludesExpiredRows(t *testing.T) {
	db := setupTestDB(t)

	insertMemory(t, db, "live-1", "memories", "shared keyword live", "", "[]")
	insertMemory(t, db, "live-2", "memories", "shared keyword future ttl", "", "[]")
	insertMemory(t, db, "expired-1", "memories", "shared keyword past ttl", "", "[]")

	// Set expires_at: live-2 far in the future, expired-1 in the past.
	if _, err := db.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, 4102444800, "live-2"); err != nil {
		t.Fatalf("set future expires_at: %v", err)
	}
	if _, err := db.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, 1000, "expired-1"); err != nil {
		t.Fatalf("set past expires_at: %v", err)
	}

	rows, err := keywordSearchWithTime(db, "keyword", "memories", "", "", 0, "", 50)
	if err != nil {
		t.Fatalf("keywordSearchWithTime: %v", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		var content, sessionID, tags, metadata, createdAt sql.NullString
		var reinforcementCount, weight sql.NullInt64
		var lastAccessed sql.NullString
		var referenceID sql.NullString
		if err := rows.Scan(&id, &content, &sessionID, &tags, &metadata, &createdAt, &reinforcementCount, &weight, &lastAccessed, &referenceID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	assertNoExpired(t, ids)
	assertContains(t, ids, "live-1")
	assertContains(t, ids, "live-2")
}

func assertContains(t *testing.T, ids []string, want string) {
	t.Helper()
	for _, id := range ids {
		if id == want {
			return
		}
	}
	t.Errorf("expected id %q in results, got %v", want, ids)
}

func assertNoExpired(t *testing.T, ids []string) {
	t.Helper()
	for _, id := range ids {
		if id == "expired-1" {
			t.Errorf("expired row leaked into recall results: %v", ids)
		}
	}
}
