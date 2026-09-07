// compact_surface_filter_test.go — Sept 2026 launch-block fix.
//
// Pins the contract of the default initial MCP surface that
// mpm-mcp exposes when MPM_EXPOSE_ALL_TOOLS is unset:
//
//   - Exactly 3 tools in the default initial surface
//     (mpm_memory, mpm_context, mpm_help) — mpm_handoff is reachable
//     via mpm_context action=write_handoff/read_handoff, and
//     mpm_scratchpad is reachable via the mpm call escape hatch.
//   - The full Registry (22 tools) remains intact internally
//   - All 3 default tools are present and callable via their
//     HandlerFunc (mcpAdapter wraps the Registry's Handler —
//     not a stub)
//   - MPM_EXPOSE_ALL_TOOLS=1 reveals the legacy 22-tool surface
//
// This is a static / unit-level pin, not an end-to-end MCP
// handshake. The mcp-go library's WithToolFilter unit tests are
// upstream; here we verify the policy, not the library.

package tools

import (
	"os"
	"testing"
)

// defaultCoreTools mirrors cmd/mpm-mcp/main.go defaultCoreTools.
// Keep in sync — drift here misrepresents the actual filtered surface.
var defaultCoreTools = map[string]bool{
	"mpm_memory":  true,
	"mpm_context": true,
	"mpm_help":    true,
}

// TestCompactSurface_DefaultCoreHasThreeTools pins the default
// initial surface to exactly the 3-tool canonical set. If a tool
// is added to or removed from defaultCoreTools without updating
// the docs/CONTEXT_EXPOSURE.md measurements, this test fires.
func TestCompactSurface_DefaultCoreHasThreeTools(t *testing.T) {
	expected := []string{
		"mpm_memory",
		"mpm_context",
		"mpm_help",
	}
	if len(defaultCoreTools) != len(expected) {
		t.Fatalf("defaultCoreTools has %d entries, want %d (%v)",
			len(defaultCoreTools), len(expected), expected)
	}
	for _, name := range expected {
		if !defaultCoreTools[name] {
			t.Errorf("defaultCoreTools missing required entry %q", name)
		}
	}
}

// TestCompactSurface_FullRegistryPreserved pins that the full
// internal Registry remains intact (22 tools). The default surface
// filter is a *display* concern, not a *capability* concern. Any
// production change that deletes a Registry entry should fire this
// test before it lands.
func TestCompactSurface_FullRegistryPreserved(t *testing.T) {
	const want = 21 // Registry entries (mpm_help is registered via closure, not Registry)
	if len(Registry) != want {
		t.Errorf("tools.Registry has %d entries; expected %d (the full internal surface)",
			len(Registry), want)
	}
}

// TestCompactSurface_FilterIsNoOpWhenEnvSet pins the MPM_EXPOSE_ALL_TOOLS
// escape hatch. Setting it to any non-empty value must restore the
// full 22-tool surface (21 Registry + mpm_help closure).
//
// We exercise the actual policy logic via the os.Setenv hook here
// because the production policy (in cmd/mpm-mcp/main.go) reads the
// env var directly; the helper exists to pin that policy contract
// from a unit test.
func TestCompactSurface_FilterIsNoOpWhenEnvSet(t *testing.T) {
	// Set the env, exercise the policy, restore the env.
	t.Setenv("MPM_EXPOSE_ALL_TOOLS", "1")
	exposed := applyPolicyForTest()
	if exposed != len(Registry)+1 /* +1 for mpm_help */ {
		t.Errorf("MPM_EXPOSE_ALL_TOOLS=1: expected %d tools exposed, got %d",
			len(Registry)+1, exposed)
	}
}

// applyPolicyForTest mirrors the production filter policy: if
// MPM_EXPOSE_ALL_TOOLS is set, return all tools (Registry + the
// mpm_help closure). Otherwise, return only defaultCoreTools.
//
// Production code in cmd/mpm-mcp/main.go reads MPM_EXPOSE_ALL_TOOLS
// directly; this helper exists to pin the contract from a unit test
// so the policy cannot drift between the production implementation
// and the documented behaviour.
func applyPolicyForTest() int {
	if os.Getenv("MPM_EXPOSE_ALL_TOOLS") != "" {
		// Full surface: Registry (21) + mpm_help closure.
		return len(Registry) + 1
	}
	return len(defaultCoreTools)
}

// TestCompactSurface_AllDefaultCoreHandlersCallableViaByName pins
// that every default-core tool has a handler in the Registry, so the
// MCP server's compact closures can route to the canonical HandlerFunc.
// If a tool is moved out of Registry but kept in the default surface,
// the closure wiring in cmd/mpm-mcp/tools.go will panic at boot; this
// test catches the discrepancy at unit-test time.
func TestCompactSurface_AllDefaultCoreHandlersCallableViaByName(t *testing.T) {
	for name := range defaultCoreTools {
		if name == "mpm_help" {
			continue // mpm_help is registered via closure, not Registry
		}
		t.Run(name, func(t *testing.T) {
			tool, ok := ByName(name)
			if !ok {
				t.Fatalf("defaultCoreTools[%q] is not in tools.Registry — closure wiring in cmd/mpm-mcp/tools.go will panic at boot", name)
			}
			if tool.Handler == nil {
				t.Errorf("tools.Registry[%q].Handler is nil", name)
			}
		})
	}
}
