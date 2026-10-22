// tools_help_parity_test.go — native MCP vs `mpm call mpm_help` parity.
//
// mpm_help is the one capability reachable through two transports: the
// native MCP tool and the `mpm call` CLI fallback. Both serve the same
// catalogue (tools.Help), so an agent cannot discover a conflicting
// schema depending on which transport it used.
//
// This test compares the two transports' actual output, not the shared
// implementation: the native handler is invoked through the MCP
// request/result envelope, and the CLI is executed as a subprocess.
// The comparison is semantic — the payloads must be equal — while
// allowing for the legitimate envelope differences between an MCP tool
// result and a CLI stdout envelope.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

var (
	helpParityBinOnce sync.Once
	helpParityBin     string
	helpParityBinErr  error

	// helpParityBuildDir is the temporary directory the shared parity
	// binary is built into.
	//
	// It is a package-level value because the binary has to OUTLIVE the
	// test that built it: several tests reuse one build, so no single
	// test may delete it. A t.TempDir() would be exactly the wrong
	// owner — its cleanup runs when THAT test ends, taking the binary
	// away from the tests still to come. The package owns the directory
	// instead, and package teardown (TestMain) removes it.
	helpParityBuildDir string
)

// TestMain owns the package's temporary build directory.
//
// The parity tests build one shared CLI binary into a temporary
// directory. That directory used to be created with os.MkdirTemp and
// never removed — the same F1 leak class the repository documents
// elsewhere — so every run of this package left a built binary behind
// in the system temporary directory.
//
// The removal lives here, at package teardown, for two reasons:
//
//   - it runs after EVERY test has finished, whether they passed or
//     failed, so an ordinary test failure cannot leak the directory;
//   - it does not run while any test still needs the binary, which a
//     per-test cleanup would.
//
// A cleanup failure is reported on stderr and turns a passing run into
// a failing one. A leaked directory is a real defect, and a silent
// cleanup failure would let it return unnoticed.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := removeHelpParityBuildDir(helpParityBuildDir); err != nil {
		fmt.Fprintf(os.Stderr,
			"mpm-mcp: could not remove the temporary parity build directory %q: %v\n",
			helpParityBuildDir, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// removeHelpParityBuildDir removes the temporary directory the parity
// binary was built into.
//
// It is deliberately tolerant of "there was nothing to remove": the
// build directory is created lazily, so a run that never builds the
// binary has no directory, and a directory that was already removed
// (by an operator cleaning the temporary directory) must not be
// reported as a failure. An empty path is the same case.
//
// A failure that is NOT "already gone" is returned. Swallowing it would
// make a leaked directory invisible: the run would be green and the
// directory would still be on disk.
func removeHelpParityBuildDir(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// buildMPMBinForHelpParity builds the mpm CLI binary once per test run.
//
// The CLI is built from the repository root with the repository's
// required FTS5 configuration so the subprocess exercises the same
// binary shape the release gate installs.
//
// The binary is shared by every test in this file, so it is built once
// (sync.Once) and the temporary directory it lives in is removed at
// package teardown rather than at the end of any one test.
func buildMPMBinForHelpParity(t *testing.T) string {
	t.Helper()
	helpParityBinOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "mpm-help-parity-bin-")
		if err != nil {
			helpParityBinErr = err
			return
		}
		// The directory is recorded the moment it exists and BEFORE the
		// build is attempted. A build that fails part-way through still
		// leaves a directory behind, and package teardown can only
		// remove what it was told about.
		helpParityBuildDir = tmp
		bin := filepath.Join(tmp, "mpm")
		cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, "./cmd/mpm")
		pkgDir, err := os.Getwd()
		if err != nil {
			helpParityBinErr = err
			return
		}
		cmd.Dir = filepath.Dir(filepath.Dir(pkgDir))
		cmd.Env = append(os.Environ(),
			"CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1",
			"CGO_LDFLAGS=-lm",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			helpParityBinErr = err
			t.Logf("go build output:\n%s", out)
			return
		}
		helpParityBin = bin
	})
	if helpParityBinErr != nil {
		t.Fatalf("build mpm CLI for parity test: %v", helpParityBinErr)
	}
	return helpParityBin
}

