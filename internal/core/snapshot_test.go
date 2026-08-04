package internal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestResolveSnapshotRequiresAgent verifies ResolveSnapshot fails when
// AgentID is empty — guards against silent saves with anonymous provenance.
func TestResolveSnapshotRequiresAgent(t *testing.T) {
	_, err := ResolveSnapshot(context.Background(), nil, WrapperContext{
		SessionID: "test-session",
	}, "", "")
	if err == nil {
		t.Fatal("expected error for missing agent_id, got nil")
	}
	var se *SnapshotError
	if !errors.As(err, &se) || se.Field != "wrapper.agent_id" {
		t.Errorf("expected SnapshotError on wrapper.agent_id, got %v", err)
	}
}

// TestResolveSnapshotRequiresSession verifies session_id is required.
func TestResolveSnapshotRequiresSession(t *testing.T) {
	_, err := ResolveSnapshot(context.Background(), nil, WrapperContext{
		AgentID: "main",
	}, "", "")
	if err == nil {
		t.Fatal("expected error for missing session_id, got nil")
	}
	var se *SnapshotError
	if !errors.As(err, &se) || se.Field != "wrapper.session_id" {
		t.Errorf("expected SnapshotError on wrapper.session_id, got %v", err)
	}
}

// TestSnapshotVersionConstant pins the schema version — bumps on shape changes.
func TestSnapshotVersionConstant(t *testing.T) {
	if SnapshotSchemaVersion != "1.0.0" {
		t.Errorf("SnapshotSchemaVersion changed unexpectedly: %q", SnapshotSchemaVersion)
	}
}

// TestEnumBounds — confidence_band, reasoning_depth, validation.status, source_tool_class.
func TestEnumBounds(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value string
		want  bool
	}{
		{"confidence_low", "confidence_band", "low", true},
		{"confidence_medium", "confidence_band", "medium", true},
		{"confidence_high", "confidence_band", "high", true},
		{"confidence_bogus", "confidence_band", "very", false},
		{"depth_shallow", "reasoning_depth", "shallow", true},
		{"depth_deep", "reasoning_depth", "deep", true},
		{"depth_bogus", "reasoning_depth", "tactical", false},
		{"status_unvalidated", "validation_status", "unvalidated", true},
		{"status_corroborated", "validation_status", "corroborated", true},
		{"status_contradicted", "validation_status", "contradicted", true},
		{"status_bogus", "validation_status", "suspect", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			switch tc.field {
			case "confidence_band":
				got = validConfidenceBands[tc.value]
			case "reasoning_depth":
				got = validReasoningDepths[tc.value]
			case "validation_status":
				got = validValidationStatuses[tc.value]
			}
			if got != tc.want {
				t.Errorf("%s=%q: want %v, got %v", tc.field, tc.value, tc.want, got)
			}
		})
	}
}

// TestShortHashStable — active_goal_id must be deterministic for correlation.
func TestShortHashStable(t *testing.T) {
	h1 := shortHash("test goal text")
	h2 := shortHash("test goal text")
	if h1 != h2 {
		t.Errorf("shortHash not stable: %s vs %s", h1, h2)
	}
	if len(h1) != 16 {
		t.Errorf("shortHash length: want 16, got %d (%s)", len(h1), h1)
	}
	h3 := shortHash("different goal text")
	if h1 == h3 {
		t.Errorf("shortHash collision across distinct inputs: %s", h1)
	}
}

// TestBuildProvenanceStaleDrop — RecentTool older than the window → no provenance.
func TestBuildProvenanceStaleDrop(t *testing.T) {
	old := &ToolCallRecord{
		ToolName: "read_file",
		CallID:   "test-call-1",
		URI:      "/tmp/foo.md",
		CalledAt: time.Now().Add(-2 * time.Minute), // well past any window
	}
	prov, ok := buildProvenanceContext(old, 60000)
	if ok || prov != nil {
		t.Errorf("expected (nil, false) for stale tool call, got (%+v, %v)", prov, ok)
	}
}

