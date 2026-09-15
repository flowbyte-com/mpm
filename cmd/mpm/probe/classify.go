// classify.go — Map wire / transport / adapter signals onto the small
// ProbeStatus presentation enum. The richer production failure class
// (synth.FailureClass) is preserved in the result's ErrorClass field.

package probe

import (
	"context"
	"errors"
	"net"
	"strings"
)

// classifyFromWire maps the production wire / transport failure signals onto
// ProbeStatus. err is the error returned by synth.DoLLMRequest or
// embeddings.Embed; class is the production FailureClass string (one of
// "FailureAuth", "FailureRateLimit", "FailureTransientTransport",
// "FailureInvalidMachineResponse", "FailureUnknown", ""). httpStatus is the
// raw HTTP status code when known (>0). bodyFragment is a bounded slice
// (≤512 bytes) of the response body used only for model-not-found detection.
//
// httpStatus ≤ 0 means transport failed before any HTTP response. In that
// case class is typically "FailureTransientTransport", but a DNS / connect
// failure can also surface with class=="" if the underlying client returns
// the error before classifyFromStatus runs.
//
// We classify model_not_found only when provider evidence indicates the
// requested model is missing — typed error or explicit field. A generic
// HTTP 404 with no model evidence becomes ProbeUnknown.
func classifyFromWire(err error, class string, httpStatus int, bodyFragment string) ProbeStatus {
	// Transport-level signals first.
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ProbeTimeout
		}
		// DNS / connection refused / TLS handshake / EOF without HTTP.
		if httpStatus <= 0 {
			return ProbeUnreachable
		}
		// Transport error after the HTTP exchange started — usually means
		// the connection broke mid-stream. Treat as unreachable.
		var ne net.Error
		if errors.As(err, &ne) {
			return ProbeUnreachable
		}
	}

	// HTTP-derived signals.
	switch class {
	case "FailureAuth":
		// HTTP 401 / 403 / 402 (intentionally excluded from ProbeAuthFailed
		// per the spec — but FailureAuth is only set for 401/403 by
		// classifyFromStatus, so this never triggers on 402).
		return ProbeAuthFailed
	case "FailureInvalidMachineResponse":
		// Parse fail / empty body / empty Content / NaN-Inf vector.
		return ProbeInvalidResponse
	case "FailureTransientTransport":
		return ProbeUnreachable
	}

	// HTTP-status-derived signals for cases that didn't classify into the
	// production FailureClass enum (e.g. a 5xx with no body).
	switch {
	case httpStatus == 401 || httpStatus == 403:
		return ProbeAuthFailed
	case httpStatus == 429:
		// Rate-limited: classification is preserved in ErrorClass;
		// presentation uses ProbeUnknown because we have no auth-shaped
		// failure semantics to surface here.
		return ProbeUnknown
	case httpStatus >= 500:
		return ProbeUnknown
	case httpStatus == 404:
		// Distinguish real "model not found" from generic 404.
		if looksLikeMissingModel(bodyFragment) {
			return ProbeModelNotFound
		}
		return ProbeUnknown
	}

	// Fallback — default to unknown rather than fabricating certainty.
	return ProbeUnknown
}

// looksLikeMissingModel reports whether the bounded body fragment contains
// provider evidence that the requested model is missing (not just any 404).
// Substring match is bounded to ≤512 bytes by the caller.
//
// Examples that match:
//
//	`{"error":{"code":"model_not_found",...}}`
//	`{"type":"error","error":{"type":"not_found_error","message":"model: foo"}}`
//	`404 model 'foo' not found`
//
// Bare `not found` without "model" wording does NOT match — too noisy.
func looksLikeMissingModel(fragment string) bool {
	if fragment == "" {
		return false
	}
	lower := strings.ToLower(fragment)
	if strings.Contains(lower, `"code":"model_not_found"`) ||
		strings.Contains(lower, `"type":"model_not_found"`) ||
		strings.Contains(lower, "model_not_found") ||
		strings.Contains(lower, "model not found") {
		return true
	}
	// Anthropic uses `not_found_error` + model wording in the message.
	if strings.Contains(lower, `"type":"not_found_error"`) && strings.Contains(lower, "model") {
		return true
	}
	return false
}

// webSearchLabel returns the canonical lowercase label for a ProbeStatus.
// Equivalent to ProbeStatus.String() — kept as an alias for readability
// in classify.go callers.
func webSearchLabel(s ProbeStatus) string { return s.String() }
