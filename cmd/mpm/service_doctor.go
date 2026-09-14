// cmd/mpm/service_doctor.go — DoctorService for Wave 2.
//
// DoctorService EARNED its existence (RFC §7) by owning aggregation
// behaviour that crosses five substrate paths:
//
//   - Database integrity (PRAGMA quick_check + page stats)
//   - Embeddings (count memories with NULL embeddings)
//   - Working Context orphans (scratchpads past decay_at)
//   - Scheduler (overdue wakes via HealthCheck)
//   - Review backlog (memories due for spaced reinforcement)
//
// If this service disappeared, every operator who wanted a single
// "is everything ok?" view would reimplement the cross-check
// aggregation. Aggregation IS behaviour — earned.
//
// Layering contract (RFC §7):
//   DoctorService composes queries across the substrate. It does
//   NOT call other services — doctor IS the composition. It does
//   NOT own rendering. It does NOT call commands.
//
// Reuses the legacy DoctorCheck/DoctorReport types in main.go so the
// engine-room `mpm ops doctor --deep-scan` and the top-level `mpm
// doctor` trust-signal flagship share the same canonical data
// shape. Differences are in which checks run and how the result
// renders — not in the data model.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm/internal/scheduler"
)

// DoctorService runs the cross-subsystem trust signals and assembles
// a legacy-DoctorReport.
type DoctorService struct {
	dm *mpminternal.DatabaseManager
}

// NewDoctorService returns the service. Returns nil if dm is nil.
func NewDoctorService(dm *mpminternal.DatabaseManager) *DoctorService {
	if dm == nil {
		return nil
	}
	return &DoctorService{dm: dm}
}

// Check runs every per-subsystem check and aggregates the verdict.
// Returns (*DoctorReport, nil) on partial degradation. Returns
// (nil, error) only when the database is completely unreachable.
func (s *DoctorService) Check() (*DoctorReport, error) {
	if s.dm == nil {
		return nil, fmt.Errorf("nil database manager")
	}
	report := &DoctorReport{Checks: []DoctorCheck{}}
	startTime := time.Now().UTC()

	// Each check is a separate method — adding a check later doesn't
	// change Check() itself.
	report.Checks = append(report.Checks, s.checkDatabase())
	report.Checks = append(report.Checks, s.checkEmbeddings())
	report.Checks = append(report.Checks, s.checkEmbeddingProvider())
	report.Checks = append(report.Checks, s.checkWorkingContextOrphans())
	report.Checks = append(report.Checks, s.checkScheduler())
	report.Checks = append(report.Checks, s.checkWakeBacklog())
	report.Checks = append(report.Checks, s.checkReviewBacklog())

	// Tally.
	for _, c := range report.Checks {
		switch c.Status {
		case "PASS":
			report.Passed++
		case "WARN":
			report.Warnings++
		case "FAIL":
			report.Failed++
		}
	}
	report.TotalChecks = len(report.Checks)
	_ = startTime // reserved for per-check timing in future waves

	return report, nil
}

// checkDatabase inspects integrity + busy_retries via HealthCheck.
func (s *DoctorService) checkDatabase() DoctorCheck {
	check := DoctorCheck{Name: "Database"}
	hc, err := s.dm.HealthCheck()
	if err != nil {
		check.Status = "FAIL"
		check.Message = fmt.Sprintf("health check failed: %v", err)
		check.Details = []string{"Inspect /home/v/.mpm/src/db/mpm.db and run 'mpm ops doctor --deep-scan' for the integrity report."}
		return check
	}
	if ok, _ := hc["ok"].(bool); !ok {
		detail := "PRAGMA quick_check did not return 'ok'"
		if v, ok2 := hc["integrity_status"].(string); ok2 {
			detail = fmt.Sprintf("integrity status: %s", v)
		}
		check.Status = "FAIL"
		check.Message = detail
		check.Details = []string{"Run 'mpm ops doctor --deep-scan' for full breakdown."}
		return check
	}
	busy, _ := hc["busy_retries"].(uint64)
	pageCount, _ := hc["page_count"].(int64)
	check.Message = fmt.Sprintf("integrity ok | %d pages | %d busy retries", pageCount, busy)
	if busy > 100 {
		check.Status = "WARN"
		check.Details = []string{"Many busy retries — investigate WAL contention with 'mpm status'."}
		return check
	}
	check.Status = "PASS"
	return check
}

