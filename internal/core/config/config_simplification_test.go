package config

import (
	"testing"
)

// TestProfileFor_ComponentsBindingWins pins step 1 of the resolution chain:
// an explicit Components[component] binding overrides both Profiles["default"]
// and the legacy Synth block.
func TestProfileFor_ComponentsBindingWins(t *testing.T) {
	c := &Config{
		Components: map[string]string{"critic": "reviewer-llm"},
		Profiles: map[string]Profile{
			"default":       {Provider: "minimax", Model: "M2.7"},
			"reviewer-llm":  {Provider: "openrouter", Model: "sonnet"},
		},
		Synth: &SynthConfig{Model: "legacy", BaseURL: "https://legacy.example/v1"},
	}
	p := c.ProfileFor("critic")
	if p == nil {
		t.Fatal("expected explicit Components binding to resolve, got nil")
	}
	if p.Name != "reviewer-llm" || p.Model != "sonnet" {
		t.Errorf("expected binding override (reviewer-llm/sonnet), got %s/%s", p.Name, p.Model)
	}
}

// TestProfileFor_DefaultsFallback pins step 2: when no Components binding
// exists, ProfileFor returns Profiles["default"]. This is the launch-default
// path for memory/critic/scheduler components.
func TestProfileFor_DefaultsFallback(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default":  {Provider: "minimax", Model: "M2.7"},
			"embedding": {Provider: "ollama", Model: "all-minilm"},
		},
	}
	p := c.ProfileFor("critic")
	if p == nil {
		t.Fatal("expected default fallback, got nil")
	}
	if p.Name != "default" || p.Model != "M2.7" {
		t.Errorf("expected default profile, got %s/%s", p.Name, p.Model)
	}
}

// TestProfileFor_SynthMigrationFallback pins step 3: when neither a
// Components binding nor Profiles["default"] exists, ProfileFor falls
// through to the legacy Synth block as a one-way migration path. The
// Synth-derived profile is reported with Name="default".
func TestProfileFor_SynthMigrationFallback(t *testing.T) {
	c := &Config{
		Synth: &SynthConfig{
			Model:   "legacy-M",
			BaseURL: "https://api.minimax.io/anthropic/v1",
			APIKey:  "legacy-key",
		},
	}
	p := c.ProfileFor("memory")
	if p == nil {
		t.Fatal("expected synth migration fallback, got nil")
	}
	if p.Name != "default" || p.Model != "legacy-M" {
		t.Errorf("expected synth-derived default profile, got %s/%s", p.Name, p.Model)
	}
	if p.APIKey != "legacy-key" {
		t.Errorf("expected API key from synth block, got %q", p.APIKey)
	}
}

// TestProfileFor_MixedConfigPrefersProfile pins the precedence rule
// (modern Profiles["default"] wins over legacy Synth block) when both
// are configured. This is the canonical migration rule.
func TestProfileFor_MixedConfigPrefersProfile(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "modern-M"},
		},
		Synth: &SynthConfig{
			Model:   "legacy-M",
			BaseURL: "https://api.minimax.io/anthropic/v1",
			APIKey:  "legacy-key",
		},
	}
	p := c.ProfileFor("memory")
	if p == nil {
		t.Fatal("expected resolution, got nil")
	}
	if p.Name != "default" || p.Model != "modern-M" {
		t.Errorf("expected modern profile to win (default/modern-M), got %s/%s", p.Name, p.Model)
	}
}

// TestProfileFor_NothingConfigured pins step 4: ProfileFor returns nil
// when nothing resolves. Callers must handle nil explicitly (e.g.
// "no profile resolves to component" diagnostic).
func TestProfileFor_NothingConfigured(t *testing.T) {
	c := &Config{}
	if p := c.ProfileFor("critic"); p != nil {
		t.Errorf("expected nil with empty config, got %+v", p)
	}
}

// TestProfileFor_EmbeddingProfileBinding pins the embedding routing
// surface: components["embedding"] = "<profile>" resolves through
// ProfileFor("embedding"). Used by the embedding subsystem as the
// canonical path.
func TestProfileFor_EmbeddingProfileBinding(t *testing.T) {
	c := &Config{
		Components: map[string]string{"embedding": "local-ollama"},
		Profiles: map[string]Profile{
			"default":      {Provider: "minimax", Model: "M2.7"},
			"local-ollama": {Provider: "ollama", Model: "all-minilm"},
		},
	}
	p := c.ProfileFor("embedding")
	if p == nil {
		t.Fatal("expected embedding profile binding to resolve, got nil")
	}
	if p.Name != "local-ollama" || p.Provider != "ollama" {
		t.Errorf("expected local-ollama profile, got %s/%s", p.Name, p.Provider)
	}
}

