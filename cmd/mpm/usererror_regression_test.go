package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoNewDirectStderrWrites enforces the worry #3 invariant: every
// user-facing CLI error goes through internal/usererror. Direct
// fmt.Fprintf(os.Stderr, ...) is allowed only for non-error paths
// (JSON envelopes, multi-line usage help).
//
// This is a static-analysis test that walks cmd/mpm/*.go and fails if
// any new fmt.Fprintf(os.Stderr, ...) call appears outside the
// explicit allow-list. The list is intentionally small — when in
// doubt, use usererror.{Error,Warn,Notice,Usage}.
//
// Adding to this list requires a justifying comment in the source:
// every whitelisted site must have a one-line reason.
func TestNoNewDirectStderrWrites(t *testing.T) {
	allowedSites := map[string]string{
		"call.go:93":        "JSON error envelope for `mpm call` — must be raw JSON, not user-formatted",
		"simple_cmds.go:618": "Multi-line usage help text — structured output, not a single error message",
		"simple_cmds.go:620": "Multi-line usage help text — structured output, not a single error message",
	}

	dir := "."
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}

	type hit struct {
		file string
		line int
		text string
	}
	var hits []hit

	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			filename := fset.Position(f.Pos()).Filename
			if !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Fprintf" {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok || x.Name != "fmt" {
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				sel2, ok := call.Args[0].(*ast.SelectorExpr)
				if !ok || sel2.Sel.Name != "Stderr" {
					return true
				}
				x2, ok := sel2.X.(*ast.Ident)
				if !ok || x2.Name != "os" {
					return true
				}
				pos := fset.Position(call.Pos())
				hits = append(hits, hit{
					file: filepath.Base(pos.Filename),
					line: pos.Line,
					text: codeAt(fset.Position(call.Pos()), filename),
				})
				return true
			})
		}
	}

	for _, h := range hits {
		key := h.file + ":" + itoa(h.line)
		reason, allowed := allowedSites[key]
		if !allowed {
			t.Errorf("direct fmt.Fprintf(os.Stderr,...) at %s:%d — use usererror.{Error,Warn,Notice,Usage} instead.\n"+
				"  Code: %s\n"+
				"  Allowed sites (with reasons):\n%s",
				h.file, h.line, h.text, formatAllowList(allowedSites))
			continue
		}
		if !hasCommentNearby(filepath.Join(".", h.file), h.line, reason) {
			t.Errorf("allowed site %s:%d missing justifying comment near line %d:\n"+
				"  expected substring: %q\n"+
				"  add the comment immediately above the call site so future readers know why this is exempt",
				h.file, h.line, h.line, reason)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func codeAt(pos token.Position, filename string) string {
	data, err := os.ReadFile(filename)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if pos.Line-1 < 0 || pos.Line-1 >= len(lines) {
		return ""
	}
	line := strings.TrimSpace(lines[pos.Line-1])
	if len(line) > 80 {
		line = line[:80] + "..."
	}
	return line
}

func formatAllowList(m map[string]string) string {
	var b strings.Builder
	for site, reason := range m {
		b.WriteString("    ")
		b.WriteString(site)
		b.WriteString(" — ")
		b.WriteString(reason)
		b.WriteString("\n")
	}
	return b.String()
}

func hasCommentNearby(path string, line int, reason string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	start := line - 11
	if start < 0 {
		start = 0
	}
	end := line - 1
	if end > len(lines) {
		end = len(lines)
	}
	for i := start; i < end; i++ {
		if strings.Contains(lines[i], reason) {
			return true
		}
	}
	return false
}
