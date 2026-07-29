// Package renderers produces human-readable output from substrate
// data shapes. Today: Markdown for review results. Future: HTML
// (for any future web-ui), CSV (for export tooling), and any other
// presentation surface that wants to consume ReviewCoordinator's
// output without depending on its internal concurrency model.
//
// Architectural intent (Wed 2026-07-29 design session):
//
//   Renderers (this package) handle presentation. They consume data
//   from engines (orchestration) and shape it for an audience.
//
//   Pure functions only. No I/O, no DB access, no goroutines,
//   no mutations. Every render is a deterministic string given the
//   same input. Tests can compare rendered output byte-for-byte.
//
//   This discipline is what makes the package safe to call from
//   contexts where state changes are forbidden — e.g. an HTTP
//   handler that wants to log a review in a structured way without
//   touching the database, or an audit tool that renders the
//   same review into multiple formats.
package renderers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/flowbyte-com/mpm-core/orchestration"
)

// FormatReviewsMarkdown renders an []orchestration.ReviewResult as
// human-readable Markdown. One section per component. Errors are
// rendered gracefully — a failed component doesn't break the
// dashboard, it shows its own error block.
//
// Layout (per component):
//
//   ## Review from <Component>
//
//   - Component: `<name>`
//   - Profile:   `<profile-name>`
//   - Provider:  `<provider>`
//   - Model:     `<model>`
//   - Duration:  <N>ms
//
//   [success] <response text>
//   OR
//   **Error:**
//   ```
//   <error text>
//   ```
//
//   ---

// FormatReviewsMarkdown is the canonical output formatter for
// review results. The signature takes the substrate's ReviewResult
// by reference so the render does not need to know about other
// source shapes — it consumes exactly what ReviewCoordinator
// produces.
//
// Stability: this function's output is the contract. Tests may
// pin specific output for specific inputs. Adding fields to the
// per-section layout is fine; removing is a breaking change.
func FormatReviewsMarkdown(results []orchestration.ReviewResult) string {
	if len(results) == 0 {
		return "_no reviews_\n"
	}

	// Stable order: components in the order they appear in input,
	// regardless of completion time. The coordinator already
	// preserves caller order via index-based slot writes, but
	// sort here defensively in case a future implementation
	// changes that.
	sorted := make([]orchestration.ReviewResult, len(results))
	copy(sorted, results)
	sortByComponent(sorted)

	var b strings.Builder
	fmt.Fprintf(&b, "Reviews (%d component%s)\n\n", len(results), plural(len(results)))

	for i, r := range sorted {
		if i > 0 {
			b.WriteString("\n---\n\n")
		}
		// Section header. Errors get tagged inline so the human
		// reader sees them at a glance — even when the rich
		// metadata is empty, the failure is visible.
		title := fmt.Sprintf("## Review from `%s`", r.Component)
		if r.Error != "" {
			title += " — ❌ failed"
		}
		fmt.Fprintf(&b, "%s\n\n", title)

		// Metadata block. Always present, even on failure — the
		// caller's debugging log benefits from knowing which
		// component + profile was attempted.
		metadata := renderMetadata(&r)
		if metadata != "" {
			fmt.Fprintf(&b, "%s\n\n", metadata)
		}

		// Body. On success, dump the model's text response. On
		// failure, dump the error in a fenced block so multi-
		// line errors / stack traces round-trip cleanly.
		if r.Error != "" {
			fmt.Fprintf(&b, "**Error:**\n\n```\n%s\n```\n", strings.TrimSpace(r.Error))
		} else if r.Response != "" {
			b.WriteString(strings.TrimRight(r.Response, "\n"))
			b.WriteString("\n")
		} else {
			b.WriteString("_(empty response)_\n")
		}
	}
	return b.String()
}

// renderMetadata returns the bulleted metadata block for one
// review result. Empty when the result has no useful metadata
// (e.g. an early-binding error before profile resolution).
func renderMetadata(r *orchestration.ReviewResult) string {
	var b strings.Builder
	if r.Component != "" {
		fmt.Fprintf(&b, "- **Component:** `%s`\n", r.Component)
	}
	if r.Profile != "" {
		fmt.Fprintf(&b, "- **Profile:** `%s`\n", r.Profile)
	}
	if r.Provider != "" {
		fmt.Fprintf(&b, "- **Provider:** `%s`\n", r.Provider)
	}
	if r.Model != "" {
		fmt.Fprintf(&b, "- **Model:** `%s`\n", r.Model)
	}
	if r.DurationMS > 0 {
		fmt.Fprintf(&b, "- **Duration:** %dms\n", r.DurationMS)
	}
	return strings.TrimRight(b.String(), "\n")
}

// sortByComponent orders ReviewResults by component name, with
// stable secondary order by the input index (preserved via the
// `index` map). ReviewCoordinator already preserves caller order
// in its output slice; this sort is defensive — the renderer
// shouldn't assume upstream ordering.
func sortByComponent(rs []orchestration.ReviewResult) {
	sort.SliceStable(rs, func(i, j int) bool {
		return rs[i].Component < rs[j].Component
	})
}

// plural returns "s" for non-1 counts, "" for 1. Tiny helper kept
// here to avoid importing golang.org/x/text or similar in this
// minimal package.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
