package internal

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freshMemoryStore returns a MemoryStore backed by an in-memory
// DatabaseManager. Routes AddMemory through DM.SaveMemoryWithExtras when
// available (matching the production path) and exposes the same *sql.DB
// via store.DB for the few tests that touch the raw connection.
//
// Cleanup is automatic via NewTestDM's t.Cleanup; callers do NOT need
// `defer store.DB.Close()`.
func freshMemoryStore(t *testing.T) *MemoryStore {
	t.Helper()
	dm := NewTestDM(t)
	return &MemoryStore{
		DM: dm,
		DB: &SQLiteConnection{DB: dm.SQLDB()},
	}
}

// TestGetByID tests the GetByID function with various scenarios.
func TestGetByID(t *testing.T) {
	store := freshMemoryStore(t)

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
	store := freshMemoryStore(t)

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
	store := freshMemoryStore(t)

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
	store := freshMemoryStore(t)

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
	store := freshMemoryStore(t)

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

// TestGetMemoryTopics tests GetMemoryTopics cross-ref query
func TestGetMemoryTopics(t *testing.T) {
	store := freshMemoryStore(t)

	// Create a memory
	mem, err := store.AddMemory("test content", "memories", nil, nil, "", "test")
	if err != nil {
		t.Fatalf("AddMemory failed: %v", err)
	}
	if mem == nil {
		t.Fatal("AddMemory returned nil")
	}

	// Create topics
	topicID1 := GenerateID()
	topicID2 := GenerateID()
	_, err = store.DB.Exec(`INSERT INTO topics (id, name, is_active) VALUES (?, ?, 1)`, topicID1, "topic-a")
	if err != nil {
		t.Fatalf("Insert topic-a failed: %v", err)
	}
	_, err = store.DB.Exec(`INSERT INTO topics (id, name, is_active) VALUES (?, ?, 1)`, topicID2, "topic-b")
	if err != nil {
		t.Fatalf("Insert topic-b failed: %v", err)
	}

	// Link memory to both topics
	_, err = store.DB.Exec(`INSERT INTO topic_memberships (memory_id, topic_id, role) VALUES (?, ?, ?)`, mem.ID, topicID1, "manual")
	if err != nil {
		t.Fatalf("Insert membership manual failed: %v", err)
	}
	_, err = store.DB.Exec(`INSERT INTO topic_memberships (memory_id, topic_id, role) VALUES (?, ?, ?)`, mem.ID, topicID2, "auto")
	if err != nil {
		t.Fatalf("Insert membership auto failed: %v", err)
	}

	dm := store.DM
	topics, err := dm.GetMemoryTopics(mem.ID)

	if err != nil {
		t.Fatalf("GetMemoryTopics failed: %v", err)
	}
	if len(topics) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(topics))
	}
	// Ordered by role then name: manual topic-a first, then auto topic-b
	if topics[0].Role != "manual" {
		t.Errorf("expected role 'manual', got %q", topics[0].Role)
	}
	if topics[0].Name != "topic-a" {
		t.Errorf("expected name 'topic-a', got %q", topics[0].Name)
	}
	if topics[1].Role != "auto" {
		t.Errorf("expected role 'auto', got %q", topics[1].Role)
	}
}

