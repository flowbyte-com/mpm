package internal

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemory_StructHasNewFields(t *testing.T) {
	m := &Memory{
		RetrievalPriority: 0.6,
		Importance:        0.8,
		Confidence:        0.85,
	}
	assert.InDelta(t, 0.6, m.RetrievalPriority, 1e-9)
	assert.InDelta(t, 0.8, m.Importance, 1e-9)
	assert.InDelta(t, 0.85, m.Confidence, 1e-9)
}

func TestLesson_StructHasNewFields(t *testing.T) {
	l := &Lesson{
		RetrievalPriority: 0.5,
		Importance:        0.7,
		Confidence:        0.7,
	}
	assert.InDelta(t, 0.5, l.RetrievalPriority, 1e-9)
	assert.InDelta(t, 0.7, l.Importance, 1e-9)
	assert.InDelta(t, 0.7, l.Confidence, 1e-9)
}

func TestAddMemory_SetsInitialConfidenceByCollection(t *testing.T) {
	cases := []struct {
		collection string
		wantConf   float64
	}{
		{"memories", 0.8},
		{"theories", 0.5},
		{"decisions", 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.collection, func(t *testing.T) {
			dm := newTestDM(t)
			mem, err := AddMemoryViaDM(dm, "test content "+tc.collection, tc.collection, nil)
			require.NoError(t, err)
			require.NotNil(t, mem)
			assert.InDelta(t, tc.wantConf, mem.Confidence, 1e-9)
		})
	}
}

func TestAddLesson_SetsInitialConfidence(t *testing.T) {
	dm := newTestDM(t)
	l, err := AddLessonViaDM(dm, "lesson content", "insight", nil)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.InDelta(t, 0.7, l.Confidence, 1e-9)
}

// AddMemoryViaDM is a test-friendly wrapper that adds a memory through the
// DatabaseManager. Implementation should match the production path so the
// initial confidence is set correctly.
func AddMemoryViaDM(dm *DatabaseManager, content, collection string, tags []string) (*Memory, error) {
	id := GenerateID()
	conf := InitialConfidence(artifactTypeFromCollection(collection))
	tagsJSON, _ := json.Marshal(tags)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, tags, retrieval_priority, importance, confidence)
		 VALUES (?, ?, ?, ?, 0.5, 0.5, ?)`,
		0, id, collection, content, tagsJSON, conf,
	)
	if err != nil {
		return nil, err
	}
	return &Memory{ID: id, Collection: collection, Content: content, Confidence: conf}, nil
}

func AddLessonViaDM(dm *DatabaseManager, content, lessonType string, tags []string) (*Lesson, error) {
	id := GenerateID()
	conf := InitialConfidence("lesson")
	tagsJSON, _ := json.Marshal(tags)
	now := time.Now().Format(time.RFC3339)
	_, err := dm.ExecTracked(
		`INSERT INTO lessons (id, type, content, tags, created, retrieval_priority, importance, confidence)
		 VALUES (?, ?, ?, ?, ?, 0.5, 0.5, ?)`,
		0, id, lessonType, content, tagsJSON, now, conf,
	)
	if err != nil {
		return nil, err
	}
	return &Lesson{ID: id, Type: LessonType(lessonType), Content: content, Confidence: conf}, nil
}

// TestMemoryStore_AddMemory_PersistsInitialConfidence closes the production-path
// coverage gap: TestAddMemory_SetsInitialConfidenceByCollection above exercises
// only the AddMemoryViaDM helper, not MemoryStore.AddMemory itself. A regression
// in the producer (e.g. dropping the InitialConfidence assignment) would not be
// caught by the helper test. This test routes through the real producer and
// reads the confidence column back from SQLite.
func TestMemoryStore_AddMemory_PersistsInitialConfidence(t *testing.T) {
	dm := newTestDM(t)
	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}

	cases := []struct {
		collection string
		wantConf   float64
	}{
		{"memories", 0.8},
		{"theories", 0.5},
		{"decisions", 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.collection, func(t *testing.T) {
			mem, err := store.AddMemory("content "+tc.collection, tc.collection, nil, nil, "", "test")
			require.NoError(t, err)
			require.NotNil(t, mem)
			assert.InDelta(t, tc.wantConf, mem.Confidence, 1e-9,
				"struct field should hold InitialConfidence for %s", tc.collection)

			var got float64
			err = dm.QueryRowTracked(
				`SELECT confidence FROM memories WHERE id = ?`, mem.ID,
			).Scan(&got)
			require.NoError(t, err)
			assert.InDelta(t, tc.wantConf, got, 1e-9,
				"persisted confidence should be %v for %s", tc.wantConf, tc.collection)
		})
	}
}

// TestDatabaseManager_AddLesson_PersistsInitialConfidence is the production-path
// counterpart to TestAddLesson_SetsInitialConfidence above. Routes through the
// real DatabaseManager.AddLesson and reads the confidence column back.
func TestDatabaseManager_AddLesson_PersistsInitialConfidence(t *testing.T) {
	dm := newTestDM(t)
	l, err := dm.AddLesson("lesson content for prod-path test", LessonTypeInsight, nil, "")
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.InDelta(t, 0.7, l.Confidence, 1e-9)

	var got float64
	err = dm.QueryRowTracked(
		`SELECT confidence FROM lessons WHERE id = ?`, l.ID,
	).Scan(&got)
	require.NoError(t, err)
	assert.InDelta(t, 0.7, got, 1e-9)
}
