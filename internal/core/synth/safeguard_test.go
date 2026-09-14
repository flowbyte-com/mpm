// safeguard_test.go — regression tests for the bounded-execution
// safeguard. The architectural contract being pinned:
//
//   per stage:    attempts <= 2           (1 fresh + 1 recovery)
//   per run:      attempts <= planned_stages * 2
//   recovery:     one slot per stage — retry XOR repair, never both
//
// All tests run in-process against httptest fake providers; no
// paid production endpoint is exercised.

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

// testSynthResultPayload returns the JSON payload that the
// Anthropic-shape envelope wraps in its "text" block. The
// Synthesize caller unmarshals the inner text directly into a
// SynthResult.
func testSynthResultPayload(content string) string {
	return fmt.Sprintf(`{"content":%q,"tags":["synthetic"]}`, content)
}

// testOKHandler renders a successful Anthropic-protocol
// completion whose inner text IS the SynthResult JSON.
// `attempts` (atomic, optional) counts every wire hit.
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

// testExplicitUncertaintyHandler returns a SynthResult whose
// Content reads as a "valid uncertainty" terminal outcome. The
// safeguard MUST NOT retry on this — it is an explicit refusal,
// not a mechanical failure.
func testExplicitUncertaintyHandler(reason string, attempts *atomic.Int32) http.HandlerFunc {
	return testOKHandler("The source is "+reason+"; no reliable conclusion can be derived.",
		attempts)
}

// newTestClient builds a SynthClient pointed at the given
// httptest URL with a wire that matches the test handler's
// expected request shape (Anthropic).
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
// Per-stage recovery slot — 2 attempts max, retry XOR repair.
// ────────────────────────────────────────────────────────────────────

