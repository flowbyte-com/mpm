package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// callSite represents one `callMpm(tool, payload)` or
// `callMpmTool(tool, payload, opts)` invocation in an adapter file.
// Used by TestAdapterCallsites_MatchGoSchema to verify the adapter
// uses only tool names + actions that exist in the live Registry.
type callSite struct {
	file    string
	line    int
	tool    string
	keys    []string // top-level keys of the literal payload (sorted); empty when literal=false
	action  string   // populated when the literal payload includes action: "foo"; empty otherwise
	literal bool     // false if the second arg is a variable, not an object literal
}

// TestAdapterCallsites_MatchGoSchema is the structural enforcement of
// lesson 6df12582cbdb9c0c (mpm_lessons): when the Go MCP registry
// renames or removes a tool, every subprocess adapter (mpm-memory-openclaw,
// mpm-pi, mpm-claude-code, mpm-hermes, etc.) MUST be updated in the same
// commit cycle, or this test fails.
//
// The original failure mode: mpm-memory-openclaw kept calling
// `query_long_term_memory` after the 33 → 13 aggregator redesign.
// The plugin's `callMpm(...)` invocations silently 404'd inside
// OpenClaw's memory-slot adapter, surfacing as `disabled:true`
// in every agent turn. Doc sweep catches some of this but at
// the wrong layer (after merge, not at compile time).
//
// This test parses every TypeScript/JavaScript adapter under
// `agent_installation/*/index.{ts,js}` for the mpm-callSite pattern
// `callMpm(<tool>, <payload>)` (or `callMpmTool(...)`), extracts
// the tool name + literal payload keys, and verifies against
// the live Registry:
//
//   (a) The tool name must exist in Registry. Renames or removals
//       trip this immediately.
//   (b) For aggregator tools (mpm_*), the literal `action` value
//       must be in the schema's action enum. New actions added to
//       a tool without adapter usage are still flagged (we only
//       catch drift where the adapter calls a stale action).
//   (c) For non-aggregator tools, every literal top-level payload
//       key must be a declared schema property. Catches typos and
//       stale keys that the schema would silently drop.
//
// Dynamic payloads (second-arg is a variable, not a literal) are
// recorded but skipped for the key-level checks. The wrapper tests
// in the adapters (e.g. round-trip CLI/MCP tests) cover those paths.
//
// Source of truth = Registry. Test fails the build on any drift.
// Free regression insurance for every adapter rewrite.
func TestAdapterCallsites_MatchGoSchema(t *testing.T) {
	// Build the lookup tables from live Registry.
	registry := map[string]struct {
		actions        map[string]bool // for mpm_* aggregators, the action enum
		requiredAction bool           // tool with action-dispatch shape (aggregator)
		props          map[string]bool // non-aggregator: declared properties
	}{}

	for _, tool := range Registry {
		var s map[string]interface{}
		if err := json.Unmarshal(tool.Schema, &s); err != nil {
			t.Fatalf("%s: schema is not valid JSON: %v", tool.Name, err)
		}

		props := map[string]bool{}
		if raw, ok := s["properties"].(map[string]interface{}); ok {
			for k := range raw {
				props[k] = true
			}
		}

		// Aggregator pattern: top-level enum on `action` means the tool
		// uses action-dispatch. Aggregators are conventionally named mpm_*.
		requiredAction := false
		actions := map[string]bool{}
		if raw, ok := s["properties"].(map[string]interface{}); ok {
			if actionField, ok := raw["action"].(map[string]interface{}); ok {
				if enumRaw, ok := actionField["enum"]; ok {
					if enumList, ok := enumRaw.([]interface{}); ok {
						requiredAction = true
						for _, v := range enumList {
							if s, ok := v.(string); ok {
								actions[s] = true
							}
						}
					}
				}
				// mark action itself as a valid top-level key
				props["action"] = true
			}
		}
		registry[tool.Name] = struct {
			actions        map[string]bool
			requiredAction bool
			props          map[string]bool
		}{actions, requiredAction, props}
	}

	// Find the agent_installation directory. Tests run from the core
	// package directory, so the workspace root is ../../
	root, err := findAgentPluginsRoot()
	if err != nil {
		t.Skipf("agent_installation not found at expected location: %v", err)
	}

	// Walk every adapter file and extract callMpm(...) callSites.
	// Two patterns are recognised: `callMpm(...)` (JS/TS pattern used
	// by openclaw + mpm-pi) and the underlying `callMpmTool(...)`
	// (openclaw's wrapper). Both share the same (tool, payload) shape.
	var sites []callSite

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base != "index.js" && base != "index.ts" {
			return nil
		}
		found, err := extractCallsites(path)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		sites = append(sites, found...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk agent_installation: %v", err)
	}
	if len(sites) == 0 {
		t.Skipf("no callMpm(...) callSites found under %s — no adapters to guard", root)
	}

	// Validate every callSite.
	var failures []string
	seenToolAdapters := map[string]map[string]bool{} // tool → per-file adapter-set
	for _, s := range sites {
		entry, ok := registry[s.tool]
		if !ok {
			failures = append(failures, fmt.Sprintf(
				"%s:%d  callMpm(%q, ...) — tool not in Registry. "+
					"Either rename the callSite to match the new schema, or "+
					"remove the callSite if the tool was deleted.",
				s.file, s.line, s.tool))
			continue
		}
		if !s.literal {
			continue // dynamic payloads can't be checked statically
		}
		// Key-level checks.
		if entry.requiredAction {
			// Aggregator: action must be in enum, top-level payload
			// keys must be exactly {action, params} (params is open).
			if s.action == "" {
				failures = append(failures, fmt.Sprintf(
					"%s:%d  callMpm(%q, ...) — aggregator call is missing literal `action` "+
						"key (required to verify the action-dispatch shape statically)",
					s.file, s.line, s.tool))
				continue
			}
			if !entry.actions[s.action] {
				failures = append(failures, fmt.Sprintf(
					"%s:%d  callMpm(%q, {action: %q, ...}) — action %q is not in "+
						"the schema enum. Either fix the call or add the action.",
					s.file, s.line, s.tool, s.action, s.action))
			}
			// Aggregator shape is constrained to action+params. Anything else
			// is suspicious (likely a leftover from the granular-tool era).
			for _, k := range s.keys {
				if k != "action" && k != "params" {
					failures = append(failures, fmt.Sprintf(
						"%s:%d  callMpm(%q, ...) — top-level key %q is not recognised "+
							"in the aggregator shape (only action+params allowed)",
						s.file, s.line, s.tool, k))
				}
			}
		} else {
			// Non-aggregator: every top-level key must be in schema.props.
			for _, k := range s.keys {
				if !entry.props[k] {
					failures = append(failures, fmt.Sprintf(
						"%s:%d  callMpm(%q, {%q: ...}) — key %q is not a declared "+
							"property in the schema. Schema would drop it.",
						s.file, s.line, s.tool, k, k))
				}
			}
		}
		// Track adapter coverage per tool, so we can surface a "no
		// adapter uses this tool" warning if a tool has zero adapters
		// calling it (probably a dead tool).
		if seenToolAdapters[s.tool] == nil {
			seenToolAdapters[s.tool] = map[string]bool{}
		}
		seenToolAdapters[s.tool][filepath.Dir(s.file)] = true
	}

	if len(failures) > 0 {
		t.Fatalf("adapter-schema drift (%d issue(s)):\n  - %s",
			len(failures), strings.Join(failures, "\n  - "))
	}
}

