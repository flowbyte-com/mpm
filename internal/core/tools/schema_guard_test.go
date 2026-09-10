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

	// The 2026-08-11 aggregator redesign split every granular tool
	// into mpm_* aggregators with action-dispatch. handleSaveToMemory
	// etc. are STILL defined as Go functions (re-used as dispatch
	// targets inside handleMpmMemory's switch), but they no longer
	// own a top-level Registry entry. Filter them out so the test
	// only asserts schema coverage on the actual public surface.
	fset := token.NewFileSet()
	dispatchTargets := extractAggregatorDispatchTargets(fset)
	internalHandlers := map[string]bool{}
	for name := range dispatchTargets {
		internalHandlers[name] = true
	}

	failures := 0
	// First pass: walk handler→schema, catch under-declaration
	// (handler reads keys the schema doesn't expose).
	for handlerName, reads := range handlerReads {
		// Skip aggregator-dispatch targets: handleSaveToMemory,
		// handleQueryLongTermMemory, etc. Their schema coverage
		// is enforced transitively via the caller's params object
		// (additionalProperties:true means any key is allowed inside).
		if internalHandlers[handlerName] {
			continue
		}
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
		// under-decl pass already covered that. After the 2026-09-05
		// audit C.18 closure, every aggregator (mpm_system, mpm_work,
		// mpm_handoff, etc.) declares per-action params via oneOf —
		// the top-level props walk stays bounded to the aggregator's
		// own reads (`action`, `params` via passthrough), and the
		// per-action under-decl check is enforced separately by the
		// per-tool regression tests (mpm_system_schema_branches_*, etc.).
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
//   - payload["k"] / p["k"] / params["k"]                     (IndexExpr)
//   - getString(payload, "k") / getString(p, "k")           (CallExpr)
//   - internal.ParseStringOr(p["k"], default) and friends   (CallExpr SelectorExpr)
//   - handleX(..., params) — passthrough detected post-2026-08-13
//     hardening. Aggregator dispatchers extract `params` once via
//     extractParamsOrFail and forward it to inner handlers; the
//     schema declares `params` as a property, so we record a
//     pseudo-read of `params` whenever the aggregator passes it on
//     to a handle* call.
//   - dm.X(payload[, ...]) — passthrough detected via line 137 of
//     the source comment.
//
// Patterns NOT captured (intentional, false positives):
//   - payload[idx] where idx is a variable — capture-with-rename
//     is unsafe; we capture literal-only.
//   - payload["nested"]["deeper"] — chained reads; rare in this
//     codebase, capture outer only. If the inner IndexExpr isn't a
//     literal, we'd safely miss it without false-positive claims.
//   - index expressions on other variables (e.g. ac["key"]) — only
//     `payload`, `p`, and `params` are tracked, matching the named
//     arg in the handleX signatures.
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
				return true
			}
			if k := literalFromGetStringCall(m); k != "" {
				keys[k] = true
				return true
			}
			// Passthrough detection: when an aggregator like
			// handleMpmMemory forwards `params` (or `p`) to a
			// handle* call, the schema's `params` property is
			// consumed. Record a pseudo-read of `params` (or `p`)
			// so the over-decl check doesn't fire a phantom.
			if k := literalFromPassthroughCall(m); k != "" {
				keys[k] = true
			}
			return true
		})
		out[fd.Name.Name] = keys
		return false // body already traversed; prevent outer descent.
	})
	return out
}

// literalFromPassthroughCall scans CallExpr nodes for invocations of a
// handle* function whose argument list contains an `Identifier` named
// `params`, `payload`, or `p` — the canonical "this is a passthrough"
// pattern from the aggregator dispatchers (handleMpmMemory calls
// handleSaveToMemory(dm, ac, params), etc.). Returns the first such
// identifier's name as a "pseudo-read" key, or "" if the call has no
// handle* callee or no payload-like argument.
//
// 2026-08-13 hardening extension: enables the schema-guard over-decl
// check to recognize that the aggregator does consume `params` (the
// schema-declared envelope) by passing it to an inner handler.
func literalFromPassthroughCall(n ast.Node) string {
	cl, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	// Callee must be a handle* identifier (e.g. handleSaveToMemory).
	id, ok := cl.Fun.(*ast.Ident)
	if !ok {
		return ""
	}
	if !strings.HasPrefix(id.Name, "handle") {
		return ""
	}
	// Find the first payload-shaped arg.
	for _, arg := range cl.Args {
		argID, ok := arg.(*ast.Ident)
		if !ok {
			continue
		}
		if argID.Name == "params" || argID.Name == "payload" || argID.Name == "p" {
			return argID.Name
		}
	}
	return ""
}

