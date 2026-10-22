// help_test.go — mpm_help's shared discovery catalogue.
//
// The catalogue is the single source both transports serve: the native
// MCP handler and the `mpm call mpm_help` CLI fallback. These tests
// pin its contract — what it enumerates, what it returns, and how it
// rejects a malformed request.

package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHelp_ListEnumeratesRegistry pins that discovery reports every
// registry tool, with the fields an agent needs to call it.
func TestHelp_ListEnumeratesRegistry(t *testing.T) {
	res, err := Help("list", "")
	if err != nil {
		t.Fatalf("Help(list) returned error: %v", err)
	}
	if res["success"] != true {
		t.Errorf("success = %v, want true", res["success"])
	}
	count, ok := res["tool_count"].(int)
	if !ok {
		t.Fatalf("tool_count is %T, want int", res["tool_count"])
	}
	if count != len(Registry) {
		t.Errorf("tool_count = %d, want %d", count, len(Registry))
	}
	rows, ok := res["tools"].([]map[string]interface{})
	if !ok {
		t.Fatalf("tools is %T, want []map[string]interface{}", res["tools"])
	}
	if len(rows) != len(Registry) {
		t.Fatalf("tools has %d rows, want %d", len(rows), len(Registry))
	}

	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		if name == "" {
			t.Errorf("row without a name: %+v", row)
			continue
		}
		seen[name] = true
		if _, ok := row["one_liner"].(string); !ok {
			t.Errorf("row %q has no one_liner: %+v", name, row)
		}
		reach, _ := row["reach_via_cli"].(string)
		if !strings.Contains(reach, "mpm call "+name) {
			t.Errorf("row %q reach_via_cli = %q, want it to name the mpm call form", name, reach)
		}
	}
	for _, tl := range Registry {
		if !seen[tl.Name] {
			t.Errorf("discovery is missing registry tool %q", tl.Name)
		}
	}
}

// TestHelp_ShowReturnsFullDescription pins that show returns the
// tool's complete description, not the one-line summary.
func TestHelp_ShowReturnsFullDescription(t *testing.T) {
	target := Registry[0]
	res, err := Help("show", target.Name)
	if err != nil {
		t.Fatalf("Help(show) returned error: %v", err)
	}
	if res["success"] != true {
		t.Errorf("success = %v, want true", res["success"])
	}
	if res["name"] != target.Name {
		t.Errorf("name = %v, want %q", res["name"], target.Name)
	}
	if res["description"] != target.Description {
		t.Errorf("description = %v, want the registry description", res["description"])
	}
	reach, _ := res["reach_via_cli"].(string)
	if !strings.Contains(reach, "mpm call "+target.Name) {
		t.Errorf("reach_via_cli = %q, want it to name the mpm call form", reach)
	}
}

// TestHelp_Rejections pins the reason each malformed request is
// rejected. The messages are part of the contract: the CLI reports
// them verbatim, and the native MCP surface reports the same text.
func TestHelp_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		action   string
		toolName string
		wantErr  string
	}{
		{"unknown action", "enlist", "", `mpm_help: unknown action "enlist" (valid: list, show)`},
		{"empty action", "", "", `mpm_help: unknown action "" (valid: list, show)`},
		{"show without tool", "show", "", "mpm_help show: 'tool' param required"},
		{"show unknown tool", "show", "mpm_not_a_tool", `mpm_help: unknown tool "mpm_not_a_tool"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Help(tc.action, tc.toolName)
			if err == nil {
				t.Fatalf("Help(%q, %q) succeeded, want an error", tc.action, tc.toolName)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			if res != nil {
				t.Errorf("a rejected request must not return a payload: %+v", res)
			}
		})
	}
}

// TestHelp_PayloadIsJSONSerializable pins that the catalogue survives
// the JSON round trip both transports perform, with the success flag
// intact.
func TestHelp_PayloadIsJSONSerializable(t *testing.T) {
	res, err := Help("list", "")
	if err != nil {
		t.Fatalf("Help(list) returned error: %v", err)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["success"] != true {
		t.Errorf("success = %v after round trip, want true", back["success"])
	}
	if _, ok := back["tools"]; !ok {
		t.Error("tools missing after round trip")
	}
}

// TestHelpActions_MatchImplementedActions pins that the machine-readable
// action list and the implemented contract agree.
//
// HelpActions exists so that callers — the agent protocol guard above all
// — can know the action vocabulary without calling Help. If the two
// drift, the guard would validate a documented invocation against an
// action the catalogue does not implement, and the operator would get an
// "unknown action" error from a command the documentation promised.
func TestHelpActions_MatchImplementedActions(t *testing.T) {
	actions := HelpActions()
	if len(actions) == 0 {
		t.Fatal("HelpActions returned no actions; the contract is empty")
	}

	for _, action := range actions {
		// A known tool name so that "show" has what it needs. The point
		// is that the action is not rejected as unknown.
		if _, err := Help(action, "mpm_memory"); err != nil {
			t.Errorf("HelpActions reports action %q but Help rejects it: %v", action, err)
		}
	}

	_, err := Help("definitely-not-an-action", "")
	if err == nil {
		t.Fatal("Help accepted an action it does not implement")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("rejection of an unknown action does not say so: %v", err)
	}
	// The message must list exactly the actions that exist, so the
	// operator is never told about an action that is not implemented.
	want := "valid: " + strings.Join(actions, ", ")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("unknown-action rejection does not list %q; got %q", want, err.Error())
	}
}

// TestCallRoutedTools_PinsTheDiscoverySurface pins the call-routed
// routing table.
//
// mpm_help must stay call-routed — the CLI fallback is the only way a
// shell-only agent can discover the surface — and its action
// vocabulary must be the catalogue's own.
func TestCallRoutedTools_PinsTheDiscoverySurface(t *testing.T) {
	if !IsCallRouted(CallRoutedHelp) {
		t.Errorf("%s must be call-routed", CallRoutedHelp)
	}
	if _, ok := ByName(CallRoutedHelp); ok {
		t.Errorf("%s must NOT be a registry entry; a registry entry would double-register under MPM_EXPOSE_ALL_TOOLS=1", CallRoutedHelp)
	}

	actions, ok := CallRoutedActions(CallRoutedHelp)
	if !ok {
		t.Fatalf("%s has no call-routed actions", CallRoutedHelp)
	}
	want := HelpActions()
	if len(actions) != len(want) {
		t.Fatalf("call-routed actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Errorf("call-routed actions = %v, want %v", actions, want)
			break
		}
	}

	// The table must hand out copies: a caller that mutates what it
	// received must not change the routing.
	routed := CallRoutedTools()
	if len(routed) != 1 {
		t.Fatalf("CallRoutedTools returned %d entries, want 1", len(routed))
	}
	routed[0].Actions[0] = "mutated"
	again, _ := CallRoutedActions(CallRoutedHelp)
	if again[0] == "mutated" {
		t.Error("CallRoutedTools exposed the routing table's action slice")
	}

	// A name that is neither a registry tool nor call-routed is
	// genuinely unknown.
	if IsCallRouted("mpm_definitely_not_a_tool") {
		t.Error("IsCallRouted accepted an unknown name")
	}
}
