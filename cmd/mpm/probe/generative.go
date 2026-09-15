// generative.go — Perform a real inference probe via the canonical
// synth.SynthClient.DoLLMRequest path. NO duplicated request builders, no
// duplicated auth header handling, no duplicated response parsing, no
// parallel Ollama HTTP path.
//
// The probe constructs a tiny body (max_tokens = probeMaxTokens), POSTs it
// through DoLLMRequest (the same wire helper the synthesis worker uses),
// and parses via the production ParseResponseBody. An HTTP 200 with empty
// Content is invalid_response — successful inference must produce usable
// output.

package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/synth"
)

// probeMaxTokens bounds the model output for a generative probe. We want
// enough tokens for a tiny reply ("OK") plus some margin for provider-side
// formatting / tool-call envelopes; far below the substrate's
// synthInternalMaxTokens (16384) which is designed for synthesis jobs that
// need to produce full content.
const probeMaxTokens = 16

// probeUserPrompt is the fixed deterministic instruction used for generative
// probes. We do NOT depend on the literal "OK" reply — we depend on the
// provider returning parseable non-empty content through the production
// parser. The instruction is short and directive to minimise the chance of
// tool-call envelopes, reasoning tokens, or other completions that might
// fail the non-empty check.
const probeUserPrompt = "Reply with exactly two words: OK READY"

// ProbeTimeoutForDoctor is the per-probe timeout for Doctor active probes.
// Longer than the original 6s to absorb cold local-model warm-up (cold
// Ollama / LM Studio can take 8-10s for first-load model materialisation).
const ProbeTimeoutForDoctor = 12 * time.Second

// ProbeTimeoutForConfigSave is a tight per-probe timeout for the post-save
// probe — the operator's `mpm config profile set/add` command must NOT
// block visibly on an unreachable endpoint. If the endpoint doesn't
// answer quickly, we surface timeout as the failure class and return
// promptly so the operator's CLI returns within ~2s. Real inference
// health is verified via `mpm doctor`, which uses the longer Doctor
// deadline above.
const ProbeTimeoutForConfigSave = 1500 * time.Millisecond

// probeGenerative executes a single generative probe against a profile
// resolved through the live config. It:
//
//  1. Reads provider / model / base_url / api_key from the profile
//  2. Builds a tiny probe-specific SynthClient (same wire dispatch as
//     production, with max_tokens overridden for the probe size)
//  3. POSTs the probe body through synth.DoLLMRequest (production wire
//     helper — URL, auth header, response parser all canonical)
//  4. Calls synth.ParseResponseBody to validate the wire envelope
//  5. Inspects the resulting parsed text for non-empty content (empty
//     content is `ProbeInvalidResponse`, per spec)
//
// The returned ProbeResult has Status classified, ErrorClass preserved
// from the production FailureClass, and ErrorSummary sanitized.
//
// `dm` may be nil — the probe is hermetic on the network side and does
// not touch the database.
func probeGenerative(ctx context.Context, p *config.Profile, kind ProbeKind) ProbeResult {
	return probeGenerativeWithTimeout(ctx, p, kind, ProbeTimeoutForDoctor)
}

// probeGenerativeWithTimeout is the variant that lets callers override
// the per-probe HTTP client timeout. The shorter of (timeout) and the
// caller's context deadline wins.
func probeGenerativeWithTimeout(ctx context.Context, p *config.Profile, kind ProbeKind, timeout time.Duration) ProbeResult {
	start := time.Now()

	if p == nil || p.Provider == "" || p.Model == "" || p.BaseURL == "" {
		return ProbeResult{
			Kind:         kind,
			Provider:     "",
			Model:        "",
			BaseURL:      "",
			BaseURLSafe:  "",
			Status:       ProbeUnknown,
			ErrorSummary: "profile not executable (provider/model/base_url required)",
			CheckedAt:    start,
		}
	}

	// Build a probe-specific SynthClient with probeMaxTokens.
	sc := &synth.SynthClient{
		Model:     p.Model,
		APIKey:    p.APIKey,
		BaseURL:   p.BaseURL,
		Timeout:   timeout,
		MaxTokens: probeMaxTokens,
		Wire:      synth.InferWire(p.BaseURL),
	}

	body := map[string]interface{}{
		"model":       p.Model,
		"max_tokens":  probeMaxTokens,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "user", "content": probeUserPrompt},
		},
	}

	raw, err := sc.DoLLMRequest(ctx, body)
	latency := time.Since(start)

	fp := ComputeFingerprint(fingerprintInput{
		Provider:   p.Provider,
		Model:      p.Model,
		BaseURL:    p.BaseURL,
		Credential: p.APIKey,
	})

	res := ProbeResult{
		Kind:        kind,
		Provider:    p.Provider,
		Model:       p.Model,
		BaseURL:     p.BaseURL,
		BaseURLSafe: sanitizeURL(p.BaseURL),
		LatencyMs:   latency.Milliseconds(),
		Fingerprint: fp,
		CheckedAt:   start,
	}

	if err != nil {
		// Map the production-side error onto ProbeStatus. SynthClient
		// returns errors that already carry the FailureClass in their
		// message; we don't have direct access to the typed FailureClass
		// here without exposing it. Use the HTTP status / classification
		// heuristics via substring match against the formatted message.
		class, httpStatus := classifySynthError(err)
		res.ErrorClass = class
		// Capture a bounded body fragment for model-not-found detection
		// when available. We extract the first 512 chars between
		// `body:` markers when synth wraps the raw body in its error.
		bodyFrag := extractBodyFragment(err)
		res.Status = classifyFromWire(err, class, httpStatus, bodyFrag)
		res.ErrorSummary = sanitizeError(err.Error())
		return res
	}

	// Parse the wire envelope using the production response parser.
	text, perr := sc.ParseResponseBody(raw, "probe")
	if perr != nil {
		// Parse failure or empty envelope — classify as invalid_response.
		// We do NOT have a meaningful HTTP status (request succeeded) but
		// the wire envelope is unusable.
		res.Status = ProbeInvalidResponse
		res.ErrorClass = "FailureInvalidMachineResponse"
		res.ErrorSummary = sanitizeError(perr.Error())
		return res
	}

	// Wire envelope parsed. Now require usable non-empty completion.
	if len(text) == 0 {
		res.Status = ProbeInvalidResponse
		res.ErrorClass = "FailureInvalidMachineResponse"
		res.ErrorSummary = sanitizeError("provider returned empty completion")
		return res
	}
	trimmed := trimAllWhitespace(text)
	if len(trimmed) == 0 {
		res.Status = ProbeInvalidResponse
		res.ErrorClass = "FailureInvalidMachineResponse"
		res.ErrorSummary = sanitizeError("provider returned blank completion")
		return res
	}

	res.Status = ProbeHealthy
	return res
}

