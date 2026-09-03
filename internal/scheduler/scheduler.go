// Package scheduler implements the universal wake executor for MPM.
//
// The scheduler is the platform-level fix for MPM's opportunistic-fold gap.
// Wakes registered in `scheduled_wakes` are dispatched at their
// `target_time` by a deadline-driven Run loop — NOT by a fixed-period
// poll. The scheduler computes the earliest pending deadline (across
// scheduled_wakes AND scheduled_tasks), sleeps until that deadline
// (bounded above by the system-maintenance interval), and drains all
// currently-due notification-kind wakes atomically.
//
// Two kinds of latency:
//
//   - In-process (same OS process as the scheduler): a NotifyScheduleChanged
//     channel signal wakes Run() from its deadline sleep. ~immediate.
//
//   - Cross-process (mpm call CLI, mpm-mcp server running as separate OS
//     processes): bounded by `min(interval, time_to_next_deadline)` —
//     the bounded-sleep floor. With the default 60s interval, cross-
//     process wake mutations surface within ≤ 60s without any
//     goroutine-channel handoff. See Run() doc for the full contract.
//
// Two kinds of dispatch:
//
//   - System kinds (snapshot, critic_audit, gc, broadcast, drill,
//     cascade_summary, cascade, cron) are owned by Tick() and run on
//     the maintenance ticker (interval — default 60s).
//
//   - Notification-kind and untagged wakes are owned by the deadline-
//     driven dispatch path (internal/scheduler/dispatch.go). They
//     fire at target_time, not on a poll cadence.
//
// Three guarantees from the locked architecture (decision 27d7b3c18199e098):
//
//   1. Default kind = notification (backward compat). Existing wakes with
//      no kind tag pass through to the notification dispatch path.
//   2. Concurrent execution. Independent system wakes fire in parallel
//      goroutines. Serial would re-introduce the SF2 race failure mode.
//   3. Failure isolation. Non-zero handler exit still marks the wake
//      fired=1 with metadata.last_error. One wake's failure cannot block
//      subsequent wakes.
//
// Singleton enforcement is via flock on a PID file so two scheduler
// instances cannot race on the same wake batch. The atomic UPDATE...
// RETURNING claim in dispatch.go provides exactly-once fire semantics
// even when the opportunistic fold in mpm call / mpm-mcp also runs on
// the same database.
package scheduler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"

	core "github.com/flowbyte-com/mpm-core"
)

