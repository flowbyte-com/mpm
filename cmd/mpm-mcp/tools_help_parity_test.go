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
)

// buildMPMBinForHelpParity builds the mpm CLI binary once per test run.
//
// The CLI is built from the repository root with the repository's
// required FTS5 configuration so the subprocess exercises the same
// binary shape the release gate installs.
func buildMPMBinForHelpParity(t *testing.T) string {
	t.Helper()
	helpParityBinOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "mpm-help-parity-bin-")
		if err != nil {
			helpParityBinErr = err
			return
		}
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

// toString renders a JSON-decoded value as a string for assertions.
func toString(v any) string {
	s, _ := v.(string)
	return s
}
