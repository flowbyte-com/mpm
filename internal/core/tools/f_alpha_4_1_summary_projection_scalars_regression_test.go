package tools

// Alpha-4.1 F-001 regression: summary projection must report truthful
// weight, created_at, and reinforcement_count. The pre-fix handlers
// asserted strict types against values produced by HybridSearch
// (int64 for created_at, float64 for weight, int for reinforcement_count)
// so every projected value silently became zero.
//
// These tests pin the truth of the agent-visible scalars through the
// full mpm_memory query path (the agent's only read surface).

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// seedMemoryWithScalars writes a memory with explicit non-default
// weight, created_at, and reinforcement_count. The handler must surface
// those values verbatim in the summary projection; if any of them is
// coerced to 0, the assertion fails.
func seedMemoryWithScalars(t *testing.T, dm mpminternal.CoreDB, tag string) (id string, wantWeight float64, wantCreatedAt int64, wantReinf int) {
	t.Helper()
	now := time.Now().Unix()
	// Pick deliberately distinctive values so a silent-zero regression
	// is obvious in a diff:
	//   weight: 7.5 (decimal — exercises float64 path)
	//   created_at: now-30d (exercises int64 path)
	//   reinforcement_count: 3 (exercises int path)
	wantCreatedAt = now - 30*86400
	wantReinf = 3
	wantWeight = 7.5

	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight, reinforcement_count)
		VALUES (?, 'memory', ?, '[]', '{}', ?, ?, ?)`,
		"alpha41-f001-"+tag, "alpha-4.1 scalar truth "+tag+" f1weight-f1date-f1reinf",
		wantCreatedAt, wantWeight, wantReinf,
	)
	if err != nil {
		t.Fatalf("seed scalars: %v", err)
	}
	return "alpha41-f001-" + tag, wantWeight, wantCreatedAt, wantReinf
}

func TestF001_SummaryProjection_ReportsTruthfulWeight(t *testing.T) {
	dm := newTestIsolatedDM(t)
	id, wantWeight, _, _ := seedMemoryWithScalars(t, dm, "weight")
	_ = id

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "alpha-4.1 scalar truth weight",
			"limit":      1,
			"projection": "summary",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Memories) == 0 {
		t.Fatalf("expected 1 memory, got 0; raw=%s", raw)
	}
	w, ok := wire.Memories[0]["weight"]
	if !ok {
		t.Fatalf("summary projection must carry weight field; raw=%s", raw)
	}
	// JSON unmarshal decodes numbers as float64 — the projection may
	// legitimately have stored an int or float64, both must surface
	// as a real number, never 0.
	var got float64
	switch v := w.(type) {
	case float64:
		got = v
	case int:
		got = float64(v)
	case int64:
		got = float64(v)
	default:
		t.Fatalf("weight type %T not number-like; raw=%s", w, raw)
	}
	if got != wantWeight {
		t.Errorf("projection weight=%v, want %v (truthful value must reach agent); raw=%s", got, wantWeight, raw)
	}
}

func TestF001_SummaryProjection_ReportsTruthfulCreatedAt(t *testing.T) {
	dm := newTestIsolatedDM(t)
	_, _, wantCreatedAt, _ := seedMemoryWithScalars(t, dm, "date")

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "alpha-4.1 scalar truth date",
			"limit":      1,
			"projection": "summary",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Memories) == 0 {
		t.Fatalf("expected 1 memory; raw=%s", raw)
	}
	c, ok := wire.Memories[0]["created_at"]
	if !ok {
		t.Fatalf("summary projection must carry created_at; raw=%s", raw)
	}
	var got int64
	switch v := c.(type) {
	case float64:
		got = int64(v)
	case int:
		got = int64(v)
	case int64:
		got = v
	default:
		t.Fatalf("created_at type %T not number-like; raw=%s", c, raw)
	}
	if got != wantCreatedAt {
		t.Errorf("projection created_at=%d, want %d (truthful timestamp must reach agent); raw=%s", got, wantCreatedAt, raw)
	}
}

func TestF001_SummaryProjection_ReportsTruthfulReinforcementCount(t *testing.T) {
	dm := newTestIsolatedDM(t)
	_, _, _, wantReinf := seedMemoryWithScalars(t, dm, "reinf")

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "alpha-4.1 scalar truth reinf",
			"limit":      1,
			"projection": "summary",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Memories) == 0 {
		t.Fatalf("expected 1 memory; raw=%s", raw)
	}
	r, ok := wire.Memories[0]["reinforcement_count"]
	if !ok {
		t.Fatalf("summary projection must carry reinforcement_count; raw=%s", raw)
	}
	var got int
	switch v := r.(type) {
	case float64:
		got = int(v)
	case int:
		got = v
	case int64:
		got = int(v)
	default:
		t.Fatalf("reinforcement_count type %T not number-like; raw=%s", r, raw)
	}
	if got != wantReinf {
		t.Errorf("projection reinforcement_count=%d, want %d; raw=%s", got, wantReinf, raw)
	}
}

// TestF001_SummaryProjection_DefaultWeightIsZeroNotLost exercises the
// boundary where weight was 0 at write time: the projection must still
// carry the field (so the agent can see "weight=0") rather than dropping
// or silently coercing it. This pins the field's presence contract.
func TestF001_SummaryProjection_DefaultWeightIsZeroNotLost(t *testing.T) {
	dm := newTestIsolatedDM(t)
	tag := "zeroweight"
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight, reinforcement_count)
		VALUES (?, 'memory', ?, '[]', '{}', ?, 0, 0)`,
		"alpha41-f001-"+tag, "alpha-4.1 zero-weight pin "+tag, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed zero weight: %v", err)
	}

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "alpha-4.1 zero-weight pin zeroweight",
			"limit":      1,
			"projection": "summary",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Memories) == 0 {
		t.Fatalf("expected 1 memory; raw=%s", raw)
	}
	w, ok := wire.Memories[0]["weight"]
	if !ok {
		t.Fatalf("summary projection must carry weight field even when 0; raw=%s", raw)
	}
	// JSON decodes as float64 for 0; both 0 (int) and 0.0 (float) are
	// accepted as truthful expressions of "no weight".
	switch v := w.(type) {
	case float64:
		if v != 0 {
			t.Errorf("weight field present but non-zero (got %v); raw=%s", v, raw)
		}
	case int:
		if v != 0 {
			t.Errorf("weight field present but non-zero (got %v); raw=%s", v, raw)
		}
	case int64:
		if v != 0 {
			t.Errorf("weight field present but non-zero (got %v); raw=%s", v, raw)
		}
	default:
		t.Fatalf("weight type %T not number-like; raw=%s", w, raw)
	}
}

