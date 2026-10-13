// subcommand_help_owners_test.go — keeps subcommandHelpOwners honest.
//
// WHY (2026-10-13). The help guard in Execute inverts the default: a
// `--help` request is only handed to a subcommand handler when that
// handler's exact "<family> <sub>" path is a key of subcommandHelpOwners.
// An omission is SAFE (the family page prints, no mutation) but it is
// silent: `mpm skill workshop --help` degraded from the workshop guide
// to the generic `mpm skill` page, and only the canonical suite caught
// it. Reading the map by eye did not.
//
// So this test derives the obligation from the SOURCE instead. It finds
// every handler that recognises a help token, traces where each one is
// dispatched from, and reconstructs the "<family> <sub>" path key. Every
// such path must then be either
//
//	(a) a key of subcommandHelpOwners — the handler prints its own page; or
//	(b) an explicit entry in knownNonOwningSubcommandHelp with a reason —
//	    the handler looks like it helps but demonstrably runs instead.
//
// There is no third option. Adding a new short-circuiting subcommand
// forces a decision at test time rather than at some future release when
// a help page has quietly gone missing.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// knownNonOwningSubcommandHelp records paths that DO look like help
// owners — their handler compares an argument against a help token — but
// that are deliberately absent from subcommandHelpOwners.
//
// Each entry must say what the path actually does. The list is the
// record of the per-path decisions that the inverted default made
// necessary; `ops lint` and `topic show` are here because they EXECUTE
// rather than print a help page, which is the exact hazard the allowlist
// exists to close.
var knownNonOwningSubcommandHelp = map[string]string{
	// --- Paths whose handler genuinely EXECUTES on a help token. These
	// are the reason the default was inverted: they are the paths that
	// must never be handed a "help" token by Execute.
	"work list":       "prints the list of works; the trailing token is consumed as data, not a help request",
	"work show":       "prints the live Working Context — it RUNS, which is why `work show` is absent from the owners map",
	"topic show":      "runs the lookup and reports `Topic not found: help`",
	"topic search":    "runs the search with the trailing token",
	"reference get":   "reads the named reference",
	"config show":     "prints current configuration",
	"config set":      "performs the assignment",
	"config edit":     "opens an editor and blocks until stdin closes",
	"config validate": "runs the structural validation and prints its result",
	"ops lint":        "reports `unknown flag \"help\"` and then lints anyway",
	"ops stats":       "accrues runtime state — verified MUTATING",
	"ops status":      "accrues runtime state — verified MUTATING",
	"ops prune":       "runs the prune and reports how many rows it removed",
	"ops export":      "dumps JSON to stdout",

	// --- Paths that are INTERCEPTED: `mpm <path> help` exits 0, prints
	// the family's help page, and never reaches the handler. Each was
	// verified empirically against an isolated workspace on 2026-10-13.
	// These are safe but less specific — where the handler would have
	// printed a page of its own, that page is the cost. Promoting any of
	// them into subcommandHelpOwners is a separate change that must first
	// prove the handler is inert; TestHelpRequestsNeverMutate does exactly
	// that for every owner key.
	"blob gc":                    "intercepted; prints the `mpm blob` page, which lists `gc [--dry-run|-n]`",
	"lesson add":                 "intercepted; prints the `mpm lesson` page, which lists `mpm lesson add <content>`",
	"memory add":                 "intercepted; prints the `mpm memory` page, which documents `mpm memory add`",
	"memory save":                "intercepted; alias of `memory add`, documented on the `mpm memory` page",
	"memory delete":              "intercepted; prints the `mpm memory` page",
	"memory list":                "intercepted; prints the `mpm memory` page",
	"memory promote":             "intercepted; prints the `mpm memory` page",
	"memory reinforce":           "intercepted; prints the `mpm memory` page",
	"memory restore":             "intercepted; prints the `mpm memory` page",
	"memory search":              "intercepted; prints the `mpm memory` page",
	"memory set-weight":          "intercepted; prints the `mpm memory` page",
	"memory show":                "intercepted; prints the `mpm memory` page",
	"memory snooze":              "intercepted; prints the `mpm memory` page",
	"memory weaken":              "intercepted; prints the `mpm memory` page",
	"ops logs":                   "intercepted; prints the `mpm ops` page — cost: the handler's own logs page is not reached",
	"ops reference":              "intercepted; prints the `mpm ops` page — cost: the handler's own reference page is not reached",
	"ops resolve-contradictions": "intercepted; prints the `mpm ops` page — cost: the handler's own resolve page is not reached",
	"session add":                "intercepted; prints the `mpm session` page, which lists `mpm session add <content>`",
	"session allocate":           "intercepted; prints the `mpm session` page, which lists `mpm session allocate`",
	"session rotate":             "intercepted; prints the `mpm session` page, which lists `mpm session rotate`",
	"shred database":             "intercepted; prints the `mpm shred` page, which states the shred scope and its limits",
}