// Wake is the projected view of a scheduled_wakes row.
//
// CreatedAt is stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1).
type Wake struct {
	ID            string                 `json:"id"`
	TargetTime    int64                  `json:"target_time"`
	Reason        string                 `json:"reason"`
	TheoryID      string                 `json:"theory_id,omitempty"`
	RecurringRule string                 `json:"recurring_rule,omitempty"`
	CreatedBy     string                 `json:"created_by"`
	CreatedAt     int64                  `json:"created_at"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

// Kind returns the dispatch key for a wake. Reads metadata.kind first;
// falls back to "notification" for any wake without an explicit system kind.
// This is the backward-compat guarantee: pre-scheduler wakes pass through
// to the existing opportunistic fold unchanged.
func (w Wake) Kind() string {
	if k, ok := w.Metadata["kind"].(string); ok && k != "" {
		return k
	}
	return "notification"
}

// HandlerFunc executes a system wake. Returning a non-nil error marks the
// wake fired with last_error set; it does NOT block subsequent wakes.
type HandlerFunc func(w Wake) error

// Scheduler is the long-running wake executor.
type Scheduler struct {
	db           *sql.DB
	dbPath       string
	log          *slog.Logger
	handlers     map[string]HandlerFunc
	tickHandlers map[string]func(ctx context.Context) error
	mu           sync.RWMutex

	// wakeCh is the in-process notification channel for callers that
	// mutate scheduled_wakes from the SAME process as the scheduler
	// (e.g. the scheduler's own tick handlers like cascade_drain or
	// cascade_wake_reconcile, which insert new wake rows). A non-blocking
	// send wakes Run() from its deadline sleep so the change is observed
	// promptly instead of waiting for the next maintenance tick.
	//
	// Capacity 1 — coalesces bursts of mutations so the scheduler
	// drains the wake queue once per wake-up rather than once per
	// mutation.
	//
	// Cross-process mutation (mpm call, mpm-mcp on a separate OS
	// process) cannot reach this channel and instead relies on the
	// bounded-sleep floor in Run() (see Run doc for the latency
	// bound — `min(interval, time_to_next_deadline)`).
	wakeCh chan struct{}

	// tickCount increments on every ticker fire (including idle ticks).
	// heartbeatEvery controls how often a heartbeat log line is emitted
	// (0 = disabled; default 100 ticks = ~100 min at 60s interval).
	// SetHeartbeat overrides the default at startup.
	tickCount      uint64
	heartbeatEvery uint64

	// lastWakeupWasTick records which select branch fired on the most
	// recent iteration, so the post-select ad-hoc drain only runs
	// after the tick branch (avoids a double drain on the
	// deadline-branch path — that branch already drained inline).
	lastWakeupWasTick atomic.Bool

	// processStartedUnix is captured at New() and persisted to
	// scheduler.state every tick. The CLI's emitSchedulerHealthWarning
	// surfaces 'not running' when process_started_unix is stale — a
	// way to detect a daemon that exited and was replaced without a
	// state-file update (which the atomic-write pattern in state.go
	// makes safe).
	processStartedUnix int64

	// captureBuf/captureMu are test-only fields for capturing slog output.
	// Production code never reads them; tests set them via
	// newTestSchedulerWithCaptureLogger and retrieve via s.captureLogs().
	captureBuf *bytes.Buffer
	captureMu  *sync.Mutex
}

// New returns a Scheduler bound to the given *sql.DB. The caller owns
// the database lifecycle (typically a *DatabaseManager from mpm-core)
// and is responsible for closing it.
//
// Architectural note: scheduler.New does NOT open or own the database.
// Connection lifecycle lives at the construction site so the DatabaseManager
// remains the singleton owner of *sql.DB. This is the F-007 fix — scheduler
// participates in DatabaseManager's connection management rather than
// bypassing it. The sqlopen_owner_test static-analysis guard covers this
// invariant and will fail if any future code opens a separate handle.
func New(db *sql.DB, log *slog.Logger) (*Scheduler, error) {
	if db == nil {
		return nil, fmt.Errorf("db is required (caller must construct via DatabaseManager)")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	s := &Scheduler{
		db:                 db,
		dbPath:             "<managed-by-DatabaseManager>",
		log:                log,
		handlers:           make(map[string]HandlerFunc),
		tickHandlers:       make(map[string]func(ctx context.Context) error),
		wakeCh:             make(chan struct{}, 1),
		heartbeatEvery:     100, // ~100 min at 60s interval; override with SetHeartbeat
		processStartedUnix: time.Now().Unix(),
	}
	// Register the notification-expiration sweep as a tick handler so
	// every scheduler tick retires notification-kind wakes whose
	// target_time is more than 7 days in the past. The sweep is
	// bounded (LIMIT 100) and runs in its own transaction so a
	// backlog drains over multiple ticks without ever blocking a
	// single tick. See wake_expiration.go for the full contract.
	s.RegisterTickHandler("notification_expiration", NotificationExpirationTickHandler(context.Background(), db))
	return s, nil
}

// SetHeartbeat overrides the default heartbeat cadence. Pass 0 to disable.
// A heartbeat is a single "scheduler heartbeat" INFO log line every N
// ticks — useful for confirming daemon liveness without journal grep
// gymnastics (silence ≠ stuck when idle).
func (s *Scheduler) SetHeartbeat(every uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatEvery = every
}

// NotifyScheduleChanged signals Run() that the wake-mutation state has
// changed in this process (e.g. a tick handler inserted a new wake row).
// Run will wake from its deadline sleep, recompute the earliest pending
// deadline, and re-arm the timer. The send is non-blocking — when the
// channel is already pending, additional signals are coalesced into the
// pending wake-up (no goroutine wait, no queue buildup).
//
// Scope: in-process only. Cross-process writers (mpm call CLI,
// mpm-mcp server running as separate OS processes) cannot reach this
// channel; their mutations are picked up via the bounded-sleep floor in
// Run() within min(interval, time_to_next_deadline) — see the Run doc.
//
// Safe to call before Run() starts (no-op when wakeCh is nil —
// defensive for callers that hand a partially-constructed Scheduler
// to a tick handler).
func (s *Scheduler) NotifyScheduleChanged() {
	if s.wakeCh == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default: // channel already pending — coalesce
	}
}

// Close is a no-op on the scheduler's bound *sql.DB. The DatabaseManager
// owns connection lifecycle; close it at the construction site, not here.
// Kept as part of the public surface for symmetry and to give tests a
// deterministic tear-down without depending on the caller.
func (s *Scheduler) Close() error {
	return nil
}

// Register attaches a HandlerFunc for the given wake kind. Registering
// overwrites any previous handler for that kind. Adding a new system kind
// is a one-line operation; central dispatch never needs editing.
func (s *Scheduler) Register(kind string, h HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[kind] = h
}

// RegisterTickHandler attaches a function to run on every Tick, after
// ProcessScheduledTasks and the wake dispatch. Tick handlers are not
// gated by wake presence — they fire unconditionally on each Tick so the
// scheduler can drive its own periodic work (e.g. cascade drain).
//
// Tick handlers run sequentially after wake dispatch completes. A handler
// that returns a non-nil error is logged but does not prevent later
// tick handlers from running.
func (s *Scheduler) RegisterTickHandler(name string, fn func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickHandlers[name] = fn
}

// QueryDueWakes returns all unfired wakes with target_time <= now.
// Does NOT mark them fired (notification wakes must remain available for
// mpm-mcp's opportunistic fold).
func (s *Scheduler) QueryDueWakes(now time.Time) ([]Wake, error) {
	rows, err := s.db.Query(
		`SELECT id, target_time, reason, COALESCE(theory_id,''), COALESCE(recurring_rule,''),
		        created_by, created_at, COALESCE(metadata,'')
		 FROM scheduled_wakes
		 WHERE fired = 0 AND target_time <= ?
		 ORDER BY target_time ASC`,
		now.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("query due wakes: %w", err)
	}
	defer rows.Close()

	var out []Wake
	for rows.Next() {
		var w Wake
		var metaJSON string
		if err := rows.Scan(&w.ID, &w.TargetTime, &w.Reason, &w.TheoryID, &w.RecurringRule,
			&w.CreatedBy, &w.CreatedAt, &metaJSON); err != nil {
			return nil, fmt.Errorf("scan wake: %w", err)
		}
		if metaJSON != "" {
			_ = json.Unmarshal([]byte(metaJSON), &w.Metadata)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MarkFired records the wake as dispatched. lastError may be empty.
func (s *Scheduler) MarkFired(id string, lastError string) error {
	if lastError == "" {
		_, err := s.db.Exec(
			`UPDATE scheduled_wakes SET fired = 1, fired_at = ?, metadata = json_set(COALESCE(metadata,'{}'), '$.last_error', NULL) WHERE id = ?`,
			time.Now().Unix(), id,
		)
		return err
	}
	_, err := s.db.Exec(
		`UPDATE scheduled_wakes SET fired = 1, fired_at = ?,
		  metadata = json_set(COALESCE(metadata,'{}'), '$.last_error', ?)
		 WHERE id = ?`,
		time.Now().Unix(), lastError, id,
	)
	return err
}

// Tick runs one scheduler iteration. Pulls due wakes, dispatches each
// according to its kind. Returns the number of system wakes executed.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	// Tick handlers fire unconditionally on every tick, regardless of
	// wake state. Dispatch happens after wake processing completes.
	defer s.dispatchTickHandlers(ctx)

	// Phase 1: poll scheduled_tasks (Agentic Cron). Any task whose
	// next_run_at has passed gets a standard scheduled_wakes row
	// injected in the same transaction as its next_run_at rollover —
	// so a daemon crash between injection and rollover cannot
	// double-fire. Log only when work actually happened; quiet ticks
	// stay quiet.
	if n, err := core.ProcessScheduledTasks(s.db); err != nil {
		s.log.Error("cron poll failed", "err", err)
	} else if n > 0 {
		s.log.Info("cron injected wakes", "count", n)
	}

	wakes, err := s.QueryDueWakes(time.Now())
	if err != nil {
		return 0, fmt.Errorf("query due: %w", err)
	}
	if len(wakes) == 0 {
		return 0, nil
	}

	// Partition: system kinds get parallel goroutines; notification kinds
	// pass through untouched so mpm-mcp's opportunistic fold handles them.
	systemWakes := make([]Wake, 0, len(wakes))
	for _, w := range wakes {
		if s.hasHandler(w.Kind()) {
			systemWakes = append(systemWakes, w)
		} else {
			s.log.Debug("notification kind, leaving for opportunistic fold",
				"wake_id", w.ID, "kind", w.Kind())
		}
	}

	if len(systemWakes) == 0 {
		return 0, nil
	}

	// Execute system wakes concurrently. Each goroutine is independent —
	// no shared state, no depends_on graph in MVP. Future hardening can
	// add a depends_on registry per wake (e.g., snapshot -> critic_audit).
	var wg sync.WaitGroup
	executed := 0
	var mu sync.Mutex
	for _, w := range systemWakes {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.executeOne(ctx, w); err != nil {
				s.log.Error("wake handler failed",
					"wake_id", w.ID, "kind", w.Kind(), "err", err)
				if mErr := s.MarkFired(w.ID, err.Error()); mErr != nil {
					s.log.Error("mark fired failed",
						"wake_id", w.ID, "err", mErr)
				}
				return
			}
			if mErr := s.MarkFired(w.ID, ""); mErr != nil {
				s.log.Error("mark fired failed", "wake_id", w.ID, "err", mErr)
				return
			}
			mu.Lock()
			executed++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return executed, nil
}

// dispatchTickHandlers runs all registered tick handlers sequentially.
// A handler that returns a non-nil error is logged but does not prevent
// later tick handlers from running. The handler list is snapshotted
// under the read lock so a handler that re-registers itself cannot
// deadlock against the read lock.
func (s *Scheduler) dispatchTickHandlers(ctx context.Context) {
	s.mu.RLock()
	tickFns := make([]struct {
		name string
		fn   func(ctx context.Context) error
	}, 0, len(s.tickHandlers))
	for name, fn := range s.tickHandlers {
		tickFns = append(tickFns, struct {
			name string
			fn   func(ctx context.Context) error
		}{name, fn})
	}
	s.mu.RUnlock()

	for _, h := range tickFns {
		if err := h.fn(ctx); err != nil {
			s.log.Error("tick handler failed",
				"handler", h.name,
				"err", err)
		}
	}
}

// executeOne runs a single wake's handler under a goroutine, with context
// timeout so a hung handler cannot pin a tick indefinitely.
//
// Logs both an "executing wake" line on entry and a "wake completed" line
// on exit (with duration_ms and err if non-nil). The exit log is the
// trace that proves a handler ran end-to-end — the entry log alone doesn't
// distinguish a fast success from a hung handler killed by ctx.
func (s *Scheduler) executeOne(ctx context.Context, w Wake) error {
	s.mu.RLock()
	h, ok := s.handlers[w.Kind()]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no handler registered for kind=%q", w.Kind())
	}
	s.log.Info("executing wake",
		"wake_id", w.ID, "kind", w.Kind(), "reason", truncate(w.Reason, 80))

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- h(w) }()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err != nil {
			s.log.Error("wake completed",
				"wake_id", w.ID,
				"kind", w.Kind(),
				"duration_ms", elapsed.Milliseconds(),
				"err", err)
		} else {
			s.log.Info("wake completed",
				"wake_id", w.ID,
				"kind", w.Kind(),
				"duration_ms", elapsed.Milliseconds())
		}
		return err
	case <-ctx.Done():
		elapsed := time.Since(start)
		s.log.Error("wake cancelled",
			"wake_id", w.ID,
			"kind", w.Kind(),
			"duration_ms", elapsed.Milliseconds(),
			"err", ctx.Err())
		return fmt.Errorf("handler context cancelled: %w", ctx.Err())
	}
}

func (s *Scheduler) hasHandler(kind string) bool {
	if kind == "notification" {
		return false
	}
	s.mu.RLock()
	_, ok := s.handlers[kind]
	s.mu.RUnlock()
	return ok
}

// computeEarliestDeadline returns the soonest pending deadline across
// all wake-mutation inputs:
//
//   - scheduled_wakes WHERE fired = 0
//     (user-scheduled ad-hoc and not-yet-fired scheduled rows)
//   - scheduled_tasks WHERE status = 'active'
//     (agentic-cron latent — its next_run_at becomes a wake when due)
//
// Returns time.Time{} (zero) when nothing is pending. The query is
// UNION ALL'd into a single MIN() — SQLite plans both branches
// efficiently against the partial covering indexes
// (idx_sw_pending, idx_st_active), keeping the cost sub-millisecond
// at realistic queue sizes.
//
// `now` is currently unused by the SQL but kept in the signature so
// future implementations can substitute server-side clock if the
// database clock drifts. The naive MIN(t) is correct — we want the
// earliest absolute deadline, not one relative to now.
func (s *Scheduler) computeEarliestDeadline(ctx context.Context, now time.Time) (time.Time, error) {
	var ts sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT MIN(t) FROM (
			SELECT target_time AS t FROM scheduled_wakes WHERE fired = 0
			UNION ALL
			SELECT next_run_at AS t FROM scheduled_tasks WHERE status = 'active'
		)
	`).Scan(&ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("compute earliest deadline: %w", err)
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	return time.Unix(ts.Int64, 0), nil
}

