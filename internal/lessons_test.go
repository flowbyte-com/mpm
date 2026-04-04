package internal

import (
	"path/filepath"
	"testing"
)

func TestLessonStoreInit(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lessons.sqlite")

	store := NewLessonStore(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Verify tables exist
	var count int
	err = store.db.QueryRow("SELECT COUNT(*) FROM lessons").Scan(&count)
	if err != nil {
		t.Errorf("Failed to query lessons: %v", err)
	}
}

func TestLessonStoreAddAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lessons.sqlite")

	store := NewLessonStore(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Add a lesson
	lesson, err := store.AddLesson("Check file extensions before executing", LessonTypeWarning, []string{"safety", "files"}, "")
	if err != nil {
		t.Fatalf("AddLesson failed: %v", err)
	}

	if lesson.Content != "Check file extensions before executing" {
		t.Errorf("Expected content, got: %s", lesson.Content)
	}
	if lesson.Type != LessonTypeWarning {
		t.Errorf("Expected type warning, got: %s", lesson.Type)
	}

	// Retrieve it
	retrieved, err := store.GetLesson(lesson.ID)
	if err != nil {
		t.Fatalf("GetLesson failed: %v", err)
	}

	if retrieved.Content != lesson.Content {
		t.Errorf("Expected %s, got: %s", lesson.Content, retrieved.Content)
	}
}

func TestLessonStoreDeduplication(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lessons.sqlite")

	store := NewLessonStore(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	content := "Test deduplication content"

	// Add same content twice
	lesson1, err := store.AddLesson(content, LessonTypeInsight, nil, "")
	if err != nil {
		t.Fatalf("AddLesson failed: %v", err)
	}

	lesson2, err := store.AddLesson(content, LessonTypeInsight, nil, "")
	if err != nil {
		t.Fatalf("AddLesson failed: %v", err)
	}

	// Should return same lesson with reinforced count
	if lesson1.ID != lesson2.ID {
		t.Errorf("Expected same ID for duplicate content, got %s and %s", lesson1.ID, lesson2.ID)
	}

	if lesson1.ReinforcementCount != 1 {
		t.Errorf("Expected reinforcement 1, got: %d", lesson1.ReinforcementCount)
	}

	retrieved, _ := store.GetLesson(lesson1.ID)
	if retrieved.ReinforcementCount != 2 {
		t.Errorf("Expected reinforcement 2 after duplicate add, got: %d", retrieved.ReinforcementCount)
	}
}

func TestLessonStoreListLessons(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lessons.sqlite")

	store := NewLessonStore(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Add lessons of different types
	store.AddLesson("Warning 1", LessonTypeWarning, nil, "")
	store.AddLesson("Practice 1", LessonTypePractice, nil, "")
	store.AddLesson("Insight 1", LessonTypeInsight, nil, "")
	store.AddLesson("Warning 2", LessonTypeWarning, nil, "")

	// List all
	all, err := store.ListLessons("")
	if err != nil {
		t.Fatalf("ListLessons failed: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("Expected 4 lessons, got: %d", len(all))
	}

	// List by type
	warnings, err := store.ListLessons(LessonTypeWarning)
	if err != nil {
		t.Fatalf("ListLessons failed: %v", err)
	}
	if len(warnings) != 2 {
		t.Errorf("Expected 2 warnings, got: %d", len(warnings))
	}

	practices, err := store.ListLessons(LessonTypePractice)
	if err != nil {
		t.Fatalf("ListLessons failed: %v", err)
	}
	if len(practices) != 1 {
		t.Errorf("Expected 1 practice, got: %d", len(practices))
	}
}

func TestLessonStoreDelete(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lessons.sqlite")

	store := NewLessonStore(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	lesson, _ := store.AddLesson("To be deleted", LessonTypeInsight, nil, "")

	err = store.DeleteLesson(lesson.ID)
	if err != nil {
		t.Fatalf("DeleteLesson failed: %v", err)
	}

	_, err = store.GetLesson(lesson.ID)
	if err == nil {
		t.Error("Expected error after deleting lesson, got nil")
	}
}

func TestLessonStoreStats(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lessons.sqlite")

	store := NewLessonStore(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Get initial stats
	stats, err := store.GetLessonStats()
	if err != nil {
		t.Fatalf("GetLessonStats failed: %v", err)
	}

	if stats["total_lessons"] != 0 {
		t.Errorf("Expected 0 lessons initially, got: %d", stats["total_lessons"])
	}

	// Add lessons
	store.AddLesson("W1", LessonTypeWarning, nil, "")
	store.AddLesson("W2", LessonTypeWarning, nil, "")
	store.AddLesson("P1", LessonTypePractice, nil, "")
	store.AddLesson("I1", LessonTypeInsight, nil, "")

	stats, err = store.GetLessonStats()
	if err != nil {
		t.Fatalf("GetLessonStats failed: %v", err)
	}

	if stats["total_lessons"] != 4 {
		t.Errorf("Expected 4 lessons, got: %d", stats["total_lessons"])
	}

	typeCounts := stats["by_type"].(map[string]int)
	if typeCounts["warning"] != 2 {
		t.Errorf("Expected 2 warnings, got: %d", typeCounts["warning"])
	}
}

func TestLessonStoreSearch(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lessons.sqlite")

	store := NewLessonStore(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	store.AddLesson("Always validate paths before rm -rf", LessonTypeWarning, []string{"safety"}, "")
	store.AddLesson("Use gofmt for Go code formatting", LessonTypePractice, []string{"go", "style"}, "")

	results, err := store.SearchLessons("validate", 10)
	if err != nil {
		t.Fatalf("SearchLessons failed: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("Expected 1 result, got: %d", len(results))
	}

	if len(results) > 0 && results[0].Content != "Always validate paths before rm -rf" {
		t.Errorf("Got wrong result: %s", results[0].Content)
	}
}
