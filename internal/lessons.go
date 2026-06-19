package internal

import "fmt"

// LessonStore provides lesson operations via DatabaseManager.
// The lessons table is part of the unified schema in DatabaseManager,
// so this store wraps DatabaseManager methods for backwards compatibility.
type LessonStore struct {
	dm *DatabaseManager
}

// NewLessonStore returns a LessonStore backed by the DatabaseManager at dbPath.
func NewLessonStore(dbPath string) (*LessonStore, error) {
	dm, err := NewDatabaseManager(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize lesson store: %w", err)
	}
	return &LessonStore{dm: dm}, nil
}

// Init is a no-op; the lessons table is created by DatabaseManager at startup.
func (s *LessonStore) Init() error {
	return nil
}

// AddLesson delegates to DatabaseManager.AddLesson
func (s *LessonStore) AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error) {
	if s.dm == nil {
		return nil, fmt.Errorf("lesson store: database not initialized")
	}
	return s.dm.AddLesson(content, lessonType, tags, sourceSessionID)
}

// GetLesson delegates to DatabaseManager.GetLesson
func (s *LessonStore) GetLesson(id string) (*Lesson, error) {
	if s.dm == nil {
		return nil, fmt.Errorf("lesson store: database not initialized")
	}
	return s.dm.GetLesson(id)
}

// ListLessons delegates to DatabaseManager.ListLessons
func (s *LessonStore) ListLessons(lessonType LessonType) ([]*Lesson, error) {
	if s.dm == nil {
		return nil, fmt.Errorf("lesson store: database not initialized")
	}
	return s.dm.ListLessons(string(lessonType))
}

// SearchLessons delegates to DatabaseManager.SearchLessons
func (s *LessonStore) SearchLessons(query string, limit int) ([]*Lesson, error) {
	if s.dm == nil {
		return nil, fmt.Errorf("lesson store: database not initialized")
	}
	return s.dm.SearchLessons(query, limit)
}

// DeleteLesson delegates to DatabaseManager.DeleteLesson
func (s *LessonStore) DeleteLesson(id string) error {
	if s.dm == nil {
		return fmt.Errorf("lesson store: database not initialized")
	}
	return s.dm.DeleteLesson(id)
}

// GetLessonStats delegates to DatabaseManager.GetLessonStats
func (s *LessonStore) GetLessonStats() (map[string]interface{}, error) {
	if s.dm == nil {
		return nil, fmt.Errorf("lesson store: database not initialized")
	}
	return s.dm.GetLessonStats()
}