// Run is the scheduler's main loop. Blocks until ctx is cancelled.
//
// Loop structure: select on ctx.Done, the in-process wakeCh, the
// ad-hoc deadline timer, and the system maintenance ticker.
//
//   - ctx.Done: shutdown.
//   - wakeCh: in-process wake mutation (same-process writers).
//   - deadlineTimer.C: a pending notification-kind wake is now due;
//     drained via dispatchDrainAdHocWakes.
//   - ticker.C: system maintenance cadence — runs Tick (cron poll +
//     system-kind dispatch + tick handlers).
//
// After every non-shutdown wake-up we recompute the earliest deadline
// and Reset the deadline timer. The deadline sleep is bounded above by
// the `interval` argument (the system maintenance cadence) so cross-
// process wake mutations (mpm call, mpm-mcp) are picked up within
// `min(interval, time_to_next_deadline)` even though those processes
// cannot send to wakeCh.
//
// Latency bounds:
//   - in-process: ~immediate (channel-send latency).
//   - cross-process: bounded by min(interval, time_to_deadline).
//
// `interval` keeps its historical meaning (the system maintenance
// cadence). The default in cmd/mpm-scheduler/main.go is 60s.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) error {
	if interval < time.Second {
		return errors.New("interval must be >= 1s")
	}
	s.log.Info("scheduler running",
		"interval", interval.String(),
		"db", s.dbPath,
		"mode", "deadline-driven")

	tick := time.NewTicker(interval)
	defer tick.Stop()

	deadlineTimer := time.NewTimer(time.Hour) // dummy; first Reset replaces it
	defer deadlineTimer.Stop()

	// Run once immediately so pre-existing pending work doesn't wait
	// an interval to be observed; also seeds initial scheduler.state.
	s.runOnce(ctx)

	// Initial deadline timer arm (after runOnce so newly-injected cron
	// wakes are visible).
	s.scheduleNextDeadline(ctx, deadlineTimer, interval)

	for {
		// Default false; set only by the tick.C case below so the
		// post-iteration ad-hoc drain runs after the maintenance tick
		// (a long tick may have made earlier deadlines overdue).
		s.lastWakeupWasTick.Store(false)

		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopping", "reason", ctx.Err())
			return nil
		case <-s.wakeCh:
			// in-process mutation — recompute deadline and continue
			s.log.Debug("in-process wake notification — recomputing deadline")
		case <-deadlineTimer.C:
			// ad-hoc deadline elapsed — drain notification-kind wakes
			n, err := dispatchDrainAdHocWakes(ctx, s.db, time.Now(), 100)
			if err != nil {
				s.log.Error("ad-hoc drain failed", "err", err)
			} else if n > 0 {
				s.log.Info("ad-hoc drain executed wakes", "count", n)
			}
		case <-tick.C:
			s.tickCount++
			s.mu.RLock()
			hb := s.heartbeatEvery
			s.mu.RUnlock()
			if hb > 0 && s.tickCount%hb == 0 {
				s.log.Info("scheduler heartbeat",
					"tick", s.tickCount,
					"interval", interval.String())
			}
			if n, err := s.Tick(ctx); err != nil {
				s.log.Error("tick failed", "err", err)
			} else if n > 0 {
				s.log.Info("tick executed system wakes", "count", n)
			}
			s.persistState()
			s.lastWakeupWasTick.Store(true)
		}

		// After every wake-up (except ctx.Done): re-arm the timer AND,
		// if we just ran a maintenance tick, opportunistically drain
		// any ad-hoc wakes that became overdue during the tick (a long
		// Tick may take seconds; a deadline that landed mid-Tick would
		// otherwise wait until the timer fired).
		if s.lastWakeupWasTick.Load() {
			n, err := dispatchDrainAdHocWakes(ctx, s.db, time.Now(), 100)
			if err != nil {
				s.log.Error("post-tick ad-hoc drain failed", "err", err)
			} else if n > 0 {
				s.log.Info("post-tick ad-hoc drain executed wakes", "count", n)
			}
		}
		s.scheduleNextDeadline(ctx, deadlineTimer, interval)
	}
}

