// cmd/mpm/provider_registry.go — Canonical provider registry.
//
// 2026-09-14 release-pass: the LLM wizard, the embedding
// wizard, the detection surface, and the help text previously
// each carried their own hand-ordered provider arrays. The
// release-pass contract collapses every public provider surface
// to one canonical registry keyed by capability.
//
// Each provider declares its capabilities as a semantic enum
// (generate / embed) rather than by vendor name. A provider may
// support one capability or both. The LLM menu is derived by
// filtering `Capabilities{Generate: true}` (plus the universal
// "custom" entry); the embedding menu is derived by filtering
// `Capabilities{Embed: true}`.
//
// Ordering (release-pass hard requirement):
//
//   - Custom is ALWAYS entry 1 (the brief explicitly carves it
//     out of alphabetical ordering — experts who know their
//     endpoint should not have to scroll past branded providers).
//   - All remaining entries are sorted alphabetically by
//     DisplayName, case-insensitive, deterministic.
//
// The registry is intentionally minimal — no discovery, no
// network. Discovery lives in cmd/mpm/detect_embedding.go and
// feeds the registry's model catalog at render time.

package main

import (
	"sort"
	"strings"
)

// Capability is a semantic role a provider can perform. The
// values are deliberately NOT vendor names; a single provider
// may support one or both.
type Capability int

const (
	// CapGenerate: provider can produce generative/completion
	// responses (LLM-style text generation). Used for the
	// Components["memory"] / Components["critic"] profile slot.
	CapGenerate Capability = 1 << iota
	// CapEmbed: provider can produce vector embeddings. Used
	// for the Components["embedding"] profile slot.
	CapEmbed
)

// ProviderDefinition describes one public provider choice.
type ProviderDefinition struct {
	// ID is the canonical profile.Provider string. Profile
	// IDs are persistent; renaming an ID breaks existing
	// configs. New entries should pick stable IDs.
	ID string

	// DisplayName is the human-readable label rendered in
	// menus and help text. Alphabetical ordering is computed
	// on this field (case-insensitive).
	DisplayName string

	// Capabilities declares which semantic roles this provider
	// supports. A provider may have any combination.
	Capabilities Capability

	// DefaultModel is the preset model suggested when this
	// provider is selected fresh. Used by the LLM wizard and
	// the detection surface. May be empty when the provider
	// requires explicit user-supplied model (e.g. Custom).
	DefaultModel string

	// DefaultBaseURL is the preset base URL. May be empty.
	DefaultBaseURL string

	// NeedsAPIKey reports whether the wizard should prompt
	// for an API key (true) or skip it because the local
	// endpoint does not require authentication (false).
	NeedsAPIKey bool
}

// Has reports whether the provider supports the given capability.
func (p ProviderDefinition) Has(c Capability) bool {
	return p.Capabilities&c != 0
}

// canonicalProviders is the single source of truth. Order in
// this slice does NOT determine menu order — views are derived
// alphabetically. Custom is exempt from alphabetical ordering
// at render time.
//
// Classification (per the release-pass brief):
//
//   A. fully supported by an existing adapter
//   B. supportable through an existing generic/OpenAI-compatible
//      adapter
//   C. requires a small adapter in this pass
//   D. unsupported / future work (deliberately not exposed)
//
// Exposed entries are A/B/C only. D entries are NOT added to the
// public menu even if their name is familiar — the brief
// explicitly forbids decorative menu options.
var canonicalProviders = []ProviderDefinition{
	// A: first-class adapters already wired in substrate.
	{
		ID:              "ollama",
		DisplayName:     "Ollama (local)",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "llama3",
		DefaultBaseURL:  "http://localhost:11434",
		NeedsAPIKey:     false,
	},

	// B: OpenAI-compatible. Synthesis uses wireOpenAI dispatch;
	// embeddings use the new OpenAICompatibleProvider added
	// in this pass.
	{
		ID:              "openai-compatible",
		DisplayName:     "OpenAI-compatible (LocalAI / LM Studio / vLLM)",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "", // operator-supplied; no single flagship
		DefaultBaseURL:  "http://localhost:1234/v1",
		NeedsAPIKey:     false,
	},

	// B: Anthropic-protocol. Synthesis supports wireAnthropic;
	// embeddings are NOT available (Anthropic has no native
	// /embeddings endpoint). Capability is CapGenerate only.
	{
		ID:              "anthropic",
		DisplayName:     "Anthropic",
		Capabilities:    CapGenerate,
		DefaultModel:    "claude-3-5-sonnet",
		DefaultBaseURL:  "https://api.anthropic.com/v1",
		NeedsAPIKey:     true,
	},

	// B: OpenAI API proper. Embedding via /v1/embeddings uses
	// the new OpenAICompatibleProvider (it speaks the same
	// wire). Default model for embedding is a sensible
	// flagship, not exhaustive.
	{
		ID:              "openai",
		DisplayName:     "OpenAI",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "gpt-4o",
		DefaultBaseURL:  "https://api.openai.com/v1",
		NeedsAPIKey:     true,
	},

	// B: MiniMax is Anthropic-protocol. Already in the
	// historical wizardPresets; preserved with its
	// anthropic-compatible base URL.
	{
		ID:              "minimax",
		DisplayName:     "MiniMax",
		Capabilities:    CapGenerate,
		DefaultModel:    "MiniMax-M2.7",
		DefaultBaseURL:  "https://api.minimax.io/anthropic/v1",
		NeedsAPIKey:     true,
	},

	// A: Custom is the expert escape hatch. Always first in
	// every menu. No defaults, no discovery.
	{
		ID:              "custom",
		DisplayName:     "Custom (I know what I'm doing)",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "",
		DefaultBaseURL:  "",
		NeedsAPIKey:     false,
	},
}

