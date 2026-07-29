// Package orchestration implements cross-component coordination
// primitives built on top of the execution-profile abstraction.
//
// Architectural intent (Wed 2026-07-29 design session):
//
//   This is the first orchestration primitive that uses Config.ProfileFor
//   + Config.CapabilityFor + Config.ComponentFor as the routing layer.
//   It must remain provider-agnostic, component-oriented, and reusable
//   by MCP tools, CLI commands, future HTTP APIs, and autonomous
//   workflows. Any caller that wants concurrent cross-component LLM
//   work composes ReviewCoordinator — not the substrate directly.
//
// Non-goals for v0.1:
//
//   - No consensus synthesis or automatic ranking of reviews.
//     The returned []ReviewResult is independent — each component
//     produces its own verdict. Consensus is the caller's job.
//   - No review persistence (saving to the database).
//     The coordinator's output is in-memory; persistence is a
//     downstream caller concern.
//   - No retry logic or recursive review requests.
//     Failures surface in ReviewResult.Error. Callers may retry
//     the whole Execute call, but the coordinator itself is
//     single-shot.
//   - No streaming responses.
//     Responses are collected as full strings before being returned.
//     Streaming is a future-RFC feature.
package orchestration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// ReviewStrategy is how a review is dispatched across components.
// v0.1 only ships StrategyParallel. Future RFCs may add sequential
// or staged strategies (e.g. parallel-with-pipeline).
type ReviewStrategy string

const (
	// StrategyParallel dispatches all component reviews concurrently.
	// Each component gets its own goroutine. The Execute function
	// returns once all goroutines complete (or the request timeout
	// fires, whichever comes first).
	StrategyParallel ReviewStrategy = "parallel"
)

// ReviewRequest is what a caller hands the coordinator.
//
//   Components:  substrate component names (memory, critic, scheduler,
//               or future components). Each is resolved to a profile
//               via Config.ProfileFor(component). At least one
//               component is required.
//   Prompt:      the user-supplied instruction. Required. Sends to
//               every component.
//   ContextData:  pre-resolved artifact text (NOT ids). The MCP
//                handler fetches artifact bodies from the database and
//                passes the resolved text here. The coordinator itself
//                never sees IDs — that boundary is the abstraction
//                the orchestration layer enforces.
//   Strategy:    how to dispatch. v0.1 only supports StrategyParallel.
//   Timeout:     per-request cap on total execution time. If zero,
//                no timeout is applied (caller's context governs).
type ReviewRequest struct {
	Components  []string
	Prompt      string
	ContextData string
	Strategy    ReviewStrategy
	Timeout     time.Duration
}

// ReviewResult is one component's contribution to a review.
//
//   Component:    the substrate function that produced this result.
//   Profile:      the profile name that fulfilled it ("" on error).
//   Provider:     the provider name from the profile ("" on error).
//   Model:        the model name from the profile ("" on error).
//   DurationMS:   wall-clock time the request took. Set even on
//                 error so the caller can see which component was slow.
//   Response:     the LLM's text response. Empty when Error is set.
//   Error:        failure description (timeout, client error, etc.).
//                 Empty on success. The reviewer is a "soft" component;
//                 an Error in one component doesn't invalidate the
//                 other components' results — the caller sees both.
type ReviewResult struct {
	Component  string
	Profile    string
	Provider   string
	Model      string
	DurationMS int
	Response   string
	Error      string
}

// ReviewCoordinator is the contract the substrate exposes for
// concurrent multi-component LLM work. Implementations must:
//
//   1. Resolve each component to a profile via Config.ProfileFor.
//   2. Fan out concurrent requests, one per component.
//   3. Honour the request Timeout (and the caller's context).
//   4. Return independent results — no global failure, no shared
//      state across components beyond what the caller supplied.
//
// The interface lets tests substitute a fake coordinator without
// standing up an LLM, and lets future RFCs add strategies
// (sequential, staged) without touching call sites.
type ReviewCoordinator interface {
	Execute(ctx context.Context, req ReviewRequest) ([]ReviewResult, error)
}

// ModelClient is the per-component question. Implementations are
// constructed by a ModelFactory and must support context-aware
// single-prompt queries (return the model's text response).
//
// Production ModelClient is built by DefaultModelFactory wrapping
// *synth.SynthClient from internal/core/synth. Tests inject mock
// implementations that return canned strings.
type ModelClient interface {
	Query(ctx context.Context, prompt string) (string, error)
}

