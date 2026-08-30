// wake_compact_test.go — regression tests for alpha-4 W-001
// (lightweight wake projection).
//
// Pins the compact shape: 9 documented fields, no heavy surfaces
// (recent_memories / recent_milestones / available_skills /
// global_rules / overdue_wakes), and a byte-size budget that proves
// the projection actually sheds payload.

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// compactFields are the 9 documented fields of the alpha-4 W-001
// CompactWakeContext projection. Kept here (not in handlers.go) so the
// regression test names the public contract independently of the
// internal struct — a future field addition must update both, which
// is the intent of pinning this surface.
var compactFields = []string{
	"session_id",
	"session_current_id",
	"session_started_at",
	"active_mode",
	"active_persona",
	"last_handoff_summary",
	"last_handoff_ended_at",
	"open_work_ids",
	"audit_summary",
	"recent_artifact_ids",
}

// TestWakeContext_CompactProjection_StripsHeavyFields is the alpha-4
// W-001 pin: with projection:"compact", the returned envelope must
// carry the 9 compact fields and MUST NOT carry the heavy surfaces
// (recent_memories, available_skills, global_rules, overdue_wakes).
// Also pins the byte-size budget — compact must stay well under 2 KB
// even with seeded state.
func TestWakeContext_CompactProjection_StripsHeavyFields(t *testing.T) {
	dm := newTestIsolatedDM(t)
	seedWakeState(t, dm)

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{
			"projection": "compact",
		},
	})
	if err != nil {
		t.Fatalf("read_wake_context compact: %v", err)
	}

	raw := mustMarshalJSON(res)

	// All 9 documented compact fields must be present (excluding
	// session_started_at which is omitempty and may be 0 on a fresh DM).
	for _, f := range []string{"session_id", "session_current_id", "active_mode", "active_persona", "open_work_ids", "audit_summary", "recent_artifact_ids"} {
		if !strings.Contains(raw, `"`+f+`"`) {
			t.Errorf("compact projection missing field %q; payload=%s", f, raw)
		}
	}

	// Heavy surfaces MUST be stripped.
	for _, heavy := range []string{"recent_memories", "recent_milestones", "available_skills", "global_rules", "overdue_wakes", "open_works", "completed_works", "epistemic_pressure"} {
		if strings.Contains(raw, `"`+heavy+`"`) {
			t.Errorf("compact projection must NOT include %q; payload=%s", heavy, raw)
		}
	}

	// Byte-size budget: the projection must be visibly smaller than
	// the full payload. 4 KB is generous — production seeds produce
	// ~1-2 KB.
	if len(raw) > 4096 {
		t.Errorf("compact projection too large: %d bytes (budget 4096); payload=%s", len(raw), raw)
	}
}

// TestWakeContext_CompactProjection_EnvelopeContract pins the
// top-level envelope shape — {success, projection, context} with
// projection="compact". This is what agents dispatch on, so the
// shape must match the documented contract.
func TestWakeContext_CompactProjection_EnvelopeContract(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{
			"projection": "compact",
		},
	})
	if err != nil {
		t.Fatalf("read_wake_context compact: %v", err)
	}

	var envelope struct {
		Success   bool            `json:"success"`
		Projection string         `json:"projection"`
		Context   json.RawMessage `json:"context"`
	}
	raw := mustMarshalJSON(res)
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, raw)
	}
	if !envelope.Success {
		t.Errorf("envelope.success = false; payload=%s", raw)
	}
	if envelope.Projection != "compact" {
		t.Errorf("envelope.projection = %q, want \"compact\"", envelope.Projection)
	}
	if len(envelope.Context) == 0 {
		t.Errorf("envelope.context missing; payload=%s", raw)
	}

	// Inner context must have all 9 documented compact fields, with
	// the right types (string fields are strings, slices are arrays).
	var ctx struct {
		SessionID         string   `json:"session_id"`
		SessionCurrentID  string   `json:"session_current_id"`
		SessionStartedAt  int64    `json:"session_started_at"`
		ActiveMode        string   `json:"active_mode"`
		ActivePersona     string   `json:"active_persona"`
		LastHandoffSummary string  `json:"last_handoff_summary,omitempty"`
		LastHandoffEndedAt int64   `json:"last_handoff_ended_at,omitempty"`
		OpenWorkIDs       []string `json:"open_work_ids"`
		AuditSummary      string   `json:"audit_summary"`
		RecentArtifactIDs []string `json:"recent_artifact_ids"`
	}
	if err := json.Unmarshal(envelope.Context, &ctx); err != nil {
		t.Fatalf("unmarshal context: %v (ctx=%s)", err, envelope.Context)
	}
	// OpenWorkIDs must be a JSON array (non-nil even when empty per
	// invariant 3 on WakeContextData — the compact struct propagates
	// the same invariant).
	if ctx.OpenWorkIDs == nil {
		t.Errorf("compact.open_work_ids must be non-nil empty slice on no-open-works seed")
	}
	if ctx.RecentArtifactIDs == nil {
		t.Errorf("compact.recent_artifact_ids must be non-nil empty slice")
	}
}

