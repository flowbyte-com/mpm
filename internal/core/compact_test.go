// compact_test.go — Phase 2 of the epistemic compaction pipeline.
//
// Eight invariants pinned:
//
//  1. Validate() — semantic checks beyond JSON shape (empty fields fail)
//  2. Pre-check (no raw) — SkippedReason="no_raw_memories", zero LLM calls
//  3. Pre-check (below threshold) — SkippedReason="below_threshold", zero LLM calls
//  4. Extract cap — exactly 50 raw memories extracted, never more
//  5. LLM failure — zero DB writes, error returned
//  6. Schema violation (bad JSON) — zero DB writes, model_schema_violation
//  7. Happy path — lesson committed, all raw memories marked, return shape correct
//  8. force=true bypasses threshold — proceeds even when raw_count <= threshold

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// withMockSynth replaces the LLM injection seam for the duration of
// the test, then restores it on cleanup. Each test gets its own
// canned response — no global state pollution across tests.
func withMockSynth(t *testing.T, fn func(ctx context.Context, raw []string) (string, error)) {
	t.Helper()
	prev := compactSynthesizeFunc
	compactSynthesizeFunc = fn
	t.Cleanup(func() { compactSynthesizeFunc = prev })
}

// ── 1. Validate ────────────────────────────────────────────────────────

func TestCompactLesson_Validate(t *testing.T) {
	cases := []struct {
		name    string
		lesson  CompactLesson
		wantErr bool
	}{
		{"all set", CompactLesson{Title: "T", Body: "B", Tags: []string{"x"}}, false},
		{"empty title", CompactLesson{Body: "B", Tags: []string{"x"}}, true},
		{"empty body", CompactLesson{Title: "T", Tags: []string{"x"}}, true},
		{"empty tags", CompactLesson{Title: "T", Body: "B"}, true},
		{"refusal sentinel", CompactLesson{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.lesson.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate: got nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate: got %v, want nil", err)
			}
		})
	}
}

// ── 2. Pre-check: no raw memories ─────────────────────────────────────

func TestCompactEpistemology_NoRawMemories(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)

	called := false
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		called = true
		return "", nil
	})

	res, err := dm.CompactEpistemology(context.Background(), false)
	if err != nil {
		t.Fatalf("CompactEpistemology: %v", err)
	}
	if res == nil {
		t.Fatal("result nil; expected no-op shape with SkippedReason")
	}
	if res.SkippedReason != "no_raw_memories" {
		t.Errorf("SkippedReason: got %q, want %q", res.SkippedReason, "no_raw_memories")
	}
	if called {
		t.Error("LLM was called despite no raw memories — pre-check failed")
	}
}

// ── 3. Pre-check: below threshold ─────────────────────────────────────

func TestCompactEpistemology_BelowThreshold(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)

	// Seed 50 raw memories — below default threshold of 100.
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 50; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'seed', ?, ?)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	called := false
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		called = true
		return "", nil
	})

	res, err := dm.CompactEpistemology(context.Background(), false)
	if err != nil {
		t.Fatalf("CompactEpistemology: %v", err)
	}
	if res.SkippedReason != "below_threshold" {
		t.Errorf("SkippedReason: got %q, want %q", res.SkippedReason, "below_threshold")
	}
	if called {
		t.Error("LLM was called despite below threshold — pre-check failed")
	}
}

// ── 4. Extract cap: exactly 50 ─────────────────────────────────────────

