package config

import "testing"

// TestLaunch_SynthCompatibility_ModernOnly pins the canonical path:
// Profiles["default"] alone resolves correctly with no legacy Synth
// block present.
func TestLaunch_SynthCompatibility_ModernOnly(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "modern-model"},
		},
	}
	p := c.ProfileFor("memory")
	if p == nil || p.Model != "modern-model" {
		t.Errorf("expected modern-model, got %+v", p)
	}
}

// TestLaunch_SynthCompatibility_LegacyOnly pins the migration path:
// a pre-profiles install with only the legacy Synth block continues
// to work via ProfileFor step 3.
func TestLaunch_SynthCompatibility_LegacyOnly(t *testing.T) {
	c := &Config{
		Synth: &SynthConfig{
			Model:   "legacy-model",
			APIKey:  "legacy-key",
			BaseURL: "https://api.legacy.example/v1",
		},
	}
	p := c.ProfileFor("memory")
	if p == nil || p.Model != "legacy-model" {
		t.Errorf("expected legacy-model, got %+v", p)
	}
	// Synth-derived profile is reported as Name="default" (the
	// canonical name). This is by design — operators see one logical
	// "default" even when their config is in legacy form.
	if p.Name != "default" {
		t.Errorf("expected Name=default for Synth-derived profile, got %s", p.Name)
	}
}

// TestLaunch_SynthCompatibility_Mixed pins the precedence rule:
// when both Profiles["default"] and a legacy Synth block are present,
// the canonical profile wins. This is the launch-critical invariant:
// modern profiles are authoritative.
func TestLaunch_SynthCompatibility_Mixed(t *testing.T) {
	c := &Config{
		Profiles: map[string]Profile{
			"default": {Provider: "minimax", Model: "modern-model"},
		},
		Synth: &SynthConfig{
			Model:   "legacy-model",
			APIKey:  "legacy-key",
			BaseURL: "https://api.legacy.example/v1",
		},
	}
	p := c.ProfileFor("memory")
	if p == nil || p.Model != "modern-model" {
		t.Errorf("expected modern-model to win over legacy-model, got %+v", p)
	}
}

// TestLaunch_SynthCompatibility_NoConfig pins the no-config fallback.
// With both Profiles and Synth empty, ProfileFor returns nil. Callers
// must handle nil explicitly (the validator surfaces this as
// "no provider configured").
func TestLaunch_SynthCompatibility_NoConfig(t *testing.T) {
	c := &Config{}
	if p := c.ProfileFor("memory"); p != nil {
		t.Errorf("expected nil with no config, got %+v", p)
	}
}

// TestLaunch_SynthCompatibility_PartialSynth pins the partial-legacy
// case: legacy Synth with only some fields populated still resolves.
// This covers operators who set just model + api_key without base_url.
func TestLaunch_SynthCompatibility_PartialSynth(t *testing.T) {
	c := &Config{
		Synth: &SynthConfig{
			Model:  "partial-legacy",
			APIKey: "legacy-key",
		},
	}
	p := c.ProfileFor("memory")
	if p == nil || p.Model != "partial-legacy" {
		t.Errorf("expected partial-legacy, got %+v", p)
	}
}
