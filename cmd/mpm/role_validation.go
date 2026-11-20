// cmd/mpm/role_validation.go — Canonical LLM-role validation
// boundary. Used by every public surface that can assign an LLM
// model (the interactive wizard, `mpm config profile set model`,
// `mpm config set model`). The boundary guarantees that
// embedding-only models cannot reach the LLM `Max tokens`
// prompt or be saved as LLM providers.
//
// 2026-09-14 release-pass: pre-fix, the wizard and `set model`
// path accepted any model string. Configuring an embedding
// model (e.g. `all-minilm`) as the LLM provider would silently
// produce a broken config — the wizard would happily ask for
// Max tokens and save a profile that fails at synthesis time.
// The fix routes both paths through this validator so the
// behaviour is consistent and the rejection is actionable.
//
// Design decisions:
//
//   - Capability-based primary detection. ProbeOllamaCapabilities
//     queries the runtime's /api/show or /api/tags endpoints and
//     parses the `capabilities` array. Positive evidence is
//     authoritative; absence of evidence is NOT evidence of
//     absence (we don't reject on uncertainty per the brief).
//
//   - Tiny fallback name list for cases where the Ollama probe
//     fails (model not installed, endpoint down, network unreachable).
//     The list covers common embedding-only models that have
//     stable naming. The fallback is secondary; if the probe
//     succeeds, the probe wins.
//
//   - Custom-provider flexibility preserved. For unknown
//     capabilities (probe failure + name not in fallback list),
//     the validator returns RoleUnknown → caller accepts.
//     Operators can still configure non-Ollama endpoints.
//
//   - The role-validation boundary lives in cmd/mpm (not in
//     internal/core) because it is presentation policy. The
//     underlying capability probe lives in internal/core because
//     it is reusable substrate data. This split keeps the policy
//     close to the wizard text and the data close to the runtime.

package main

import (
	"fmt"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// RoleDecision is the outcome of the LLM-role validator.
type RoleDecision int

const (
	// RoleValid: model is acceptable as an LLM provider.
	RoleValid RoleDecision = iota
	// RoleEmbeddingOnly: model is positively identified as
	// embedding-only. The wizard / set path MUST reject this
	// model for the LLM role and direct the operator at the
	// embedding configuration surface.
	RoleEmbeddingOnly
	// RoleUnknown: capability could not be determined. The
	// validator returns this when the Ollama probe fails AND
	// the model name does not match the fallback list. The
	// caller MUST accept (preserves custom-provider flexibility).
	RoleUnknown
)

// String renders a stable diagnostic label.
func (r RoleDecision) String() string {
	switch r {
	case RoleValid:
		return "valid"
	case RoleEmbeddingOnly:
		return "embedding-only"
	case RoleUnknown:
		return "unknown"
	default:
		return "?"
	}
}

// fallbackEmbeddingModelNames is a tiny secondary list of
// well-known embedding-only model names. The capability probe
// is the primary signal; this list is consulted only when the
// probe is unavailable (timeout, non-Ollama endpoint, model
// not installed on a remote server).
//
// Inclusion rule: model is widely deployed, has stable naming,
// and is documented as embedding-only on its official model
// card. Substring match is case-insensitive.
var fallbackEmbeddingModelNames = []string{
	"all-minilm",     // sentence-transformers / Ollama
	"nomic-embed",    // Ollama nomic-embed-text family
	"mxbai-embed",    // mixedbread-ai via Ollama
	"bge-embed",      // BAAI BGE family via Ollama
	"e5-",            // intfloat/e5 family
	"gte-",           // Alibaba GTE family
	"snowflake-arctic-embed",
	"text-embedding-", // OpenAI embedding family (e.g. text-embedding-3-small)
	"embed-",         // generic Ollama embed prefix
	"embedding",      // last-resort: explicit "embedding" in name
}

// ValidateLLMRole inspects provider / model / baseURL and
// returns the role decision for using this model as an LLM
// (synthesis / generative) provider.
//
// Inputs:
//
//   provider — "ollama" or "custom" (or any other value). The
//              probe is only attempted for ollama / custom
//              with an http(s) base URL.
//   model    — the model name (e.g. "all-minilm", "llama3").
//   baseURL  — provider endpoint URL. Empty is allowed; we
//              skip the probe and fall through to the fallback
//              name list.
//
// The probe is best-effort. Network errors, timeouts, and
// non-Ollama endpoints all return RoleUnknown (not an error).
// The wizard surfaces the result as either "model accepted" or
// "embedding-only model rejected — use embedding config".
func ValidateLLMRole(provider, model, baseURL string) RoleDecision {
	if strings.TrimSpace(model) == "" {
		return RoleValid // empty model is a wizard skip; not our call.
	}

	// Try the capability probe for ollama-style endpoints.
	if isOllamaLikeEndpoint(provider, baseURL) {
		caps, _ := mpminternal.ProbeOllamaCapabilities(baseURL, model)
		if caps.FromAPI {
			if caps.IsEmbeddingOnly() {
				return RoleEmbeddingOnly
			}
			if caps.CanComplete {
				return RoleValid
			}
			// API explicitly says model has no completion.
			// Treat as embedding-only (rejection on evidence).
			return RoleEmbeddingOnly
		}
		// Probe failed — fall through to the name list.
	}

	// Fallback: substring match against well-known embedding names.
	lower := strings.ToLower(model)
	for _, fragment := range fallbackEmbeddingModelNames {
		if strings.Contains(lower, fragment) {
			return RoleEmbeddingOnly
		}
	}
	return RoleUnknown
}

// isOllamaLikeEndpoint reports whether the provider/URL
// combination is worth probing. We probe for "ollama" or any
// custom URL pointing at an http(s) endpoint (covers local
// Ollama-compatible servers on non-standard ports). We skip
// well-known remote providers where the probe would never
// succeed (anthropic, openai, custom with no URL).
func isOllamaLikeEndpoint(provider, baseURL string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "ollama" {
		return true
	}
	// Custom with an http(s) base URL — likely Ollama-compatible
	// local server. The probe will fail cleanly for non-Ollama
	// endpoints and we fall back to the name list.
	if p == "custom" || p == "" {
		u := strings.TrimSpace(baseURL)
		return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
	}
	return false
}

// RejectEmbeddingOnlyLLM is the user-facing message rendered
// when ValidateLLMRole returns RoleEmbeddingOnly. The wording
// follows the canonical CLI grammar:
//
//   "X" is an embedding model, not a generative LLM.
//   LLM provider    → synthesis / generative features
//   Embedding model → semantic / vector retrieval
//   Use:
//     mpm config profile add <name> --model <id> --base-url <url>
//     mpm config profile set <name> provider custom
//     mpm config component set embedding <name>
//
// The model argument is the rejected model name (verbatim).
// Returned as a single multi-line string so callers can print
// it directly. Kept in this file (not the wizard) so non-
// interactive paths (`mpm config profile set model`) can reuse
// the same wording.
//
// 2026-09-14 final-simplification: the previous hint referenced
// `mpm config detect-embedding`. That subcommand was removed in
// the same pass; manual Custom + protocol configuration is the
// canonical replacement.
func RejectEmbeddingOnlyLLM(model string) string {
	return fmt.Sprintf(
		"%q is an embedding model, not a generative LLM.\n\n"+
			"  LLM provider    → synthesis / generative features\n"+
			"  Embedding model → semantic / vector retrieval\n\n"+
			"Use:\n"+
			"  mpm config profile add <name> --model <id> --base-url <url>\n"+
			"  mpm config profile set <name> provider custom\n"+
			"  mpm config component set embedding <name>",
		model,
	)
}
