// Package probe implements the canonical model connectivity / inference
// health probe for MPM. It reuses the production wire helpers (synth.DoLLMRequest
// for generation, the buildProvider factory in internal/core/embeddings for
// embeddings) so Doctor proves MPM's real configured inference path works.
//
// Public surface (deliberately two entry points, no hybrid cache-then-probe):
//
//   - ReadCachedHealth(cfg) — pure read of system_config[model_probe_results].
//     Used by `mpm` no-args; never performs network IO.
//   - RunActiveProbes(ctx, cfg) — concurrent real-network probes of every
//     effective binding. Used by `mpm doctor`. Persists bounded results
//     after `errgroup.Wait()` (single atomic merge).
//
// Status vocabulary is the small presentation enum below. Failures preserve
// the production FailureClass verbatim in the `error_class` field so the
// presentation never loses truth.
//
// Probe cache lives at system_config[model_probe_results] — bounded diagnostic
// state, NOT a cognitive artifact (no memories / lessons / work writes).
// `mpm` no-args never writes to it; only Doctor and config-time verification do.
package probe

import (
	"time"
)

// ProbeStatus is the canonical presentation enum surfaced by Doctor and the
// dashboard. It is intentionally small. The richer FailureClass string lives
// in ProbeResult.ErrorClass so production-side truth is never lost.
type ProbeStatus int

const (
	// ProbeHealthy: a real inference / embedding call succeeded through the
	// production adapter with a non-empty, parseable result.
	ProbeHealthy ProbeStatus = iota
	// ProbeAuthFailed: provider refused the request with HTTP 401/403.
	ProbeAuthFailed
	// ProbeModelNotFound: provider response positively indicates the requested
	// model is missing (typed error or explicit field). A generic 404 does
	// NOT map here — see classify.go.
	ProbeModelNotFound
	// ProbeUnreachable: transport failure (DNS / TCP / TLS / connect).
	ProbeUnreachable
	// ProbeTimeout: context deadline exceeded.
	ProbeTimeout
	// ProbeInvalidResponse: malformed provider response (parse fail / empty
	// body / empty Content / NaN-Inf embedding vector).
	ProbeInvalidResponse
	// ProbeDisabled: the binding is intentionally disabled (e.g.
	// Components["embedding"] == "disabled"). Never a failure.
	ProbeDisabled
	// ProbeUnknown: anything not confidently classified; preserves truth in
	// ErrorClass (e.g. FailureRateLimit, FailureUnknown).
	ProbeUnknown
)

// String returns the canonical lowercase label used in JSON, cache rows,
// and rendered output. Keep stable — diagnostic surfaces depend on it.
func (s ProbeStatus) String() string {
	switch s {
	case ProbeHealthy:
		return "healthy"
	case ProbeAuthFailed:
		return "auth_failed"
	case ProbeModelNotFound:
		return "model_not_found"
	case ProbeUnreachable:
		return "unreachable"
	case ProbeTimeout:
		return "timeout"
	case ProbeInvalidResponse:
		return "invalid_response"
	case ProbeDisabled:
		return "disabled"
	case ProbeUnknown:
		return "unknown"
	}
	return "unknown"
}

// CachedProbe is the persistent shape stored under
// system_config[model_probe_results]. It carries connection-health facts only —
// routing labels (Components, Capabilities) are derived at render time from
// the live config. A profile may be re-routed (e.g. memory → router becomes
// planner → router) without invalidating cached health.
//
// No `Hits` field — mpm no-args is read-only on this row. No secret material
// (API key, URL userinfo, query params, fragments) is persisted.
type CachedProbe struct {
	Fingerprint  string `json:"fingerprint"`             // hex SHA-256 over exact material connection fields (transient input only)
	Provider     string `json:"provider"`                // ollama / openai / anthropic / openrouter / custom / ...
	Model        string `json:"model"`                   // operator-set model id
	BaseURLSafe  string `json:"base_url_safe"`           // sanitized URL (userinfo / query / fragment stripped)
	Status       string `json:"status"`                  // see ProbeStatus.String
	LatencyMs    int64  `json:"latency_ms"`              // wall clock elapsed
	ErrorClass   string `json:"error_class,omitempty"`   // synth.FailureClass string (preserves truth)
	ErrorSummary string `json:"error_summary,omitempty"` // bounded human-readable summary, ≤160 chars, sanitized
	CheckedAt    int64  `json:"checked_at"`              // unix seconds
}

// ProbeResult is the in-memory shape returned by RunActiveProbes. It carries
// routing labels (Components, Capabilities) for direct per-call consumers
// (Doctor rendering). These labels are NOT written back to system_config.
type ProbeResult struct {
	Fingerprint  string    // canonical key, derived from connection fields
	Kind         ProbeKind // generative or embedding
	Components   []string  // component names binding to this profile
	Capabilities []string  // capability labels ultimately pointing to these components
	Provider     string    // canonical provider id from the binding
	Model        string    // model id from the binding
	BaseURL      string    // material base url from the binding (transient; for sanitization)
	BaseURLSafe  string    // sanitized for display / persistence
	Status       ProbeStatus
	LatencyMs    int64
	ErrorClass   string // synth.FailureClass — preserves truth
	ErrorSummary string // bounded ≤160 chars, no secrets
	CheckedAt    time.Time
	SourceCached bool // true when produced from ReadCachedHealth; false when fresh
}

// ProbeKind discriminates which production path the probe invoked.
type ProbeKind int

const (
	ProbeKindGenerative ProbeKind = iota
	ProbeKindEmbedding
)

// SystemConfigKey is the canonical key under which probe results are stored
// in the system_config table. Public so tests, callers, and migration
// tooling can refer to the same name.
const SystemConfigKey = "model_probe_results"

// DefaultMaxCachedRecords bounds the cache size in system_config. With O(≤8)
// bound profiles in a typical MPM install, 64 is comfortably unbounded; the
// bound exists only to defend against misconfigured installs that produce
// unbounded fingerprints.
const DefaultMaxCachedRecords = 64

// DefaultFreshnessTTL is the maximum age (since checked_at) at which a cached
// record is considered "fresh" by ReadCachedHealth. Older records are tagged
// stale but still returned (so `mpm` can render an explicit `?` indicator
// rather than falsely green).
const DefaultFreshnessTTL = 5 * time.Minute
