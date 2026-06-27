// Package tools is the single source of truth for the MPM tool surface.
//
// Every MPM tool (memory write, recall, lessons, decisions, theories,
// evidence, GC, session handoff, etc.) is registered exactly once in
// this package. Both `mpm call <tool>` (CLI) and the MCP server
// (cmd/mpm-mcp) iterate the same Registry — adding a new tool requires
// touching exactly one file.
//
// Design notes (post-2026-06-26 review):
//
//   - No reflection. Handler signature is explicit: takes the
//     DatabaseManager + ActiveContext + payload map, returns
//     (interface{}, error). Both the CLI and the MCP server call the
//     same function. MCP just wraps the result in *mcp.CallToolResult.
//
//   - Schema is json.RawMessage. Both surfaces can read it: the CLI
//     uses it for `--help`-style introspection (future), the MCP server
//     passes it via NewToolWithRawSchema. Hand-writing the JSON schema
//     is verbose but explicit; the alternative (MCP's WithString/WithNumber
//     builders) requires a separate translation step that adds
//     maintenance for no behavior win.
//
//   - Registry is a package-level slice (not a map). Linear scan is fine
//     for 33 tools; explicit ordering makes the file readable. If we
//     ever need faster lookup, a name→index map can be built at init.
package tools

import (
	"encoding/json"

	"mpm/internal"
)

// HandlerFunc is the canonical tool-execution signature. Both the CLI
// dispatcher and the MCP server use it directly.
//
// dm is the shared DatabaseManager. ac carries the active mode/persona
// that produced the call (CLI: read from package globals; MCP: passed at
// server construction). payload is the JSON-unmarshalled arguments map.
//
// Return value semantics:
//   - (result, nil): success; result is JSON-marshalled for both surfaces.
//   - (nil, err): failure; CLI exits 1 with the error message; MCP returns
//     mcp.NewToolResultErrorFromErr(...).
type HandlerFunc func(dm *internal.DatabaseManager, ac internal.ActiveContext, payload map[string]interface{}) (interface{}, error)

// Tool is one entry in the Registry. The struct is intentionally flat —
// no nested config, no description vs long_description vs hints. Every
// field is required and self-explanatory.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Handler     HandlerFunc
}
//go:generate go run ../../cmd/gen-readme