// runOnce does an immediate startup sweep: Tick (cron poll + system
// dispatch + tick handlers) and persistState. Called once at scheduler
// start, before the deadline-timer-armed select loop begins. Distinct
// from Tick() only by its inline persistState + log line.
func (s *Scheduler) runOnce(ctx context.Context) {
	if n, err := s.Tick(ctx); err != nil {
		s.log.Error("startup tick failed", "err", err)
	} else if n > 0 {
		s.log.Info("startup tick executed system wakes", "count", n)
	}
	s.persistState()
}

// scheduleNextDeadline recomputes the earliest pending deadline and
// (re)arms the timer. Sleep duration = min(interval, time.Until(earliest))
// when something is pending, else `interval`. d=0 means "fire on the
// next loop iteration" (deadline already past — the drain handler runs
// without further delay).
func (s *Scheduler) scheduleNextDeadline(ctx context.Context, t *time.Timer, interval time.Duration) {
	earliest, err := s.computeEarliestDeadline(ctx, time.Now())
	if err != nil {
		s.log.Error("compute earliest deadline failed", "err", err)
		resetTimer(t, interval)
		return
	}
	var d time.Duration
	if earliest.IsZero() {
		d = interval
	} else {
		d = time.Until(earliest)
		if d <= 0 {
			d = 0 // overdue — fire immediately
		}
		if d > interval {
			d = interval
		}
	}
	resetTimer(t, d)
	if !earliest.IsZero() && d > 0 {
		s.log.Debug("next deadline",
			"delta", d.String(),
			"abs", earliest.UTC().Format(time.RFC3339))
	}
}

