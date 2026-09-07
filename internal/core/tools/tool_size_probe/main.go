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
// Keep in sync — drift here misrepresents the actual filtered surface.
var defaultCoreTools = map[string]bool{
	"mpm_memory":     true,
	"mpm_context":    true,
	"mpm_handoff":    true,
	"mpm_scratchpad": true,
	"mpm_help":       true,
}

// mpmHelpDescription is the description as registered via the
// cmd/mpm-mcp closure (NOT in tools.Registry — kept here only so
// the probe can measure it).
const mpmHelpDescription = "Capability discovery. Returns every registered MPM tool name with a terse one-liner. Use when: an agent wants to know what specialist tools are reachable but not initially exposed. Specialists are reachable via the host shell: mpm call <tool> --payload. Use action=list for the full catalogue; action=show with params.tool=<name> for one tool."

// mpmHelpSchema is the schema as registered via the cmd/mpm-mcp
// closure (mcp.WithString / mcp.WithDescription). We compute its
// byte length by serialising to a comparable JSON shape.
const mpmHelpSchema = `{"type":"object","properties":{"action":{"type":"string","description":"list or show."},"tool":{"type":"string","description":"Required for action=show. The tool name to inspect."}},"required":["action"]}`

// compactCoreToolSpecs is the actual MCP wire surface for the four
// non-help core tools. The full schemas remain in tools.Registry
// (used by CLI / substrate); the MCP server registers these compact
// versions via closures. Keep in sync with cmd/mpm-mcp/tools.go.
var compactCoreToolSpecs = []struct {
	name, description, schema string
}{
	{
		"mpm_memory",
		"Persistent memory. Save facts/learnings; query by text/id; show one row; shred (hard delete); reinforce/weaken; snooze/promote; patch metadata; review. Required params: action. For broad queries projection defaults to summary (256 chars + pointer); use projection=full or mpm_resolve for unabridged content. shred is permanent — no restore path. Call mpm_help show mpm_memory for the full schema.",
		`{"type":"object","properties":{"action":{"type":"string","description":"save | query | show | shred | reinforce | weaken | snooze | patch | promote."},"params":{"type":"object","description":"Per-action params. Common keys: fact (string), query (string), id (string), memory_id (string), tags (string[]), weight (number), limit (number), projection (summary|full), scope (all|local|shared), ttl (duration like 7d/24h). See tools/full-schema/mpm_memory.json via mpm_help for the complete contract."}},"required":["action"]}`,
	},
	{
		"mpm_context",
		"Session context. read_wake_context (browses recent memories, handoffs, overdue wakes); read_directives; proactive_recall_hint (suggests context-relevant memories); route (auto-selects mode/persona); query_global_rules / record_global_rule. Required params: action. For read_wake_context use projection=compact for ~130 token bounded output.",
		`{"type":"object","properties":{"action":{"type":"string","description":"read_wake_context | read_directives | proactive_recall_hint | query_global_rules | record_global_rule | route."},"params":{"type":"object","description":"Per-action params. read_wake_context accepts projection: compact | full. route accepts prompt (string). See tools/full-schema/mpm_context.json via mpm_help."}},"required":["action"]}`,
	},
	{
		"mpm_handoff",
		"Session handoff. write: persist summary at session end (REQUIRED: summary; optional commitments, open_questions, state). read: fetch handoff for next session. list/shred: manage the handoff ledger. summary must be non-empty; chat acks are NOT session-close events. Required params: action.",
		`{"type":"object","properties":{"action":{"type":"string","description":"write | read | list | shred."},"params":{"type":"object","description":"Per-action params. write accepts summary (string, required), state (clean|crashed|interrupted|force_end), commitments (string[]), open_questions (string[]). read/list accept session_id (string)."}},"required":["action"]}`,
	},
	{
		"mpm_scratchpad",
		"Volatile within-session working state. flush: write a partial thought; read: list active scratchpads; discard: drop one; promote: turn a scratchpad into a permanent memory (mpm_memory save). Not cross-session — promoted items become memories; unpromoted items are lost at session end. Required params: action.",
		`{"type":"object","properties":{"action":{"type":"string","description":"flush | read | discard | promote."},"params":{"type":"object","description":"flush accepts thesis (string, required), supporting (string). read/discard/promote accept id (string)."}},"required":["action"]}`,
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
