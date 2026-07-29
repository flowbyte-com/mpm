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
// api_key / base_url / max_tokens / timeout_seconds).
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
		// Provider-side validation. Ollama + other local servers run
		// without auth; everything else needs an api_key.
		if !profileLocallyHosted(profile) && profile.APIKey == "" {
			return nil, fmt.Errorf("model factory: profile %q has no api_key and provider %q is not local",
				profile.Name, profile.Provider)
		}
		timeout := time.Duration(profile.TimeoutSecs) * time.Second
		if timeout <= 0 {
			timeout = 300 * time.Second // substrate default
		}
		sc := &synth.SynthClient{
			Model:     profile.Model,
			APIKey:    profile.APIKey,
			BaseURL:   profile.BaseURL,
			MaxTokens: profile.MaxTokens,
			Timeout:   timeout,
		}
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
	res, err := a.sc.Synthesize(ctx, []string{prompt})
	if err != nil {
		return "", fmt.Errorf("orchestration: %w", err)
	}
	if res == nil || res.Content == "" {
		return "", fmt.Errorf("orchestration: empty synthesis response")
	}
	return res.Content, nil
}