// classifySynthError extracts the production FailureClass and HTTP status
// from a synth.DoLLMRequest error. synth wraps failures as
// `API returned HTTP <code>: <body>` or `API request failed: <reason>`.
// We surface the human-readable message into ErrorSummary (sanitized) and
// recover the structured class via regex / prefix match.
func classifySynthError(err error) (class string, httpStatus int) {
	if err == nil {
		return "", 0
	}
	msg := err.Error()
	// Standard synth error format: `API returned HTTP <n>: <body>`.
	var httpPrefix = "API returned HTTP "
	if i := indexOf(msg, httpPrefix); i >= 0 {
		rest := msg[i+len(httpPrefix):]
		// rest starts with the status code as digits.
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j > 0 {
			n := 0
			for _, c := range rest[:j] {
				n = n*10 + int(c-'0')
			}
			httpStatus = n
			switch {
			case n == 401 || n == 403:
				class = "FailureAuth"
			case n == 429:
				class = "FailureRateLimit"
			case n >= 500:
				class = "FailureUnknown"
			}
			return class, httpStatus
		}
	}
	// Transport-level failure: `API request failed: ...`.
	if indexOf(msg, "API request failed") >= 0 {
		class = "FailureTransientTransport"
		return class, 0
	}
	// Build / marshal failures.
	if indexOf(msg, "build request") >= 0 || indexOf(msg, "marshal llm request") >= 0 {
		class = "FailureUnknown"
		return class, 0
	}
	// Recovery failure: typed error string includes `on recovery attempt`.
	if indexOf(msg, "recovery attempt") >= 0 {
		class = "FailureUnknown"
		return class, httpStatus
	}
	return class, httpStatus
}

// extractBodyFragment pulls at most 512 bytes of provider response body
// from a synth-formatted error message. Used to detect model_not_found
// without holding the full response in memory.
//
// synth wraps raw bodies inline: e.g.
//
//	"API returned HTTP 404: {\"error\":{\"code\":\"model_not_found\"}}"
//
// We take everything after the first colon of `API returned HTTP <n>:`,
// bounded to 512 chars.
func extractBodyFragment(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	prefix := "API returned HTTP "
	i := indexOf(msg, prefix)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(prefix):]
	// Skip the status digits.
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j >= len(rest) {
		return ""
	}
	body := rest[j+1:] // skip `:`
	const max = 512
	if len(body) > max {
		body = body[:max]
	}
	return body
}

// trimAllWhitespace strips leading / trailing whitespace including \r \n.
func trimAllWhitespace(b []byte) []byte {
	start := 0
	end := len(b)
	for start < end && isWhitespace(b[start]) {
		start++
	}
	for end > start && isWhitespace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// indexOf is a tiny strings.Index replacement to avoid an import cycle
// when this file is consumed from internal/core packages (probe package is
// cmd/mpm/probe — internal pkg imports would not invert that, but keeping
// the dependency surface minimal helps).
func indexOf(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// Ensure mpminternal is referenced so imports don't go stale under
// partial-edit tooling.
var _ mpminternal.CoreDB // type-only reference; package import retained
var _ = http.StatusOK
var _ = io.EOF
var _ = fmt.Errorf