// TestProfileFor_EmbeddingDisabledSentinel pins the "disabled" sentinel:
// components.embedding = "disabled" returns nil so the embedding resolver
// can map it to IntentionallyDisabled. Same shape as a missing binding.
func TestProfileFor_EmbeddingDisabledSentinel(t *testing.T) {
	c := &Config{
		Components: map[string]string{"embedding": "disabled"},
		Profiles:   map[string]Profile{"default": {Provider: "minimax", Model: "M2.7"}},
	}
	if p := c.ProfileFor("embedding"); p != nil {
		t.Errorf("expected nil for disabled sentinel, got %+v", p)
	}
}

// TestProfileFor_ComponentBoundToMissingProfile pins the broken-binding
// case: Components["critic"] = "ghost" with no matching Profiles["ghost"]
// falls through to Profiles["default"] (step 2). Operators see the
// broken binding in `mpm config validate`, but runtime ProfileFor returns
// a usable profile rather than nil.
func TestProfileFor_ComponentBoundToMissingProfile(t *testing.T) {
	c := &Config{
		Components: map[string]string{"critic": "ghost"},
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "M2.7"},
		},
	}
	p := c.ProfileFor("critic")
	if p == nil {
		t.Fatal("expected default fallback when binding is broken, got nil")
	}
	if p.Name != "default" {
		t.Errorf("expected default fallback profile, got %q", p.Name)
	}
}

// TestCapabilityFor_ConfiguredMapping pins the capability vocabulary
// surface: Config.CapabilityFor returns the operator-set binding
// (planner/reviewer/reflect are the launch defaults).
func TestCapabilityFor_ConfiguredMapping(t *testing.T) {
	c := &Config{
		Capabilities: map[string]string{
			"reviewer": "critic",
			"planner":  "memory",
		},
	}
	if c.CapabilityFor("reviewer") != "critic" {
		t.Errorf("expected reviewer → critic, got %q", c.CapabilityFor("reviewer"))
	}
	if c.CapabilityFor("planner") != "memory" {
		t.Errorf("expected planner → memory, got %q", c.CapabilityFor("planner"))
	}
	if c.CapabilityFor("nonexistent") != "" {
		t.Errorf("expected empty for unbound capability, got %q", c.CapabilityFor("nonexistent"))
	}
}

// TestCapabilityFor_UnconfiguredFallsBackToDefaults pins the v0.1
// behaviour: CapabilityFor returns the canonical binding from
// DefaultCapabilities when no explicit Config.Capabilities entry
// exists. Truly unknown capability names still return "" so callers
// can distinguish "no capability here" from "no binding at all".
func TestCapabilityFor_UnconfiguredFallsBackToDefaults(t *testing.T) {
	c := &Config{}
	if got := c.CapabilityFor("reviewer"); got != "critic" {
		t.Errorf("expected reviewer → critic (default), got %q", got)
	}
	if got := c.CapabilityFor("reflect"); got != "critic" {
		t.Errorf("expected reflect → critic (default), got %q", got)
	}
	if got := c.CapabilityFor("planner"); got != "memory" {
		t.Errorf("expected planner → memory (default), got %q", got)
	}
	// Truly unknown name returns "" — caller treats it as a component.
	if got := c.CapabilityFor("nonexistent-cap"); got != "" {
		t.Errorf("expected empty for unknown capability, got %q", got)
	}
}

// TestProfileFor_ResolvedNameSetsCorrectly verifies that the defensive
// copy returned by ProfileFor has Name set to the resolved key
// (matching the binding or "default"), NOT the empty Name from
// Profiles[name].
func TestProfileFor_ResolvedNameSetsCorrectly(t *testing.T) {
	c := &Config{
		Components: map[string]string{"critic": "reviewer-llm"},
		Profiles: map[string]Profile{
			// Note: Name field empty here — ProfileFor must fill it.
			"reviewer-llm": {Provider: "openrouter", Model: "sonnet"},
		},
	}
	p := c.ProfileFor("critic")
	if p == nil {
		t.Fatal("expected resolution, got nil")
	}
	if p.Name != "reviewer-llm" {
		t.Errorf("expected Name=reviewer-llm (binding key), got %q", p.Name)
	}
}
