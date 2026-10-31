// credentials.go — canonical credential resolution for generative providers.
//
// Every generative caller in the substrate resolves its credential
// through ResolveAPIKey, so one rule governs all of them: explicit
// configuration wins, the provider's own environment variable is the
// fallback. Before this file existed the rule lived inline in
// NewSynthClient only, and the orchestration model factory and the
// generative probe each re-derived (incorrectly) that no environment
// credential was available to them. That produced a split substrate:
// `mpm synthesize` authenticated with an environment-only credential
// while `mpm_request_review` refused to start on the same profile.
//
// Provider isolation (2026-09-14 release-pass): each provider reads
// ONLY its own environment variables. There is no cross-provider
// fallthrough, so MINIMAX_API_KEY can never satisfy an OpenAI profile
// and OPENAI_API_KEY can never satisfy a MiniMax profile. A generic
// OpenAI-compatible endpoint therefore requires OAI_COMPAT_API_KEY or
// an explicit profile api_key — never a borrowed secret.

package synth

import (
	"strings"
)

// ResolveAPIKey returns the credential to present to the endpoint at
// baseURL, or "" when no source supplies one.
//
// Precedence:
//
//  1. configuredKey, when non-empty. A profile carrying `api_key` in
//     mpm_config.json is authoritative and the environment is never
//     consulted. This preserves every install that stored a key on
//     disk and keeps those configs working unchanged.
//
//     ONE EXCEPTION: a configured value shaped like an environment
//     reference ("$NAME" / "${NAME}") is resolved AS a reference — see
//     credential_reference.go. A literal placeholder such as
//     "$MINIMAX_API_KEY" used to be sent to the provider verbatim,
//     shadowing the very environment credential it named. A value that
//     is not reference-shaped is still a literal secret.
//
//  2. Otherwise, the provider-specific environment variable for
//     baseURL, tried in the order APIKeyEnvVars returns.
//
// Callers decide what an empty result means: memory synthesis treats
// a missing credential as an acceptable degradation (the substrate
// explicitly does not fail a memory save over an unconfigured LLM),
// while the orchestration model factory and the probe treat it as a
// hard error. The resolver itself never guesses.
//
// This function discards the REASON a credential could not be resolved.
// A caller that needs to tell the operator WHY — a reference to a
// variable that is not set, a reference to a variable this endpoint
// does not declare — must call ResolveCredential instead.
//
// The return value is the secret. Callers must not log it, format it
// into an error message, or persist it anywhere.
func ResolveAPIKey(configuredKey, baseURL string) string {
	key, _ := ResolveCredential(configuredKey, baseURL)
	return key
}

// APIKeyEnvVars returns the environment variables that may supply a
// credential for baseURL, in priority order. The mapping is keyed off
// the canonical base URL's wire shape and domain substring, the same
// heuristic the wire dispatcher uses — the substrate stores no provider
// enum, so the URL is the operative signal.
//
// Exported so error messages can name the exact variable an operator
// should export, instead of saying only "no api_key".
func APIKeyEnvVars(baseURL string) []string {
	lu := strings.ToLower(baseURL)
	switch inferWire(baseURL) {
	case wireOpenAI:
		switch {
		case strings.Contains(lu, "openrouter"):
			return []string{"OPENROUTER_API_KEY"}
		case strings.Contains(lu, "googleapis"):
			return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
		case strings.Contains(lu, "x.ai"):
			return []string{"XAI_API_KEY"}
		case strings.Contains(lu, "mistral.ai"):
			return []string{"MISTRAL_API_KEY"}
		case strings.Contains(lu, "cohere.com"):
			return []string{"COHERE_API_KEY"}
		case strings.Contains(lu, "openai.com"),
			strings.Contains(lu, "/openai/"),
			strings.HasSuffix(lu, "/openai"):
			return []string{"OPENAI_API_KEY"}
		default:
			// Generic OpenAI-compatible endpoints (LM Studio, LocalAI,
			// vLLM, ...): require the dedicated OAI_COMPAT_API_KEY.
			// OPENAI_API_KEY and OPENROUTER_API_KEY are deliberately
			// not listed here.
			return []string{"OAI_COMPAT_API_KEY"}
		}
	default:
		// Anthropic-protocol endpoints. MiniMax is the substrate's
		// first-class Anthropic-protocol provider; ANTHROPIC_API_KEY
		// stays as a secondary so Anthropic-protocol proxies keep
		// working without a second code path.
		return []string{"MINIMAX_API_KEY", "ANTHROPIC_API_KEY"}
	}
}

// PrimaryAPIKeyEnvVar returns the first environment variable that can
// supply a credential for baseURL — the one an operator should export.
// Returns "" when no variable covers the endpoint.
func PrimaryAPIKeyEnvVar(baseURL string) string {
	if names := APIKeyEnvVars(baseURL); len(names) > 0 {
		return names[0]
	}
	return ""
}