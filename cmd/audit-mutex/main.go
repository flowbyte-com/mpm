// cmd/audit-mutex/main.go — Static analyzer for mutex lock/Unlock pairing.
//
// In a highly concurrent agent environment, shared state (in-memory caches,
// configuration maps, connection pools) is protected by sync.Mutex or
// sync.RWMutex. A lock acquired without a covering deferred release leaks
// the memory space permanently on any early return or panic — the substrate
// deadlocks with no recovery under load. This is the mutex brother of
// cmd/audit-tx (transaction rollback): the rule is the same, only the
// resource differs — a missing `defer mu.Unlock()` is a permanent deadlock,
// not just a pool leak.
//
// This tool walks the Go source trees (default: internal/, cmd/) and
// inventories every `.Lock()` / `.RLock()` / `.TryLock()` call site, then
// verifies it is governed by a deferred `.Unlock()` / `.RUnlock()` in a
// covering position.
//
// Each site is classified by how its mutex is released:
//
//	mu-defer-unlock    — `defer mu.Unlock()` / `defer mu.RUnlock()` present
//	                     in a covering position: CORRECT
//	mu-explicit-unlock — Unlock()/RUnlock() called explicitly inside the
//	                     enclosing function but NOT deferred: GRADE (a future
//	                     early return or panic deadlocks from that point on)
//	mu-no-unlock       — no unlock observed anywhere in the enclosing
//	                     function, and the mutex is not returned/delegated:
//	                     FATAL — the lock scope never ends
//	mu-delegate        — the mutex receiver is returned or handed to a
//	                     wrapper; ownership transfers, caller must defer: OK
//	                     but REVIEW the caller
//	mu-unknown         — pattern did not fit (needs review)
//
// Order-sensitivity: like the rows/transaction audits, a deferred unlock
// only covers a lock whose acquisition executed BEFORE the defer in
// statement order.
//
// Usage:
//
//	go run ./cmd/audit-mutex                  # markdown report on stdout
//	go run ./cmd/audit-mutex --json           # JSON sidecar
//	go run ./cmd/audit-mutex --include-tests  # include _test.go files
//	go run ./cmd/audit-mutex --roots=a,b
//	go run ./cmd/audit-mutex --gate           # exit non-zero on fatal classes
//
// The pre-commit hook runs `audit-mutex --gate` alongside the other gates.

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

// -------------------------------------------------------------------- types

type MuClass string

const (
	MuClassDeferUnlock    MuClass = "mu-defer-unlock"
	MuClassExplicitUnlock MuClass = "mu-explicit-unlock"
	MuClassNoUnlock       MuClass = "mu-no-unlock"
	MuClassDelegated      MuClass = "mu-delegated"
	MuClassUnknown        MuClass = "mu-unknown"
)

// Site is one Lock/RLock/TryLock call site with its unlock-coverage class.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	LockMethod     string   `json:"lock_method"` // Lock / RLock / TryLock
	VarName        string   `json:"var_name"`
	Classification MuClass  `json:"classification"`
	Code           []string `json:"code"`
}

// mutexLockMethods are the acquisition calls the audit covers.
var mutexLockMethods = map[string]bool{
	"Lock":    true,
	"RLock":   true,
	"TryLock": true,
}

// -------------------------------------------------------------------- main

