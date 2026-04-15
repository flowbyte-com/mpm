package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractLessonIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")

	if err := InitMiniBotDB(dbPath); err != nil {
		t.Fatalf("InitMiniBotDB failed: %v", err)
	}

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// First insert
	lesson := Lesson{
		ID:      "test-lesson-1",
		Content: "Always validate input before processing",
		Type:    "insight",
		Tags:    "validation,security",
	}
	if err := ExtractLesson(db, lesson); err != nil {
		t.Fatalf("ExtractLesson failed: %v", err)
	}

	// Check reinforcement_count is 1
	var count int
	err = db.QueryRow("SELECT reinforcement_count FROM lessons WHERE content = ?", lesson.Content).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query reinforcement_count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected reinforcement_count 1, got %d", count)
	}

	// Second insert with same content - should be idempotent (increment count)
	lesson2 := Lesson{
		ID:      "test-lesson-2",
		Content: "Always validate input before processing", // same content
		Type:    "insight",
		Tags:    "", // empty tags to test coalesce
	}
	if err := ExtractLesson(db, lesson2); err != nil {
		t.Fatalf("ExtractLesson idempotent insert failed: %v", err)
	}

	// Check reinforcement_count is now 2
	err = db.QueryRow("SELECT reinforcement_count FROM lessons WHERE content = ?", lesson.Content).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query reinforcement_count after second insert: %v", err)
	}
	if count != 2 {
		t.Errorf("expected reinforcement_count 2 after idempotent insert, got %d", count)
	}
}

func TestGetRecentLessons(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")

	if err := InitMiniBotDB(dbPath); err != nil {
		t.Fatalf("InitMiniBotDB failed: %v", err)
	}

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert lessons with different reinforcement counts
	lessons := []Lesson{
		{ID: "l1", Content: "Lesson one", Type: "insight", Tags: "test", ReinforcementCount: 1},
		{ID: "l2", Content: "Lesson two", Type: "rule", Tags: "test", ReinforcementCount: 3},
		{ID: "l3", Content: "Lesson three", Type: "pattern", Tags: "test", ReinforcementCount: 2},
	}

	for _, l := range lessons {
		ExtractLesson(db, l)
	}

	// GetRecentLessons should return ordered by reinforcement_count DESC
	recent, err := GetRecentLessons(db, 10)
	if err != nil {
		t.Fatalf("GetRecentLessons failed: %v", err)
	}

	if len(recent) != 3 {
		t.Fatalf("expected 3 lessons, got %d", len(recent))
	}

	// Should be ordered: Lesson two (3), Lesson three (2), Lesson one (1)
	if recent[0].Content != "Lesson two" {
		t.Errorf("expected first lesson to be 'Lesson two', got '%s'", recent[0].Content)
	}
	if recent[1].Content != "Lesson three" {
		t.Errorf("expected second lesson to be 'Lesson three', got '%s'", recent[1].Content)
	}
	if recent[2].Content != "Lesson one" {
		t.Errorf("expected third lesson to be 'Lesson one', got '%s'", recent[2].Content)
	}
}

func TestGetLessonsByTag(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")

	if err := InitMiniBotDB(dbPath); err != nil {
		t.Fatalf("InitMiniBotDB failed: %v", err)
	}

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	lessons := []Lesson{
		{ID: "l1", Content: "Lesson about Go", Type: "insight", Tags: "golang,backend"},
		{ID: "l2", Content: "Lesson about Python", Type: "insight", Tags: "python,backend"},
		{ID: "l3", Content: "Lesson about testing", Type: "insight", Tags: "golang,testing"},
	}

	for _, l := range lessons {
		ExtractLesson(db, l)
	}

	// Search for golang tag
	golang, err := GetLessonsByTag(db, "golang")
	if err != nil {
		t.Fatalf("GetLessonsByTag failed: %v", err)
	}

	if len(golang) != 2 {
		t.Fatalf("expected 2 golang lessons, got %d", len(golang))
	}
}

func TestProposeAndApplyIdentityPatch(t *testing.T) {
	dir := t.TempDir()

	// Create initial IDENTITY.md
	initialIdentity := `# MiniBot v1.0
Type: assistant
Core traits: helpful`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(initialIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Propose a patch
	patchContent := `# MiniBot v1.1
Type: assistant
Core traits: helpful,concise
Boundaries: no advice`
	if err := ProposeIdentityPatch(dir, patchContent); err != nil {
		t.Fatalf("ProposeIdentityPatch failed: %v", err)
	}

	// Verify patch file exists
	patchPath := filepath.Join(dir, "IDENTITY_PATCH.md")
	if _, err := os.Stat(patchPath); os.IsNotExist(err) {
		t.Error("IDENTITY_PATCH.md was not created")
	}

	// Apply the patch
	if err := ApplyIdentityPatch(dir); err != nil {
		t.Fatalf("ApplyIdentityPatch failed: %v", err)
	}

	// Verify patch file is deleted
	if _, err := os.Stat(patchPath); !os.IsNotExist(err) {
		t.Error("IDENTITY_PATCH.md should be deleted after apply")
	}

	// Verify IDENTITY.md is updated
	updated, err := os.ReadFile(filepath.Join(dir, "IDENTITY.md"))
	if err != nil {
		t.Fatal(err)
	}

	if string(updated) != patchContent {
		t.Errorf("IDENTITY.md was not updated correctly.\nExpected:\n%s\nGot:\n%s", patchContent, string(updated))
	}
}

func TestRegisterAndGetSelfTools(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")

	if err := InitMiniBotDB(dbPath); err != nil {
		t.Fatalf("InitMiniBotDB failed: %v", err)
	}

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Register a self-generated tool
	err = RegisterSelfTool(db, "analyze_code", "Analyzes code structure", `{"name": "analyze_code", "description": "Analyzes code structure"}`)
	if err != nil {
		t.Fatalf("RegisterSelfTool failed: %v", err)
	}

	// Get self tools
	tools, err := GetSelfTools(db)
	if err != nil {
		t.Fatalf("GetSelfTools failed: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if tools[0]["name"] != "analyze_code" {
		t.Errorf("expected tool name 'analyze_code', got '%s'", tools[0]["name"])
	}

	if tools[0]["source"] != "self" {
		t.Errorf("expected source 'self', got '%s'", tools[0]["source"])
	}

	// Verify definition is stored
	if tools[0]["definition"] == "" {
		t.Error("expected non-empty definition")
	}
}

func TestRegisterSelfToolReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")

	if err := InitMiniBotDB(dbPath); err != nil {
		t.Fatalf("InitMiniBotDB failed: %v", err)
	}

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Register same tool name twice
	err = RegisterSelfTool(db, "test_tool", "First description", `{"name": "test_tool"}`)
	if err != nil {
		t.Fatalf("RegisterSelfTool failed: %v", err)
	}

	err = RegisterSelfTool(db, "test_tool", "Updated description", `{"name": "test_tool", "updated": true}`)
	if err != nil {
		t.Fatalf("RegisterSelfTool update failed: %v", err)
	}

	tools, err := GetSelfTools(db)
	if err != nil {
		t.Fatalf("GetSelfTools failed: %v", err)
	}

	// Should still have only 1 tool (replaced)
	if len(tools) != 1 {
		t.Errorf("expected 1 tool after replace, got %d", len(tools))
	}

	if tools[0]["description"] != "Updated description" {
		t.Errorf("expected updated description, got '%s'", tools[0]["description"])
	}
}