// resetTimer safely resets a time.Timer, draining the channel if the
// timer has already fired but the value hasn't yet been read. This is
// the canonical Go pattern documented in the standard library: an
// unserviced timer can deadlock a Reset into spamming zero-delay fires.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// AcquireLock grabs an exclusive flock on path. Returns the file handle
// (caller must defer Release) or an error if another instance holds it.
func AcquireLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire flock (another scheduler running?): %w", err)
	}
	return f, nil
}

// truncate shortens a string for log output. Used to keep reason fields
// from blowing up logs when they include long URLs or free text.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ── Built-in handlers ─────────────────────────────────────────────────────
//
// Each handler is a thin wrapper around an existing CLI tool. Handlers
// return errors; the scheduler translates those into last_error metadata
// and marks fired regardless of outcome (failure isolation).

// SnapshotHandler creates an atomic pre-flight SQLite backup via
// sqlite3 .backup. Replaces pre_critic_snapshot.sh. Writes to the same
// backups/critic-pre/ directory the legacy shell script used.
//
// metadata.label: optional filename suffix (default: epoch). The label
// is restricted to [A-Za-z0-9._-] because it is interpolated into a
// SQL string literal passed to sqlite3; an unconstrained label could
// inject SQL or escape the path. Untrusted labels are dropped to the
// epoch default rather than failing the snapshot.
func SnapshotHandler(w Wake) error {
	dbPath := defaultDBPath()
	backupDir := filepath.Join(filepath.Dir(dbPath), "..", "..", "backups", "critic-pre")
	if env := os.Getenv("MPM_BACKUP_DIR"); env != "" {
		backupDir = env
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("mkdir backup dir: %w", err)
	}

	label := fmt.Sprintf("%d", time.Now().Unix())
	if l, ok := w.Metadata["label"].(string); ok && l != "" {
		if safe := sanitizeSnapshotLabel(l); safe != "" {
			label = safe
		}
	}
	snapshot := filepath.Join(backupDir, fmt.Sprintf("mpm_pre_critic_%s.db", label))

	// sqlite3 .backup is atomic and WAL-safe; concurrent writers are not blocked.
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf(".backup '%s'", snapshot))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sqlite3 .backup: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Verify the snapshot is sane before trusting it as rollback target.
	integ := exec.Command("sqlite3", snapshot, "PRAGMA integrity_check;")
	out, err := integ.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		return fmt.Errorf("integrity_check: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return rotateSnapshots(backupDir, 7)
}

