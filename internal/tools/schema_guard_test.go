package tools

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// TestSchemaSupersetOfHandlerPayloadReads guards against the silent
// drift class closed by c4c16a1 / 633ddf2 (2026-07-05): JSON-Schema
// properties that fail to expose the keys their handlers actually
// read from the payload. MCP clients can't supply those keys (schema
// validation rejects them); CLI callers via `mpm call <tool>` route
// through the same schema, surface them as runtime errors. Both
// paths fail silently until a downstream caller exercises the
// handler — which is why the drift was live for months.
//
// Invariant: ∀ tool ∈ Registry,
//     schema.properties(tool) ⊇ handler-payload-reads(tool)
//
// Schemas are allowed to over-declare (advertise keys the handler
// doesn't use) — that's safe. What schemas MUST NOT do is
// under-declare, because the schema is the contract exposed to
// non-CLI callers and the first validator reached on CLI calls.
//
// Free regression insurance for any tool added to Registry. Lands in
// internal/tools per the architectural call (frictionless pre-commit,
// no DB, fast AST walk).
func TestSchemaSupersetOfHandlerPayloadReads(t *testing.T) {
	schemaProps := extractSchemaProperties(t)
	handlerReads := extractHandlerPayloadReads(t)

	if len(handlerReads) == 0 {
		t.Fatal("extractHandlerPayloadReads returned no handlers — extractor may be broken")
	}

	failures := 0
	for handlerName, reads := range handlerReads {
		// Map handler → tool name: strip "handle" prefix + camel-to-snake
		// (handleAddEvidence → add_evidence, handleLogToChangelog → log_to_changelog, etc.).
		toolName := camelToSnake(strings.TrimPrefix(handlerName, "handle"))
		props, ok := schemaProps[toolName]
		if !ok {
			// Different drift class: handler exists, but Registry has no entry.
			// Surface as a separate error so the root cause is observable.
			t.Errorf("handler %q has no matching Registry entry %q", handlerName, toolName)
			failures++
			continue
		}
		if len(reads) == 0 {
			continue // handler accepts empty payload (e.g. read_wake_context); nothing to enforce.
		}
		var missing []string
		for k := range reads {
			if !props[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("%s: handler reads payload keys %v that are not declared in the JSON-Schema",
				toolName, missing)
			failures++
		}
	}
	if failures > 0 {
		t.Logf("(see %d drift(s) above — fix the schema in internal/tools/registry_list.go to declare these properties; schemas are required to superset handler reads.)", failures)
	}
}

// camelToSnake converts CamelCase → snake_case, including acronyms.
// e.g.
//   "AddEvidence"     → "add_evidence"
//   "LogToChangelog"  → "log_to_changelog"
//   "GCRun"           → "gc_run"        (not "g_c_run")
//   "XMLHttpRequest"  → "xml_http_request"
//
// Word-boundary rule: insert '_' before an uppercase letter when
// either (a) the previous letter is lowercase (camelCase boundary)
// or (b) previous is uppercase AND next is lowercase (end of an
// acronym, e.g. the 'R' in GCRun signals end of "GC" and start of
// "Run"). Acronym-internal boundaries (X/M/L in XMLHttpRequest) do
// NOT trigger — those letters stay joined.
func camelToSnake(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				prev := runes[i-1]
				prevLower := prev >= 'a' && prev <= 'z'
				prevUpper := prev >= 'A' && prev <= 'Z'
				var nextLower bool
				if i+1 < len(runes) {
					next := runes[i+1]
					nextLower = next >= 'a' && next <= 'z'
				}
				if prevLower || (prevUpper && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r + 32) // 'A' + 32 == 'a'
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// extractSchemaProperties walks Registry and returns
// tool-name → set-of-property-names declared in each tool's JSON-Schema.
func extractSchemaProperties(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
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
		// required[] is a subset of properties; intentionally not enforced
		// here (handler may read but not require). Properties-with-claim is
		// the only thing that breaks callers when missing.
		out[tool.Name] = props
	}
	return out
}

// extractHandlerPayloadReads parses internal/tools/handlers.go via
// go/parser + go/ast and returns handler-name → set-of-keys-read-from-the-payload-argument.
//
// Patterns captured:
//   - payload["k"] / p["k"]                                 (IndexExpr)
//   - getString(payload, "k") / getString(p, "k")           (CallExpr)
//
// Patterns NOT captured (intentional, false positives):
//   - payload[idx] where idx is a variable — capture-with-rename
//     is unsafe; we capture literal-only.
//   - payload["nested"]["deeper"] — chained reads; rare in this
//     codebase, capture outer only. If the inner IndexExpr isn't a
//     literal, we'd safely miss it without false-positive claims.
//   - index expressions on other variables (e.g. ac["key"]) — only
//     `payload` and `p` are tracked, matching the named arg in the
//     handleX signatures.
func extractHandlerPayloadReads(t *testing.T) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handlers.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse handlers.go: %v", err)
	}
	out := map[string]map[string]bool{}

	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fd.Name.Name, "handle") {
			return true
		}
		keys := map[string]bool{}
		ast.Inspect(fd, func(m ast.Node) bool {
			if k := literalFromPayloadIndex(m); k != "" {
				keys[k] = true
			}
			if k := literalFromGetStringCall(m); k != "" {
				keys[k] = true
			}
			return true
		})
		out[fd.Name.Name] = keys
		return false // body already traversed; prevent outer descent.
	})
	return out
}

// literalFromPayloadIndex returns the literal key from a payload["k"]
// or p["k"] expression, including the TypeAssertExpr-wrapped forms
// (payload["k"].(string), payload["k"].(float64), etc. — the IndexExpr
// sits underneath and is visited independently by ast.Inspect).
// Returns "" if the node isn't a payload-index with a string literal.
func literalFromPayloadIndex(n ast.Node) string {
	idx, ok := n.(*ast.IndexExpr)
	if !ok {
		return ""
	}
	id, ok := idx.X.(*ast.Ident)
	if !ok || (id.Name != "payload" && id.Name != "p") {
		return ""
	}
	lit, ok := idx.Index.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	return strings.Trim(lit.Value, `"`)
}

// literalFromGetStringCall returns the literal key from
// getString(payload, "k") / getString(p, "k"). Returns "" otherwise.
func literalFromGetStringCall(n ast.Node) string {
	cl, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	fn, ok := cl.Fun.(*ast.Ident)
	if !ok || fn.Name != "getString" {
		return ""
	}
	if len(cl.Args) < 2 {
		return ""
	}
	id, ok := cl.Args[0].(*ast.Ident)
	if !ok || (id.Name != "payload" && id.Name != "p") {
		return ""
	}
	lit, ok := cl.Args[1].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	return strings.Trim(lit.Value, `"`)
}
