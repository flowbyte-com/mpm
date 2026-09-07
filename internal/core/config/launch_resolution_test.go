package config

import "testing"

// TestLaunch_CanonicalDefaultPath pins the launch-default resolution:
// when only Profiles["default"] is configured, every substrate component
// that has no explicit override resolves through Profiles["default"].
//
// This is the "fresh alpha user has one profile and nothing else" path.
// The user must be able to answer "what model is MPM using?" with
// "Profiles['default']" and the answer must be unambiguous.
func TestLaunch_CanonicalDefaultPath(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "MiniMax-M2.7", BaseURL: "https://api.minimax.io/anthropic/v1", APIKey: "test-key"},
		},
	}

	// memory / critic / scheduler / synth / embedding all have no
	// explicit binding. They must each resolve to Profiles["default"].
	components := []string{"memory", "critic", "scheduler", "synth"}
	for _, comp := range components {
		p := c.ProfileFor(comp)
		if p == nil {
			t.Errorf("component %q: expected Profiles[default] fallback, got nil", comp)
			continue
		}
		if p.Name != "default" || p.Model != "MiniMax-M2.7" || p.Provider != "minimax" {
			t.Errorf("component %q: expected default profile, got name=%s provider=%s model=%s",
				comp, p.Name, p.Provider, p.Model)
		}
	}

	// Embedding is special: it has its own binding key. Without an
	// explicit components.embedding binding, ProfileFor("embedding")
	// still falls through to Profiles["default"] — operators who don't
	// configure a separate embedding profile use the default LLM.
	p := c.ProfileFor("embedding")
	if p == nil || p.Model != "MiniMax-M2.7" {
		t.Errorf("expected embedding to fall through to default, got %+v", p)
	}
}

// TestLaunch_CriticOverridePath pins the specialised-component case:
// with Profiles.critic defined and Components.critic → "critic" bound,
// the critic component resolves to the specialised profile while
// memory stays on the default.
func TestLaunch_CriticOverridePath(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "DefaultModel"},
			"critic":  {Provider: "anthropic", Model: "claude-opus"},
		},
		Components: map[string]string{
			"critic": "critic",
		},
	}

	// critic → Profiles.critic
	p := c.ProfileFor("critic")
	if p == nil || p.Name != "critic" || p.Model != "claude-opus" {
		t.Errorf("expected critic → Profiles.critic, got %+v", p)
	}

	// memory → Profiles.default (no binding for memory)
	p = c.ProfileFor("memory")
	if p == nil || p.Name != "default" || p.Model != "DefaultModel" {
		t.Errorf("expected memory → Profiles.default, got %+v", p)
	}

	// scheduler → Profiles.default
	p = c.ProfileFor("scheduler")
	if p == nil || p.Name != "default" {
		t.Errorf("expected scheduler → Profiles.default, got %+v", p)
	}
}

// TestLaunch_CapabilityToProfile_DefaultMapping pins the v0.1 default
// capability mappings: reviewer/reflect → critic, planner/summarise →
// memory. ResolveComponents returns the bound component names, which
// then resolve through ProfileFor to actual profiles.
func TestLaunch_CapabilityToProfile_DefaultMapping(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "DefaultModel"},
			"critic":  {Provider: "anthropic", Model: "claude-opus"},
		},
		Components: map[string]string{
			"critic": "critic",
			"memory": "default",
		},
	}

	cases := []struct {
		capability  string
		wantProfile string
		wantModel   string
	}{
		{"reviewer", "critic", "claude-opus"},   // default cap → critic
		{"reflect", "critic", "claude-opus"},    // default cap → critic
		{"planner", "default", "DefaultModel"},  // default cap → memory → default profile
		{"summarise", "default", "DefaultModel"}, // default cap → memory → default profile
	}
	for _, tc := range cases {
		resolved := c.ResolveComponents([]string{tc.capability})
		if len(resolved) != 1 {
			t.Errorf("%s: expected 1 resolved component, got %v", tc.capability, resolved)
			continue
		}
		p := c.ProfileFor(resolved[0])
		if p == nil {
			t.Errorf("%s: resolved %q → ProfileFor returned nil", tc.capability, resolved[0])
			continue
		}
		if p.Name != tc.wantProfile || p.Model != tc.wantModel {
			t.Errorf("%s: expected %s/%s, got %s/%s",
				tc.capability, tc.wantProfile, tc.wantModel, p.Name, p.Model)
		}
	}
}

// TestLaunch_CapabilityOverride pins the operator-override path:
// explicit Capabilities["reviewer"] = "memory" routes reviewer
// requests through memory, overriding the canonical default.
func TestLaunch_CapabilityOverride(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "DefaultModel"},
			"critic":  {Provider: "anthropic", Model: "claude-opus"},
		},
		Components: map[string]string{
			"critic": "critic",
			"memory": "default",
		},
		Capabilities: map[string]string{
			"reviewer": "memory", // operator override
		},
	}

	// reviewer → memory → default profile
	resolved := c.ResolveComponents([]string{"reviewer"})
	if len(resolved) != 1 || resolved[0] != "memory" {
		t.Fatalf("expected reviewer → memory, got %v", resolved)
	}
	p := c.ProfileFor(resolved[0])
	if p == nil || p.Model != "DefaultModel" {
		t.Errorf("expected DefaultModel via memory, got %+v", p)
	}

	// reflect is NOT overridden — still hits the default → critic.
	resolved = c.ResolveComponents([]string{"reflect"})
	if len(resolved) != 1 || resolved[0] != "critic" {
		t.Errorf("expected reflect → critic (default), got %v", resolved)
	}
}

// TestLaunch_DirectComponentStillWorks pins the contract: capability
// names are aliases on top of component names. A caller that already
// addresses components directly must continue to work unchanged.
func TestLaunch_DirectComponentStillWorks(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "DefaultModel"},
			"critic":  {Provider: "anthropic", Model: "claude-opus"},
		},
		Components: map[string]string{
			"critic": "critic",
			"memory": "default",
		},
	}

	// Direct component invocation must bypass capability translation.
	resolved := c.ResolveComponents([]string{"memory", "critic"})
	if len(resolved) != 2 || resolved[0] != "memory" || resolved[1] != "critic" {
		t.Errorf("expected direct components to pass through, got %v", resolved)
	}

	p := c.ProfileFor(resolved[0])
	if p == nil || p.Model != "DefaultModel" {
		t.Errorf("expected memory → DefaultModel, got %+v", p)
	}
	p = c.ProfileFor(resolved[1])
	if p == nil || p.Model != "claude-opus" {
		t.Errorf("expected critic → claude-opus, got %+v", p)
	}
}

// TestLaunch_EmbeddingSeparateSurface pins the embedding path:
// components.embedding = "embedding" routes the embedding subsystem to
// a dedicated profile that is independent of the general LLM profile.
func TestLaunch_EmbeddingSeparateSurface(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default":  {Provider: "minimax", Model: "MiniMax-M2.7"},
			"embedding": {Provider: "ollama", Model: "all-minilm"},
		},
		Components: map[string]string{
			"embedding": "embedding",
		},
	}

	p := c.ProfileFor("embedding")
	if p == nil {
		t.Fatal("expected embedding profile, got nil")
	}
	if p.Provider != "ollama" || p.Model != "all-minilm" {
		t.Errorf("expected embedding profile (ollama/all-minilm), got %s/%s", p.Provider, p.Model)
	}

	// General LLM profile unchanged.
	p = c.ProfileFor("memory")
	if p == nil || p.Model != "MiniMax-M2.7" {
		t.Errorf("expected memory → default MiniMax-M2.7, got %+v", p)
	}
}