// loadMPMAST parses every non-test Go source file in this package.
// Test files are excluded on purpose: their help-shaped string literals
// are assertions about behaviour, not behaviour.
func loadMPMAST(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no sources parsed — the derivation below would vacuously pass")
	}
	return files
}

// handlerRecognisesHelp reports whether fn compares an argument against
// one of the help tokens, or delegates to requireHelpShortCircuit.
//
// The comparison test is deliberately loose. A false positive costs one
// entry in knownNonOwningSubcommandHelp; a false negative is exactly the
// omission that cost the workshop guide.
func handlerRecognisesHelp(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.Ident:
			if x.Name == "requireHelpShortCircuit" {
				found = true
			}
		case *ast.BinaryExpr:
			if x.Op != token.EQL {
				return true
			}
			for _, side := range [2]ast.Expr{x.X, x.Y} {
				lit, ok := side.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if v == "help" || v == "-h" || v == "--help" {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// calledHandler extracts the handler name from a `return f(...)` result.
// Direct `return f` (a func value) is accepted too.
func calledHandler(e ast.Expr) string {
	var id *ast.Ident
	switch x := e.(type) {
	case *ast.Ident:
		id = x
	case *ast.CallExpr:
		id, _ = x.Fun.(*ast.Ident)
	}
	if id == nil || !strings.HasPrefix(id.Name, "handle") {
		return ""
	}
	return id.Name
}

// returnedHandlerNames returns every function a switch case returns,
// keyed by the case's string literal ("" when the case has none).
func returnedHandlerNames(st *ast.SwitchStmt) map[string][]string {
	out := map[string][]string{}
	for _, s := range st.Body.List {
		cc, ok := s.(*ast.CaseClause)
		if !ok {
			continue
		}
		key := ""
		if len(cc.List) == 1 {
			if lit, ok := cc.List[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				key, _ = strconv.Unquote(lit.Value)
			}
		}
		for _, stmt := range cc.Body {
			ret, ok := stmt.(*ast.ReturnStmt)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			if id := calledHandler(ret.Results[0]); id != "" {
				out[key] = append(out[key], id)
			}
		}
	}
	return out
}

// TestSubcommandHelpOwnersCoverEveryShortCircuit is the standing guard.
// It reconstructs every "<family> <sub>" path whose handler recognises a
// help token and demands that the path has been consciously classified.
func TestSubcommandHelpOwnersCoverEveryShortCircuit(t *testing.T) {
	files := loadMPMAST(t)

	recognises := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if handlerRecognisesHelp(fn) {
				recognises[fn.Name.Name] = true
			}
		}
	}
	if len(recognises) == 0 {
		t.Fatal("no help-recognising handlers found — the derivation regressed, not the CLI")
	}

	// familyHandler -> subcommand -> handlers it returns.
	subDispatch := map[string]map[string][]string{}
	// top-level command -> the family handler Execute returns for it.
	topDispatch := map[string][]string{}

	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				st, ok := n.(*ast.SwitchStmt)
				if !ok {
					return true
				}
				for sub, handlers := range returnedHandlerNames(st) {
					if subDispatch[fn.Name.Name] == nil {
						subDispatch[fn.Name.Name] = map[string][]string{}
					}
					subDispatch[fn.Name.Name][sub] = append(subDispatch[fn.Name.Name][sub], handlers...)
				}
				return true
			})
			// Execute's dispatch is identified structurally: it is the
			// `switch cmd.Name` whose cases all return a handler.
			if fn.Name.Name == "Execute" {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					st, ok := n.(*ast.SwitchStmt)
					if !ok {
						return true
					}
					tag, ok := st.Tag.(*ast.SelectorExpr)
					if !ok || tag.Sel.Name != "Name" {
						return true
					}
					for cmd, handlers := range returnedHandlerNames(st) {
						if cmd != "" {
							topDispatch[cmd] = handlers
						}
					}
					return true
				})
			}
		}
	}

	type obligation struct{ path, handler string }
	var owed []obligation
	for cmd, families := range topDispatch {
		for _, family := range families {
			for sub, handlers := range subDispatch[family] {
				if sub == "" {
					continue // not a subcommand path
				}
				for _, h := range handlers {
					if recognises[h] {
						owed = append(owed, obligation{cmd + " " + sub, h})
					}
				}
			}
		}
	}
	if len(owed) == 0 {
		t.Fatal("derived no subcommand help obligations — the derivation regressed, not the CLI")
	}
	sort.Slice(owed, func(i, j int) bool { return owed[i].path < owed[j].path })

	var unclassified []string
	seen := map[string]bool{}
	for _, o := range owed {
		if seen[o.path] {
			continue
		}
		seen[o.path] = true
		switch {
		case subcommandHelpOwners[o.path]:
			continue
		case knownNonOwningSubcommandHelp[o.path] != "":
			continue
		default:
			unclassified = append(unclassified,
				o.path+"  (dispatched to "+o.handler+", which recognises a help token)")
		}
	}

	if len(unclassified) > 0 {
		t.Errorf("%d subcommand help path(s) are unclassified:\n  %s\n\n"+
			"Either add the path to subcommandHelpOwners (the handler prints its own page), "+
			"or add it to knownNonOwningSubcommandHelp with a reason (the handler runs instead). "+
			"An unclassified path prints the FAMILY page instead of the subcommand's own — "+
			"silent, and invisible until someone notices the help page changed.",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}
}

