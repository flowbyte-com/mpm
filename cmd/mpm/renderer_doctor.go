// cmd/mpm/renderer_doctor.go — DoctorRenderer for Wave 2.
//
// Renders the legacy DoctorReport as a single-screen dashboard with
// ✓ / ⚠ / ✗ markers (TTY) or [PASS]/[WARN]/[FAIL] text labels (non-TTY).
//
// Layering contract (RFC §7):
//   Renderer consumes DoctorReport (built by DoctorService). NEVER
//   calls services, NEVER calls commands. Pure presentation.
//
// All rendering goes through the shared render package — the inline
// lipgloss.NewStyle calls remaining here are the cron-retention block
// (dim-gray prose) and the canonical heading token, both mirrored
// from render.Heading / render.Hint for consistency.
//
// TTY detection happens in the handler before the renderer is
// constructed so the renderer's behaviour matches the channel.

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// DoctorRenderer writes a DoctorReport to a stream.
type DoctorRenderer struct {
	out      io.Writer
	useEmoji bool
}

// NewDoctorRenderer writes to out (or io.Discard if nil). When useEmoji
// is true, renders ✓/⚠/✗ markers (TTY); when false, uses [PASS]/[WARN]/
// [FAIL] text labels (log files / CI / agent log capture).
func NewDoctorRenderer(out io.Writer, useEmoji bool) *DoctorRenderer {
	if out == nil {
		out = io.Discard
	}
	return &DoctorRenderer{out: out, useEmoji: useEmoji}
}

// Render writes the DoctorReport to the renderer's writer.
//
// Output shape:
//
//	MPM · Doctor
//	Generated <timestamp>
//
//	✓ ALL PASS   (or ⚠ WARNINGS or ✗ FAILURES followed by count)
//
//	✓  Database          integrity ok | 19 pages | 0 busy retries
//	⚠  Embeddings        12 / 290 missing (4%) — under threshold
//	     → Run 'mpm ops backfill-embeddings' to fill the gaps.
//	✗  Working Context   7 scratchpads past decay_at — cleanup not running
//	     → Run 'mpm ops gc --shred-negative'.
//
// The overall status is computed from the report's PASS/WARN/FAIL tally.
// The per-row markers mirror the per-check status.
func (r *DoctorRenderer) Render(report *DoctorReport) error {
	if report == nil {
		return fmt.Errorf("nil doctor report")
	}

	// Heading + timestamp routed through the shared render package
	// so the heading token stays canonical across the product.
	if err := render.Heading(r.out, "Doctor"); err != nil {
		return err
	}
	if err := render.Timestamp(r.out, nowRFC3339()); err != nil {
		return err
	}
	if err := render.BlankLine(r.out); err != nil {
		return err
	}

	// Overall status from counts.
	overallStatus := "PASS"
	if report.Failed > 0 {
		overallStatus = "FAIL"
	} else if report.Warnings > 0 {
		overallStatus = "WARN"
	}
	if err := render.Marker(r.out, overallStatus); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(r.out, " %s\n", overallSummary(report, r.useEmoji)); err != nil {
		return err
	}
	if err := render.BlankLine(r.out); err != nil {
		return err
	}

	for _, check := range report.Checks {
		if err := render.CheckRow(r.out, check.Status, check.Name, check.Message, check.Details); err != nil {
			return err
		}
		// Cron-retention interpretation: surface the diagnostic-contract
		// fields (pending, eligible, retention phase, scheduler uptime)
		// so a fresh agent can distinguish startup stabilization from
		// steady state from genuine degradation without reading source.
		if cr := check.CronRetention; cr != nil {
			r.renderCronRetention(cr)
		}
	}

	// Additive observability synthesis. Each section is wrapped
	// in a degraded-safe renderer (Unavailability sentinel shows
	// the failure without crashing Doctor).
	if report.Usage != nil {
		if err := r.renderUsage(report.Usage); err != nil {
			return err
		}
	}
	if report.Attention != nil {
		if err := r.renderAttention(report.Attention); err != nil {
			return err
		}
	}
	return nil
}

