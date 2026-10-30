package config

// timeout.go — the ONE definition of the LLM request timeout's default and
// the ONE validation policy that decides what a configured value means.
//
// WHY THIS FILE EXISTS
// -------------------
// The request timeout used to be decided in three places at once:
//
//   - `internal/core/synth/client.go` (NewSynthClient) — a literal
//     `300 * time.Second`;
//   - `internal/core/synth/client.go` (NewClientForProfile) — a second
//     literal `300 * time.Second`;
//   - `internal/core/orchestration/model_factory.go` — a third literal.
//
// and the onboarding wizard asked the operator to type a number, which
// then had to agree with all three. Four places, one meaning, no way to
// keep them in agreement.
//
// It now has one home: `DefaultTimeoutSeconds` here, and one policy:
// `EffectiveTimeoutSeconds` here. Every reader goes through the policy.
//
// THE POLICY
// ----------
//
//   - a POSITIVE configured value is used exactly as configured;
//   - ZERO, NEGATIVE or ABSENT falls back to `DefaultTimeoutSeconds`.
//
// Why absent and zero are treated the same: `timeout_seconds` is an
// `omitempty` integer, so "absent" and "explicitly 0" are
// indistinguishable once the file is parsed, and neither is a
// meaningful request timeout.
//
// Why a negative value is not honoured: a negative duration is not a
// timeout, it is an inverted one. Passing it to `http.Client{Timeout:}`
// would mean "no timeout" (Go treats a non-positive Timeout as
// unlimited) — so an operator who typed `-5` would silently get an
// unbounded request rather than the error they expected.
//
// A negative value is therefore INVALID, and the documented consequence
// is that it is replaced by the default. `mpm config set` and
// `mpm config profile set` reject a non-positive value at the point of
// entry, so this fallback only ever applies to a hand-edited JSON file.
//
// WHAT THIS IS NOT
// ----------------
//
// This is the MODEL REQUEST timeout. It is deliberately separate from the
// CONNECTION-VERIFICATION timeouts the probe uses (12s for a Doctor
// probe, 1.5s for a post-save probe), which are properties of the
// probe, not of the provider. A slow provider must be reported as a
// timeout by the probe, never as an authentication failure — the probe
// classifies `context.DeadlineExceeded` as a timeout and the HTTP 401/403
// response as an auth failure, and the two never share a code path.
//
// The operator can still set the field by hand:
//
//	"timeout_seconds": 300
//
// It is an advanced setting, not an onboarding decision.

// DefaultTimeoutSeconds is the substrate's default LLM request timeout,
// in seconds.
//
// 300 seconds is long enough for a large generation on a slow provider
// and short enough that a hung request does not hold a slot forever. It
// is the value the example configuration in docs/CONFIGURATION.md
// carries, and the value a freshly-configured profile is written with.
const DefaultTimeoutSeconds = 300

// EffectiveTimeoutSeconds resolves the request timeout to the number of
// seconds that will actually be used.
//
// See the file comment for the policy. The short form:
//
//	positive value → used as-is
//	zero / negative / absent → DefaultTimeoutSeconds
//
// It is the ONLY sanctioned way to turn a configured value into a
// duration. A caller that reads a raw `TimeoutSecs` and uses it
// directly has reimplemented the policy, and has probably got the
// negative case wrong.
func EffectiveTimeoutSeconds(configured int) int {
	if configured > 0 {
		return configured
	}
	return DefaultTimeoutSeconds
}

// TimeoutSecondsIsValid reports whether a configured value is a usable
// request timeout.
//
// Only a positive value is valid. Zero and negative values are
// invalid, and `EffectiveTimeoutSeconds` replaces them with the
// default.
func TimeoutSecondsIsValid(configured int) bool {
	return configured > 0
}
