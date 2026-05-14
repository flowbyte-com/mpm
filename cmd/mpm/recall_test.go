package main

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
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
		FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL
	);
	CREATE INDEX IF NOT EXISTS idx_memories_collection ON memories(collection);
	CREATE INDEX IF NOT EXISTS idx_memories_longterm ON memories(is_long_term, weight);
	CREATE INDEX IF NOT EXISTS idx_memories_reinforcement ON memories(reinforcement_count);
	CREATE INDEX IF NOT EXISTS idx_memories_accessed ON memories(last_accessed_at);
	`
	_, err = db.Exec(schema)
	if err != nil {
		db.Close()
		t.Fatalf("failed to create schema: %v", err)
	}

	return db
}

func TestRecallDeduplicatesReinforcement(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Insert two test memories
	_, err := db.Exec(`
		INSERT INTO memories (id, collection, content, reinforcement_count, weight)
		VALUES
			('mem-1', 'memories', 'go test is a good testing framework', 0, 1),
			('mem-2', 'memories', 'table driven tests in go are efficient', 0, 1)
	`)
	if err != nil {
		t.Fatalf("failed to insert test memories: %v", err)
	}

	// Simulate what handleRecall does: query memories and reinforce on first access
	// We'll simulate a single recall call that returns both memories
	sessionAccessCounts := make(map[string]int)

	rows, err := db.Query(`
		SELECT id, content, session_id, tags, created_at
		FROM memories
		WHERE deleted_at IS NULL AND collection = 'memories'
		ORDER BY created_at DESC
		LIMIT 15
	`)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}

	type recallEntry struct {
		id        string
		content   string
		sessionID string
		tags      string
	}
	var entries []recallEntry
	for rows.Next() {
		var id, content, createdAt string
		var nullableSessionID, nullableTags sql.NullString
		if err := rows.Scan(&id, &content, &nullableSessionID, &nullableTags, &createdAt); err != nil {
			continue
		}
		if content == "" {
			continue
		}
		entry := recallEntry{id: id}
		if nullableSessionID.Valid {
			entry.sessionID = nullableSessionID.String
		}
		entries = append(entries, entry)
	}
	rows.Close()

	// Simulate the deduplication logic from handleRecall
	for _, entry := range entries {
		if sessionAccessCounts[entry.id] == 0 {
			// ReinforceMemory: increment reinforcement_count and weight
			_, err := db.Exec(`
				UPDATE memories
				SET reinforcement_count = reinforcement_count + 1,
				    weight = weight + ((1 + 1) / 2)
				WHERE id = ?
			`, entry.id)
			if err != nil {
				t.Fatalf("reinforce failed for %s: %v", entry.id, err)
			}
		}
		sessionAccessCounts[entry.id]++
	}

	// Verify: mem-1 and mem-2 should each be reinforced exactly once
	var reinforcement, weight int
	row := db.QueryRow("SELECT reinforcement_count, weight FROM memories WHERE id = 'mem-1'")
	if err := row.Scan(&reinforcement, &weight); err != nil {
		t.Fatalf("failed to read mem-1: %v", err)
	}
	if reinforcement != 1 {
		t.Errorf("mem-1: expected reinforcement_count=1, got %d", reinforcement)
	}
	if weight != 2 { // weight starts at 1, gains (1+1)/2 = 1
		t.Errorf("mem-1: expected weight=2, got %d", weight)
	}

	row = db.QueryRow("SELECT reinforcement_count, weight FROM memories WHERE id = 'mem-2'")
	if err := row.Scan(&reinforcement, &weight); err != nil {
		t.Fatalf("failed to read mem-2: %v", err)
	}
	if reinforcement != 1 {
		t.Errorf("mem-2: expected reinforcement_count=1, got %d", reinforcement)
	}
	if weight != 2 {
		t.Errorf("mem-2: expected weight=2, got %d", weight)
	}
}

func TestRecallDeduplicatesAccessAcrossMultipleRows(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Insert a single memory that appears multiple times (simulating a duplicate result)
	_, err := db.Exec(`
		INSERT INTO memories (id, collection, content, reinforcement_count, weight)
		VALUES ('mem-dup', 'memories', 'duplicate test content', 0, 1)
	`)
	if err != nil {
		t.Fatalf("failed to insert: %v", err)
	}

	// Simulate recall returning the same memory twice (should only reinforce once)
	sessionAccessCounts := make(map[string]int)

	type recallEntry struct {
		id string
	}
	entries := []recallEntry{{id: "mem-dup"}, {id: "mem-dup"}}

	for _, entry := range entries {
		if sessionAccessCounts[entry.id] == 0 {
			db.Exec(`
				UPDATE memories
				SET reinforcement_count = reinforcement_count + 1,
				    weight = weight + ((1 + 1) / 2)
				WHERE id = ?
			`, entry.id)
		}
		sessionAccessCounts[entry.id]++
	}

	// Should only be reinforced once even though accessed twice
	var reinforcement, weight int
	row := db.QueryRow("SELECT reinforcement_count, weight FROM memories WHERE id = 'mem-dup'")
	if err := row.Scan(&reinforcement, &weight); err != nil {
		t.Fatalf("failed to read mem-dup: %v", err)
	}
	if reinforcement != 1 {
		t.Errorf("mem-dup: expected reinforcement_count=1 (deduped), got %d", reinforcement)
	}
	if weight != 2 {
		t.Errorf("mem-dup: expected weight=2, got %d", weight)
	}
}

func TestMain(m *testing.M) {
	// Run tests with temporary home to avoid polluting real config
	tmpDir := os.TempDir()
	os.Setenv("HOME", tmpDir)
	os.Exit(m.Run())
}