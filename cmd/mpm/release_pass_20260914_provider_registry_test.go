// release_pass_20260914_provider_registry_test.go — Regression
// coverage for the 2026-09-14 capability-oriented provider
// registry and its derived views (LLM menu, embedding menu,
// model catalogue, detection contract).
//
// Coverage required by the release-pass brief:
//   A. Custom is first
//   B. remaining providers alphabetically sorted
//   C. deterministic across repeated runs
//   D. Custom model first where offered (model selection —
//      exercised through wizardPresets' choice.id/label)
//   G. generate-only appears in LLM view
//   H. embed-only appears in embedding view
//   I. embed-only rejected from LLM (covered by 10eab45d tests)
//   J. dual-capability appears in both views
//   K. unknown capability follows permissive custom contract
//   AA. existing role-validation tests remain green (separate
//       file)

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProviderRegistry_CustomFirst (cases A) — Custom is always
// index 0 in every capability-filtered view.
func TestProviderRegistry_CustomFirst(t *testing.T) {
	for _, cap := range []Capability{CapGenerate, CapEmbed} {
		view := providersFor(cap)
		if len(view) == 0 {
			t.Errorf("providersFor(%v) returned empty", cap)
			continue
		}
		if view[0].ID != "custom" {
			t.Errorf("providersFor(%v) first entry = %q, want \"custom\"", cap, view[0].ID)
		}
	}
}

// TestProviderRegistry_AlphabeticalAfterCustom (cases B, C) —
// All entries after Custom are sorted by DisplayName
// (case-insensitive). Repeated calls produce identical order.
func TestProviderRegistry_AlphabeticalAfterCustom(t *testing.T) {
	for _, cap := range []Capability{CapGenerate, CapEmbed} {
		view := providersFor(cap)
		// Compare sorted-by-DisplayName against actual ordering.
		for i := 1; i+1 < len(view); i++ {
			li := strings.ToLower(view[i].DisplayName)
			lj := strings.ToLower(view[i+1].DisplayName)
			if li > lj {
				t.Errorf("providersFor(%v) entry %d (%q) sorts AFTER entry %d (%q); want alphabetical",
					cap, i, view[i].DisplayName, i+1, view[i+1].DisplayName)
			}
		}

		// Determinism: run three times, check identical output.
		var snapshots [3][]string
		for k := 0; k < 3; k++ {
			viewK := providersFor(cap)
			ids := make([]string, len(viewK))
			for j, p := range viewK {
				ids[j] = p.ID
			}
			snapshots[k] = ids
		}
		equal := func(a, b []string) bool {
			if len(a) != len(b) {
				return false
			}
			for i := range a {
				if a[i] != b[i] {
					return false
				}
			}
			return true
		}
		if !equal(snapshots[0], snapshots[1]) || !equal(snapshots[1], snapshots[2]) {
			t.Errorf("providersFor(%v) is not deterministic across runs:\n  run1=%v\n  run2=%v\n  run3=%v",
				cap, snapshots[0], snapshots[1], snapshots[2])
		}
	}
}

// TestProviderRegistry_LLMView (cases G, J) — Anthropic, MiniMax,
// and dual-capability providers appear in the LLM view;
// generate-only providers without dual capability do too.
func TestProviderRegistry_LLMView(t *testing.T) {
	llms := providersFor(CapGenerate)
	ids := map[string]bool{}
	for _, p := range llms {
		ids[p.ID] = true
	}
	for _, want := range []string{"custom", "ollama", "openai-compatible", "anthropic", "openai", "minimax"} {
		if !ids[want] {
			t.Errorf("LLM view missing provider %q (got: %v)", want, ids)
		}
	}
}

// TestProviderRegistry_EmbeddingView (cases H, J) — Only
// providers with CapEmbed (Ollama, OpenAI, openai-compatible,
// Custom) appear. Anthropic + MiniMax do NOT (they have no
// native embedding endpoint).
func TestProviderRegistry_EmbeddingView(t *testing.T) {
	embs := providersFor(CapEmbed)
	ids := map[string]bool{}
	for _, p := range embs {
		ids[p.ID] = true
	}
	for _, want := range []string{"custom", "ollama", "openai-compatible", "openai"} {
		if !ids[want] {
			t.Errorf("Embedding view missing provider %q (got: %v)", want, ids)
		}
	}
	for _, dont := range []string{"anthropic", "minimax"} {
		if ids[dont] {
			t.Errorf("Embedding view unexpectedly includes %q (no native embedding endpoint)", dont)
		}
	}
}

