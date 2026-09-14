// do_request.go — wire-aware central HTTP helper for all LLM call sites.
//
// 2026-09-14 release-pass: every call here routes through Plan's
// recovery slot. There is exactly ONE recovery slot per stage,
// shared between transient retry and structural repair. The
// classification table:
//
//   FailureClass              | Recovery slot used? | Notes
//   --------------------------|---------------------|----------------------------
//   transient_transport      | yes (RetryKind)     | one attempt max
//   auth / billing / 403      | NO                 | stop immediately
//   rate_limit (no RA)        | NO                 | stop immediately
//   rate_limit (RA <= 10s)    | yes (RetryKind)     | one attempt max
//   rate_limit (RA > 10s)     | NO                 | stop immediately
//   unknown 4xx               | NO                 | conservative; stop
//
// Auth/billing refusals cannot retry. Unknown 4xx (e.g. 400
// bad payload) cannot retry. The conservative default for
// anything we don't recognise is "stop" — better to lose a
// single run than to retry blindly.
//
// Structural-decode / empty-content failures are handled at the
// call site (Synthesize / SynthesizeCompactLesson) — they
// consume the recovery slot for RepairKind, not RetryKind.

package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrInvalidMachineResponse is returned by DoLLMRequestWithPlan
// when the server returned 200 OK but the body could not be
// read. Call sites treat this as FailureInvalidMachineResponse
// and consume the recovery slot via AttemptRecovery(RepairKind).
var ErrInvalidMachineResponse = errors.New("synth: invalid machine response")

// retryAfterSecondsLimit is the threshold at which a
// Retry-After value is honoured. Anything above the threshold
// means "wait too long" — stop and surface the provider's
// directive instead of sleeping inside the run.
//
// 10 seconds is the operative rule: provider response budgets
// of <= 10s are reasonable for a synthesis run; budgets above
// that suggest the provider is genuinely throttled and the
// operator's run should fail rather than sleep.
const retryAfterSecondsLimit = 10

// maxRetryDelay is the upper bound on how long doOnce sleeps
// before its single recovery attempt.
const maxRetryDelay = retryAfterSecondsLimit * time.Second

// AttemptMode labels the kind of attempt being made against
// the plan. The plan categorises attempts into Fresh and
// Recovery, enforcing the "2 attempts per stage" rule (1
// fresh + 1 recovery slot, the slot is consumed by retry OR
// repair, never both).
type AttemptMode int

const (
	AttemptFresh AttemptMode = iota
	AttemptRecovery
)

// DoLLMRequest is the historical entry point that builds a
// per-call plan and delegates to DoLLMRequestWithPlan.
func (sc *SynthClient) DoLLMRequest(ctx context.Context, body map[string]interface{}) ([]byte, error) {
	return sc.DoLLMRequestWithPlan(ctx, body, NewPerCallPlan(), AttemptFresh)
}

// requestCallcounter is a debug-only monotonic counter used
// by tests to verify behaviour without exposing the Plan.
// Production code MUST NOT consult this.
var requestCallcounter atomic.Int64

// LastRequestCalls returns the test-only call count.
func LastRequestCalls() int64 { return requestCallcounter.Load() }

// ResetCallcounter clears the test-only counter.
func ResetCallcounter() { requestCallcounter.Store(0) }