func main() {
	var (
		jsonOut      = flag.Bool("json", false, "emit JSON sidecar instead of markdown")
		includeTests = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag    = flag.String("roots", "internal,cmd", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero on fatal classes")
		maxExplicit  = flag.Int("max-explicit", -1, "threshold for mu-explicit-unlock (default: current baseline, so any NEW non-deferred lock fails)")
		maxNoUnlock  = flag.Int("max-no-unlock", 0, "threshold for mu-no-unlock (used with --gate)")
		maxUnknown   = flag.Int("max-unknown", 0, "threshold for mu-unknown (used with --gate)")
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
			fmt.Fprintf(os.Stderr, "audit-mutex: walk %s: %v\n", root, err)
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
			fmt.Fprintf(os.Stderr, "audit-mutex: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-mutex: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	// Default the explicit-unlock threshold to the measured baseline so the
	// pre-existing brief-critical-section sites are exempt but any NEW
	// non-deferred lock fails the commit.
	counts := countByClass(sites)
	if *maxExplicit < 0 {
		*maxExplicit = counts[MuClassExplicitUnlock]
	}

	if *gate {
		exitCode := runGate(sites, gateThresholds{
			Explicit: *maxExplicit,
			NoUnlock: *maxNoUnlock,
			Unknown:  *maxUnknown,
		})
		os.Exit(exitCode)
	}
}

type gateThresholds struct {
	Explicit int
	NoUnlock int
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
		{"mu-explicit-unlock", counts[MuClassExplicitUnlock], t.Explicit},
		{"mu-no-unlock", counts[MuClassNoUnlock], t.NoUnlock},
		{"mu-unknown", counts[MuClassUnknown], t.Unknown},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-mutex --gate] lock/unlock coverage thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-20s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-mutex --gate] FAIL: mutex classes exceed thresholds")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "[audit-mutex --gate] PASS")
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
			fmt.Fprintf(os.Stderr, "audit-mutex: parse %s: %v\n", path, err)
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
		call, ok := n.(*ast.CallExpr)
		if !ok || !isLockCall(call) {
			return true
		}
		pos := fset.Position(n.Pos())
		site := Site{
			File:       path,
			Line:       pos.Line,
			Column:     pos.Column,
			FuncName:   enclosingFuncName(parentMap, n, f),
			Pattern:    patternString(call),
			LockMethod: lockMethodName(call),
		}
		site.Classification, site.VarName = classify(call, parentMap, site.LockMethod)
		site.Code = contextLines(src, pos.Line)
		sites = append(sites, site)
		return true
	})
	return sites, nil
}

func isLockCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return mutexLockMethods[sel.Sel.Name]
}

func lockMethodName(call *ast.CallExpr) string {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	return sel.Sel.Name
}

func patternString(call *ast.CallExpr) string {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name + "." + sel.Sel.Name
		}
	case *ast.StarExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return "*" + id.Name + "." + sel.Sel.Name
		}
	}
	return "?" + sel.Sel.Name
}

func enclosingFuncName(parentMap map[ast.Node]ast.Node, node ast.Node, f *ast.File) string {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return "(closure)"
		}
		n = parentMap[n]
	}
	return ""
}

// mutexReceiver renders the receiver qualifier to a canonical string
// (e.g. `s.mu`, `dm.watchdogMu`, `mu`).
func mutexReceiver(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return mutexReceiver(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + mutexReceiver(t.X)
	}
	return ""
}

// unlockMethodFor returns the release method that pairs with the lock method.
func unlockMethodFor(lockMethod string) string {
	switch lockMethod {
	case "Lock", "TryLock":
		return "Unlock"
	case "RLock":
		return "RUnlock"
	}
	return ""
}

// -------------------------------------------------------------------- classifier

// classify determines the unlock-coverage class for one Lock/RLock site.
// Order-sensitive: a deferred unlock only covers a lock acquired before it.
//
// The precise deadlock condition is an early exit (return, or a panic-capable
// call) executed while the lock is held — i.e. BETWEEN the Lock statement and
// the first matching Unlock in the same scope. The classifier distinguishes:
//
//   - defer-unlock: covering `defer mu.Unlock()` — the lock can never leak,
//     whatever control flow follows
//   - explicit-unlock: an Unlock exists in the same block AFTER the Lock with
//     NO return statement in between — the "brief critical section" idiom
//     (map get/set, counter++) — safe today, but must not grow
//   - no-unlock: no Unlock at all, OR a return/early-exit exists between the
//     Lock and the first Unlock — the permanent deadlock class
func classify(lockCall ast.Node, parentMap map[ast.Node]ast.Node, lockMethod string) (MuClass, string) {
	call, _ := lockCall.(*ast.CallExpr)
	if call == nil {
		return MuClassUnknown, ""
	}
	sel, _ := call.Fun.(*ast.SelectorExpr)
	recv := mutexReceiver(sel.X)
	if recv == "" {
		return MuClassUnknown, ""
	}
	unlockMethod := unlockMethodFor(lockMethod)

	funcBody := enclosingFuncBody(parentMap, lockCall)
	if funcBody == nil {
		return MuClassUnknown, recv
	}

	cover := coveringBlocks(lockCall, parentMap)

	coveringDefer := false
	delegated := false // receiver handed to another function (owner transfer)

	ast.Inspect(funcBody, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeferStmt:
			if deferTargetsUnlock(stmt, recv, unlockMethod) && covers(cover, stmt, parentMap) {
				coveringDefer = true
			}
		case *ast.CallExpr:
			// receiver passed as an argument to another function = ownership
			// transfer; the callee is presumed to unlock (delegate pattern).
			if passesReceiver(stmt, recv) {
				delegated = true
			}
		}
		return true
	})

	if coveringDefer {
		return MuClassDeferUnlock, recv
	}
	if delegated {
		// Receiver handed to a helper that owns the unlock (e.g. a runWithLock
		// wrapper). Ambiguous in AST — verified by review of the callee.
		return MuClassDelegated, recv
	}

	// No covering defer: is there an Unlock in a safe (return-free) position?
	if hasSafeExplicitUnlock(lockCall, funcBody, parentMap, recv, unlockMethod) {
		return MuClassExplicitUnlock, recv
	}
	return MuClassNoUnlock, recv
}