// nativeHelp invokes the native MCP mpm_help handler and returns the
// JSON payload it produced, or the error text when the handler
// returned an error result.
func nativeHelp(t *testing.T, args map[string]any) (payload map[string]any, errText string) {
	t.Helper()
	h := makeHelpHandler(nil, core.ActiveContext{})
	req := mcp.CallToolRequest{}
	req.Params.Name = "mpm_help"
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("native mpm_help returned a transport error: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("native mpm_help returned no content")
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("native mpm_help content is %T, want mcp.TextContent", res.Content[0])
	}
	if res.IsError {
		return nil, text.Text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("native mpm_help payload is not JSON: %v\n%s", err, text.Text)
	}
	return out, ""
}

// cliHelp invokes `mpm call mpm_help` as a subprocess against an
// isolated HOME and returns the parsed stdout envelope plus the exit
// code.
func cliHelp(t *testing.T, bin, payload string) (env map[string]any, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, "call", "mpm_help", "--payload", payload)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"MPM_SCHEDULER_DISABLED=1",
	}
	out, err := cmd.Output()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("mpm call mpm_help failed: %v", err)
		}
		code = ee.ExitCode()
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &env); err != nil {
		t.Fatalf("CLI stdout is not a JSON envelope: %v\n%s", err, out)
	}
	return env, code
}

// TestHelpHandler_ParityWithCLIList pins that the native MCP surface
// and the CLI fallback report the same catalogue.
func TestHelpHandler_ParityWithCLIList(t *testing.T) {
	bin := buildMPMBinForHelpParity(t)

	native, errText := nativeHelp(t, map[string]any{"action": "list"})
	if errText != "" {
		t.Fatalf("native mpm_help list failed: %s", errText)
	}
	cli, exitCode := cliHelp(t, bin, `{"action":"list"}`)
	if exitCode != 0 {
		t.Fatalf("CLI mpm_help list exited %d, want 0", exitCode)
	}

	if native["success"] != true || cli["success"] != true {
		t.Errorf("success flags differ: native=%v cli=%v", native["success"], cli["success"])
	}
	if native["tool_count"] != cli["tool_count"] {
		t.Errorf("tool_count differs: native=%v cli=%v", native["tool_count"], cli["tool_count"])
	}
	if len(tools.Registry) != 0 {
		// The catalogue is the registry; both transports must report it
		// in full.
		nativeRows, _ := native["tools"].([]any)
		cliRows, _ := cli["tools"].([]any)
		if len(nativeRows) != len(tools.Registry) {
			t.Errorf("native lists %d tools, want %d", len(nativeRows), len(tools.Registry))
		}
		if len(cliRows) != len(tools.Registry) {
			t.Errorf("CLI lists %d tools, want %d", len(cliRows), len(tools.Registry))
		}
	}

	// The payloads must be identical, not merely similar: the same tool
	// names, the same one-liners and the same reachability strings.
	nativeJSON, _ := json.Marshal(native)
	cliJSON, _ := json.Marshal(cli)
	if string(nativeJSON) != string(cliJSON) {
		t.Errorf("native and CLI discovery payloads differ\nnative=%s\ncli=%s", nativeJSON, cliJSON)
	}
}

// TestHelpHandler_ParityWithCLIShow pins the same parity for the show
// action, including the schema the agent receives for a known tool.
func TestHelpHandler_ParityWithCLIShow(t *testing.T) {
	bin := buildMPMBinForHelpParity(t)

	native, errText := nativeHelp(t, map[string]any{"action": "show", "tool": "mpm_memory"})
	if errText != "" {
		t.Fatalf("native mpm_help show failed: %s", errText)
	}
	cli, exitCode := cliHelp(t, bin, `{"action":"show","params":{"tool":"mpm_memory"}}`)
	if exitCode != 0 {
		t.Fatalf("CLI mpm_help show exited %d, want 0", exitCode)
	}

	nativeJSON, _ := json.Marshal(native)
	cliJSON, _ := json.Marshal(cli)
	if string(nativeJSON) != string(cliJSON) {
		t.Errorf("native and CLI show payloads differ\nnative=%s\ncli=%s", nativeJSON, cliJSON)
	}
	if native["description"] == "" || native["description"] == nil {
		t.Error("show must return the tool's full description")
	}
	if !strings.Contains(toString(native["reach_via_cli"]), "mpm call mpm_memory") {
		t.Errorf("reach_via_cli = %v, want it to name the mpm call form", native["reach_via_cli"])
	}
}

