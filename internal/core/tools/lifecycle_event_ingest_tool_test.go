// lifecycle_event_ingest_tool_test.go — the TOOL SURFACE of the
// mpm-event/0.1 ingest (P5).
//
// The core tests (internal/core/lifecycle_events_test.go) pin the storage
// and the validation. These pin the WIRE SHAPE: the exact bytes a caller
// reads.
//
// The one shape decision that matters most is pinned here: the receipt is
// a TOP-LEVEL SIBLING of "success", not nested under a "payload" key.
//
// WHY THAT MATTERS, precisely. The reference adapter spawns
// `mpm call mpm_system --payload …`, parses the LAST JSON line of stdout,
// and hands the WHOLE parsed object to its transport as `res.payload`. The
// transport then reads `res.payload.receipt`. So the receipt must sit
// beside `success` at the top level of the `mpm call` envelope.
//
// It is also the substrate's own convention: none of the handlers in
// handlers.go that return {"success": true, …} nest their result under a
// "payload" key. A nested receipt would be a malformed receipt.

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// lifecycleToolEnvelope builds a VALID mpm-event/0.1 envelope in the shape
// the reference adapter emits.
func lifecycleToolEnvelope() map[string]interface{} {
	return map[string]interface{}{
		"schema_version":       mpminternal.LifecycleEventSchemaVersion,
		"event_id":             "ev1_" + strings.Repeat("a", 64),
		"idempotency":          "strict",
		"event_type":           "session.observed",
		"observation":          "observed",
		"observed_at_ms":       int64(1793021872123),
		"host":                 "opencode",
		"host_version":         "2.1.4",
		"source":               "opencode-plugin",
		"adapter_version":      "abc1234",
		"project_id":           nil,
		"project_id_source":    "unresolved",
		"framework_name":       "opencode",
		"framework_session_id": "ses_abc",
		"operation":            "absent",
		"result_status":        "unknown",
		"trust":                "observation",
		"meta": map[string]interface{}{
			"host_event_type": "session.text.delta",
			"host_call_id":    "call_shared",
		},
		"payload": nil,
	}
}

// TestEventIngest_ReceiptIsATopLevelSibling pins the wire shape.
//
// The receipt must be reachable as envelope["receipt"], and the envelope
// must NOT carry a "payload" key holding it — that is the shape the
// reference adapter reads, and it is the substrate's convention.
func TestEventIngest_ReceiptIsATopLevelSibling(t *testing.T) {
	dm := newTestSharedDM(t)
	env := lifecycleToolEnvelope()

	res, err := handleMpmSystem(dm, defaultACForPatch(), map[string]interface{}{
		"action": "event_ingest",
		"params": map[string]interface{}{
			"schema_version": mpminternal.LifecycleEventSchemaVersion,
			"event":          env,
		},
	})
	if err != nil {
		t.Fatalf("event_ingest: %v", err)
	}
	env2, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("the result must be a map, got %T", res)
	}
	if env2["success"] != true {
		t.Errorf("the envelope must carry success: true, got %v", env2["success"])
	}
	if _, nested := env2["payload"]; nested {
		t.Error("the envelope must NOT nest the receipt under a \"payload\" key — " +
			"the reference adapter reads envelope[\"receipt\"]")
	}
	receipt, ok := env2["receipt"].(map[string]interface{})
	if !ok {
		t.Fatalf("the envelope must carry a top-level \"receipt\" object, got %v", env2["receipt"])
	}
	if receipt["event_id"] != env["event_id"] {
		t.Errorf("the receipt's event_id = %v, want %v", receipt["event_id"], env["event_id"])
	}
	if _, ok := receipt["ingested_at_ms"].(int64); !ok {
		t.Errorf("the receipt's ingested_at_ms must be an integer, got %T", receipt["ingested_at_ms"])
	}
	if _, ok := receipt["duplicate"].(bool); !ok {
		t.Errorf("the receipt's duplicate must be a boolean, got %T", receipt["duplicate"])
	}
	if receipt["duplicate"] != false {
		t.Error("the first ingest must not report duplicate: true")
	}
}