// TestProviderRegistry_DualCapability (case J) — Ollama and
// openai-compatible have BOTH capabilities; Anthropic and
// MiniMax have only CapGenerate.
func TestProviderRegistry_DualCapability(t *testing.T) {
	for _, p := range canonicalProviders {
		switch p.ID {
		case "ollama", "openai-compatible", "openai":
			if !p.Has(CapGenerate) || !p.Has(CapEmbed) {
				t.Errorf("%q should have both capabilities; got %v", p.ID, p.Capabilities)
			}
		case "anthropic", "minimax":
			if !p.Has(CapGenerate) || p.Has(CapEmbed) {
				t.Errorf("%q should have CapGenerate only; got %v", p.ID, p.Capabilities)
			}
		}
	}
}

// TestProviderRegistry_PresetForIDUnknown covers the
// presetForID unknown-ID case: the function returns (zero,
// false) when the ID doesn't match a registered provider.
func TestProviderRegistry_PresetForIDUnknown(t *testing.T) {
	if _, ok := presetForID("nonexistent-provider"); ok {
		t.Errorf("presetForID must return false for unknown IDs")
	}
	p, ok := presetForID("ollama")
	if !ok {
		t.Fatalf("presetForID(\"ollama\") returned not-ok")
	}
	if p.DisplayName == "" {
		t.Errorf("presetForID(\"ollama\") returned empty DisplayName")
	}
}

// TestProviderRegistry_ModelCatalogEmptyPerSimplification pins the
// 2026-09-14 simplification contract: branded model catalogues
// (GPT families, Claude families, Gemini families, Grok, Mistral,
// Command, OpenRouter Free Router, etc.) are NOT advertised by
// `modelCatalogFor`. MPM is a substrate, not a provider catalogue;
// operators type the current model ID freeform at the Custom
// prompt.
//
// Each branded provider ID's catalog is empty/nil. Custom remains
// the public entry path that accepts any model string.
func TestProviderRegistry_ModelCatalogEmptyPerSimplification(t *testing.T) {
	for _, provider := range []string{
		"openai", "anthropic", "minimax", "ollama",
		"cohere", "google-gemini", "mistral", "xai",
		"openrouter", "openai-compatible",
	} {
		catalog := modelCatalogFor(provider)
		if len(catalog) != 0 {
			t.Errorf("modelCatalogFor(%q) must be empty (no branded presets); got %v", provider, catalog)
		}
	}
}

// TestProviderRegistry_CustomIsCapabilityUniversal pins the
// invariant: Custom can be used for BOTH generate and embed,
// regardless of the operator's setup. This is the expert escape
// hatch.
func TestProviderRegistry_CustomIsCapabilityUniversal(t *testing.T) {
	p, ok := presetForID("custom")
	if !ok {
		t.Fatalf("presetForID(\"custom\") returned not-ok")
	}
	if !p.Has(CapGenerate) || !p.Has(CapEmbed) {
		t.Errorf("Custom must support both capabilities; got %v", p.Capabilities)
	}
}

// TestProviderRegistry_OrderingStableAcrossRegistryReads
// (case C) — Reading the registry multiple times produces
// identical orderings. Defends against map-iteration-order
// regressions.
func TestProviderRegistry_OrderingStableAcrossRegistryReads(t *testing.T) {
	for _, cap := range []Capability{CapGenerate, CapEmbed} {
		var first, second []string
		{
			v := providersFor(cap)
			first = make([]string, len(v))
			for i, p := range v {
				first[i] = p.ID
			}
		}
		{
			v := providersFor(cap)
			second = make([]string, len(v))
			for i, p := range v {
				second[i] = p.ID
			}
		}
		if len(first) != len(second) {
			t.Fatalf("view length differs: %v vs %v", first, second)
		}
		for i := range first {
			if first[i] != second[i] {
				t.Errorf("view order differs at %d: %q vs %q", i, first[i], second[i])
			}
		}
	}
}

// ensure encoding/json package is used (helpers may grow).
var _ = json.Marshal
