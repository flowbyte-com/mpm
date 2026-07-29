package orchestration_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/orchestration"
)

// mockModelClient is a fake ModelClient for tests. It records every
// Query call so tests can assert on prompt shape, concurrency, and
// per-component dispatch order, and returns a canned response or
// canned error after an optional delay.
type mockModelClient struct {
	mu         sync.Mutex
	calls      []string // recorded query strings
	returns   string
	returnsErr error
	delay     time.Duration

	inflightN atomic.Int32 // tracks max concurrent calls for ordering tests
	peakInflight atomic.Int32
}

// Query implements ModelClient. The mock records the prompt, bumps
// in-flight count, simulates work via delay, then returns the
// canned values.
func (m *mockModelClient) Query(ctx context.Context, prompt string) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, prompt)
	m.mu.Unlock()

	cur := m.inflightN.Add(1)
	defer m.inflightN.Add(-1)

	// Track peak concurrent queries for assertions.
	for {
		peak := m.peakInflight.Load()
		if cur <= peak || m.peakInflight.CompareAndSwap(peak, cur) {
			break
		}
	}

	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return m.returns, m.returnsErr
}

// mockFactory returns a ModelFactory that distributes per-component
// mock clients. Tests construct one mock per component (each can
// have its own canned response / delay / error) and the factory
// dispatches based on profile.Name.
func mockFactory(clients map[string]*mockModelClient) orchestration.ModelFactory {
	return func(profile config.Profile) (orchestration.ModelClient, error) {
		mc, ok := clients[profile.Name]
		if !ok {
			return nil, errors.New("mock: no client for profile " + profile.Name)
		}
		return mc, nil
	}
}

// failureFactory always returns an error — used to verify error-
// path surfacing in ReviewResult.Error.
func failureFactory(msg string) orchestration.ModelFactory {
	return func(profile config.Profile) (orchestration.ModelClient, error) {
		return nil, errors.New(msg)
	}
}

// --- Tests ---------------------------------------------------------------

// TestExecute_Concurrent verifies that fan-out is genuinely
// concurrent. Three components, each with a 50ms delay. Wall-clock
// should be ~50ms, not 150ms. Tolerates 4x slack for slow CI.
func TestExecute_Concurrent(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"a": {Name: "a", Provider: "test", Model: "ma"},
			"b": {Name: "b", Provider: "test", Model: "mb"},
			"c": {Name: "c", Provider: "test", Model: "mc"},
		},
		Components: map[string]string{
			"compA": "a",
			"compB": "b",
			"compC": "c",
		},
	}
	clients := map[string]*mockModelClient{
		"a": {returns: "result-A", delay: 50 * time.Millisecond},
		"b": {returns: "result-B", delay: 50 * time.Millisecond},
		"c": {returns: "result-C", delay: 50 * time.Millisecond},
	}
	coord := orchestration.NewDefaultReviewCoordinator(cfg, mockFactory(clients))

	req := orchestration.ReviewRequest{
		Components: []string{"compA", "compB", "compC"},
		Prompt:     "test",
		Strategy:   orchestration.StrategyParallel,
	}

	start := time.Now()
	results, err := coord.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	elapsed := time.Since(start)

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	// If this took > 200ms, fan-out is sequential. ~50-150ms is
	// the expected range; we accept up to 4x for slow CI.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Execute took %v — components ran sequentially (should be ~50ms with concurrent fan-out)", elapsed)
	}

	// Each mock should have received exactly one call.
	for name, mc := range clients {
		mc.mu.Lock()
		got := len(mc.calls)
		mc.mu.Unlock()
		if got != 1 {
			t.Errorf("%s: got %d calls, want 1", name, got)
		}
	}

	// Components must be independent: each result maps to its
	// component, response carries through.
	want := map[string]string{
		"compA": "result-A",
		"compB": "result-B",
		"compC": "result-C",
	}
	for _, r := range results {
		if r.Response != want[r.Component] {
			t.Errorf("%s: got response %q, want %q", r.Component, r.Response, want[r.Component])
		}
		if r.Error != "" {
			t.Errorf("%s: unexpected error %q", r.Component, r.Error)
		}
	}
}