// TestHelpHandler_ShowSchemaParityWithCLI pins that both transports
// return the tool's registered JSON input schema as a structured JSON
// object, and that the object is the registry's own schema.
//
// This is the check that matters for an agent acting on the result: it
// must be able to read the action vocabulary, the required fields and
// the nested parameter properties out of either transport's answer. A
// schema serialised as an escaped JSON string would force the reader to
// unescape and re-parse it — and the two transports could then drift in
// how that string is shaped without any test noticing.
//
// The comparison is against the registry definition, not against a
// hand-written expectation, so a second copy of the schema cannot
// quietly diverge from the one the dispatcher validates against.
func TestHelpHandler_ShowSchemaParityWithCLI(t *testing.T) {
	bin := buildMPMBinForHelpParity(t)

	native, errText := nativeHelp(t, map[string]any{"action": "show", "tool": "mpm_memory"})
	if errText != "" {
		t.Fatalf("native mpm_help show failed: %s", errText)
	}
	cli, exitCode := cliHelp(t, bin, `{"action":"show","params":{"tool":"mpm_memory"}}`)
	if exitCode != 0 {
		t.Fatalf("CLI mpm_help show exited %d, want 0", exitCode)
	}

	nativeSchema, ok := native["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("native input_schema is %T, want a JSON object", native["input_schema"])
	}
	cliSchema, ok := cli["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("CLI input_schema is %T, want a JSON object", cli["input_schema"])
	}

	nativeJSON, _ := json.Marshal(nativeSchema)
	cliJSON, _ := json.Marshal(cliSchema)
	if string(nativeJSON) != string(cliJSON) {
		t.Errorf("native and CLI schemas differ\nnative=%s\ncli=%s", nativeJSON, cliJSON)
	}

	registered, ok := tools.ByName("mpm_memory")
	if !ok {
		t.Fatal("mpm_memory is not registered")
	}
	var want map[string]any
	if err := json.Unmarshal(registered.Schema, &want); err != nil {
		t.Fatalf("registry schema for mpm_memory is not a JSON object: %v", err)
	}
	wantJSON, _ := json.Marshal(want)
	if string(nativeJSON) != string(wantJSON) {
		t.Errorf("native schema differs from the registry schema\n got: %s\nwant: %s", nativeJSON, wantJSON)
	}

	// The structure that makes the schema actionable must survive the
	// transport: the action vocabulary, the required fields, and the
	// nested parameter properties.
	props, _ := nativeSchema["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	if enum, _ := action["enum"].([]any); len(enum) == 0 {
		t.Error("native schema has no action enum")
	}
	if required, _ := nativeSchema["required"].([]any); len(required) == 0 {
		t.Error("native schema has no required fields")
	}
	params, _ := props["params"].(map[string]any)
	if nested, _ := params["properties"].(map[string]any); len(nested) == 0 {
		t.Error("native schema has no nested params properties")
	}
}

// TestHelpHandler_ParityWithCLIError pins that both transports report
// the same reason for a rejected request.
//
// The envelopes differ legitimately — the native transport returns an
// MCP error result, the CLI returns {"success":false,"error":...} and
// a non-zero exit — but the reason itself must be identical.
func TestHelpHandler_ParityWithCLIError(t *testing.T) {
	bin := buildMPMBinForHelpParity(t)

	_, nativeErr := nativeHelp(t, map[string]any{"action": "enlist"})
	if nativeErr == "" {
		t.Fatal("native mpm_help accepted an unknown action")
	}
	cli, exitCode := cliHelp(t, bin, `{"action":"enlist"}`)
	if exitCode == 0 {
		t.Error("CLI mpm_help accepted an unknown action (exit 0)")
	}
	cliErr := toString(cli["error"])
	if cliErr != nativeErr {
		t.Errorf("error text differs:\nnative=%q\ncli=%q", nativeErr, cliErr)
	}
	if cli["success"] != false {
		t.Errorf("CLI success = %v, want false", cli["success"])
	}
}