// Deliberately NOT exposed as menu entries (D — future work):
//
//   - AWS Bedrock: requires AWS SigV4 signing. Out of scope for
//     this release.
//   - Azure OpenAI: protocol-compatible with OpenAI but
//     deployment-name routing differs. Operators can use the
//     Custom entry with the Azure endpoint until a first-class
//     adapter lands.
//   - Google Gemini / Vertex AI: different auth + protocol.
//     Future work.
//   - Cohere: different API shape.
//   - Voyage AI: different API shape.
//   - Hugging Face TEI: OpenAI-compatible `/v1/embeddings`; can
//     be reached today via the openai-compatible entry with a
//     custom base URL. Promoting to a first-class entry is
//     future work once we want TEI-specific model catalogs.

// providersFor returns the providers that support capability
// c, sorted with Custom first and the remainder alphabetical
// by DisplayName (case-insensitive). The returned slice is a
// fresh copy; callers may not mutate canonicalProviders through
// it.
func providersFor(c Capability) []ProviderDefinition {
	var out []ProviderDefinition
	for _, p := range canonicalProviders {
		if p.Has(c) {
			out = append(out, p)
		}
	}
	sortProvidersWithCustomFirst(out)
	return out
}

// sortProvidersWithCustomFirst reorders providers so the entry
// with ID=="custom" is at index 0 and the remainder are sorted
// alphabetically by DisplayName (case-insensitive, ID as a
// deterministic tiebreak). In-place mutation of the input
// slice — the caller retains ownership. Returns the same
// slice header for chainability.
func sortProvidersWithCustomFirst(providers []ProviderDefinition) []ProviderDefinition {
	// Partition: custom goes first, rest follows.
	var custom ProviderDefinition
	hasCustom := false
	rest := make([]ProviderDefinition, 0, len(providers))
	for _, p := range providers {
		if p.ID == "custom" {
			custom = p
			hasCustom = true
			continue
		}
		rest = append(rest, p)
	}
	sort.SliceStable(rest, func(i, j int) bool {
		li := strings.ToLower(rest[i].DisplayName)
		lj := strings.ToLower(rest[j].DisplayName)
		if li != lj {
			return li < lj
		}
		// Tiebreak on ID so map-iteration order cannot reorder.
		return rest[i].ID < rest[j].ID
	})
	if hasCustom {
		out := append([]ProviderDefinition{custom}, rest...)
		copy(providers, out)
	} else {
		copy(providers, rest)
	}
	return providers
}

// presetForID returns the canonical provider with the given ID,
// or (ProviderDefinition{}, false) if no match.
func presetForID(id string) (ProviderDefinition, bool) {
	for _, p := range canonicalProviders {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderDefinition{}, false
}

// modelCatalogFor returns the curated model preset list for a
// provider, for use when no live discovery is available. The
// Custom entry gets an empty list (operator types the model).
//
// 2026-09-14 release-pass: do NOT hard-code a single flagship
// per provider. Each catalog includes a small set of widely-
// deployed models. Operators are not punished for picking a
// non-flagship; the Custom entry guarantees forward compatibility.
var modelCatalogFor = func(providerID string) []string {
	switch providerID {
	case "openai":
		return []string{
			"gpt-4o",
			"gpt-4o-mini",
			"o3",
			"o3-mini",
			"o4-mini",
		}
	case "anthropic":
		return []string{
			"claude-3-5-haiku",
			"claude-3-5-sonnet",
			"claude-3-opus",
			"claude-sonnet-4-5",
		}
	case "minimax":
		return []string{
			"MiniMax-M2.7",
			"MiniMax-M2.7-highspeed",
		}
	case "ollama":
		// Ollama: discovery is preferred over a preset catalog.
		// The catalog is a fallback used only when /api/tags is
		// unreachable. Empty here signals "discover if you can".
		return nil
	case "openai-compatible":
		// Operator-supplied. We have no model catalog for
		// arbitrary compatible endpoints.
		return nil
	}
	return nil
}

// modelSortKey returns the comparison key used for model
// ordering. Custom (manual) entry is ALWAYS first; remainder
// is sorted alphabetically, case-insensitive, with a
// deterministic tiebreak on the original string so equal
// namespacing cannot reorder.
func modelSortKey(name string) string {
	return strings.ToLower(name)
}
