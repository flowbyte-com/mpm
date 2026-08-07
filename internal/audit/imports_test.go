package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// parseImportsFixture builds a File from source for boundary tests.
func parseImportsFixture(t *testing.T, path, src string) *File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return &File{
		Path:      path,
		FSet:      fset,
		AST:       f,
		Src:       []byte(src),
		Dir:       filepath.Dir(path),
		ParentMap: map[ast.Node]ast.Node{},
	}
}

func TestImportsRule_ForbiddenEdges(t *testing.T) {
	cases := []struct {
		name string
		path string
		src  string
		edge string
	}{
		{
			name: "core-no-main via cmd",
			path: "internal/core/evil.go",
			src:  "package core\nimport \"github.com/flowbyte-com/mpm/cmd/mpm\"\n",
			edge: "core-no-main",
		},
		{
			name: "core-no-main via internal",
			path: "internal/core/evil.go",
			src:  "package core\nimport \"github.com/flowbyte-com/mpm/internal/critic\"\n",
			edge: "core-no-main",
		},
		{
			name: "audit-no-core",
			path: "internal/audit/evil.go",
			src:  "package audit\nimport \"github.com/flowbyte-com/mpm/internal/core\"\n",
			edge: "audit-no-core",
		},
		{
			name: "libs-no-cmd",
			path: "internal/scheduler/evil.go",
			src:  "package sched\nimport \"github.com/flowbyte-com/mpm/cmd/mpm\"\n",
			edge: "libs-no-cmd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewImportsRule()
			sites := r.AuditFile(parseImportsFixture(t, tc.path, tc.src))
			found := false
			for _, s := range sites {
				if s.Pattern == tc.edge {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %s boundary violation on %s, got %+v", tc.edge, tc.path, sites)
			}
		})
	}
}

func TestImportsRule_LegitImportsPass(t *testing.T) {
	cases := []struct {
		name string
		path string
		src  string
	}{
		{
			name: "scheduler imports core module (consumer)",
			path: "internal/scheduler/ok.go",
			src:  "package sched\nimport \"github.com/flowbyte-com/mpm-core\"\n",
		},
		{
			name: "core imports its own module",
			path: "internal/core/ok.go",
			src:  "package core\nimport \"github.com/flowbyte-com/mpm-core/synth\"\n",
		},
		{
			name: "cmd binary imports audit",
			path: "cmd/mpm-lint/main.go",
			src:  "package main\nimport \"github.com/flowbyte-com/mpm/internal/audit\"\n",
		},
		{
			name: "stdlib import",
			path: "internal/core/ok.go",
			src:  "package core\nimport \"fmt\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewImportsRule()
			sites := r.AuditFile(parseImportsFixture(t, tc.path, tc.src))
			if len(sites) != 0 {
				t.Fatalf("expected no violations on %s, got %+v", tc.path, sites)
			}
		})
	}
}

func TestImportsRule_GateIsZeroTolerance(t *testing.T) {
	r := NewImportsRule()
	checks := r.GateChecks(nil, map[string]int{ImportsForbidden: 0})
	if len(checks) != 1 || checks[0].Name != ImportsForbidden {
		t.Fatalf("unexpected gate checks: %+v", checks)
	}
	if checks[0].Threshold != 0 {
		t.Fatalf("import-forbidden must be zero-tolerance, got %d", checks[0].Threshold)
	}

	// A real violation must trip the gate — RunGate returns 1 when a class
	// exceeds its threshold.
	sites := r.AuditFile(parseImportsFixture(t, "internal/scheduler/evil.go",
		"package sched\nimport \"github.com/flowbyte-com/mpm/cmd/mpm\"\n"))
	if len(sites) == 0 {
		t.Fatal("fixture produced no sites; gate test is vacuous")
	}
	checks = r.GateChecks(sites, map[string]int{ImportsForbidden: 0})
	for i := range checks {
		if checks[i].Actual > checks[i].Threshold {
			// RunGate is stderr-only and returns 1 on failure — covered
			// behaviorally; here we assert the check enforcement decision.
			if checks[i].Name == ImportsForbidden {
				return
			}
		}
	}
	t.Fatal("gate check did not flag the import-forbidden violation")
}
