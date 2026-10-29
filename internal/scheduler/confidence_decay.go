// internal/scheduler/confidence_decay.go
//
// Per-tick confidence-decay maintenance (issue #92).
//
// The substrate models confidence decay over elapsed time, but the
// stored value only moves when something recomputes it. A bounded
// pass lives in internal/core (RunConfidenceDecay); this handler is the
// automatic trigger. It is deliberately small: one registered tick
// handler, its own cadence gate in system_config, and no new scheduler
// machinery.
//
// Cadence keys (all configurable through system_config, env fallback
// MPM_CONFIDENCE_DECAY_* — see the precedence note below):
//
//	confidence.decay.enabled         0 = off (DEFAULT), 1 = on
//	confidence.decay.interval_hours  6   minimum time between passes
//	confidence.decay.limit           100 artifacts per pass
//	confidence.decay.min_age_hours   24  artifacts younger than this
//	                                     (or checked more recently) are skipped
//
// THE APPROVED POLICY (issue #92, operator decision 2026-10-29)
//
// The pass is OFF by default and runs only on an explicit opt-in. A
// substrate that decays confidence unattended changes the value a
// retrieval ranking sees, without anyone asking — and the defaults that
// made it run were selected silently, with no operator decision recorded
// anywhere. The four numbers above are the operator's decision, and they
// live in internal/core (DefaultConfidenceDecayEnabled,
// DefaultConfidenceDecayInterval, DefaultConfidenceDecayLimit,
// DefaultConfidenceDecayMinAge) so that the scheduler, the documentation
// and the tests cannot drift apart.
//
// HOW TO ENABLE IT
//
//  1. mpm ops confidence decay --dry-run
//     See what a pass would do before letting one run unattended.
//
//  2. Set confidence.decay.enabled = 1 in system_config.
//
//     Step 2 can be skipped when the key is absent from system_config and
//     the scheduler's environment carries MPM_CONFIDENCE_DECAY_ENABLED=1
//     instead. The environment is the weaker of the two: it is consulted
//     only when the key is absent.
//
// HOW TO DISABLE IT
//
//	Set confidence.decay.enabled = 0 in system_config, or unset the
//	environment variable. An explicit 0 in system_config WINS over the
//	environment variable — see the precedence note below.
//
// CONFIGURATION PRECEDENCE (and why it is the way it is)
//
//	DatabaseManager.GetConfigInt reads system_config FIRST, then the
//	environment, then the built-in default. So an ABSENT key means "the
//	operator never chose" and takes the default (off); an EXPLICIT value
//	in system_config always wins, including an explicit 0, so the
//	operator's setting survives an upgrade; and the environment variable
//	is consulted only when the key is absent, so it is a FALLBACK, not an
//	override. (The comment above used to call it an override, which was
//	wrong: an operator who exported MPM_CONFIDENCE_DECAY_ENABLED=1
//	against an install that already had confidence.decay.enabled = 0
//	would have been ignored, silently.)
//
// WHAT AN UPGRADE CHANGES
//
//	Nothing is written to system_config by the upgrade, so nothing can be
//	clobbered. An install that never chose a value had NO row for
//	confidence.decay.enabled, and therefore starts OFF after the upgrade.
//	That is the intended consequence of the decision: the old default was
//	selected silently, so there is no operator intent to preserve. An
//	install that DID choose — enabled=1 or enabled=0 — keeps its choice.
//
// The tick fires every scheduler interval (60s by default); the gate
// makes the pass run at most once per interval_hours. The last-run
// marker advances even when individual artifacts fail, so a broken
// artifact cannot turn the scheduler into a busy loop — it is retried on
// the next scheduled pass, and the failure is logged and returned.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

const (
	confidenceDecayEnabledKey  = "confidence.decay.enabled"
	confidenceDecayIntervalKey = "confidence.decay.interval_hours"
	confidenceDecayLimitKey    = "confidence.decay.limit"
	confidenceDecayMinAgeKey   = "confidence.decay.min_age_hours"
)

// ConfidenceDecayHandler runs the bounded decay pass on a cadence.
type ConfidenceDecayHandler struct {
	dm     *core.DatabaseManager
	logger *slog.Logger
	now    func() time.Time
}

// NewConfidenceDecayHandler constructs the handler. Tests override the
// clock through the unexported `now` field.
func NewConfidenceDecayHandler(dm *core.DatabaseManager, logger *slog.Logger) *ConfidenceDecayHandler {
	return &ConfidenceDecayHandler{dm: dm, logger: logger, now: time.Now}
}