func TestCompactEpistemology_ExtractLimit(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)

	// Seed 120 raw memories — well above the cap of 50.
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 120; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'seed', ?, ?)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Drop the threshold so the LLM call fires.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction', '{"raw_threshold":1}', '')
	`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	var capturedBatch int
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		capturedBatch = len(raw)
		// Return invalid JSON so the LLM call records but no DB writes happen.
		return "this is not json", nil
	})

	_, err := dm.CompactEpistemology(context.Background(), false)
	if err == nil {
		t.Fatal("expected error from schema-violation path; got nil")
	}
	if capturedBatch != compactBatchSize {
		t.Errorf("LLM input batch: got %d, want %d", capturedBatch, compactBatchSize)
	}
}

// ── 5. LLM failure: zero DB writes ─────────────────────────────────────

func TestCompactEpistemology_LLMFailure(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)
	if _, err := dm.SQLDB().Exec(`DELETE FROM lessons_base`); err != nil {
		t.Fatalf("clear lessons: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction', '{"raw_threshold":1}', '')
	`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 5; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'seed', ?, ?)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return "", errors.New("synthesize: API request failed: connection refused")
	})

	_, err := dm.CompactEpistemology(context.Background(), false)
	if err == nil {
		t.Fatal("expected error from LLM failure; got nil")
	}
	if !strings.Contains(err.Error(), "synthesize") {
		t.Errorf("error: got %v, want contains 'synthesize'", err)
	}

	// Verify zero DB writes.
	var rawCount, lessonCount int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE json_extract(metadata,'$.compacted_into') IS NULL`).Scan(&rawCount)
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&lessonCount)
	if rawCount != 5 {
		t.Errorf("raw memories should be untouched: got %d marked, want 0", 5-rawCount)
	}
	if lessonCount != 0 {
		t.Errorf("lessons table should be empty: got %d, want 0", lessonCount)
	}
}

// ── 6. Schema violation: zero DB writes ──────────────────────────────

func TestCompactEpistemology_SchemaViolation(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)
	if _, err := dm.SQLDB().Exec(`DELETE FROM lessons_base`); err != nil {
		t.Fatalf("clear lessons: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction', '{"raw_threshold":1}', '')
	`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 5; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'seed', ?, ?)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return "this is not valid json {", nil
	})

	_, err := dm.CompactEpistemology(context.Background(), false)
	if err == nil {
		t.Fatal("expected error from schema violation; got nil")
	}
	if !strings.Contains(err.Error(), "model_schema_violation") {
		t.Errorf("error: got %v, want contains 'model_schema_violation'", err)
	}

	// Verify zero DB writes (the load-bearing invariant).
	var rawCount, lessonCount int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE json_extract(metadata,'$.compacted_into') IS NULL`).Scan(&rawCount)
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&lessonCount)
	if rawCount != 5 {
		t.Errorf("raw memories should be untouched: got %d marked, want 0", 5-rawCount)
	}
	if lessonCount != 0 {
		t.Errorf("lessons table should be empty after schema violation: got %d, want 0", lessonCount)
	}
}

// ── 6b. Validation failure: empty fields ──────────────────────────────

func TestCompactEpistemology_ValidationFailure(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)
	if _, err := dm.SQLDB().Exec(`DELETE FROM lessons_base`); err != nil {
		t.Fatalf("clear lessons: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction', '{"raw_threshold":1}', '')
	`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 5; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'seed', ?, ?)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Model returns the refusal sentinel — valid JSON, empty fields.
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return `{"title":"","body":"","tags":[]}`, nil
	})

	_, err := dm.CompactEpistemology(context.Background(), false)
	if err == nil {
		t.Fatal("expected validation error; got nil")
	}
	if !strings.Contains(err.Error(), "lesson_validation_failed") {
		t.Errorf("error: got %v, want contains 'lesson_validation_failed'", err)
	}

	// Zero DB writes.
	var lessonCount int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&lessonCount)
	if lessonCount != 0 {
		t.Errorf("lessons table should be empty after validation failure: got %d, want 0", lessonCount)
	}
}

// ── 7. Happy path ─────────────────────────────────────────────────────

