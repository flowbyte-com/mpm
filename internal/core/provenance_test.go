package internal

import (
	"os"
	"testing"
)

func TestProvenance_NewFromEnv_PriorityJSON(t *testing.T) {
	clearProvenanceEnv(t)
	t.Setenv("MPM_PROVENANCE", `{"framework":"mpm_cli","model":"sonnet","thinking_level":"high"}`)
	t.Setenv("MPM_PROVENANCE_MODEL", "should-be-overridden")

	r := NewFromEnv()
	if r.base.FrameworkName != "mpm_cli" {
		t.Errorf("framework = %q, want mpm_cli", r.base.FrameworkName)
	}
	if r.base.ModelName != "sonnet" {
		t.Errorf("model = %q, want sonnet", r.base.ModelName)
	}
	if r.base.ThinkingLevel == nil || *r.base.ThinkingLevel != "high" {
		t.Errorf("thinking_level = %v, want high", r.base.ThinkingLevel)
	}
}

func TestProvenance_NewFromEnv_FlatFallback(t *testing.T) {
	clearProvenanceEnv(t)
	// No MPM_PROVENANCE — flat vars should populate.
	t.Setenv("MPM_PROVENANCE_MODEL", "haiku")
	t.Setenv("MPM_PROVENANCE_FRAMEWORK", "openclaw")
	t.Setenv("MPM_PROVENANCE_THINKING_LEVEL", "med")

	r := NewFromEnv()
	if r.base.ModelName != "haiku" {
		t.Errorf("model = %q, want haiku", r.base.ModelName)
	}
	if r.base.FrameworkName != "openclaw" {
		t.Errorf("framework = %q, want openclaw", r.base.FrameworkName)
	}
	if r.base.ThinkingLevel == nil || *r.base.ThinkingLevel != "med" {
		t.Errorf("thinking_level = %v, want med", r.base.ThinkingLevel)
	}
}

func TestProvenance_NewFromEnv_DefaultsUnknown(t *testing.T) {
	clearProvenanceEnv(t)

	r := NewFromEnv()
	if r.base.ActorKind != "unknown" {
		t.Errorf("actor_kind = %q, want unknown", r.base.ActorKind)
	}
	if r.base.FrameworkName != "" {
		t.Errorf("framework_name = %q, want empty (NULL)", r.base.FrameworkName)
	}
	if r.base.ModelName != "" {
		t.Errorf("model_name = %q, want empty (NULL)", r.base.ModelName)
	}
	if r.base.ThinkingLevel != nil {
		t.Errorf("thinking_level = %v, want nil (NULL)", r.base.ThinkingLevel)
	}
}

func TestProvenance_ResolverResolvePerCallOverrides(t *testing.T) {
	clearProvenanceEnv(t)
	t.Setenv("MPM_PROVENANCE_MODEL", "opus")
	t.Setenv("MPM_SESSION_ID", "sess-base")

	r := NewFromEnv()
	eff := r.Resolve("sess-override", "inv-1", "parent-x", "parent-inv-x")

	if eff.SessionID != "sess-override" {
		t.Errorf("session override = %q, want sess-override", eff.SessionID)
	}
	if eff.InvocationID != "inv-1" {
		t.Errorf("invocation = %q, want inv-1", eff.InvocationID)
	}
	if eff.ParentArtifactID != "parent-x" {
		t.Errorf("parent = %q, want parent-x", eff.ParentArtifactID)
	}
	if eff.ModelName != "opus" {
		t.Errorf("model from base = %q, want opus", eff.ModelName)
	}
}

func TestProvenance_NullVsEmptySemantics(t *testing.T) {
	clearProvenanceEnv(t)

	// Env var unset → NULL/empty
	r := NewFromEnv()
	if r.base.ModelName != "" {
		t.Errorf("unset model = %q, want empty", r.base.ModelName)
	}

	// Env var set to empty string → NULL/empty (treated as unset)
	t.Setenv("MPM_PROVENANCE_MODEL", "")
	r = NewFromEnv()
	if r.base.ModelName != "" {
		t.Errorf("empty model = %q, want empty", r.base.ModelName)
	}

	// Env var set to a value → preserved
	t.Setenv("MPM_PROVENANCE_MODEL", "sonnet")
	r = NewFromEnv()
	if r.base.ModelName != "sonnet" {
		t.Errorf("set model = %q, want sonnet", r.base.ModelName)
	}
}

// clearProvenanceEnv removes all MPM_PROVENANCE_* vars and MPM_SESSION_ID/AGENT_ID.
// Tests call this in their first step so leftover env from a previous test
// cannot leak in.
func clearProvenanceEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if len(kv) > 15 && (kv[:15] == "MPM_PROVENANCE_" || kv[:12] == "MPM_SESSION_") {
			// kv is "KEY=value"; we want KEY
			for i, c := range kv {
				if c == '=' {
					os.Unsetenv(kv[:i])
					break
				}
			}
		}
	}
	if kv := os.Getenv("MPM_PROVENANCE"); kv != "" {
		os.Unsetenv("MPM_PROVENANCE")
	}
}
