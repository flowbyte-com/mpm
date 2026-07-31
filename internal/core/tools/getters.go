package tools

import (
	"os"

	"github.com/flowbyte-com/mpm-core/config"
)

// getString extracts a string value from a JSON-decoded payload map.
// Returns "" when the key is missing or holds a non-string type.
func getString(raw map[string]interface{}, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}

// resolveRouteWorkspace returns the MPM workspace path for `mpm route`.
// Resolution order: MPM_ROUTE_WORKSPACE env var → config.GetMPMDir()
// (which honours MPM_WORKSPACE then falls back to $HOME/.mpm).
//
// Returning "." here is wrong — the MCP server is launched from an arbitrary
// cwd and ./mode does not resolve. The 2026-07-30 audit caught this; the
// canonical pattern is config.GetMPMDir() (see internal/core/config/config.go).
func resolveRouteWorkspace() string {
	if v := os.Getenv("MPM_ROUTE_WORKSPACE"); v != "" {
		return v
	}
	return config.GetMPMDir()
}
