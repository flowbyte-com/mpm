package internal

// bare_pool_write_ratchet_test.go — issue #178, permanent prevention.
//
// WHAT THIS FILE IS
// -----------------
// Issue #178's audit found 172 bare pool writes (`dm.db.Exec(...)`,
// `dm.SQLDB().Exec(...)`, `s.DB.Exec(...)`) that bypass ExecTracked's
// statement-level retry budget: under contention that outlasts the
// busy timeout the write is simply lost. The corrections route the
// dangerous ones through a budget; this file keeps the class
// discharged.
//
// WHY A RATCHET AND NOT A ZERO-TOLERANCE GATE
// -------------------------------------------
// The baseline is over a hundred sites, most of which already propagate
// the error, run once at startup under a lock, or are deliberately
// best-effort. Demanding zero bare writes would force a giant patch
// touching every database call — exactly what the issue forbids. So
// this guard fails when the count goes UP in any file, the way
// `mpm-lint --gate` treats review classes: measurable progress stays
// green, and a newly introduced bare write fails review loudly.
//
// WHAT COUNTS
// -----------
// A bare pool write is a direct `Exec` call whose receiver is a pool
// handle (`*.db`, `*.DB`, `*.SQLDB()`, a local `db`/`q`/`ex` handle)
// with a write-verb SQL (DML and DDL alike — a growing DDL count must
// also trip review, since DDL wants a transaction, not a budget), that
// is NOT lexically inside a `WithTx` callback and NOT on a `tx`
// handle. `node.Tx().Exec(...)` is transaction-owned and is not
// counted.
//
// The classification is syntactic and therefore an approximation —
// see the audit document for the shapes that defeat it
// (`docs/dev/bare-write-risk-assessment-2026-10-29.md`, "three bugs in
// my first scanner"). The approximation is load-bearing in exactly one
// direction: it may OVER-count (a site that is actually safe still
// trips the ratchet until it is exempted), it must never UNDER-count
// a genuinely bare write. Every shape below was checked against the
// audit scanner's inventory before the baseline was recorded.
//
// NON-VACUITY
// -----------
// TestBarePoolScannerFlagsRepresentativeShapes feeds the classifier
// synthetic sources and asserts each bare shape is caught — including
// a deliberately reintroduced defect — and that the guarded and
// transaction-owned shapes are not.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// bareWriteVerb matches a SQL statement that writes, DML or DDL. DDL
// is included deliberately: a multi-statement DDL Exec may have
// applied part of itself, so a retry can compound the damage and DDL
// wants a transaction instead — but a NEW bare DDL site must still
// trip review rather than land silently.
var bareWriteVerb = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|REPLACE|CREATE|DROP|ALTER)\b`)

// barePoolSite is one direct Exec call the ratchet tracks.
type barePoolSite struct {
	File string
	Line int
	SQL  string
}

// isPoolReceiver reports whether the receiver of an Exec call is a
// pool handle rather than a transaction handle.
func isPoolReceiver(x ast.Expr) bool {
	switch r := x.(type) {
	case *ast.SelectorExpr:
		// dm.db, cm.dm.db, s.DB, sc.DB — the pool behind the manager.
		return r.Sel.Name == "db" || r.Sel.Name == "DB"
	case *ast.CallExpr:
		// dm.SQLDB() — the raw pool. node.Tx() is transaction-owned.
		if sel, ok := r.Fun.(*ast.SelectorExpr); ok {
			return sel.Sel.Name == "SQLDB"
		}
		return true
	case *ast.Ident:
		// tx is the transaction handle by universal convention in
		// this tree; everything else holding an Exec method and a
		// write-verb SQL is treated as a pool write. This direction
		// is deliberate: an unknown receiver fails closed (counted)
		// rather than open (missed).
		return r.Name != "tx"
	}
	return true
}

// withTxRanges returns the source ranges of every function literal
// passed directly to a WithTx call: statements inside them are
// transaction-owned, not bare pool writes.
func withTxRanges(fset *token.FileSet, file *ast.File) [][2]token.Pos {
	var out [][2]token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithTx" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				out = append(out, [2]token.Pos{lit.Pos(), lit.End()})
			}
		}
		return true
	})
	_ = fset
	return out
}

// scanBarePoolSource records every bare pool write in one file.
func scanBarePoolSource(rel string, src []byte) ([]barePoolSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	scope := literalScope(file.Decls)
	txRanges := withTxRanges(fset, file)

	inTx := func(pos token.Pos) bool {
		for _, r := range txRanges {
			if r[0] <= pos && pos <= r[1] {
				return true
			}
		}
		return false
	}

	var sites []barePoolSite
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Exec" {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		recv, ok := receiverOf(call)
		if !ok || !isPoolReceiver(recv) {
			return true
		}
		if inTx(call.Pos()) {
			return true
		}
		sql := resolveSQL(call.Args[0], scope)
		if !bareWriteVerb.MatchString(sql) {
			return true
		}
		sites = append(sites, barePoolSite{
			File: rel,
			Line: fset.Position(call.Pos()).Line,
			SQL:  sql,
		})
		return true
	})
	return sites, nil
}

// receiverOf returns the receiver expression of a selector call.
func receiverOf(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	return sel.X, true
}

// scanBarePoolRepository walks the repository and classifies every
// bare pool write, mirroring scanRepository's walk.
func scanBarePoolRepository(root string) (map[string][]barePoolSite, []string, error) {
	byFile := map[string][]barePoolSite{}
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == ".git" || base == ".build" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sites, perr := scanBarePoolSource(rel, src)
		if perr != nil {
			return perr
		}
		if len(sites) > 0 {
			byFile[rel] = sites
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return byFile, files, nil
}

// barePoolWriteBaseline is the per-file count of bare pool writes on
// the tree the ratchet was recorded against. A file whose count grows
// fails the guard: the author either routes the new write through a
// budget (and the count does not grow) or records an exemption (and
// the count is adjusted with a reason).
//
// Regenerating: run TestDumpBarePoolCounts (kept out of the tree; see
// the procedure in the phase-2 guard PR) and paste the table.
var barePoolWriteBaseline = map[string]int{
	"cmd/mpm-mcp/audit_hook.go":                   1,
	"cmd/mpm/audit_hook.go":                       1,
	"cmd/mpm/backfill_embeddings.go":              1,
	"cmd/mpm/drill_cmds.go":                       3,
	"cmd/mpm/handlers_epistemology.go":            1,
	"cmd/mpm/handlers_gc.go":                      5,
	"cmd/mpm/handlers_restore.go":                 1,
	"cmd/mpm/handlers_topic.go":                   1,
	"cmd/mpm/ingest.go":                           3,
	"cmd/mpm/main.go":                             1,
	"cmd/mpm/ops_init_topics.go":                  1,
	"cmd/mpm/recall.go":                           1,
	"cmd/mpm/self_heal.go":                        2,
	"cmd/mpm/simple_cmds.go":                      4,
	"internal/core/admission_db.go":               1,
	"internal/core/arbitration.go":                1,
	"internal/core/audit.go":                      5,
	"internal/core/broadcast.go":                  1,
	"internal/core/cascade_materializer.go":       5,
	"internal/core/cascade_outbox.go":             1,
	"internal/core/cascade_wake_reconcile.go":     1,
	"internal/core/cluster_proposals.go":          3,
	"internal/core/contradiction_log.go":          1,
	"internal/core/contradiction_verification.go": 1,
	"internal/core/db.go":                         43,
	"internal/core/embedding_migration.go":        2,
	"internal/core/epistemology_tools.go":         1,
	"internal/core/fts_recovery.go":               3,
	"internal/core/gc_tools.go":                   3,
	"internal/core/handoff.go":                    5,
	"internal/core/ingest.go":                     3,
	"internal/core/lesson_proposals.go":           3,
	"internal/core/lessons_fts_sync.go":           1,
	"internal/core/memory.go":                     18,
	"internal/core/memory_tools.go":               7,
	"internal/core/migrate_md.go":                 3,
	"internal/core/reference_ingest.go":           1,
	"internal/core/retrieval_metadata.go":         3,
	"internal/core/review.go":                     1,
	"internal/core/scheduled_tasks.go":            2,
	"internal/core/seed/engine.go":                2,
	"internal/core/skill_db.go":                   3,
	"internal/core/testhelpers.go":                1,
	"internal/core/tools/handlers.go":             1,
	"internal/core/vector_index.go":               2,
	"internal/core/wake_tools.go":                 1,
	"internal/core/web_db.go":                     11,
	"internal/scheduler/drill_handler.go":         1,
	"internal/telemetry/store.go":                 1,
}

// TestBarePoolWriteCountDoesNotGrow is the ratchet: no file may gain
// a bare pool write. Removing them (routing through ExecTracked, or
// moving the write inside a transaction) keeps the file green.
func TestBarePoolWriteCountDoesNotGrow(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	byFile, _, err := scanBarePoolRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}

	var grew []string
	for file, sites := range byFile {
		allowed, recorded := barePoolWriteBaseline[file]
		if !recorded {
			grew = append(grew, fmt.Sprintf("%s: %d bare pool writes in a file with no recorded baseline "+
				"(route them through ExecTracked, move them inside a transaction, or record an exemption)",
				file, len(sites)))
			continue
		}
		if len(sites) > allowed {
			grew = append(grew, fmt.Sprintf("%s: %d bare pool writes, baseline %d",
				file, len(sites), allowed))
		}
	}
	if len(grew) > 0 {
		sort.Strings(grew)
		t.Fatalf("bare pool writes grew — route the new write through ExecTracked (budget 5), "+
			"move it inside a transaction, or record an exemption with its reason:\n  %s",
			strings.Join(grew, "\n  "))
	}
}

// TestBarePoolScannerFlagsRepresentativeShapes is the negative control:
// the classifier must catch every bare shape — including a deliberately
// reintroduced defect — and must not flag the guarded or
// transaction-owned shapes.
func TestBarePoolScannerFlagsRepresentativeShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			"bare dm.db.Exec is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`INSERT INTO t VALUES (?)`, 1) }\ntype M struct { db *D }\ntype D struct {}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare dm.SQLDB().Exec is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.SQLDB().Exec(`UPDATE t SET a = 1`) }\ntype M struct{}\nfunc (m *M) SQLDB() *D { return nil }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare s.DB.Exec is caught",
			"package p\nfunc f(s *M) { _, _ = s.DB.Exec(`DELETE FROM t`) }\ntype M struct { DB *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare local db.Exec is caught",
			"package p\nfunc f(db *D) { _, _ = db.Exec(`CREATE TABLE t (a)`) }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"reintroduced defect: a new bare write is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`INSERT INTO audit_log VALUES (?)`, 1) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"guarded ExecTracked is not flagged",
			"package p\nfunc f(dm *M) { _, _ = dm.ExecTracked(`INSERT INTO t VALUES (?)`, 5, 1) }\ntype M struct{}\nfunc (m *M) ExecTracked(q string, n int, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			"tx.Exec inside a WithTx callback is not flagged",
			"package p\nfunc f(dm *M) { _ = dm.WithTx(func(tx *T) error { _, _ = tx.Exec(`UPDATE t SET a = 1`); return nil }) }\ntype M struct{}\ntype T struct{}\nfunc (m *M) WithTx(fn func(*T) error) error { return fn(nil) }\nfunc (t *T) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			"node.Tx().Exec is not flagged",
			"package p\nfunc f(n *N) { _, _ = n.Tx().Exec(`DELETE FROM t`) }\ntype N struct{}\ntype T struct{}\nfunc (n *N) Tx() *T { return nil }\nfunc (t *T) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			"a read is not flagged",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Query(`SELECT * FROM t`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Query(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sites, err := scanBarePoolSource("synthetic.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(sites) != tc.want {
				t.Fatalf("got %d bare pool sites, want %d", len(sites), tc.want)
			}
		})
	}
}

// TestBarePoolScanReachesEveryRuntimePackage mirrors the budget
// guard's reach test: a scan that cannot see a package cannot ratchet
// it.
func TestBarePoolScanReachesEveryRuntimePackage(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	byFile, _, err := scanBarePoolRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}
	countUnder := func(prefix string) int {
		n := 0
		for f, sites := range byFile {
			if f == prefix || strings.HasPrefix(f, prefix+"/") {
				n += len(sites)
			}
		}
		return n
	}
	for _, prefix := range []string{"cmd/mpm", "internal/scheduler", "internal/core"} {
		if countUnder(prefix) == 0 {
			t.Errorf("the bare-pool scan reached no bare write under %q", prefix)
		}
	}
	total := 0
	for _, sites := range byFile {
		total += len(sites)
	}
	if total == 0 {
		t.Fatal("no bare pool write was examined: the scan found nothing, so this test proves nothing")
	}
}