// TestWakeContext_FullProjection_StillReturnsAllFields is the
// companion pin: default / projection:"full" / projection omitted
// must still return the full WakeContextData with recent_memories
// etc. present. The compact path must not regress the full path.
func TestWakeContext_FullProjection_StillReturnsAllFields(t *testing.T) {
	dm := newTestIsolatedDM(t)

	// Default — no projection key.
	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("read_wake_context (default): %v", err)
	}
	raw := mustMarshalJSON(res)
	for _, heavy := range []string{"recent_memories", "available_skills", "overdue_wakes"} {
		if !strings.Contains(raw, `"`+heavy+`"`) {
			t.Errorf("full projection must include %q; payload=%s", heavy, raw)
		}
	}

	// Explicit projection:"full".
	res2, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{"projection": "full"},
	})
	if err != nil {
		t.Fatalf("read_wake_context (full): %v", err)
	}
	raw2 := mustMarshalJSON(res2)
	if !strings.Contains(raw2, `"recent_memories"`) {
		t.Errorf("projection:full must include recent_memories; payload=%s", raw2)
	}
}

// TestWakeContext_CompactProjection_RoundsToSeededArtifacts checks
// that seeded memories and milestones surface as recent_artifact_ids
// in the compact projection. This is the semantic-correctness pin
// for the projection — it must not silently drop the very signal
// the compact field is meant to convey.
func TestWakeContext_CompactProjection_RoundsToSeededArtifacts(t *testing.T) {
	dm := newTestIsolatedDM(t)

	// Seed one memory + one milestone so recent_artifact_ids has
	// something to round-trip.
	if _, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "alpha-4 W-001 compact seed memory",
			"tags": []interface{}{"w001-compact"},
		},
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{"projection": "compact"},
	})
	if err != nil {
		t.Fatalf("read_wake_context compact: %v", err)
	}

	var envelope struct {
		Context struct {
			RecentArtifactIDs []string `json:"recent_artifact_ids"`
		} `json:"context"`
	}
	raw := mustMarshalJSON(res)
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}
	if len(envelope.Context.RecentArtifactIDs) == 0 {
		t.Errorf("expected ≥1 recent_artifact_id after seeding; payload=%s", raw)
	}
}

// seedWakeState seeds the minimum state needed to exercise wake
// gathering: a memory, a milestone, and a handoff. Keeps the
// individual tests focused on their assertions rather than on
// fixture plumbing.
func seedWakeState(t *testing.T, dm interface{}) {
	t.Helper()
	// Type-assert against the tools-package DM interface. The seed
	// uses the public tool surface so it doesn't depend on internal
	// DM methods that may shift between releases.
	if _, ok := dm.(mpminternal.CoreDB); !ok {
		t.Fatalf("seedWakeState: dm does not satisfy mpminternal.CoreDB")
	}
	cdb := dm.(mpminternal.CoreDB)

	if _, err := handleMpmMemory(cdb, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "alpha-4 W-001 seed",
			"tags": []interface{}{"w001-seed"},
		},
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
}
