package tools

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// TestSchemaSupersetOfHandlerPayloadReads guards against TWO silent
// drift classes:
//
//  (1) Under-declaration: handler reads payload["k"] but the JSON-Schema
//      doesn't declare property "k". MCP clients can't supply it (schema
//      validation rejects unknown keys depending on additionalProperties);
//      CLI callers via `mpm call <tool>` route through the same schema
//      and surface runtime errors. Both paths fail silently until a
//      downstream caller exercises the handler — which is why the
//      original drift (c4c16a1 / 633ddf2) was live for months.
//
//  (2) Over-declaration: JSON-Schema declares property "k" but the
//      handler doesn't read it (no payload["k"], no getString(payload,"k")).
//      A client supplies the value, schema validates it, the handler
//      silently drops it. The call "succeeds" but the data is gone —
//      a lying contract. Surfaced 2026-07-06 by add_evidence schema
//      advertising source_url / source_path with no DB column, no
//      EvidenceInput field, no handler read, and no CLI flag.
//
// Invariant: ∀ tool ∈ Registry,
//     schema.properties(tool) = handler-payload-reads(tool)
//
// Both directions of drift are now hard-fails: the schema is the
// single source of truth for what the wire accepts, and any divergence
// from the handler is a bug to fix, not a hint to relax.
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
	// First pass: walk handler→schema, catch under-declaration
	// (handler reads keys the schema doesn't expose).
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
			t.Errorf("%s: handler reads payload keys %v that are not declared in the JSON-Schema (under-declaration)",
				toolName, missing)
			failures++
		}
	}

	// Second pass: walk schema→handler, catch over-declaration
	// (schema advertises keys the handler silently drops). The
	// exception is handlers that pass the payload through to another
	// function (no literal payload[k] / getString in the handler
	// body); for those the AST extractor sees zero reads and we skip
	// the over-decl check entirely to avoid false positives. We
	// detect "passthrough handlers" by checking if the handler has
	// any property at all in schemaProps — if the schema declares
	// properties but the handler reads zero of them, it's likely
	// passthrough or a missing-impl bug, and the over-decl list
	// would be a false positive cascade.
	for toolName, props := range schemaProps {
		// Find the matching handler. handle<PascalCasedToolName>.
		handlerName := "handle" + snakeToCamel(toolName)
		reads, ok := handlerReads[handlerName]
		if !ok {
			// Different drift class: schema has an entry but no handler.
			// Skip — the registry roundtrip test catches this.
			continue
		}
		// If the handler reads NOTHING, the AST extractor can't see
		// into the passthrough. Skip over-decl check to avoid false
		// positives on the passthrough pattern. Handlers that read
		// nothing AND the schema has no properties are fine; the
		// under-decl pass already covered that.
		if len(reads) == 0 {
			continue
		}
		var phantom []string
		for k := range props {
			if !reads[k] {
				phantom = append(phantom, k)
			}
		}
		if len(phantom) > 0 {
			sort.Strings(phantom)
			t.Errorf("%s: JSON-Schema declares properties %v that the handler never reads (over-declaration — clients think they supplied these values but they are silently dropped)",
				toolName, phantom)
			failures++
		}
	}

	if failures > 0 {
		t.Logf("(see %d drift(s) above — fix the schema in internal/tools/registry_list.go to declare only what the handler reads, and ensure the handler reads everything the schema declares. Source of truth = handler. Schema = the published contract.)", failures)
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

// snakeToCamel is the inverse of camelToSnake for the simple case
// used in the test: word-boundary-separated lowercase tokens, first
// token lowercase, subsequent tokens capitalized. Acronym collapse
// (XMLHttpRequest → xml_http_request → XmlHttpRequest) is not needed
// here because the Registry tool names use simple lower-snake form
// (add_evidence, log_to_changelog). If a tool name ever uses
// acronyms, this helper will need to grow the same boundary logic
// camelToSnake uses.
//
//   "add_evidence"     → "AddEvidence"
//   "log_to_changelog" → "LogToChangelog"
//   "gc_run"           → "GcRun"
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		runes := []rune(p)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
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
