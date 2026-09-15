// embedding.go — Probe via the production EmbeddingProvider factory
// (mpminternal.buildProvider). The factory picks the right adapter
// (OllamaProvider for provider=ollama, OpenAICompatibleProvider for
// openai-compatible/openai/openrouter/custom) using the same dispatch
// production embedding calls use. NO parallel HTTP code path.
//
// We classify the embedding result on:
//   - non-empty vector
//   - all values finite (no NaN/Inf)
//   - dimensional check is deferred (today's contract does not encode a
//     known dimension; we accept whatever the provider returns)

package probe

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
)

// probeEmbedText is the canonical input for embedding probes. Tiny, fixed,
// deterministic.
const probeEmbedText = "mpm probe"

// probeEmbedding executes a single embedding probe against a profile,
// routed through the production factory. Returns the canonical
// ProbeResult shape; Status, LatencyMs, ErrorClass, ErrorSummary all set
// per the production failure surface (Ollama returns
// `embed: Ollama returned <code>: <body>` and similar; OpenAI-Compatible
// returns `embed: request failed` / `decode:` style errors).
func probeEmbedding(ctx context.Context, p *config.Profile) ProbeResult {
	start := time.Now()

	if p == nil || p.Provider == "" || p.Model == "" || p.BaseURL == "" {
		return ProbeResult{
			Kind:         ProbeKindEmbedding,
			BaseURL:      "",
			BaseURLSafe:  "",
			Status:       ProbeUnknown,
			ErrorSummary: "profile not executable (provider/model/base_url required)",
			CheckedAt:    start,
		}
	}

	provider, err := mpminternal.BuildEmbeddingProvider(p)
	if err != nil || provider == nil {
		latency := time.Since(start)
		return ProbeResult{
			Kind:         ProbeKindEmbedding,
			Provider:     p.Provider,
			Model:        p.Model,
			BaseURL:      p.BaseURL,
			BaseURLSafe:  sanitizeURL(p.BaseURL),
			Fingerprint:  ComputeFingerprint(fingerprintInput{Provider: p.Provider, Model: p.Model, BaseURL: p.BaseURL, Credential: p.APIKey}),
			Status:       ProbeUnknown,
			ErrorClass:   "FailureUnknown",
			ErrorSummary: sanitizeError(fmt.Sprintf("build provider: %v", err)),
			LatencyMs:    latency.Milliseconds(),
			CheckedAt:    start,
		}
	}

	vec, err := provider.Embed(probeEmbedText)
	latency := time.Since(start)

	res := ProbeResult{
		Kind:        ProbeKindEmbedding,
		Provider:    p.Provider,
		Model:       p.Model,
		BaseURL:     p.BaseURL,
		BaseURLSafe: sanitizeURL(p.BaseURL),
		Fingerprint: ComputeFingerprint(fingerprintInput{Provider: p.Provider, Model: p.Model, BaseURL: p.BaseURL, Credential: p.APIKey}),
		LatencyMs:   latency.Milliseconds(),
		CheckedAt:   start,
	}

	if err != nil {
		class, httpStatus := classifyEmbeddingError(err)
		bodyFrag := extractEmbeddingBodyFragment(err)
		res.Status = classifyFromWire(err, class, httpStatus, bodyFrag)
		res.ErrorClass = class
		res.ErrorSummary = sanitizeError(err.Error())
		return res
	}

	// Vector invariants: non-empty, all finite.
	if len(vec) == 0 {
		res.Status = ProbeInvalidResponse
		res.ErrorClass = "FailureInvalidMachineResponse"
		res.ErrorSummary = sanitizeError("embedding provider returned empty vector")
		return res
	}
	for i, v := range vec {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			_ = i
			res.Status = ProbeInvalidResponse
			res.ErrorClass = "FailureInvalidMachineResponse"
			res.ErrorSummary = sanitizeError("embedding vector contained non-finite value")
			return res
		}
	}

	res.Status = ProbeHealthy
	return res
}

// classifyEmbeddingError extracts the production FailureClass and HTTP
// status from an embeddings.Embed error. The internal/core/embeddings.go
// package produces either:
//   - Ollama:    `embed: Ollama returned <n>: <body>`
//   - OpenAI-Compatible: `embed: endpoint returned <n>: <body>`
//   - OpenAI-Compatible: `embed: endpoint returned no embedding for model "..."`
//     (HTTP 200 + empty data — invalid response, not transport failure)
//   - `embed: decode: ...` (parse failure)
//   - `embed: request failed: ...` (transport)
//
// We map each shape onto the canonical ProbeStatus.
func classifyEmbeddingError(err error) (class string, httpStatus int) {
	if err == nil {
		return "", 0
	}
	msg := err.Error()
	// Try both status-code-bearing prefixes.
	prefixes := []string{"Ollama returned ", "endpoint returned "}
	for _, prefix := range prefixes {
		if i := indexOf(msg, prefix); i >= 0 {
			rest := msg[i+len(prefix):]
			// The OpenAI-Compatible empty case has no leading digits —
			// it's `endpoint returned no embedding for model "..."`
			// which is invalid_response, NOT transport failure.
			if rest == "" || rest[0] < '0' || rest[0] > '9' {
				if indexOf(rest, "no embedding for model") >= 0 {
					return "FailureInvalidMachineResponse", 200
				}
				continue
			}
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
				case n == 404:
					class = "FailureUnknown"
				}
				return class, httpStatus
			}
		}
	}
	// Transport / decode variants.
	if indexOf(msg, "request failed") >= 0 || indexOf(msg, "new request") >= 0 {
		class = "FailureTransientTransport"
		return class, 0
	}
	if indexOf(msg, "decode:") >= 0 {
		class = "FailureInvalidMachineResponse"
		return class, 0
	}
	return class, httpStatus
}

// extractEmbeddingBodyFragment pulls ≤512 bytes of provider body from an
// embedding error for model_not_found detection. Handles both Ollama
// and OpenAI-Compatible error prefixes.
func extractEmbeddingBodyFragment(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, prefix := range []string{"Ollama returned ", "endpoint returned "} {
		if i := indexOf(msg, prefix); i >= 0 {
			rest := msg[i+len(prefix):]
			// Skip status digits if present.
			j := 0
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			if j+1 >= len(rest) {
				return ""
			}
			body := rest[j+1:] // skip ": "
			const max = 512
			if len(body) > max {
				body = body[:max]
			}
			return body
		}
	}
	return ""
}

// Sanity: trim is local to this file.
func init() {
	// Strings trim import is used below at boundary.
	_ = strings.TrimSpace
}

// _ silences any cyclic-import / blank-var concerns.
var _ = strings.Contains
var _ = (*config.Profile)(nil)
