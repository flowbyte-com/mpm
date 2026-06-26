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