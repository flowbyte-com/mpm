// Imports rule — architectural boundary enforcement.
//
// Unlike the other rules (which classify call sites into a positive
// vocabulary), this rule is a violation detector: it maps forbidden import
// edges and emits a site for every import that crosses one. Because the
// engine already parses every file and holds the ast.File node (with its
// flat Imports slice) on the shared File, no recursive traversal is needed
// — the rule reads the imports off the root node directly.
//
// The edges encode MPM's actual module split:
//
//	core-no-main   — internal/core is the standalone mpm-core module. It
//	                 must never import github.com/flowbyte-com/mpm/... (the
//	                 main module path); if it does, the standalone split is
//	                 silently re-coupled and the standalone-Core invariant
//	                 is broken.
//	audit-no-core   — internal/audit must stay a pure static-analysis
//	                 library. Importing internal/core (or mpm-core) would
//	                 tether the linter to the database schema and make the
//	                 pre-commit gate depend on core compiling.
//	libs-no-cmd     — internal/* libraries (critic, scheduler, audit) are
//	                 below the presentation layer; they must not import
//	                 cmd/* executables.
//
// Matching works on path prefixes (isSubstring for from-matching against the
// relative file path, plain prefix for the import path). All three edges are
// measured clean today, so the gate runs at zero with no grandfathering.
//
// Gate: import-forbidden at 0 — any crossing edge fails the commit.
package audit

import (
	"fmt"
	"io"
	"strings"
)

// forbiddenEdge defines a single architectural boundary constraint.
type forbiddenEdge struct {
	name        string   // report key, e.g. core-no-ma
	fromPrefix  string   // the importing tree, e.g. "internal/core"
	toPrefixes  []string // forbidden dependencies, e.g. "github.com/flowbyte-com/mpm"
	isSubstring bool     // if true, fromPrefix is matched as a substring of the file path
}

// ImportsForbidden is the single class of the imports rule.
const ImportsForbidden = "import-forbidden"

var architecturalBoundaries = []forbiddenEdge{
	{
		name:        "core-no-main",
		fromPrefix:  "internal/core",
		toPrefixes:  []string{"github.com/flowbyte-com/mpm/internal", "github.com/flowbyte-com/mpm/cmd", "github.com/flowbyte-com/mpm/agent"},
		isSubstring: true,
	},
	{
		name:        "audit-no-core",
		fromPrefix:  "internal/audit",
		toPrefixes:  []string{"github.com/flowbyte-com/mpm-core", "github.com/flowbyte-com/mpm/internal/core"},
		isSubstring: true,
	},
	{
		name:        "libs-no-cmd",
		fromPrefix:  "internal",
		toPrefixes:  []string{"github.com/flowbyte-com/mpm/cmd"},
		isSubstring: true,
	},
}

// ImportsRule audits the import statement of every file against the
// architectural boundaries.
type ImportsRule struct{}

// NewImportsRule constructs the architectural-dependency rule.
func NewImportsRule() Rule { return &ImportsRule{} }

// Name implements Rule.
func (r *ImportsRule) Name() string { return "imports" }

// Prepare implements Rule (boundaries are static).
func (r *ImportsRule) Prepare(files []*File) {}

// AuditFile implements Rule by reading the flat Imports slice off the
// already-parsed ast.File.
func (r *ImportsRule) AuditFile(f *File) []Site {
	var sites []Site
	for _, imp := range f.AST.Imports {
		importPath := strings.Trim(imp.Path.Value, "`\"")
		for _, edge := range architecturalBoundaries {
			matchFrom := false
			if edge.isSubstring {
				matchFrom = strings.Contains(f.Path, edge.fromPrefix)
			} else {
				matchFrom = strings.HasPrefix(f.Path, edge.fromPrefix)
			}
			if !matchFrom {
				continue
			}
			for _, to := range edge.toPrefixes {
				if strings.HasPrefix(importPath, to) {
					pos := f.FSet.Position(imp.Pos())
					sites = append(sites, Site{
						File:           f.Path,
						Line:           pos.Line,
						Column:         pos.Column,
						Pattern:        edge.name,
						Classification: ImportsForbidden,
						Reason:         fmt.Sprintf("%s: %s cannot import %s", edge.name, edge.fromPrefix, importPath),
						Code:           ContextLines(f.Src, pos.Line),
					})
				}
			}
		}
	}
	return sites
}

// EmitMarkdown implements Rule.
func (r *ImportsRule) EmitMarkdown(w io.Writer, sites []Site) {
	if len(sites) == 0 {
		fmt.Fprintln(w, "# Import Boundary Audit")
		fmt.Fprintln(w, "**No architectural boundary violations.** Layers are a DAG.")
		return
	}
	fmt.Fprintln(w, "# Import Boundary Audit")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Forbidden imports found:** %d\n\n", len(sites))
	for _, s := range sites {
		fmt.Fprintf(w, "### `%s` — line %d\n", s.File, s.Line)
		fmt.Fprintf(w, "- **Boundary:** %s\n", s.Pattern)
		fmt.Fprintf(w, "- **Violation:** %s\n", s.Reason)
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}
}

// GateChecks implements Rule. A single violation fails the gate: import-
// forbidden sits at zero with no grandfathered baseline.
func (r *ImportsRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	return []GateCheck{
		{ImportsForbidden, counts[ImportsForbidden], max[ImportsForbidden]},
	}
}