// DoLLMRequestWithPlan routes a single semantic LLM attempt
// through the Plan's bounded-execution guard. The mode
// argument tells the helper whether the call is the FIRST
// fresh attempt (AttemptFresh) or a recovery attempt after
// the caller has already classified the failure as
// structural / reparable (AttemptRecovery).
//
// Fresh-mode flow:
//   1. plan.AttemptFresh(fingerprint)
//   2. wire attempt
//   3. if transient_transport OR bounded 429 →
//      consume recovery slot (RetryKind), retry ONCE
//   4. if auth / unbounded 429 / unknown 4xx → stop
//   5. on 200 → return raw body
//
// Recovery-mode flow (caller has classified the failure as
// FailureInvalidMachineResponse; recovery slot already
// allocated to RepairKind via plan.AttemptRecovery BEFORE
// this call):
//   1. wire attempt
//   2. on 200 → return raw body
//   3. on any failure → stop (no second recovery; per-stage
//      cap = 2 means after THIS call we're done)
//
// Returns the raw response body on success. Returns
// ErrBoundedPlanExceeded if the safeguard trips, or a typed
// error for auth / billing / rate-limit failures.
func (sc *SynthClient) DoLLMRequestWithPlan(ctx context.Context, body map[string]interface{}, plan *Plan, mode AttemptMode) ([]byte, error) {
	requestCallcounter.Add(1)
	if plan == nil {
		plan = NewPerCallPlan()
	}
	fingerprint := fingerprintFromBody(body)

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal llm request: %w", err)
	}

	switch mode {
	case AttemptFresh:
		if err := plan.AttemptFresh(fingerprint); err != nil {
			return nil, err
		}
	case AttemptRecovery:
		// Caller has already allocated the recovery slot via
		// AttemptRecovery(fpr, RepairKind). We do NOT call
		// AttemptFresh here — that would count a duplicate
		// fresh attempt and the plan would refuse.
	default:
		return nil, fmt.Errorf("invalid AttemptMode: %d", mode)
	}

	respBody, class, httpStatus, err := sc.doOnce(ctx, payload)
	if err == nil && class == 0 {
		plan.RecordSuccess()
		return respBody, nil
	}
	if err != nil {
		if httpStatus > 0 {
			class = classifyFromStatus(httpStatus)
		} else {
			class = classifyFromStatus(0)
		}
	}
	plan.RecordFailure(class)

	// Recovery-mode attempts never retry. The slot is already
	// consumed; a second failure stops the stage.
	if mode == AttemptRecovery {
		return nil, fmt.Errorf("API request failed on recovery attempt (%s): %s",
			class, errString(err, httpStatus))
	}

	// Fresh-mode: classify → either consume retry slot + retry,
	// or fail-fast.
	switch class {
	case FailureAuth:
		return nil, fmt.Errorf("provider refused request (%s): %s",
			class, errString(err, httpStatus))

	case FailureRateLimit:
		retryAfterSeconds, retryable := retryAfterBoundedSeconds()
		if !retryable {
			return nil, fmt.Errorf("provider rate-limited; Retry-After unbounded or > %ds: %s",
				retryAfterSecondsLimit, errString(err, httpStatus))
		}
		if err := plan.AttemptRecovery(fingerprint, RecoveryRetry); err != nil {
			return nil, err
		}
		delay := time.Duration(retryAfterSeconds) * time.Second
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		respBody, class, httpStatus, err = sc.doOnce(ctx, payload)
		if err == nil && class == 0 {
			plan.RecordSuccess()
			return respBody, nil
		}
		if err != nil {
			if httpStatus > 0 {
				class = classifyFromStatus(httpStatus)
			} else {
				class = classifyFromStatus(0)
			}
		}
		plan.RecordFailure(class)
		return nil, fmt.Errorf("provider rate-limited; recovery attempt failed (%s): %s",
			class, errString(err, httpStatus))

	case FailureTransientTransport:
		if err := plan.AttemptRecovery(fingerprint, RecoveryRetry); err != nil {
			return nil, err
		}
		// Transient retry: short, ctx-aware backoff. We do NOT
		// apply provider 429 directives here — those live on the
		// rate-limit branch.
		const transientBackoff = 1 * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(transientBackoff):
		}
		respBody, class, httpStatus, err = sc.doOnce(ctx, payload)
		if err == nil && class == 0 {
			plan.RecordSuccess()
			return respBody, nil
		}
		if err != nil {
			if httpStatus > 0 {
				class = classifyFromStatus(httpStatus)
			} else {
				class = classifyFromStatus(0)
			}
		}
		plan.RecordFailure(class)
		return nil, fmt.Errorf("API request failed after recovery (%s): %s",
			class, errString(err, httpStatus))

	default:
		if err != nil {
			return nil, fmt.Errorf("API request failed (%s): %w", class, err)
		}
		return nil, fmt.Errorf("API returned HTTP %d: %s", httpStatus, string(respBody))
	}
}

