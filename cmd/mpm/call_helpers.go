package main

// getString extracts a string value from a JSON-decoded payload map.
// Returns "" when the key is missing or holds a non-string type.
//
// Originally lived in cmd/mpm/switch.go as a utility for the persona
// switcher TUI. After that TUI was deleted (unreachable from any main
// entry point), getString stayed — call.go's evidence / confidence /
// changelog handlers depend on it. Kept here as the single home so
// future TUI work doesn't accidentally re-introduce a copy.
func getString(raw map[string]interface{}, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}