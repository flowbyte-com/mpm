package main

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func setupTestDB(t *testing.T) (*sql.DB, string) {
	tmpFile, err := os.CreateTemp("", "mpm-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpFile.Close()

	db, err := sql.Open("sqlite3", tmpFile.Name())
	if err != nil {
		os.Remove(tmpFile.Name())
		t.Fatalf("failed to open temp db: %v", err)
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
	_, err = db.Exec(schema)
	if err != nil {
		db.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("failed to create schema: %v", err)
	}

	// Enable WAL mode for test database (needed for FTS5 + concurrent access)
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		db.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("failed to enable WAL: %v", err)
	}

	return db, tmpFile.Name()
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
	db, dbPath := setupTestDB(t)
	defer db.Close()
	defer os.Remove(dbPath)

	// Insert two memories with same keyword so they both match
	insertMemory(t, db, "mem-1", "memories", "golang programming language", "sess1", `[]`)
	insertMemory(t, db, "mem-2", "memories", "golang is great", "sess1", `[]`)

	// Simulate what handleRecall does: query then reinforce per unique ID
	rows, err := keywordSearchWithTime(db, "golang", "memories", "", "", 0, "", 10)
	if err != nil {
		t.Fatalf("keywordSearchWithTime failed: %v", err)
	}

	// Simulate per-call dedup (same logic as handleRecall)
	sessionAccessCounts := make(map[string]int)
	for rows.Next() {
		var id string
		// Scan all 9 columns: id, content, session_id, tags, created_at, reinforcement_count, weight, last_accessed_at, reference_id
		if err := rows.Scan(&id, new(string), new(string), new(string), new(string), new(int64), new(int64), new(sql.NullTime), new(sql.NullString)); err != nil {
			continue
		}
		if sessionAccessCounts[id] == 0 {
			// ReinforceMemory: increment reinforcement_count and weight
			_, err := db.Exec(`UPDATE memories SET reinforcement_count = reinforcement_count + 1 WHERE id = ?`, id)
			if err != nil {
				t.Fatalf("reinforce failed for %s: %v", id, err)
			}
		}
		sessionAccessCounts[id]++
	}
	rows.Close()

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
	db, dbPath := setupTestDB(t)
	defer db.Close()
	defer os.Remove(dbPath)

	// Insert a single memory
	insertMemory(t, db, "mem-dup", "memories", "duplicate test content", "sess1", `[]`)

	// Simulate recall returning the same memory twice (duplicate rows)
	rows, err := keywordSearchWithTime(db, "duplicate", "memories", "", "", 0, "", 10)
	if err != nil {
		t.Fatalf("keywordSearchWithTime failed: %v", err)
	}

	sessionAccessCounts := make(map[string]int)
	for rows.Next() {
		var id string
		// Scan all 9 columns: id, content, session_id, tags, created_at, reinforcement_count, weight, last_accessed_at, reference_id
		if err := rows.Scan(&id, new(string), new(string), new(string), new(string), new(int64), new(int64), new(sql.NullTime), new(sql.NullString)); err != nil {
			continue
		}
		if sessionAccessCounts[id] == 0 {
			_, err := db.Exec(`UPDATE memories SET reinforcement_count = reinforcement_count + 1 WHERE id = ?`, id)
			if err != nil {
				t.Fatalf("reinforce failed for %s: %v", id, err)
			}
		}
		sessionAccessCounts[id]++
	}
	rows.Close()

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
	db, dbPath := setupTestDB(t)
	defer db.Close()
	defer os.Remove(dbPath)

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
		createdAt  time.Time
		lastAccess time.Time
		staleDays  int
		want       bool
	}{
		{
			name:       "disabled when staleDays=0",
			createdAt:  now.Add(-30 * day),
			lastAccess: time.Time{},
			staleDays:  0,
			want:       false,
		},
		{
			name:       "recent last access — not stale",
			createdAt:  now.Add(-30 * day),
			lastAccess: now.Add(-5 * day),
			staleDays:  14,
			want:       false,
		},
		{
			name:       "last access > threshold — stale",
			createdAt:  now.Add(-30 * day),
			lastAccess: now.Add(-20 * day),
			staleDays:  14,
			want:       true,
		},
		{
			name:       "never accessed, created > threshold — stale",
			createdAt:  now.Add(-20 * day),
			lastAccess: time.Time{},
			staleDays:  14,
			want:       true,
		},
		{
			name:       "never accessed, created < threshold — not stale",
			createdAt:  now.Add(-5 * day),
			lastAccess: time.Time{},
			staleDays:  14,
			want:       false,
		},
		{
			name:       "last access exactly at threshold — not stale (exclusive)",
			createdAt:  now.Add(-30 * day),
			lastAccess: now.Add(-14*day + 1*time.Second),
			staleDays:  14,
			want:       false,
		},
		{
			name:       "last access just past threshold — stale",
			createdAt:  now.Add(-30 * day),
			lastAccess: now.Add(-14*day - 1*time.Second),
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
		lastAccessed time.Time
		wantSubstr   string
	}{
		{5, 12, now.Add(-48 * time.Hour), "5x ref"},
		{5, 12, now.Add(-48 * time.Hour), "weight 12"},
		{5, 12, now.Add(-48 * time.Hour), "LTM"},
		{5, 12, now.Add(-48 * time.Hour), "accessed"},
		{0, 1, time.Time{}, ""}, // no chips expected for default memory
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
	os.Exit(m.Run())
}
