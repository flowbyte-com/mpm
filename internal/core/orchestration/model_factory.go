// model_factory.go — production ModelFactory implementation.
//
// ReviewCoordinator takes a ModelFactory and calls it once per
// component. The production ModelFactory builds a thin adapter over
// *synth.SynthClient — the substrate's existing LLM HTTP client.
//
// Why an adapter? *synth.SynthClient exposes Synthesize(fragments)
// (multi-fragment). The orchestrator needs a single-prompt Query.
// They are the same operation under the hood, just with different
// framing. The adapter maps one to the other without requiring
// every orchestrator caller to think about fragment semantics.

package orchestration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/synth"
)

// DefaultModelFactory returns a ModelFactory that wraps the
// substrate's *synth.SynthClient for each profile. The SynthClient
// is configured from the profile fields (provider / model /
// api_key / base_url / timeout_seconds).
//
// max_tokens is deliberately NOT one of them: the output-token budget
// is supplied by the substrate (synth.InternalMaxTokens), per the
// contract documented on synth.NewSynthClient. config.Profile still
// carries the field so existing on-disk configs keep parsing, but the
// runtime does not honour it here.
//
// Per-profile client construction is intentionally NOT cached.
// Each ModelFactory call rebuilds — profiles can change between
// requests (operator runs 'mpm config profile set ...'), and a
// stale cache would mask that. The cost of client construction is
// negligible (struct literal), so caching would optimise for
// nothing.
//
// When fields the underlying SynthClient can't honour are present
// (Temperature*, Reasoning), they are recorded for future use but
// not yet threaded through — the substrate's HTTP layer currently
// lacks temperature wiring. This is a forward-compatibility hook.
func DefaultModelFactory() ModelFactory {
	return func(profile config.Profile) (ModelClient, error) {
		// Credential resolution is the substrate's single rule: an
		// explicit profile api_key wins, otherwise the provider's own
		// environment variable is consulted (synth.ResolveCredential).
		// Resolving here rather than reading profile.APIKey directly is
		// what lets an environment-only credential drive review and
		// critic requests, the same way it already drives synthesis.
		//
		// A configured api_key that names an environment variable is
		// resolved AS one, and a reference that cannot be resolved is
		// reported as such — with the reason — rather than being sent
		// to the provider as a literal placeholder and refused as an
		// invalid credential.
		apiKey, credentialErr := synth.ResolveCredential(profile.APIKey, profile.BaseURL)
		if credentialErr != nil {
			return nil, fmt.Errorf("model factory: profile %q: %w", profile.Name, credentialErr)
		}

		// Provider-side validation. Ollama + other local servers run
		// without auth; everything else needs a credential from one of
		// the two sources above. The error names the variable to export
		// so an environment-only operator is not sent to edit the config.
		if !profileLocallyHosted(profile) && apiKey == "" {
			return nil, fmt.Errorf(
				"model factory: profile %q has no credential for provider %q (not locally hosted): "+
					"set api_key on the profile or export %s",
				profile.Name, profile.Provider, synth.PrimaryAPIKeyEnvVar(profile.BaseURL))
		}
		timeout := time.Duration(config.EffectiveTimeoutSeconds(profile.TimeoutSecs)) * time.Second
		// The client is built through the substrate's single
		// construction point so the protocol, the credential and the
		// authentication scheme are resolved exactly as they are for
		// synthesis and for the Doctor probe.
		//
		// The previous struct literal here left SynthClient.Wire at its
		// zero value, so the factory ALWAYS spoke the Anthropic
		// protocol regardless of the profile's endpoint, and it had no
		// way to present a bearer token to an OpenAI-compatible
		// endpoint. That divergence is what let the Doctor probe and
		// the review coordinator disagree with synthesis about the same
		// profile.
		//
		// Output tokens are NOT user-configurable. The substrate owns
		// the budget (synth/client.go documents the contract); this
		// factory previously copied profile.MaxTokens into the literal,
		// which put the wire cap in the operator's hands. A profile with
		// max_tokens=10 capped every review response at ten tokens, and
		// a profile that simply omitted the field put `max_tokens: 0` on
		// the wire. Sourced from the synth package so the ceiling has
		// exactly one definition.
		sc := synth.NewClientForProfile(profile, timeout, synth.InternalMaxTokens())
		return &synthClientAdapter{sc: sc}, nil
	}
}

// profileLocallyHosted heuristically identifies a provider that
// doesn't require an api_key (Ollama, LM Studio, OpenAI-compatible
// local servers, etc.). Substrate doesn't store a provider enum;
// the URL shape is the operative signal — same heuristic the
// legacy Synth fallback uses (inferProviderFromURL).
func profileLocallyHosted(p config.Profile) bool {
	u := strings.ToLower(p.BaseURL)
	return strings.Contains(u, "ollama") ||
		strings.Contains(u, "11434") ||
		strings.Contains(u, "lmstudio") ||
		strings.Contains(u, "localhost") ||
		strings.Contains(u, "127.0.0.1")
}

// synthClientAdapter wraps *synth.SynthClient to satisfy the
// ModelClient interface. Single-prompt Query maps to the multi-
// fragment Synthesize ([]string{prompt}); the substrate's synthesis
// worker handles the join internally.
type synthClientAdapter struct {
	sc *synth.SynthClient
}

// Query implements ModelClient.
func (a *synthClientAdapter) Query(ctx context.Context, prompt string) (string, error) {
	res, err := a.sc.SynthesizeWithPlan(ctx, []string{prompt}, synth.NewPerCallPlan())
	if err != nil {
		return "", fmt.Errorf("orchestration: %w", err)
	}
	if res == nil || res.Content == "" {
		return "", fmt.Errorf("orchestration: empty synthesis response")
	}
	return res.Content, nil
}
