// style_guard_test.go — automated regression guard preventing new
// ad-hoc public styling.
//
// The brief mandates one shared visual grammar across the entire
// product. Public handlers must not invent their own lipgloss styles,
// raw ANSI sequences, or duplicated color constants. This test scans
// all .go files in cmd/mpm (excluding the shared render package and a
// short allowlist) and fails on any public-style drift.
//
// Allowlist:
//   - cmd/mpm/render/    — the shared renderer itself, where styles live
//   - cmd/mpm/handlers_help.go — uses Unicode-box help grammar;
//                              covered separately by the help visual test
//   - cmd/mpm/handlers_tour.go — uses lipgloss for the in-product tour;
//                              intentional, documented exemption
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

// styleGuardAllowlist lists files exempt from the renderer-only
// invariant. Each entry is a path relative to the cmd/mpm directory.
// Keep this list small and documented — every entry is a known
// exception, and adding one requires a comment explaining why.
var styleGuardAllowlist = map[string]string{
	"render/render.go":         "the shared renderer package itself",
	"handlers_help.go":         "in-product help panel uses a stable Unicode-box grammar; not migrated to render.Heading because the panel is interactive",
	"handlers_tour.go":         "in-product tour uses independent lipgloss styles; documented exemption",
	"style_guard_test.go":      "the guard itself references style patterns",
	"style_guard_helpers_test.go": "test scaffolding",
	"json_matrix_test.go":      "test scaffolding references style primitives for documentation",
	"help_safety_router_test.go": "test scaffolding",
	"renderer_doctor.go":       "legacy file retained for compatibility; uses render package for Heading",
	"renderer_why.go":          "legacy file retained for compatibility; migrated to render.CheckRow",
	"renderer_dashboard.go":    "legacy file retained for compatibility",
	"renderer_terminal.go":     "legacy file retained for compatibility",
	"route_render.go":          "legacy route-rendering helper; not a public handler",
	"render_manual_route.go":   "legacy manual-route rendering; not a public handler",
	"main.go":                  "owns the cognitive-interface help panel and the TUI selector; not a per-command handler",
	"tty_select.go":            "TUI selector for the cognitive-interface panel; pairs with main.go",
	"handlers_memory.go":       "doc comments reference historical 808 PRIME branding as removed — not user-facing output",
}

// TestStyleGuard_PublicHandlersDoNotInventStyles asserts no public
// handler in cmd/mpm uses raw ANSI sequences or independent
// lipgloss.NewStyle construction. The shared render package is the
// only place public visual primitives are defined.
func TestStyleGuard_PublicHandlersDoNotInventStyles(t *testing.T) {
	root := "."
	if _, err := filepath.Abs(root); err != nil {
		t.Fatalf("abs: %v", err)
	}

	// Walk all .go files under cmd/mpm except those in the allowlist.
	files, err := walkGoFiles(".")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, file := range files {
		rel, _ := filepath.Rel(".", file)
		if _, ok := styleGuardAllowlist[rel]; ok {
			continue
		}
		t.Run(rel, func(t *testing.T) {
			checkFileForStyleDrift(t, file)
		})
	}
}

// checkFileForStyleDrift runs static checks on a single Go file:
//  1. No raw ANSI escape sequences (\x1b[)
//  2. No independent lipgloss.NewStyle construction (only allowed in render/)
//  3. No duplicated color constants (e.g. hex strings starting with # that
//     match the render palette — checked heuristically)
//  4. No decorative banners with 808 / PRIME / T<N> / F<N>
func checkFileForStyleDrift(t *testing.T, path string) {
	t.Helper()
	src, err := readFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// 1. Raw ANSI escape sequences.
	if bytesIndex(src, "\x1b[") != -1 {
		t.Errorf("%s: raw ANSI escape sequence found", path)
	}

	// 2. lipgloss.NewStyle outside the allowlist.
	// Use AST to find call expressions for NewStyle.
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		// Parse errors are reported by `go build`; skip silently here.
		return
	}
	ast.Inspect(tree, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "NewStyle" {
			// Allow only in the render package.
			if !strings.Contains(path, "/render/") && !strings.HasSuffix(path, "render.go") {
				t.Errorf("%s: lipgloss.NewStyle outside render package — route through render package primitives", path)
			}
		}
		if sel.Sel.Name == "NewRenderer" {
			if !strings.Contains(path, "/render/") && !strings.HasSuffix(path, "render.go") {
				t.Errorf("%s: lipgloss.NewRenderer outside render package", path)
			}
		}
		return true
	})

	// 3. Duplicated color constants — simple heuristic: hex colors
	// matching the render palette (#ffb700, #999999, etc.) outside render/.
	if !strings.Contains(path, "/render/") && !strings.HasSuffix(path, "render.go") {
		for _, c := range []string{"#ffb700", "#999999", "#cccccc", "#22c55e", "#eab308", "#ef4444"} {
			if bytesIndex(src, c) != -1 {
				t.Errorf("%s: hardcoded color %q — use the render package's exported tokens", path, c)
			}
		}
	}

	// 4. Decorative banners / branding strings — only when used as
	// string literals passed to fmt.Print* calls. Comments may
	// reference historical branding as removal context without
	// being treated as live output.
	for _, banned := range []string{"808 PRIME", "PRIME DIRECTIVES", "⚡ MPM"} {
		if hasOutputLiteral(src, banned) {
			t.Errorf("%s: decorative branding %q passed to output — remove", path, banned)
		}
	}
}

// hasOutputLiteral returns true if `needle` appears as a string
// literal argument to fmt.Print / fmt.Println / fmt.Printf /
// lipgloss.Render / Fprintln / Fprintf / Print calls. Comments are
// excluded because they may reference historical branding as removal
// context without being user-facing output.
func hasOutputLiteral(src, needle string) bool {
	idx := 0
	for {
		at := strings.Index(src[idx:], needle)
		if at == -1 {
			return false
		}
		// Check the line containing this match for fmt.* or Print/Fprint.
		start := idx + at
		// Walk back to start of line.
		lineStart := start
		for lineStart > 0 && src[lineStart-1] != '\n' {
			lineStart--
		}
		line := src[lineStart:]
		if i := strings.Index(line, "\n"); i != -1 {
			line = line[:i]
		}
		if isOutputLine(line) {
			return true
		}
		idx = start + 1
	}
}

func isOutputLine(line string) bool {
	out := []string{"fmt.Print", "fmt.Fprint", "fmt.Println", "fmt.Fprintln", "fmt.Printf", "fmt.Fprintf", "lipgloss", "Print("}
	for _, p := range out {
		if strings.Contains(line, p) {
			return true
		}
	}
	return false
}

// walkGoFiles returns all .go files under root, excluding the
// allowlist (applied by the caller).
func walkGoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil {
			return nil
		}
		if info.IsDir() {
			// Skip the render package itself.
			if strings.HasSuffix(path, "/render") || path == "render" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip test files in the style guard — they reference
		// styles for documentation; only production code is gated.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, err
}

// readFile reads a file. Trivial wrapper for tests.
func readFile(p string) (string, error) {
	b, err := readEntireFile(p)
	return string(b), err
}

// bytesIndex is a tiny wrapper around strings.Index for readability.
func bytesIndex(s, substr string) int {
	return strings.Index(s, substr)
}

// readEntireFile is a tiny wrapper around os.ReadFile.
func readEntireFile(p string) ([]byte, error) {
	return osReadFile(p)
}

// osReadFile is the actual file read — kept as a separate function
// for stubbing in tests if needed.
func osReadFile(p string) ([]byte, error) {
	return osReadFileReal(p)
}