// renderUsage emits the Doctor Usage section. Bounded facts; no
// full-table dumps. All counts come from indexed tool_invocations
// reads. Empty / new installs render as zeros, never as fake data.
func (r *DoctorRenderer) renderUsage(u *DoctorUsage) error {
	if err := render.Heading(r.out, "Usage"); err != nil {
		return err
	}
	if err := render.BlankLine(r.out); err != nil {
		return err
	}
	if u.Unavailable != nil {
		fmt.Fprintf(r.out, "  observability history unavailable: %s\n", u.Unavailable.Message)
		return render.BlankLine(r.out)
	}
	fmt.Fprintf(r.out, "  Tool calls          %d today · %d in 7d\n",
		u.Window24hInvocations, u.Window7dInvocations)
	if len(u.FrameworksObserved) > 0 {
		fmt.Fprintf(r.out, "  Frameworks          %s\n", strings.Join(u.FrameworksObserved, " · "))
	}
	if len(u.MostUsedTools) > 0 {
		parts := make([]string, 0, len(u.MostUsedTools))
		for _, mt := range u.MostUsedTools {
			parts = append(parts, fmt.Sprintf("%s (%d)", mt.Tool, mt.Count))
		}
		fmt.Fprintf(r.out, "  Most used           %s\n", strings.Join(parts, " · "))
	}
	if len(u.OutcomeDistribution) > 0 || u.HistoricalUnclassified > 0 {
		var parts []string
		// Canonical ordering — same as AllOutcomeClasses.
		for _, class := range mpminternal.AllOutcomeClasses {
			if c, ok := u.OutcomeDistribution[string(class)]; ok && c > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", class, c))
			}
		}
		if u.HistoricalUnclassified > 0 {
			parts = append(parts, fmt.Sprintf("historical/unclassified %d", u.HistoricalUnclassified))
		}
		if len(parts) > 0 {
			fmt.Fprintf(r.out, "  Outcomes            %s\n", strings.Join(parts, " · "))
		}
	}
	exposeLabel := fmt.Sprintf("%d exposed · %d registered", u.ExposedTools, u.RegisteredTools)
	if u.MCPExposeAllEnv {
		exposeLabel += " (MPM_EXPOSE_ALL_TOOLS=1)"
	}
	fmt.Fprintf(r.out, "  MCP tools           %s\n", exposeLabel)
	return render.BlankLine(r.out)
}

// renderAttention emits the Doctor Attention section. Three sub-areas:
// operational issues (bounded recent substrate/integration/timeout/internal
// events), audit clusters (high-volume bounded), and security policy
// (deliberate blocks — informational only, NOT MPM failures).
func (r *DoctorRenderer) renderAttention(a *DoctorAttention) error {
	if err := render.Heading(r.out, "Attention"); err != nil {
		return err
	}
	if err := render.BlankLine(r.out); err != nil {
		return err
	}
	if a.Unavailable != nil {
		fmt.Fprintf(r.out, "  observability history unavailable: %s\n", a.Unavailable.Message)
		return render.BlankLine(r.out)
	}
	fmt.Fprintf(r.out, "  Operational issues  %d in 7d\n", a.OperationalIssues7d)
	for _, ev := range a.OperationalEvents {
		if ev.EventCode != "" {
			fmt.Fprintf(r.out, "    %s · %s · %d occurrences · last seen %s\n",
				ev.Component, ev.EventCode, ev.Count,
				formatUnixTimeAgo(ev.LastSeen))
		} else {
			fmt.Fprintf(r.out, "    %s · %d occurrences · last seen %s\n",
				ev.Component, ev.Count, formatUnixTimeAgo(ev.LastSeen))
		}
	}
	fmt.Fprintf(r.out, "  Audit clusters       %d active\n", len(a.AuditClusters))
	for _, cl := range a.AuditClusters {
		fmt.Fprintf(r.out, "    %s · %d occurrences · last seen %s\n",
			cl.Component, cl.Count, formatUnixTimeAgo(cl.LastSeen))
	}
	if a.SecurityEvents7d > 0 {
		fmt.Fprintf(r.out, "  Security policy      %d blocked sensitive writes in 7d\n",
			a.SecurityEvents7d)
	}
	return render.BlankLine(r.out)
}