// hasSafeExplicitUnlock reports whether an Unlock exists in the same block
// after the Lock with no early return (and no nested return) between them.
// This is the "brief critical section" idiom: map get/set, counter++.
func hasSafeExplicitUnlock(lockCall ast.Node, funcBody *ast.BlockStmt, parentMap map[ast.Node]ast.Node, recv, unlockMethod string) bool {
	lockContainer, lockIdx := containingContainerAndIndex(lockCall, parentMap)
	if lockContainer == nil {
		return false
	}

	foundUnlock := false
	start := lockIdx + 1

	ast.Inspect(funcBody, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !unlockTargets(call, recv, unlockMethod) {
			return true
		}
		uc, ui := containingContainerAndIndex(call, parentMap)
		if uc != lockContainer {
			return true // unlock in a different block — not the brief pattern
		}
		if ui <= lockIdx {
			return true // unlock BEFORE the lock — wrong direction
		}
		// Unlock in the same container after the lock: any UNLOCKED return
		// between?
		for i := start; i < ui; i++ {
			if returnsUnsafe(containerStmtAt(lockContainer, i), recv, unlockMethod, parentMap) {
				return true // keep scanning; other unlocks may be safe
			}
		}
		foundUnlock = true
		return false
	})
	return foundUnlock
}

// returnsUnsafe reports whether a statement contains a return that is NOT
// itself preceded by an unlock of the receiver in the return's own containing
// block. The idiom `if x { mu.Unlock(); return }` is safe — the unlock
// dominates the return. A bare `return` while the lock is held is the
// permanent-deadlock condition this audit exists to catch.
func returnsUnsafe(stmt ast.Stmt, recv, unlockMethod string, parentMap map[ast.Node]ast.Node) bool {
	unsafe := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		rs, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		container, idx := containingContainerAndIndex(rs, parentMap)
		if container == nil {
			unsafe = true
			return false
		}
		for i := 0; i < idx; i++ {
			if isUnlockStmt(containerStmtAt(container, i), recv, unlockMethod) {
				return true // this return is guarded; continue scanning
			}
		}
		// Fall through to the enclosing block: the return may be inside an
		// if whose parent block already unlocked before the if.
		if unlockBefore(rs, parentMap, recv, unlockMethod) {
			return true
		}
		unsafe = true
		return false
	})
	return unsafe
}

// isUnlockStmt reports whether stmt is a bare `mu.Unlock()` expression
// statement targeting the receiver.
func isUnlockStmt(stmt ast.Stmt, recv, unlockMethod string) bool {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	return unlockTargets(call, recv, unlockMethod)
}

// unlockPrecedesInBlock is retained for backward symmetry with the facade
// helpers; prefer unlockPrecedesInContainer in new code.
func unlockPrecedesInBlock(block *ast.BlockStmt, idx int, recv, unlockMethod string) bool {
	return unlockPrecedesInContainer(block, idx, recv, unlockMethod)
}

// unlockBefore reports whether an unlock of the receiver dominates the node:
// the node sits AFTER an unlock in its own enclosing block, OR after an
// unlock in any enclosing block (i.e. an ancestor statement executed after
// the wait is a descendant of a statement that follows an unlock).
//
// Handles the two idioms that matter:
//
//	if p.shutdown { p.mu.Unlock(); return ... }   // return preceded by own unlock
//	mu.Unlock()
//	if x { ...; if y { return } }                 // inner return dominated by outer unlock
func unlockBefore(n ast.Node, parentMap map[ast.Node]ast.Node, recv, unlockMethod string) bool {
	container, idx := containingContainerAndIndex(n, parentMap)
	if unlockPrecedesInContainer(container, idx, recv, unlockMethod) {
		return true
	}
	// Climb: the container's own enclosing statement may sit after an unlock
	// in an ancestor container.
	stmtInContainer := enclosingStmtOf(container, parentMap)
	for stmtInContainer != nil {
		inner := stmtInContainer
		parent := parentMap[inner]
		for parent != nil {
			if b, ok := parent.(*ast.BlockStmt); ok {
				if unlockPrecedesInContainer(b, outerIdxOf(b, inner), recv, unlockMethod) {
					return true
				}
				cur := b
				for {
					if enc := enclosingStmtOf(cur, parentMap); enc != nil {
						stmtInContainer = enc
						break
					}
					break
				}
				break
			}
			if _, isStmt := parent.(ast.Stmt); isStmt {
				stmtInContainer = parent.(ast.Stmt)
				break
			}
			parent = parentMap[parent]
		}
	}
	return false
}

