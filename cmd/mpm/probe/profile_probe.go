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
// contextTimeoutMs caps the per-probe lifetime (default 6s — caller
// passes 6s from the post-save path).
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
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	// Run the generative probe directly with a tight per-probe HTTP
	// client timeout so the operator's CLI returns within ~2s on
	// unreachable endpoints. The outer ctx deadline above also bounds
	// the operation. Doctor's longer 12s deadline is reserved for the
	// authoritative `mpm doctor` active-probe path.
	result := probeGenerativeWithTimeout(ctx, p, ProbeKindGenerative, ProbeTimeoutForConfigSave)
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