// checkEmbeddings inspects the embedding_source provenance of live memories.
// Reports per spec §7.1 table:
//   hash>0 AND provider=0 → FAIL (fully degraded)
//   hash>0               → WARN (legacy rows present)
//   null/total > 10%     → WARN (backfill needed)
//   all real provider    → PASS
func (s *DoctorService) checkEmbeddings() DoctorCheck {
	check := DoctorCheck{Name: "Embeddings"}
	var total, hashCount, nullCount int
	if err := s.dm.QueryRowTracked(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`,
	).Scan(&total); err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not count memories: %v", err)
		return check
	}
	if err := s.dm.QueryRowTracked(
		`SELECT
			COALESCE(SUM(CASE WHEN embedding_source='hash' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN embedding_source='null' THEN 1 ELSE 0 END), 0)
		FROM memories WHERE deleted_at IS NULL`,
	).Scan(&hashCount, &nullCount); err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not count embedding sources: %v", err)
		return check
	}
	providerCount := total - hashCount - nullCount
	switch {
	case hashCount > 0 && providerCount == 0:
		check.Status = "FAIL"
		check.Message = fmt.Sprintf("%d legacy hash rows; no real embeddings persisted; semantic search fully degraded", hashCount)
		check.Details = []string{"Run `mpm ops migrate-embeddings` to classify and remediate."}
	case hashCount > 0:
		check.Status = "WARN"
		check.Message = fmt.Sprintf("%d legacy hash rows present", hashCount)
		check.Details = []string{"Run `mpm ops migrate-embeddings` to classify and remediate."}
	case total > 0 && nullCount*10 > total: // > 10%
		check.Status = "WARN"
		check.Message = fmt.Sprintf("%d / %d without embedding — run `mpm ops backfill-embeddings`", nullCount, total)
	default:
		check.Status = "PASS"
		// Final release-pass wording (defect: doctor said "all from real
		// provider" while the embedding-provider check said "no
		// provider configured" — apparently contradictory). The two
		// checks now distinguish historical vs current state:
		// stored vectors are provider-generated (passive voice — does
		// not claim the provider is still reachable); the
		// EmbeddingProvider check reports whether NEW embeddings can
		// be generated.
		//
		// Final-pass wording: label this row "Stored embeddings" so
		// the historical-vs-current distinction is unambiguous in
		// the rendered output (the Embedding provider row directly
		// below is the forward-looking signal). The label change is
		// in service_doctor.go's Check.Name; the message stays in
		// passive voice so it doesn't claim current provider state.
		check.Name = "Stored embeddings"
		check.Message = fmt.Sprintf("%d memories, all provider-generated", total)
	}
	return check
}

// checkEmbeddingProvider reads the canonical EmbeddingConfig and surfaces
// the four provider states per spec §7.1 table.
// Does NOT perform a network probe; reads the cached config snapshot.
func (s *DoctorService) checkEmbeddingProvider() DoctorCheck {
	check := DoctorCheck{Name: "Embedding provider"}
	cfg := mpminternal.DefaultEmbeddingConfig()
	switch {
	case cfg.IntentionallyDisabled:
		check.Status = "PASS"
		check.Message = "intentionally disabled"
		return check
	case cfg.Source == mpminternal.EmbeddingSourceAbsent:
		check.Status = "WARN"
		check.Message = "no embedding provider configured"
		return check
	case cfg.Status == mpminternal.EmbeddingStatusConfigured:
		note := ""
		if cfg.Source == mpminternal.EmbeddingSourceEnvFallback {
			note = " (legacy env fallback)"
		}
		check.Status = "PASS"
		check.Message = fmt.Sprintf("provider %q reachable%s", cfg.ProviderName, note)
		return check
	case cfg.Status == mpminternal.EmbeddingStatusUnreachable:
		check.Status = "WARN"
		check.Message = fmt.Sprintf("provider %q unreachable: %v", cfg.ProviderName, cfg.LastError)
		return check
	case cfg.Status == mpminternal.EmbeddingStatusMisconfigured:
		check.Status = "WARN"
		check.Message = fmt.Sprintf("provider misconfigured: %v", cfg.LastError)
		return check
	}
	check.Status = "WARN"
	check.Message = "unknown embedding state"
	return check
}

// checkWorkingContextOrphans counts scratchpads past decay_at.
// OK if 0, WARN if 1-2, FAIL if many.
// P3 fix: use integer epoch comparison (decay_at is INTEGER Unix seconds),
// not string comparison against 'YYYY-MM-DD HH:MM:SS' which misclassifies.
func (s *DoctorService) checkWorkingContextOrphans() DoctorCheck {
	check := DoctorCheck{Name: "Working Context"}
	var n int
	err := s.dm.QueryRowTracked(
		`SELECT COUNT(*) FROM ephemeral_scratchpad WHERE decay_at IS NOT NULL AND decay_at < CAST(strftime('%s','now') AS INTEGER)`,
	).Scan(&n)
	if err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not query scratchpad: %v", err)
		return check
	}
	if n == 0 {
		check.Status = "PASS"
		check.Message = "no orphaned scratchpads"
		return check
	}
	if n <= 2 {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("%d scratchpad(s) past decay_at — promote or clear", n)
		check.Details = []string{"Run 'mpm ops gc' or individually 'mpm work show --session-id <id>' + 'promote|clear'."}
		return check
	}
	check.Status = "FAIL"
	check.Message = fmt.Sprintf("%d scratchpads past decay_at — cleanup isn't running", n)
	check.Details = []string{"Run 'mpm ops gc --shred-negative' to clear stale ephemeral state."}
	return check
}

// checkScheduler inspects scheduler daemon health: cron-wake retention,
// scheduler.state.json heartbeat (uptime, last-tick liveness). This is
// the DAEMON HEALTH signal — distinct from the Wake backlog check,
// which surfaces application-domain work that should have run by now.
//
// Final release-pass correction (2026-09-14): the previous code combined
// the daemon-health verdict with the actionable-overdue verdict, so a
// healthy scheduler with a domain-work backlog was reported as a
// scheduler daemon failure. The two are now separate DoctorCheck rows.
func (s *DoctorService) checkScheduler() DoctorCheck {
	check := DoctorCheck{Name: "Scheduler"}

	// Cron-retention interpretation. Always computed (best-effort) so
	// the diagnostic surface is informative even when state-file or
	// HealthCheck are unavailable.
	check.CronRetention = s.cronRetentionStatus()

	// Daemon-only verdict. HealthCheck is used purely for the cron
	// retention surface here; actionable-overdue is reported by
	// checkWakeBacklog.
	hc, err := s.dm.HealthCheck()
	if err != nil {
		check.Status = cronRetentionToStatus(check.CronRetention)
		check.Message = cronRetentionToMessage(check.CronRetention)
		return check
	}

	// Cron retention severity alone drives the Scheduler row status.
	check.Status = cronRetentionToStatus(check.CronRetention)
	check.Message = cronRetentionToMessage(check.CronRetention)
	_ = hc
	return check
}

// checkWakeBacklog counts actionable overdue wakes (domain work that
// should have run by now and hasn't). Distinct from checkScheduler:
// a healthy scheduler daemon with a backlog is a domain-work concern,
// not a daemon failure. The verdict caps at WARN regardless of count.
func (s *DoctorService) checkWakeBacklog() DoctorCheck {
	check := DoctorCheck{Name: "Wake backlog"}
	hc, err := s.dm.HealthCheck()
	if err != nil {
		check.Status = "WARN"
		check.Message = "could not query wake backlog"
		return check
	}
	overdue, _ := hc["wakes_overdue"].(int64)
	_, msg, details := actionableOverdueVerdict(overdue)
	check.Status = overdueSeverityToStatus(statusSeverityForOverdue(overdue))
	check.Message = msg
	check.Details = details
	return check
}

// statusSeverityForOverdue maps the actionable-overdue count to a
// severity tier (0=PASS, 1=WARN). Backlog is never FAIL — a daemon can
// have an enormous backlog while remaining healthy.
func statusSeverityForOverdue(overdue int64) int {
	if overdue == 0 {
		return 0
	}
	return 1
}

// cronRetentionStatus computes the diagnostic-contract fields from the
// scheduler.state.json heartbeat plus a single bounded DB scan over
// scheduled_wakes. Constants come from internal/scheduler — never
// duplicated here.
func (s *DoctorService) cronRetentionStatus() *CronRetentionStatus {
	now := time.Now().Unix()
	out := &CronRetentionStatus{
		RetentionWindowSec: int64(scheduler.CronRetentionWindow.Seconds()),
		SweepCadenceSec:    int64(scheduler.CronRetentionCadence.Seconds()),
		NormalLimit:        scheduler.CronRetentionNormalLimit,
		CatchUpLimit:       scheduler.CronRetentionCatchUpLimit,
	}

	// Pending cron count (cheap single COUNT).
	if pending, err := s.cronPendingCount(); err == nil {
		out.Pending = pending
	}
	// Eligible cron backlog (bounded — LIMIT CronRetentionCatchUpLimit+1).
	if backlog, err := s.cronEligibleBacklog(now); err == nil {
		out.EligibleBacklog = backlog
	}

	// Scheduler uptime + last-tick liveness from scheduler.state.json.
	startedAt, lastTick, stateErr := readSchedulerStateHeartbeat(schedulerStatePath())
	if stateErr == nil && startedAt > 0 {
		out.SchedulerUptimeSec = now - startedAt
		// Cadence-derived last/next expected sweep — NOT the actual
		// last sweep (which is in-process and not persisted). The
		// diagnostic surfaces the SCHEDULED sweep boundaries; if the
		// daemon actually failed to a sweep, the live cron row count
		// will surface it via EligibleBacklog, not via the heartbeat.
		cadenceSec := int64(scheduler.CronRetentionCadence.Seconds())
		sweepCount := (now - startedAt) / cadenceSec
		lastExpectedSweep := startedAt + sweepCount*cadenceSec
		nextExpectedSweep := lastExpectedSweep + cadenceSec
		out.LastExpectedSweepAgoSec = now - lastExpectedSweep
		out.SecondsUntilNextExpectedSweep = nextExpectedSweep - now

		// Phase classification.
		if out.SchedulerUptimeSec < 2*cadenceSec {
			out.Phase = "startup_stabilization"
			out.Interpretation = "Recent cron wakes are intentionally retained. The first actual row retirement normally occurs around two hours after daemon start under the current hourly cadence and strict-< cutoff."
		} else {
			out.Phase = "steady"
			out.Interpretation = "Approximately one retention-window of recent cron wakes is expected at steady state; backlog pulses between sweeps."
		}

		// Stalled-detection. If last_tick is too old, flag FAIL.
		if lastTick > 0 {
			lastTickAge := now - lastTick
			if lastTickAge > 5*60 {
				// last tick > 5 min ago — the heartbeat is the
				// most authoritative liveness signal the cron
				// sweep relies on. Surface as a system-level
				// degraded verdict via Interpretation.
				if out.Interpretation != "" {
					out.Interpretation += " "
				}
				out.Interpretation += fmt.Sprintf("Scheduler last tick was %ds ago — daemon may be stalled.", lastTickAge)
			}
		}
	} else {
		// State file unreadable. Don't fabricate uptime. Phase stays empty.
		out.Phase = ""
	}
	return out
}

// cronPendingCount counts cron-kind scheduled_wakes with fired=0.
// Excludes fired rows because they're already retired.
func (s *DoctorService) cronPendingCount() (int, error) {
	var n int
	if err := s.dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM scheduled_wakes
		WHERE fired = 0 AND json_extract(metadata, '$.kind') = 'cron'
	`).Scan(&n); err != nil {
		return 0, fmt.Errorf("cron pending count: %w", err)
	}
	return n, nil
}

