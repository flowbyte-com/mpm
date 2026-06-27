package tools

import "os"

// getString extracts a string value from a JSON-decoded payload map.
// Returns "" when the key is missing or holds a non-string type.
func getString(raw map[string]interface{}, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}

// resolveRouteWorkspace returns the MPM workspace path for `mpm route`.
// Resolution order: MPM_ROUTE_WORKSPACE env var → "." (current directory).
// The router inside will then load mode/ and persona/ from this base path.
func resolveRouteWorkspace() string {
	if v := os.Getenv("MPM_ROUTE_WORKSPACE"); v != "" {
		return v
	}
	return "."
}
