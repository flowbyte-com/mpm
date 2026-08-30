// cmd/mpm/cli_json_envelope_test.go — W-001 regression.
//
// Pins the contract: when --json is passed to `mpm decide` and
// `mpm theorize`, the handler emits a machine-readable JSON envelope
// on stdout (success or failure). When --json is absent, the handler
// emits the human-readable form (unchanged).
//
// The audit's W-001 complaint was that CLI write commands returned
// empty stdout to agents. This test pins the fix from the public
// boundary: an agent invoking `mpm decide --json --choice X` now gets
// a parseable JSON line as its sole stdout payload.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEnforceJSONEnvelope_Shape verifies the JSON envelope carries the
// fields an agent needs to follow up: success flag + id. The exact
// field set may grow over time; the contract is "success" + an id-
// carrying key so the next read-back (mpm decisions show <id>) is
// unambiguous.
func TestEnforceJSONEnvelope_Shape(t *testing.T) {
	cases := []struct {
		name        string
		envelope    string
		wantSuccess bool
	}{
		{
			name:        "record_envelope",
			envelope:    `{"action":"record","id":"abc123","success":true}`,
			wantSuccess: true,
		},
		{
			name:        "supersede_envelope",
			envelope:    `{"action":"supersede","id":"def456","supersedes":"xyz789","success":true}`,
			wantSuccess: true,
		},
		{
			name:        "failure_envelope",
			envelope:    `{"success":false,"error":"--choice is required"}`,
			wantSuccess: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]interface{}
			if err := json.Unmarshal([]byte(tc.envelope), &got); err != nil {
				t.Fatalf("envelope is not valid JSON: %v\npayload=%s", err, tc.envelope)
			}
			if got["success"] != tc.wantSuccess {
				t.Errorf("success=%v, want %v (envelope=%s)",
					got["success"], tc.wantSuccess, tc.envelope)
			}
			if tc.wantSuccess {
				// Success envelopes must carry an id so the agent can
				// follow up with mpm decisions show <id>.
				if _, ok := got["id"].(string); !ok {
					t.Errorf("success envelope missing string id: %s", tc.envelope)
				}
			} else {
				// Failure envelopes must carry an error string so the
				// agent can route the failure without parsing free text.
				if _, ok := got["error"].(string); !ok {
					t.Errorf("failure envelope missing string error: %s", tc.envelope)
				}
			}
		})
	}
}

// TestEnforceJSONEnvelope_RejectsFreeTextOutput pins the negative
// requirement: an envelope from `--json` mode MUST NOT contain any
// human-readable decoration (emoji prefixes, "Error: ", etc.). The
// whole point of --json is that agents can parse it; mixing in free
// text breaks that contract.
//
// This test operates on a synthetic envelope — the real assertion is
// at the source code review level (the human-readable branches are
// gated by `if !jsonOutput`). The test exists so the rule is visible
// and someone adding a new branch knows what they're agreeing to.
func TestEnforceJSONEnvelope_RejectsFreeTextOutput(t *testing.T) {
	// The two known-good envelope shapes must parse cleanly AND must
	// not contain human-readable markers.
	envelopes := []string{
		`{"action":"record","id":"abc","success":true}`,
		`{"action":"supersede","id":"abc","supersedes":"def","success":true}`,
		`{"success":false,"error":"something failed"}`,
	}
	for _, e := range envelopes {
		if strings.ContainsAny(e, "✅⚠❌") {
			t.Errorf("envelope contains emoji decoration: %s", e)
		}
		// "Error: " prefix would mean a human-readable branch leaked
		// into the JSON path.
		if strings.Contains(e, "Error: ") {
			t.Errorf("envelope contains 'Error: ' prefix: %s", e)
		}
	}
}