// TestEventIngest_DuplicateIsReported pins that a repeated strict event
// id is reported as a duplicate and does not create a second row.
func TestEventIngest_DuplicateIsReported(t *testing.T) {
	dm := newTestSharedDM(t)
	env := lifecycleToolEnvelope()

	call := func() map[string]interface{} {
		res, err := handleMpmSystem(dm, defaultACForPatch(), map[string]interface{}{
			"action": "event_ingest",
			"params": map[string]interface{}{
				"schema_version": mpminternal.LifecycleEventSchemaVersion,
				"event":          env,
			},
		})
		if err != nil {
			t.Fatalf("event_ingest: %v", err)
		}
		return res.(map[string]interface{})
	}

	first := call()["receipt"].(map[string]interface{})
	second := call()["receipt"].(map[string]interface{})

	if second["duplicate"] != true {
		t.Error("the second ingest must report duplicate: true")
	}
	if second["ingested_at_ms"] != first["ingested_at_ms"] {
		t.Errorf("the duplicate's ingested_at_ms = %v, want the ORIGINAL %v — "+
			"the receipt must be read back from the stored row",
			second["ingested_at_ms"], first["ingested_at_ms"])
	}

	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lifecycle_events`).Scan(&n); err != nil {
		t.Fatalf("count lifecycle_events: %v", err)
	}
	if n != 1 {
		t.Errorf("stored rows = %d, want exactly 1", n)
	}
}

// TestEventIngest_RejectionCarriesTheReasonToken pins that a rejection's
// message starts with the machine-readable reason, so a counter can group
// by it without parsing prose.
func TestEventIngest_RejectionCarriesTheReasonToken(t *testing.T) {
	dm := newTestSharedDM(t)

	cases := []struct {
		name   string
		mutate func(map[string]interface{})
		want   string
	}{
		{"unknown version", func(e map[string]interface{}) {
			e["schema_version"] = "mpm-event/0.9"
		}, mpminternal.LifecycleRejectUnknownVersion},
		{"unknown event type", func(e map[string]interface{}) {
			e["event_type"] = "session.resumed"
		}, mpminternal.LifecycleRejectUnknownEventType},
		{"non-null payload", func(e map[string]interface{}) {
			e["payload"] = map[string]interface{}{"content": "secret"}
		}, mpminternal.LifecycleRejectInvalidEnvelope},
		{"over-limit envelope", func(e map[string]interface{}) {
			e["adapter_version"] = strings.Repeat("x", 600)
		}, mpminternal.LifecycleRejectLimitExceeded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := lifecycleToolEnvelope()
			tc.mutate(env)
			_, err := handleMpmSystem(dm, defaultACForPatch(), map[string]interface{}{
				"action": "event_ingest",
				"params": map[string]interface{}{
					"schema_version": mpminternal.LifecycleEventSchemaVersion,
					"event":          env,
				},
			})
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.HasPrefix(err.Error(), tc.want+":") {
				t.Errorf("the rejection message must start with %q, got %q", tc.want+":", err.Error())
			}
			// And it must classify to the typed outcome code, so the
			// counters can group by reason without parsing prose.
			class, code := mpminternal.ClassifyError(err)
			if class != mpminternal.OutcomeClassValidation {
				t.Errorf("outcome_class = %q, want %q", class, mpminternal.OutcomeClassValidation)
			}
			if code != tc.want {
				t.Errorf("outcome_code = %q, want %q", code, tc.want)
			}
		})
	}

	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lifecycle_events`).Scan(&n); err != nil {
		t.Fatalf("count lifecycle_events: %v", err)
	}
	if n != 0 {
		t.Errorf("stored rows = %d, want 0 — a rejected event must not be stored", n)
	}
}

