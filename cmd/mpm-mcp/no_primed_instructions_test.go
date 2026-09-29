// no_primed_instructions_test.go — regression guard for the 2026-09-29
// removal of MCP `initialize.instructions`. The behavioural contract is
// the same one the old drift test pinned, but at the externally
// observable boundary: when a client sends a JSON-RPC `initialize`
// request, the response MUST NOT carry any MPM primer content in
// `result.instructions`.
//
// The 2026-09-29 simplification removed the `instructionsPrimer` symbol
// entirely (no `//go:embed`, no `WithInstructions` call, no
// `cmd/mpm-mcp/instructions_primer.txt` file). The drift test that
// pinned the old contract is gone with the file. This test is its
// replacement: it pins the *absence* of the primer at the MCP boundary.
//
// **Brittleness notice.** This test is INTENTIONALLY brittle on the
// observable outcome. A failure here means the production MCP
// initialize response carries MPM instruction text — which is the
// contract the operator asked us to remove. The test exercises the
// production option set via `buildMCPServerCore()`, the exact
// constructor main() uses for its MCP server. A future contributor
// who re-adds `server.WithInstructions(...)` to `buildMCPServerCore`
// (or to main() in a way that bypasses `buildMCPServerCore`) is
// caught here.
//
// A small filesystem guard (TestNoPrimedInstructions_NoInstructionsPrimerFile)
// supplements the behavioural test: it asserts the embedded artefact
// file is gone, so a second `//go:embed` of any primer is also caught.

package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// forbiddenPrimerSubstrings are phrases that, if they appeared in the
// MCP `initialize.instructions` response, would indicate the primer
// path has been silently re-introduced. The list is anchored to the
// pre-2026-09-29 primer's stable strings so a partial regression
// (someone embeds half of the primer) is caught as well.
//
// Kept short and stable — every entry is a phrase the old primer
// contained verbatim. Adding an entry is a deliberate decision; the
// default position is "no MPM instruction text in initialize".
var forbiddenPrimerSubstrings = []string{
	// Header stable opening (per the old primer's first line).
	"MPM behavioral contract loaded",
	// Compact-surface claim (locked in 2026-09-08).
	"3-tool compact MCP surface",
	// Wake envelope pointer (per old §1.2 of the managed block).
	"`<contextual_focus>`",
	// Diagnostic surface distinction (per old §1.2).
	"contextual_candidates",
	// CLI fallback footer (per the old primer's last paragraph).
	"fall back to `mpm call",
}

// TestNoPrimedInstructions_InitializeResponseEmpty is the load-bearing
// regression guard. It walks the production option set
// (`buildMCPServerCore()`, the exact constructor main() uses) and
// sends a JSON-RPC `initialize` request. The `result.instructions`
// field MUST be empty and MUST NOT contain any of the forbidden
// primer substrings.
//
// If `main.go` or `buildMCPServerCore` ever reintroduces
// `server.WithInstructions(...)` or otherwise ships MPM instruction
// text via Initialize, this fails.
func TestNoPrimedInstructions_InitializeResponseEmpty(t *testing.T) {
	// Exercise the production option set used by main(). If a new
	// option is added here that injects text into the initialize
	// response, this test fails. (main()'s buildMCPServer wraps
	// buildMCPServerCore with RegisterAllTools; tool registration
	// does not affect the initialize response.)
	s := buildMCPServerCore()

	message := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      mcp.NewRequestId(int64(1)),
		Request: mcp.Request{
			Method: "initialize",
		},
	}
	messageBytes, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal initialize request: %v", err)
	}

	response := s.HandleMessage(context.Background(), messageBytes)
	resp, ok := response.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("initialize response was not a JSONRPCResponse: %T", response)
	}
	initResult, ok := resp.Result.(mcp.InitializeResult)
	if !ok {
		t.Fatalf("initialize result was not an InitializeResult: %T", resp.Result)
	}

	if initResult.Instructions != "" {
		t.Errorf(
			"MCP `initialize.instructions` must be empty (post-2026-09-29 "+
				"contract: behavioural contract lives in the per-host managed "+
				"instruction file, not in initialize.instructions). Got %d bytes:\n%s",
			len(initResult.Instructions),
			initResult.Instructions,
		)
	}

	// Defensive: even if Instructions is non-empty, the prohibited
	// primer substrings must not appear. This catches a future
	// contributor who uses the field for an unrelated purpose (a
	// non-MCP note is fine; an MPM behavioural primer is not).
	for _, sub := range forbiddenPrimerSubstrings {
		if strings.Contains(initResult.Instructions, sub) {
			t.Errorf(
				"MCP `initialize.instructions` contains forbidden primer "+
					"substring %q — the MCP `instructions` field must not carry "+
					"MPM behavioural content. Full response:\n%s",
				sub, initResult.Instructions,
			)
		}
	}
}

// TestNoPrimedInstructions_NoInstructionsPrimerFile verifies the
// embedded artefact is gone. The file is the source of truth that the
// Go binary would `//go:embed` into the instruction string; its
// re-introduction is a strong indicator the primer path has been
// silently revived.
//
// This is a filesystem check, not a source check. A future contributor
// who reintroduces `cmd/mpm-mcp/instructions_primer.txt` will fail
// here even if they hide the `//go:embed` somewhere else.
func TestNoPrimedInstructions_NoInstructionsPrimerFile(t *testing.T) {
	// The file is referenced by the old path; we check relative to
	// the test file's directory (cmd/mpm-mcp/).
	const primerPath = "instructions_primer.txt"
	if _, err := os.Stat(primerPath); err == nil {
		t.Errorf(
			"%s must not exist — the MCP `initialize.instructions` "+
				"primer is removed; the file is the embedded artefact and "+
				"its re-introduction reverses the 2026-09-29 contract change",
			primerPath,
		)
	}
}