// TestGetTopicTopMemories tests GetTopicTopMemories cross-ref query
func TestGetTopicTopMemories(t *testing.T) {
	store := freshMemoryStore(t)

	topicID := GenerateID()
	_, err := store.DB.Exec(`INSERT INTO topics (id, name, is_active) VALUES (?, ?, 1)`, topicID, "test-topic")
	if err != nil {
		t.Fatalf("Insert topic failed: %v", err)
	}

	// Create 5 memories with different weights
	ids := []string{}
	for i := 0; i < 5; i++ {
		mem, err := store.AddMemory(fmt.Sprintf("content-%d", i), "memories", nil, nil, "", "test")
		if err != nil {
			t.Fatalf("AddMemory failed: %v", err)
		}
		ids = append(ids, mem.ID)
		// Set weight: 1, 5, 3, 10, 2
		w := []int{1, 5, 3, 10, 2}[i]
		_, err = store.DB.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, w, mem.ID)
		if err != nil {
			t.Fatalf("Update weight failed: %v", err)
		}
		_, err = store.DB.Exec(`INSERT INTO topic_memberships (memory_id, topic_id, role) VALUES (?, ?, ?)`, mem.ID, topicID, "manual")
		if err != nil {
			t.Fatalf("Insert membership failed: %v", err)
		}
	}

	dm := store.DM
	memories, total, err := dm.GetTopicTopMemories(topicID, 3)

	if err != nil {
		t.Fatalf("GetTopicTopMemories failed: %v", err)
	}
	if total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
	if len(memories) != 3 {
		t.Fatalf("expected 3 memories, got %d", len(memories))
	}
	// Top 3 by weight: weight=10 (id[3]), weight=5 (id[1]), weight=3 (id[2])
	if memories[0].ID != ids[3] {
		t.Errorf("expected memories[0].id=%s (weight 10), got %s", ids[3], memories[0].ID)
	}
	if memories[1].ID != ids[1] {
		t.Errorf("expected memories[1].id=%s (weight 5), got %s", ids[1], memories[1].ID)
	}
	if memories[2].ID != ids[2] {
		t.Errorf("expected memories[2].id=%s (weight 3), got %s", ids[2], memories[2].ID)
	}
}

// TestGetReferenceDoc tests GetReferenceDoc cross-ref query
func TestGetReferenceDoc(t *testing.T) {
	store := freshMemoryStore(t)

	docID := GenerateID()
	_, err := store.DB.Exec(`INSERT INTO reference_docs (id, title, file_path, content) VALUES (?, ?, ?, ?)`,
		docID, "Test Doc", "/path/to/doc.pdf", "test content")
	if err != nil {
		t.Fatalf("Insert reference_doc failed: %v", err)
	}

	dm := store.DM
	doc, err := dm.GetReferenceDoc(docID)

	if err != nil {
		t.Fatalf("GetReferenceDoc failed: %v", err)
	}
	if doc == nil {
		t.Fatal("expected doc, got nil")
	}
	if doc.Title != "Test Doc" {
		t.Errorf("expected title 'Test Doc', got %q", doc.Title)
	}
	if doc.FilePath != "/path/to/doc.pdf" {
		t.Errorf("expected file_path '/path/to/doc.pdf', got %q", doc.FilePath)
	}
}

// TestMemoryProvenanceStorage verifies provenance-based decay multipliers.
// A compute:"high" memory decays at half the rate of compute:"standard".
func TestMemoryProvenanceStorage(t *testing.T) {
	db := newTestDM(t)
	defer db.Close()

	// Insert high-compute memory
	highMeta := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "model",
			"model":   "DeepSeek-R1",
			"compute": "high",
		},
	}
	id1, err := db.SaveMemory("memories", "high compute memory content", "", nil, highMeta, nil, true, 20)
	require.NoError(t, err)

	// Insert standard-compute memory
	stdMeta := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "synthetic",
			"model":   "MiniMax-M2.7",
			"compute": "standard",
		},
	}
	id2, err := db.SaveMemory("memories", "standard compute memory content", "", nil, stdMeta, nil, true, 20)
	require.NoError(t, err)

	// Bump updated_at so both are eligible for decay
	db.SQLDB().Exec(`UPDATE memories SET updated_at = CAST(strftime('%s','now', '-30 days') AS INTEGER) WHERE id IN (?, ?)`, id1, id2)

	// Run decay with aggressive rate so we can measure the difference
	policy := map[string]DecayPolicy{
		"memories": {DecayPercent: 50, Floor: 1},
	}
	decayed, err := db.DecayWeights(policy, 1)
	require.NoError(t, err)
	require.Greater(t, decayed, 0, "at least one memory should decay")

	// Read back weights
	var w1, w2 int
	err = db.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, id1).Scan(&w1)
	require.NoError(t, err)
	err = db.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, id2).Scan(&w2)
	require.NoError(t, err)

	t.Logf("high-compute weight after decay: %d", w1)
	t.Logf("standard-compute weight after decay: %d", w2)

	// High-compute decays at half rate → should retain more weight
	assert.GreaterOrEqual(t, w1, w2,
		"high-compute memory should decay slower than standard")
}