// doOnce executes the HTTP exchange. Returns (body, class,
// httpStatus, err). When err == nil, class is the zero value
// (success). When err != nil, class is computed by
// classifyFromStatus when a status is present, and
// FailureTransientTransport otherwise (no status → transport
// failure). The Retry-After response header (if present) is
// captured into lastRetryAfterHeader so the rate-limit
// branch above can consult it without re-parsing.
func (sc *SynthClient) doOnce(ctx context.Context, payload []byte) ([]byte, FailureClass, int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		sc.BaseURL+sc.Wire.path(), bytes.NewReader(payload))
	if err != nil {
		return nil, FailureTransientTransport, 0,
			fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	authName, authValue := sc.Wire.authHeader(sc.APIKey)
	req.Header.Set(authName, authValue)

	client := &http.Client{Timeout: sc.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, FailureTransientTransport, 0,
			fmt.Errorf("API request failed: %w", err)
	}
	// Capture Retry-After BEFORE reading the body so the policy
	// dispatcher sees it on every fresh-mode call.
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		recordRetryAfter(ra)
	} else {
		recordRetryAfter("")
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, FailureInvalidMachineResponse, resp.StatusCode,
			fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return respBody, 0, resp.StatusCode, nil
	}
	return respBody, classifyFromStatus(resp.StatusCode), resp.StatusCode,
		fmt.Errorf("API returned HTTP %d: %s", resp.StatusCode, string(respBody))
}

// classifyFromStatus maps an HTTP status code to a failure
// class. 4xx codes that mean "you broke something" or "I
// refuse" map to FailureAuth / FailureRateLimit /
// FailureTransientTransport as appropriate. The default is
// FailureUnknown — treated conservatively as non-retriable
// by the policy dispatcher.
func classifyFromStatus(status int) FailureClass {
	switch {
	case status == 401 || status == 403 || status == 402:
		return FailureAuth
	case status == 429:
		return FailureRateLimit
	case status >= 500 && status < 600:
		return FailureTransientTransport
	}
	return FailureUnknown
}

// errString returns the most informative of (error, status).
// Used in error message formatting; never returns the body
// byte slice verbatim.
func errString(err error, status int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", status)
}

// lastRetryAfterHeader is captured between doOnce calls. The
// value is process-global; that is acceptable because the
// safeguard is per-run.
var lastRetryAfterHeader atomic.Value

func recordRetryAfter(s string) {
	lastRetryAfterHeader.Store(s)
}

// retryAfterFromCache returns the most-recently-observed
// Retry-After header value (string-typed).
func retryAfterFromCache() string {
	v := lastRetryAfterHeader.Load()
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// retryAfterBoundedSeconds parses Retry-After (integer-seconds
// or HTTP-date) and returns (seconds, retryable). For
// integer-seconds, the bound is retryAfterSecondsLimit (10s).
//
// HTTP-date Retry-After is the spec-defined form (RFC 7231 §7.1.3).
// We parse it with the same package stdlib parser. If parsing
// fails, treat as unbounded and stop.
func retryAfterBoundedSeconds() (int, bool) {
	ra := retryAfterFromCache()
	if ra == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
		if n < 0 {
			return 0, false
		}
		if n > retryAfterSecondsLimit {
			return n, false
		}
		return n, true
	}
	t, err := http.ParseTime(ra)
	if err != nil {
		return 0, false
	}
	now := time.Now()
	d := int(t.Sub(now).Seconds())
	if d < 0 {
		return 0, true
	}
	if d > retryAfterSecondsLimit {
		return d, false
	}
	return d, true
}

// fingerprintFromBody returns a 64-bit FNV hash of the body
// marshalled bytes.
func fingerprintFromBody(body map[string]interface{}) uint64 {
	if body == nil {
		return 0
	}
	b, err := json.Marshal(body)
	if err != nil {
		return 0
	}
	return fnvSum64(b)
}

// fnvSum64 is a tiny FNV-1a implementation.
func fnvSum64(b []byte) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	h := offset
	for _, c := range b {
		h ^= uint64(c)
		h *= prime
	}
	return h
}
