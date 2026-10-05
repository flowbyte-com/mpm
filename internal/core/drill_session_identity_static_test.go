// drill_session_identity_static_test.go — §15 structural guard for the
// drill-run session identity invariant (§4).
//
// Pre-§6, ClaudeCodeHarness.Launch minted its own session_id with
// `h.sessionID = uuid.NewString()`. That silently broke
// drill_runs.session_id == tool_invocations.session_id. The §6 fix
// made Launch take the orchestrator's sessionID as a parameter and
// removed the mint; the post-fix API rejects an empty sessionID with
// a clear error.
//
// This file prevents the regression from being re-introduced by
// scanning the harness source for the forbidden patterns:
//
//  1. No `uuid.NewString()` call anywhere in the harness — a fresh
//     mint is the exact mechanism that produced the bug.
//  2. No import of `github.com/google/uuid` in the harness — that
//     import is only needed by the mint; if it reappears the mint is
//     about to follow.
//  3. Launch's signature must include a sessionID parameter — a
//     caller passing "" gets the rejection in (1) but if the param
//     vanishes, callers would silently drop their id and we'd be
//     back to the same bug shape.
//
// The structural guard is hermetic: it parses source, doesn't run
// any test that depends on Go's loader state, and fails loudly when
// the forbidden patterns reappear. §16 reintroduces the bug
// temporarily to confirm §3's assertions catch it; §15 here is the
// cheaper always-on tripwire.

package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestClaudeCodeHarness_NoSessionIDMintInSource is the §15 structural
// guard: ClaudeCodeHarness must not mint its own session_id. The
// pre-§6 mint was `h.sessionID = uuid.NewString()` inside Launch;
// the §6 fix removed both the call and the uuid import. If either
// returns, this test fires.
func TestClaudeCodeHarness_NoSessionIDMintInSource(t *testing.T) {
	const harnessPath = "drill_claude_code_harness.go"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, harnessPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", harnessPath, err)
	}

	// (1) No `uuid.NewString()` call anywhere in the harness.
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if x.Name == "uuid" && sel.Sel.Name == "NewString" {
			pos := fset.Position(call.Pos())
			t.Errorf("§15 static guard: forbidden uuid.NewString() call at %s:%d:%d — "+
				"ClaudeCodeHarness must NOT mint session_ids. Pre-§6 this was the "+
				"drill-run session identity bug source (Launch mints B, caller "+
				"already stored A, drill_runs ↔ tool_invocations join returns 0 rows). "+
				"§6 makes Launch take the orchestrator's sessionID; remove this call.",
				pos.Filename, pos.Line, pos.Column)
		}
		return true
	})

	// (2) No import of `github.com/google/uuid` in the harness — the
	// import exists solely to enable the mint. If anyone re-adds it,
	// the mint is about to follow.
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "github.com/google/uuid" {
			t.Errorf("§15 static guard: forbidden import of %q in %s — "+
				"the uuid package is not needed by the harness post-§6. "+
				"Re-adding it is the first step toward reintroducing the mint.",
				path, harnessPath)
		}
	}
}

// TestClaudeCodeHarness_LaunchAcceptsSessionID is the §15 structural
// guard on the API shape: Launch must take a sessionID parameter. The
// pre-§6 signature was `Launch(ctx, drill)` and the bug was that the
// harness minted its own id; the §6 signature is `Launch(ctx, drill,
// sessionID)` and an empty sessionID is rejected. If anyone removes
// the parameter (or makes it variadic with no-op behaviour for the
// empty case), the bug returns.
func TestClaudeCodeHarness_LaunchAcceptsSessionID(t *testing.T) {
	const harnessPath = "drill_claude_code_harness.go"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, harnessPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", harnessPath, err)
	}

	// Find the *ast.FuncDecl whose receiver type is *ClaudeCodeHarness
	// and whose name is "Launch". Confirm its parameter list ends
	// with `sessionID string`.
	var launch *ast.FuncDecl
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != "Launch" {
			continue
		}
		launch = fd
		break
	}
	if launch == nil {
		t.Fatal("§15 static guard: ClaudeCodeHarness.Launch not found in source")
	}

	if got := len(launch.Type.Params.List); got < 3 {
		t.Fatalf("§15 static guard: Launch has %d parameters, want >= 3 "+
			"(ctx, drill, sessionID). Pre-§6 the sessionID parameter was the "+
			"missing third arg; that absence was the bug. Restoring it is the "+
			"§6 contract.", got)
	}
	lastParam := launch.Type.Params.List[len(launch.Type.Params.List)-1]
	if len(lastParam.Names) != 1 || lastParam.Names[0].Name != "sessionID" {
		t.Errorf("§15 static guard: Launch's last parameter = %q, want %q. "+
			"The sessionID parameter must be named sessionID so the §6 "+
			"rejection-when-empty message reads naturally and the "+
			"parameter's intent is obvious at call sites.",
			fieldName(lastParam), "sessionID")
	}

	// Confirm sessionID's type is `string` (not a pointer, slice, etc.
	// — that would let a caller pass nil/empty-slice and bypass the
	// empty-string check). Strip pointer wrapping if present.
	typ := lastParam.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if ident, ok := typ.(*ast.Ident); !ok || ident.Name != "string" {
		t.Errorf("§15 static guard: Launch's sessionID parameter type = %s, "+
			"want string. Anything else (pointer, interface, struct) lets a "+
			"caller pass nil and bypass the empty-rejection.", fieldType(lastParam))
	}
}

