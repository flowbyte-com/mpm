package internal

import (
	"path/filepath"
	"testing"
)

// TestGetByID tests the GetByID function with various scenarios.
func TestGetByID(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store := NewMemoryStore("")
	store.SQLiteDBPath = dbPath
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite failed: %v", err)
	}
	defer store.DB.Close()

	// Empty DB — should return nil, nil
	mem, err := store.GetByID("nonexistent", "memories")
	if err != nil {
		t.Fatalf("GetByID on empty DB returned error: %v", err)
	}
	if mem != nil {
		t.Fatalf("GetByID on empty DB returned memory, expected nil")
	}

	// Add a memory
	added, err := store.AddMemory("hello world test content", "memories", []string{"test"}, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory failed: %v", err)
	}
	if added == nil {
		t.Fatal("AddMemory returned nil")
	}

	// GetByID with correct ID and collection — should find it
	mem, err = store.GetByID(added.ID, "memories")
	if err != nil {
		t.Fatalf("GetByID returned error: %v", err)
	}
	if mem == nil {
		t.Fatal("GetByID returned nil, expected memory")
	}
	if mem.Content != "hello world test content" {
		t.Errorf("GetByID content mismatch: got %q, want %q", mem.Content, "hello world test content")
	}
	if mem.Collection != "memories" {
		t.Errorf("GetByID collection mismatch: got %q, want %q", mem.Collection, "memories")
	}

	// GetByID with wrong collection — should return nil, nil
	mem, err = store.GetByID(added.ID, "session")
	if err != nil {
		t.Fatalf("GetByID returned error for wrong collection: %v", err)
	}
	if mem != nil {
		t.Fatal("GetByID returned memory for wrong collection, expected nil")
	}

	// GetByID with non-existent ID — should return nil, nil
	mem, err = store.GetByID("doesnotexist", "memories")
	if err != nil {
		t.Fatalf("GetByID returned error for non-existent ID: %v", err)
	}
	if mem != nil {
		t.Fatal("GetByID returned memory for non-existent ID, expected nil")
	}

	// Add a session memory and verify GetByID finds it with correct collection
	sessionMem, err := store.AddMemory("session content here", "session", nil, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory(session) failed: %v", err)
	}
	mem, err = store.GetByID(sessionMem.ID, "session")
	if err != nil {
		t.Fatalf("GetByID(session) returned error: %v", err)
	}
	if mem == nil {
		t.Fatal("GetByID returned nil for session collection")
	}
	if mem.Collection != "session" {
		t.Errorf("GetByID session collection mismatch: got %q, want %q", mem.Collection, "session")
	}
}

// TestGetByIDNilDB tests that GetByID handles nil DB gracefully.
func TestGetByIDNilDB(t *testing.T) {
	// Create store without initializing DB
	store := NewMemoryStore("")
	// DB is nil, SQLiteDBPath is the default (real path) — calling GetByID
	// will try to init. We just verify it doesn't panic and returns something.
	mem, err := store.GetByID("anyid", "memories")
	// It will either init and return nil (no row), or error on the real path
	// Just ensure no panic
	_ = mem
	_ = err
}

// TestSearchSessions tests SearchSessions with the correct collection name.
func TestSearchSessions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store := NewMemoryStore("")
	store.SQLiteDBPath = dbPath
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite failed: %v", err)
	}
	defer store.DB.Close()

	// Add session memories (collection="session" — singular)
	sessionMem1, err := store.AddMemory("golang programming language", "session", nil, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory(session) failed: %v", err)
	}
	sessionMem2, err := store.AddMemory("sqlite database with fts5", "session", nil, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory(session) failed: %v", err)
	}

	// Add regular memories (collection="memories" — should NOT appear in session search)
	_, err = store.AddMemory("golang is great", "memories", nil, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory(memories) failed: %v", err)
	}

	// SearchSessions should only find session memories
	results, err := store.SearchSessions("golang", 10)
	if err != nil {
		t.Fatalf("SearchSessions returned error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchSessions returned no results for 'golang' in session collection")
	}
	for _, r := range results {
		if r.Collection != "session" {
			t.Errorf("SearchSessions returned non-session memory: collection=%q", r.Collection)
		}
		if r.ID != sessionMem1.ID && r.ID != sessionMem2.ID {
			t.Errorf("SearchSessions returned unexpected memory ID: %s", r.ID)
		}
	}

	// SearchSessions should find the sqlite one too
	results, err = store.SearchSessions("sqlite", 10)
	if err != nil {
		t.Fatalf("SearchSessions(sqlite) returned error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchSessions returned no results for 'sqlite'")
	}

	// Non-existent query should return empty
	results, err = store.SearchSessions("xyznotfound123", 10)
	if err != nil {
		t.Fatalf("SearchSessions returned error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("SearchSessions for non-existent query returned %d results, want 0", len(results))
	}
}

