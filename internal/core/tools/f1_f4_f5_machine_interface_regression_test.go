// f1_f4_f5_machine_interface_regression_test.go — regressions for audit
// findings F1 (handoff bridge drops open_questions / dead mpm_session),
// F4 (save echoes unbounded payload), and F5 (query returns full payloads
// inline).
package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func bigPayload(n int) string {
	return strings.Repeat("x", n)
}

// TestF4_SaveEchoBounded: a very large save must NOT echo the full content
// back; the echo is bounded, explicitly flagged, and carries a pointer.
// Persistence must still hold the FULL payload.
func TestF4_SaveEchoBounded(t *testing.T) {
	dm := newTestIsolatedDM(t)

	huge := bigPayload(30 * 1024) // the audited ~30 KB payload
	result, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": huge,
			"tags":    []interface{}{"f4"},
		},
	})
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	res := result.(map[string]interface{})

	echoed, _ := res["content"].(string)
	if len(echoed) >= 30*1024 {
		t.Fatalf("F4 REGRESSION: save echoed %d bytes of content; wire echo must be bounded", len(echoed))
	}
	if truncated, _ := res["content_truncated"].(bool); !truncated {
		t.Errorf("bounded echo must be explicitly flagged content_truncated=true")
	}
	if n := asInt(res["content_bytes"]); n != 30*1024 {
		t.Errorf("content_bytes = %v, want 30720 (full stored size)", res["content_bytes"])
	}
	if p, _ := res["pointer"].(string); !strings.HasPrefix(p, "mpm://memory/") {
		t.Errorf("response must carry a retrieval pointer, got %v", res["pointer"])
	}

	// Full payload persisted — bounding is presentation-only.
	id, _ := res["id"].(string)
	var stored string
	if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if len(stored) != 30*1024 {
		t.Fatalf("stored content truncated to %d bytes — data loss", len(stored))
	}

	// Small payloads are NOT flagged truncated.
	small, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{"fact": "tiny"},
	})
	if err != nil {
		t.Fatalf("small save: %v", err)
	}
	smallRes := small.(map[string]interface{})
	if _, flagged := smallRes["content_truncated"]; flagged {
		t.Errorf("small payload must not carry truncation flag")
	}
}

// TestF5_QueryResultsBoundedWithExplicitOptOut: query hits are bounded per
// item with flags + pointers; full_content=true restores unbounded output
// deliberately.
func TestF5_QueryResultsBoundedWithExplicitOptOut(t *testing.T) {
	dm := newTestIsolatedDM(t)

	for i := 0; i < 3; i++ {
		if _, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"action": "save",
			"params": map[string]interface{}{"fact": "f5marker " + bigPayload(12 * 1024)},
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	for _, mode := range []string{"default", "projected"} {
		payload := map[string]interface{}{
			"action": "query",
			"params": map[string]interface{}{"query": "f5marker", "limit": float64(3)},
		}
		if mode == "projected" {
			payload["params"].(map[string]interface{})["projection"] = true
		}
		result, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, payload)
		if err != nil {
			t.Fatalf("%s query: %v", mode, err)
		}
		raw := mustMarshalJSON(result)
		if len(raw) > 40*1024 {
			t.Errorf("%s mode: response %d bytes exceeds sane bound", mode, len(raw))
		}
	}

	// Explicit opt-out returns full content deliberately.
	result, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{"query": "f5marker", "full_content": true},
	})
	if err != nil {
		t.Fatalf("opt-out query: %v", err)
	}
	raw := mustMarshalJSON(result)
	if !strings.Contains(raw, strings.Repeat("x", 5000)) {
		t.Errorf("full_content=true must return unbounded content")
	}
}

func asInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return -1
}

func mustMarshalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestF13_HandoffWriteRoundTripsOpenQuestions drives the write surface the
// aliased bridge lands on: open_questions/commitments must persist and be
// returned, not silently dropped (F13).
func TestF13_HandoffWriteRoundTripsOpenQuestions(t *testing.T) {
	dm := newTestIsolatedDM(t)

	for _, tc := range [][]string{
		nil,                                  // zero questions
		{"single question"},                  // one question
		{"why did A fail?", "who owns B?", "specials: \"quotes\", 'apostrophes', ünïcödé"},
	} {
		res, err := handleMpmHandoff(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"action": "write",
			"params": map[string]interface{}{
				"session_id":     "sess-f13-roundtrip",
				"summary":        "round trip probe",
				"open_questions": toStringSlice(tc),
			},
		})
		if err != nil {
			t.Fatalf("write failed: %v", err)
		}
		raw := mustMarshalJSON(res)
		var wire struct {
			Handoff struct {
				OpenQuestions []string `json:"open_questions"`
			} `json:"handoff"`
		}
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(wire.Handoff.OpenQuestions) != len(tc) {
			t.Fatalf("open_questions count = %d, want %d (raw=%s)", len(wire.Handoff.OpenQuestions), len(tc), raw)
		}
		for i := range tc {
			if wire.Handoff.OpenQuestions[i] != tc[i] {
				t.Errorf("open_questions[%d] = %q, want %q", i, wire.Handoff.OpenQuestions[i], tc[i])
			}
		}

		// Read path returns them too.
		readRes, err := handleMpmHandoff(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"action": "read",
			"params": map[string]interface{}{},
		})
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		rawRead := mustMarshalJSON(readRes)
		if len(tc) > 0 && !strings.Contains(rawRead, tc[0]) {
			t.Errorf("read path dropped open_questions: %s", rawRead)
		}
	}
}

func toStringSlice(in []string) interface{} {
	if in == nil {
		return nil
	}
	out := make([]interface{}, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