// findAgentPluginsRoot returns the absolute path to the agent_installation
// directory adjacent to the workspace containing the tools package.
// The package is at internal/core/tools, so the workspace root is ../../.
func findAgentPluginsRoot() (string, error) {
	// cwd is the directory `go test` was invoked from (the tools package).
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// Walk upward looking for a directory containing `agent_installation/`.
	dir := cwd
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "agent_installation")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("agent_installation/ not found within %d levels of %s", 6, cwd)
}

// callSitePattern recognises `callMpm("X", <arg>)` and
// `callMpmTool("X", <arg>, ...)`. Captures: (1) full literal payload
// (when present), (2) the tool name.
var callSitePattern = regexp.MustCompile(`callMpm(?:Tool)?\s*\(\s*["']([a-z][a-z0-9_]*)["']\s*,\s*([^)]+?)\)`)

// extractCallsites scans a single JS/TS adapter file and returns
// the callMpm(...) callSites it finds. Second-arg shape detection:
//
//   - Object literal starting with `{` and ending with the matching
//     brace → keys are extracted, action key is captured if present.
//   - Any other expression → literal=false (dynamic).
func extractCallsites(path string) ([]callSite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src := string(raw)
	lines := strings.Split(src, "\n")

	var out []callSite
	for lineIdx, line := range lines {
		// Strip line comments — schema-guard regex shouldn't fire on
		// text inside comments. Block comments span lines, but the
		// call sites we care about rarely appear there in practice.
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		matches := callSitePattern.FindAllStringSubmatchIndex(line, -1)
		for _, m := range matches {
			toolName := line[m[2]:m[3]]
			argStart := m[4]
			argEnd := m[5]
			arg := strings.TrimSpace(line[argStart:argEnd])

			// Take the FIRST character of arg — `{` means literal object.
			if !strings.HasPrefix(arg, "{") {
				// dynamic payload (variable, function call, ...). Skip
				// key-level checks; record for tool-name verification.
				out = append(out, callSite{
					file:    path,
					line:    lineIdx + 1,
					tool:    toolName,
					literal: false,
				})
				continue
			}

			// Try to find the matching closing brace — covers single-arg
			// and multi-arg forms where the first arg is the payload.
			// In the `callMpmTool(...)` multi-arg shape, the regex's
			// non-greedy `[^)]+?` would stop at the first `)`, but the
			// payload is still the first `{...}` block. Re-extract from
			// the source if necessary.
			payload := extractFirstObject(arg)
			keys := objectKeys(payload)
			action := ""
			for _, k := range keys {
				if k == "action" {
					action = extractActionValue(payload)
					break
				}
			}
			sort.Strings(keys)
			out = append(out, callSite{
				file:    path,
				line:    lineIdx + 1,
				tool:    toolName,
				keys:    keys,
				action:  action,
				literal: true,
			})
		}
	}
	return out, nil
}