// TestDrillHandlerCLI_RunClaudeCodeDrillPassesSessionID is the §15
// static guard on the orchestrator side: the scheduler and the CLI
// must pass the orchestrator-owned sessionID through to
// harness.Launch. Pre-§6 both sides had `_ = sessionID // harness
// mints its own` comments — a comment-only marker for a real bug.
// The §6 fix removed those comments and threaded sessionID into
// Launch. If either file silently drops sessionID again, this trips.
func TestDrillHandlerCLI_RunClaudeCodeDrillPassesSessionID(t *testing.T) {
	cases := []struct {
		path string
		// sentinelForbidden is a phrase that MUST NOT appear in the
		// file post-§6. Each phrase represents the pre-§6 comment
		// shape that documented the bug (rather than fixed it).
		sentinelForbidden []string
	}{
		{
			path: "../../internal/scheduler/drill_handler.go",
			sentinelForbidden: []string{
				"// harness mints its own",
				"_ = sessionID // reserved for future telemetry",
			},
		},
		{
			path: "../../cmd/mpm/drill_cmds.go",
			sentinelForbidden: []string{
				"// harness owns its session",
				"_ = sessionID // harness owns its session",
			},
		},
	}

	for _, c := range cases {
		// AST scan for `_ = sessionID` — the dead-letter suppression
		// is the load-bearing marker that sessionID is being
		// thrown away. Post-§6 sessionID is passed through to
		// Launch; if it isn't used, the suppression reappears.
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, c.path, nil, parser.ParseComments)
		if err != nil {
			t.Errorf("parse %s: %v", c.path, err)
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			// Looking for `_ = sessionID` — single LHS ident "_",
			// single RHS ident "sessionID".
			if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			lhs, ok := assign.Lhs[0].(*ast.Ident)
			if !ok || lhs.Name != "_" {
				return true
			}
			rhs, ok := assign.Rhs[0].(*ast.Ident)
			if !ok || rhs.Name != "sessionID" {
				return true
			}
			pos := fset.Position(assign.Pos())
			t.Errorf("§15 static guard: %s:%d:%d contains a `_ = sessionID` "+
				"suppression. Pre-§6 this was the marker that sessionID was "+
				"being thrown away — exactly the drill-run session identity "+
				"bug (drill_runs.session_id = A, harness.Launch mint B, "+
				"tool_invocations.session_id = B, no join). §6 fixes this by "+
				"threading sessionID into harness.Launch; remove the suppression.",
				pos.Filename, pos.Line, pos.Column)
			return true
		})

		// Sentinel phrases — the textual comment shape that
		// documented the bug pre-§6. Same intent as the AST scan,
		// but a separate failure mode for older commenting styles
		// (e.g. someone re-adding the comment without re-adding
		// the suppression line).
		for _, phrase := range c.sentinelForbidden {
			// Re-parse with comments to scan docstrings too.
			fileWithComments, err := parser.ParseFile(fset, c.path, nil, parser.ParseComments)
			if err != nil {
				continue
			}
			for _, cg := range fileWithComments.Comments {
				if strings.Contains(cg.Text(), phrase) {
					t.Errorf("§15 static guard: %s contains sentinel %q. "+
						"Pre-§6 this comment was the marker that the "+
						"orchestrator was throwing away the sessionID "+
						"the caller minted. §6 fixes this; remove the comment.",
						c.path, phrase)
				}
			}
		}
	}
}

// fieldName formats an *ast.Field as a human-readable parameter
// name. Used only in failure messages.
func fieldName(f *ast.Field) string {
	if len(f.Names) == 0 {
		return "(unnamed)"
	}
	names := make([]string, len(f.Names))
	for i, n := range f.Names {
		names[i] = n.Name
	}
	return strings.Join(names, ",")
}

// fieldType formats an *ast.Field's type as a short string. Used
// only in failure messages.
func fieldType(f *ast.Field) string {
	switch t := f.Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return "*" + ident.Name
		}
	}
	return "(complex)"
}