// TickHandler returns the function the scheduler invokes each tick.
func (h *ConfidenceDecayHandler) TickHandler() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if h == nil || h.dm == nil {
			return nil
		}
		// The pass is OFF unless the operator has said otherwise. The
		// key is consulted first and only falls back to the approved
		// default when it is ABSENT, so an explicit 0 (an operator who
		// turned it off) is preserved across an upgrade just as an
		// explicit 1 is.
		if h.dm.GetConfigInt(confidenceDecayEnabledKey, core.DefaultConfidenceDecayEnabled) == 0 {
			return nil
		}
		now := h.now()

		intervalHours := h.dm.GetConfigInt(confidenceDecayIntervalKey, int(core.DefaultConfidenceDecayInterval/time.Hour))
		if intervalHours <= 0 {
			intervalHours = int(core.DefaultConfidenceDecayInterval / time.Hour)
		}
		if last, ok := h.lastRun(); ok && now.Sub(last) < time.Duration(intervalHours)*time.Hour {
			return nil
		}

		limit := h.dm.GetConfigInt(confidenceDecayLimitKey, core.DefaultConfidenceDecayLimit)
		minAgeHours := h.dm.GetConfigInt(confidenceDecayMinAgeKey, int(core.DefaultConfidenceDecayMinAge/time.Hour))

		res, err := h.dm.RunConfidenceDecay(core.ConfidenceDecayOptions{
			Now:    now,
			Limit:  limit,
			MinAge: time.Duration(minAgeHours) * time.Hour,
			// The tick's context is threaded in so a shutdown during a
			// pass stops it at the next artifact boundary. The artifacts
			// already processed keep their watermark and the rest stay
			// due, so the next scheduled pass resumes them.
			Ctx: ctx,
		})
		if err != nil {
			return fmt.Errorf("confidence decay pass: %w", err)
		}

		// Advance the cadence marker even on a partial pass: the failed
		// artifacts stay due (their watermark was rolled back) and are
		// retried at the next scheduled run, not on every tick.
		//
		// A CANCELLED pass is the exception: it was interrupted, not
		// completed, so the marker is not advanced and the next tick may
		// resume the artifacts it did not reach. Leaving the marker
		// behind would hide the unfinished backlog behind a 6-hour
		// cadence gate for no reason.
		if !res.Cancelled {
			if _, err := h.dm.SaveSystemConfig(
				core.ConfidenceDecayLastRunKey,
				strconv.FormatInt(now.Unix(), 10),
				strconv.FormatInt(now.Unix(), 10),
				"",
			); err != nil && h.logger != nil {
				h.logger.Warn("confidence decay: could not persist last-run marker", "err", err)
			}
		}

		if h.logger != nil && (res.Changed > 0 || res.Failed > 0 || res.Deferred > 0 || res.Cancelled) {
			h.logger.Info("confidence decay pass",
				"due", res.Due,
				"scanned", res.Scanned,
				"changed", res.Changed,
				"unchanged", res.Unchanged,
				"failed", res.Failed,
				"deferred", res.Deferred,
				"cancelled", res.Cancelled,
			)
		}
		if res.Cancelled {
			// A cancelled pass is NOT an error. The scheduler cancels
			// the tick context when it shuts down, and this handler is
			// the only one that could be mid-pass at that moment;
			// logging it as a failed tick would put a red line in the
			// journal on every restart. The cancellation is reported in
			// the Info line above, and the state is what matters: the
			// artifacts it did not reach are still due, and the next
			// tick resumes them.
			return nil
		}
		if res.Failed > 0 {
			return fmt.Errorf("confidence decay: %d of %d artifact(s) failed; they remain due",
				res.Failed, res.Scanned)
		}
		return nil
	}
}

// lastRun reads the persisted cadence marker. Absent or unparsable
// markers mean "never run", so the first tick after startup runs.
func (h *ConfidenceDecayHandler) lastRun() (time.Time, bool) {
	cfg, err := h.dm.GetSystemConfig(core.ConfidenceDecayLastRunKey)
	if err != nil {
		return time.Time{}, false
	}
	// The marker's raw_json is the pass clock (Unix seconds). Reading it
	// rather than the row's updated_at keeps the cadence tied to the
	// clock the pass actually observed — production uses wall time, tests
	// inject a fixed clock, and both behave identically.
	if raw, ok := cfg["raw_json"].(string); ok {
		if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
			return time.Unix(secs, 0), true
		}
	}
	switch v := cfg["updated_at"].(type) {
	case int64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(v, 0), true
	case int:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(v), 0), true
	}
	return time.Time{}, false
}
