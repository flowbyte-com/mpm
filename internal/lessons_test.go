package internal

import (
	"testing"
)

func TestLessonStoreInit(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()

	// Verify lessons table exists
	var count int
	err = dm.db.QueryRow("SELECT COUNT(*) FROM lessons").Scan(&count)
	if err != nil {
		t.Errorf("Failed to query lessons: %v", err)
	}
}

func TestLessonStoreAddAndGet(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()

	// Wipe existing lessons to ensure clean state
	dm.db.Exec("DELETE FROM lessons")

	// Add a lesson
	lesson, err := dm.AddLesson("Check file extensions before executing", LessonTypeWarning, []string{"safety", "files"}, "")
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
	retrieved, err := dm.GetLesson(lesson.ID)
	if err != nil {
		t.Fatalf("GetLesson failed: %v", err)
	}

	if retrieved.Content != lesson.Content {
		t.Errorf("Expected %s, got: %s", lesson.Content, retrieved.Content)
	}
}

func TestLessonStoreDeduplication(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()

	// Wipe existing lessons to ensure clean state
	dm.db.Exec("DELETE FROM lessons")

	content := "Test deduplication content"

	// Add same content twice
	lesson1, err := dm.AddLesson(content, LessonTypeInsight, nil, "")
	if err != nil {
		t.Fatalf("AddLesson failed: %v", err)
	}

	lesson2, err := dm.AddLesson(content, LessonTypeInsight, nil, "")
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

	retrieved, _ := dm.GetLesson(lesson1.ID)
	if retrieved.ReinforcementCount != 2 {
		t.Errorf("Expected reinforcement 2 after duplicate add, got: %d", retrieved.ReinforcementCount)
	}
}

func TestLessonStoreListLessons(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()

	// Wipe any existing lessons from previous test runs using this dm
	dm.db.Exec("DELETE FROM lessons")

	// Add lessons of different types
	dm.AddLesson("Warning 1", LessonTypeWarning, nil, "")
	dm.AddLesson("Practice 1", LessonTypePractice, nil, "")
	dm.AddLesson("Insight 1", LessonTypeInsight, nil, "")
	dm.AddLesson("Warning 2", LessonTypeWarning, nil, "")

	// List all
	all, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("ListLessons failed: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("Expected 4 lessons, got: %d", len(all))
	}

	// List by type
	warnings, err := dm.ListLessons(string(LessonTypeWarning))
	if err != nil {
		t.Fatalf("ListLessons failed: %v", err)
	}
	if len(warnings) != 2 {
		t.Errorf("Expected 2 warnings, got: %d", len(warnings))
	}

	practices, err := dm.ListLessons(string(LessonTypePractice))
	if err != nil {
		t.Fatalf("ListLessons failed: %v", err)
	}
	if len(practices) != 1 {
		t.Errorf("Expected 1 practice, got: %d", len(practices))
	}
}

func TestLessonStoreDelete(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()

	lesson, _ := dm.AddLesson("To be deleted", LessonTypeInsight, nil, "")

	err = dm.DeleteLesson(lesson.ID)
	if err != nil {
		t.Fatalf("DeleteLesson failed: %v", err)
	}

	_, err = dm.GetLesson(lesson.ID)
	if err == nil {
		t.Error("Expected error after deleting lesson, got nil")
	}
}

func TestLessonStoreStats(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()

	// Wipe existing lessons to ensure clean state
	dm.db.Exec("DELETE FROM lessons")

	// Get initial stats
	stats, err := dm.GetLessonStats()
	if err != nil {
		t.Fatalf("GetLessonStats failed: %v", err)
	}

	if stats["total_lessons"] != 0 {
		t.Errorf("Expected 0 lessons initially, got: %d", stats["total_lessons"])
	}

	// Add lessons
	dm.AddLesson("W1", LessonTypeWarning, nil, "")
	dm.AddLesson("W2", LessonTypeWarning, nil, "")
	dm.AddLesson("P1", LessonTypePractice, nil, "")
	dm.AddLesson("I1", LessonTypeInsight, nil, "")

	stats, err = dm.GetLessonStats()
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
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()

	dm.db.Exec("DELETE FROM lessons")
	dm.AddLesson("Always validate paths before rm -rf", LessonTypeWarning, []string{"safety"}, "")
	dm.AddLesson("Use gofmt for Go code formatting", LessonTypePractice, []string{"go", "style"}, "")

	results, err := dm.SearchLessons("validate", 10)
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
