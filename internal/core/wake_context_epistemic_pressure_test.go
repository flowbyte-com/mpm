// wake_context_epistemic_pressure_test.go — pins the epistemic_pressure contract.
//
// Four invariants:
//
//  1. View definition: epistemic_pressure_v exists, returns raw_count and
//     lesson_count columns, both int.
//  2. Raw definition: a memory in the 'memories' collection with no
//     metadata.compacted_into key counts toward raw_count. A memory
//     with the key set does NOT count. A deleted memory does NOT count.
//  3. Cold-start: with LessonCount == 0, Ratio is 0.0 (not NaN, not +Inf)
//     and Exceeded is false unless raw_count > threshold.
//  4. Threshold override: setting system_config.compaction.raw_threshold
//     changes the Exceeded signal without code changes.
//
// All four are independent so a regression in one surfaces clearly.

package internal

import (
	"math"
	"testing"
	"time"
)

// handleReadWakeContext is the package-internal handler used by
// mcp__read_wake_context. Defined in internal/core/tools/handlers.go.
// Imported here via the same package, so no alias needed.

// TestEpistemicPressure_ViewExists pins the schema-level invariant
// that the view is present and queryable. A dropped view would cascade
// into NaN/0 in every wake_context response — caught here before it
// hits the wire.
func TestEpistemicPressure_ViewExists(t *testing.T) {
	dm := NewTestDM(t)

	var rawCount, lessonCount int
	err := dm.SQLDB().QueryRow(`SELECT raw_count, lesson_count FROM epistemic_pressure_v`).Scan(&rawCount, &lessonCount)
	if err != nil {
		t.Fatalf("epistemic_pressure_v missing or unqueryable: %v", err)
	}
	// Fresh test DB: zero raw memories, zero lessons.
	if rawCount != 0 || lessonCount != 0 {
		t.Errorf("fresh DB: got raw=%d lesson=%d, want 0/0", rawCount, lessonCount)
	}
}

// TestEpistemicPressure_RawCountDefinition seeds three memories: one
// raw (no metadata), one with compacted_into set, one soft-deleted.
// Only the first should count. This is the load-bearing invariant —
// if the SQL filter regresses, compaction would shred provenance.
func TestEpistemicPressure_RawCountDefinition(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)

	now := time.Now().UTC().Format(time.RFC3339)
	mustInsert := func(id, metadataJSON string, deleted bool) {
		var deletedAt interface{}
		if deleted {
			deletedAt = time.Now().Unix()
		} else {
			deletedAt = nil
		}
		var meta interface{}
		if metadataJSON != "" {
			meta = metadataJSON
		} else {
			meta = nil
		}
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, created_at, updated_at, deleted_at)
			VALUES (?, 'memories', 'seed content', ?, ?, ?, ?)
		`, id, meta, now, now, deletedAt); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mustInsert("raw-no-meta", "", false)
	mustInsert("raw-empty-meta", `{}`, false)
	mustInsert("raw-compacted", `{"compacted_into":"lesson-xyz"}`, false)
	mustInsert("raw-deleted", `{}`, true)
	mustInsert("wrong-collection", `{}`, false)

	// Fix wrong-collection: it's in 'memories' too — set collection explicitly.
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET collection='notes' WHERE id='wrong-collection'`); err != nil {
		t.Fatalf("update collection: %v", err)
	}

	got := dm.gatherEpistemicPressure()
	if got.RawCount != 2 {
		t.Errorf("raw_count: got %d, want 2 (raw-no-meta + raw-empty-meta; compacted/deleted/wrong-collection excluded)", got.RawCount)
	}
}

// TestEpistemicPressure_ColdStart verifies the divide-by-zero guard.
// A fresh agent with 0 lessons and 0 raw memories must produce a
// valid (non-NaN, non-Inf) Ratio and Exceeded=false.
func TestEpistemicPressure_ColdStart(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)

	got := dm.gatherEpistemicPressure()
	if got.LessonCount != 0 {
		t.Fatalf("precondition: lessons should be 0 in fresh test DB, got %d", got.LessonCount)
	}
	if got.Ratio != 0.0 {
		t.Errorf("cold-start Ratio: got %v, want 0.0 (divide-by-zero guard)", got.Ratio)
	}
	if math.IsNaN(got.Ratio) || math.IsInf(got.Ratio, 0) {
		t.Errorf("cold-start Ratio is not finite: %v", got.Ratio)
	}
	if got.Exceeded {
		t.Error("cold-start Exceeded should be false")
	}
	if got.Threshold != 100 {
		t.Errorf("default threshold: got %d, want 100", got.Threshold)
	}
}

