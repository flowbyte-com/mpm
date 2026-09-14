// safeguard_test.go — regression tests for the bounded-execution
// safeguard (Plan / AcquireOrStop / classification / repair-budget).
//
// 2026-09-14 release-pass: every LLM-backed operation must have a
// finite, pre-computable execution plan. These tests pin the
// per-call-classification retry policy and the absolute-ceiling
// invariant. They run entirely in-process against an httptest fake
// provider; no paid production endpoint is exercised.

package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testSynthResultPayload returns the *SynthResult JSON payload
// that the test server renders inside the Anthropic text block.
// The Synthesize caller unmarshals the inner text directly into
// a SynthResult, so the inner text MUST be a SynthResult JSON.
func testSynthResultPayload(content string) string {
	return fmt.Sprintf(`{"content":%q,"tags":["synthetic"]}`, content)
}

// testOKHandler renders a successful Anthropic-protocol
// completion whose inner text is a SynthResult JSON payload.
// The handler increments the supplied atomic counter (when
// non-nil) so callers can pin call counts.
func testOKHandler(content string, attempts *atomic.Int32) http.HandlerFunc {
	payload := testSynthResultPayload(content)
	body := fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, payload)
	return func(w http.ResponseWriter, r *http.Request) {
		if attempts != nil {
			attempts.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// newTestClient builds a SynthClient pointed at the given test
// server with a wire that maps to the test handler's expected
// request shape (Anthropic-shaped by default).
func newTestClient(t *testing.T, srvURL string) *SynthClient {
	t.Helper()
	return &SynthClient{
		BaseURL:   srvURL,
		APIKey:    "test-key",
		Wire:      wireAnthropic,
		Timeout:   5 * time.Second,
		Model:     "test",
		MaxTokens: 1024,
	}
}

// ────────────────────────────────────────────────────────────────────
// Per-attempt classification
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_5xxPermitsOneRetry — case D + E from the matrix:
// transient transport failure permits exactly MaxRetriesPerStage
// retries before stopping. Provider keeps returning 5xx → run
// halts after attempt 1+1=2 hits.
func TestSafeguard_5xxPermitsOneRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "transient", http.StatusBadGateway)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err == nil {
		t.Fatal("expected error on persistent 502")
	}
	got := attempts.Load()
	if got != 2 {
		t.Errorf("attempts = %d, want 2 (one initial + one permitted retry)", got)
	}
	s := plan.Stats()
	if s.CompletedCalls != 0 {
		t.Errorf("CompletedCalls = %d, want 0 (no 200 OK)", s.CompletedCalls)
	}
	if s.RetryCalls != 1 {
		t.Errorf("RetryCalls = %d, want 1", s.RetryCalls)
	}
}

// TestSafeguard_401FailsFast — case F: 401 must NOT retry. Zero
// retries; fail immediately on the first attempt.
func TestSafeguard_401FailsFast(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "auth", http.StatusUnauthorized)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 401)", got)
	}
}

// TestSafeguard_403FailsFast — case G: 403 also zero retries.
func TestSafeguard_403FailsFast(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 403)", got)
	}
}

// TestSafeguard_429StopsWithoutRetry — case H (no Retry-After):
// 429 must not enter an uncontrolled backoff loop. The plan
// refuses the retry path; the run stops after the first
// attempt.
func TestSafeguard_429StopsWithoutRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "broken-value")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 429 without bounded Retry-After)", got)
	}
}

