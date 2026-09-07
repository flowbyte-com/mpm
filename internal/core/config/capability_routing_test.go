package config

import (
	"testing"
)

// TestCapabilityRouting_DefaultReviewerMapsToCritic pins the v0.1
// capability routing: with no explicit Capabilities map, the canonical
// `reviewer` capability resolves to the `critic` component (and through
// ProfileFor, to the critic profile).
//
// This is the LIVE behaviour the runtime now wires up — capabilities
// influence request_review routing via cfg.ResolveComponents at the
// handler boundary.
func TestCapabilityRouting_DefaultReviewerMapsToCritic(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"critic":  {Name: "critic", Provider: "anthropic", Model: "claude-opus"},
			"default": {Name: "default", Provider: "minimax", Model: "M2.7"},
		},
		Components: map[string]string{
			"critic": "critic",
			"memory": "default",
		},
		// Capabilities nil — falls back to DefaultCapabilities.
	}

	got := cfg.ResolveComponents([]string{"reviewer"})
	if len(got) != 1 || got[0] != "critic" {
		t.Errorf("expected reviewer → critic via defaults, got %v", got)
	}

	got = cfg.ResolveComponents([]string{"reflect"})
	if len(got) != 1 || got[0] != "critic" {
		t.Errorf("expected reflect → critic via defaults, got %v", got)
	}

	got = cfg.ResolveComponents([]string{"planner", "summarise"})
	if len(got) != 2 || got[0] != "memory" || got[1] != "memory" {
		t.Errorf("expected planner/summarise → memory via defaults, got %v", got)
	}
}

// TestCapabilityRouting_ExplicitOverrideWins pins precedence: when an
// operator has explicitly bound a capability in mpm_config.json, that
// binding overrides the canonical default.
func TestCapabilityRouting_ExplicitOverrideWins(t *testing.T) {
	cfg := &Config{
		Capabilities: map[string]string{
			"reviewer": "default", // operator-overridden: reviewer → default
		},
	}

	got := cfg.ResolveComponents([]string{"reviewer"})
	if len(got) != 1 || got[0] != "default" {
		t.Errorf("expected explicit override reviewer → default, got %v", got)
	}

	// reflect still falls through to default → critic.
	got = cfg.ResolveComponents([]string{"reflect"})
	if len(got) != 1 || got[0] != "critic" {
		t.Errorf("expected reflect → critic (default), got %v", got)
	}
}

// TestCapabilityRouting_DirectComponentPassThrough pins the
// backward-compatibility invariant: explicit component names pass
// through ResolveComponents unchanged, so `components=["memory","critic"]`
// continues to work.
func TestCapabilityRouting_DirectComponentPassThrough(t *testing.T) {
	cfg := &Config{
		Capabilities: map[string]string{
			"reviewer": "critic",
		},
	}
	got := cfg.ResolveComponents([]string{"memory", "critic"})
	if len(got) != 2 || got[0] != "memory" || got[1] != "critic" {
		t.Errorf("expected direct components to pass through, got %v", got)
	}

	// Unknown name passes through too — ProfileFor handles the lookup
	// against Components / Profiles["default"] / synth fallback.
	got = cfg.ResolveComponents([]string{"scheduler"})
	if len(got) != 1 || got[0] != "scheduler" {
		t.Errorf("expected unknown name to pass through, got %v", got)
	}
}

// TestCapabilityRouting_EmptyAndNilConfig pins the nil-safe path:
// even a nil *Config resolves canonical capability names through
// DefaultCapabilities. The ReviewCoordinator accepts a nil cfg
// (ProfileFor chains handle it), and ResolveComponents must not panic
// or silently drop names that DO have a default.
func TestCapabilityRouting_EmptyAndNilConfig(t *testing.T) {
	var nilCfg *Config
	got := nilCfg.ResolveComponents([]string{"reviewer", "memory"})
	// "reviewer" → critic (default); "memory" passes through (not a
	// canonical capability name).
	if len(got) != 2 || got[0] != "critic" || got[1] != "memory" {
		t.Errorf("nil config should consult DefaultCapabilities, got %v", got)
	}

	cfg := &Config{} // explicit empty — same behaviour as nil
	got = cfg.ResolveComponents([]string{"reviewer"})
	if len(got) != 1 || got[0] != "critic" {
		t.Errorf("empty config should still use DefaultCapabilities, got %v", got)
	}

	got = cfg.ResolveComponents(nil)
	if got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
}

// TestCapabilityRouting_MissingCapabilityPassesThrough pins the
// "name is not a capability" path. A name that doesn't match any
// capability (explicit or default) is treated as a component name and
// forwarded to ProfileFor unchanged.
func TestCapabilityRouting_MissingCapabilityPassesThrough(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"critic": {Name: "critic", Provider: "test", Model: "m"},
		},
	}
	got := cfg.ResolveComponents([]string{"nonexistent-thing"})
	if len(got) != 1 || got[0] != "nonexistent-thing" {
		t.Errorf("expected unknown name to pass through, got %v", got)
	}
}