func TestCompactEpistemology_HappyPath(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)
	if _, err := dm.SQLDB().Exec(`DELETE FROM lessons_base`); err != nil {
		t.Fatalf("clear lessons: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction', '{"raw_threshold":1}', '')
	`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	const seedCount = 10
	now := time.Now().UTC().Format(time.RFC3339)
	seededIDs := make([]string, 0, seedCount)
	for i := 0; i < seedCount; i++ {
		id := "raw-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+i%26))
		seededIDs = append(seededIDs, id)
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'seed content', ?, ?)
		`, id, now, now); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		// Return a valid lesson. Tags include a sentinel that lets the
		// test verify the body parses through correctly.
		lesson := CompactLesson{
			Title: "Synthesized Lesson",
			Body:  "The body of the lesson, captured from the batch.",
			Tags:  []string{"test-tag", "happy-path"},
		}
		b, _ := json.Marshal(lesson)
		return string(b), nil
	})

	res, err := dm.CompactEpistemology(context.Background(), false)
	if err != nil {
		t.Fatalf("CompactEpistemology: %v", err)
	}
	if res == nil {
		t.Fatal("result nil")
	}
	if res.Compacted != seedCount {
		t.Errorf("Compacted: got %d, want %d", res.Compacted, seedCount)
	}
	if res.LessonsCreated != 1 {
		t.Errorf("LessonsCreated: got %d, want 1", res.LessonsCreated)
	}
	if res.RawMarked != seedCount {
		t.Errorf("RawMarked: got %d, want %d", res.RawMarked, seedCount)
	}
	if res.LessonID == "" {
		t.Error("LessonID empty; expected les-* id")
	}
	if !strings.HasPrefix(res.LessonID, "les-") {
		t.Errorf("LessonID prefix: got %q, want les- prefix", res.LessonID)
	}

	// Verify all 10 raw memories have compacted_into = lesson_id.
	for _, id := range seededIDs {
		var compactedInto string
		err := dm.SQLDB().QueryRow(`
			SELECT json_extract(metadata, '$.compacted_into') FROM memories WHERE id = ?
		`, id).Scan(&compactedInto)
		if err != nil {
			t.Errorf("read %s: %v", id, err)
			continue
		}
		if compactedInto != res.LessonID {
			t.Errorf("memory %s: compacted_into=%q, want %q", id, compactedInto, res.LessonID)
		}
	}

	// Verify lesson table has exactly one row with the right content.
	var content string
	var contentHash string
	err = dm.SQLDB().QueryRow(`SELECT content, content_hash FROM lessons WHERE id = ?`, res.LessonID).Scan(&content, &contentHash)
	if err != nil {
		t.Fatalf("read lesson: %v", err)
	}
	// Content is the JSON envelope {title, body}. Verify the round-trip.
	var env struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		t.Fatalf("envelope parse: %v", err)
	}
	if env.Title != "Synthesized Lesson" {
		t.Errorf("envelope title: got %q, want %q", env.Title, "Synthesized Lesson")
	}
	if env.Body != "The body of the lesson, captured from the batch." {
		t.Errorf("envelope body: got %q, want %q", env.Body, "The body of the lesson, captured from the batch.")
	}
}

// ── 8. force=true bypasses threshold ───────────────────────────────────

func TestCompactEpistemology_ForceBypassesThreshold(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)
	if _, err := dm.SQLDB().Exec(`DELETE FROM lessons_base`); err != nil {
		t.Fatalf("clear lessons: %v", err)
	}

	// Seed 5 raw memories. Default threshold is 100 — below threshold.
	const seedCount = 5
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < seedCount; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'seed', ?, ?)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	called := false
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		called = true
		lesson := CompactLesson{
			Title: "Forced Compaction",
			Body:  "Body",
			Tags:  []string{"force"},
		}
		b, _ := json.Marshal(lesson)
		return string(b), nil
	})

	// Without force — should skip (below threshold).
	res, err := dm.CompactEpistemology(context.Background(), false)
	if err != nil {
		t.Fatalf("no-force: %v", err)
	}
	if res.SkippedReason != "below_threshold" {
		t.Errorf("no-force SkippedReason: got %q, want below_threshold", res.SkippedReason)
	}
	if called {
		t.Error("no-force LLM was called")
	}

	// With force — should proceed.
	res, err = dm.CompactEpistemology(context.Background(), true)
	if err != nil {
		t.Fatalf("force: %v", err)
	}
	if res.SkippedReason != "" {
		t.Errorf("force SkippedReason: got %q, want empty", res.SkippedReason)
	}
	if res.Compacted != seedCount {
		t.Errorf("force Compacted: got %d, want %d", res.Compacted, seedCount)
	}
	if !called {
		t.Error("force LLM was not called")
	}
}

// Note: handler-level test (compact_epistemology registration in
// tools.Registry) would require importing
// "github.com/flowbyte-com/mpm-core/tools", which creates an import
// cycle (tools/handlers.go imports internal/core). The handler wiring
// is verified by the build (the registry entry references
// handleCompactEpistemology — if either name drifts, build fails).
// The orchestrator behavior is the load-bearing surface; that's what
// the rest of this file pins.