// TestF001_SummaryProjection_LargeIDsAndTimestampsPresent exercises the
// "large IDs/timestamps" branch from the audit spec. The handler must
// not silently drop fields just because their numeric value is large.
func TestF001_SummaryProjection_LargeIDsAndTimestampsPresent(t *testing.T) {
	dm := newTestIsolatedDM(t)
	tag := "large"
	// Use a deliberately large timestamp (year 2099) so any float64
	// rounding loss would be visible.
	wantCreatedAt := int64(4089667200) // 2099-07-04
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight, reinforcement_count)
		VALUES (?, 'memory', ?, '[]', '{}', ?, 11, 4)`,
		"alpha41-f001-"+tag, "alpha-4.1 large-timestamp pin "+tag, wantCreatedAt)
	if err != nil {
		t.Fatalf("seed large: %v", err)
	}

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "alpha-4.1 large-timestamp pin large",
			"limit":      1,
			"projection": "summary",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Memories) == 0 {
		t.Fatalf("expected 1 memory; raw=%s", raw)
	}
	c, ok := wire.Memories[0]["created_at"]
	if !ok {
		t.Fatalf("summary projection must carry created_at; raw=%s", raw)
	}
	var got int64
	switch v := c.(type) {
	case float64:
		got = int64(v)
	case int:
		got = int64(v)
	case int64:
		got = v
	default:
		t.Fatalf("created_at type %T not number-like; raw=%s", c, raw)
	}
	if got != wantCreatedAt {
		t.Errorf("projection created_at=%d, want %d (large-timestamp must round-trip); raw=%s", got, wantCreatedAt, raw)
	}
}

// TestF001_SummaryProjection_SummaryIncludesReasoningMetadata sanity
// check that the projection also surfaces the rationale/score fields
// (the audit's repro showed those were dropped alongside the scalars).
// If this fails, the bug class is broader than the audit flagged.
func TestF001_SummaryProjection_SummaryIncludesReasoningMetadata(t *testing.T) {
	dm := newTestIsolatedDM(t)
	tag := "rationale"
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight, reinforcement_count)
		VALUES (?, 'memory', ?, '[]', '{}', ?, 5, 2)`,
		"alpha41-f001-"+tag, "alpha-4.1 rationale pin "+tag, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "alpha-4.1 rationale pin rationale",
			"limit":      1,
			"projection": "summary",
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	raw := mustMarshalJSON(res)
	var wire struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Memories) == 0 {
		t.Fatalf("expected 1 memory; raw=%s", raw)
	}
	rationale, _ := wire.Memories[0]["rationale"].(string)
	if rationale == "" || rationale == "memory" {
		t.Errorf("rationale should reflect collection/weight/reinforcement metadata, got %q; raw=%s", rationale, raw)
	}
	if !strings.Contains(rationale, "weight 5") {
		t.Errorf("rationale should mention weight=5, got %q; raw=%s", rationale, raw)
	}
	if !strings.Contains(rationale, "2x ref") {
		t.Errorf("rationale should mention reinforcement count, got %q; raw=%s", rationale, raw)
	}
	// score must be present and non-zero
	score, ok := wire.Memories[0]["score"]
	if !ok {
		t.Fatalf("score field must be present in summary projection; raw=%s", raw)
	}
	switch v := score.(type) {
	case float64:
		if v == 0 {
			t.Errorf("score must be non-zero when weight/reinf are non-zero, got 0; raw=%s", raw)
		}
	case int:
		if v == 0 {
			t.Errorf("score must be non-zero when weight/reinf are non-zero, got 0; raw=%s", raw)
		}
	default:
		t.Errorf("score type %T not number-like; raw=%s", score, raw)
	}
	_ = fmt.Sprintf("ok")
}
