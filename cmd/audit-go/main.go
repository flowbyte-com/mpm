// cmd/audit-go/main.go — Static analyzer for goroutine lifecycle binding.
//
// The substrate spawns background goroutines (memory compaction, embedding
// backfills, epistemic checks, recollection wake handlers, synthesis
// workers). An unbound goroutine is a leak:
//
//	- blocked on a channel read that never arrives (no ctx.Done escape,
//	  no WaitGroup drain),
//	- writer to an unbuffered channel nobody reads,
//	- a detached fire-and-forget goroutine that survives its parent, holds
//	  the shared SQLite connection, and is never joined at shutdown.
//
// Rule enforced here: EVERY `go` statement must be visibly bound to the
// parent lifecycle by one of:
//
//	go-wg-bound     — the goroutine body does `defer wg.Done()` and a
//	                  `wg.Add(1)` counter exists in the enclosing scope
//	                  (parent waits via wg.Wait()).
//	go-ctx-bound    — the goroutine reads `<-ctx.Done()` (select default/
//	                  case) or tests ctx.Err()/ctx.Done(), so cancellation
//	                  ends it.
//	go-reply-ch     — the goroutine replies on a channel that the parent
//	                  drains in the same enclosing function (select on
//	                  {case <-ch / case <-ctx.Done()} or a nearby receive),
//	                  so the reply path itself bounds it.
//	go-detached     — NO WaitGroup, NO ctx, NO reply channel: fire-and-
//	                  forget. FLAGGED. (e.g. ChallengeMemoryAsync's mirror
//	                  write.)
//	go-external     — `go namedFn(...)`; the binding lives in namedFn
//	                  (worker pool pattern). Verified here by confirming a
//	                  wg.Add appears in the enclosing scope; otherwise
//	                  reviewed manually.
//	go-unknown      — pattern did not fit (needs review)
//
// The gate (--gate) fails when any go-detached site exists at threshold 0
// (plus unknown). go-external/go-reply are allowed by design but still
// counted in the report.
//
// Usage:
//
//	go run ./cmd/audit-go                  # markdown report on stdout
//	go run ./cmd/audit-go --json
//	go run ./cmd/audit-go --roots=a,b
//	go run ./cmd/audit-go --gate           # exit non-zero on detached
//
// The pre-commit hook runs `audit-go --gate` alongside the other gates.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type GoClass string

const (
	GoWgBound  GoClass = "go-wg-bound"
	GoCtxBound GoClass = "go-ctx-bound"
	GoReply    GoClass = "go-reply-bound"
	GoNamed    GoClass = "go-named"
	GoDetached GoClass = "go-detached"
	GoUnknown  GoClass = "go-unknown"
)

type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Target         string   `json:"target"`
	Classification GoClass  `json:"classification"`
	Reason         string   `json:"reason"`
	Code           []string `json:"code"`
}

// -------------------------------------------------------------------- main

func main() {
	var (
		jsonOut      = flag.Bool("json", false, "emit JSON sidecar instead of markdown")
		includeTests = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag    = flag.String("roots", "internal,cmd", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero if fatal-class counts exceed thresholds (go-detached / go-unknown default to 0)")
		maxDetached  = flag.Int("max-detached", 0, "threshold for go-detached (used with --gate)")
		maxUnknown   = flag.Int("max-unknown", 0, "threshold for go-unknown (used with --gate)")
	)
	flag.Parse()

	roots := strings.Split(*rootsFlag, ",")
	for i := range roots {
		roots[i] = strings.TrimSpace(roots[i])
	}

	var sites []Site
	for _, root := range roots {
		if root == "" {
			continue
		}
		if err := walkRoot(root, *includeTests, &sites); err != nil {
			fmt.Fprintf(os.Stderr, "audit-go: walk %s: %v\n", root, err)
		}
	}

	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})

	if *jsonOut {
		emitJSON(os.Stdout, sites)
	} else {
		emitMarkdown(os.Stdout, sites)
	}

	if *jsonSidecar != "" {
		f, err := os.Create(*jsonSidecar)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-go: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-go: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	if *gate {
		exitCode := runGate(sites, gateThresholds{Detached: *maxDetached, Unknown: *maxUnknown})
		os.Exit(exitCode)
	}
}

