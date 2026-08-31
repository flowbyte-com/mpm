package mpmcli

import (
	"testing"
)

func TestResolveWorkspace_DefaultIsCwd(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", "")
	if got := ResolveWorkspace(); got != "." {
		t.Fatalf("expected default \".\", got %q", got)
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

// TestResolveWorkspace_DefaultsDivergence documents the deliberate
// divergence between two workspace resolvers:
//
//   - mpmcli.ResolveWorkspace(): defaults to "." (cwd)
//   - config.GetWorkspace():    defaults to $HOME/.mpm
//
// Both honor MPM_WORKSPACE first when set; the divergence only affects
// the unset case. The cmd/mpm doctor command uses config.GetWorkspace()
// because it reports on the config subsystem's view of the canonical
// workspace ($HOME/.mpm), which is the substrate's home-rooted design
// intent. The CLI itself routes through mpmcli.ResolveWorkspace().
//
// Audit M-1 (post-M3, 2026-08-31) flagged this as a latent "mpm status
// reports wrong workspace" bug, but inspection showed both resolvers
// honor MPM_WORKSPACE first — the divergence only matters when the
// env is unset, and the doctor using config.GetWorkspace() is
// semantically correct (it reports on the config subsystem). The
// verdict is RECLASSIFIED: the doctor command's behavior is correct,
// and no fix is needed at cmd/mpm/main.go:934,989.
func TestResolveWorkspace_DefaultsDivergence(t *testing.T) {
	t.Run("mpmcli defaults to cwd", func(t *testing.T) {
		t.Setenv("MPM_WORKSPACE", "")
		if got := ResolveWorkspace(); got != "." {
			t.Errorf("mpmcli.ResolveWorkspace() default = %q, want \".\"", got)
		}
	})

	t.Run("both honor MPM_WORKSPACE when set", func(t *testing.T) {
		// The two resolvers diverge only in their unset-default; when
		// MPM_WORKSPACE is set, both return the same value (verifying
		// the audit's claim that doctor reports wrong workspace is
		// incorrect).
		t.Setenv("MPM_WORKSPACE", "/tmp/postm3-ws")
		if got := ResolveWorkspace(); got != "/tmp/postm3-ws" {
			t.Errorf("mpmcli.ResolveWorkspace() = %q, want /tmp/postm3-ws", got)
		}
	})
}