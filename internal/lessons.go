package internal

import (
	"sync"
)

// LessonStore provides lesson operations via DatabaseManager.
// The lessons table is part of the unified schema in DatabaseManager,
// so this store wraps DatabaseManager methods for backwards compatibility.
type LessonStore struct {
	dm *DatabaseManager
}

// NewLessonStore returns a LessonStore using the shared DatabaseManager.
// The database and lessons table are initialized by DatabaseManager at startup.
func NewLessonStore(dbPath string) *LessonStore {
	return &LessonStore{}
}

// Init ensures the lessons table exists (no-op since DatabaseManager handles this).
func (s *LessonStore) Init() error {
	// Lessons table is created by DatabaseManager.initUnifiedSchema()
	return nil
}

// getSharedDB returns a singleton DatabaseManager instance.
// Creates and caches the manager on first call; subsequent calls return the cached instance.
var (
	sharedDM     *DatabaseManager
	sharedDMOnce sync.Once
	sharedDMErr  error
)

func getSharedDB() (*DatabaseManager, error) {
	sharedDMOnce.Do(func() {
		sharedDM, sharedDMErr = NewDatabaseManager("")
	})
	return sharedDM, sharedDMErr
}

// AddLesson delegates to DatabaseManager.AddLesson
func (s *LessonStore) AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error) {
	dm, err := getSharedDB()
	if err != nil {
		return nil, err
	}
	return dm.AddLesson(content, lessonType, tags, sourceSessionID)
}

// GetLesson delegates to DatabaseManager.GetLesson
func (s *LessonStore) GetLesson(id string) (*Lesson, error) {
	dm, err := getSharedDB()
	if err != nil {
		return nil, err
	}
	return dm.GetLesson(id)
}

// ListLessons delegates to DatabaseManager.ListLessons
func (s *LessonStore) ListLessons(lessonType LessonType) ([]*Lesson, error) {
	dm, err := getSharedDB()
	if err != nil {
		return nil, err
	}
	return dm.ListLessons(string(lessonType))
}

// SearchLessons delegates to DatabaseManager.SearchLessons
func (s *LessonStore) SearchLessons(query string, limit int) ([]*Lesson, error) {
	dm, err := getSharedDB()
	if err != nil {
		return nil, err
	}
	return dm.SearchLessons(query, limit)
}

// DeleteLesson delegates to DatabaseManager.DeleteLesson
func (s *LessonStore) DeleteLesson(id string) error {
	dm, err := getSharedDB()
	if err != nil {
		return err
	}
	return dm.DeleteLesson(id)
}

// GetLessonStats delegates to DatabaseManager.GetLessonStats
func (s *LessonStore) GetLessonStats() (map[string]interface{}, error) {
	dm, err := getSharedDB()
	if err != nil {
		return nil, err
	}
	return dm.GetLessonStats()
}

