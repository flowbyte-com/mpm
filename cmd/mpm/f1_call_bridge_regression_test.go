// f1_call_bridge_regression_test.go — F1 regression at the CLI machine
// interface (`mpm call`).
//
// Audit finding F1: the mpm_session bridge returned "no parseable JSON"
// while underlying handoff operations worked. Root causes fixed:
//
//  1. The retired `mpm_session` tool name (still registered by agent
//     plugins) failed tools.ByName and printed the error to STDERR only —
//     zero bytes on stdout, which every agent adapter parses.
//  2. All early errors (unknown tool, payload parse) had the same
//     stderr-only contract violation.
package main

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func TestF1_UnknownToolEmitsParseableEnvelopeOnStdout(t *testing.T) {
	out := captureStdout(t, func() {
		handleCall([]string{"definitely_not_a_tool", "--payload", "{}"})
	})
	if strings.TrimSpace(out) == "" {
		t.Fatal("F1 REGRESSION: unknown-tool error produced zero bytes on stdout")
	}
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not parseable JSON: %q (%v)", out, err)
	}
	if env["success"] != false {
		t.Errorf("envelope success = %v, want false", env["success"])
	}
	if s, _ := env["error"].(string); !strings.Contains(s, "unknown tool") {
		t.Errorf("error = %q, want it to mention 'unknown tool'", s)
	}
}

func TestF1_MalformedPayloadEmitsParseableEnvelope(t *testing.T) {
	out := captureStdout(t, func() {
		handleCall([]string{"mpm_memory", "--payload", "{not json"})
	})
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not parseable JSON: %q", out)
	}
	if env["success"] != false {
		t.Errorf("expected failure envelope, got %v", env)
	}
}

func TestF1_LegacySessionAliasMapping(t *testing.T) {
	cases := []struct {
		action      string
		wantTool    string
		wantAction  string
	}{
		{"end", "mpm_handoff", "write"},
		{"handoff", "mpm_handoff", "read"},
		{"list_handoffs", "mpm_handoff", "list"},
		{"flush", "mpm_scratchpad", "flush"},
		{"read", "mpm_scratchpad", "read"},
		{"discard", "mpm_scratchpad", "discard"},
		{"promote_scratchpad", "mpm_scratchpad", "promote"},
	}
	for _, tc := range cases {
		gotAction, gotTool, params, ok := resolveLegacySessionAlias("mpm_session", map[string]interface{}{
			"action": tc.action,
			"params": map[string]interface{}{"session_id": "s1"},
		})
		if !ok || gotTool != tc.wantTool || gotAction != tc.wantAction {
			t.Errorf("alias(%q) = (%q,%q,%v), want (%q,%q,true)", tc.action, gotAction, gotTool, ok, tc.wantAction, tc.wantTool)
		}
		if params["session_id"] != "s1" {
			t.Errorf("alias(%q) dropped session_id param", tc.action)
		}
	}
	// Unknown action → no mapping.
	if _, _, _, ok := resolveLegacySessionAlias("mpm_session", map[string]interface{}{"action": "bogus"}); ok {
		t.Errorf("bogus mpm_session action must not map")
	}
	// Other tools unaffected.
	if _, _, _, ok := resolveLegacySessionAlias("mpm_memory", map[string]interface{}{}); ok {
		t.Errorf("mpm_memory must not be aliased")
	}
}

func TestF1_LegacyTopLevelShapeMapped(t *testing.T) {
	// Legacy plugin shape puts fields at top level next to action.
	action, target, params, ok := resolveLegacySessionAlias("mpm_session", map[string]interface{}{
		"action":     "end",
		"session_id": "legacy-sess",
		"summary":    "top-level shape",
	})
	if !ok || target != "mpm_handoff" || action != "write" {
		t.Fatalf("mapping wrong: %v %q %q", ok, target, action)
	}
	if params["session_id"] != "legacy-sess" || params["summary"] != "top-level shape" {
		t.Fatalf("top-level fields not carried into params: %#v", params)
	}
}

// TestF1_SessionBridgeEndToEnd writes a handoff through the ALIASED surface
// semantics (the exact path that used to fail with "no parseable JSON"),
// then reads it back through mpm_handoff read.
func TestF1_SessionBridgeEndToEnd(t *testing.T) {
	dm := newTestDMForCmd(t)

	writeRes, err := runHandler(dm, "mpm_handoff", map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"session_id":     "sess-f1-e2e",
			"summary":        "bridge end-to-end",
			"open_questions": []interface{}{"is the bridge healthy?"},
		},
	})
	if err != nil {
		t.Fatalf("aliased write failed: %v", err)
	}
	raw, _ := json.Marshal(writeRes)
	var wr struct {
		Handoff struct {
			SessionID     string   `json:"session_id"`
			Summary       string   `json:"summary"`
			OpenQuestions []string `json:"open_questions"`
		} `json:"handoff"`
	}
	if err := json.Unmarshal(raw, &wr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wr.Handoff.Summary != "bridge end-to-end" || len(wr.Handoff.OpenQuestions) != 1 {
		t.Fatalf("write round trip lost data: %s", raw)
	}

	readRes, err := runHandler(dm, "mpm_handoff", map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	rawRead, _ := json.Marshal(readRes)
	if !strings.Contains(string(rawRead), "sess-f1-e2e") {
		t.Fatalf("read did not return written handoff: %s", rawRead)
	}
	_ = mpminternal.ActiveContext{}
}
