// tool_size_probe measures the byte size of each registered MCP tool's
// schema and description, as the MCP server would serialise them in
// `tools/list`. It is a standalone program that exists only for the
// context-economics measurement harness; it does NOT alter production
// behaviour and is excluded from the normal `make test` traversal (the
// internal/core/tools module has its own go.mod — see CLAUDE.md §3).
//
// Usage: go run ./internal/core/tools/tool_size_probe
//
// The probe also computes the filtered "default core" surface that
// mpm-mcp exposes when MPM_EXPOSE_ALL_TOOLS is unset (see
// docs/CONTEXT_EXPOSURE.md). The mpm_help discovery tool lives in
// cmd/mpm-mcp rather than tools.Registry (avoiding an init cycle),
// so its description and schema are hardcoded here for measurement.

package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/flowbyte-com/mpm-core/tools"
)

// defaultCoreTools mirrors cmd/mpm-mcp/main.go defaultCoreTools.
// 3-tool compact surface (post Sept-2026 final polish):
//   mpm_memory  persist/recall/show/shred (and reinforce/weaken/...)
//   mpm_context wake/directives/route/handoff (write/read)
//   mpm_help    capability discovery
// Keep in sync — drift here misrepresents the actual filtered surface.
var defaultCoreTools = map[string]bool{
	"mpm_memory":  true,
	"mpm_context": true,
	"mpm_help":    true,
}

// mpmHelpDescription is the description as registered via the
// cmd/mpm-mcp closure (NOT in tools.Registry — kept here only so
// the probe can measure it).
const mpmHelpDescription = "Capability discovery. action=list: every MPM tool name + reach_via_cli. action=show tool=<name>: full description. Specialists via 'mpm call <tool>'."

// mpmHelpSchema is the schema as registered via the cmd/mpm-mcp
// closure (mcp.WithString / mcp.WithDescription). We compute its
// byte length by serialising to a comparable JSON shape.
const mpmHelpSchema = `{"type":"object","properties":{"action":{"type":"string","description":"list or show."},"tool":{"type":"string","description":"Required for action=show."}},"required":["action"]}`

// compactCoreToolSpecs is the actual MCP wire surface for the
// non-help core tools. The full schemas remain in tools.Registry
// (used by CLI / substrate); the MCP server registers these compact
// versions via closures. Keep in sync with cmd/mpm-mcp/tools.go.
//
// 3-tool surface (post Sept-2026 final polish):
//   mpm_memory  persist/recall/show/shred
//   mpm_context wake/directives/route + write_handoff/read_handoff
//   mpm_help    capability discovery (registered separately)
var compactCoreToolSpecs = []struct {
	name, description, schema string
}{
	{
		"mpm_memory",
		"Persistent memory: save/query/show facts across sessions. Required: action. Use projection=summary (default, ~256 chars + pointer) for recall; projection=full for unabridged content. shred is permanent. Call mpm_help show mpm_memory for the full schema.",
		`{"type":"object","properties":{"action":{"type":"string","description":"save | query | show | shred | reinforce | weaken | snooze | patch | promote."},"params":{"type":"object","description":"save: fact (string). query: query (string), projection (summary|full), limit (number). show: id (string). shred: id (string). reinforce/weaken: id (string), delta (number). See mpm_help."}},"required":["action"]}`,
	},
	{
		"mpm_context",
		"Session state: read_wake_context, write_handoff, read_handoff, read_directives, route. Required: action. Use projection=compact (~130 tokens) for read_wake_context. write_handoff requires summary; chat acks are NOT session-close events.",
		`{"type":"object","properties":{"action":{"type":"string","description":"read_wake_context | write_handoff | read_handoff | read_directives | proactive_recall_hint | query_global_rules | record_global_rule | route."},"params":{"type":"object","description":"read_wake_context: projection (compact|full). write_handoff: summary (string, required). read_handoff: session_id (string). route: prompt (string)."}},"required":["action"]}`,
	},
}

