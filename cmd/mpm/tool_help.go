// cmd/mpm/tool_help.go — `mpm help <tool-name>` introspection.
//
// W-007 + W-009: the audit found that an agent had to invoke
// `mpm call mpm_foo` with a bogus payload just to learn the valid
// actions and required parameters. This file implements
// `mpm help <tool-name>` so both surfaces are discoverable from one
// command.
//
// The schema for the tool is the same hand-written JSON Schema that the
// dispatcher uses for runtime validation. We never hand-write a second
// registry or a second param list — the only way to drift is to remove
// the entry from tools.Registry, which would also break dispatch.
//
// Output for `mpm help mpm_memory`:
//
//   mpm help mpm_memory — Persistent memory for facts, ...
//
//   Usage:
//     mpm call mpm_memory --payload '<json>'
//     mpm-mcp (stdio) → tool name "mpm_memory"
//
//   Valid actions: ...
//
//   Parameters per action:
//
//     save
//       fact            string   required
//       collection      string   default "general"
//       weight          number   default 5.0
//       tags            array    default []
//       ...
//
//     query
//       query           string   required
//       limit           integer  default 10
//       ...
//
//   Full JSON schema (machine-readable):
//   { ... }
//
// The format is designed to be greppable by an agent — `mpm help
// mpm_memory | grep required` surfaces every required field in one
// pass, no trial-and-error needed.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/flowbyte-com/mpm-core/tools"
)

// paramDoc describes a single property in the JSON Schema, flattened
// for human reading.
type paramDoc struct {
	Name        string
	Type        string   // "string" | "integer" | "number" | "boolean" | "array" | "object"
	Required    bool
	Default     string   // string form, "" if absent
	Description string   // first sentence of the schema description
	EnumValues  []string // populated when the property has an enum constraint
}

// printToolHelp handles `mpm help <tool-name>` where <tool-name> matches
// a registered MCP tool. Returns true when help was printed so the
// caller can suppress the "no help available" fallback.
//
// Derives the action list and parameter docs from tools.Registry — the
// same slice the `mpm call <name>` dispatcher iterates. There is
// intentionally no parallel registry here.
func printToolHelp(toolName string) bool {
	tool, ok := tools.ByName(toolName)
	if !ok {
		return false
	}

	actions := extractActionEnum(tool.Schema)
	required := extractRequired(tool.Schema)
	props := extractProperties(tool.Schema)

	fmt.Printf("mpm help %s — %s\n\n", tool.Name, oneLine(tool.Description, 120))

	fmt.Println("Usage:")
	fmt.Printf("  mpm call %s --payload '<json>'\n", tool.Name)
	fmt.Printf("  mpm-mcp (stdio) → tool name %q\n\n", tool.Name)

	fmt.Println("Valid actions:")
	if len(actions) == 0 {
		fmt.Println("  (no actions enum declared; check the schema)")
	} else {
		for _, a := range actions {
			fmt.Printf("  - %s\n", a)
		}
	}

	fmt.Println()
	fmt.Println("Top-level parameters:")
	if len(props) == 0 {
		fmt.Println("  (no top-level properties)")
	} else {
		// Stable order so agent output is deterministic.
		names := make([]string, 0, len(props))
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := props[n]
			marker := "optional"
			if p.Required {
				marker = "required"
			}
			enumSuffix := ""
			if len(p.EnumValues) > 0 {
				enumSuffix = " enum=" + strings.Join(p.EnumValues, "|")
			}
			defaultSuffix := ""
			if p.Default != "" {
				defaultSuffix = " default=" + p.Default
			}
			fmt.Printf("  %-15s %-8s %s%s%s\n", p.Name, p.Type, marker, defaultSuffix, enumSuffix)
			if p.Description != "" {
				fmt.Printf("    %s\n", oneLine(p.Description, 100))
			}
		}
		// Surface required list explicitly — this is the bit an agent
		// most often needs. Keeping it visible at the bottom so a
		// quick scan still surfaces it.
		if len(required) > 0 {
			fmt.Println()
			fmt.Printf("Required: %s\n", strings.Join(required, ", "))
		}
	}

	fmt.Println()
	fmt.Println("Full JSON schema (machine-readable):")
	fmt.Println(string(prettyJSON(tool.Schema)))

	fmt.Println()
	fmt.Println("For human-readable examples, see: mpm help --all")
	return true
}

// extractActionEnum returns the `enum` array from the top-level
// `action` property of a JSON Schema document. Returns nil if absent.
func extractActionEnum(schema json.RawMessage) []string {
	var doc struct {
		Properties struct {
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil
	}
	return doc.Properties.Action.Enum
}

// extractRequired returns the top-level `required` array from the
// schema. These are the property names that must be present in every
// payload, regardless of action.
func extractRequired(schema json.RawMessage) []string {
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil
	}
	return doc.Required
}

// extractProperties flattens the schema's top-level properties into a
// map keyed by name. Each value carries the human-readable summary we
// emit in printToolHelp.
func extractProperties(schema json.RawMessage) map[string]paramDoc {
	var doc struct {
		Properties map[string]struct {
			Type        interface{} `json:"type"`
			Description string      `json:"description"`
			Default     interface{} `json:"default"`
			Enum        []string    `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil
	}
	if doc.Properties == nil {
		return nil
	}

	required := make(map[string]bool, len(doc.Required))
	for _, r := range doc.Required {
		required[r] = true
	}

	out := make(map[string]paramDoc, len(doc.Properties))
	for name, p := range doc.Properties {
		out[name] = paramDoc{
			Name:        name,
			Type:        schemaType(p.Type),
			Required:    required[name],
			Default:     formatDefault(p.Default),
			Description: p.Description,
			EnumValues:  p.Enum,
		}
	}
	return out
}

// schemaType normalises the JSON Schema `type` field into a short
// human-readable form. JSON Schema permits either a string ("string")
// or a list (["string","null"]); we collapse to the first concrete type.
func schemaType(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				return s
			}
		}
		return "any"
	default:
		return "any"
	}
}

// formatDefault renders a JSON Schema default value as a one-line
// string. Returns "" when no default is set so callers can suppress
// the field entirely.
func formatDefault(v interface{}) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return ""
		}
		return x
	case bool, float64:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// prettyJSON reformats raw JSON with 2-space indent. Falls back to the
// raw bytes on marshal error (shouldn't happen for well-formed input).
func prettyJSON(raw json.RawMessage) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// oneLine collapses a multi-line description into a single-line
// summary suitable for a header line. Truncates at maxLen runes.
func oneLine(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}