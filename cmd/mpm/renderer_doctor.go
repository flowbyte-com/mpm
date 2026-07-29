// cmd/mpm/renderer_doctor.go — DoctorRenderer for Wave 2.
//
// Renders the legacy DoctorReport as a single-screen dashboard with
// ✓ / ⚠ / ✗ markers (TTY) or [PASS]/[WARN]/[FAIL] text labels (non-TTY).
//
// Layering contract (RFC §7):
//   Renderer consumes DoctorReport (built by DoctorService). NEVER
//   calls services, NEVER calls commands. Pure presentation.
//
// TTY detection happens in the handler before the renderer is
// constructed so the renderer's behaviour matches the channel.

package main

import (
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/lipgloss"
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
//   MPM · Doctor
//   Generated <timestamp>
//
//   ✓ ALL PASS   (or ⚠ WARNINGS or ✗ FAILURES followed by count)
//
//   ✓  Database          integrity ok | 19 pages | 0 busy retries
//   ⚠  Embeddings        12 / 290 missing (4%) — under threshold
//        → Run 'mpm ops backfill-embeddings' to fill the gaps.
//   ✗  Working Context   7 scratchpads past decay_at — cleanup not running
//        → Run 'mpm ops gc --shred-negative'.
//
// The overall status is computed from the report's PASS/WARN/FAIL tally.
// The per-row markers mirror the per-check status.
func (r *DoctorRenderer) Render(report *DoctorReport) error {
	if report == nil {
		return fmt.Errorf("nil doctor report")
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700"))
	if _, err := fmt.Fprintln(r.out, title.Render("MPM · Doctor")); err != nil {
		return err
	}
	subtitle := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
	if _, err := fmt.Fprintln(r.out, subtitle.Render(nowRFC3339())); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(r.out); err != nil {
		return err
	}

	// Overall status from counts.
	overallStatus := "PASS"
	if report.Failed > 0 {
		overallStatus = "FAIL"
	} else if report.Warnings > 0 {
		overallStatus = "WARN"
	}
	if _, err := fmt.Fprintf(r.out, "%s %s\n",
		markerFor(overallStatus, r.useEmoji),
		overallSummary(report, r.useEmoji),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(r.out); err != nil {
		return err
	}

	for _, check := range report.Checks {
		marker := markerFor(check.Status, r.useEmoji)
		if _, err := fmt.Fprintf(r.out, "%s  %-22s %s\n",
			marker,
			check.Name,
			check.Message,
		); err != nil {
			return err
		}
		for _, d := range check.Details {
			hint := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
			if _, err := fmt.Fprintf(r.out, "        %s\n", hint.Render("→ "+d)); err != nil {
				return err
			}
		}
	}
	return nil
}

// markerFor returns ✓ (PASS), ⚠ (WARN), or ✗ (FAIL) when useEmoji is true;
// "[PASS]"/"[WARN]"/"[FAIL]" otherwise.
func markerFor(status string, useEmoji bool) string {
	if useEmoji {
		switch status {
		case "PASS":
			return "✓"
		case "WARN":
			return "⚠"
		case "FAIL":
			return "✗"
		}
	}
	return fmt.Sprintf("[%s]", status)
}

// overallSummary returns the overall-summary line shown after the
// per-section markers. Examples:
//
//   PASS — all 5 checks healthy
//   WARN — 2 warnings, 3 passed (no failures)
//   FAIL — 1 failure, 1 warning, 3 passed
func overallSummary(report *DoctorReport, useEmoji bool) string {
	total := report.TotalChecks
	if total == 0 {
		return "no checks ran"
	}
	if report.Failed > 0 {
		return fmt.Sprintf("%d failure%s, %d warning%s, %d passed",
			report.Failed, plural(report.Failed),
			report.Warnings, plural(report.Warnings),
			report.Passed)
	}
	if report.Warnings > 0 {
		return fmt.Sprintf("%d warning%s, %d passed",
			report.Warnings, plural(report.Warnings),
			report.Passed)
	}
	return fmt.Sprintf("all %d checks healthy", total)
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
