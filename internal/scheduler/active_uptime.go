package scheduler

// Active-uptime accounting.
//
// The persistent scheduler is the only MPM process whose lifetime is
// meaningful, so it is the only thing that can define "active uptime".
// This file accrues that uptime into durable state (owned by core) once
// per tick; the Critic reads the result to decide whether a memory has
// accumulated enough ACTIVE residency to be challenged.
//
// CLOCK MODEL — MONOTONIC PROCESS ELAPSED, NOT WALL GAPS.
//
// Each tick computes how long THIS PROCESS has been alive using a
// monotonic source and adds only the increment since the previous tick:
//
//	elapsed = now().Sub(processStart)     // monotonic
//	delta   = max(0, elapsed - lastElapsed)
//	persist delta; lastElapsed = elapsed
//
// Because `elapsed` restarts near zero in every new process, a restart
// after an 8h outage contributes ZERO — the gap is never reconstructed
// from wall time. The same holds for forward/backward wall-clock jumps
// (NTP step, suspend/resume): no wall delta is consulted anywhere, so
// none can fabricate time.
//
// A crash loses the tail accrued since the last persisted tick. That is
// a deliberate, conservative UNDERCOUNT; it is never backfilled.
//
// WHY NOT scheduler.state: that file is a cross-process HEALTH BRIDGE.
// Its writes deliberately swallow errors ("observability, not critical
// path"), it is not atomically updated, and it is JSON on a filesystem
// path rather than transactional state. Accounting state must not
// inherit those failure modes — a lost or partial uptime write would
// silently move every memory's settling boundary.

import (
	"context"
	"database/sql"
	"sync"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// activeUptimeClock supplies the scheduler's process-monotonic elapsed
// time. Production uses a time.Time captured at New() plus time.Since,
// which Go derives from a monotonic reading — immune to wall-clock
// adjustment. Tests substitute a controllable source so accounting is
// exercised with synthetic durations and never a sleep.
type activeUptimeClock interface {
	// elapsed returns how long THIS PROCESS has been alive.
	elapsed() time.Duration
}

// realActiveUptimeClock is the production monotonic clock.
type realActiveUptimeClock struct{ start time.Time }

func (c realActiveUptimeClock) elapsed() time.Duration { return time.Since(c.start) }

// activeUptimeAccruer owns the per-process bookkeeping for cumulative
// active uptime. All fields are guarded by mu; the scheduler's tick loop
// calls Accrue from a single goroutine, but Tick is also exercised
// directly and concurrently in tests, so the state is not assumed
// single-threaded.
type activeUptimeAccruer struct {
	mu          sync.Mutex
	clock       activeUptimeClock
	lastElapsed time.Duration
}

func newActiveUptimeAccruer(clock activeUptimeClock) *activeUptimeAccruer {
	if clock == nil {
		clock = realActiveUptimeClock{start: time.Now()}
	}
	return &activeUptimeAccruer{clock: clock, lastElapsed: 0}
}

// Accrue adds this process's elapsed time since the previous accrual to
// the durable cumulative total, populates any missing per-memory
// settling baselines against the resulting value, and returns the new
// cumulative active seconds.
//
// ORDERING (Section 6 of the design): the global counter is advanced
// FIRST and baselines are stamped with the RESULTING value, so a memory
// admitted during this tick anchors to the post-accrual total and can
// never receive credit for time before the scheduler observed it. At
// most one tick interval of settling credit is lost in that window,
// which is conservative and accepted.
//
// This runs at the START of Tick, before due wakes — and specifically
// before a critic_audit wake is dispatched — so the audit that this very
// tick triggers observes both the current global active seconds and a
// fully populated baseline set.
//
// FAIL CLOSED: malformed durable state yields an error and NO writes.
// The caller logs and continues; a tick that cannot account for uptime
// must not fabricate it, and must not block ordinary wake processing.
func (a *activeUptimeAccruer) Accrue(ctx context.Context, db *sql.DB) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	elapsed := a.clock.elapsed()
	delta := core.ActiveUptimeFromElapsed(elapsed, a.lastElapsed)

	total, err := core.AccrueActiveUptime(ctx, db, delta, elapsed.Milliseconds())
	if err != nil {
		// Do NOT advance lastElapsed on failure: the unaccrued interval
		// is retried next tick rather than silently dropped.
		return 0, err
	}
	a.lastElapsed = elapsed

	if _, err := core.CaptureMissingSettlingBaselines(ctx, db, total); err != nil {
		return total, err
	}
	return total, nil
}