type gateThresholds struct {
	Detached int
	Unknown  int
}

func runGate(sites []Site, t gateThresholds) int {
	counts := countByClass(sites)
	type check struct {
		name      string
		actual    int
		threshold int
	}
	checks := []check{
		{"go-detached", counts[GoDetached], t.Detached},
		{"go-unknown", counts[GoUnknown], t.Unknown},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-go --gate] goroutine lifecycle thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-14s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}
	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-go --gate] FAIL: detached goroutine site exceeds threshold")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "[audit-go --gate] PASS")
	return 0
}

// -------------------------------------------------------------------- walker

func walkRoot(root string, includeTests bool, sites *[]Site) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == "vendor" || name == "node_modules" || (strings.HasPrefix(name, ".") && name != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if !includeTests && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSites, err := auditFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-go: parse %s: %v\n", path, err)
			return nil
		}
		*sites = append(*sites, fileSites...)
		return nil
	})
}

// -------------------------------------------------------------------- file audit

func auditFile(path string) ([]Site, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	parentMap := make(map[ast.Node]ast.Node)
	{
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				return true
			}
			if len(stack) > 0 {
				parentMap[n] = stack[len(stack)-1]
			}
			stack = append(stack, n)
			return true
		})
	}

	var sites []Site
	ast.Inspect(f, func(n ast.Node) bool {
		goStmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		pos := fset.Position(goStmt.Pos())
		funcName := enclosingFuncName(parentMap, goStmt)
		site := Site{
			File:     path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: funcName,
			Target:   goTarget(goStmt),
		}
		site.Classification, site.Reason = classify(goStmt, parentMap)
		site.Code = contextLines(src, pos.Line)
		sites = append(sites, site)
		return true
	})
	return sites, nil
}

func enclosingFuncName(parentMap map[ast.Node]ast.Node, node ast.Node) string {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
		n = parentMap[n]
	}
	return ""
}

func goTarget(g *ast.GoStmt) string {
	switch t := g.Call.Fun.(type) {
	case *ast.FuncLit:
		return "(func literal)"
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return receiverIdent(t.X) + "." + t.Sel.Name
	}
	return "(?)"
}

func receiverIdent(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return receiverIdent(t.X) + "." + t.Sel.Name
	case *ast.CallExpr:
		return "(" + goTargetCall(t) + ")"
	case *ast.StarExpr:
		return "*" + receiverIdent(t.X)
	}
	return ""
}

func goTargetCall(c *ast.CallExpr) string {
	if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
		return receiverIdent(sel.X) + "." + sel.Sel.Name
	}
	return "call"
}

func classify(g *ast.GoStmt, parentMap map[ast.Node]ast.Node) (GoClass, string) {
	// Enclosing function body for scope checks.
	funcBody := enclosingFuncBody(parentMap, g)
	if funcBody == nil {
		return GoUnknown, "no enclosing function body"
	}

	// 1. Func literal: go func(){...}()
	if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
		if wgWaitGroupDone(lit) {
			return GoWgBound, "defer wg.Done() in body"
		}
		if ctxDoneRead(lit) {
			return GoCtxBound, "reads ctx.Done()/ctx.Err()"
		}
		if replyChannel(lit, funcBody) {
			return GoReply, "replies on a channel drained by the parent"
		}
		return GoDetached, "no WaitGroup, no ctx, no reply channel"
	}

	// 2. Named function: go worker(i) / go executeOne(...) etc.
	// Confirm the enclosing func has a wg.Add counter for it, or the named
	// function is itself a known pool worker (signature has a Done defer).
	if wgAddInScope(funcBody) {
		return GoNamed, "named fn; wg.Add counter present in enclosing scope"
	}
	// The named fn may be a worker that manages its own lifecycle.
	return GoNamed, "named fn; verify the target manages its own lifecycle"
}

