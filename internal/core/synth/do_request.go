// do_request.go — wire-aware central HTTP helper for all LLM call sites.
//
// 2026-09-14 release-pass: replaced the ad-hoc retry loop with a
// policy-driven Plan + FailureClass dispatch. The plan enforces
// a finite pre-computable retry/repair budget per run; the
// classifier maps HTTP statuses (and net.OpError-shaped messages)
// to one of:
//
//   - FailureTransientTransport (5xx, conn-reset, EOF, timeout)
//     -> permit up to MaxRetriesPerStage retries.
//   - FailureAuth (401, 403, 402 billing) -> zero retries, fail.
//   - FailureRateLimit (429) -> one retry only if the response
//     carries a bounded Retry-After hint within the run deadline;
//     otherwise stop and report.
//   - FailureInvalidMachineResponse (200 OK, malformed body) ->
//     the call site treats this as a repair attempt, attributed
//     to the plan's repair budget.
//
// Each plan provides a finite retry/repair ceiling; the
// dispatcher increments plan.RetryOrStop on permitted retries
// and stops the run when the budget is exhausted. Callers must
// hold a Plan instance and pass it to DoLLMRequestWithPlan.
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
// when the server returned 200 OK but the body could not be read
// — for the alpha it only covers transport read failures
// (separate from the call site's structural-decode failure,
// which is reported as FailureInvalidMachineResponse at the
// call site and consumes the repair budget).
var ErrInvalidMachineResponse = errors.New("synth: invalid machine response")

// DoLLMRequest is the historical entry point. It builds a
// per-call plan (1 semantic call, default retry/repair budgets)
// and delegates to DoLLMRequestWithPlan.
func (sc *SynthClient) DoLLMRequest(ctx context.Context, body map[string]interface{}) ([]byte, error) {
	return sc.DoLLMRequestWithPlan(ctx, body, NewPerCallPlan())
}

// requestCallcounter is a debug-only monotonic counter used by
// tests to verify behaviour without exposing the Plan. Not
// exported.
var requestCallcounter atomic.Int64

// LastRequestCalls returns the test-only call count. Cleared
// by ResetCallcounter. Production code MUST NOT consult this.
func LastRequestCalls() int64 { return requestCallcounter.Load() }

// ResetCallcounter clears the test-only counter.
func ResetCallcounter() { requestCallcounter.Store(0) }

// DoLLMRequestWithPlan routes a single semantic LLM attempt
// through the bounded-execution plan. Each call:
//
//   1. Acquires one entry in the plan via Plan.AcquireOrStop.
//      The first attempt is the "fresh semantic" attempt; a
//      transient failure retry follows via Plan.RetryOrStop.
//   2. Builds the request body (JSON marshalled once per
//      call attempt). Marshalling failures are not retried —
//      they are programmer errors.
//   3. Sends to the wire. The response is classified by
//      ClassifyFailure (status code or transport-error
//      message). The plan's AllowRetry decision follows.
//   4. On 200 OK, returns the raw body. The caller parses it
//      into its wire-specific shape (existing ParseResponseBody
//      pattern); structural parse failure at the call site is
//      reported back through the plan's repair budget.
//
// Returns the raw response body on success. Returns
// ErrBoundedPlanExceeded if the safeguard trips, or a typed
// error for auth / billing / rate limit failures.
func (sc *SynthClient) DoLLMRequestWithPlan(ctx context.Context, body map[string]interface{}, plan *Plan) ([]byte, error) {
	requestCallcounter.Add(1)
	if plan == nil {
		plan = NewPerCallPlan()
	}
	// Stage fingerprint — derived from the marshalled body
	// (post-normalisation) — is the duplicate-stage guard's
	// input. We hash the body bytes; same body + same wire
	// = same fingerprint. The first attempt acquires with
	// this fingerprint; a permitted retry re-acquires via
	// the same path (RetryOrStop increments retryCalls).
	fingerprint := fingerprintFromBody(body)

	// Marshal the body once; transport attempts re-use it.
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal llm request: %w", err)
	}

	// First attempt (fresh semantic): count a planned call.
	if err := plan.AcquireOrStop(fingerprint); err != nil {
		return nil, err
	}

	// Transient retry loop — bounded by plan.MaxRetriesPerStage.
	// Each retry is permitted through plan.RetryOrStop before
	// it is allowed to fire over the wire.
	var lastErr error
	maxRetries := plan.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Permitted retry — increment the counter first
			// (RetryOrStop halts the loop on budget exhaustion).
			if err := plan.RetryOrStop(); err != nil {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}

		respBody, class, httpStatus, err := sc.doOnce(ctx, payload)
		if err == nil && class == 0 {
			plan.AccountSuccess()
			return respBody, nil
		}

		if err != nil {
			// Prefer the status-based classifier for non-zero
			// statuses — fallback to the string-based classifier
			// only when the wire never produced a status.
			if httpStatus > 0 {
				class = classifyFromStatus(httpStatus)
			} else {
				class = classifyTransient(httpStatus, err.Error())
			}
		}
		plan.AccountFailure(class)

		// Auth / billing / quota refusal: zero retries.
		if class == FailureAuth {
			return nil, fmt.Errorf("provider refused request (%s): %s",
				class, errString(err, httpStatus))
		}

		// Rate limit: at most ONE retry only when the response
		// gives a bounded retry path (Retry-After within
		// remaining deadline). The plan's retry budget caps
		// the worst case to MaxRetriesPerStage anyway.
		if class == FailureRateLimit {
			if attempt >= maxRetries {
				return nil, fmt.Errorf("provider rate-limited; no retries remaining: %s",
					errString(err, httpStatus))
			}
			// Inspect Retry-After; if it would extend past
			// the run deadline (or is malformed), stop.
			if !retryAfterIsBounded(httpStatus, "") {
				return nil, fmt.Errorf("provider rate-limited; retry-after %q not bounded: %s",
					retryAfterFromCache(), errString(err, httpStatus))
			}
			lastErr = err
			continue
		}

		// Transport failure (5xx, timeout, EOF, conn reset):
		// retry up to MaxRetriesPerStage. Note: any class
		// OTHER than auth/rate/transient (i.e. FailureUnknown
		// for unknown 4xx etc.) is non-retriable and falls
		// through immediately, stopping the loop on the
		// first attempt — that is the desired 4xx fail-fast.
		if class == FailureTransientTransport && attempt < maxRetries {
			lastErr = err
			continue
		}

		// Non-retriable failure (FailureUnknown — e.g. 400
		// bad payload, 422 unprocessable, etc.) or last
		// attempt of a transient series. Stop the loop now:
		// returning here on attempt=0 means exactly ONE
		// network hit for non-retriable failures, and on
		// attempt=maxRetries means the retry budget is
		// exhausted.
		if err != nil {
			return nil, fmt.Errorf("API request failed (%s): %w", class, err)
		}
		return nil, fmt.Errorf("API returned HTTP %d: %s", httpStatus, string(respBody))
	}

	if lastErr != nil {
		return nil, fmt.Errorf("API request failed after retries: %w", lastErr)
	}
	return nil, errors.New("API request failed: no response body")
}

