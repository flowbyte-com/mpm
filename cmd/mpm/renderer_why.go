// cmd/mpm/renderer_why.go — WhyRenderer for Wave 2.
//
// Renders a WhyReport as a multi-section provenance answer.
//
// Layering contract (RFC §7):
//   Renderer consumes WhyReport (built by WhyService). NEVER calls
//   services, NEVER calls commands. Pure presentation.
//
// The render is intentionally text-mode (no emoji markers) — the
// why report is information-dense and emoji-on-every-line makes it
// harder to read. The DoctorRenderer uses ✓/⚠/✗ because it's a
// glanceable dashboard; the WhyRenderer writes paragraphs because
// it's an explanation.

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	mpminternal "github.com/flowbyte-com/mpm-core"
)

// WhyRenderer writes a WhyReport to a stream.
type WhyRenderer struct {
	out io.Writer
}

// NewWhyRenderer writes to out (or io.Discard if nil).
func NewWhyRenderer(out io.Writer) *WhyRenderer {
	if out == nil {
		out = io.Discard
	}
	return &WhyRenderer{out: out}
}

// Render writes the WhyReport. Sections:
//
//   - Identity       (id, kind, content preview, weight, LTM flag)
//   - Provenance     (created/updated/last-accessed, session)
//   - Evidence       (count + top-N rows)
//   - Confidence     (recent history, top-N)
//   - Retrieval      (reuse + success counts + last retrieved)
//   - Skip reason    (only when artifact not found)
//
// Sections with no data render as "(none)" so the operator always
// sees the dashboard shape regardless of how many sections returned
// data.
func (r *WhyRenderer) Render(report *WhyReport) error {
	if report == nil {
		return fmt.Errorf("nil why report")
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700"))
	subtitle := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
	section := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700"))

	if _, err := fmt.Fprintln(r.out, title.Render("MPM · Why")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(r.out, "%s\n\n", subtitle.Render(
		report.GeneratedAt.Format("2006-01-02 15:04:05 UTC"))); err != nil {
		return err
	}

	if report.SkipReason != "" {
		muted := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
		fmt.Fprintln(r.out, muted.Render(report.SkipReason))
		return nil
	}

	fmt.Fprintf(r.out, "Artifact: %s\n", report.ArtifactID)
	fmt.Fprintf(r.out, "Kind:     %s\n", report.ArtifactKind)
	fmt.Fprintln(r.out)

	if report.Identity != nil {
		fmt.Fprintln(r.out, section.Render("Identity"))
		r.renderIdentity(report.Identity)
		fmt.Fprintln(r.out)
	}

	if report.Provenance != nil {
		fmt.Fprintln(r.out, section.Render("Provenance"))
		r.renderProvenance(report.Provenance)
		fmt.Fprintln(r.out)
	}

	fmt.Fprintln(r.out, section.Render(fmt.Sprintf("Evidence (%d observations)", report.EvidenceCount)))
	r.renderEvidence(report.EvidenceRows)
	fmt.Fprintln(r.out)

	fmt.Fprintln(r.out, section.Render(fmt.Sprintf("Confidence history (%d events)", len(report.ConfidenceHistory))))
	r.renderConfidence(report.ConfidenceHistory)
	fmt.Fprintln(r.out)

	if report.Retrieval != nil {
		fmt.Fprintln(r.out, section.Render("Retrieval"))
		r.renderRetrieval(report.Retrieval)
		fmt.Fprintln(r.out)
	}
	return nil
}

func (r *WhyRenderer) renderIdentity(id *WhyIdentity) {
	if id == nil {
		fmt.Fprintln(r.out, "  (none)")
		return
	}
	preview := id.Content
	if len(preview) > 240 {
		preview = preview[:240] + "…"
	}
	fmt.Fprintf(r.out, "  content : %s\n", preview)
	if len(id.Tags) > 0 {
		fmt.Fprintf(r.out, "  tags    : %s\n", strings.Join(id.Tags, ", "))
	}
	ltm := "no"
	if id.IsLTM {
		ltm = "yes"
	}
	fmt.Fprintf(r.out, "  weight  : %d    LTM: %s\n", id.Weight, ltm)
}

func (r *WhyRenderer) renderProvenance(p *WhyProvenance) {
	if p == nil {
		fmt.Fprintln(r.out, "  (none)")
		return
	}
	fmt.Fprintf(r.out, "  created    : %s\n", formatTSOr(p.CreatedAt, "(unknown)"))
	fmt.Fprintf(r.out, "  updated    : %s\n", formatTSOr(p.UpdatedAt, "(unknown)"))
	if p.LastAccessed != nil {
		fmt.Fprintf(r.out, "  accessed   : %s\n", p.LastAccessed.Format("2006-01-02 15:04:05 UTC"))
	} else {
		fmt.Fprintln(r.out, "  accessed   : (never)")
	}
	if p.SessionID != "" {
		fmt.Fprintf(r.out, "  session_id : %s\n", p.SessionID)
	}
	fmt.Fprintf(r.out, "  framework  : %s\n", orUnknown(p.FrameworkName))
	fmt.Fprintf(r.out, "  adapter    : %s\n", orUnknown(p.FrameworkAdapter))
	fmt.Fprintf(r.out, "  model      : %s\n", orUnknown(p.ModelName))
}

// orUnknown returns v when non-empty, "(unknown)" otherwise. Used so the
// provenance panel always reads as "complete with NULL fallbacks" rather
// than disappearing entirely when a legacy artifact predates the column.
func orUnknown(v string) string {
	if v == "" {
		return "(unknown)"
	}
	return v
}

func (r *WhyRenderer) renderEvidence(rows []EvidenceRow) {
	if len(rows) == 0 {
		fmt.Fprintln(r.out, "  (no evidence attached)")
		return
	}
	maxRows := 5
	if len(rows) < maxRows {
		maxRows = len(rows)
	}
	for _, row := range rows[:maxRows] {
		fmt.Fprintf(r.out, "  • [%s] strength %.2f — by %s",
			row.Type, row.Strength, row.CreatedBy)
		if !row.CreatedAt.IsZero() {
			fmt.Fprintf(r.out, " (%s)", row.CreatedAt.Format("2006-01-02"))
		}
		fmt.Fprintln(r.out)
		if row.SourceGroup != "" {
			fmt.Fprintf(r.out, "    source: %s\n", row.SourceGroup)
		}
		if row.Notes != "" {
			fmt.Fprintf(r.out, "    notes:  %s\n", truncateForWhy(row.Notes, 80))
		}
	}
	if len(rows) > maxRows {
		fmt.Fprintf(r.out, "  ... and %d more (use 'mpm call mpm_evidence --payload '{\"action\":\"list\",...}' for full list)\n",
			len(rows)-maxRows)
	}
}

func (r *WhyRenderer) renderConfidence(rows []ConfidenceRow) {
	if len(rows) == 0 {
		fmt.Fprintln(r.out, "  (no confidence events recorded)")
		return
	}
	maxRows := 5
	if len(rows) < maxRows {
		maxRows = len(rows)
	}
	for _, row := range rows[:maxRows] {
		fmt.Fprintf(r.out, "  • conf %.3f  evid %d  trigger: %s  at %s\n",
			row.Confidence, row.EvidenceCount, row.Trigger, row.ComputedAt.Format("2006-01-02 15:04"))
	}
}

func (r *WhyRenderer) renderRetrieval(ret *WhyRetrieval) {
	fmt.Fprintf(r.out, "  reuse_count   : %d\n", ret.ReuseCount)
	fmt.Fprintf(r.out, "  success_count : %d\n", ret.SuccessCount)
	if ret.LastRetrievedAt != nil {
		fmt.Fprintf(r.out, "  last_retrieved: %s\n", mpminternal.FormatUnixSeconds(*ret.LastRetrievedAt))
	} else {
		fmt.Fprintln(r.out, "  last_retrieved: (never)")
	}
}

// formatTSOr formats t as ISO date if non-zero; otherwise returns def.
func formatTSOr(t time.Time, def string) string {
	if t.IsZero() {
		return def
	}
	return t.Format("2006-01-02 15:04:05 UTC")
}

// truncateForWhy returns s shortened to max chars with a trailing
// "…" suffix. Used for inline evidence notes so the renderer doesn't
// blow out a single line.
func truncateForWhy(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