// TestExecute_Independence verifies that one component's failure
// does NOT abort the others — the design non-goal is "independent
// review results only".
func TestExecute_Independence(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"a": {Name: "a", Provider: "test", Model: "ma"},
			"b": {Name: "b", Provider: "test", Model: "mb"},
		},
		Components: map[string]string{
			"compA": "a",
			"compB": "b",
		},
	}
	clients := map[string]*mockModelClient{
		"a": {returnsErr: errors.New("component-a failed")},
		"b": {returns: "success-b"},
	}
	coord := orchestration.NewDefaultReviewCoordinator(cfg, mockFactory(clients))

	req := orchestration.ReviewRequest{
		Components: []string{"compA", "compB"},
		Prompt:     "p",
		Strategy:   orchestration.StrategyParallel,
	}
	results, err := coord.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v (one component failing must not abort the whole review)", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	byComp := map[string]orchestration.ReviewResult{}
	for _, r := range results {
		byComp[r.Component] = r
	}
	if byComp["compA"].Error == "" {
		t.Error("compA: expected non-empty Error after client failure")
	}
	if byComp["compA"].Response != "" {
		t.Errorf("compA: expected empty Response on error, got %q", byComp["compA"].Response)
	}
	if byComp["compB"].Error != "" {
		t.Errorf("compB: unexpected error %q", byComp["compB"].Error)
	}
	if byComp["compB"].Response != "success-b" {
		t.Errorf("compB: got %q, want success-b", byComp["compB"].Response)
	}
}

// TestExecute_MissingProfile verifies the failure-mode surface:
// when a component is bound to a profile that doesn't exist
// (and there's no 'default' fallback), the coordinator records a
// per-component error and the others proceed.
func TestExecute_MissingProfile(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"present": {Name: "present", Provider: "test", Model: "mp"},
		},
		Components: map[string]string{
			"compMissing": "absent",     // binds to a missing profile
			"compPresent": "present",
		},
	}
	clients := map[string]*mockModelClient{
		"present": {returns: "ok"},
	}
	coord := orchestration.NewDefaultReviewCoordinator(cfg, mockFactory(clients))

	req := orchestration.ReviewRequest{
		Components: []string{"compMissing", "compPresent"},
		Prompt:     "p",
		Strategy:   orchestration.StrategyParallel,
	}
	results, err := coord.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	byComp := map[string]orchestration.ReviewResult{}
	for _, r := range results {
		byComp[r.Component] = r
	}
	if !strings.Contains(byComp["compMissing"].Error, "no profile resolves") {
		t.Errorf("compMissing: expected 'no profile resolves' error, got %q", byComp["compMissing"].Error)
	}
	if byComp["compPresent"].Error != "" || byComp["compPresent"].Response != "ok" {
		t.Errorf("compPresent: unexpected result %+v", byComp["compPresent"])
	}
}

// TestExecute_Timeout verifies that a per-request Timeout cuts
// off individual queries that take too long. Other queries that
// finish quickly still return; long queries get ctx.Err().
func TestExecute_Timeout(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"a": {Name: "a", Provider: "test", Model: "ma"},
			"b": {Name: "b", Provider: "test", Model: "mb"},
		},
		Components: map[string]string{
			"slow": "a",
			"fast": "b",
		},
	}
	clients := map[string]*mockModelClient{
		"a": {returns: "slow-result", delay: 500 * time.Millisecond},
		"b": {returns: "fast-result", delay: 5 * time.Millisecond},
	}
	coord := orchestration.NewDefaultReviewCoordinator(cfg, mockFactory(clients))

	req := orchestration.ReviewRequest{
		Components: []string{"slow", "fast"},
		Prompt:     "p",
		Strategy:   orchestration.StrategyParallel,
		Timeout:    100 * time.Millisecond,
	}
	results, err := coord.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	byComp := map[string]orchestration.ReviewResult{}
	for _, r := range results {
		byComp[r.Component] = r
	}
	if !strings.Contains(byComp["slow"].Error, "deadline") && !strings.Contains(byComp["slow"].Error, "timeout") {
		t.Errorf("slow: expected timeout error, got %q", byComp["slow"].Error)
	}
	if byComp["fast"].Error != "" || byComp["fast"].Response != "fast-result" {
		t.Errorf("fast: unexpected result %+v", byComp["fast"])
	}
}