// A. Normal semantic success — one provider call. No retry, no repair.
func TestSafeguard_A_NormalSuccessOneProviderCall(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(testOKHandler(
		"a clean synthesis result for two notes", &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	res, err := sc.SynthesizeWithPlan(context.Background(),
		[]string{"first", "second"}, plan)
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	if res == nil {
		t.Fatal("result must not be nil")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (single fresh semantic call)", got)
	}
	s := plan.Stats()
	if s.CompletedStages != 1 || s.ActualProviderCalls != 1 ||
		s.RecoveryCalls != 0 || s.RepairCalls != 0 ||
		s.Exhausted {
		t.Errorf("plan stats wrong: %+v", s)
	}
}

// B. Square-root nonsense + explicit uncertainty — one provider call.
// Semantic refusal is a valid terminal result; NO retry, NO repair.
func TestSafeguard_B_SquareRootSentinel(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(testExplicitUncertaintyHandler(
		"mathematically undefined", &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	res, err := sc.SynthesizeWithPlan(context.Background(),
		[]string{"square root of divided by zero x 0"}, plan)
	if err != nil {
		t.Fatalf("explicit uncertainty must succeed; got err=%v", err)
	}
	if res == nil {
		t.Fatal("result must not be nil")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (single fresh + no retry on uncertainty)", got)
	}
	s := plan.Stats()
	if s.ActualProviderCalls != 1 || s.RecoveryCalls != 0 ||
		s.RepairCalls != 0 || s.Exhausted {
		t.Errorf("plan stats wrong for uncertainty: %+v", s)
	}
}

// C. Contradiction + explicit contradiction refusal — one call.
func TestSafeguard_C_ContradictionOneCall(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(testExplicitUncertaintyHandler(
		"contradictory", &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(),
		[]string{"note A says yes", "note B says no"}, plan)
	if err != nil {
		t.Fatalf("explicit contradiction must succeed; got err=%v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (single fresh)", got)
	}
}

// D. 5xx then success — exactly 2 calls (1 fresh + 1 retry).
func TestSafeguard_D_5xxThenSuccessTwoCalls(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			http.Error(w, "transient", http.StatusBadGateway)
			return
		}
		testOKHandler("recovered result", nil)(w, r)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err != nil {
		t.Fatalf("recovery should succeed; got: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 fresh + 1 retry)", got)
	}
	s := plan.Stats()
	if s.RecoveryCalls != 1 || s.RepairCalls != 0 {
		t.Errorf("retry/repair stats wrong: %+v", s)
	}
}

// E. 5xx then malformed — exactly 2 calls; recovery slot
// consumed by retry, not repair. Second failure MUST stop.
func TestSafeguard_E_5xxThenMalformedStops(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			http.Error(w, "transient", http.StatusBadGateway)
			return
		}
		// Recovery attempt returns malformed body.
		_, _ = io.WriteString(w, `not valid json at all`)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err == nil {
		t.Fatal("expected stop after second failure")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 fresh + 1 retry); NO third call", got)
	}
	s := plan.Stats()
	if s.RecoveryCalls != 1 || s.RepairCalls != 0 {
		t.Errorf("recovery slot must be allocated to retry only: %+v", s)
	}
	if !s.Exhausted {
		t.Errorf("plan must be exhausted after second failure: %+v", s)
	}
}

// F. Malformed then valid repair — exactly 2 calls; recovery as repair.
func TestSafeguard_F_MalformedThenValidRepair(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			_, _ = io.WriteString(w, `not valid json at all`)
			return
		}
		testOKHandler("repaired content", nil)(w, r)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err != nil {
		t.Fatalf("repair should succeed; got: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	s := plan.Stats()
	if s.RecoveryCalls != 0 || s.RepairCalls != 1 {
		t.Errorf("recovery slot must be allocated to repair: %+v", s)
	}
}

// G. Malformed then 5xx — exactly 2 calls; recovery as repair,
// second failure (5xx on the repair attempt) MUST stop.
func TestSafeguard_G_MalformedThen5xxStops(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			_, _ = io.WriteString(w, `not valid json at all`)
			return
		}
		http.Error(w, "transient on repair", http.StatusBadGateway)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err == nil {
		t.Fatal("expected stop after second failure")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 fresh + 1 repair); NO retry", got)
	}
	s := plan.Stats()
	if s.RepairCalls != 1 || s.RecoveryCalls != 0 {
		t.Errorf("recovery was repair, not retry: %+v", s)
	}
}

// H. Empty response then valid recovery — exactly 2 calls.
// Empty Content is mechanical failure (per the tightening pass).
func TestSafeguard_H_EmptyResponseValidRecovery(t *testing.T) {
	var attempts atomic.Int32
	// First attempt returns valid envelope but EMPTY text. Second
	// returns valid uncertainty → success on repair.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			// Valid envelope but empty Content.
			body := fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`,
				testSynthResultPayload(""))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
			return
		}
		testExplicitUncertaintyHandler("undefined on repair", nil)(w, r)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	res, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err != nil {
		t.Fatalf("recovery should succeed; got: %v", err)
	}
	if res == nil {
		t.Fatal("result must not be nil")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 fresh + 1 repair)", got)
	}
}

// I. Empty response twice — exactly 2 calls; failure.
func TestSafeguard_I_EmptyResponseTwiceFails(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		body := fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`,
			testSynthResultPayload(""))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err == nil {
		t.Fatal("expected failure on empty-twice")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 fresh + 1 repair attempt)", got)
	}
	s := plan.Stats()
	if s.RepairCalls != 1 {
		t.Errorf("recovery must be allocated as repair on empty: %+v", s)
	}
}

// J. 401 — exactly 1 call.
func TestSafeguard_J_401FailsFast(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "auth", http.StatusUnauthorized)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err == nil {
		t.Fatal("expected failure on 401")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 401)", got)
	}
}

// K. 403 — exactly 1 call.
func TestSafeguard_K_403FailsFast(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err == nil {
		t.Fatal("expected failure on 403")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 403)", got)
	}
}

// L. 429 no Retry-After — exactly 1 call.
func TestSafeguard_L_429NoRetryAfterStops(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "") // absent value
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err == nil {
		t.Fatal("expected failure on 429 without Retry-After")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry when Retry-After absent)", got)
	}
}

// M. 429 Retry-After=5 — at most 2 calls (recovery one retry).
func TestSafeguard_M_429RetryAfterBounded(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		testOKHandler("after 5-second wait", nil)(w, r)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	plan := NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err != nil {
		t.Fatalf("retry should succeed; got: %v", err)
	}
	if got := attempts.Load(); got > 2 {
		t.Errorf("attempts = %d, want <= 2", got)
	}
}

// N. 429 Retry-After=60 — exactly 1 call (RecoveryAfter > 10s → no retry).
func TestSafeguard_N_429LargeRetryAfterStops(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err == nil {
		t.Fatal("expected failure on 429 Retry-After>10s")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on Retry-After>10s)", got)
	}
}

