// cmd/mpm/formatters/formatter.go — Formatters layer for Wave 1.
//
// A Formatter reshapes a domain model into a presentation-shaped form
// for downstream rendering or encoding. Formatters are pure data
// transformations — no I/O, no stdout, no DB access.
//
// Layering contract (RFC §7):
//   Formatters sit between Services and Renderers/Encoders.
//   They do NOT call other formatters, do NOT call commands,
//   do NOT own SQL.
//
// Concrete examples (Wave 1):
//   - WorkingContextFormatter.Format(*WorkingContext) → WorkingContextView
//     shapes for either dashboard-embed (compact) or terminal-default
//     (full Markdown) presentation contexts.
//   - WorkingContextFormatter.CompactStatus(*WorkingContext) →
//     WorkingContextStatusView
//     a one-line summary for the `mpm work status` command.
//
// Future formatters (Wave 2+): DecisionFormatter, SkillFormatter,
// TheoryFormatter, etc. — each owns the per-kind reshape logic for
// the `mpm continue` dashboard sections.

package main

// Formatter is the minimal interface every concrete Formatter
// satisfies. The View field is intentionally `any` — different
// formatters produce different view shapes. Renderers and
// Encoders consume the view via type assertion.
type Formatter interface {
	Format(input interface{}) (any, error)
}