func main() {
	// Walk the Registry slice (defined in registry_list.go). Each entry is
	// Tool{Name, Description, Schema}; Schema is a json.RawMessage that the
	// MCP server embeds verbatim into the tools/list wire response. We
	// measure both fields so the measurement reflects what actually
	// reaches the model boundary.
	type entry struct {
		name        string
		descBytes   int
		schemaBytes int
		total       int
	}
	var entries []entry
	totalDesc := 0
	totalSchema := 0
	for _, t := range tools.Registry {
		e := entry{
			name:        t.Name,
			descBytes:   len(t.Description),
			schemaBytes: len(t.Schema),
			total:       len(t.Description) + len(t.Schema),
		}
		totalDesc += e.descBytes
		totalSchema += e.schemaBytes
		entries = append(entries, e)
	}

	// Inject mpm_help — registered via cmd/mpm-mcp closure, NOT in
	// tools.Registry (init cycle avoidance). Hardcoded above to keep
	// the probe honest about the actual filtered surface.
	entries = append(entries, entry{
		name:        "mpm_help",
		descBytes:   len(mpmHelpDescription),
		schemaBytes: len(mpmHelpSchema),
		total:       len(mpmHelpDescription) + len(mpmHelpSchema),
	})
	totalDesc += len(mpmHelpDescription)
	totalSchema += len(mpmHelpSchema)

	// Build the filtered core surface using the COMPACT specs
	// (registered via cmd/mpm-mcp closure), NOT the full Registry
	// entries. This mirrors what the MCP server actually exposes
	// after WithToolFilter + compact-tool-registration.
	coreEntries := make([]entry, 0, len(compactCoreToolSpecs)+1)
	coreEntries = append(coreEntries, entry{
		name:        "mpm_help",
		descBytes:   len(mpmHelpDescription),
		schemaBytes: len(mpmHelpSchema),
		total:       len(mpmHelpDescription) + len(mpmHelpSchema),
	})
	var coreDesc, coreSchema int
	for _, e := range coreEntries {
		coreDesc += e.descBytes
		coreSchema += e.schemaBytes
	}
	for _, s := range compactCoreToolSpecs {
		e := entry{
			name:        s.name,
			descBytes:   len(s.description),
			schemaBytes: len(s.schema),
			total:       len(s.description) + len(s.schema),
		}
		coreEntries = append(coreEntries, e)
		coreDesc += e.descBytes
		coreSchema += e.schemaBytes
	}

	// Compute the actual JSON wire payload the MCP server would emit
	// for the filtered surface. Each entry has description + JSON schema
	// (built via the same mcp.NewToolWithRawSchema / mcp.NewTool
	// marshalling the MCP library performs).
	coreWire := map[string]map[string]interface{}{}
	coreWire["mpm_help"] = map[string]interface{}{
		"description": mpmHelpDescription,
		"schema":      json.RawMessage(mpmHelpSchema),
	}
	for _, s := range compactCoreToolSpecs {
		coreWire[s.name] = map[string]interface{}{
			"description": s.description,
			"schema":      json.RawMessage(s.schema),
		}
	}
	coreBytes, _ := json.MarshalIndent(coreWire, "", "  ")
	fmt.Printf("\n=== DEFAULT CORE (filtered + compact) ===\n")
	fmt.Printf("tools: %d (filtered from %d)\n", len(coreEntries), len(entries))
	fmt.Printf("desc+schema bytes: %d\n", coreDesc+coreSchema)
	fmt.Printf("approximate JSON wire payload: %d bytes / ~%d cl100k tokens\n",
		len(coreBytes), len(coreBytes)/4)

	// Per-tool listing for the filtered set.
	sort.Slice(coreEntries, func(i, j int) bool { return coreEntries[i].name < coreEntries[j].name })
	fmt.Printf("\n%-25s  %10s  %10s  %10s\n", "tool", "desc_bytes", "schema_bytes", "total")
	fmt.Println(repeat("-", 60))
	for _, e := range coreEntries {
		fmt.Printf("%-25s  %10d  %10d  %10d\n", e.name, e.descBytes, e.schemaBytes, e.total)
	}
	fmt.Println(repeat("-", 60))
	fmt.Printf("%-25s  %10d  %10d  %10d\n", "TOTAL (core)", coreDesc, coreSchema, coreDesc+coreSchema)

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})

	nameW := 0
	for _, e := range entries {
		if len(e.name) > nameW {
			nameW = len(e.name)
		}
	}
	fmt.Printf("%-*s  %10s  %10s  %10s\n", nameW, "tool", "desc_bytes", "schema_bytes", "total")
	fmt.Println(repeat("-", nameW+34))
	for _, e := range entries {
		fmt.Printf("%-*s  %10d  %10d  %10d\n", nameW, e.name, e.descBytes, e.schemaBytes, e.total)
	}
	fmt.Println(repeat("-", nameW+34))
	fmt.Printf("%-*s  %10d  %10d  %10d\n", nameW, "TOTAL", totalDesc, totalSchema, totalDesc+totalSchema)

	// Also serialise a representative wire-frame (object per tool with
	// description + schema) so the measurement reflects one realistic
	// tools/list JSON body.
	wire := struct {
		Tools map[string]map[string]interface{} `json:"tools"`
	}{Tools: map[string]map[string]interface{}{}}
	for _, t := range tools.Registry {
		wire.Tools[t.Name] = map[string]interface{}{
			"description": t.Description,
			"schema":      json.RawMessage(t.Schema),
		}
	}
	b, _ := json.MarshalIndent(wire, "", "  ")
	fmt.Printf("\ntools/list payload (JSON-serialised): %d bytes\n", len(b))
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// descFor returns the registered tool description for the wire-frame
// build. mpm_help is special: registered via closure, so we read from
// the constant.
func descFor(name string) string {
	if name == "mpm_help" {
		return mpmHelpDescription
	}
	t, ok := tools.ByName(name)
	if !ok {
		return ""
	}
	return t.Description
}
