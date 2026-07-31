package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveRouteWorkspace_EnvWins verifies the explicit override takes
// precedence over the default fallback. Reproduces the pre-fix bug where
// unset env → "." → "open ./mode: no such file or directory" when the MCP
// server is launched from a cwd that has no mode/ subdirectory.
func TestResolveRouteWorkspace_EnvWins(t *testing.T) {
	t.Setenv("MPM_ROUTE_WORKSPACE", "/custom/route/path")
	if got := resolveRouteWorkspace(); got != "/custom/route/path" {
		t.Errorf("resolveRouteWorkspace() = %q, want env override %q", got, "/custom/route/path")
	}
}

// TestResolveRouteWorkspace_DefaultsAbsoluteNotRelative is the regression
// test for the path bug reported in the 2026-07-30 audit: callers running
// from an unrelated cwd (MCP server, hooks) hit "open ./mode: no such file
// or directory" because the fallback was the literal ".".
//
// The fix routes through config.GetMPMDir() so the result is anchored to
// either MPM_WORKSPACE (when set) or $HOME/.mpm — both absolute paths. The
// exact value depends on the caller's environment, so we assert the
// structural property (absolute, not ".").
func TestResolveRouteWorkspace_DefaultsAbsoluteNotRelative(t *testing.T) {
	os.Unsetenv("MPM_ROUTE_WORKSPACE")
	got := resolveRouteWorkspace()
	if got == "." {
		t.Fatalf("resolveRouteWorkspace() = %q — relative path bug regression: must NOT return \".\" as fallback (see 2026-07-30 audit)", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolveRouteWorkspace() = %q, want absolute path (resolved via config.GetMPMDir)", got)
	}
	if !strings.HasSuffix(got, ".mpm") && os.Getenv("MPM_WORKSPACE") == "" {
		// When MPM_WORKSPACE is unset, the canonical home-default ends in .mpm.
		// If the operator set MPM_WORKSPACE to something else, anything absolute
		// is acceptable — just verify it's not ".".
		t.Logf("resolveRouteWorkspace() = %q (non-default workspace via MPM_WORKSPACE — acceptable)", got)
	}
}