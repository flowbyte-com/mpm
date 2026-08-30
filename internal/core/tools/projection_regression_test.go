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

// ────────────────────────────────────────────────────────────────────
// Alpha-4 D-002/W-003 — accept "mode" as a deprecated alias of
// "projection", map mode="content" → "full", reject mode="verbose"
// and any other unknown value with a canonical-list error.
// ────────────────────────────────────────────────────────────────────

// TestProjection_Normalize_HelperMatrix pins normalizeProjection's
// resolution rules without going through the full query path. The
// matrix covers every branch documented in the helper's contract:
// default, both-set-equal, both-set-differ, mode=content, mode=verbose,
// unknown on either key.
func TestProjection_Normalize_HelperMatrix(t *testing.T) {
	cases := []struct {
		name        string
		params      map[string]interface{}
		want        string
		wantErrFrag string // empty = no error; otherwise substring that must appear
	}{
		{"default (no keys set)", map[string]interface{}{}, "summary", ""},
		{"projection=summary explicit", map[string]interface{}{"projection": "summary"}, "summary", ""},
		{"projection=full explicit", map[string]interface{}{"projection": "full"}, "full", ""},
		{"mode=summary alone", map[string]interface{}{"mode": "summary"}, "summary", ""},
		{"mode=full alone", map[string]interface{}{"mode": "full"}, "full", ""},
		{"mode=content maps to full", map[string]interface{}{"mode": "content"}, "full", ""},
		{"both same value (summary)", map[string]interface{}{"projection": "summary", "mode": "summary"}, "summary", ""},
		{"both same value (full)", map[string]interface{}{"projection": "full", "mode": "full"}, "full", ""},
		{"both differ — projection wins (summary)", map[string]interface{}{"projection": "summary", "mode": "content"}, "summary", ""},
		{"both differ — projection wins (full)", map[string]interface{}{"projection": "full", "mode": "summary"}, "full", ""},
		{"mode=verbose rejected", map[string]interface{}{"mode": "verbose"}, "", "canonical values: [summary, full]"},
		{"projection=verbose rejected", map[string]interface{}{"projection": "verbose"}, "", "canonical values: [summary, full]"},
		{"projection=rich rejected", map[string]interface{}{"projection": "rich"}, "", "canonical values: [summary, full]"},
		{"mode=rich rejected", map[string]interface{}{"mode": "rich"}, "", "canonical values: [summary, full]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeProjection(c.params)
			if c.wantErrFrag != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (result=%q)", c.wantErrFrag, got)
				}
				if !strings.Contains(err.Error(), c.wantErrFrag) {
					t.Fatalf("error %q does not contain %q", err.Error(), c.wantErrFrag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestProjection_MemoryQuery_ModeContentMapsToFull is the public-boundary
// pin for D-002/W-003: an agent that sends `mode:"content"` (the legacy
// shape) gets full content without making a second mpm_resolve call.
func TestProjection_MemoryQuery_ModeContentMapsToFull(t *testing.T) {
	dm := newTestIsolatedDM(t)
	seedBigMemory(t, dm, strings.Repeat("k", 4096), "d002-mode-content")

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "d002-mode-content",
			"limit": float64(1),
			"mode":  "content",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	if !strings.Contains(raw, strings.Repeat("k", 3000)) {
		t.Errorf(`mode:"content" must return unbounded content; payload=%s`, raw)
	}
	var wire struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal([]byte(raw), &wire)
	if wire.Mode != "full" {
		t.Errorf("wire mode = %q, want %q (mode:content should map to full)", wire.Mode, "full")
	}
}

// TestProjection_MemoryQuery_ModeVerboseRejected is the public-boundary
// pin for the "verbose is not a projection knob" rule.
func TestProjection_MemoryQuery_ModeVerboseRejected(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "anything",
			"limit": float64(1),
			"mode":  "verbose",
		},
	})
	if err == nil {
		t.Fatalf("expected error for mode:verbose, got nil")
	}
	if !strings.Contains(err.Error(), "canonical values: [summary, full]") {
		t.Errorf("error %q does not list canonical values", err.Error())
	}
}

// TestProjection_MemoryQuery_BothSet_ProjectionWins covers the case
// where both keys are sent with different values: the canonical
// "projection" wins, "mode" is silently ignored (no error — the
// caller expressed clear intent on the canonical key).
func TestProjection_MemoryQuery_BothSet_ProjectionWins(t *testing.T) {
	dm := newTestIsolatedDM(t)
	seedBigMemory(t, dm, strings.Repeat("p", 4096), "d002-both-set")

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "d002-both-set",
			"limit":      float64(1),
			"projection": "summary",
			"mode":       "content", // would map to full if it won
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal([]byte(raw), &wire)
	if wire.Mode != "summary" {
		t.Errorf("wire mode = %q, want %q (projection canonical, mode ignored)", wire.Mode, "summary")
	}
	if strings.Contains(raw, strings.Repeat("p", 1000)) {
		t.Errorf("projection=summary must NOT inline content; payload=%s", raw)
	}
}

// TestProjection_MemoryQuery_UnknownValueRejected covers a typo
// (`projection: "rich"`) — the response must surface a parseable
// error naming the canonical values, not silently coerce.
func TestProjection_MemoryQuery_UnknownValueRejected(t *testing.T) {
	dm := newTestIsolatedDM(t)

	_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "anything",
			"limit":      float64(1),
			"projection": "rich",
		},
	})
	if err == nil {
		t.Fatalf("expected error for projection:rich, got nil")
	}
	if !strings.Contains(err.Error(), `unknown projection "rich"`) {
		t.Errorf("error %q does not name the rejected value", err.Error())
	}
	if !strings.Contains(err.Error(), "canonical values: [summary, full]") {
		t.Errorf("error %q does not list canonical values", err.Error())
	}
}