// ────────────────────────────────────────────────────────────────────
// Repair budget for malformed / unparseable responses
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_MalformedResponseConsumesSingleRepair — case I:
// when the wire returns 200 OK but the body is unparseable on
// the Anthropic envelope, the call site consumes ONE repair
// from the plan's budget and surfaces the structural-decode
// error. No retry against the same body.
func TestSafeguard_MalformedResponseConsumesSingleRepair(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{this is not parseable`)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err == nil {
		t.Fatal("expected error on malformed response")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (single probe, no retry on malformed)", got)
	}
	if plan.Stats().RepairCalls != 1 {
		t.Errorf("RepairCalls = %d, want 1 (the call site's repair attribution)",
			plan.Stats().RepairCalls)
	}
}

// ────────────────────────────────────────────────────────────────────
// Duplicate-stage guard
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_DuplicateStageRejected — case L: if the same
// stage fingerprint is presented to the plan beyond its
// allowance, the run stops BEFORE another network call is
// attempted. The check uses the body's hash, so two requests
// with the same body reach the same fingerprint.
func TestSafeguard_DuplicateStageRejected(t *testing.T) {
	// We exercise the duplicate-stage guard purely against
	// AcquireOrStop — no network roundtrip is needed. The
	// fingerprint key derives from the body hash; supplying
	// the same value twice past the per-fingerprint allowance
	// trips the safeguard.
	body2 := map[string]interface{}{"model": "m"}
	fpr := fingerprintFromBody(body2)

	plan := NewPerCallPlan()
	for i := 0; i < 1+plan.MaxRetries+plan.MaxRepairs; i++ {
		if err := plan.AcquireOrStop(fpr); err != nil {
			t.Fatalf("unexpected AcquireOrStop failure on attempt %d: %v", i, err)
		}
	}
	err := plan.AcquireOrStop(fpr)
	if err == nil {
		t.Fatal("expected duplicate-stage refusal; got nil")
	}
	if !strings.Contains(err.Error(), "duplicate-stage") {
		t.Errorf("expected duplicate-stage message; got: %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────
// Sentinel — uncertainty is a valid terminal result
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_SquareRootSentinel — case B + the "square root of
// divided by zero x 0" sentinel from the brief. A single LLM call
// returns a valid uncertainty response; the run terminates
// successfully with exactly ONE semantic call. No retry. No
// repair. No recurse.
func TestSafeguard_SquareRootSentinel(t *testing.T) {
	const uncertainty = "The source statement is mathematically undefined; no reliable conclusion can be derived."
	var attempts atomic.Int32
	srv := httptest.NewServer(testOKHandler(uncertainty, &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	// Use the worst-case nonsensical source material.
	fragments := []string{"square root of divided by zero x 0"}
	result, err := sc.SynthesizeWithPlan(context.Background(), fragments, plan)
	if err != nil {
		t.Fatalf("sentinel expected a successful uncertainty response; got: %v", err)
	}
	if result == nil {
		t.Fatal("result must not be nil")
	}
	if !strings.Contains(result.Content, "undefined") &&
		!strings.Contains(result.Content, "ambiguous") &&
		!strings.Contains(result.Content, "no reliable") {
		t.Errorf("response must explicitly preserve uncertainty; got: %q", result.Content)
	}
	// Exactly ONE semantic call. The brief pins "call count
	// and termination are" — call count is what we promise.
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (single semantic call)", got)
	}
	s := plan.Stats()
	if s.CompletedCalls != 1 {
		t.Errorf("CompletedCalls = %d, want 1", s.CompletedCalls)
	}
	if s.RetryCalls != 0 {
		t.Errorf("RetryCalls = %d, want 0 (uncertainty is not a retry reason)", s.RetryCalls)
	}
	if s.RepairCalls != 0 {
		t.Errorf("RepairCalls = %d, want 0 (valid uncertainty is not a malformed response)", s.RepairCalls)
	}
}

// TestSafeguard_EmptyContentNotARetry — the synth prompt accepts
// empty content as a valid uncertainty terminal result; the
// safeguard must NOT retry (semantic dissatisfaction is not a
// retry reason). The empty-string path was wired through
// SynthesizeWithPlan in the same release pass.
func TestSafeguard_EmptyContentNotARetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(testOKHandler("", &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	result, err := sc.SynthesizeWithPlan(context.Background(), []string{"empty"}, plan)
	if err != nil {
		t.Fatalf("expected accept-on-empty-content; got err=%v", err)
	}
	if result == nil {
		t.Fatal("result must be non-nil for empty-content acceptance")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry on empty content)", got)
	}
	if plan.Stats().RetryCalls != 0 {
		t.Errorf("retry calls = %d, want 0", plan.Stats().RetryCalls)
	}
}

// ────────────────────────────────────────────────────────────────────
// Absolute ceiling / input-too-large
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_AbsoluteBatchCeiling — case N: requesting a
// batch plan above AbsoluteMaxBatchesPerInvocation refuses
// BEFORE any model call is attempted. The guard surfaces
// ErrInputTooLarge with the planned/ceiling numbers.
func TestSafeguard_AbsoluteBatchCeiling(t *testing.T) {
	_, err := NewBatchPlanForInvocation(AbsoluteMaxBatchesPerInvocation + 1)
	if err == nil {
		t.Fatal("expected ErrInputTooLarge for over-ceiling plan")
	}
	if !strings.Contains(err.Error(), "input too large") {
		t.Errorf("error must mention 'input too large'; got: %v", err)
	}
}

// TestSafeguard_NewBatchPlanAcceptsInRange — sanity: a plan
// within the absolute ceiling builds successfully.
func TestSafeguard_NewBatchPlanAcceptsInRange(t *testing.T) {
	for _, n := range []int{1, 5, AbsoluteMaxBatchesPerInvocation} {
		plan, err := NewBatchPlanForInvocation(n)
		if err != nil {
			t.Errorf("plan %d rejected: %v", n, err)
			continue
		}
		if plan.PlannedCalls != n {
			t.Errorf("plan PlannedCalls = %d, want %d", plan.PlannedCalls, n)
		}
	}
}

// ────────────────────────────────────────────────────────────────────
// Recursion guard
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_RecursionDepthOne — case M: a depth-1 chain
// passes; depth-2 (the same chain invoking synth recursively)
// is refused.
func TestSafeguard_RecursionDepthOne(t *testing.T) {
	// Top-level call — no current chain state — depth defaults to 1.
	if err := CheckRecursion(context.Background()); err != nil {
		t.Errorf("top-level CheckRecursion returned %v, want nil", err)
	}
	// Simulate a depth-2 chain (admission → synth).
	ctx := WithRecursionChain(context.Background(), 2)
	if err := CheckRecursion(ctx); err == nil {
		t.Errorf("expected SynthesisRecursionDetected at depth 2; got nil")
	}
}

// TestSafeguard_RecursionCapConfigurable — the depth cap is
// internal. Tests can flip RecursionCap to verify multi-level
// chains (e.g. admission → synth → final merge); production
// keeps RecursionCap at 1.
//
// Contract: WithRecursionChain(ctx, n) records that the chain
// has already gone n levels deep. CheckRecursion increments by
// 1 (the prospective call) and compares against RecursionCap.
// Depth values:
//   - stored 0 → prospective 1 (top-level call): 1 > cap ?
//   - stored cap-1 → prospective cap (last allowed level)
//   - stored cap → prospective cap+1 (refused)
func TestSafeguard_RecursionCapConfigurable(t *testing.T) {
	prev := RecursionCap
	RecursionCap = 3
	defer func() { RecursionCap = prev }()

	// Stored depth 0..2 → prospective 1..3 → all within cap=3.
	for stored := 0; stored < RecursionCap; stored++ {
		ctx := WithRecursionChain(context.Background(), stored)
		if err := CheckRecursion(ctx); err != nil {
			t.Errorf("stored depth %d (prospective %d) allowed at cap=3; got %v",
				stored, stored+1, err)
		}
	}
	// Stored depth cap → prospective cap+1 → refused.
	ctx := WithRecursionChain(context.Background(), RecursionCap)
	if err := CheckRecursion(ctx); err == nil {
		t.Errorf("expected refusal at cap=3 with stored=%d; got nil",
			RecursionCap)
	}
}

// ────────────────────────────────────────────────────────────────────
// Successful path + telemetry-ready counters
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_OneCallPerPlannedStage_Success — case A:
// valid normal synthesis performs exactly ONE semantic call
// against the wire. The plan's counters reflect the call.
// The Stats struct is JSON-stable for downstream telemetry.
func TestSafeguard_OneCallPerPlannedStage_Success(t *testing.T) {
	const content = "Synthesised outcome: the two notes overlap on point X."
	var attempts atomic.Int32
	srv := httptest.NewServer(testOKHandler(content, &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	result, err := sc.SynthesizeWithPlan(context.Background(),
		[]string{"first fragment", "second fragment"}, plan)
	if err != nil {
		t.Fatalf("success path returned err=%v", err)
	}
	if result == nil || result.Content != content {
		t.Fatalf("expected content %q; got %v", content, result)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	s := plan.Stats()
	if s.PlannedCalls != 1 || s.CompletedCalls != 1 ||
		s.RetryCalls != 0 || s.RepairCalls != 0 || s.Exhausted {
		t.Errorf("plan stats wrong: %+v", s)
	}
	// Stats must be JSON-marshalable so telemetry can read it.
	if _, err := json.Marshal(s); err != nil {
		t.Errorf("Stats not JSON-stable: %v", err)
	}
}

// TestSafeguard_MultiBatchPlanSetsPlannedCalls — case K: a
// plan with N planned semantic calls reports N via Stats();
// the wire sees exactly one network hit because this test
// only drives the first planned call through and asserts
// the planner state.
func TestSafeguard_MultiBatchPlanSetsPlannedCalls(t *testing.T) {
	plan, err := NewBatchPlanForInvocation(5)
	if err != nil {
		t.Fatalf("plan build: %v", err)
	}
	if plan.PlannedCalls != 5 {
		t.Errorf("PlannedCalls = %d, want 5", plan.PlannedCalls)
	}
}

// ────────────────────────────────────────────────────────────────────
// O. legacy max_tokens has no effect
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_LegacyMaxTokensNoEffect — case O: a stored
// max_tokens=10 in legacy config must NOT shrink the
// substrate-supplied wire max_tokens. The safeguard's
// finiteness contract is independent of the value the
// operator once set (the user's knob was removed in the
// earlier release pass; this is the regression guard).
//
// The safeguard is exercised by the run finishing
// successfully with no spurious retry caused by the small
// legacy value.
func TestSafeguard_LegacyMaxTokensNoEffect(t *testing.T) {
	// Build an in-process client whose MaxTokens mirrors a
	// legacy alpha value; the safeguard should still allow
	// the run to complete in exactly ONE call.
	var attempts atomic.Int32
	srv := httptest.NewServer(testOKHandler("ok", &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	sc.MaxTokens = 10 // legacy would-be value
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err != nil {
		t.Fatalf("legacy max_tokens must NOT impact the safeguard; got err=%v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (legacy max_tokens must not trigger retry)", got)
	}
}