// TestEventIngest_UnknownVersionIsRejectedNotIgnored pins the version
// authority: a caller that sends an envelope of a version this endpoint
// does not know gets a REJECTION with the machine-readable reason
// `unknown_version` — not a silent success, and not a partial parse.
//
// A silent success is the failure mode the contract's counter requirement
// exists to prevent: the caller would come to believe an observation is
// durable when nothing stored it.
func TestEventIngest_UnknownVersionIsRejectedNotIgnored(t *testing.T) {
	dm := newTestSharedDM(t)
	env := lifecycleToolEnvelope()
	env["schema_version"] = "mpm-event/0.2"

	_, err := handleMpmSystem(dm, defaultACForPatch(), map[string]interface{}{
		"action": "event_ingest",
		"params": map[string]interface{}{
			"schema_version": mpminternal.LifecycleEventSchemaVersion,
			"event":          env,
		},
	})
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if !strings.HasPrefix(err.Error(), mpminternal.LifecycleRejectUnknownVersion+":") {
		t.Errorf("the rejection message must start with %q, got %q",
			mpminternal.LifecycleRejectUnknownVersion+":", err.Error())
	}

	// And a caller that declares an unknown version in the REQUEST is
	// rejected the same way.
	_, err = handleMpmSystem(dm, defaultACForPatch(), map[string]interface{}{
		"action": "event_ingest",
		"params": map[string]interface{}{
			"schema_version": "mpm-event/0.2",
			"event":          env,
		},
	})
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if !strings.HasPrefix(err.Error(), mpminternal.LifecycleRejectUnknownVersion+":") {
		t.Errorf("the rejection message must start with %q, got %q",
			mpminternal.LifecycleRejectUnknownVersion+":", err.Error())
	}
}

// TestEventCounters_Surface pins the counter surface's shape.
func TestEventCounters_Surface(t *testing.T) {
	dm := newTestSharedDM(t)

	env := lifecycleToolEnvelope()
	if _, err := handleMpmSystem(dm, defaultACForPatch(), map[string]interface{}{
		"action": "event_ingest",
		"params": map[string]interface{}{
			"schema_version": mpminternal.LifecycleEventSchemaVersion,
			"event":          env,
		},
	}); err != nil {
		t.Fatalf("event_ingest: %v", err)
	}

	res, err := handleMpmSystem(dm, defaultACForPatch(), map[string]interface{}{
		"action": "event_counters",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("event_counters: %v", err)
	}
	env2 := res.(map[string]interface{})
	if env2["success"] != true {
		t.Errorf("the envelope must carry success: true, got %v", env2["success"])
	}
	counters, ok := env2["counters"].(map[string]interface{})
	if !ok {
		t.Fatalf("the envelope must carry a top-level \"counters\" object, got %v", env2["counters"])
	}
	for _, key := range []string{
		"stored_total", "stored_by_event_type", "stored_by_schema_version",
		"rejected_total", "rejected_by_reason",
	} {
		if _, present := counters[key]; !present {
			t.Errorf("the counters must carry %q", key)
		}
	}
	if counters["stored_total"].(int64) != 1 {
		t.Errorf("stored_total = %v, want 1", counters["stored_total"])
	}
}

// TestEventIngest_RegistryAdvertisesBothActions pins that the registry
// declares the two actions, so a caller can discover them.
func TestEventIngest_RegistryAdvertisesBothActions(t *testing.T) {
	tool := findMpmSystemTool(t)
	var schema map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("the mpm_system schema is not valid JSON: %v", err)
	}
	props := schema["properties"].(map[string]interface{})
	action := props["action"].(map[string]interface{})
	enum := action["enum"].([]interface{})
	found := map[string]bool{}
	for _, e := range enum {
		found[e.(string)] = true
	}
	for _, want := range []string{"event_ingest", "event_counters"} {
		if !found[want] {
			t.Errorf("the mpm_system action enum must advertise %q", want)
		}
	}
}