// TestHelpHandler_NotARegistryEntry pins that mpm_help remains a
// closure rather than a registry entry.
//
// A registry entry would double-register the tool under
// MPM_EXPOSE_ALL_TOOLS=1 (where the full surface is exposed alongside
// the compact one) and would change the CLI's "unknown tool: ...
// available: ..." list and `mpm help --all`. The CLI reaches mpm_help
// by explicit routing instead.
func TestHelpHandler_NotARegistryEntry(t *testing.T) {
	for _, name := range tools.Names() {
		if name == "mpm_help" {
			t.Error("mpm_help must not be a registry entry; it is registered as a native MCP closure")
		}
	}
	if _, ok := tools.ByName("mpm_help"); ok {
		t.Error("tools.ByName(\"mpm_help\") must not resolve")
	}
}

// TestHelpParity_BuildDirectoryIsTrackedForTeardown pins that the
// directory package teardown removes is the one the shared binary
// actually lives in.
//
// The binary is shared: several tests reuse one build, so no test may
// delete it — but the package must still remove the directory it left
// behind. The package can only do that if it knows where the directory
// is. If the build stopped recording it, teardown would silently remove
// nothing and the leak would return unnoticed.
func TestHelpParity_BuildDirectoryIsTrackedForTeardown(t *testing.T) {
	bin := buildMPMBinForHelpParity(t)

	if helpParityBuildDir == "" {
		t.Fatal("the parity build directory was not recorded; package teardown would have nothing to remove")
	}
	if !strings.HasPrefix(bin, helpParityBuildDir+string(os.PathSeparator)) {
		t.Errorf("the shared binary %q is not inside the tracked build directory %q", bin, helpParityBuildDir)
	}
}

// TestHelpParity_CleanupRemovesTheBuildDirectory proves the cleanup
// helper removes what it is given, including the nested binary inside
// it.
//
// The directory is removed by the PACKAGE, so this test exercises the
// helper against a directory of its own. It deliberately does not touch
// the shared build directory: that binary is still required by the
// other tests in this file.
func TestHelpParity_CleanupRemovesTheBuildDirectory(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "nested", "mpm")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("create the build directory: %v", err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatalf("create the build artifact: %v", err)
	}

	if err := removeHelpParityBuildDir(dir); err != nil {
		t.Fatalf("cleanup of %q failed: %v", dir, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the build directory %q still exists after cleanup (stat error: %v)", dir, err)
	}
}

// TestHelpParity_CleanupIsANoOpWhenNothingWasBuilt proves cleanup
// tolerates the two "there was nothing to remove" cases.
//
// The build directory is created lazily, so a run that never builds the
// binary has no directory at all — and a directory an operator already
// cleaned away must not be reported as a failure. Either case would
// otherwise fail an otherwise-green test run for no reason.
func TestHelpParity_CleanupIsANoOpWhenNothingWasBuilt(t *testing.T) {
	if err := removeHelpParityBuildDir(""); err != nil {
		t.Errorf("cleanup of an empty path must be a no-op, got %v", err)
	}
	never := filepath.Join(t.TempDir(), "never-created")
	if err := removeHelpParityBuildDir(never); err != nil {
		t.Errorf("cleanup of a directory that was never created must be a no-op, got %v", err)
	}
}

// TestHelpParity_CleanupReportsAFailure proves the cleanup helper
// REPORTS a failure instead of swallowing it.
//
// A cleanup that cannot remove its directory leaves the leak in place.
// Swallowing the error would make that leak invisible: the run would be
// green and the directory would still be on disk.
//
// The realistic failure is a permission problem — a directory the
// owner cannot write to. That variant does not stop root from
// removing it, so this test uses the equivalent failure that behaves
// the same for every user: a path whose parent is a regular file,
// which cannot be removed by anybody.
func TestHelpParity_CleanupReportsAFailure(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("create the blocking file: %v", err)
	}

	err := removeHelpParityBuildDir(filepath.Join(blocker, "build-dir"))
	if err == nil {
		t.Error("cleanup must report a failure it could not complete")
	}
}

// toString renders a JSON-decoded value as a string for assertions.
func toString(v any) string {
	s, _ := v.(string)
	return s
}