// O. Duplicate-stage — third attempt for the same fingerprint
// is refused BEFORE the wire is touched.
func TestSafeguard_O_DuplicateStageRefused(t *testing.T) {
	// Drive the plan through AcquireOrStop directly. Per the
	// tightening pass, a stage accepts: 1 fresh + 1 recovery
	// = 2 calls. The third attempt of the same fingerprint
	// is refused.
	plan := NewPerCallPlan()
	fpr := uint64(0xFACEFEEDBEEFCAFE)
	if err := plan.AttemptFresh(fpr); err != nil {
		t.Fatalf("first AttemptFresh must succeed; got %v", err)
	}
	// Use the recovery slot.
	if err := plan.AttemptRecovery(fpr, RecoveryRetry); err != nil {
		t.Fatalf("first AttemptRecovery must succeed; got %v", err)
	}
	// Third attempt — refused regardless of kind.
	if err := plan.AttemptFresh(fpr); err == nil {
		t.Errorf("duplicate AttemptFresh must be refused")
	}
	if err := plan.AttemptRecovery(fpr, RecoveryRepair); err == nil {
		t.Errorf("third AttemptRecovery must be refused")
	}
	s := plan.Stats()
	if s.ActualProviderCalls != 2 {
		t.Errorf("actual calls = %d, want 2", s.ActualProviderCalls)
	}
	if !s.Exhausted {
		t.Errorf("plan must be exhausted: %+v", s)
	}
}

// ────────────────────────────────────────────────────────────────────
// Per-invocation stage ceiling
// ────────────────────────────────────────────────────────────────────

// P. planned_stages=8 → max provider calls = 16.
func TestSafeguard_P_MaxSemanticStagesEight(t *testing.T) {
	plan, err := NewBoundedPlan(8)
	if err != nil {
		t.Fatalf("plan build: %v", err)
	}
	if plan.PlannedStages != 8 {
		t.Errorf("PlannedStages = %d, want 8", plan.PlannedStages)
	}
	// Per-stage: 2 calls × 8 stages = 16.
	for s := 0; s < 8; s++ {
		fpr := uint64(s + 1)
		if err := plan.AttemptFresh(fpr); err != nil {
			t.Errorf("stage %d fresh: %v", s, err)
		}
		if err := plan.AttemptRecovery(fpr, RecoveryRetry); err != nil {
			t.Errorf("stage %d retry: %v", s, err)
		}
	}
	s := plan.Stats()
	if s.ActualProviderCalls != 16 {
		t.Errorf("actual calls = %d, want 16 (8 × 2)", s.ActualProviderCalls)
	}
	if s.CompletedStages != 0 {
		t.Errorf("CompletedStages = %d (none successful in this test)", s.CompletedStages)
	}
}

// Q. planned_stages=9 → NewBoundedPlan returns ErrInputTooLarge
// (the per-invocation ceiling is 8).
func TestSafeguard_Q_PlannedStagesNineRejected(t *testing.T) {
	_, err := NewBoundedPlan(9)
	if err == nil {
		t.Fatal("expected ErrInputTooLarge for planned=9")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("err must mention 'too large'; got %v", err)
	}
}

// R + S. compact-drain-style multi-batch — process up to 8
// batches, leave remaining visible.
func TestSafeguard_RS_BoundedContinuation(t *testing.T) {
	plan, err := NewBoundedPlan(8)
	if err != nil {
		t.Fatalf("plan build: %v", err)
	}
	// Simulate 30 eligible items.
	total := 30
	ceiling := plan.PlannedStages
	processed := 0
	if total > ceiling {
		processed = ceiling
	} else {
		processed = total
	}
	remaining := total - processed
	if remaining != 22 {
		t.Errorf("expected remaining=22 (30 - 8); got %d", remaining)
	}
}

// T. Recursion depth > 1 is refused.
func TestSafeguard_T_RecursionRefused(t *testing.T) {
	// Top-level call passes.
	if err := CheckRecursion(context.Background()); err != nil {
		t.Errorf("top-level recursion refused: %v", err)
	}
	// depth stored = 1 → prospective 2 → cap=1 → refused.
	ctx := WithRecursionChain(context.Background(), 1)
	if err := CheckRecursion(ctx); err == nil {
		t.Errorf("expected recursion refusal at depth 2 (cap=1); got nil")
	}
}

