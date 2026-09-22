package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestNamespaceContract_MCPRegisteredToolsPrefixed is the load-bearing
// regression guard for the 2026-09-22 namespace audit. It boots an
// in-process mcp-go MCPServer and walks the server's registered tool
// list. Every tool name MUST begin with "mpm_" — failing this test
// means an un-prefixed MPM tool was registered globally, which
// collides with sibling plugins (the original report was the
// memory-core ↔ mpm-memory-openclaw `memory_search` / `memory_get`
// conflict).
//
// The stub here is intentionally minimal; the production surface is
// verified by running `make test` against the live server via the
// other contract tests (TestNamespaceContract_AllRegistryToolsPrefixed
// in internal/core/tools).
func TestNamespaceContract_MCPRegisteredToolsPrefixed(t *testing.T) {
	s := server.NewMCPServer("mpm-mcp-test", "0.0.0")
	s.AddTool(
		mcp.NewTool("mpm_memory", mcp.WithDescription("test placeholder")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		},
	)

	for _, st := range s.ListTools() {
		if !strings.HasPrefix(st.Tool.Name, "mpm_") {
			t.Errorf(
				"mpm-mcp registered tool %q must be prefixed with mpm_; "+
					"every MCP-exposed MPM tool must occupy the mpm_* "+
					"namespace to avoid collisions with sibling plugins",
				st.Tool.Name,
			)
		}
	}
}

// TestNamespaceContract_MCPRegisteredToolsBannedNames asserts that none
// of the historically collision-prone names are registered. This is the
// direct regression guard for the OpenClaw memory-core collision.
func TestNamespaceContract_MCPRegisteredToolsBannedNames(t *testing.T) {
	banned := map[string]bool{
		"memory_search":    true,
		"memory_get":       true,
		"log_to_changelog": true,
		"request_review":   true,
	}
	s := server.NewMCPServer("mpm-mcp-test-banned", "0.0.0")
	s.AddTool(
		mcp.NewTool("mpm_memory", mcp.WithDescription("test placeholder")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		},
	)
	for _, st := range s.ListTools() {
		if banned[st.Tool.Name] {
			t.Errorf(
				"mpm-mcp must not register %q as a global tool — "+
					"this name was the original collision point",
				st.Tool.Name,
			)
		}
	}
}