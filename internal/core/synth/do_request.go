// do_request.go — wire-aware central HTTP helper for all LLM call sites.
//
// H-1 (post-M3 audit, 2026-08-31): SynthesizeCompactLesson (compact.go:85,90)
// and EvaluateCandidate (admission.go:129,134) hardcoded `/messages` and
// `X-Api-Key` instead of using sc.Wire.path() and sc.Wire.authHeader().
// Synthesize already routed through the dispatch table; the two
// siblings did not. The fix extracts the path + header + retry loop
// into a single helper that all three call.
//
// Why one helper, not "make siblings call the wire methods directly":
// the three sites have different request bodies (compact prompt vs
// synthesis prompt vs admission prompt) and different response shapes
// (string vs SynthResult vs AdmitResult). The body shape is caller
// concern; everything else — path, auth, retry on 5xx, fail-fast on
// 4xx, context cancellation — is shared infrastructure and lives here.
//
// The retry contract is one retry on 5xx with a 3s delay. 4xx and
// non-retryable transport errors fail immediately (admission was
// previously a single attempt — adding retry here is a behaviour
// parity win, not a regression).
package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DoLLMRequest centralizes the wire-aware HTTP path/header selection
// and retry behaviour for every LLM call site in the package. Returns
// the raw response body on success (the caller parses the wire-specific
// envelope via ParseResponseBody).
//
// Path is taken from sc.Wire.path() (Anthropic: /messages,
// OpenAI: /chat/completions). Authorization header is taken from
// sc.Wire.authHeader(apiKey) (Anthropic: X-Api-Key, OpenAI:
// Authorization: Bearer). Both are inferred once at NewSynthClient
// time from BaseURL.
//
// Retry policy: one retry on 5xx (with 3s backoff, ctx-aware),
// fail-fast on 4xx and transport errors.
//
// Exported because admission.go (internal/core package) needs to call
// it; an unexported method on SynthClient cannot be reached across
// the package boundary. The export is the structural fix for H-1
// (post-M3 audit, 2026-08-31): three call sites in two packages
// converge on one helper, so future wire variants land in one place.
func (sc *SynthClient) DoLLMRequest(ctx context.Context, body map[string]interface{}) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal llm request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= 1; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST",
			sc.BaseURL+sc.Wire.path(), bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		authName, authValue := sc.Wire.authHeader(sc.APIKey)
		req.Header.Set(authName, authValue)

		client := &http.Client{Timeout: sc.Timeout}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("API request failed: %w", err)
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		if resp.StatusCode == 200 {
			return respBody, nil
		}

		// Retry once on server errors (5xx), bail on everything else.
		if attempt == 0 && resp.StatusCode >= 500 && resp.StatusCode < 600 {
			lastErr = fmt.Errorf("API returned HTTP %d: %s", resp.StatusCode, string(respBody))
			continue
		}
		return nil, fmt.Errorf("API returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil, fmt.Errorf("API request failed after retries: %w", lastErr)
}