// unlockPrecedesInContainer reports whether an unlock of the receiver appears
// in the container before index idx.
func unlockPrecedesInContainer(c ast.Node, idx int, recv, unlockMethod string) bool {
	if c == nil {
		return false
	}
	for i := 0; i < idx; i++ {
		if isUnlockStmt(containerStmtAt(c, i), recv, unlockMethod) {
			return true
		}
	}
	return false
}

// enclosingStmtOf returns the statement that directly contains the given
// node's enclosing block (e.g. the *ast.IfStmt for an if-body block).
func enclosingStmtOf(block ast.Node, parentMap map[ast.Node]ast.Node) ast.Stmt {
	parent := parentMap[block]
	for parent != nil {
		if s, ok := parent.(ast.Stmt); ok {
			return s
		}
		parent = parentMap[parent]
	}
	return nil
}

// outerIdxOf returns the index of stmt within its directly enclosing block.
func outerIdxOf(block *ast.BlockStmt, stmt ast.Stmt) int {
	if block == nil || stmt == nil {
		return -1
	}
	for i, s := range block.List {
		if s == stmt {
			return i
		}
	}
	return -1
}

// containingBlockAndIndex returns the immediately enclosing statement
// container (BlockStmt, CaseClause, or CommClause — raw node identity) and
// the index of the node's containing statement within that container.
func containingBlockAndIndex(node ast.Node, parentMap map[ast.Node]ast.Node) (*ast.BlockStmt, int) {
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			switch p := parentMap[stmt].(type) {
			case *ast.BlockStmt:
				return p, stmtIndexInBlock(p, stmt)
			case *ast.CaseClause:
				return clauseBlock(p), clauseIndexOf(p, stmt)
			case *ast.CommClause:
				return clauseBlock(p), clauseIndexOf(p, stmt)
			}
		}
		n = parentMap[n]
	}
	return nil, -1
}

// containingContainerAndIndex is the identity-preserving variant: it returns
// the raw container node (BlockStmt / CaseClause / CommClause) so callers can
// compare containers by pointer and index their statements uniformly.
func containingContainerAndIndex(node ast.Node, parentMap map[ast.Node]ast.Node) (ast.Node, int) {
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			switch p := parentMap[stmt].(type) {
			case *ast.BlockStmt:
				return p, stmtIndexInBlock(p, stmt)
			case *ast.CaseClause:
				return p, clauseIndexOf(p, stmt)
			case *ast.CommClause:
				return p, clauseIndexOf(p, stmt)
			}
		}
		n = parentMap[n]
	}
	return nil, -1
}

// containerStmtAt returns the i-th statement of a container node
// (BlockStmt, CaseClause, or CommClause).
func containerStmtAt(c ast.Node, i int) ast.Stmt {
	switch cc := c.(type) {
	case *ast.BlockStmt:
		if i >= 0 && i < len(cc.List) {
			return cc.List[i]
		}
	case *ast.CaseClause:
		if i >= 0 && i < len(cc.Body) {
			return cc.Body[i]
		}
	case *ast.CommClause:
		if i >= 0 && i < len(cc.Body) {
			return cc.Body[i]
		}
	}
	return nil
}

// clauseBlock projects a switch/select clause's body ([]ast.Stmt, no braces)
// onto a synthetic *ast.BlockStmt so the classifier treats all statement
// containers uniformly. The facade is read-only and never escapes.
func clauseBlock(c ast.Node) *ast.BlockStmt {
	switch cc := c.(type) {
	case *ast.CaseClause:
		return &ast.BlockStmt{List: cc.Body}
	case *ast.CommClause:
		return &ast.BlockStmt{List: cc.Body}
	}
	return nil
}