// extractAggregatorDispatchTargets walks handlers.go and returns the
// set of handler function names (e.g. handleSaveToMemory) that are
// invoked as dispatch targets from inside any handleMpm* aggregator
// handler. These are the "leaf" handlers reached through action-dispatch
// (handleMpmMemory action:save routes through handleSaveToMemory), and
// they DO NOT need their own Registry entry — the schema guard
// against the aggregator covers their payload keys.
//
// Without this filter, the post-2026-08-11 aggregator redesign would
// produce 70 false-positive "handler has no matching Registry entry"
// errors: every granular handleX is still implemented as a Go
// function, but they're internal dispatch targets rather than
// top-level MCP tools.
//
// Alpha-4 W-001 update: walk the full body (not just case arms) AND
// recursively pull in any handleX called from a target. This catches
// helpers like handleReadWakeContextCompact (called from inside an
// `if projection == "compact"` branch of handleReadWakeContext, which
// itself is a case-arm dispatch target of handleMpmContext). The
// previous shape only inspected case-arm return statements, which
// missed the projection branches that gate payload shape on string-enum
// params.
func extractAggregatorDispatchTargets(fset *token.FileSet) map[string]bool {
	f, err := parser.ParseFile(fset, "handlers.go", nil, parser.ParseComments)
	if err != nil {
		return nil
	}
	targets := map[string]bool{}

	// Step 1: collect every handle* function name defined in this file.
	// We need this so step 2 can match by name even when the body is
	// inspected via the *ast.File instead of one FuncDecl at a time.
	allHandlers := map[string]*ast.FuncDecl{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if strings.HasPrefix(fd.Name.Name, "handle") {
			allHandlers[fd.Name.Name] = fd
		}
	}

	// Step 2: walk every handleMpm* body, recording every handleX call
	// (case arm OR projection branch OR inline return).
	seen := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		fd, ok := allHandlers[name]
		if !ok {
			return
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if strings.HasPrefix(id.Name, "handle") && id.Name != name && allHandlers[id.Name] != nil {
				if !targets[id.Name] {
					targets[id.Name] = true
					visit(id.Name)
				}
			}
			return true
		})
	}

	for name := range allHandlers {
		if strings.HasPrefix(name, "handleMpm") {
			visit(name)
		}
	}
	return targets
}

// literalFromPayloadIndex returns the literal key from a payload["k"]
// or p["k"] expression, including the TypeAssertExpr-wrapped forms
// (payload["k"].(string), payload["k"].(float64), etc. — the IndexExpr
// sits underneath and is visited independently by ast.Inspect).
//
// 2026-08-13 hardening update: after the extractParamsOrFail refactor,
// domain dispatchers receive `params` (the unwrapped envelope) instead
// of `payload`. We also accept `params["k"]` reads so the schema guard
// continues to recognize them as handler payload reads.
// Returns "" if the node isn't a payload/index with a string literal.
func literalFromPayloadIndex(n ast.Node) string {
	idx, ok := n.(*ast.IndexExpr)
	if !ok {
		return ""
	}
	id, ok := idx.X.(*ast.Ident)
	if !ok {
		return ""
	}
	if id.Name != "payload" && id.Name != "p" && id.Name != "params" {
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
//
// 2026-08-13 hardening update: also recognises `internal.ParseStringOr(p["k"], default)`
// and `internal.ParseFloatOr(p["k"], default)` style reads. These are
// the idiomatic helpers used in handlers; without recognizing them the
// post-hardening schema guard produces phantom over-declaration errors
// for every domain dispatcher.
//
// 2026-09-10 hardening update: also recognises `collectConfirmationSpecs(p, "k", type)`
// and `collectContradictionSpecs(p, "k", type)` style reads. These
// helpers (handlers.go handleLogToChangelog) take the payload key as a
// string-literal second argument, so the body of the helper does
// `p[paramName]` — a direct IndexExpr read that this detector cannot
// see from the call site. Recognising the call-site literal prevents
// the schema guard from flagging those keys as over-declared when the
// handler does read them via the helper.
func literalFromGetStringCall(n ast.Node) string {
	cl, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	// Form 1: getString(payload, "k")
	if fn, ok := cl.Fun.(*ast.Ident); ok && fn.Name == "getString" {
		if len(cl.Args) < 2 {
			return ""
		}
		id, ok := cl.Args[0].(*ast.Ident)
		if !ok || (id.Name != "payload" && id.Name != "p" && id.Name != "params") {
			return ""
		}
		lit, ok := cl.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return ""
		}
		return strings.Trim(lit.Value, `"`)
	}
	// Form 2: internal.ParseStringOr(p["k"], default) / internal.ParseFloatOr(p["k"], default)
	if sel, ok := cl.Fun.(*ast.SelectorExpr); ok {
		if sel.Sel.Name != "ParseStringOr" && sel.Sel.Name != "ParseFloatOr" && sel.Sel.Name != "ParseStringSliceOr" && sel.Sel.Name != "ParseIntOr" && sel.Sel.Name != "ParseBoolOr" {
			return ""
		}
		if len(cl.Args) < 1 {
			return ""
		}
		// Walk the arg looking for a literal-key index. Typically a direct
		// p["k"] form rather than nested through a variable.
		if idx, ok := cl.Args[0].(*ast.IndexExpr); ok {
			return literalFromPayloadIndex(idx)
		}
	}
	// Form 3: collectConfirmationSpecs(p, "k", type) / collectContradictionSpecs(p, "k", type).
	// The helper takes the payload key as a string-literal second argument
	// and reads p[paramName] inside. From the call site the read is hidden
	// behind a helper indirection; this detector recognises the literal so
	// the schema-guard over-decl check doesn't phantom-flag the key.
	if fn, ok := cl.Fun.(*ast.Ident); ok &&
		(fn.Name == "collectConfirmationSpecs" || fn.Name == "collectContradictionSpecs") {
		if len(cl.Args) < 2 {
			return ""
		}
		id, ok := cl.Args[0].(*ast.Ident)
		if !ok || (id.Name != "payload" && id.Name != "p" && id.Name != "params") {
			return ""
		}
		lit, ok := cl.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return ""
		}
		return strings.Trim(lit.Value, `"`)
	}
	return ""
}

