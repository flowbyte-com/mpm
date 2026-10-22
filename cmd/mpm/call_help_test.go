// call_help_test.go — `mpm call mpm_help` capability discovery.
//
// mpm_help is MPM's capability-discovery surface: action=list returns
// every registry tool with its reachability, action=show returns a
// tool's full description together with its registered JSON input
// schema. It is registered as a native MCP closure
// (cmd/mpm-mcp/tools.go::makeHelpHandler), which made it the one
// capability a shell-only agent could not reach: `mpm call` dispatches
// registry-backed tools, and the discovery closure is not a registry
// entry.
//
// These tests drive the real binary against an isolated workspace.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/tools"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// callHelpMPM runs the freshly-built mpm binary against an isolated
// workspace plus any extra environment entries, and returns stdout,
// stderr and the process error (non-nil means a non-zero exit).
//
// The environment comes from testenv.WithExtra, which pins
// MPM_WORKSPACE and HOME to temporary directories and keeps the
// ambient environment out. A child that inherited the ambient
// environment could resolve the operator's live installation; the
// repository's subprocess-isolation guard (internal/testenv) exists for
// exactly that reason and rejects any MPM subprocess that does not pin
// its environment. `extraEnv` entries are appended last, so a caller can
// deliberately point MPM_WORKSPACE somewhere else.
func callHelpMPM(t *testing.T, extraEnv []string, stdin string, args ...string) (string, string, error) {
	t.Helper()
	if mpmBin == "" {
		t.Skip("mpm binary not built (see TestMain); skipping CLI discovery regression")
	}
	cmd := exec.Command(mpmBin, args...)
	cmd.Env = testenv.WithExtra(t, extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	return so.String(), se.String(), err
}

// decodeHelpEnvelope parses the JSON envelope `mpm call` writes to
// stdout.
func decodeHelpEnvelope(t *testing.T, stdout string) map[string]interface{} {
	t.Helper()
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\nstdout=%s", err, stdout)
	}
	return env
}

// TestCallHelp_ListEnumeratesRegistry pins the discovery contract: a
// shell-only agent must be able to enumerate every capability MPM
// exposes, with the same tool names the native MCP surface reports.
func TestCallHelp_ListEnumeratesRegistry(t *testing.T) {
	stdout, stderr, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":"list"}`)
	if err != nil {
		t.Fatalf("mpm call mpm_help list failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != true {
		t.Fatalf("expected success=true, got %v\nstdout=%s", env["success"], stdout)
	}
	count, ok := env["tool_count"].(float64)
	if !ok {
		t.Fatalf("tool_count missing or not numeric: %v\nstdout=%s", env["tool_count"], stdout)
	}
	if int(count) != len(tools.Registry) {
		t.Errorf("tool_count = %d, want %d (the live registry)", int(count), len(tools.Registry))
	}
	rows, ok := env["tools"].([]interface{})
	if !ok {
		t.Fatalf("tools is not an array: %v\nstdout=%s", env["tools"], stdout)
	}
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		row, ok := r.(map[string]interface{})
		if !ok {
			t.Fatalf("tool row is not an object: %v", r)
		}
		name, _ := row["name"].(string)
		if name == "" {
			t.Errorf("tool row missing name: %v", row)
			continue
		}
		seen[name] = true
		if _, ok := row["reach_via_cli"].(string); !ok {
			t.Errorf("tool %q row missing reach_via_cli: %v", name, row)
		}
		if _, ok := row["one_liner"].(string); !ok {
			t.Errorf("tool %q row missing one_liner: %v", name, row)
		}
	}
	for _, tl := range tools.Registry {
		if !seen[tl.Name] {
			t.Errorf("discovery list is missing registry tool %q", tl.Name)
		}
	}
}