// formatUnixTimeAgo renders a unix timestamp as a relative-time
// string ("5m ago" / "3h ago" / "2d ago"). Zero means unknown.
func formatUnixTimeAgo(unix int64) string {
	if unix <= 0 {
		return "unknown"
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// renderCronRetention emits a compact, human-readable block of the
// cron-retention diagnostic under a Scheduler check. Always renders
// when CronRetention is present (PASS or otherwise) — the operator's
// signal is the Phase/EligibleBacklog context, not just the verdict.
func (r *DoctorRenderer) renderCronRetention(cr *CronRetentionStatus) {
	indent := "        "
	if cr.Phase != "" {
		fmt.Fprintf(r.out, "%sCron retention phase: %s\n", indent, cr.Phase)
	}
	if cr.SchedulerUptimeSec > 0 {
		fmt.Fprintf(r.out, "%sScheduler uptime: %s\n", indent,
			formatSchedulerDuration(time.Duration(cr.SchedulerUptimeSec)*time.Second))
	}
	fmt.Fprintf(r.out, "%sPending cron wakes: %d · eligible backlog: %d · retention window: %s · cadence: %s\n",
		indent, cr.Pending, cr.EligibleBacklog,
		formatSchedulerDuration(time.Duration(cr.RetentionWindowSec)*time.Second),
		formatSchedulerDuration(time.Duration(cr.SweepCadenceSec)*time.Second),
	)
	if cr.LastExpectedSweepAgoSec > 0 {
		fmt.Fprintf(r.out, "%sLast expected sweep: %s ago · next expected sweep: %s\n",
			indent,
			formatSchedulerDuration(time.Duration(cr.LastExpectedSweepAgoSec)*time.Second),
			formatSchedulerDuration(time.Duration(cr.SecondsUntilNextExpectedSweep)*time.Second),
		)
	}
	if cr.Interpretation != "" {
		render.Hint(r.out, cr.Interpretation)
	}
}

// formatSchedulerDuration formats a Duration in compact human form.
// "1h 25m" / "25m 14s" / "14s". Zero or negative values render as "—".
func formatSchedulerDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// markerFor returns ✓ (PASS), ⚠ (WARN), ✗ (FAIL), or ○ (INFO) when
// useEmoji is true; "[PASS]"/"[WARN]"/"[FAIL]"/"[INFO]" otherwise.
// "INFO" is the neutral status for absent optional features
// (e.g. embedding model when embeddings are optional); the marker
// does NOT imply a successful check.
func markerFor(status string, useEmoji bool) string {
	if useEmoji {
		switch status {
		case "PASS":
			return "✓"
		case "WARN":
			return "⚠"
		case "FAIL":
			return "✗"
		case "INFO":
			return "○"
		}
	}
	return fmt.Sprintf("[%s]", status)
}

// overallSummary returns the overall-summary line shown after the
// per-section markers. Examples:
//
//	PASS — all 5 checks healthy
//	WARN — 2 warnings, 3 passed (no failures)
//	FAIL — 1 failure, 1 warning, 3 passed
//
// 2026-09-14 release-pass: informational checks (status=INFO) are
// excluded from the tally so an absent optional feature does not
// appear unhealthy. Informational count is appended to the
// summary line so the operator can still see it.
func overallSummary(report *DoctorReport, useEmoji bool) string {
	total := report.TotalChecks
	if total == 0 {
		return "no checks ran"
	}
	infoSuffix := ""
	if report.Informational > 0 {
		infoSuffix = fmt.Sprintf(" · %d informational", report.Informational)
	}
	if report.Failed > 0 {
		return fmt.Sprintf("%d failure%s, %d warning%s, %d passed%s",
			report.Failed, plural(report.Failed),
			report.Warnings, plural(report.Warnings),
			report.Passed, infoSuffix)
	}
	if report.Warnings > 0 {
		return fmt.Sprintf("%d warning%s, %d passed%s",
			report.Warnings, plural(report.Warnings),
			report.Passed, infoSuffix)
	}
	return fmt.Sprintf("all %d checks healthy%s", total, infoSuffix)
}

// plural returns "s" for non-1 counts; "" for 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// nowRFC3339 returns the current UTC time in RFC3339 format.
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// (Nothing else needed in this file — the renderer is intentionally
// minimal. Any future presentation concerns land here.)