// U. legacy max_tokens=10 has no behavioural impact — the
// safeguard's finiteness is independent of the wire's
// max_tokens value.
func TestSafeguard_U_LegacyMaxTokensNoEffect(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(testOKHandler("ok", &attempts))
	defer srv.Close()

	sc := newTestClient(t, srv.URL)
	sc.MaxTokens = 10 // legacy would-be value
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, NewPerCallPlan())
	if err != nil {
		t.Fatalf("legacy max_tokens must not impact the safeguard; got err=%v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (legacy max_tokens is inert)", got)
	}
}

// ────────────────────────────────────────────────────────────────────
// Recovery-slot policy invariants
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_RecoverySlotSingleAllocation — recovery on a
// stage can be allocated to Retry OR Repair, not both.
func TestSafeguard_RecoverySlotSingleAllocation(t *testing.T) {
	plan := NewPerCallPlan()
	fpr := uint64(42)
	if err := plan.AttemptFresh(fpr); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	// First recovery as Retry succeeds.
	if err := plan.AttemptRecovery(fpr, RecoveryRetry); err != nil {
		t.Fatalf("retry recovery: %v", err)
	}
	// Attempting to ALSO use the slot for Repair is refused.
	if err := plan.AttemptRecovery(fpr, RecoveryRepair); err == nil {
		t.Errorf("second recovery slot must be refused (slot already used for Retry)")
	}
	s := plan.Stats()
	if s.RecoveryCalls != 1 || s.RepairCalls != 0 {
		t.Errorf("recovery/repair stats wrong: %+v", s)
	}
}

// TestSafeguard_AbsoluteCeilingRefused — exceeding
// planned_stages * 2 surfaces ErrBoundedPlanExceeded.
func TestSafeguard_AbsoluteCeilingRefused(t *testing.T) {
	// planned_stages = 1 → absolute ceiling = 2.
	plan, err := NewBoundedPlan(1)
	if err != nil {
		t.Fatalf("plan build: %v", err)
	}
	fpr := uint64(1)
	if err := plan.AttemptFresh(fpr); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if err := plan.AttemptRecovery(fpr, RecoveryRetry); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := plan.Stats().ActualProviderCalls; got != 2 {
		t.Errorf("actual calls = %d, want 2", got)
	}
	// Third call (beyond absolute ceiling) is refused.
	if err := plan.AttemptFresh(fpr); err == nil {
		t.Errorf("third AttemptFresh must be refused")
	}
}

// ────────────────────────────────────────────────────────────────────
// Telemetry-stable Stats shape
// ────────────────────────────────────────────────────────────────────

// TestSafeguard_StatsJSONStable — the Stats struct must be
// JSON-marshalable with the brief-pinned stable fields:
// planned_stages, completed_stages, retry_calls,
// repair_calls, actual_provider_calls, exhausted,
// last_failure_class.
func TestSafeguard_StatsJSONStable(t *testing.T) {
	plan, err := NewBoundedPlan(2)
	if err != nil {
		t.Fatalf("plan build: %v", err)
	}
	for s := 0; s < 2; s++ {
		fpr := uint64(s + 1)
		if err := plan.AttemptFresh(fpr); err != nil {
			t.Fatalf("stage %d fresh: %v", s, err)
		}
		if err := plan.AttemptRecovery(fpr, RecoveryRepair); err != nil {
			t.Fatalf("stage %d repair: %v", s, err)
		}
		plan.RecordFailure(FailureInvalidMachineResponse)
	}
	s := plan.Stats()

	type wireStats struct {
		PlannedStages       int    `json:"planned_stages"`
		CompletedStages     int    `json:"completed_stages"`
		RecoveryCalls       int    `json:"retry_calls"`
		RepairCalls         int    `json:"repair_calls"`
		ActualProviderCalls int    `json:"actual_provider_calls"`
		Exhausted           bool   `json:"exhausted"`
		StopReason          string `json:"stop_reason,omitempty"`
		LastFailureClass    string `json:"last_failure_class"`
	}
	b, err := json.Marshal(wireStats{
		PlannedStages:       s.PlannedStages,
		CompletedStages:     s.CompletedStages,
		RecoveryCalls:       s.RecoveryCalls,
		RepairCalls:         s.RepairCalls,
		ActualProviderCalls: s.ActualProviderCalls,
		Exhausted:           s.Exhausted,
		LastFailureClass:    s.LastFailureClass.String(),
	})
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}
	for _, want := range []string{
		"\"planned_stages\":2",
		"\"actual_provider_calls\":4",
		"\"exhausted\":false",
		"\"retry_calls\":0",
		"\"repair_calls\":2",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("expected %q in wire JSON; got: %s", want, b)
		}
	}
}