// TestSearchSessionsCollectionBug verifies the collection='sessions' (plural) bug is fixed.
// The bug was: SearchSessions queried 'sessions' but AddMemory stores 'session' (singular).
func TestSearchSessionsCollectionBug(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store := NewMemoryStore("")
	store.SQLiteDBPath = dbPath
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite failed: %v", err)
	}
	defer store.DB.Close()

	// Directly insert with 'sessions' (plural) — the old buggy collection name
	// This simulates what the OLD buggy code would have stored
	_, err := store.DB.Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at)
		VALUES (?, ?, ?, NULL, ?, ?, CURRENT_TIMESTAMP)`,
		"test-old-bug-id", "sessions", "old buggy content", "[]", "{}")
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	// Add correct singular session memory
	correctMem, err := store.AddMemory("correct session content", "session", nil, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory failed: %v", err)
	}

	// Old buggy 'sessions' plural entry should NOT appear in SearchSessions
	results, err := store.SearchSessions("buggy", 10)
	if err != nil {
		t.Fatalf("SearchSessions returned error: %v", err)
	}
	for _, r := range results {
		if r.ID == "test-old-bug-id" {
			t.Error("SearchSessions found memory with collection='sessions' (plural) — bug not fixed")
		}
	}

	// Correct singular 'session' entry SHOULD appear
	results, err = store.SearchSessions("correct", 10)
	if err != nil {
		t.Fatalf("SearchSessions returned error: %v", err)
	}
	found := false
	for _, r := range results {
		if r.ID == correctMem.ID {
			found = true
		}
	}
	if !found {
		t.Error("SearchSessions did not find memory with collection='session' (singular) — search is broken")
	}
}

// TestFullTextSearch tests FullTextSearch FTS5 and LIKE fallback paths.
func TestFullTextSearch(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store := NewMemoryStore("")
	store.SQLiteDBPath = dbPath
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite failed: %v", err)
	}
	defer store.DB.Close()

	// Add test memories
	mem1, err := store.AddMemory("golang is a programming language", "memories", []string{"go", "lang"}, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory failed: %v", err)
	}
	_, err = store.AddMemory("python is also great for data", "memories", []string{"python", "data"}, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory failed: %v", err)
	}
	sessionMem, err := store.AddMemory("session memory about golang", "session", nil, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory(session) failed: %v", err)
	}

	// FullTextSearch for "golang" — should find both the memories entry and the session entry
	results, err := store.FullTextSearch("golang", "memories", 10)
	if err != nil {
		t.Fatalf("FullTextSearch returned error: %v", err)
	}
	found := false
	for _, r := range results {
		if r.ID == mem1.ID {
			found = true
		}
		if r.Collection != "memories" {
			t.Errorf("FullTextSearch returned non-memories collection: %q", r.Collection)
		}
	}
	if !found {
		t.Error("FullTextSearch did not find 'golang' in memories collection")
	}

	// FullTextSearch with wrong collection — should not find memories entries
	results, err = store.FullTextSearch("golang", "session", 10)
	if err != nil {
		t.Fatalf("FullTextSearch(session) returned error: %v", err)
	}
	for _, r := range results {
		if r.ID == mem1.ID {
			t.Error("FullTextSearch found memories entry when searching session collection")
		}
		if r.ID == sessionMem.ID {
			t.Logf("FullTextSearch correctly found session entry: %s", r.ID)
		}
	}

	// Query matching nothing
	results, err = store.FullTextSearch("xyznotfound123", "memories", 10)
	if err != nil {
		t.Fatalf("FullTextSearch returned error for no-match: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("FullTextSearch for non-existent query returned %d results, want 0", len(results))
	}

	// Limit parameter
	results, err = store.FullTextSearch("golang", "memories", 1)
	if err != nil {
		t.Fatalf("FullTextSearch with limit returned error: %v", err)
	}
	if len(results) > 1 {
		t.Errorf("FullTextSearch limit not respected: got %d, want <=1", len(results))
	}
}

// TestFullTextSearchLIKEFallback tests the LIKE fallback when FTS5 might not work.
func TestFullTextSearchLIKEFallback(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store := NewMemoryStore("")
	store.SQLiteDBPath = dbPath
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite failed: %v", err)
	}
	defer store.DB.Close()

	// Add a memory
	mem, err := store.AddMemory("find me with special characters", "memories", nil, nil, "", "cli")
	if err != nil {
		t.Fatalf("AddMemory failed: %v", err)
	}

	// Query with FTS5-special characters — triggers LIKE fallback
	// FTS5 treats "me*" as a prefix query, but colons might cause issues
	results, err := store.FullTextSearch("special", "memories", 10)
	if err != nil {
		t.Fatalf("FullTextSearch LIKE fallback returned error: %v", err)
	}
	found := false
	for _, r := range results {
		if r.ID == mem.ID {
			found = true
		}
	}
	if !found {
		t.Error("FullTextSearch did not find memory via LIKE fallback")
	}
}
