// Package mpmcli provides shared setup helpers for the mpm CLI and MCP
// binaries. The two entry points (cmd/mpm/main.go, cmd/mpm-mcp/main.go)
// were duplicating the same env-var reads and default-resolution logic;
// this package is the single source of truth.
//
// The package is intentionally tiny — it doesn't own the DatabaseManager
// (which is still created in the binary, since the CLI opens it lazily
// per-handler and the MCP server opens it once at boot).
package mpmcli

import (
	"github.com/flowbyte-com/mpm-core"
	"os"
)

// ResolveWorkspace reads MPM_WORKSPACE and falls back to "." if unset.
// Used by both the CLI and MCP server to find the workspace root.
func ResolveWorkspace() string {
	if w := os.Getenv("MPM_WORKSPACE"); w != "" {
		return w
	}
	return "."
}

// ActiveContextFromEnv reads MPM_ACTIVE_MODE / MPM_ACTIVE_PERSONA and
// returns the ActiveContext that write handlers will stamp onto rows.
// Both the CLI and the MCP server stamp these onto every memory/decision
// they create so the row carries provenance about which mode/persona
// produced it.
//
// MPM_SESSION_ID, when set, scopes every tool_invocations row the MCP
// server writes to that session. The drill orchestrator sets it before
// spawning a real framework harness (e.g. Claude Code) so the audit
// rows attributable to that drill can be queried together.
//
// MPM_FRAMEWORK, when set, identifies the calling agent framework
// (e.g. "openclaw", "opencode", "pi", "claude-code", "hermes"). The
// substrate uses this for wake-time directive scope filtering
// (ReadDirectivesForFramework in internal/core/directive_tools.go) so
// framework-scoped directives only reach their intended consumer.
// Unset defaults to "mcp" so existing single-MCP callers see no
// behaviour change — ReadDirectivesForFramework falls back to
// global-only when framework is the empty string, so the hardcoded
// default never accidentally surfaces a framework-scoped directive.
//
// Precedence: MPM_PROVENANCE_FRAMEWORK (canonical) is consulted
// before MPM_FRAMEWORK (legacy fallback). Mirrors the precedence used
// by internal/core/provenance.go (artifact-write channel) and
// cmd/mpm/call.go (call-handler channel). See docs/provenance-env.md.
//
// See docs/archive/directives.md §5 for the runtime transport
// contract.
func ActiveContextFromEnv() internal.ActiveContext {
	framework := os.Getenv("MPM_PROVENANCE_FRAMEWORK")
	if framework == "" {
		framework = os.Getenv("MPM_FRAMEWORK")
	}
	if framework == "" {
		framework = "mcp"
	}
	return internal.ActiveContext{
		Mode:          os.Getenv("MPM_ACTIVE_MODE"),
		Persona:       os.Getenv("MPM_ACTIVE_PERSONA"),
		SessionID:     os.Getenv("MPM_SESSION_ID"),
		FrameworkName: framework,
	}
}