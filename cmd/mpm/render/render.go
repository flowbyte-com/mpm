// Package render is the shared human-output rendering layer for mpm.
//
// Every public human-facing CLI command routes its output through the
// primitives in this package. The goal is one visual grammar across the
// entire product. The canonical visual baseline is `mpm doctor`.
//
// Rules enforced by this package:
//
//   - Headings use MPM · <Command> form (canonical).
//   - Section headings are sentence case (no ALL CAPS, no novelty glyphs).
//   - Directives is exactly "Directives" — never "MPM · Directives".
//   - Status markers are ✓ ⚠ ✗.
//   - Body is regular weight; secondary text is dim gray.
//   - JSON paths NEVER pass through this package.
//
// Adding a new public surface? Use the existing primitives; do not
// invent new lipgloss styles inline.
package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Color tokens — the canonical palette. Centralized here so a single
// edit propagates across every surface. The amber/yellow heading value
// mirrors Doctor's approved style.
const (
	HeadingColor   = "#ffb700" // amber/yellow — MPM · heading token
	BodyColor      = ""        // normal foreground
	SecondaryColor = "#999999" // dim gray — hints, secondary text
	SuccessColor   = "#22c55e" // green — pass
	WarningColor   = "#eab308" // amber — warn
	ErrorColor     = "#ef4444" // red — fail
	LabelColor     = "#cccccc" // subtle gray — labels
	ValueColor     = ""        // normal foreground — values
)

// Marker vocabulary. Use these constants instead of literal glyphs.
const (
	MarkerPass = "✓"
	MarkerWarn = "⚠"
	MarkerFail = "✗"
	MarkerInfo = "○" // neutral / informational (not a pass, not a failure)
)

// Heading prints the canonical command heading. Form: "MPM · <Command>".
// Bold + amber/yellow. Appends a single trailing newline.
func Heading(w io.Writer, command string) error {
	style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(HeadingColor))
	_, err := fmt.Fprintln(w, style.Render("MPM · "+command))
	return err
}

// SectionHeading prints a sentence-case section label. NOT bold —
// headings are bold; sections inherit body weight. Appends newline.
func SectionHeading(w io.Writer, label string) error {
	_, err := fmt.Fprintln(w, label)
	return err
}

// Section prints a section heading in dim gray followed by a body line.
// Useful for "Identity" / "Provenance" / "Evidence" panels.
func Section(w io.Writer, label string) error {
	return SectionHeading(w, label)
}

// Timestamp prints a timestamp line in dim gray.
func Timestamp(w io.Writer, ts string) error {
	style := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color(SecondaryColor))
	_, err := fmt.Fprintln(w, style.Render(ts))
	return err
}

// Label prints a label:value pair on one line. Label is dim gray;
// value is body color. Use for tabular KeyValue rows.
func Label(w io.Writer, label, value string) error {
	style := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color(LabelColor))
	_, err := fmt.Fprintf(w, "  %s : %s\n", style.Render(strings.TrimRight(label, " :")), value)
	return err
}

// KeyValue is an alias for Label retained for clarity at call sites.
func KeyValue(w io.Writer, label, value string) error {
	return Label(w, label, value)
}

// Hint prints a dim-gray "→ text" guidance line. Indent by 8 spaces
// to match Doctor's existing convention.
func Hint(w io.Writer, text string) error {
	style := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color(SecondaryColor))
	_, err := fmt.Fprintf(w, "        %s\n", style.Render("→ "+text))
	return err
}

// Success prints a ✓-prefixed success line in green.
func Success(w io.Writer, text string) error {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(SuccessColor))
	_, err := fmt.Fprintln(w, style.Render(MarkerPass+"  "+text))
	return err
}

// Warning prints a ⚠-prefixed warning line in amber.
func Warning(w io.Writer, text string) error {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(WarningColor))
	_, err := fmt.Fprintln(w, style.Render(MarkerWarn+"  "+text))
	return err
}

// Error prints a ✗-prefixed error line in red. Use only for inline
// error rendering inside an otherwise-successful human output stream —
// standalone CLI errors go through usererror.
func Error(w io.Writer, text string) error {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(ErrorColor))
	_, err := fmt.Fprintln(w, style.Render(MarkerFail+"  "+text))
	return err
}

// Marker renders a status marker (✓ ⚠ ✗ ○) tinted by status.
// "INFO" renders the neutral marker (○) — informational, not a
// successful check, not a failure. Use INFO for absent optional
// features (e.g. embedding model when embeddings are optional).
func Marker(w io.Writer, status string) error {
	var m string
	var c string
	switch status {
	case "PASS":
		m, c = MarkerPass, SuccessColor
	case "WARN":
		m, c = MarkerWarn, WarningColor
	case "FAIL":
		m, c = MarkerFail, ErrorColor
	case "INFO":
		m, c = MarkerInfo, LabelColor
	default:
		m = " "
		c = LabelColor
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(c))
	_, err := fmt.Fprint(w, style.Render(m))
	return err
}

// CheckRow prints a single Doctor-style row: "  <marker> <Name padded> <Message>"
// followed by Details as Hint lines.
func CheckRow(w io.Writer, status, name, message string, details []string) error {
	if err := Marker(w, status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  %-22s %s\n", name, message); err != nil {
		return err
	}
	for _, d := range details {
		if err := Hint(w, d); err != nil {
			return err
		}
	}
	return nil
}

// Divider prints a single thin separator line.
func Divider(w io.Writer) error {
	_, err := fmt.Fprintln(w, strings.Repeat("─", 60))
	return err
}

// BlankLine prints an empty line.
func BlankLine(w io.Writer) error {
	_, err := fmt.Fprintln(w)
	return err
}

// TableHeader prints a sentence-case table header line in dim gray.
// Caller is responsible for the corresponding data rows.
func TableHeader(w io.Writer, columns ...string) error {
	style := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color(SecondaryColor))
	_, err := fmt.Fprintln(w, style.Render(strings.Join(columns, "  ")))
	return err
}

// IDBadge prints an id in subtle gray — used for canonical IDs and
// pointers in human output so the eye doesn't mistake them for body text.
func IDBadge(w io.Writer, label string) error {
	style := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color(LabelColor))
	_, err := fmt.Fprintln(w, style.Render(label))
	return err
}

// Plain prints an unstyled line — body color, regular weight. Use for
// arbitrary human prose where no semantic color is appropriate.
func Plain(w io.Writer, line string) error {
	_, err := fmt.Fprintln(w, line)
	return err
}

// Plainf prints an unstyled formatted line — body color, regular weight.
func Plainf(w io.Writer, format string, args ...interface{}) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}
