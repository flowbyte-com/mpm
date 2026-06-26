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
	"mpm/internal"
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
func ActiveContextFromEnv() internal.ActiveContext {
	return internal.ActiveContext{
		Mode:    os.Getenv("MPM_ACTIVE_MODE"),
		Persona: os.Getenv("MPM_ACTIVE_PERSONA"),
	}
}