// sanitizeSnapshotLabel restricts a wake-metadata label to a safe
// filename suffix. Returns "" for any input containing characters
// outside [A-Za-z0-9._-] (the snapshot falls back to the epoch
// default in that case rather than failing the snapshot). The
// restriction prevents two injection paths:
//
//  1. SQL injection via the sqlite3 .backup string literal (the
//     label flows into `.backup '...mpm_pre_critic_<label>.db'`).
//  2. Path traversal via ../ (the label becomes part of the
//     destination filename).
//
// Length is capped at 64 chars to keep filenames bounded.
func sanitizeSnapshotLabel(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return ""
		}
	}
	return s
}

// CriticAuditHandler shells out to the mpm-critic binary for one audit
// cycle. Decoupled from internal/critic/ so the scheduler stays a thin
// dispatcher; the critic evolves independently. If the binary isn't on
// PATH, the handler returns an error so the wake is marked fired with
// last_error (failure isolation).
func CriticAuditHandler(w Wake) error {
	bin := os.Getenv("MPM_CRITIC_BIN")
	if bin == "" {
		bin = "mpm-critic"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "MPM_DB_PATH="+defaultDBPath())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpm-critic: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// GCHandler triggers the existing gc_run sweep. Reads dry_run and aggressive
// flags from wake metadata so per-cycle tuning is possible.
func GCHandler(w Wake) error {
	dryRun := "true"
	if d, ok := w.Metadata["dry_run"].(bool); ok && !d {
		dryRun = "false"
	}
	args := []string{"call", "gc_run", fmt.Sprintf(`{"dry_run":%s}`, dryRun)}
	if a, ok := w.Metadata["aggressive"].(bool); ok && a {
		args[2] = `{"dry_run":false,"aggressive":true}`
	}
	cmd := exec.Command("mpm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpm call gc_run: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// BroadcastHandler triggers an active-dissemination broadcast to receiving
// agents via mpm ops broadcast. Used for high-priority cross-agent pushes.
func BroadcastHandler(w Wake) error {
	target := ""
	if t, ok := w.Metadata["target"].(string); ok {
		target = t
	}
	args := []string{"ops", "broadcast"}
	if target != "" {
		args = append(args, "--target", target)
	}
	cmd := exec.Command("mpm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpm ops broadcast: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

func defaultDBPath() string {
	if env := os.Getenv("MPM_DB_PATH"); env != "" {
		return env
	}
	// Default to the workspace-relative path mpm-mcp uses.
	cwd, err := os.Getwd()
	if err != nil {
		return "src/db/mpm.db"
	}
	candidate := filepath.Join(cwd, "src", "db", "mpm.db")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return "src/db/mpm.db"
}

// rotateSnapshots keeps the most recent N snapshots in dir and deletes
// older ones. Used by SnapshotHandler so the backup directory doesn't
// grow unbounded.
func rotateSnapshots(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type snap struct {
		path  string
		mtime time.Time
	}
	var snaps []snap
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "mpm_pre_critic_") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, snap{
			path:  filepath.Join(dir, e.Name()),
			mtime: fi.ModTime(),
		})
	}
	if len(snaps) <= keep {
		return nil
	}
	// Sort newest first.
	for i := 0; i < len(snaps); i++ {
		for j := i + 1; j < len(snaps); j++ {
			if snaps[j].mtime.After(snaps[i].mtime) {
				snaps[i], snaps[j] = snaps[j], snaps[i]
			}
		}
	}
	for _, s := range snaps[keep:] {
		_ = os.Remove(s.path)
	}
	return nil
}