// cmd/mpm/provider_registry.go — Canonical provider registry.
//
// 2026-09-14 config-simplification pass: MPM is a substrate, not
// a provider catalogue. Public wizard UX exposes ONLY Custom;
// branded presets are deliberately removed from the menus users
// see. The registry below is the INTERNAL truth — used by:
//
//   - Runtime profile resolution by ID (`presetForID`). Existing
//     profiles written under a branded provider ID
//     (provider=openai, provider=anthropic, provider=ollama,
//     etc.) MUST continue to load and wire correctly without
//     the operator having to rename them. The provider ID is
//     the address; the registry resolves it to wire, capability,
//     and base URL hints.
//   - The role validator (ValidateLLMRole) for capability
//     filtering — Ollama capability probing, embedding-only
//     rejection, etc.
//   - The embedding discovery surface (`mpm config
//     detect-embedding`), which probes Ollama +
//     OpenAI-compatible localhost endpoints regardless of which
//     branded IDs exist in the registry.
//
// Public wizard choices, persisted legacy/current provider IDs,
// and internal transport adapters are intentionally separated:
//
//   - Public wizards (the `mpm config` interactive LLM/embedding
//     prompts, `mpm config profile add`, `mpm config` help text)
//     surface ONLY Custom + a protocol picker (OpenAI-compatible
//     / Anthropic-compatible / Ollama). The wizard writes
//     `provider: "custom"` and a base URL; runtime wire
//     inference decides which adapter speaks.
//   - Persisted provider IDs (Profiles[...].Provider) keep their
//     historical values so existing configs continue to load
//     unchanged. Runtime falls through to base-URL wire inference
//     for any ID.
//
// Branded model catalogues (GPT families, Claude families,
// Gemini families, Grok, Mistral, Command, OpenRouter Free
// Router, etc.) are no longer advertised by the wizard. Each
// operator types the current model ID freeform in the Custom
// path; the canonical model strings live with the provider, not
// in this substrate.
//
// Ordering (capability-oriented public-API contract that still
// applies when callers do want the full registry view, e.g. for
// diagnostic surfaces or future operator tooling):
//
//   - Custom is ALWAYS entry 1.
//   - All remaining entries are sorted alphabetically by
//     DisplayName, case-insensitive, deterministic.
//
// The registry carries NO network IO. Discovery lives in
// cmd/mpm/detect_embedding.go.

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

	// A: OpenAI-compatible. The canonical "wire" abstraction
	// surfaced through the public wizard when the operator picks
	// Custom + OpenAI-compatible protocol. Synthesis uses
	// wireOpenAI dispatch; embeddings use the
	// OpenAICompatibleProvider. DefaultBaseURL is a localhost
	// example only (LM Studio on 1234 is the most common local
	// endpoint); operators override per their stack.
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
	// DefaultModel is empty per the simplification rule
	// (operators type any current Anthropic model ID at the
	// Custom prompt; runtime reads Profiles[...].Model verbatim).
	{
		ID:              "anthropic",
		DisplayName:     "Anthropic",
		Capabilities:    CapGenerate,
		DefaultModel:    "",
		DefaultBaseURL:  "https://api.anthropic.com/v1",
		NeedsAPIKey:     true,
	},

	// B: Cohere exposes an OpenAI-compat endpoint at
	// api.cohere.com/v1 for CHAT / GENERATION only. Cohere's
	// Embed v2 endpoint uses a different request shape
	// (input_type, embedding_types) and is NOT covered by the
	// OpenAI-compat surface. Capability is therefore
	// CapGenerate only. DefaultModel is empty per the
	// simplification rule.
	{
		ID:              "cohere",
		DisplayName:     "Cohere",
		Capabilities:    CapGenerate,
		DefaultModel:    "",
		DefaultBaseURL:  "https://api.cohere.com/v1",
		NeedsAPIKey:     true,
	},

	// B: OpenAI API proper. Embedding via /v1/embeddings uses
	// the OpenAICompatibleProvider (same wire). DefaultModel is
	// left empty deliberately — the public wizard does NOT
	// advertise a flagship OpenAI model. Existing operators
	// who've set DefaultModel manually continue to use it;
	// runtime reads Profiles[...].Model verbatim.
	{
		ID:              "openai",
		DisplayName:     "OpenAI",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "",
		DefaultBaseURL:  "https://api.openai.com/v1",
		NeedsAPIKey:     true,
	},

	// B: Google Gemini exposes an OpenAI-compat endpoint at
	// generativelanguage.googleapis.com/v1beta/openai.
	// Capability is generate + embed. DefaultModel is empty
	// per the simplification rule.
	{
		ID:              "google-gemini",
		DisplayName:     "Google Gemini",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "",
		DefaultBaseURL:  "https://generativelanguage.googleapis.com/v1beta/openai",
		NeedsAPIKey:     true,
	},

	// B: MiniMax is Anthropic-protocol. DefaultModel is empty per
	// the simplification rule (operators type any current MiniMax
	// model ID at the Custom prompt).
	{
		ID:              "minimax",
		DisplayName:     "MiniMax",
		Capabilities:    CapGenerate,
		DefaultModel:    "",
		DefaultBaseURL:  "https://api.minimax.io/anthropic/v1",
		NeedsAPIKey:     true,
	},

	// B: Mistral AI exposes an OpenAI-compat endpoint at
	// api.mistral.ai/v1 for both chat and embeddings. The
	// `/v1/embeddings` endpoint accepts the OpenAI-shaped
	// `{input, model}` payload, so the existing
	// OpenAICompatibleProvider works against Mistral unchanged.
	// DefaultModel is empty per the simplification rule.
	{
		ID:              "mistral",
		DisplayName:     "Mistral AI",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "",
		DefaultBaseURL:  "https://api.mistral.ai/v1",
		NeedsAPIKey:     true,
	},

	// B: OpenRouter is the multi-model aggregator. It
	// exposes an OpenAI-compat endpoint at
	// openrouter.ai/api/v1. DefaultModel is empty per the
	// simplification rule (OpenRouter Free Router is not
	// advertised). Embedding discovery at
	// `mpm config detect-embedding` still probes
	// /api/v1/models when reachable.
	{
		ID:              "openrouter",
		DisplayName:     "OpenRouter",
		Capabilities:    CapGenerate | CapEmbed,
		DefaultModel:    "",
		DefaultBaseURL:  "https://openrouter.ai/api/v1",
		NeedsAPIKey:     true,
	},

	// B: xAI exposes an OpenAI-compat endpoint at api.x.ai/v1.
	// DefaultModel is empty per the simplification rule.
	{
		ID:              "xai",
		DisplayName:     "xAI",
		Capabilities:    CapGenerate,
		DefaultModel:    "",
		DefaultBaseURL:  "https://api.x.ai/v1",
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
//   - AWS Bedrock: requires AWS SigV4 signing. Out of scope.
//   - Azure OpenAI: protocol-compatible with OpenAI but
//     deployment-name routing differs. Operators reach Azure
//     via Custom + the Azure endpoint until a first-class
//     adapter lands.
//   - Google Vertex AI: different auth + protocol. Operators
//     reach Vertex via Custom + the Vertex endpoint.
//   - Voyage AI: different API shape.
//   - Hugging Face TEI: OpenAI-compatible `/v1/embeddings`;
//     reachable today via Custom + OpenAI-compatible protocol
//     + the TEI endpoint. Promoting to a first-class entry is
//     future work.
//
// Public wizard menus expose ONLY Custom + a protocol picker.
// All branded entries in the slice above remain for backwards-
// compatible runtime profile resolution by ID — see the file
// header comment for the layering.

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
// provider. Used internally by runtime code that needs a fallback
// list (e.g. `openRouterCatalogForView` when live `/api/v1/models`
// discovery is unavailable). Public wizard UX does NOT consult this
// map — the public menu path goes through Custom + freeform model
// entry, which keeps the substrate free of vendor rename churn.
//
// 2026-09-14 config-simplification: do not advertise branded model
// catalogues. MPM does not maintain model-string lists per vendor
// (claude-*, gpt-*, gemini-*, grok-*, mistral-*, command-*,
// OpenRouter Free Router, etc.). Operators type the current model
// ID at the Custom prompt; new releases do not have to track
// vendor rebrandings.
//
// All entries return nil (empty list) so callers fall through to
// their discovery paths or freeform prompts. The function signature
// is preserved for backwards compat with internal callers; new code
// should NOT add branded presets here.
var modelCatalogFor = func(providerID string) []string {
	switch providerID {
	case "ollama":
		// Live discovery via /api/tags is the canonical path.
		// Empty here is intentional — operator types or discovery
		// populates.
		return nil
	case "openai-compatible":
		return nil
	case "openrouter":
		// Live discovery (probeOpenRouter) is preferred. The
		//// inline empty return signals "discover when possible".
		return nil
	}
	return nil
}

// modelCatalogIsEmpty is a small helper for code that wants to
// express "no branded preset" semantically without nil-checks at
// every callsite. Returns true when the model catalog for the given
// provider ID is nil/empty (i.e. the registry has no branded
// presets for it). New code may use this to gate UI that would
// otherwise depend on branded presets.
func modelCatalogIsEmpty(providerID string) bool {
	return len(modelCatalogFor(providerID)) == 0
}

// modelSortKey returns the comparison key used for model
// ordering. Custom (manual) entry is ALWAYS first; remainder
// is sorted alphabetically, case-insensitive, with a
// deterministic tiebreak on the original string so equal
// namespacing cannot reorder.
func modelSortKey(name string) string {
	return strings.ToLower(name)
}
