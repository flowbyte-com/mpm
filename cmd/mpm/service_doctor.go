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
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
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
	report.Checks = append(report.Checks, s.checkWorkingContextOrphans())
	report.Checks = append(report.Checks, s.checkScheduler())
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

// checkEmbeddings counts memories without embeddings.
// WARN if >= 10% of active memories are missing embeddings.
func (s *DoctorService) checkEmbeddings() DoctorCheck {
	check := DoctorCheck{Name: "Embeddings"}
	var total, missing int
	if err := s.dm.QueryRowTracked(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`,
	).Scan(&total); err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not count memories: %v", err)
		return check
	}
	if err := s.dm.QueryRowTracked(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND embedding IS NULL`,
	).Scan(&missing); err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not count missing embeddings: %v", err)
		return check
	}
	if total == 0 {
		check.Status = "PASS"
		check.Message = "no memories to embed"
		return check
	}
	pct := (missing * 100) / total
	if missing == 0 {
		check.Status = "PASS"
		check.Message = fmt.Sprintf("%d memories, all embedded", total)
		return check
	}
	if pct >= 10 {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("%d / %d missing (%d%%) — semantic search degraded", missing, total, pct)
		check.Details = []string{"Run 'mpm ops backfill-embeddings' to fill the gaps."}
		return check
	}
	check.Status = "PASS"
	check.Message = fmt.Sprintf("%d / %d missing (%d%%) — under the 10%% threshold", missing, total, pct)
	return check
}

// checkWorkingContextOrphans counts scratchpads past decay_at.
// OK if 0, WARN if 1-2, FAIL if many.
func (s *DoctorService) checkWorkingContextOrphans() DoctorCheck {
	check := DoctorCheck{Name: "Working Context"}
	var n int
	err := s.dm.QueryRowTracked(
		`SELECT COUNT(*) FROM ephemeral_scratchpad WHERE decay_at < strftime('%Y-%m-%d %H:%M:%S', 'now')`,
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

// checkScheduler inspects overdue wakes via HealthCheck.
func (s *DoctorService) checkScheduler() DoctorCheck {
	check := DoctorCheck{Name: "Scheduler"}
	hc, err := s.dm.HealthCheck()
	if err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("health check unavailable: %v", err)
		return check
	}
	overdue, _ := hc["wakes_overdue"].(int64)
	if overdue == 0 {
		check.Status = "PASS"
		check.Message = "no overdue wakes"
		return check
	}
	if overdue <= 5 {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("%d wake(s) overdue", overdue)
		check.Details = []string{"Check 'mpm status' for daemon health; 'journalctl --user -u mpm-scheduler' if needed."}
		return check
	}
	check.Status = "FAIL"
	check.Message = fmt.Sprintf("%d wakes overdue — scheduler may be stalled", overdue)
	check.Details = []string{"Restart mpm-scheduler: 'systemctl --user restart mpm-scheduler'."}
	return check
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
