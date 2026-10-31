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
	"errors"
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

// ProbeTimeoutForVerification is the SINGLE per-request bound for every
// connection-verification probe.
//
// WHY ONE NUMBER
// --------------
// Two probes existed with two bounds — 12s for Doctor, 1.5s for the
// post-save verification — and they disagreed about the same question:
// "did the endpoint answer?". A bound that is right for one surface is
// wrong for the other, and two numbers are two chances to be wrong.
//
// It is now ONE number, used by both.
//
// WHY 15 SECONDS, AND NOT 1.5
// ---------------------------
// The post-save verification's bound was 1.5 seconds. That is SHORTER
// than the provider's own typical latency, so a VALID credential was
// reported as a timeout. Measured against the live MiniMax
// Anthropic-compatible surface through this canonical probe path
// (7 requests per model, three separate runs):
//
//	MiniMax-M3     min  415ms   median 560-700ms   max 4761ms
//	MiniMax-M2.7   min  444ms   median 603-1007ms  max 2220ms
//
// The slowest healthy call was 4761ms — THREE TIMES the old 1.5s
// bound. A slow-but-valid provider was therefore reported as a
// timeout, and a timeout on a credential surface is what an operator
// reads as "my credential is wrong". That sends them to fix something
// that was never broken.
//
// The tail is the number that matters, and it is heavy: the median is
// well under a second, but a minority of calls take three to five
// seconds. A bound sized to the median would fail half the time.
//
// 15 seconds is 3.1x the worst observed call, and 20x the substrate's
// 300s model-request default. It is BOUNDED: an endpoint that does not
// answer is reported as a timeout after 15 seconds, and the probe
// never waits without a deadline.
//
// The cost of the generous bound is bounded too. An endpoint that
// REFUSES the connection fails immediately — the bound is only spent
// on an endpoint that accepts the connection and then stays silent,
// which is exactly the case where patience is warranted.
//
// 15 seconds also serves the Doctor surface, whose own reason for a
// long bound survives: a cold local model can take 8-10s to
// materialise. One number now serves both.
//
// WHAT IT IS NOT
// --------------
// This is NOT the model-request timeout. `timeout_seconds` is how long
// the substrate waits for a GENERATION; this is how long a verification
// waits for an endpoint to answer a tiny connectivity question. They are
// different knobs, and this bound is never read from the profile — see
// internal/core/config/timeout.go for the request timeout's own policy.
//
// A verification must never wait as long as a generation, and it must
// never be shortened by an operator who tuned `timeout_seconds` for
// their generation workload.
const ProbeTimeoutForVerification = 15 * time.Second

