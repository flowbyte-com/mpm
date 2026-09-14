// cmd/mpm/renderer_why.go — WhyRenderer for Wave 2.
//
// Renders a WhyReport as a multi-section provenance answer.
//
// Layering contract (RFC §7):
//   Renderer consumes WhyReport (built by WhyService). NEVER calls
//   services, NEVER calls commands. Pure presentation.
//
// Migrated to the shared render package (cmd/mpm/render/) so the
// heading token, dim secondary text, and section labels use the
// canonical visual contract. The Why report is information-dense —
// no emoji markers; paragraphs read as explanation. Status markers
// are reserved for the Doctor dashboard.

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
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

	// Heading + timestamp routed through the shared render package.
	if err := render.Heading(r.out, "Why"); err != nil {
		return err
	}
	if err := render.Timestamp(r.out, report.GeneratedAt.Format("2006-01-02 15:04:05 UTC")); err != nil {
		return err
	}
	if err := render.BlankLine(r.out); err != nil {
		return err
	}

	if report.SkipReason != "" {
		if _, err := fmt.Fprintln(r.out, report.SkipReason); err != nil {
			return err
		}
		return nil
	}

	if err := render.Label(r.out, "Artifact", report.ArtifactID); err != nil {
		return err
	}
	if err := render.Label(r.out, "Kind", report.ArtifactKind); err != nil {
		return err
	}
	if err := render.BlankLine(r.out); err != nil {
		return err
	}

	if report.Identity != nil {
		if err := render.Section(r.out, "Identity"); err != nil {
			return err
		}
		r.renderIdentity(report.Identity)
		if err := render.BlankLine(r.out); err != nil {
			return err
		}
	}

	if report.Provenance != nil {
		if err := render.Section(r.out, "Provenance"); err != nil {
			return err
		}
		r.renderProvenance(report.Provenance)
		if err := render.BlankLine(r.out); err != nil {
			return err
		}
	}

	if err := render.Section(r.out, fmt.Sprintf("Evidence (%d observations)", report.EvidenceCount)); err != nil {
		return err
	}
	r.renderEvidence(report.EvidenceRows)
	if err := render.BlankLine(r.out); err != nil {
		return err
	}

	if err := render.Section(r.out, fmt.Sprintf("Confidence history (%d events)", len(report.ConfidenceHistory))); err != nil {
		return err
	}
	r.renderConfidence(report.ConfidenceHistory)
	if err := render.BlankLine(r.out); err != nil {
		return err
	}

	if report.Retrieval != nil {
		if err := render.Section(r.out, "Retrieval"); err != nil {
			return err
		}
		r.renderRetrieval(report.Retrieval)
		if err := render.BlankLine(r.out); err != nil {
			return err
		}
	}
	return nil
}

func (r *WhyRenderer) renderIdentity(id *WhyIdentity) {
	if id == nil {
		render.Plain(r.out, "  (none)")
		return
	}
	preview := id.Content
	if len(preview) > 240 {
		preview = preview[:240] + "…"
	}
	render.Label(r.out, "content", preview)
	if len(id.Tags) > 0 {
		render.Label(r.out, "tags", strings.Join(id.Tags, ", "))
	}
	ltm := "no"
	if id.IsLTM {
		ltm = "yes"
	}
	render.Label(r.out, "weight", fmt.Sprintf("%d    LTM: %s", id.Weight, ltm))
}

func (r *WhyRenderer) renderProvenance(p *WhyProvenance) {
	if p == nil {
		render.Plain(r.out, "  (none)")
		return
	}
	render.Label(r.out, "created", formatTSOr(p.CreatedAt, "(unknown)"))
	render.Label(r.out, "updated", formatTSOr(p.UpdatedAt, "(unknown)"))
	if p.LastAccessed != nil {
		render.Label(r.out, "accessed", p.LastAccessed.Format("2006-01-02 15:04:05 UTC"))
	} else {
		render.Label(r.out, "accessed", "(never)")
	}
	if p.SessionID != "" {
		render.Label(r.out, "session_id", p.SessionID)
	}
	render.Label(r.out, "framework", orUnknown(p.FrameworkName))
	render.Label(r.out, "adapter", orUnknown(p.FrameworkAdapter))
	render.Label(r.out, "model", orUnknown(p.ModelName))
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
		render.Plain(r.out, "  (no evidence attached)")
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
		render.Plain(r.out, "  (no confidence events recorded)")
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
	render.Label(r.out, "reuse_count", fmt.Sprintf("%d", ret.ReuseCount))
	render.Label(r.out, "success_count", fmt.Sprintf("%d", ret.SuccessCount))
	if ret.LastRetrievedAt != nil {
		render.Label(r.out, "last_retrieved", mpminternal.FormatUnixSeconds(*ret.LastRetrievedAt))
	} else {
		render.Label(r.out, "last_retrieved", "(never)")
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
