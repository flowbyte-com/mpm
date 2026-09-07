// tool_size_probe measures the byte size of each registered MCP tool's
// schema and description, as the MCP server would serialise them in
// `tools/list`. It is a standalone program that exists only for the
// context-economics measurement harness; it does NOT alter production
// behaviour and is excluded from the normal `make test` traversal (the
// internal/core/tools module has its own go.mod — see CLAUDE.md §3).
//
// Usage: go run ./internal/core/tools/tool_size_probe

package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/flowbyte-com/mpm-core/tools"
)

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
