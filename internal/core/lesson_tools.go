// lesson_tools.go — DM methods for the lesson collection.
//
// Lessons are reusable knowledge that survives across tasks:
// best practices (practice), warnings (warning), and insights (insight).
// Three methods, three MCP tools, three `mpm call` entries.
package internal

import "fmt"

// SaveLesson persists a lesson. Mirrors callSaveLesson.
func (dm *DatabaseManager) SaveLesson(fact, lessonType string, tags []string) (map[string]interface{}, *Lesson, error) {
	if lessonType == "" {
		lessonType = "insight"
	}
	if err := ValidateLessonType(lessonType); err != nil {
		return nil, nil, err
	}
	lesson, err := dm.AddLesson(fact, LessonType(lessonType), tags, "")
	if err != nil {
		return nil, nil, fmt.Errorf("add lesson: %w", err)
	}
	return map[string]interface{}{
		"success":       true,
		"id":            lesson.ID,
		"type":          string(lesson.Type),
		"reinforcement": lesson.ReinforcementCount,
	}, lesson, nil
}

// SearchLessonsLimited is a convenience wrapper that defaults limit to 10.
func (dm *DatabaseManager) SearchLessonsLimited(query string) ([]map[string]interface{}, error) {
	lessons, err := dm.SearchLessons(query, 10)
	if err != nil {
		return nil, fmt.Errorf("search lessons: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(lessons))
	for _, l := range lessons {
		items = append(items, map[string]interface{}{
			"id":      l.ID,
			"type":    string(l.Type),
			"content": l.Content,
			"tags":    l.Tags,
		})
	}
	return items, nil
}

// ListLessonsFiltered returns lessons of a given type, or all lessons if empty.
//
// RECOMMENDED 12: the lesson struct carries structured fact fields
// (reinforcement_count, source_session_id, retrieval_priority,
// importance, confidence) that were being silently dropped by the
// minimal id/type/content/tags/created_at projection. Agents relying
// on `mpm_lessons list` to triage which lessons are "high confidence
// practice-grade" vs. "single observation warning" had no way to see
// those signal fields — they had to query each lesson by id. This
// projection now exposes the full fact surface, with NULL source_session_id
// carried as a sql.NullString then unwrapped to a nullable JSON
// string (matching the Lesson struct's own JSON contract).
func (dm *DatabaseManager) ListLessonsFiltered(lessonType string) ([]map[string]interface{}, error) {
	lessons, err := dm.ListLessons(lessonType)
	if err != nil {
		return nil, fmt.Errorf("list lessons: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(lessons))
	for _, l := range lessons {
		var sourceSession interface{}
		if l.SourceSessionID.Valid {
			sourceSession = l.SourceSessionID.String
		} else {
			sourceSession = nil
		}
		items = append(items, map[string]interface{}{
			"id":                  l.ID,
			"type":                string(l.Type),
			"content":             l.Content,
			"tags":                l.Tags,
			"created_at":          l.Created,
			"reinforcement_count": l.ReinforcementCount,
			"source_session_id":   sourceSession,
			"retrieval_priority":  l.RetrievalPriority,
			"importance":          l.Importance,
			"confidence":          l.Confidence,
		})
	}
	return items, nil
}