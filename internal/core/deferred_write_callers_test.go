// deferred_write_callers_test.go — every production caller must HANDLE
// the deferred queue's refusal.
//
// WHY THIS FILE EXISTS.
//
// DeferDurableWrite returns false when the obligation could not be
// accepted — the queue was full, or the queue was closed. Its own
// documentation says:
//
//	It returns false when the obligation could not be accepted (the
//	queue is full, or the queue is closed), in which case fn was NOT
//	called and the caller owns the failure.
//
// A caller that IGNORES that result is claiming a write was queued when
// it was not. The independent review of PR #235 found three production
// call sites that discarded it — including the failure-path audit row,
// which the PR itself classifies as essential audit evidence.
//
// This test pins the invariant at the SOURCE level, so a future call
// site cannot reintroduce the defect by accident. It is a source
// invariant rather than a behavioural one on purpose: the refusal path
// needs a saturated process-wide queue to reach, and a test that
// saturated the shared queue would break every test that followed it.
//
// The scan covers the three modules that hold production call sites:
// this package, internal/core/tools, and cmd/mpm-mcp.

package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// deferredWriteCallNames are the entry points whose boolean result the
// caller owns.
var deferredWriteCallNames = map[string]bool{
	"DeferDurableWrite":       true,
	"LogAuditDeferred":        true,
	"RecordRetrievalDeferred": true,
}

// deferredWriteCallerDirs are the directories scanned for production
// call sites, relative to this file's directory.
var deferredWriteCallerDirs = []string{
	".",
	"tools",
	"../../cmd/mpm-mcp",
}

// TestDeferredWriteCallers_HandleTheRefusal pins that every production
// caller of the deferred queue's entry points USES the boolean result.
//
// A call whose result is discarded as a statement — or assigned to `_` —
// is a caller that cannot tell whether the write was queued.
func TestDeferredWriteCallers_HandleTheRefusal(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the source file")
	}
	base := filepath.Dir(thisFile)

	scanned := 0
	for _, dir := range deferredWriteCallerDirs {
		target := filepath.Clean(filepath.Join(base, dir))
		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(target, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			discarded, total := discardedDeferredWriteCalls(f)
			scanned += total
			for _, call := range discarded {
				t.Errorf("%s: %s discards its refusal result — the caller must handle it "+
					"(a rejected obligation was never queued and will never be written)",
					path, call)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("the scan found no deferred-write call sites at all; the scanner is broken")
	}
}

// discardedDeferredWriteCalls returns a description of every deferred
// write call in the file whose result is discarded, and the total number
// of deferred write calls in the file.
func discardedDeferredWriteCalls(f *ast.File) ([]string, int) {
	var out []string
	total := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if deferredWriteCallName(call) != "" {
				total++
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.ExprStmt:
			// A bare `f(...)` statement: the result is discarded.
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if name := deferredWriteCallName(call); name != "" {
					out = append(out, name+"()")
				}
			}
		case *ast.DeferStmt:
			if call, ok := stmt.Call.Fun.(*ast.CallExpr); ok {
				if name := deferredWriteCallName(call); name != "" {
					out = append(out, name+"()")
				}
			}
		case *ast.GoStmt:
			if call, ok := stmt.Call.Fun.(*ast.CallExpr); ok {
				if name := deferredWriteCallName(call); name != "" {
					out = append(out, name+"()")
				}
			}
		case *ast.AssignStmt:
			// `_, _ = f(...)` discards the result too.
			if len(stmt.Rhs) == 1 {
				if call, ok := stmt.Rhs[0].(*ast.CallExpr); ok {
					if name := deferredWriteCallName(call); name != "" && allBlank(stmt.Lhs) {
						out = append(out, name+"()")
					}
				}
			}
		}
		return true
	})
	return out, total
}

// deferredWriteCallName returns the name of the deferred-write entry
// point a call invokes, or "" if it is not one.
func deferredWriteCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if deferredWriteCallNames[fun.Name] {
			return fun.Name
		}
	case *ast.SelectorExpr:
		if deferredWriteCallNames[fun.Sel.Name] {
			return fun.Sel.Name
		}
	}
	return ""
}

// allBlank reports whether every name in the list is the blank
// identifier.
func allBlank(names []ast.Expr) bool {
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		ident, ok := n.(*ast.Ident)
		if !ok || ident.Name != "_" {
			return false
		}
	}
	return true
}