// TestBuildProvenanceFreshAccept — RecentTool within window → provenance stamped.
func TestBuildProvenanceFreshAccept(t *testing.T) {
	fresh := &ToolCallRecord{
		ToolName: "read_file",
		CallID:   "test-call-2",
		URI:      "/tmp/foo.md",
		CalledAt: time.Now().Add(-5 * time.Second),
	}
	prov, ok := buildProvenanceContext(fresh, 60000)
	if !ok || prov == nil {
		t.Fatalf("expected provenance block, got (%+v, %v)", prov, ok)
	}
	if prov.SourceTool != "read_file" {
		t.Errorf("SourceTool: want read_file, got %q", prov.SourceTool)
	}
	if prov.ObservationWindowMS <= 0 || prov.ObservationWindowMS > 60000 {
		t.Errorf("ObservationWindowMS out of range: %d", prov.ObservationWindowMS)
	}
}

// TestBuildProvenanceURITooLong — uri > 2048 → reject, don't truncate.
func TestBuildProvenanceURITooLong(t *testing.T) {
	longURI := strings.Repeat("a", URIMaxChars+1)
	rt := &ToolCallRecord{
		ToolName: "web_fetch",
		CallID:   "test-call-3",
		URI:      longURI,
		CalledAt: time.Now(),
	}
	_, ok := buildProvenanceContext(rt, 60000)
	if ok {
		t.Errorf("expected (nil, false) for URI > %d chars, but build succeeded", URIMaxChars)
	}
	// Validate() must also catch this for defense-in-depth.
	snap := &EpistemicSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Creator:       &CreatorContext{AgentID: "a", SessionID: "s"},
		Provenance: &ProvenanceContext{
			SourceTool:          "web_fetch",
			URI:                 longURI,
			ObservationWindowMS: 100,
		},
	}
	if err := snap.Validate(); err == nil {
		t.Error("Validate should reject URI > URIMaxChars")
	}
}

// TestValidateTriggerEvidenceInvariant — trigger_evidence_id required iff
// status ≠ unvalidated. Pins the contract.
func TestValidateTriggerEvidenceInvariant(t *testing.T) {
	cases := []struct {
		name    string
		state   ValidationState
		wantErr bool
	}{
		{
			name:    "unvalidated_no_trigger_ok",
			state:   ValidationState{Status: "unvalidated", EvidenceCount: 0},
			wantErr: false,
		},
		{
			name:    "unvalidated_with_trigger_rejected",
			state:   ValidationState{Status: "unvalidated", EvidenceCount: 0, TriggerEvidenceID: "abc123"},
			wantErr: true,
		},
		{
			name:    "corroborated_no_trigger_rejected",
			state:   ValidationState{Status: "corroborated", EvidenceCount: 1},
			wantErr: true,
		},
		{
			name:    "corroborated_with_trigger_ok",
			state:   ValidationState{Status: "corroborated", EvidenceCount: 1, TriggerEvidenceID: "abc123"},
			wantErr: false,
		},
		{
			name:    "contradicted_with_trigger_ok",
			state:   ValidationState{Status: "contradicted", EvidenceCount: 1, TriggerEvidenceID: "abc123"},
			wantErr: false,
		},
		{
			name:    "bogus_status_rejected",
			state:   ValidationState{Status: "suspect", EvidenceCount: 0},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &EpistemicSnapshot{
				SchemaVersion: SnapshotSchemaVersion,
				Creator:       &CreatorContext{AgentID: "a", SessionID: "s"},
				Validation:    &tc.state,
			}
			err := snap.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestMergeIntoPreservesExistingKeys — MergeInto must not clobber agent-invented
// top-level metadata keys.
func TestMergeIntoPreservesExistingKeys(t *testing.T) {
	snap := &EpistemicSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Creator:       &CreatorContext{AgentID: "main", SessionID: "test"},
	}
	existing := map[string]interface{}{
		"provenance": map[string]interface{}{"source": "agent"}, // legacy DecayWeights contract
		"custom_tag": "user-added",
	}
	out := snap.MergeInto(existing)
	if out["provenance"] == nil {
		t.Error("MergeInto clobbered existing provenance block")
	}
	if out["custom_tag"] != "user-added" {
		t.Error("MergeInto clobbered existing custom_tag")
	}
	if out["_epistemic_snapshot"] == nil {
		t.Error("MergeInto did not stamp _epistemic_snapshot")
	}
}
