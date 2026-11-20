// cmd/mpm/renderers/dashboard.go — DashboardRenderer for `mpm continue`.
//
// DashboardRenderer is the presentation output for the composite
// `mpm continue` command. It composes multiple per-section views
// (each produced by its own formatter/section-owner) into one
// session-resumption dashboard.
//
// Layering contract (RFC §7):
//   DashboardRenderer NEVER queries SQLite. NEVER calls services.
//   It consumes a DashboardModel — a pre-built data structure
//   produced by ContinueService.Compose.
//
// The reason this layer exists at all (and why it is *not* the same
// as TerminalRenderer):
//   - ContinueService is the *composer* of the dashboard data.
//   - DashboardRenderer is the *writer* of the dashboard layout.
//   - Command handlers do BOTH via tiny adapter code; neither
//     composers nor layouts belong in the command handler itself.
//
// Wave 1 ships a minimal DashboardRenderer — the per-section
// views are simple structured text. Wave 2 may add JSONRenderer
// (alongside DashboardRenderer) for `--json` output. The shape
// of DashboardModel is designed to be JSON-serialisable so the
// JSONEncoder can emit the same dashboard for scripts.

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// DashboardSection is one block of the `mpm continue` output.
// Section name is the visible label (e.g., "Working Context",
// "Recent Decisions"). Body is the already-formatted presentation
// text for that section. Sections in the final output appear in
// the order they were added to the model.
type DashboardSection struct {
	Name string
	Body string
	// Kind is the type tag ("working_context", "wake_context",
	// "decisions", "skills", "theories", "status"). Used by
	// future `--kind` filter on `mpm continue` and by the
	// JSONEncoder for typed output.
	Kind string
}

// DashboardModel is the assembled dashboard. ContinueService builds
// this; DashboardRenderer writes it.
type DashboardModel struct {
	Title    string             // header for the dashboard
	Subtitle string             // optional second line under title
	Sections []DashboardSection // ordered list
	// GeneratedAt is the timestamp ContinueService.Composer ran.
	// Useful for "this dashboard is from N seconds ago" framing.
	GeneratedAt time.Time
}

// WorkingContextView is the per-section view that ContinueService
// builds from WorkingContextService.GetCurrent + WorkingContextFormatter.
// Renderers consume it via the DashboardSection.Body field.
type WorkingContextView struct {
	SessionID  string
	Thesis     string
	Supporting string
	AgeLabel   string
	ExpiresIn  string
}

// CompactMarkdown produces a one-line + body representation suitable
// for the dashboard section. Pure data → presentation transform.
func (v *WorkingContextView) CompactMarkdown() string {
	if v == nil {
		return "  (no working context)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  session : %s\n", v.SessionID)
	fmt.Fprintf(&b, "  thesis  : %s\n", v.Thesis)
	if v.SupportsExpander() {
		fmt.Fprintf(&b, "  details : %s\n", truncate(v.Supporting, 200))
	}
	if v.AgeLabel != "" {
		fmt.Fprintf(&b, "  age     : %s\n", v.AgeLabel)
	}
	if v.ExpiresIn != "" {
		fmt.Fprintf(&b, "  expires : %s\n", v.ExpiresIn)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SupportsExpander reports whether the supporting context is
// non-empty — used to decide whether to render the details row.
func (v *WorkingContextView) SupportsExpander() bool {
	return v != nil && strings.TrimSpace(v.Supporting) != ""
}

// DashboardRenderer writes a DashboardModel to a terminal stream.
// Uses lipgloss styling aligned with the existing `mpm help` output
// (see main.go:1236+).
type DashboardRenderer struct {
	out     io.Writer
	styles  DashboardStyles
}

// DashboardStyles is the small subset of lipgloss styling that
// `mpm continue` needs. Keeping the styling local (rather than
// importing the help styles) means the DashboardRenderer can be
// moved into a sub-package without dragging the whole help-style
// surface along.
type DashboardStyles struct {
	Title    lipgloss.Style
	Section  lipgloss.Style
	SectionH lipgloss.Style
	Body     lipgloss.Style
	Muted    lipgloss.Style
}

// DefaultDashboardStyles returns the project-default palette for
// dashboard rendering. Colours match the existing help palette
// in main.go so the operator visual experience is consistent.
func DefaultDashboardStyles() DashboardStyles {
	return DashboardStyles{
		Title:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700")),
		SectionH: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700")),
		Section:  lipgloss.NewStyle().Foreground(lipgloss.Color("#7d7d7d")),
		Body:     lipgloss.NewStyle().Foreground(lipgloss.Color("#e6e6e6")),
		Muted:    lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999")),
	}
}

// NewDashboardRenderer returns a DashboardRenderer with default
// styling writing to out (or io.Discard if out is nil).
func NewDashboardRenderer(out io.Writer) *DashboardRenderer {
	if out == nil {
		out = io.Discard
	}
	return &DashboardRenderer{out: out, styles: DefaultDashboardStyles()}
}

// Render writes the dashboard to the renderer's writer. Each
// section is rendered as a labelled block; empty sections are
// rendered as "(none)" so the operator always sees the dashboard
// shape regardless of how many sections returned data.
func (dr *DashboardRenderer) Render(model *DashboardModel) error {
	if model == nil {
		return fmt.Errorf("nil dashboard model")
	}

	// Header
	if model.Title != "" {
		if _, err := fmt.Fprintln(dr.out, dr.styles.Title.Render(model.Title)); err != nil {
			return err
		}
	}
	if model.Subtitle != "" {
		if _, err := fmt.Fprintln(dr.out, dr.styles.Muted.Render(model.Subtitle)); err != nil {
			return err
		}
	}

	// Sections
	for i, s := range model.Sections {
		if i > 0 {
			if _, err := fmt.Fprintln(dr.out); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(dr.out, "%s\n",
			dr.styles.SectionH.Render(s.Name)); err != nil {
			return err
		}
		body := s.Body
		if strings.TrimSpace(body) == "" {
			body = dr.styles.Muted.Render("  (none)")
		} else {
			body = dr.styles.Body.Render(body)
		}
		if _, err := fmt.Fprintln(dr.out, body); err != nil {
			return err
		}
	}

	// Footer
	if !model.GeneratedAt.IsZero() {
		if _, err := fmt.Fprintf(dr.out, "\n%s\n",
			dr.styles.Muted.Render(fmt.Sprintf("Generated %s",
				model.GeneratedAt.Format("2006-01-02 15:04:05 UTC")))); err != nil {
			return err
		}
	}
	return nil
}

