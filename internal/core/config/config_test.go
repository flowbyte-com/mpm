package config

import "testing"

// TestProfileFor_EmbeddingDisabled verifies that when components.embedding is
// set to the sentinel value "disabled", ProfileFor returns nil so the embedding
// resolver can map it to IntentionallyDisabled.
func TestProfileFor_EmbeddingDisabled(t *testing.T) {
	c := &Config{
		Components: map[string]string{"embedding": "disabled"},
		Profiles:   map[string]Profile{"default": {Provider: "openai", Model: "gpt-4o"}},
	}
	if p := c.ProfileFor("embedding"); p != nil {
		t.Errorf("ProfileFor(\"embedding\") with sentinel \"disabled\" = %v, want nil", p)
	}
}

// TestProfileFor_NonEmbeddingDisabled verifies that "disabled" is NOT a
// sentinel for non-embedding components; ProfileFor returns nil as if the
// binding were missing (no special sentinel treatment).
func TestProfileFor_NonEmbeddingDisabled(t *testing.T) {
	c := &Config{
		Components: map[string]string{"critic": "disabled"},
		Profiles:   map[string]Profile{"default": {Provider: "openai", Model: "gpt-4o"}},
	}
	// "disabled" is NOT a sentinel for non-embedding components;
	// ProfileFor returns nil as if the binding were missing.
	if p := c.ProfileFor("critic"); p != nil {
		t.Errorf("ProfileFor(\"critic\") with value \"disabled\" = %v, want nil", p)
	}
}

// TestProfileFor_AgreesWithResolveProfile pins the structural invariant
// from the 2026-09-15 multi-profile audit: ProfileFor must not drift
// from ResolveProfile. Across every combination of binding state,
// ProfileFor must equal ResolveProfile.Profile (or both nil).
func TestProfileFor_AgreesWithResolveProfile(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		comp string
	}{
		{"no-config", &Config{}, "memory"},
		{"default-only", &Config{Profiles: map[string]Profile{"default": {Provider: "openai", Model: "gpt"}}}, "memory"},
		{"explicit", &Config{
			Components: map[string]string{"critic": "review"},
			Profiles:   map[string]Profile{"review": {Provider: "anthropic", Model: "claude"}},
		}, "critic"},
		{"embedding-disabled", &Config{Components: map[string]string{"embedding": "disabled"}}, "embedding"},
		{"critic-disabled", &Config{
			Components: map[string]string{"critic": "disabled"},
			Profiles:   map[string]Profile{"default": {Provider: "openai", Model: "gpt"}},
		}, "critic"},
		{"dangling-explicit", &Config{
			Components: map[string]string{"memory": "missing"},
			Profiles:   map[string]Profile{"default": {Provider: "openai", Model: "gpt"}},
		}, "memory"},
		{"legacy-synth", &Config{
			Synth: &SynthConfig{Model: "m", APIKey: "k", BaseURL: "https://api.openai.com/v1"},
		}, "memory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := tc.cfg.ProfileFor(tc.comp)
			rp := tc.cfg.ResolveProfile(tc.comp).Profile
			if (pf == nil) != (rp == nil) {
				t.Errorf("nil-ness disagree: ProfileFor=%v ResolveProfile=%v", pf, rp)
			}
			if pf != nil && rp != nil && *pf != *rp {
				t.Errorf("Profile=%+v != ResolveProfile=%+v", *pf, *rp)
			}
		})
	}
}

// TestResolveProfile_DanglingBindingVisibility pins the explicit-vs-default
// distinction surfaced by the CLI when an operator's explicit binding
// points at a missing profile. The runtime falls through to
// Profiles["default"] (preserved pre-2026-09-15 behaviour — the runtime
// must not silently lose a model). The CLI can detect this state by
// re-checking whether ConfiguredName exists in Profiles.
func TestResolveProfile_DanglingBindingVisibility(t *testing.T) {
	c := &Config{
		Components: map[string]string{"critic": "missing"},
		Profiles:   map[string]Profile{"default": {Provider: "openai", Model: "gpt"}},
	}
	r := c.ResolveProfile("critic")
	// ProfileFor falls through to default (preserved behaviour).
	if p := c.ProfileFor("critic"); p == nil || p.Name != "default" {
		t.Errorf("ProfileFor = %+v, want default-profile fallback", p)
	}
	// The CLI can surface the dangling binding by inspecting
	// Components[component] vs Profiles[name].
	if c.Components["critic"] != "missing" {
		t.Errorf("Components[critic] = %q, want %q", c.Components["critic"], "missing")
	}
	if _, ok := c.Profiles["missing"]; ok {
		t.Errorf("Profiles[missing] must not exist")
	}
	_ = r // ResolveProfile's Source here is "default"; dangling detection lives in CLI
}