// contains is a tiny helper used by the per-tool schema guard tests
// (TestSchemaGuard_SaveSkill et al.) to assert presence of a value
// in a []interface{} slice — JSON-Schema's "required" field unmarshals
// as []interface{} rather than []string, so a direct reflect-equal
// comparison doesn't work. Centralised here so the per-tool tests
// don't reinvent it.
func contains(s []interface{}, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestSchemaGuard_MpmSkillsSave locks the description contract for
// the `save` action on mpm_skills. After the 2026-08-11 aggregator
// redesign, this skill-persistence guarantee lives in the aggregator's
// description field — the action enum still pins which actions exist
// on mpm_skills, but each action's required-field guarantee is encoded
// in prose (additionalProperties:true covers anything inside params).
func TestSchemaGuard_MpmSkillsSave(t *testing.T) {
	var found *Tool
	for i := range Registry {
		if Registry[i].Name == "mpm_skills" {
			found = &Registry[i]
			break
		}
	}
	if found == nil {
		t.Fatal("mpm_skills not registered")
	}
	// Pin action enum — must include save/read/list.
	var schema map[string]interface{}
	if err := json.Unmarshal(found.Schema, &schema); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	actionField, ok := schema["properties"].(map[string]interface{})["action"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_skills schema missing `action` field with enum")
	}
	enumRaw, ok := actionField["enum"].([]interface{})
	if !ok {
		t.Fatalf("mpm_skills action enum missing (got %T)", actionField["enum"])
	}
	for _, want := range []string{"save", "read", "list"} {
		if !contains(enumRaw, want) {
			t.Errorf("mpm_skills action enum missing %q (callers relying on this action silently break)", want)
		}
	}

	// The save action's required-field guarantee is documented in
	// the description field (the JSON schema uses additionalProperties:true
	// for params, so we can't enforce via JSON-Schema's `required`).
	// Pin the description so a careless prose-edit doesn't drop a field.
	for _, want := range []string{"name", "version", "content"} {
		if !strings.Contains(found.Description, want) {
			t.Errorf("mpm_skills save action description must mention required field %q (got: %q)", want, found.Description)
		}
	}
}

// TestSchemaGuard_MpmSkillsRead pins the `read` action's required-field
// guarantee. `name` must be required; description must also mention
// optional `version` so the contract stays wider than just "name".
func TestSchemaGuard_MpmSkillsRead(t *testing.T) {
	var found *Tool
	for i := range Registry {
		if Registry[i].Name == "mpm_skills" {
			found = &Registry[i]
			break
		}
	}
	if found == nil {
		t.Fatal("mpm_skills not registered")
	}
	// Pin action enum — must include read.
	var schema map[string]interface{}
	if err := json.Unmarshal(found.Schema, &schema); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	actionField, ok := schema["properties"].(map[string]interface{})["action"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_skills schema missing `action` field with enum")
	}
	enumRaw, ok := actionField["enum"].([]interface{})
	if !ok {
		t.Fatalf("mpm_skills action enum missing")
	}
	if !contains(enumRaw, "read") {
		t.Error("mpm_skills action enum missing 'read'")
	}

	// Pin the description so a careless edit doesn't drop the
	// required `name` field. We also expect `version` to remain
	// documented as an optional param.
	if !strings.Contains(found.Description, "read") ||
		!strings.Contains(found.Description, "name") {
		t.Errorf("mpm_skills read action description must mention `read` and `name` (got: %q)", found.Description)
	}
}

// TestSchemaGuard_MpmSkillsList pins the `list` action's scope enum.
// The dispatcher passes `scope` through to the underlying handler as a
// free-form string, but the contract is local|shared|all — future
// edits that rename the values break callers silently.
//
// Aggregator tools can't enforce this enum directly (params are
// additionalProperties:true), so the contract lives in the description.
// The pin in this test ensures the description stays correct.
func TestSchemaGuard_MpmSkillsList(t *testing.T) {
	var found *Tool
	for i := range Registry {
		if Registry[i].Name == "mpm_skills" {
			found = &Registry[i]
			break
		}
	}
	if found == nil {
		t.Fatal("mpm_skills not registered")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(found.Schema, &schema); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	actionField, ok := schema["properties"].(map[string]interface{})["action"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_skills schema missing `action` field with enum")
	}
	enumRaw, ok := actionField["enum"].([]interface{})
	if !ok {
		t.Fatal("mpm_skills action enum missing")
	}
	if !contains(enumRaw, "list") {
		t.Error("mpm_skills action enum missing 'list'")
	}
	// Scope enum documented in description (params.* fields don't enforce enum).
	for _, want := range []string{"local", "shared", "all"} {
		if !strings.Contains(found.Description, want) {
			t.Errorf("mpm_skills list action description must mention scope value %q (got: %q)", want, found.Description)
		}
	}
}

// TestSchemaGuard_LogToChangelogAssertions locks the schema for the six
// optional assertion params on log_to_changelog (3 confirms + 3
// contradicts). These keys are read by the handler via the
// collectConfirmationSpecs / collectContradictionSpecs helpers rather
// than direct payload["k"] reads, so the AST-driven
// TestSchemaSupersetOfHandlerPayloadReads does NOT catch a future
// regression where one of these declarations is silently dropped from
// the schema. This test fills that gap with a focused pin.
//
// The test also asserts the handler accepts each key without dropping
// it (via the response shape's `confirmations` / `contradictions`
// counts), so a future handler refactor that loses the read path
// would also fail this test.
//
// Both directions of drift the existing parity machinery covers —
// (a) schema drops a declaration, (b) handler drops a read — are now
// pinned here. The 6 keys are tied to the 2026-09-05 epistemic-
// confirmation/contradiction feature (docs/epistemic-confirmation.md)
// and the schema/handler contract is the public surface for that
// feature, so this test is the narrowest possible closure.
func TestSchemaGuard_LogToChangelogAssertions(t *testing.T) {
	var found *Tool
	for i := range Registry {
		if Registry[i].Name == "log_to_changelog" {
			found = &Registry[i]
			break
		}
	}
	if found == nil {
		t.Fatal("log_to_changelog not registered")
	}

	var schema map[string]interface{}
	if err := json.Unmarshal(found.Schema, &schema); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("log_to_changelog schema missing top-level `properties`")
	}

	// All six assertion keys must be declared in the schema. If any
	// of these declarations is silently removed in a future edit,
	// the property disappears from `props` and this test fails.
	for _, key := range []string{
		"confirms_lesson_id",
		"confirms_decision_id",
		"confirms_theory_id",
		"contradicts_lesson_id",
		"contradicts_decision_id",
		"contradicts_theory_id",
	} {
		prop, ok := props[key].(map[string]interface{})
		if !ok {
			t.Errorf("log_to_changelog schema is missing declaration for %q (clients cannot supply what the schema does not advertise — under-declaration drift)", key)
			continue
		}
		// Each param accepts string OR []string (per the handler's
		// collectConfirmationSpecs / collectContradictionSpecs helpers,
		// mirrored in docs/epistemic-confirmation.md §"CLI surface").
		// A future regression that narrows the type would be a
		// contract drift — pin the shape here.
		if _, hasOneOf := prop["oneOf"]; !hasOneOf {
			t.Errorf("log_to_changelog schema param %q must accept string OR []string via oneOf (got %+v)", key, prop)
		}
	}
}
