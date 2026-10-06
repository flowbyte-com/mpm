package mpmcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestResolveWorkspace_DefaultsToCanonicalRuntime pins the default to the
// canonical user runtime. It used to assert "." (the cwd), which only ever
// made sense while the checkout and the runtime root were one directory.
func TestResolveWorkspace_DefaultsToCanonicalRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MPM_WORKSPACE", "")
	want := filepath.Join(home, ".mpm")
	if got := ResolveWorkspace(); got != want {
		t.Fatalf("ResolveWorkspace() = %q, want %q", got, want)
	}
}

// TestResolveWorkspace_IsIndependentOfCwd is the property the old default
// violated: where the process is launched from must not decide where runtime
// state lives.
func TestResolveWorkspace_IsIndependentOfCwd(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MPM_WORKSPACE", "")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(wd) }()

	want := filepath.Join(home, ".mpm")
	if got := ResolveWorkspace(); got != want {
		t.Fatalf("ResolveWorkspace() = %q from cwd %q, want %q", got, elsewhere, want)
	}
}

func TestResolveWorkspace_UsesEnvWhenSet(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", "/tmp/openclaw")
	if got := ResolveWorkspace(); got != "/tmp/openclaw" {
		t.Fatalf("expected /tmp/openclaw, got %q", got)
	}
}

func TestActiveContextFromEnv_Empty(t *testing.T) {
	t.Setenv("MPM_ACTIVE_MODE", "")
	t.Setenv("MPM_ACTIVE_PERSONA", "")
	ac := ActiveContextFromEnv()
	if ac.Mode != "" || ac.Persona != "" {
		t.Fatalf("expected empty ActiveContext, got %+v", ac)
	}
}

func TestActiveContextFromEnv_Populated(t *testing.T) {
	t.Setenv("MPM_ACTIVE_MODE", "808")
	t.Setenv("MPM_ACTIVE_PERSONA", "openclaw-808")
	ac := ActiveContextFromEnv()
	if ac.Mode != "808" {
		t.Errorf("Mode = %q, want 808", ac.Mode)
	}
	if ac.Persona != "openclaw-808" {
		t.Errorf("Persona = %q, want openclaw-808", ac.Persona)
	}
}

// TestActiveContextFromEnv_MPM_FRAMEWORK pins the directive-scope
// transport contract: MPM_FRAMEWORK populates ActiveContext.FrameworkName,
// which ReadDirectivesForFramework uses to filter wake-time directives.
// Empty/unset must fall back to "mcp" so existing single-MCP callers
// see no behaviour change.
func TestActiveContextFromEnv_MPM_FRAMEWORK(t *testing.T) {
	t.Run("unset defaults to mcp", func(t *testing.T) {
		t.Setenv("MPM_FRAMEWORK", "")
		ac := ActiveContextFromEnv()
		if ac.FrameworkName != "mcp" {
			t.Errorf("FrameworkName = %q, want mcp", ac.FrameworkName)
		}
	})

	t.Run("openclaw reaches FrameworkName", func(t *testing.T) {
		t.Setenv("MPM_FRAMEWORK", "openclaw")
		ac := ActiveContextFromEnv()
		if ac.FrameworkName != "openclaw" {
			t.Errorf("FrameworkName = %q, want openclaw", ac.FrameworkName)
		}
	})

	t.Run("opencode reaches FrameworkName", func(t *testing.T) {
		t.Setenv("MPM_FRAMEWORK", "opencode")
		ac := ActiveContextFromEnv()
		if ac.FrameworkName != "opencode" {
			t.Errorf("FrameworkName = %q, want opencode", ac.FrameworkName)
		}
	})
}

// TestActiveContextFromEnv_FrameworkPrecedence pins the canonical
// precedence for the framework-name env var resolution. Audit M-2
// (post-M3, 2026-08-31) found that mpmcli only read MPM_FRAMEWORK and
// silently ignored MPM_PROVENANCE_FRAMEWORK — even though the
// artifact-write channel (provenance.go) and the call-handler channel
// (call.go) had honored canonical-first since 2026-08.
//
// Precedence (after fix):
//   1. MPM_PROVENANCE_FRAMEWORK (canonical)
//   2. MPM_FRAMEWORK (legacy fallback)
//   3. "mcp" (default)
func TestActiveContextFromEnv_FrameworkPrecedence(t *testing.T) {
	cases := []struct {
		name          string
		provenanceEnv string
		frameworkEnv  string
		want          string
	}{
		{"both_set_canonical_wins", "opencode", "claude-code", "opencode"},
		{"only_canonical", "opencode", "", "opencode"},
		{"only_legacy", "", "claude-code", "claude-code"},
		{"both_empty_defaults_to_mcp", "", "", "mcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MPM_PROVENANCE_FRAMEWORK", tc.provenanceEnv)
			t.Setenv("MPM_FRAMEWORK", tc.frameworkEnv)
			ac := ActiveContextFromEnv()
			if ac.FrameworkName != tc.want {
				t.Errorf("FrameworkName = %q, want %q", ac.FrameworkName, tc.want)
			}
		})
	}
}

// TestResolveWorkspace_DefaultsDivergence pins that the two workspace
// resolvers now AGREE.
//
// They used to diverge deliberately:
//
//   - mpmcli.ResolveWorkspace(): defaulted to "." (cwd)
//   - config.GetWorkspace():    defaulted to $HOME/.mpm
//
// That divergence was not load-bearing. It meant the ghost-DB defect fixed
// in config.GetWorkspace() after the 2026-07-21 incident (lesson
// 59fe3f8ff3e1549e) was still live in mpmcli, and audit M-1 (2026-08-31)
// reclassified the mismatch as harmless only because it had not yet traced
// what the CLI and MCP server actually did with a "." workspace.
//
// mpmcli.ResolveWorkspace() now delegates to config.GetWorkspace(), so this
// holds by construction rather than by two implementations agreeing.
func TestResolveWorkspace_DefaultsDivergence(t *testing.T) {
	t.Run("both default to the canonical runtime root", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("MPM_WORKSPACE", "")

		want := filepath.Join(home, ".mpm")
		if got := ResolveWorkspace(); got != want {
			t.Errorf("mpmcli.ResolveWorkspace() default = %q, want %q", got, want)
		}
		if got := config.GetWorkspace(); got != want {
			t.Errorf("config.GetWorkspace() default = %q, want %q", got, want)
		}
		if ResolveWorkspace() != config.GetWorkspace() {
			t.Errorf("resolvers disagree: mpmcli=%q config=%q",
				ResolveWorkspace(), config.GetWorkspace())
		}
	})

	t.Run("both honor MPM_WORKSPACE when set", func(t *testing.T) {
		t.Setenv("MPM_WORKSPACE", "/tmp/postm3-ws")
		if got := ResolveWorkspace(); got != "/tmp/postm3-ws" {
			t.Errorf("mpmcli.ResolveWorkspace() = %q, want /tmp/postm3-ws", got)
		}
		if got := config.GetWorkspace(); got != "/tmp/postm3-ws" {
			t.Errorf("config.GetWorkspace() = %q, want /tmp/postm3-ws", got)
		}
	})
}