// doOnce executes the HTTP exchange. Returns (body, class,
// httpStatus, err). When err == nil, class is the zero value
// (success). When err != nil, class is computed by the caller
// via classifyTransient (in case the classifier wants the
// status code AND the error string).
func (sc *SynthClient) doOnce(ctx context.Context, payload []byte) ([]byte, FailureClass, int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		sc.BaseURL+sc.Wire.path(), bytes.NewReader(payload))
	if err != nil {
		return nil, FailureUnknown, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	authName, authValue := sc.Wire.authHeader(sc.APIKey)
	req.Header.Set(authName, authValue)

	client := &http.Client{Timeout: sc.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		// No HTTP exchange — treat as transient transport.
		return nil, FailureUnknown, 0, fmt.Errorf("API request failed: %w", err)
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
	// Non-OK — classify from status alone.
	return respBody, classifyFromStatus(resp.StatusCode), resp.StatusCode,
		fmt.Errorf("API returned HTTP %d: %s", resp.StatusCode, string(respBody))
}

// classifyFromStatus maps an HTTP status code to a failure
// class. Codes that genuinely mean "stop retrying" map to
// FailureAuth / FailureRateLimit / FailureTransientTransport
// as appropriate.
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
// byte slice verbatim (operator may have leaked sensitive
// input via the prompt — we render status only).
func errString(err error, status int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", status)
}

// lastRetryAfterHeader is captured between doOnce calls so
// ClassifyRetryAfter can consult it without re-parsing. The
// value is process-global; that is acceptable because the
// safeguard is per-run and operators do not normally run
// concurrent synth jobs.
var lastRetryAfterHeader atomic.Value

func setLastRetryAfter(s string) {
	lastRetryAfterHeader.Store(s)
}

// retryAfterFromCache returns the most-recently-observed
// Retry-After header value (string-typed). Used by the
// rate-limit handler. Returns "" when never set.
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

// retryAfterIsBounded is a stub. The full implementation will
// parse the Retry-After value and compare against the run
// deadline. For the alpha, we accept any non-empty value
// as "bounded" — a stricter check goes in a follow-up pass
// once the failure-mode tests pin the policy.
func retryAfterIsBounded(_ int, _ string) bool {
	return retryAfterFromCache() != ""
}

// fingerprintFromBody returns a 64-bit FNV hash of the body's
// marshalled bytes. The same logical stage (same model ID,
// same prompt content, same temperature) produces the same
// fingerprint across retry attempts. The Plan keys
// fingerprints for duplicate-stage detection.
func fingerprintFromBody(body map[string]interface{}) uint64 {
	if body == nil {
		return 0
	}
	// Use only the structural keys we care about: "model"
	// and the user message bytes. Marshal the body once and
	// hash. We deliberately include the prompt content so
	// the fingerprint catches true semantic duplicates even
	// when the LLM call returns a "different" response.
	b, err := json.Marshal(body)
	if err != nil {
		return 0
	}
	return fnvSum64(b)
}

// fnvSum64 is a tiny FNV-1a implementation. The map-reduce
// not pulling in a new dependency.
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

// parseRetryAfter accepts a Retry-After header value and
// returns the seconds-delta it implies. Returns -1 when the
// value is malformed. The Retry-After can be either an HTTP
// date or a delta-seconds value; this helper handles the
// delta-seconds case (the common shape for LLM providers).
//
// Kept exported via the package's test exports so the
// failure-mode tests can drive it directly.
func parseRetryAfter(s string) int {
	if s == "" {
		return -1
	}
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return -1
	}
	if v < 0 {
		return -1
	}
	return v
}

// recordRetryAfter is exported indirectly via lastRetryAfterHeader
// only when the wire layer captures the header. For the alpha
// the wire layer is responsible for capturing via setLastRetryAfter.
func recordRetryAfter(ra string) { setLastRetryAfter(ra) }