// TestKnownNonOwningSubcommandHelpDoesNotShadowOwners keeps the two
// tables disjoint: a path that is classified twice is a path whose
// behaviour is described two incompatible ways.
func TestKnownNonOwningSubcommandHelpDoesNotShadowOwners(t *testing.T) {
	for path, reason := range knownNonOwningSubcommandHelp {
		if subcommandHelpOwners[path] {
			t.Errorf("path %q is both an owner and a known non-owner (%q)", path, reason)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("known non-owner %q has no stated reason", path)
		}
	}
}

// TestSubcommandHelpOwnersFamiliesDispatchSubcommands verifies every key
// of subcommandHelpOwners is shaped like "<family> <sub>" where <family>
// really is a subcommand-dispatching command. A typo in a key silently
// disables an entry — which is how `skill workshop` went missing without
// anything complaining.
func TestSubcommandHelpOwnersFamiliesDispatchSubcommands(t *testing.T) {
	for path := range subcommandHelpOwners {
		parts := strings.Fields(path)
		if len(parts) < 2 {
			t.Errorf("subcommandHelpOwners key %q is not a \"<family> <sub>\" path", path)
			continue
		}
		if !commandsWithSubcommandDispatch[parts[0]] {
			t.Errorf("subcommandHelpOwners key %q names family %q, which is not in commandsWithSubcommandDispatch "+
				"— isSubcommandHelp can never match it", path, parts[0])
		}
	}
}
