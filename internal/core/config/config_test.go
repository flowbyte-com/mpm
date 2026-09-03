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