// extractFirstObject returns the brace-balanced leading object from
// `arg` (the second argument to callMpm). Tolerates trailing junk
// (e.g. `}, opts)` in the multi-arg callMTool shape).
func extractFirstObject(arg string) string {
	// If the arg starts with `(...)`, unwrap to the inner content.
	arg = strings.TrimSpace(arg)
	if strings.HasPrefix(arg, "(") {
		// Shouldn't happen given the regex, but defensive.
		end := findMatchingParen(arg, 0)
		if end < 0 {
			return arg
		}
		arg = strings.TrimSpace(arg[1:end])
	}
	if !strings.HasPrefix(arg, "{") {
		return arg
	}
	end := findMatchingBrace(arg, 0)
	if end < 0 {
		return arg
	}
	return arg[:end+1]
}

// findMatchingBrace returns the index of the brace matching the open
// brace at `start`, accounting for nesting and string literals.
func findMatchingBrace(s string, start int) int {
	depth := 0
	inStr := byte(0) // '"', '\'', or 0
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			switch c {
			case '\\':
				i++ // skip next char
				continue
			case inStr:
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = c
			continue
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// findMatchingParen returns the index of the paren matching the open
// paren at `start` (used for `(payload, opts)`-shaped callSites).
func findMatchingParen(s string, start int) int {
	depth := 0
	inStr := byte(0)
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			switch c {
			case '\\':
				i++
				continue
			case inStr:
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// objectKeys parses a leading `{...}` JS/TS object literal and returns
// the top-level identifier keys. Robust against identifier and string
// keys; ignores computed/dynamic keys (they aren't statically checkable).
func objectKeys(payload string) []string {
	// Strip the outermost braces.
	s := strings.TrimSpace(payload)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	s = s[1:findMatchingBrace(s, 0)]
	if s == "" {
		return nil
	}

	var keys []string
	// Split on top-level commas (depth 0, outside strings).
	depth := 0
	inStr := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			switch c {
			case '\\':
				i++
				continue
			case inStr:
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = c
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			depth--
		case ',':
			if depth == 0 {
				keys = append(keys, keyFromMember(strings.TrimSpace(s[start:i])))
				start = i + 1
			}
		}
	}
	// Last member.
	if start < len(s) {
		keys = append(keys, keyFromMember(strings.TrimSpace(s[start:])))
	}

	// Dedupe and drop dynamic/computed entries.
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" || k == "<dynamic>" {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// keyFromMember extracts the key from a single object member.
// Tolerates: `key: ...`, `"key": ...`, `'key': ...`, `[expr]: ...` (dynamic).
func keyFromMember(member string) string {
	if member == "" {
		return ""
	}
	switch member[0] {
	case '"', '\'':
		// String-literal key.
		end := -1
		for i := 1; i < len(member); i++ {
			if member[i] == '\\' {
				i++
				continue
			}
			if member[i] == member[0] {
				end = i
				break
			}
		}
		if end < 0 {
			return ""
		}
		return member[1:end]
	case '[':
		// Computed key — skip statically.
		return "<dynamic>"
	}
	// Bareword identifier (no quotes).
	for i := 0; i < len(member); i++ {
		if member[i] == ':' || member[i] == ' ' || member[i] == '\t' {
			return strings.TrimSpace(member[:i])
		}
	}
	return ""
}

// extractActionValue returns the literal string value of an
// `action: "foo"` member in the payload, or "" if not a literal.
func extractActionValue(payload string) string {
	m := regexp.MustCompile(`["']?action["']?\s*:\s*["']([^"']+)["']`).FindStringSubmatch(payload)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