// cronEligibleBacklog counts cron-kind scheduled_wakes whose
// target_time < cutoff (strict-< matches production semantics). The
// count is naturally bounded by elapsed_time × production_rate; even
// in runaway scenarios, an unbounded COUNT(*) is sub-millisecond on
// the cron rows because the idx_sw_pending partial index covers the
// (fired=0, target_time) lookup. The internal cron-retention sweep
// query uses LIMIT for its own cap; the diagnostic does not need it.
func (s *DoctorService) cronEligibleBacklog(nowUnix int64) (int, error) {
	cutoff := nowUnix - int64(scheduler.CronRetentionWindow.Seconds())
	var n int
	if err := s.dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time < ?
		  AND json_extract(metadata, '$.kind') = 'cron'
	`, cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("cron eligible backlog: %w", err)
	}
	return n, nil
}

// cronRetentionToStatus maps the retention fields onto PASS/WARN/FAIL.
// Classification ladder:
//
//	PASS — eligible backlog at or below the normal sweep capacity (60).
//	       A single normal sweep can clear the entire pool.
//
//	WARN — eligible backlog above normal capacity. The system has
//	       accumulated retention debt beyond what a single sweep can
//	       retire. Sustained WARN means multiple sweeps have been
//	       missed OR catch-up mode has been engaged (≥ 240).
//
//	FAIL — eligible backlog > 2 * CronRetentionCatchUpLimit (360).
//	       Multiple sweeps have been missed and the daemon is not
//	       retiring rows at the expected rate.
//
// Limitation: the diagnostic does not persist lastSweepUnix, so it
// cannot directly distinguish "between-sweep pulse with backlog
// just over 60" from "persistent retention debt". The classification
// uses capacity thresholds honestly — eligible > 60 means "at least
// one normal sweep couldn't clear the backlog" — and surfaces the
// limitation via the Phase + interpretation fields.
func cronRetentionToStatus(c *CronRetentionStatus) string {
	if c == nil {
		return "WARN"
	}
	// Stalled detector: if Interpretation mentions stalled, it's FAIL.
	if c.Interpretation != "" && containsStalledMarker(c.Interpretation) {
		return "FAIL"
	}
	if c.EligibleBacklog > 2*scheduler.CronRetentionCatchUpLimit {
		return "FAIL"
	}
	if c.EligibleBacklog >= scheduler.CronRetentionCatchUpThreshold {
		return "WARN"
	}
	if c.Phase == "steady" && c.EligibleBacklog > scheduler.CronRetentionNormalLimit {
		return "WARN"
	}
	return "PASS"
}

func containsStalledMarker(s string) bool {
	return strings.Contains(s, "stalled")
}

// cronRetentionToMessage renders the human-readable one-liner. Full
// detail lines go in Details.
func cronRetentionToMessage(c *CronRetentionStatus) string {
	if c == nil {
		return "scheduler state unavailable"
	}
	switch c.Phase {
	case "startup_stabilization":
		if c.EligibleBacklog > 0 {
			return fmt.Sprintf("cron wake retention healthy — startup stabilization (backlog %d will be retired at next sweep)", c.EligibleBacklog)
		}
		return "cron wake retention healthy — startup stabilization"
	case "steady":
		if c.EligibleBacklog > 0 {
			return fmt.Sprintf("cron wake retention healthy — %d rows eligible (next sweep in ~%dm)", c.EligibleBacklog, c.SecondsUntilNextExpectedSweep/60)
		}
		return "cron wake retention healthy — steady"
	default:
		return "scheduler state file unreadable — using actionable-overdue heuristic only"
	}
}

// actionableOverdueVerdict surfaces wake backlog as a domain work
// concern, NOT a scheduler daemon failure. Final release-pass
// correction (2026-09-13): the previous code conflated the two —
// "5+ wakes overdue" was reported as FAIL with the suggestion to
// restart mpm-scheduler. That conflation misleads operators: the
// scheduler daemon can be healthy (steady uptime, healthy sweep
// cadence, no cron backlog) while domain work is overdue. The
// scheduler daemon's own health is reported separately by the
// CronRetentionStatus fields (uptime, last sweep, etc.).
//
// Overdue wakes here mean: there are domain tasks (cascade reconciles,
// review reminders) that should have run by now and haven't. They
// are a backlog, not a daemon failure. The verdict caps at WARN
// regardless of count; an empty backlog is PASS.
func actionableOverdueVerdict(overdue int64) (int, string, []string) {
	if overdue == 0 {
		return 0, "no overdue wakes", nil
	}
	if overdue <= 5 {
		return 1, fmt.Sprintf("%d wake(s) overdue", overdue),
			[]string{"Domain work backlog — not a scheduler daemon failure. Run 'mpm ops review --stale' to triage."}
	}
	return 1, fmt.Sprintf("%d wakes overdue", overdue),
		[]string{"Domain work backlog — not a scheduler daemon failure. Run 'mpm ops review --stale' to triage."}
}

func statusSeverity(status string) int {
	switch status {
	case "PASS":
		return 0
	case "WARN":
		return 1
	default:
		return 2
	}
}

func overdueSeverityToStatus(sev int) string {
	switch sev {
	case 0:
		return "PASS"
	case 1:
		return "WARN"
	default:
		return "FAIL"
	}
}

// checkReviewBacklog counts memories due for spaced reinforcement
// (last_accessed_at IS NULL or > 14 days).
func (s *DoctorService) checkReviewBacklog() DoctorCheck {
	check := DoctorCheck{Name: "Review backlog"}
	items, err := s.dm.GetSpacedReinforcementReview(14, 100)
	if err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not query review candidates: %v", err)
		return check
	}
	n := len(items)
	if n == 0 {
		check.Status = "PASS"
		check.Message = "no memories due for review (last-accessed < 14d threshold)"
		return check
	}
	if n <= 5 {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("%d memories due for spaced review", n)
		check.Details = []string{"Run 'mpm ops review' to clear the backlog."}
		return check
	}
	check.Status = "WARN"
	check.Message = fmt.Sprintf("%d memories due for spaced review (>5 considered backlog)", n)
	check.Details = []string{"Run 'mpm ops review --stale' to clear the backlog."}
	return check
}

// schedulerStatePath returns the canonical scheduler.state.json path.
// Indirected through a package variable so tests can override it without
// touching the production MPM_WORKSPACE / scheduler.StateFilePath()
// machinery — important because the live scheduler.state.json is
// written by a separate process (mpm-scheduler) and overwriting it
// from a unit test would corrupt live state.
var schedulerStatePath = func() string { return scheduler.StateFilePath() }

// readSchedulerStateHeartbeat reads the canonical scheduler.state.json
// (written by internal/scheduler.persistState on every tick) and
// returns process_started_unix + last_tick_unix. The diagnostic uses
// process_started_unix for uptime and last_tick_unix for liveness;
// it does NOT use lastSweepUnix (not persisted by design — see
// internal/scheduler/wake_expiration.go: cronRetentionState is in-process).
//
// Returns (0, 0, err) when the file is unreadable. Callers must handle
// the zero case as "state unavailable" rather than fabricating uptime.
func readSchedulerStateHeartbeat(path string) (int64, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var s struct {
		LastTickUnix       int64 `json:"last_tick_unix"`
		ProcessStartedUnix int64 `json:"process_started_unix"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, 0, err
	}
	return s.ProcessStartedUnix, s.LastTickUnix, nil
}
