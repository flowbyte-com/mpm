// drill_session_identity_static_test.go — §15 AST guard for the
// orchestrator-side dead-letter suppression pattern.
//
// The behavioral tests in this package (TestClaudeCodeHarness_
// SessionIdentity_CallerOwned) and in internal/scheduler/drill_
// handler_test.go pin the §4 invariant end-to-end. They fail loudly
// when Launch mints its own id or when the scheduler/CLI drop the
// orchestrator's sessionID — but only when the regression actually
// breaks the audit-row join. A subtler regression reintroduces the
// suppression pattern with a different variable name, e.g.:
//
//	var S = uuid.NewString()
//	_ = sessionID // suppress the orchestrator's id
//	h.Launch(ctx, drill, S) // use the locally-minted one instead
//
// In that shape the audit-row join still passes (both ids are
// non-empty UUIDs) but the orchestrator's contract — "I gave you a
// sessionID, you used it" — is silently violated. Behavioral tests
// can't cheaply pin this because they only assert on the join result,
// not on which sessionID source reached Launch.
//
// This file is the cheaper always-on tripwire for that suppression
// pattern. It scans the orchestrator sources (scheduler + CLI) for
// the load-bearing dead-letter marker `_ = sessionID` and rejects the
// pattern on sight. The harness-side checks
// (TestClaudeCodeHarness_NoSessionIDMintInSource and
// TestClaudeCodeHarness_LaunchAcceptsSessionID from earlier drafts)
// were dropped during consolidation: they were redundant with the
// behavioral test (any Launch-side mint regression fails
// TestClaudeCodeHarness_SessionIdentity_CallerOwned) and with
// compilation (any Launch signature regression breaks every caller).

package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDrillHandlerCLI_RunClaudeCodeDrillPassesSessionID is the §15
// AST guard for the orchestrator-side dead-letter suppression. The
// scheduler and the CLI must pass the orchestrator-owned sessionID
// through to harness.Launch. Pre-§6 both sides had `_ = sessionID
// // harness mints its own` comments — a comment-only marker for a
// real bug. The §6 fix removed those comments and threaded sessionID
// into Launch. If either file silently drops sessionID again, this
// trips.
//
// Behavioral tests in
// internal/scheduler/drill_handler_test.go (TestDrillHandler_ClaudeCode
// Path_SessionIdentityEndToEnd) cover the join-result consequence but
// not the dead-letter pattern specifically. This AST guard fills that
// gap as the cheapest possible pin.
func TestDrillHandlerCLI_RunClaudeCodeDrillPassesSessionID(t *testing.T) {
	cases := []struct {
		path string
	}{
		{path: "../../internal/scheduler/drill_handler.go"},
		{path: "../../cmd/mpm/drill_cmds.go"},
	}

	for _, c := range cases {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, c.path, nil, parser.ParseComments)
		if err != nil {
			t.Errorf("parse %s: %v", c.path, err)
			continue
		}

		// AST scan for `_ = sessionID` — the dead-letter suppression
		// is the load-bearing marker that sessionID is being thrown
		// away. Post-§6 sessionID is passed through to Launch; if it
		// isn't used, the suppression reappears.
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
	}
}
