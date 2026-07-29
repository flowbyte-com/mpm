// cmd/mpm/renderers/terminal.go — Renderers layer for Wave 1.
//
// Renderers consume view-shaped data (produced by Formatters) and
// write human-readable terminal output. They are the output-medium
// adapter for human operators.
//
// Layering contract (RFC §7):
//   Renderers sit at the bottom of the presentation pipeline.
//   They NEVER call services, NEVER call formatters, NEVER call
//   commands. They are the last stop before the operator sees
//   the result.
//
// This file implements:
//
//   - TerminalRenderer — full-fidelity Markdown output for
//     `mpm work show`, `mpm why`, etc. (commands that show one thing.)
//   - StatusRenderer — single-line + indicator markers for
//     `mpm work status`.
//
// DashboardRenderer (for `mpm continue`) lives in dashboard.go.

package main

import (
	"fmt"
	"io"
	"strings"
)

// TerminalRenderer writes a single section's content to a terminal.
// Used by `mpm work show` (full Markdown output), `mpm why <id>` (per-
// artifact introspection), etc.
//
// The renderer is intentionally minimal — it does NOT apply lipgloss
// styling or colour. Styles belong in the per-command presentation
// step (see handlers_continue.go for the dashboard layout which uses
// the lipgloss styling already in main.go). The TerminalRenderer just
// emits the view-shaped data with sensible whitespace + indicators.
type TerminalRenderer struct {
	out io.Writer
}

// NewTerminalRenderer returns a renderer that writes to out. If out
// is nil, output goes to io.Discard.
func NewTerminalRenderer(out io.Writer) *TerminalRenderer {
	if out == nil {
		out = io.Discard
	}
	return &TerminalRenderer{out: out}
}

// RenderMarkdown writes content to the terminal as-is, with a single
// trailing newline. The caller is responsible for ensuring the
// content is shaped for terminal display (use a Formatter).
//
// The "render" is deliberately trivial — by the time data reaches
// the renderer it should already be presentation-shaped.
func (r *TerminalRenderer) RenderMarkdown(content string) error {
	_, err := fmt.Fprintln(r.out, strings.TrimRight(content, "\n"))
	return err
}

// RenderError writes an error to the terminal as a single line prefixed
// with the canonical error marker.
func (r *TerminalRenderer) RenderError(err error) error {
	if err == nil {
		return nil
	}
	_, werr := fmt.Fprintf(r.out, "❌ %s\n", err.Error())
	return werr
}

// RenderInfo writes an info-style line to the terminal.
func (r *TerminalRenderer) RenderInfo(label, content string) error {
	_, err := fmt.Fprintf(r.out, "%s: %s\n", label, content)
	return err
}

// StatusRenderer is the single-line renderer for `mpm work status`.
// Returns light metadata (session_id, thesis preview, age, expiry).
type StatusRenderer struct {
	out io.Writer
}

// NewStatusRenderer returns a StatusRenderer writing to out.
func NewStatusRenderer(out io.Writer) *StatusRenderer {
	if out == nil {
		out = io.Discard
	}
	return &StatusRenderer{out: out}
}

// WorkingContextStatus is the per-shape data the StatusRenderer
// consumes. Returned by WorkingContextFormatter.CompactStatus — kept
// here (in renderers) because the renderer shapes how the view
// renders. Future Wave 2+ may move shape definition to a separate
// view package.
type WorkingContextStatus struct {
	SessionID  string
	ThesisHead string // first ~80 chars of thesis
	AgeLabel   string // e.g., "12 minutes ago"
	ExpiresIn  string // e.g., "in 23 hours" or "EXPIRED"
}

// Render formats a single WorkingContextStatus as a one-line report.
func (sr *StatusRenderer) Render(s WorkingContextStatus) error {
	_, err := fmt.Fprintf(sr.out,
		"Working Context\n  session_id : %s\n  thesis     : %s\n  age        : %s\n  expires    : %s\n",
		s.SessionID, s.ThesisHead, s.AgeLabel, s.ExpiresIn,
	)
	return err
}

// RenderEmpty is the `no working context present` rendering for
// `mpm work status` when the store has no row (or it's expired).
func (sr *StatusRenderer) RenderEmpty(sessionID string) error {
	_, err := fmt.Fprintf(sr.out,
		"Working Context\n  session_id : %s\n  state      : (no active working context)\n",
		sessionID,
	)
	return err
}
