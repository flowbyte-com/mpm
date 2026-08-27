package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

const truncationSuffix = "... [truncated, resolve pointer for full text]"

// seedBigMemory saves one oversized memory for projection assertions.
func seedBigMemory(t *testing.T, dm mpminternal.CoreDB, fact, tag string) string {
	t.Helper()
	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": fact,
			"tags": []interface{}{tag},
		},
	})
	if err != nil {
		t.Fatalf("seed save: %v", err)
	}
	r := res.(map[string]interface{})
	id, _ := r["id"].(string)
	return id
}

func TestProjection_MemoryQuery_DefaultsToSummary(t *testing.T) {
	dm := newTestIsolatedDM(t)
	seedBigMemory(t, dm, strings.Repeat("y", 4096), "proj-default")

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action":  "query",
		"params": map[string]interface{}{"query": "proj-default", "limit": float64(1)},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	raw := mustMarshalJSON(res)
	var wire struct {
		Mode     string                   `json:"mode"`
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire.Mode != "summary" {
		t.Errorf("default mode = %q, want \"summary\"", wire.Mode)
	}
	if len(wire.Memories) != 1 {
		t.Fatalf("want 1 memory, got %d", len(wire.Memories))
	}
	if _, has := wire.Memories[0]["content"]; has {
		t.Errorf("summary mode must NOT inline content field")
	}
	summary, _ := wire.Memories[0]["summary"].(string)
	if !strings.Contains(summary, truncationSuffix) {
		t.Errorf("summary %q must end with %q", summary, truncationSuffix)
	}
	ptr, _ := wire.Memories[0]["pointer"].(string)
	if !strings.HasPrefix(ptr, "mpm://memory/") {
		t.Errorf("summary mode must carry pointer, got %v", ptr)
	}
}

func TestProjection_MemoryQuery_FullReturnsUnbounded(t *testing.T) {
	dm := newTestIsolatedDM(t)
	seedBigMemory(t, dm, strings.Repeat("z", 4096), "proj-full")

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "proj-full",
			"limit":      float64(1),
			"projection": "full",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	if !strings.Contains(raw, strings.Repeat("z", 3000)) {
		t.Errorf("projection=full must return unbounded content; payload=%s", raw)
	}
	var wire struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal([]byte(raw), &wire)
	if wire.Mode != "full" {
		t.Errorf("mode = %q, want \"full\"", wire.Mode)
	}
}

func TestProjection_MemoryQuery_ExplicitSummaryMatchesDefault(t *testing.T) {
	dm := newTestIsolatedDM(t)
	seedBigMemory(t, dm, strings.Repeat("w", 4096), "proj-explicit")

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "proj-explicit",
			"limit":      float64(1),
			"projection": "summary",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	if strings.Contains(raw, strings.Repeat("w", 1000)) {
		t.Errorf("projection=summary must NOT inline unbounded content; payload=%s", raw)
	}
	if !strings.Contains(raw, truncationSuffix) {
		t.Errorf("projection=summary must carry truncation suffix; payload=%s", raw)
	}
}

func TestProjection_LessonSearch_DefaultsToSummary(t *testing.T) {
	dm := newTestIsolatedDM(t)
	// Seed a lesson via the lessons tool.
	big := strings.Repeat("L", 4096)
	if _, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":  big,
			"type":  "practice",
			"tags":  []interface{}{"proj-lesson"},
		},
	}); err != nil {
		t.Fatalf("seed lesson: %v", err)
	}

	res, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "search",
		"params": map[string]interface{}{"query": "proj-lesson"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Mode    string                   `json:"mode"`
		Lessons []map[string]interface{} `json:"lessons"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire.Mode != "summary" {
		t.Errorf("mode = %q, want \"summary\"", wire.Mode)
	}
	if len(wire.Lessons) == 0 {
		t.Fatalf("want ≥1 lesson, got 0")
	}
	first := wire.Lessons[0]
	if _, has := first["content"]; has {
		t.Errorf("summary mode must NOT inline content field")
	}
	summary, _ := first["summary"].(string)
	if !strings.Contains(summary, truncationSuffix) {
		t.Errorf("summary %q must end with %q", summary, truncationSuffix)
	}
	ptr, _ := first["pointer"].(string)
	if !strings.HasPrefix(ptr, "mpm://lesson/") {
		t.Errorf("summary mode must carry mpm://lesson/ pointer, got %v", ptr)
	}
}

func TestProjection_LessonList_FullReturnsUnbounded(t *testing.T) {
	dm := newTestIsolatedDM(t)
	big := strings.Repeat("M", 4096)
	if _, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{"fact": big, "type": "insight"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{"projection": "full"},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	raw := mustMarshalJSON(res)
	if !strings.Contains(raw, strings.Repeat("M", 3000)) {
		t.Errorf("projection=full must return unbounded content; payload=%s", raw)
	}
}
