package main

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestHandleConfigShow_EmbeddingProfile verifies the output for a
// real profile-bound, configured provider. Profile line must appear
// (not the source line that fakes a profile binding). Provider name
// must be split from the model into separate fields.
func TestHandleConfigShow_EmbeddingProfile(t *testing.T) {
	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source:       mpminternal.EmbeddingSourceProfile,
		ProfileName:  "local-ollama",
		ProviderName: "ollama:nomic-embed-text",
		Status:       mpminternal.EmbeddingStatusConfigured,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	out := captureStdout(t, func() {
		c := minimalConfigForShow()
		handleConfigShow(c)
	})

	// Source label for Profile is the bare Source.String().
	if !strings.Contains(out, "source:") {
		t.Errorf("missing source line; got:\n%s", out)
	}
	if !strings.Contains(out, "profile: local-ollama") {
		t.Errorf("profile line missing or wrong value; got:\n%s", out)
	}
	// Provider/model split
	if !strings.Contains(out, "provider: ollama") {
		t.Errorf("expected provider line 'ollama'; got:\n%s", out)
	}
	if !strings.Contains(out, "model:    nomic-embed-text") {
		t.Errorf("expected model line 'nomic-embed-text'; got:\n%s", out)
	}
	if !strings.Contains(out, "status:") || !strings.Contains(out, "configured") {
		t.Errorf("status line missing; got:\n%s", out)
	}
	// The env-fallback label must NOT appear for profile source.
	if strings.Contains(out, "env (legacy fallback)") {
		t.Errorf("profile source should not display env fallback label; got:\n%s", out)
	}
}

// TestHandleConfigShow_EmbeddingEnvFallback verifies the env fallback path
// suppresses the profile line (no fake binding) and splits provider/model.
func TestHandleConfigShow_EmbeddingEnvFallback(t *testing.T) {
	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source:       mpminternal.EmbeddingSourceEnvFallback,
		ProviderName: "ollama:nomic-embed-text",
		Status:       mpminternal.EmbeddingStatusConfigured,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	out := captureStdout(t, func() {
		c := minimalConfigForShow()
		handleConfigShow(c)
	})

	// Source label uses the human-readable env fallback string.
	if !strings.Contains(out, "env (legacy fallback)") {
		t.Errorf("expected source label 'env (legacy fallback)'; got:\n%s", out)
	}
	// Profile line MUST be absent — there is no profile binding here.
	if strings.Contains(out, "profile:") {
		t.Errorf("env fallback must not display a profile line; got:\n%s", out)
	}
	// Provider/model split.
	if !strings.Contains(out, "provider: ollama") {
		t.Errorf("expected provider 'ollama'; got:\n%s", out)
	}
	if !strings.Contains(out, "model:    nomic-embed-text") {
		t.Errorf("expected model 'nomic-embed-text'; got:\n%s", out)
	}
	if !strings.Contains(out, "status:  configured") {
		t.Errorf("expected status 'configured'; got:\n%s", out)
	}
}

// TestHandleConfigShow_EmbeddingDisabled verifies the disabled path
// surfaces the explicit operator-intent line and suppresses provider/model.
func TestHandleConfigShow_EmbeddingDisabled(t *testing.T) {
	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source:                mpminternal.EmbeddingSourceDisabled,
		ProviderName:          "null",
		IntentionallyDisabled: true,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	out := captureStdout(t, func() {
		c := minimalConfigForShow()
		handleConfigShow(c)
	})

	if !strings.Contains(out, "intentionally disabled") {
		t.Errorf("expected 'intentionally disabled' line; got:\n%s", out)
	}
	// Provider/model must NOT appear in the disabled state — these are
	// operator-meaningful information for active configurations only.
	if strings.Contains(out, "provider:") || strings.Contains(out, "model:") {
		t.Errorf("disabled state should not print provider/model; got:\n%s", out)
	}
	if strings.Contains(out, "status:") {
		t.Errorf("disabled state should not print status; got:\n%s", out)
	}
}

// TestHandleConfigShow_EmbeddingAbsent verifies the absent path surfaces
// the explicit diagnostic line and suppresses the misleading
// "provider: null" / "status: null" sentinels.
func TestHandleConfigShow_EmbeddingAbsent(t *testing.T) {
	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source:       mpminternal.EmbeddingSourceAbsent,
		ProviderName: "null",
		Status:       mpminternal.EmbeddingStatusNull,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	out := captureStdout(t, func() {
		c := minimalConfigForShow()
		handleConfigShow(c)
	})

	if !strings.Contains(out, "absent") {
		t.Errorf("expected source label containing 'absent'; got:\n%s", out)
	}
	if !strings.Contains(out, "none configured") {
		t.Errorf("expected explicit 'none configured' diagnostic; got:\n%s", out)
	}
	// The misleading "provider: null" sentinel must NOT appear.
	if strings.Contains(out, "provider: null") {
		t.Errorf("absent state should not print 'provider: null'; got:\n%s", out)
	}
	if strings.Contains(out, "status:") {
		t.Errorf("absent state should not print status; got:\n%s", out)
	}
	// Profile line must NOT appear either.
	if strings.Contains(out, "profile:") {
		t.Errorf("absent state should not print profile; got:\n%s", out)
	}
}

// TestSplitProviderModel verifies the parsing helper directly so the
// display formatter stays well-defined for every ProviderName shape.
func TestSplitProviderModel(t *testing.T) {
	tests := []struct {
		in           string
		wantProvider string
		wantModel    string
	}{
		{"ollama:nomic-embed-text", "ollama", "nomic-embed-text"},
		{"openai:text-embedding-3-small", "openai", "text-embedding-3-small"},
		{"", "", ""},
		{"null", "null", ""},
		{"openrouter", "openrouter", ""},
	}
	for _, tt := range tests {
		gotP, gotM := splitProviderModel(tt.in)
		if gotP != tt.wantProvider || gotM != tt.wantModel {
			t.Errorf("splitProviderModel(%q) = (%q, %q), want (%q, %q)",
				tt.in, gotP, gotM, tt.wantProvider, tt.wantModel)
		}
	}
}

// minimalConfigForShow builds a *config.Config with the minimum fields
// required by handleConfigShow's downstream blocks (Synthesis, Profiles).
func minimalConfigForShow() *config.Config {
	c := &config.Config{}
	enabled := true
	c.SynthesisEnabled = &enabled
	return c
}