// ProbeTotalBudget is the ceiling on a WHOLE verification, across every
// request the probe may make.
//
// A verification can make at most TWO requests: the first attempt, and
// one bounded recovery — a single alternative authentication scheme, or
// a single transient-transport retry. Each is bounded by
// ProbeTimeoutForVerification, so the ceiling is two bounds plus the
// backoff between them.
//
// The total budget MUST be larger than the per-request bound, and it
// must be the OUTER deadline. If the outer deadline were shorter than
// the per-request bound, the outer one would fire first and the probe
// would report a bare context deadline instead of the provider's own
// client timeout — the same failure, wearing a different message, and
// indistinguishable from it at the surface that renders it.
//
// The budget is exported so the call site does not carry its own
// magic number: the bound and the budget that must exceed it are
// decided in ONE place.
const ProbeTotalBudget = 40 * time.Second

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
	return probeGenerativeWithTimeout(ctx, p, kind, ProbeTimeoutForVerification)
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
	//
	// The client is built through the substrate's single construction
	// point, so the probe resolves exactly what the runtime resolves:
	// the same protocol, the same credential (an explicit profile
	// api_key first, the provider's own environment variable second)
	// and the same authentication scheme. Reading p.APIKey directly
	// here made every environment-only install report a false auth
	// failure from the probe while real synthesis against the same
	// profile succeeded; re-deriving the wire here meant the probe
	// could disagree with the model factory about the protocol.
	sc := synth.NewClientForProfile(*p, timeout, probeMaxTokens)
	// The probe IS the explicitly approved connection verification, so
	// it — and only it — may make the bounded alternative
	// authentication attempt when the provider names a different
	// authentication header. The attempt is bounded to one extra
	// request and consumes the plan's single recovery slot.
	sc.AllowAuthNegotiation = true

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
		// The explicit authentication-scheme override participates in
		// the fingerprint: changing it changes the request that would
		// be made, so a cached result for the previous override must
		// not be presented as current.
		OtherKeyValues: []FingerprintField{{Key: "auth_scheme", Value: p.AuthScheme}},
	})

	// The authentication scheme that was presented. After a
	// successful bounded negotiation this is the scheme that actually
	// worked, which is what the operator needs to pin.
	//
	// An unrecognised operator override resolves to an error: the
	// scheme is left empty rather than reporting a derived scheme the
	// operator never asked for. The request itself fails with the
	// explicit error, which the summary below carries.
	authScheme := ""
	if scheme, schemeErr := sc.AuthScheme(); schemeErr == nil {
		authScheme = scheme.String()
	}

	res := ProbeResult{
		Kind:        kind,
		Provider:    p.Provider,
		Model:       p.Model,
		BaseURL:     p.BaseURL,
		BaseURLSafe: sanitizeURL(p.BaseURL),
		LatencyMs:   latency.Milliseconds(),
		Fingerprint: fp,
		CheckedAt:   start,
		AuthScheme:  authScheme,
	}

	if err != nil {
		// Map the production-side error onto ProbeStatus. SynthClient
		// returns errors that already carry the FailureClass in their
		// message; we don't have direct access to the typed FailureClass
		// here without exposing it. Use the HTTP status / classification
		// heuristics via substring match against the formatted message.
		class, httpStatus := classifySynthError(err)
		// A TYPED authentication refusal overrides that. The status it
		// carries belongs to the attempt that produced the failure: on
		// the negotiated path that is the FINAL attempt, and only the
		// final one describes the failure the operator must act on
		// (402 on the second attempt means "unbilled", not "invalid
		// credential").
		var refusal *synth.AuthRefusalError
		if errors.As(err, &refusal) {
			httpStatus = refusal.Status
			class = authRefusalClass(refusal.Status)
		}
		res.ErrorClass = class
		// Capture a bounded body fragment for model-not-found detection
		// when available. We extract the first 512 chars between
		// `body:` markers when synth wraps the raw body in its error.
		bodyFrag := extractBodyFragment(err)
		res.Status = classifyFromWire(err, class, httpStatus, bodyFrag)
		res.ErrorSummary = sanitizeError(err.Error())
		// Keep an invalid credential, an unbilled subscription and an
		// under-privileged key distinguishable. They are three
		// different problems with three different remedies, and
		// collapsing them into one "auth failed" line is what made the
		// first clean-install report untrustworthy.
		res.AuthFailure = synth.ClassifyAuthFailure(httpStatus).String()
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

// authRefusalClass maps an authentication refusal's HTTP status onto
// the probe's failure-class vocabulary.
//
// The three refusals the operator can act on — an invalid credential, an
// unbilled subscription and an under-privileged key — share one class
// (FailureAuth) and are distinguished separately by AuthFailure.
func authRefusalClass(status int) string {
	switch {
	case status == 401 || status == 402 || status == 403:
		return "FailureAuth"
	case status == 429:
		return "FailureRateLimit"
	case status >= 500:
		return "FailureUnknown"
	case status == 0:
		// The attempt never reached an HTTP response.
		return "FailureTransientTransport"
	}
	return "FailureUnknown"
}

// classifySynthError extracts the production FailureClass and HTTP status
// from a synth.DoLLMRequest error.
//
// The production layer hands the status over as a TYPED field
// (synth.AuthRefusalError) whenever the refusal was an authentication
// refusal. That field is authoritative, and the caller reads it — see
// probeGenerative, which consults it before trusting this function's
// message parsing.
//
// The message parsing below is the fallback for the failure shapes the
// production layer does not type. On the negotiated path it now agrees
// with the typed field: the message names the FINAL attempt's status in
// the standard "API returned HTTP <n>:" form, and the initial attempt's
// status is named without that prefix, so there is exactly one
// occurrence to find.
func classifySynthError(err error) (class string, httpStatus int) {
	if err == nil {
		return "", 0
	}
	// A deliberate credential-redirect refusal is recognised through the
	// typed sentinel rather than through a substring of the message. It
	// is NOT a transport failure: the endpoint answered, MPM declined
	// to follow the redirect, and the refusal's own message must reach
	// the operator instead of being reported as "unreachable".
	if errors.Is(err, synth.ErrCredentialRedirectRefused) {
		return "FailureCredentialRedirectRefused", 0
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
			case n == 402:
				// 2026-10-23 (HAL-9001): the production classifier
				// (synth.classifyFromStatus) maps 401 / 402 / 403 to
				// FailureAuth. This duplicate of it omitted 402, so
				// the probe reported an EMPTY ErrorClass for a billing
				// refusal and the operator's report had no truthful
				// class at all. Keep the two classifiers in agreement.
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
//
// The LAST occurrence is used, not the first: on the negotiated path the
// message names both attempts, and the body that describes the failure
// the operator must act on is the final attempt's.
func extractBodyFragment(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	prefix := "API returned HTTP "
	i := lastIndexOf(msg, prefix)
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

// lastIndexOf is the mirror of indexOf: it returns the position of the
// LAST occurrence of needle, or -1.
//
// The negotiated authentication path names both attempts in one message,
// so the final attempt's answer is the one that describes the failure
// the operator has to act on.
func lastIndexOf(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := len(haystack) - len(needle); i >= 0; i-- {
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
