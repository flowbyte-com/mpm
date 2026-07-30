package main

// route_apply.go — minimal build fix for the broken HEAD (commit 4c9cc97)
// where cmd/mpm/router.go references stripApplyFlag and applyRouteToActive
// before their definitions landed in the working tree.
//
// The full feature work (route_render.go refactor + getters refactor +
// active.json update) lives in stash@{0} and stash@{1}. The companion plan
// (docs/superpowers/specs/2026-07-30-unix-epoch-timestamps-design.md)
// re-applies that stash on top of the migration branch after Task 18.
//
// This file isolates the two missing function definitions so HEAD compiles
// standalone. Keep this file's surface area tight: if you find yourself
// wanting to extend these helpers beyond the call sites in router.go, the
// full route_render.go refactor (in the stash) probably already covers it.

import (
	"os"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// stripApplyFlag removes --apply from args and returns (apply, cleanedArgs).
// Other flags are passed through unchanged. Idempotent — calling twice
// produces the same result.
func stripApplyFlag(args []string) (bool, []string) {
	apply := false
	cleaned := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--apply" {
			apply = true
			continue
		}
		cleaned = append(cleaned, a)
	}
	return apply, cleaned
}

// applyRouteToActive merges the route report into active.json. Rules
// (2026-07-30 audit; makes active.json a live signal, not a stale bag):
//   - Persona: update when SelectedPersona is non-empty AND different from
//     current. Empty result preserves the operator's manually-set persona
//     on no-op routes (low-signal prompts).
//   - Modes: replace when SelectedModes is non-empty. Empty result
//     preserves current modes for the same reason.
//   - Updated: bump only when something actually changed.
//
// On any disk error (read or write), the function returns silently —
// routing must never block the user. Stderr is also gated behind isatty
// to avoid corrupting hook output.
func applyRouteToActive(report internal.RoutingReport) {
	current, err := internal.LoadActiveJSON()
	if err != nil {
		if isatty(os.Stderr) {
			usererror.Warn("route --apply: load active.json: %v", err)
		}
		return
	}

	next := *current // shallow copy — Modes slice is replaced wholesale below
	changed := false

	if report.SelectedPersona != "" && report.SelectedPersona != current.Persona {
		next.Persona = report.SelectedPersona
		changed = true
	}
	if len(report.SelectedModes) > 0 && !equalStringSlices(report.SelectedModes, current.Modes) {
		next.Modes = append([]string(nil), report.SelectedModes...)
		changed = true
	}

	if !changed {
		return
	}

	next.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := internal.SaveActiveJSON(&next); err != nil {
		if isatty(os.Stderr) {
			usererror.Warn("route --apply: save active.json: %v", err)
		}
	}
}

// equalStringSlices reports whether two string slices have identical contents
// in identical order. Modes are ordered in the route report (threshold-filtered,
// score-sorted) so positional equality is the right check.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}