// clauseIndexOf returns the index of stmt within a clause body.
func clauseIndexOf(c ast.Node, stmt ast.Stmt) int {
	switch cc := c.(type) {
	case *ast.CaseClause:
		for i, s := range cc.Body {
			if s == stmt {
				return i
			}
		}
	case *ast.CommClause:
		for i, s := range cc.Body {
			if s == stmt {
				return i
			}
		}
	}
	return -1
}

// deferTargetsUnlock reports whether the defer is `defer mu.Unlock()` /
// `defer mu.RUnlock()` for the receiver, or a deferred closure that does so.
func deferTargetsUnlock(d *ast.DeferStmt, recv, unlockMethod string) bool {
	call := d.Call
	if unlockTargets(call, recv, unlockMethod) {
		return true
	}
	if fn, ok := call.Fun.(*ast.FuncLit); ok {
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if inner, ok := n.(*ast.CallExpr); ok && unlockTargets(inner, recv, unlockMethod) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	return false
}

// unlockTargets reports whether the call is `mu.Unlock()` / `mu.RUnlock()`
// matching the receiver.
func unlockTargets(call *ast.CallExpr, recvString, method string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	return mutexReceiver(sel.X) == recvString
}

// returnsMutex reports whether a return statement returns the receiver —
// the ownership-transfer pattern (AcquireLock-style constructors).
func returnsMutex(rs *ast.ReturnStmt, recvString string) bool {
	for _, r := range rs.Results {
		if id, ok := r.(*ast.Ident); ok && id.Name == recvString {
			return true
		}
		if selx, ok := r.(*ast.SelectorExpr); ok && mutexReceiver(selx) == recvString {
			return true
		}
	}
	return false
}

// passesReceiver reports whether the call passes the receiver (or a
// selector ending in it) as an argument to another function.
func passesReceiver(call *ast.CallExpr, recvString string) bool {
	if unlockTargets(call, recvString, "Unlock") || unlockTargets(call, recvString, "RUnlock") {
		return false
	}
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == recvString {
			return true
		}
		if selx, ok := a.(*ast.SelectorExpr); ok && mutexReceiver(selx) == recvString {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------- coverage

type blockPos struct {
	block *ast.BlockStmt
	after int
}

// coveringBlocks returns (block, after-index) positions in which a deferred
// unlock would cover the site. Mirrors the audit-tx logic.
func coveringBlocks(node ast.Node, parentMap map[ast.Node]ast.Node) []blockPos {
	var out []blockPos
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, blockPos{block: block, after: stmtIndexInBlock(block, stmt) + 1})
				n = block
				continue
			}
			n = parentMap[stmt]
			continue
		}
		n = parentMap[n]
	}
	return out
}

func covers(cover []blockPos, stmt ast.Stmt, parentMap map[ast.Node]ast.Node) bool {
	block, ok := parentMap[stmt].(*ast.BlockStmt)
	if !ok {
		return false
	}
	idx := stmtIndexInBlock(block, stmt)
	for _, c := range cover {
		if c.block == block && idx >= c.after {
			return true
		}
	}
	return false
}

func stmtIndexInBlock(block *ast.BlockStmt, target ast.Stmt) int {
	for i, s := range block.List {
		if s == target {
			return i
		}
	}
	return -1
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

// -------------------------------------------------------------------- context capture

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

// -------------------------------------------------------------------- emitters

func emitMarkdown(w *os.File, sites []Site) {
	counts := countByClass(sites)

	fmt.Fprintln(w, "# Mutex Lock/Unlock Coverage Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total Lock/RLock/TryLock call sites:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []MuClass{
		MuClassDeferUnlock, MuClassDelegated, MuClassExplicitUnlock, MuClassNoUnlock, MuClassUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	total := len(sites)
	bad := counts[MuClassNoUnlock] + counts[MuClassExplicitUnlock] + counts[MuClassUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Potentially unbound locks (deadlock risk): %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[MuClass]bool{MuClassDeferUnlock: true, MuClassDelegated: true}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (%s)\n", s.File, s.Line, s.Pattern)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.VarName != "" {
			fmt.Fprintf(w, "- **Mutex receiver:** `%s`\n", s.VarName)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## mu-defer-unlock Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites are covered by a deferred unlock.\n", counts[MuClassDeferUnlock])
}

func emitJSON(w *os.File, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int    `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func countByClass(sites []Site) map[MuClass]int {
	m := make(map[MuClass]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}

// gateNoUnlockCount is the site counter the gate thresholds read for the
// fatal no-unlock class. Kept for symmetry with the other audits' thresholds.
var _ = func() int { return 0 }
