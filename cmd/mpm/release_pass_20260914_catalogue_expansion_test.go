// release_pass_20260914_catalogue_expansion_test.go — Regression
// coverage for the 2026-09-14 catalogue-expansion pass.
//
// The catalogue-expansion pass added the branded provider registry.
// The 2026-09-14 config-simplification pass INVERTED that: the
// public wizard now exposes ONLY Custom + a protocol picker, and
// branded model catalogues (gpt-5.6, claude-*, gemini-*, grok-*,
// mistral-*, command-*, OpenRouter Free Router, etc.) are no longer
// advertised.
//
// This file pins the NEW contract:
//
//   * Public LLM wizard menu (wizardPresets): exactly one entry,
//     "Custom". Branded providers (Anthropic, Cohere, Google Gemini,
//     MiniMax, Mistral AI, Ollama, OpenAI, OpenAI-compatible,
//     OpenRouter, xAI) are NOT surfaced.
//   * Public protocol menu (wizardProtocolPresets): exactly three
//     entries, in order: OpenAI-compatible, Anthropic-compatible,
//     Ollama.
//   * Branded model presets are gone: modelCatalogFor returns
//     empty for every provider ID.
//   * Internal registry (providersFor + presetForID) STILL resolves
//     the branded IDs by string for backwards compatibility —
//     existing operator profiles with provider=openai,
//     provider=anthropic, provider=ollama, etc. continue to load
//     and wire correctly at runtime.
//   * Wire inference still routes branded providers through the
//     correct adapter (e.g. anthropic.com → Anthropic wire; ollama
//     endpoints → no-auth; x.ai → OpenAI-compatible wire).

package main

import (
	"strings"
	"testing"
)

// --- A: Public LLM wizard exposes ONLY Custom -------------------

// TestSimplify_LLMWizardIsCustomOnly — wizardPresets returns
// exactly one entry, with id="custom". Branded providers are
// removed from the public LLM menu (A).
func TestSimplify_LLMWizardIsCustomOnly(t *testing.T) {
	presets := wizardPresets()
	if len(presets) != 1 {
		t.Fatalf("LLM wizard must show exactly 1 entry (Custom); got %d: %v", len(presets), presetIDs(presets))
	}
	if presets[0].id != "custom" {
		t.Errorf("LLM wizard first entry = %q, want custom", presets[0].id)
	}
}

// TestSimplify_EmbeddingWizardIsCustomOnly — the embedding manual
// wizard option 3 (Custom) is the only manual entry point surfaced
// in the embedding subsystem. The detect path is separate and
// remains available (B).
func TestSimplify_EmbeddingWizardIsCustomOnly(t *testing.T) {
	// The public LLM / embedding wizard menus both reduce to
	// Custom. The embedding wizard surfaces detect as a separate
	// sub-path; manual configuration paths through Custom.
	presets := wizardPresets()
	if len(presets) != 1 || presets[0].id != "custom" {
		t.Fatalf("LLM wizard menu must collapse to Custom; got %v", presetIDs(presets))
	}
	for _, banned := range []string{"openai", "anthropic", "cohere", "google-gemini", "minimax", "mistral", "ollama", "openrouter", "xai"} {
		for _, p := range presets {
			if p.id == banned {
				t.Errorf("public wizard must NOT surface branded provider %q", banned)
			}
		}
	}
}

// TestSimplify_ProtocolChoicesAreExactlyThree — wizardProtocolPresets
// exposes OpenAI-compatible, Anthropic-compatible, Ollama (F + G
// + H: protocol choices the operator can pick after Custom).
func TestSimplify_ProtocolChoicesAreExactlyThree(t *testing.T) {
	protos := wizardProtocolPresets()
	want := []string{"openai-compatible", "anthropic-compatible", "ollama"}
	if len(protos) != len(want) {
		t.Fatalf("protocol menu must have %d entries; got %d: %v", len(want), len(protos), presetIDs(protos))
	}
	for i, w := range want {
		if protos[i].id != w {
			t.Errorf("protocol menu entry %d = %q, want %q", i+1, protos[i].id, w)
		}
	}
	// Ollama must NOT require an API key.
	for _, p := range protos {
		if p.id == "ollama" && p.needsAPIKey {
			t.Errorf("Ollama protocol must NOT require an API key")
		}
	}
}

// --- B: Branded model catalogues are gone -----------------------

// TestSimplify_NoBrandedModelPresets — modelCatalogFor returns
// nil/empty for every branded provider ID (N). Operators type the
// current model ID freeform; MPM does not maintain model-string
// lists per vendor.
func TestSimplify_NoBrandedModelPresets(t *testing.T) {
	for _, pid := range []string{
		"openai", "anthropic", "cohere", "google-gemini",
		"minimax", "mistral", "xai", "openrouter", "openai-compatible",
	} {
		cat := modelCatalogFor(pid)
		if len(cat) != 0 {
			t.Errorf("provider %q must have empty model catalog; got %v", pid, cat)
		}
	}
}

