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