// ModelFactory is the dependency injection point. ReviewCoordinator
// calls factory(profile) once per component in the request.
//
// factory returns a *ModelClient (or an error if the profile is
// not actionable — e.g. missing api_key for a non-Ollama provider).
type ModelFactory func(profile config.Profile) (ModelClient, error)

// DefaultReviewCoordinator is the production implementation
// of ReviewCoordinator. Concurrent fan-out via sync.WaitGroup;
// per-request timeout enforced via context.WithTimeout.
type DefaultReviewCoordinator struct {
	cfg     *config.Config
	factory ModelFactory
}

// NewDefaultReviewCoordinator builds a DefaultReviewCoordinator
// over the given Config and ModelFactory. factory may be nil —
// in that case DefaultModelFactory is used.
func NewDefaultReviewCoordinator(cfg *config.Config, factory ModelFactory) *DefaultReviewCoordinator {
	if factory == nil {
		factory = DefaultModelFactory()
	}
	return &DefaultReviewCoordinator{cfg: cfg, factory: factory}
}

// Execute fans out the request across components concurrently.
//
// Concurrency model:
//
//   - One goroutine per component (sync.WaitGroup).
//   - Each goroutine writes its result into a pre-allocated slot
//     in the results slice (indexed by the original component
//     order, so the output preserves the caller's request order).
//   - The per-request Timeout, when non-zero, is applied to the
//     derived context. Individual goroutines inherit the cancel.
//
// Return value: independent []ReviewResult. Errors per component
// surface inside the result, not as a global failure — one component
// timing out does NOT abort the others. The second return value is
// reserved for genuine infrastructure errors (e.g. invalid request).
func (c *DefaultReviewCoordinator) Execute(ctx context.Context, req ReviewRequest) ([]ReviewResult, error) {
	if req.Strategy != "" && req.Strategy != StrategyParallel {
		return nil, fmt.Errorf("orchestration: only strategy=%q is supported in v0.1 (got %q)", StrategyParallel, req.Strategy)
	}
	if len(req.Components) == 0 {
		return nil, fmt.Errorf("orchestration: at least one component is required")
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("orchestration: prompt is required")
	}

	// Apply per-request timeout if specified. The caller-supplied
	// context remains the parent — cancellation propagates upward.
	effectiveCtx := ctx
	var cancel context.CancelFunc = func() {}
	if req.Timeout > 0 {
		effectiveCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	results := make([]ReviewResult, len(req.Components))
	var wg sync.WaitGroup
	for i, comp := range req.Components {
		i, comp := i, comp // closure capture
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.executeOne(effectiveCtx, req, comp)
		}()
	}
	wg.Wait()
	return results, nil
}

// executeOne is the per-component body. Sets Component/Profile/
// Provider/Model/DurationMS even on error so the caller can
// debug which component was slow or broken.
func (c *DefaultReviewCoordinator) executeOne(ctx context.Context, req ReviewRequest, component string) ReviewResult {
	start := time.Now()
	res := ReviewResult{Component: component}

	profile := c.cfg.ProfileFor(component)
	if profile == nil {
		res.Error = fmt.Sprintf("no profile resolves to component %q (run `mpm config component set %q <profile>`)", component, component)
		res.DurationMS = int(time.Since(start).Milliseconds())
		return res
	}
	res.Profile = profile.Name
	res.Provider = profile.Provider
	res.Model = profile.Model

	client, err := c.factory(*profile)
	if err != nil {
		res.Error = fmt.Sprintf("client build failed: %v", err)
		res.DurationMS = int(time.Since(start).Milliseconds())
		return res
	}

	prompt := buildReviewPrompt(req.Prompt, req.ContextData, component)
	response, err := client.Query(ctx, prompt)
	res.DurationMS = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Response = response
	return res
}

// buildReviewPrompt composes the prompt each component sees.
//
// Layout:
//   - First line tags which component is answering
//     (helps the model self-identify, useful when responses
//     are aggregated downstream).
//   - Then the pre-resolved artifact text (ContextData),
//     wrapped in a "## Artifact" section if present.
//   - Then the user's request (Prompt), wrapped in "## Prompt".
//
// The component name is informational only — it doesn't influence
// the model's response. The model sees the same prompt for every
// component; the difference between components is *which model* and
// *which provider* answers.
func buildReviewPrompt(prompt, contextData, component string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Reviewer component: %s]\n\n", component)
	if contextData != "" {
		b.WriteString("## Artifact\n\n")
		b.WriteString(contextData)
		b.WriteString("\n\n")
	}
	b.WriteString("## Prompt\n\n")
	b.WriteString(prompt)
	return b.String()
}