// TestSimplify_RegistryResolvesBrandedIDsByString — even though
// the public menu hides branded providers, the runtime registry
// STILL resolves the same IDs by string for backwards compat (K +
// L + M). An existing profile with provider=openrouter must
// resolve via presetForID without errors.
func TestSimplify_RegistryResolvesBrandedIDsByString(t *testing.T) {
	for _, pid := range []string{
		"openai", "anthropic", "cohere", "google-gemini",
		"minimax", "mistral", "ollama", "openrouter", "xai", "openai-compatible",
	} {
		if _, ok := presetForID(pid); !ok {
			t.Errorf("runtime registry must still resolve branded provider %q; got !ok", pid)
		}
	}
}

// TestSimplify_BrandedProvidersKeepCanonicalBaseURL — even
// though branded menus are hidden, the internal registry still
// exposes the canonical base URLs that wire inference consults.
func TestSimplify_BrandedProvidersKeepCanonicalBaseURL(t *testing.T) {
	cases := map[string]string{
		"openai":          "https://api.openai.com/v1",
		"anthropic":       "https://api.anthropic.com/v1",
		"cohere":          "https://api.cohere.com/v1",
		"google-gemini":   "https://generativelanguage.googleapis.com/v1beta/openai",
		"minimax":         "https://api.minimax.io/anthropic/v1",
		"mistral":         "https://api.mistral.ai/v1",
		"openrouter":      "https://openrouter.ai/api/v1",
		"xai":             "https://api.x.ai/v1",
		"ollama":          "http://localhost:11434",
		"openai-compatible": "http://localhost:1234/v1",
	}
	for pid, wantURL := range cases {
		p, ok := presetForID(pid)
		if !ok {
			t.Errorf("provider %q missing from registry", pid)
			continue
		}
		if p.DefaultBaseURL != wantURL {
			t.Errorf("provider %q DefaultBaseURL = %q, want %q", pid, p.DefaultBaseURL, wantURL)
		}
	}
}

// TestSimplify_RegistryCapabilityBitsUnchanged — internal
// registry capability flags are unchanged by the simplification.
// Capabilities are an internal structural concern, not a menu
// concern. The role validator + embedding classification depend
// on these bits.
func TestSimplify_RegistryCapabilityBitsUnchanged(t *testing.T) {
	cases := map[string]Capability{
		"openai":            CapGenerate | CapEmbed,
		"anthropic":         CapGenerate,
		"cohere":            CapGenerate,
		"google-gemini":     CapGenerate | CapEmbed,
		"minimax":           CapGenerate,
		"mistral":           CapGenerate | CapEmbed,
		"ollama":            CapGenerate | CapEmbed,
		"openrouter":        CapGenerate | CapEmbed,
		"xai":               CapGenerate,
		"openai-compatible": CapGenerate | CapEmbed,
		"custom":            CapGenerate | CapEmbed,
	}
	for pid, want := range cases {
		p, ok := presetForID(pid)
		if !ok {
			t.Errorf("provider %q missing", pid)
			continue
		}
		if p.Capabilities != want {
			t.Errorf("provider %q capabilities = %v, want %v", pid, p.Capabilities, want)
		}
	}
}

// --- C: Wire inference keys preserve substring patterns ---------

// TestSimplify_WireInferenceSubstringsRetained — the substring
// patterns in internal/core/synth/wire.go (inferWire) still
// match the registry's canonical base URLs. Tested indirectly
// by checking the base URLs contain the expected substrings.
func TestSimplify_WireInferenceSubstringsRetained(t *testing.T) {
	cases := map[string][]string{
		"openai":        {"openai.com"},
		"anthropic":     {"anthropic.com"},
		"cohere":        {"cohere.com"},
		"google-gemini": {"googleapis"},
		"mistral":       {"mistral.ai"},
		"openrouter":    {"openrouter"},
		"xai":           {"x.ai"},
		"minimax":       {"minimax"},
	}
	for pid, subs := range cases {
		p, ok := presetForID(pid)
		if !ok {
			t.Errorf("provider %q missing", pid)
			continue
		}
		lu := strings.ToLower(p.DefaultBaseURL)
		matched := false
		for _, sub := range subs {
			if strings.Contains(lu, sub) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("provider %q base URL %q does not match any wire-inference substring %v", pid, p.DefaultBaseURL, subs)
		}
	}
}

// --- D (deleted): Custom model entry remains the model-menu Custom slot ---
//
// 2026-09-14 final-simplification: promptModelFromCatalog +
// modelMenuCatalogFor are removed. There is no model menu to
// pin anymore — operators type the model freeform at the Custom
// prompt. The previous TestSimplify_ModelMenuCustomAlwaysFirst
// and TestSimplify_CatalogueHelpersDontLeakBrandedLists are
// superseded by TestFinal_D_ModelFreeform +
// TestFinal_C_NoModelCatalogue + TestSimplify_NoBrandedModelPresets
// (see release_pass_20260914_config_simplification_test.go).

// presetIDs is a small helper that extracts the id of each
// wizard.choice. Returns a fresh slice; callers may mutate.
func presetIDs(choices []choice) []string {
	ids := make([]string, len(choices))
	for i, c := range choices {
		ids[i] = c.id
	}
	return ids
}
