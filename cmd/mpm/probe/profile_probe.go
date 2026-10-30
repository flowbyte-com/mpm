// profile_probe.go — Single-profile probe used by the post-save
// verification path in `handleProfileAdd` / `handleProfileSet`. The
// profile does NOT need to be bound to a component — the probe exercises
// the configured connection directly through the production adapter.
//
// This is the small per-profile surface that complements RunActiveProbes
// (which iterates effective routing). RunActiveProbes is for `mpm doctor`;
// ProbeSingle is for the post-save UX line — its result is fresh and
// informative regardless of whether the operator has bound the profile yet.

package probe

import (
	"context"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
)

// ProbeSingle runs the canonical generative + embedding probes against
// a single profile and persists the result to system_config[model_probe_results].
// The caller may pass nil for dm to skip persistence.
//
// The probe is best-effort: it returns (ProbeResult, error). The error
// is non-nil only for transport-level failures that prevent the call from
// returning a structured result. The Caller of probeAfterSet handles
// rendering; the on-disk error is logged in Status / ErrorClass for
// later Doctor inspection.
//
// THE DEADLINE
// ------------
// A verification is bounded by TWO numbers, both owned by this package:
//
//   - ProbeTimeoutForVerification bounds ONE request;
//   - ProbeTotalBudget bounds the WHOLE verification, across the first
//     attempt and the one bounded recovery attempt it may make.
//
// When the caller supplies NO deadline, this function applies the total
// budget itself, so the two numbers cannot drift apart at the call site.
// When the caller DOES supply a deadline, that deadline wins — it is an
// explicit choice by the caller — but it must not be shorter than
// ProbeTimeoutForVerification, or the verification is truncated below
// the bound it is supposed to be measured against.
//
// The total budget is deliberately LARGER than the per-request bound.
// If the outer deadline were shorter, the outer one would fire first and
// the probe would report a bare context deadline instead of the
// provider's own client timeout — the same failure in a different
// message.
func ProbeSingle(ctx context.Context, dm *mpminternal.DatabaseManager, p *config.Profile) (ProbeResult, error) {
	if !CanProbe(p) {
		return ProbeResult{
			Kind:     ProbeKindGenerative,
			Provider: "", Model: "", BaseURL: "", BaseURLSafe: "",
			Status:       ProbeUnknown,
			ErrorSummary: "profile not executable",
			CheckedAt:    time.Now(),
		}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, cancel = context.WithTimeout(ctx, ProbeTotalBudget)
		defer cancel()
	}
	// The per-request bound is the PROBE's own, never the profile's
	// `timeout_seconds`: an operator who tuned the request timeout for
	// a long generation must not thereby make every verification fail,
	// and must never be told their credential was invalid when the
	// endpoint simply answered late.
	result := probeGenerativeWithTimeout(ctx, p, ProbeKindGenerative, ProbeTimeoutForVerification)
	if dm != nil {
		existing, _ := LoadCache(dm)
		current := map[string]struct{}{result.Fingerprint: {}}
		if result.Fingerprint == "" {
			current = map[string]struct{}{}
		}
		merged := MergeCache(existing, []ProbeResult{result}, current)
		_ = SaveCache(dm, merged)
	}
	return result, nil
}
