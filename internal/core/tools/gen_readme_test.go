package tools

import (
	"strings"
	"testing"
)

// TestRenderToolSurface_Deterministic verifies the generator output is
// stable across calls (no map iteration order, no time-dependent
// fields). This guards against a future contributor adding a
// non-deterministic element to RenderToolSurface that would create
// spurious git diffs on every `go generate`.
func TestRenderToolSurface_Deterministic(t *testing.T) {
	a := RenderToolSurface()
	b := RenderToolSurface()
	if a != b {
		t.Errorf("RenderToolSurface not deterministic across calls:\n  first:\n%s\n\n  second:\n%s", a, b)
	}
}

// TestRenderToolSurface_NoTrailingWhitespace verifies each line in the
// generated block has no trailing whitespace. Trailing spaces survive
// markdown rendering but create noisy diffs.
func TestRenderToolSurface_NoTrailingWhitespace(t *testing.T) {
	out := RenderToolSurface()
	for i, line := range strings.Split(out, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
	}
}

// TestRenderToolSurface_IncludesAllRegistryTools verifies every tool
// in the Registry appears in the generated output. This catches the
// "added a tool, didn't run go generate" regression: a missing entry
// would mean the README claims fewer tools than actually exist.
func TestRenderToolSurface_IncludesAllRegistryTools(t *testing.T) {
	out := RenderToolSurface()
	for _, tool := range Registry {
		if !strings.Contains(out, tool.Name) {
			t.Errorf("Registry tool %q missing from generated surface", tool.Name)
		}
	}
}
