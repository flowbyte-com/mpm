// help_test.go — mpm_help's shared discovery catalogue.
//
// The catalogue is the single source both transports serve: the native
// MCP handler and the `mpm call mpm_help` CLI fallback. These tests
// pin its contract — what it enumerates, what it returns, and how it
// rejects a malformed request.

package tools

import (
	"encoding/json"
	"reflect"
	"sort"
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

// ── input_schema: the registered JSON input schema ──────────────────────
//
// A description says what a tool is for. A schema says what a payload may
// contain. An agent invoking an unfamiliar operation needs the schema —
// the action vocabulary, the required fields, the nested parameter
// properties — because guessing any of them produces a rejected call.
//
// The schema must be the registry's OWN schema, verbatim: the same object
// the MCP surface publishes as the tool's inputSchema and the same object
// the runtime dispatcher validates against. A second, hand-written copy
// would drift from the one that actually decides whether a call is legal.

// helpSchemaOf returns a tool's registered schema decoded as a JSON object.
func helpSchemaOf(t *testing.T, tool Tool) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &out); err != nil {
		t.Fatalf("registry schema for %q is not a JSON object: %v", tool.Name, err)
	}
	return out
}

// helpActionEnum returns the enum of a schema's top-level "action"
// property — the action vocabulary the dispatcher accepts.
func helpActionEnum(schema map[string]interface{}) []string {
	props, _ := schema["properties"].(map[string]interface{})
	action, _ := props["action"].(map[string]interface{})
	raw, _ := action["enum"].([]interface{})
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// helpStringSlice renders a JSON array of strings.
func helpStringSlice(v interface{}) []string {
	raw, _ := v.([]interface{})
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// helpNestedParamKeys returns the property names of a schema's nested
// "params" object — the per-action parameters, one level down.
func helpNestedParamKeys(schema map[string]interface{}) []string {
	props, _ := schema["properties"].(map[string]interface{})
	params, _ := props["params"].(map[string]interface{})
	nested, _ := params["properties"].(map[string]interface{})
	out := make([]string, 0, len(nested))
	for k := range nested {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestHelp_ShowIncludesRegisteredInputSchema pins that show returns the
// tool's registered JSON input schema for EVERY registry tool, and that
// it returns it as a JSON object rather than an escaped JSON string.
//
// The type assertion is the load-bearing part: a schema serialised as a
// string is unreadable to an agent that wants to introspect it, and an
// agent that must unescape and re-parse it is one step away from
// misreading it.
func TestHelp_ShowIncludesRegisteredInputSchema(t *testing.T) {
	if len(Registry) == 0 {
		t.Fatal("the registry is empty; the test would prove nothing")
	}

	for _, target := range Registry {
		t.Run(target.Name, func(t *testing.T) {
			res, err := Help("show", target.Name)
			if err != nil {
				t.Fatalf("Help(show, %q) returned error: %v", target.Name, err)
			}

			raw, ok := res["input_schema"]
			if !ok {
				t.Fatalf("show response for %q has no input_schema: %+v", target.Name, res)
			}
			got, ok := raw.(map[string]interface{})
			if !ok {
				t.Fatalf("input_schema is %T, want a JSON object (map[string]interface{})", raw)
			}

			want := helpSchemaOf(t, target)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("input_schema differs from the registry schema for %q\n got: %#v\nwant: %#v", target.Name, got, want)
			}
		})
	}
}

// TestHelp_ShowSchemaSurvivesJSONRoundTripAsAnObject pins that the
// schema reaches the wire as a JSON object, not as an escaped string.
//
// The transports marshal the whole response, so a schema that is a
// string on the way out of Help would arrive at the agent as a string.
// This checks the marshalled bytes directly, because that is what the
// agent actually sees.
func TestHelp_ShowSchemaSurvivesJSONRoundTripAsAnObject(t *testing.T) {
	res, err := Help("show", "mpm_memory")
	if err != nil {
		t.Fatalf("Help(show) returned error: %v", err)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"input_schema":"`) {
		t.Error("input_schema was serialised as a JSON string; it must be a JSON object")
	}
	if !strings.Contains(string(data), `"input_schema":{`) {
		t.Errorf("input_schema is not a JSON object in the marshalled payload: %s", helpFirstN(string(data), 200))
	}

	var back map[string]interface{}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := back["input_schema"].(map[string]interface{}); !ok {
		t.Fatalf("input_schema is %T after the round trip, want map[string]interface{}", back["input_schema"])
	}
}

// TestHelp_ShowSchemaCarriesActionEnumRequiredAndNestedProperties pins
// the three structural things an agent needs from a schema, against
// values that are asserted independently of the registry — so a bug that
// returns the wrong tool's schema, or flattens the nesting, is caught
// even if the registry itself changed.
func TestHelp_ShowSchemaCarriesActionEnumRequiredAndNestedProperties(t *testing.T) {
	target, ok := ByName("mpm_memory")
	if !ok {
		t.Fatal("mpm_memory is not registered")
	}

	res, err := Help("show", target.Name)
	if err != nil {
		t.Fatalf("Help(show) returned error: %v", err)
	}
	got, ok := res["input_schema"].(map[string]interface{})
	if !ok {
		t.Fatalf("input_schema is %T, want map[string]interface{}", res["input_schema"])
	}

	// 1. The action enum — the action vocabulary the dispatcher accepts.
	actions := helpActionEnum(got)
	for _, want := range []string{"save", "query", "show", "shred", "reinforce"} {
		if !helpContains(actions, want) {
			t.Errorf("action enum %v is missing %q", actions, want)
		}
	}
	if !reflect.DeepEqual(actions, helpActionEnum(helpSchemaOf(t, target))) {
		t.Errorf("action enum differs from the registry:\n got: %v\nwant: %v", actions, helpActionEnum(helpSchemaOf(t, target)))
	}

	// 2. The required fields.
	required := helpStringSlice(got["required"])
	if !helpContains(required, "action") {
		t.Errorf("required = %v, want it to contain \"action\"", required)
	}
	if !reflect.DeepEqual(required, helpStringSlice(helpSchemaOf(t, target)["required"])) {
		t.Errorf("required differs from the registry:\n got: %v\nwant: %v", required, helpStringSlice(helpSchemaOf(t, target)["required"]))
	}

	// 3. The nested parameter properties — one level down, inside "params".
	nested := helpNestedParamKeys(got)
	for _, want := range []string{"fact", "query", "projection", "limit"} {
		if !helpContains(nested, want) {
			t.Errorf("nested params properties %v are missing %q", nested, want)
		}
	}
	if !reflect.DeepEqual(nested, helpNestedParamKeys(helpSchemaOf(t, target))) {
		t.Errorf("nested params properties differ from the registry:\n got: %v\nwant: %v", nested, helpNestedParamKeys(helpSchemaOf(t, target)))
	}
}

// TestHelp_ShowSchemaPreservesOneOfBranches pins that a schema with
// more than one branch keeps every branch.
//
// Three registry tools (mpm_handoff, mpm_system, mpm_work) declare a
// top-level "oneOf" alongside their properties. A schema that lost those
// branches would still look like an object and would still carry an
// action enum, so only an explicit check catches the loss.
func TestHelp_ShowSchemaPreservesOneOfBranches(t *testing.T) {
	checked := 0
	for _, target := range Registry {
		want := helpSchemaOf(t, target)
		branches, hasBranches := want["oneOf"].([]interface{})
		if !hasBranches {
			continue
		}
		checked++

		res, err := Help("show", target.Name)
		if err != nil {
			t.Fatalf("Help(show, %q) returned error: %v", target.Name, err)
		}
		got, ok := res["input_schema"].(map[string]interface{})
		if !ok {
			t.Fatalf("input_schema is %T, want map[string]interface{}", res["input_schema"])
		}
		gotBranches, ok := got["oneOf"].([]interface{})
		if !ok {
			t.Errorf("%q has a oneOf in the registry but none in the returned schema", target.Name)
			continue
		}
		if len(gotBranches) != len(branches) {
			t.Errorf("%q oneOf has %d branches in the returned schema, want %d", target.Name, len(gotBranches), len(branches))
			continue
		}
		if !reflect.DeepEqual(gotBranches, branches) {
			t.Errorf("%q oneOf branches differ from the registry", target.Name)
		}
	}
	if checked == 0 {
		t.Fatal("no registry tool declares a oneOf; the fixture proves nothing")
	}
}

// TestHelp_ShowPreservesExistingFields pins that adding the schema did
// not disturb the fields show already returned.
func TestHelp_ShowPreservesExistingFields(t *testing.T) {
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
		t.Error("description changed")
	}
	reach, _ := res["reach_via_cli"].(string)
	if !strings.Contains(reach, "mpm call "+target.Name) {
		t.Errorf("reach_via_cli = %q, want it to name the mpm call form", reach)
	}
}

// TestHelp_ListDoesNotCarrySchemas pins that the list action stays a
// summary.
//
// The list is the cheap enumeration an agent scans; embedding 21 full
// schemas in it would make the common case expensive for no benefit. The
// schema belongs to show, which is the per-tool detail request.
func TestHelp_ListDoesNotCarrySchemas(t *testing.T) {
	res, err := Help("list", "")
	if err != nil {
		t.Fatalf("Help(list) returned error: %v", err)
	}
	rows, ok := res["tools"].([]map[string]interface{})
	if !ok {
		t.Fatalf("tools is %T, want []map[string]interface{}", res["tools"])
	}
	for _, row := range rows {
		if _, present := row["input_schema"]; present {
			t.Errorf("row %v carries a schema; the list action must stay a summary", row["name"])
		}
	}
}

// helpContains reports whether needle is in haystack.
func helpContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// helpFirstN returns at most n leading characters of s.
func helpFirstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