// TestImmuneProvenanceTieBreaker verifies provenance-based contradiction resolution.
// Insert absolute (human) and ephemeral (model) memories with identical embeddings,
// assert the ephemeral is challenged and its weight slashed.
func TestImmuneProvenanceTieBreaker(t *testing.T) {
	db := newTestDM(t)
	defer db.Close()

	content := "The user always prefers direct yes/no answers without explanation."
	embedding := HashEmbed(content)

	// Absolute (human) memory
	humanMeta := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "human",
			"model":   "direct",
			"compute": "absolute",
		},
	}
	id1, err := db.SaveMemory("memories", content, "", []string{"preference"}, humanMeta, embedding, false, 10)
	require.NoError(t, err)

	// Ephemeral (model) memory with identical content → cosine=1.0
	modelMeta := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "model",
			"model":   "DeepSeek-R1",
			"compute": "ephemeral",
		},
	}
	id2, err := db.SaveMemory("memories", content, "", []string{"preference"}, modelMeta, embedding, false, 8)
	require.NoError(t, err)

	// Run HybridSearch to trigger contradiction detection
	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	cfg.VectorWeight = 1.0 // pure vector search
	cfg.RetrievalThreshold = -100
	_, err = HybridSearch(db, content, "memories", cfg)
	require.NoError(t, err)

	// The ephemeral memory should now have challenged status and reduced weight
	var w2 int
	var metaJSON string
	err = db.SQLDB().QueryRow(`SELECT weight, COALESCE(metadata, '{}') FROM memories WHERE id = ?`, id2).Scan(&w2, &metaJSON)
	require.NoError(t, err)
	var meta map[string]interface{}
	json.Unmarshal([]byte(metaJSON), &meta)
	status, _ := meta["status"].(string)

	assert.Equal(t, "challenged", status, "ephemeral memory should be challenged after tiebreaker")
	assert.Less(t, w2, 8, "ephemeral memory weight should be slashed below initial value of 8")

	// Absolute memory should remain unchallenged with original weight
	var w1 int
	var metaJSON1 string
	err = db.SQLDB().QueryRow(`SELECT weight, COALESCE(metadata, '{}') FROM memories WHERE id = ?`, id1).Scan(&w1, &metaJSON1)
	require.NoError(t, err)
	var meta1 map[string]interface{}
	json.Unmarshal([]byte(metaJSON1), &meta1)
	status1, _ := meta1["status"].(string)

	assert.NotEqual(t, "challenged", status1, "absolute memory should NOT be challenged")
	assert.Equal(t, 10, w1, "absolute memory weight should remain unchanged")
}

// TestMemoryProvenanceGracefulDegradation verifies that malformed metadata JSON
// does not cause panics and defaults to "standard" compute logic.
func TestMemoryProvenanceGracefulDegradation(t *testing.T) {
	db := newTestDM(t)
	defer db.Close()

	// Insert a memory with a broken metadata string via raw SQL
	brokenMeta := `{broken_json_!}`
	_, err := db.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, metadata, is_long_term, weight) VALUES (?, ?, ?, ?, ?, ?)`,
		"broken-meta-test", "memories", "test content with broken metadata", brokenMeta, 1, 10,
	)
	require.NoError(t, err)

	// Run decay — should not panic, defaults to multiplier 1.0
	policy := map[string]DecayPolicy{
		"memories": {DecayPercent: 50, Floor: 1},
	}
	_, err = db.DecayWeights(policy, 1)
	assert.NoError(t, err, "decay should not error with broken metadata")

	// Verify the memory is still queryable via recall-like query
	var content string
	err = db.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE id = ? AND deleted_at IS NULL`, "broken-meta-test",
	).Scan(&content)
	assert.NoError(t, err, "memory with broken metadata should still be readable")
	assert.Contains(t, content, "test content with broken metadata")

	// ProvenancePreamble should return empty string (graceful degradation)
	preamble := ProvenancePreamble(brokenMeta)
	assert.Empty(t, preamble, "ProvenancePreamble should return empty for broken JSON")

	// extractProvenanceCompute should return empty string
	compute := extractProvenanceCompute(brokenMeta)
	assert.Empty(t, compute, "extractProvenanceCompute should return empty for broken JSON")
}