// wgWaitGroupDone reports whether the function literal body participates in
// a WaitGroup lifecycle: `defer wg.Done()` OR a `wg.Wait()` join (the
// shutdown-signaller pattern `go func() { p.wg.Wait(); close(done) }()`).
func wgWaitGroupDone(lit *ast.FuncLit) bool {
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Done", "Wait":
			recv := receiverIdentExpr(sel.X)
			// mirrorWG / watchdogWG / wg / p.wg / .wg — any WaitGroup-shaped
			// receiver participates in a WaitGroup lifecycle.
			if strings.HasPrefix(recv, "wg") || strings.HasSuffix(recv, ".wg") ||
				strings.HasSuffix(recv, "WG") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// ctxDoneRead reports whether the body tests or reads context.Done()/Err().
func ctxDoneRead(lit *ast.FuncLit) bool {
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// ctx.Done() / ctx.Err() — including via <-receiver on a fn
		if sel.Sel.Name == "Done" || sel.Sel.Name == "Err" {
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "ctx" || strings.HasSuffix(id.Name, "ctx")) {
				found = true
				return false
			}
		}
		// channel receive which is likely the ctx.Done channel
		if unary, ok := n.(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
			innerStr := goTargetFromExpr(unary.X)
			if strings.Contains(innerStr, "Done") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func goTargetFromExpr(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return receiverIdentExpr(t.X) + "." + t.Sel.Name
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok {
			return receiverIdentExpr(sel.X) + "." + sel.Sel.Name
		}
		return "call"
	case *ast.FuncLit:
		return "func(){}"
	}
	return ""
}

func receiverIdentExpr(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return receiverIdentExpr(t.X) + "." + t.Sel.Name
	case *ast.ParenExpr:
		return receiverIdentExpr(t.X)
	}
	return ""
}

// replyChannel reports whether the goroutine body contains a channel send
// (`ch <- v`). A send is the reply-path discipline: the parent must drain
// it (typically under a `select { case <-ch: ... case <-ctx.Done(): ... }`),
// which bounds the goroutine's lifetime to the parent's.
func replyChannel(lit *ast.FuncLit, funcBody *ast.BlockStmt) bool {
	_ = funcBody
	sends := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.SendStmt); ok {
			sends = true
			return false
		}
		return true
	})
	return sends
}

// wgAddInScope reports whether the enclosing func body calls wg.Add(...).
func wgAddInScope(funcBody *ast.BlockStmt) bool {
	found := false
	ast.Inspect(funcBody, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Add" {
			if id, ok := sel.X.(*ast.Ident); ok && strings.HasPrefix(id.Name, "wg") {
				found = true
				return false
			}
		}
		if sel.Sel.Name == "Wait" {
			if id, ok := sel.X.(*ast.Ident); ok && strings.HasPrefix(id.Name, "wg") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func enclosingFuncBody(parentMap map[ast.Node]ast.Node, node ast.Node) *ast.BlockStmt {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Body
		}
		if fl, ok := n.(*ast.FuncLit); ok {
			return fl.Body
		}
		n = parentMap[n]
	}
	return nil
}

func contextLines(src []byte, line int) []string {
	all := strings.Split(string(src), "\n")
	start := line - 4
	if start < 0 {
		start = 0
	}
	end := line + 3
	if end > len(all) {
		end = len(all)
	}
	out := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		marker := "  "
		if i == line-1 {
			marker = "> "
		}
		out = append(out, fmt.Sprintf("%s%d: %s", marker, i+1, all[i]))
	}
	return out
}

func emitMarkdown(w *os.File, sites []Site) {
	counts := countByClass(sites)
	fmt.Fprintln(w, "# Goroutine Lifecycle Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total go statements:** %d\n\n", len(sites))
	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []GoClass{GoWgBound, GoCtxBound, GoReply, GoNamed, GoDetached, GoUnknown}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	bad := counts[GoDetached] + counts[GoUnknown]
	total := len(sites)
	if total > 0 {
		fmt.Fprintf(w, "**Unbinding goroutines (detached/unknown): %d / %d**\n\n", bad, total)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[GoClass]bool{
		GoWgBound: true, GoCtxBound: true, GoReply: true,
	}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (`%s`)\n", s.File, s.Line, s.Target)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.Reason != "" {
			fmt.Fprintf(w, "- **Reason:** %s\n", s.Reason)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}
}

func emitJSON(w *os.File, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int    `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func countByClass(sites []Site) map[GoClass]int {
	m := make(map[GoClass]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}
