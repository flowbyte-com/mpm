// cmd/mpm/probe_dashboard.go — Render the compact "Models" line on the
// `mpm` no-args dashboard. Reads cached probe results; NEVER performs
// network IO. If the cache has no recent verified state, surfaces an
// honest `?` (or `—` if no effective bindings exist).
//
// Output grammar (matches the spec exactly):
//
//   All active healthy        → Models        ✓ N/N healthy · checked Ns ago
//   Disabled + failures        → Models        ! X/Y healthy · 1 disabled
//   Disabled-only              → Models        ✓ N healthy · 1 disabled
//   Stale / missing / mismatch → Models        ? not recently verified · run 'mpm doctor'
//   No effective bindings      → Models        — not configured

package main

import (
	"fmt"
	"io"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"

	"github.com/flowbyte-com/mpm/cmd/mpm/probe"
	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// renderModelsDashboard writes the compact Models line to w. Reads the
// bounded system_config cache; render is purely structural (no network IO).
//
// dm may be nil — in that case the dashboard always surfaces the
// not-recently-verified state.
func renderModelsDashboard(w io.Writer, dm *mpminternal.DatabaseManager, cfg *config.Config) {
	if cfg == nil {
		// Without a config we can't classify "not configured" vs
		// "not verified"; render the most conservative state.
		render.Plain(w, "  Models          — not configured")
		return
	}

	targets := probe.ResolveEffectiveTargets(cfg)
	if len(targets) == 0 {
		render.Plain(w, "  Models          — not configured")
		return
	}

	// Partition targets by status: active vs disabled. Disabled rows
	// exclude from the health numerator / denominator.
	activeCount := 0
	disabledCount := 0
	for _, t := range targets {
		if t.Disabled {
			disabledCount++
		} else {
			activeCount++
		}
	}

	if activeCount == 0 {
		// Every binding is disabled. Render an honest all-disabled state.
		render.Plainf(w, "  Models          ✓ %d healthy · %d disabled\n", 0, disabledCount)
		return
	}

	if dm == nil {
		render.Plain(w, "  Models          ? not recently verified · run 'mpm doctor'")
		return
	}

	cached, err := probe.ReadCachedHealth(dm, cfg)
	if err != nil || len(cached) == 0 {
		render.Plain(w, "  Models          ? not recently verified · run 'mpm doctor'")
		return
	}

	now := time.Now()
	healthyCount := 0
	freshAndHealthy := 0
	freshCount := 0
	staleCount := 0
	failed := []probe.CachedProbe{}
	for _, r := range cached {
		stale := probe.IsStale(r, now)
		if stale {
			staleCount++
			continue
		}
		freshCount++
		if r.Status == "healthy" {
			healthyCount++
			freshAndHealthy++
		} else {
			failed = append(failed, r)
		}
	}

	// A target count differs from cached count because we dedup by fingerprint;
	// for visual rendering we treat healthy/failed against the bound active count.
	if staleCount == len(cached) {
		render.Plain(w, "  Models          ? not recently verified · run 'mpm doctor'")
		return
	}

	healthyActive := 0
	for _, r := range cached {
		if !probe.IsStale(r, now) && r.Status == "healthy" {
			healthyActive++
		}
	}
	// Identify failures per fingerprint for the per-failed list.
	_ = healthyCount
	_ = freshAndHealthy
	_ = freshCount

	// Aggregate outcome.
	totalActive := activeCount
	_ = totalActive

	disabledSuffix := ""
	if disabledCount > 0 {
		disabledSuffix = fmt.Sprintf(" · %d disabled", disabledCount)
	}

	// Build the headline.
	var headline string
	if len(failed) == 0 {
		// All fresh cached rows are healthy.
		headline = fmt.Sprintf("  Models          ✓ %d/%d healthy · checked %s ago%s",
			healthyActive, activeCount, formatAgeFreshest(cached, now), disabledSuffix)
		render.Plain(w, headline)
		return
	}

	headline = fmt.Sprintf("  Models          ! %d/%d healthy%s",
		healthyActive, activeCount, disabledSuffix)
	render.Warning(w, headline)

	// Per-failed rows.
	for _, r := range failed {
		summary := r.ErrorSummary
		if summary == "" {
			summary = r.Status
		}
		render.Plainf(w, "  %-15s ✗ %s · %s\n",
			r.Model, summary, friendlyLabelFromStatus(r.Status))
	}
}

// formatAgeFreshest returns a short human label for the freshest record's
// age (e.g. "18s", "2m", "1h"). Used in the dashboard "checked Ns ago" tag.
func formatAgeFreshest(cached []probe.CachedProbe, now time.Time) string {
	if len(cached) == 0 {
		return "?"
	}
	var newest int64
	for _, r := range cached {
		if r.CheckedAt > newest {
			newest = r.CheckedAt
		}
	}
	if newest == 0 {
		return "?"
	}
	d := now.Sub(time.Unix(newest, 0))
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// friendlyLabelFromStatus maps probe status strings onto human labels.
func friendlyLabelFromStatus(s string) string {
	switch s {
	case "healthy":
		return "healthy"
	case "auth_failed":
		return "auth failed"
	case "model_not_found":
		return "model not found"
	case "unreachable":
		return "unreachable"
	case "timeout":
		return "timeout"
	case "invalid_response":
		return "invalid response"
	case "disabled":
		return "disabled"
	case "unknown":
		return "unknown"
	}
	return s
}