// TestEpistemicPressure_ThresholdOverride verifies the system_config
// path. Inserting raw_json={"raw_threshold":50} at key="compaction"
// drops the threshold from 100 to 50 without code changes.
func TestEpistemicPressure_ThresholdOverride(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)
	_, _ = dm.SQLDB().Exec(`DELETE FROM system_config WHERE key='compaction'`)

	// Seed 60 raw memories — exceeds default 100? No. Exceeds override 50? Yes.
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 60; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at, deleted_at)
			VALUES (?, 'memories', 'seed', ?, ?, NULL)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Without override: Exceeded should be false (60 < 100).
	got := dm.gatherEpistemicPressure()
	if got.Threshold != 100 {
		t.Errorf("default threshold: got %d, want 100", got.Threshold)
	}
	if got.Exceeded {
		t.Error("60 raw memories should not exceed default threshold 100")
	}

	// Apply override via system_config.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction', '{"raw_threshold":50}', '')
	`); err != nil {
		t.Fatalf("insert system_config: %v", err)
	}

	got = dm.gatherEpistemicPressure()
	if got.Threshold != 50 {
		t.Errorf("override threshold: got %d, want 50", got.Threshold)
	}
	if !got.Exceeded {
		t.Error("60 raw memories should exceed overridden threshold 50")
	}
}

// TestEpistemicPressure_LastCompactedAtEmpty verifies the field is
// empty string when no compaction has happened. Agents branch on
// non-empty; an absent (NULL) row in system_config must surface
// as "" rather than "null" or a missing field.
func TestEpistemicPressure_LastCompactedAtEmpty(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM system_config WHERE key='compaction.last_run'`)

	got := dm.gatherEpistemicPressure()
	if got.LastCompactedAt != "" {
		t.Errorf("LastCompactedAt: got %q, want empty string", got.LastCompactedAt)
	}
}

// TestEpistemicPressure_LastCompactedAtSet verifies the field surfaces
// the RFC3339 timestamp from system_config.compaction.last_run when
// present. Independent of the rest of the pressure surface — tests
// that the wake_context gather reads both the gauge and the ledger
// without coupling.
func TestEpistemicPressure_LastCompactedAtSet(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM system_config WHERE key='compaction.last_run'`)

	timestamp := "2026-07-24T19:31:00Z"
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction.last_run', ?, '')
	`, `{"last_run_at":"`+timestamp+`","lesson_id":"les-abc","raw_marked":50}`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got := dm.gatherEpistemicPressure()
	if got.LastCompactedAt != timestamp {
		t.Errorf("LastCompactedAt: got %q, want %q", got.LastCompactedAt, timestamp)
	}
}

// TestEpistemicPressure_RatioWithLessons verifies the secondary
// signal (raw:lesson density) computes correctly when both counts
// are non-zero.
func TestEpistemicPressure_RatioWithLessons(t *testing.T) {
	dm := NewTestDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM memories`)
	_, _ = dm.SQLDB().Exec(`DELETE FROM lessons_base`)

	// Seed 30 raw memories.
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 30; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at, deleted_at)
			VALUES (?, 'memories', 'seed', ?, ?, NULL)
		`, "raw-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now, now); err != nil {
			t.Fatalf("seed raw %d: %v", i, err)
		}
	}
	// Seed 5 lessons.
	for i := 0; i < 5; i++ {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO lessons_base (id, type, content, created)
			VALUES (?, 'insight', 'lesson content', ?)
		`, "les-"+time.Now().Format("150405.000000")+"-"+string(rune('a'+i%26)), now); err != nil {
			t.Fatalf("seed lesson %d: %v", i, err)
		}
	}

	got := dm.gatherEpistemicPressure()
	if got.RawCount != 30 {
		t.Errorf("raw_count: got %d, want 30", got.RawCount)
	}
	if got.LessonCount != 5 {
		t.Errorf("lesson_count: got %d, want 5", got.LessonCount)
	}
	wantRatio := 6.0 // 30/5
	if got.Ratio != wantRatio {
		t.Errorf("ratio: got %v, want %v (30 raw / 5 lessons)", got.Ratio, wantRatio)
	}
}

// TestHandleReadWakeContext_IncludesEpistemicPressure lives in the
// tools package (the handler is defined there, not in internal/core).
// The wire-format invariant is pinned at the handler test site — see
// internal/core/tools/handlers_test.go's TestHandleReadWakeContext_IncludesEpistemicPressure.
//
// What this file DOES pin (core layer):
//   - The VIEW definition and its column types
//   - The cold-start divide-by-zero guard
//   - The system_config threshold override path
//   - The raw-count filter semantics (compacted_into exclusion)