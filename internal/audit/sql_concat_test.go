package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestSQL_SprintfConcatFormatString_AllowlistGuarded pins the
// classification of a fmt.Sprintf call whose format string is split
// across two adjacent string literals joined with `+` (the shape Go
// emits for SQL statements that exceed 80 cols), and whose interpolated
// arguments are allowlist-guarded function params.
//
// Pre-fix, classifySprintf cast `call.Args[0]` to *ast.BasicLit and
// failed on the *ast.BinaryExpr, falling through to SQLBuilt even
// though the args were statically safe. The allowlist helper
// (isAllowlistGuardedParam) was correctly identifying the upstream
// `rebuildMemoriesFtsTableAllowlist[ftsTable]` check — only the format
// extraction was failing.
//
// Mirrors the real-world shape at
// internal/core/migration_memories_affinity_rebuild.go:945 in
// repopulateStandaloneFTS.
func TestSQL_SprintfConcatFormatString_AllowlistGuarded(t *testing.T) {
	src := `package p

import (
	"context"
	"database/sql"
	"fmt"
)

var rebuildMemoriesFtsTableAllowlist = map[string]bool{}
var rebuildMemoriesTableAllowlist = map[string]bool{}

func repopulateStandaloneFTS(ctx context.Context, tx *sql.Tx, ftsTable, table string) error {
	if !rebuildMemoriesFtsTableAllowlist[ftsTable] {
		return fmt.Errorf("nope")
	}
	if !rebuildMemoriesTableAllowlist[table] {
		return fmt.Errorf("nope")
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO %s(rowid, content, collection, session_id, tags) "+
			"SELECT rowid, content, collection, session_id, tags FROM %s",
		ftsTable, table,
	))
	return err
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "diag.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parentMap := make(map[ast.Node]ast.Node)
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

	af := &File{
		Path:      "diag.go",
		FSet:      fset,
		AST:       f,
		Src:       []byte(src),
		ParentMap: parentMap,
	}

	r := &SQLRule{}
	sites := r.AuditFile(af)
	if len(sites) != 1 {
		t.Fatalf("expected exactly 1 site, got %d", len(sites))
	}
	if sites[0].Classification != SQLFmtSafe {
		t.Errorf("classification: got %s, want %s (the format-string concatenation was misread as constructed SQL)",
			sites[0].Classification, SQLFmtSafe)
	}
}

// TestSQL_StringLitConcat covers the stringLitConcat helper directly.
func TestSQL_StringLitConcat(t *testing.T) {
	fset := token.NewFileSet()
	cases := []struct {
		name    string
		expr    string
		want    string
		wantOk  bool
	}{
		{"basic lit", `"hello %s"`, `"hello %s"`, true},
		// ast.BasicLit.Value preserves the surrounding quotes from the
		// source form; sprintfVerbCount is robust to that (it scans for
		// '%' bytes, which the quote characters never match).
		{"concat of two", `"a %s " + "b %s"`, `"a %s ""b %s"`, true},
		{"concat of three", `"a" + "b" + "c"`, `"a""b""c"`, true},
		{"concat with non-string", `"a" + ident`, ``, false},
		{"concat with subtract", `"a" - "b"`, ``, false},
		{"int literal", `42`, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p; var _ = " + tc.expr
			f, err := parser.ParseFile(fset, "t.go", src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			// Find the GenDecl with the var declaration
			var got string
			var ok bool
			ast.Inspect(f, func(n ast.Node) bool {
				gd, isGd := n.(*ast.GenDecl)
				if !isGd || gd.Tok != token.VAR {
					return true
				}
				for _, spec := range gd.Specs {
					vs, isVs := spec.(*ast.ValueSpec)
					if !isVs {
						continue
					}
					for _, v := range vs.Values {
						got, ok = stringLitConcat(v)
					}
				}
				return true
			})
			if ok != tc.wantOk {
				t.Errorf("ok: got %v, want %v (got value %q)", ok, tc.wantOk, got)
			}
			if ok && got != tc.want {
				t.Errorf("value: got %q, want %q", got, tc.want)
			}
		})
	}
}