// TestCallHelp_ShowReturnsFullDescription pins the show action: the
// documented schema for a known tool, in the canonical {action, params}
// payload shape.
func TestCallHelp_ShowReturnsFullDescription(t *testing.T) {
	stdout, stderr, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":"show","params":{"tool":"mpm_memory"}}`)
	if err != nil {
		t.Fatalf("mpm call mpm_help show failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != true {
		t.Fatalf("expected success=true, got %v\nstdout=%s", env["success"], stdout)
	}
	if env["name"] != "mpm_memory" {
		t.Errorf("name = %v, want mpm_memory", env["name"])
	}
	desc, _ := env["description"].(string)
	if desc == "" {
		t.Errorf("show must return a description, got %v", env["description"])
	}
	reach, _ := env["reach_via_cli"].(string)
	if !strings.Contains(reach, "mpm call mpm_memory") {
		t.Errorf("reach_via_cli = %q, want it to name the mpm call form", reach)
	}
}

// TestCallHelp_ShowAcceptsTopLevelToolParam pins that the flat payload
// shape used by the native MCP tool ("tool" beside "action") also
// works, so an agent that learned the native schema is not punished for
// using it through the CLI.
func TestCallHelp_ShowAcceptsTopLevelToolParam(t *testing.T) {
	stdout, stderr, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":"show","tool":"mpm_memory"}`)
	if err != nil {
		t.Fatalf("mpm call mpm_help show failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != true {
		t.Fatalf("expected success=true, got %v\nstdout=%s", env["success"], stdout)
	}
	if env["name"] != "mpm_memory" {
		t.Errorf("name = %v, want mpm_memory", env["name"])
	}
}

// TestCallHelp_ShowUnknownToolRejected pins that an unknown tool is
// reported, not silently answered with an empty schema.
func TestCallHelp_ShowUnknownToolRejected(t *testing.T) {
	stdout, _, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":"show","params":{"tool":"mpm_not_a_tool"}}`)
	if err == nil {
		t.Fatalf("expected non-zero exit for an unknown tool\nstdout=%s", stdout)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != false {
		t.Errorf("expected success=false, got %v\nstdout=%s", env["success"], stdout)
	}
	msg, _ := env["error"].(string)
	if !strings.Contains(msg, "unknown tool") {
		t.Errorf("error = %q, want it to name the unknown tool", msg)
	}
	if !strings.Contains(msg, "mpm_not_a_tool") {
		t.Errorf("error = %q, want it to name the tool that was asked for", msg)
	}
}

// TestCallHelp_ShowWithoutToolRejected pins that show without a tool
// name is a rejected request rather than a list.
func TestCallHelp_ShowWithoutToolRejected(t *testing.T) {
	stdout, _, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":"show"}`)
	if err == nil {
		t.Fatalf("expected non-zero exit for show without a tool\nstdout=%s", stdout)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != false {
		t.Errorf("expected success=false, got %v\nstdout=%s", env["success"], stdout)
	}
	msg, _ := env["error"].(string)
	if !strings.Contains(msg, "'tool' param required") {
		t.Errorf("error = %q, want it to say the tool param is required", msg)
	}
}

// TestCallHelp_UnknownActionRejected pins that an unsupported action is
// rejected predictably, naming the valid actions.
func TestCallHelp_UnknownActionRejected(t *testing.T) {
	stdout, _, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":"enlist"}`)
	if err == nil {
		t.Fatalf("expected non-zero exit for an unknown action\nstdout=%s", stdout)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != false {
		t.Errorf("expected success=false, got %v\nstdout=%s", env["success"], stdout)
	}
	msg, _ := env["error"].(string)
	if !strings.Contains(msg, "unknown action") {
		t.Errorf("error = %q, want it to report an unknown action", msg)
	}
	if !strings.Contains(msg, "list") || !strings.Contains(msg, "show") {
		t.Errorf("error = %q, want it to name the valid actions (list, show)", msg)
	}
}

// TestCallHelp_MalformedPayloadRejected pins that a malformed payload
// does not silently succeed.
func TestCallHelp_MalformedPayloadRejected(t *testing.T) {
	stdout, _, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":`)
	if err == nil {
		t.Fatalf("expected non-zero exit for a malformed payload\nstdout=%s", stdout)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != false {
		t.Errorf("expected success=false, got %v\nstdout=%s", env["success"], stdout)
	}
	msg, _ := env["error"].(string)
	if !strings.Contains(msg, "failed to parse payload") {
		t.Errorf("error = %q, want it to report a payload parse failure", msg)
	}
}

// TestCallHelp_StdinPayloadPath pins the documented stdin input path.
func TestCallHelp_StdinPayloadPath(t *testing.T) {
	stdout, stderr, err := callHelpMPM(t, nil, `{"action":"list"}`,
		"call", "mpm_help")
	if err != nil {
		t.Fatalf("mpm call mpm_help via stdin failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != true {
		t.Fatalf("expected success=true, got %v\nstdout=%s", env["success"], stdout)
	}
}

// TestCallHelp_PayloadFilePath pins the documented --payload-file input
// path.
func TestCallHelp_PayloadFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, []byte(`{"action":"list"}`), 0o600); err != nil {
		t.Fatalf("write payload file: %v", err)
	}
	stdout, stderr, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload-file", path)
	if err != nil {
		t.Fatalf("mpm call mpm_help via --payload-file failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != true {
		t.Fatalf("expected success=true, got %v\nstdout=%s", env["success"], stdout)
	}
}

// TestCallHelp_DoesNotTouchWorkspaceDatabase pins that capability
// discovery is read-only with respect to the store: it must not create
// the workspace database, because an agent discovering what it can do
// should not be able to materialise a database as a side effect.
func TestCallHelp_DoesNotTouchWorkspaceDatabase(t *testing.T) {
	workspace := t.TempDir()
	stdout, stderr, err := callHelpMPM(t,
		[]string{"MPM_WORKSPACE=" + workspace}, "", "call", "mpm_help",
		"--payload", `{"action":"list"}`)
	if err != nil {
		t.Fatalf("mpm call mpm_help list failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	env := decodeHelpEnvelope(t, stdout)
	if env["success"] != true {
		t.Fatalf("expected success=true, got %v\nstdout=%s", env["success"], stdout)
	}
	dbPath := filepath.Join(workspace, "src", "db", "mpm.db")
	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Errorf("discovery created %s; capability discovery must not materialise a database", dbPath)
	}
}

// TestCallHelp_ShowReturnsInputSchemaAsAJSONObject pins that the CLI
// fallback hands the agent the tool's registered JSON input schema, and
// hands it over as a JSON object rather than an escaped JSON string.
//
// The description says what a tool is for; the schema says what a payload
// may contain. An agent invoking an unfamiliar operation needs the
// schema — the action vocabulary, the required fields, the nested
// parameter properties — because guessing any of them produces a
// rejected call. A schema serialised as a string has to be unescaped and
// re-parsed by the reader, which is one step away from misreading it.
//
// The schema is compared against the registry's own definition, so a
// second hand-written copy would fail here rather than drift silently.
func TestCallHelp_ShowReturnsInputSchemaAsAJSONObject(t *testing.T) {
	stdout, stderr, err := callHelpMPM(t, nil, "", "call", "mpm_help",
		"--payload", `{"action":"show","params":{"tool":"mpm_memory"}}`)
	if err != nil {
		t.Fatalf("mpm call mpm_help show failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}

	// The wire form matters: an object, not a string.
	if strings.Contains(stdout, `"input_schema":"`) {
		t.Errorf("input_schema was serialised as a JSON string; it must be a JSON object\nstdout=%s", stdout)
	}
	if !strings.Contains(stdout, `"input_schema":{`) {
		t.Errorf("input_schema is not a JSON object in the CLI envelope\nstdout=%s", stdout)
	}

	env := decodeHelpEnvelope(t, stdout)
	got, ok := env["input_schema"].(map[string]interface{})
	if !ok {
		t.Fatalf("input_schema is %T, want a JSON object\nstdout=%s", env["input_schema"], stdout)
	}

	registered, ok := tools.ByName("mpm_memory")
	if !ok {
		t.Fatal("mpm_memory is not registered")
	}
	var want map[string]interface{}
	if err := json.Unmarshal(registered.Schema, &want); err != nil {
		t.Fatalf("registry schema for mpm_memory is not a JSON object: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("input_schema differs from the registry schema\n got: %#v\nwant: %#v", got, want)
	}

	// The schema must still carry the structure that makes it usable:
	// the action vocabulary, the required fields, the nested parameters.
	props, _ := got["properties"].(map[string]interface{})
	action, _ := props["action"].(map[string]interface{})
	enum, _ := action["enum"].([]interface{})
	if len(enum) == 0 {
		t.Error("input_schema has no action enum; the schema cannot guide an action choice")
	}
	if required, _ := got["required"].([]interface{}); len(required) == 0 {
		t.Error("input_schema has no required fields")
	}
	params, _ := props["params"].(map[string]interface{})
	nested, _ := params["properties"].(map[string]interface{})
	if len(nested) == 0 {
		t.Error("input_schema has no nested params properties")
	}
}

// TestCallHelp_CallRoutedCatalogueIsDispatchable pins that every
// capability the shared routing table advertises as call-routed is
// actually served by this CLI.
//
// The routing table (tools.CallRoutedTools) is the single definition of
// which names `mpm call` can reach without a registry entry. The
// agent protocol guard resolves documented invocations against that
// table, so a name listed there without a handler here would be
// documented as reachable and then fail at call time — the exact
// stale-documentation failure the guard exists to prevent.
//
// The probe uses an action no capability accepts. A name that reaches
// a handler is rejected by that handler ("unknown action"); a name
// that never reaches one is rejected by the router ("unknown tool").
// The distinction is what this test asserts.
func TestCallHelp_CallRoutedCatalogueIsDispatchable(t *testing.T) {
	routed := tools.CallRoutedTools()
	if len(routed) == 0 {
		t.Fatal("the call-routed routing table is empty; mpm_help must be reachable through the CLI fallback")
	}

	for _, capability := range routed {
		stdout, stderr, err := callHelpMPM(t, nil, "",
			"call", capability.Name, "--payload", `{"action":"__no_such_action__"}`)
		if err == nil {
			t.Fatalf("%s accepted an action it does not implement\nstdout=%s", capability.Name, stdout)
		}
		env := decodeHelpEnvelope(t, stdout)
		if env["success"] != false {
			t.Fatalf("%s: expected success=false, got %v\nstdout=%s", capability.Name, env["success"], stdout)
		}
		errText, _ := env["error"].(string)
		if strings.Contains(errText, "unknown tool") {
			t.Errorf("%s is in the call-routed routing table but the CLI rejected it as an unknown tool: %s", capability.Name, errText)
		}
		if !strings.Contains(errText, "unknown action") {
			t.Errorf("%s did not reach its handler (want an action rejection, got %q)\nstderr=%s", capability.Name, errText, stderr)
		}
	}
}