// TestExecute_PromptShape verifies the prompt the coordinator
// builds contains the expected headers + body. The component
// name and ContextData must both be visible to the model.
func TestExecute_PromptShape(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"a": {Name: "a", Provider: "test", Model: "ma"},
		},
		Components: map[string]string{"c": "a"},
	}
	clients := map[string]*mockModelClient{
		"a": {returns: "ok"},
	}
	coord := orchestration.NewDefaultReviewCoordinator(cfg, mockFactory(clients))

	req := orchestration.ReviewRequest{
		Components:  []string{"c"},
		Prompt:      "PLEASE-CRITIQUE-THIS",
		ContextData: "## Preamble\nthis is the artifact text",
		Strategy:    orchestration.StrategyParallel,
	}
	if _, err := coord.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := clients["a"].calls[0]
	for _, want := range []string{
		"Reviewer component: c",
		"## Artifact",
		"this is the artifact text",
		"## Prompt",
		"PLEASE-CRITIQUE-THIS",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q; got:\n%s", want, got)
		}
	}
}

// TestExecute_EmptyRequest verifies the input-validation surface:
// at least one component is required; an empty prompt is an error.
func TestExecute_EmptyRequest(t *testing.T) {
	cfg := &config.Config{}
	coord := orchestration.NewDefaultReviewCoordinator(cfg, mockFactory(nil))

	if _, err := coord.Execute(context.Background(), orchestration.ReviewRequest{
		Prompt:   "p",
		Strategy: orchestration.StrategyParallel,
	}); err == nil {
		t.Error("expected error for empty components, got nil")
	}

	if _, err := coord.Execute(context.Background(), orchestration.ReviewRequest{
		Components: []string{"c"},
		Strategy:   orchestration.StrategyParallel,
	}); err == nil {
		t.Error("expected error for empty prompt, got nil")
	}
}

// TestExecute_UnsupportedStrategy verifies the v0.1 surface: only
// StrategyParallel is supported; future strategies return an
// error early (rather than falling through to parallel).
func TestExecute_UnsupportedStrategy(t *testing.T) {
	cfg := &config.Config{}
	coord := orchestration.NewDefaultReviewCoordinator(cfg, mockFactory(nil))
	_, err := coord.Execute(context.Background(), orchestration.ReviewRequest{
		Components: []string{"c"},
		Prompt:     "p",
		Strategy:   "sequential",
	})
	if err == nil {
		t.Error("expected error for unsupported strategy, got nil")
	}
}

// TestExecute_FactoryFailure verifies that a ModelFactory error
// (e.g. missing api_key for a non-Ollama profile) surfaces as a
// per-component ReviewResult.Error, not a global failure.
func TestExecute_FactoryFailure(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"default": {Name: "default", Provider: "openai", Model: "gpt-x"},
		},
		Components: map[string]string{"c": "default"},
	}
	coord := orchestration.NewDefaultReviewCoordinator(cfg,
		failureFactory("missing api_key"))

	results, err := coord.Execute(context.Background(), orchestration.ReviewRequest{
		Components: []string{"c"},
		Prompt:     "p",
		Strategy:   orchestration.StrategyParallel,
	})
	if err != nil {
		t.Fatalf("Execute: %v (factory failure should be per-component, not global)", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !strings.Contains(results[0].Error, "missing api_key") {
		t.Errorf("expected factory error to surface, got %q", results[0].Error)
	}
}
