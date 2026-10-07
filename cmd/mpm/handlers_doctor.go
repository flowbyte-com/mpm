// cmd/mpm/handlers_doctor.go — `mpm doctor` handler for Wave 2.
//
// Tiny adapter composing DoctorService + DoctorRenderer. Per RFC §7:
// commands own no behaviour, no SQL, no rendering.
//
// TTY detection: when stdout is a terminal, the renderer uses emoji
// markers (✓ ⚠ ✗); when stdout is not a terminal (piped, redirected,
// agent log capture), the renderer uses text labels ([PASS] [WARN]
// [FAIL]). The handler does the isatty check and tells the renderer
// which mode to use; the renderer stays pure.
//
// Exit codes:
//   0 = all checks PASS
//   1 = any check WARN, no FAIL
//   2 = any check FAIL
//
// This lets scripts use `mpm doctor && proceed` reliably AND
// `mpm doctor || handle-warning` to react to warnings.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/probe"
)

// handleDoctor runs DoctorService.Check and renders the report.
//
// Final release-pass: --json is now honoured. The canonical envelope is
// {timestamp, summary, checks} — checks is a JSON-serialisable slice
// of DoctorCheck. JSON output NEVER routes through the human renderer.
//
// Flag truthfulness (2026-10-07). This handler previously accepted
// --all/--deep-scan/--explain/--fix in an empty switch arm and discarded
// them, so every one of those flags produced output byte-identical to
// plain `mpm doctor` while the CLI advertised their behaviour. The
// contract is now explicit:
//
//	doctor                    the standard report
//	doctor --json             the same report as a JSON envelope
//	doctor --explain          FTS5 query plan (EXPLAIN QUERY PLAN)
//	doctor --deep-scan        on-demand FTS/integrity audit
//	doctor --deep-scan --fix  the audit plus its bounded remediation
//
// --all and a bare --fix are REJECTED rather than silently accepted.
// Neither has an implementation to delegate to: there is no "extended"
// check set distinct from the standard report, and the standard report
// has no remediation path. See doctorFlagContract for the full rules.
func handleDoctor(args []string) int {
	opts, err := parseDoctorFlags(args)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}

	// --explain is its own audit mode: it prints an EXPLAIN QUERY PLAN
	// tree and returns. It precedes --deep-scan exactly as it does in
	// `mpm ops doctor`, so the two entry points cannot disagree.
	if opts.explain {
		runDoctorExplain()
		return 0
	}

	// --deep-scan skips the standard suite and runs the on-demand
	// integrity checks. With --fix it additionally applies the single
	// bounded remediation this codebase implements (soft-delete ghost
	// cleanup); that mutation is scoped to memories_fts and is the only
	// write path doctor has.
	if opts.deepScan {
		runDoctorDeepScan(dm, opts.fix)
		return 0
	}

	svc := NewDoctorService(dm)
	if svc == nil {
		usererror.Error("doctor: failed to wire service")
		return 1
	}

	report, err := svc.Check()
	if err != nil {
		usererror.Error("doctor: %v", err)
		return 1
	}

	// Model connectivity probes — runs concurrently with bounded fan-out,
	// persists results to system_config[model_probe_results]. Errors per
	// profile become DoctorCheck rows alongside the structural report.
	if cfg, lerr := config.LoadConfig(); lerr == nil && cfg != nil {
		if pres, perr := probe.RunActiveProbes(context.Background(), cfg, dm); perr == nil && len(pres) > 0 {
			report.Checks = append(report.Checks, modelChecksFromProbes(pres)...)
		}
	}

	// Tally AFTER every row (base + probe) is appended so the
	// summary line reflects exactly what the renderer will show.
	// See service_doctor.go:DoctorReport.Tally for the canonical
	// counting rules.
	report.Tally()

	if opts.json {
		// JSON output — never routes through the human renderer.
		// Use os.Stdout directly so the contract holds even when
		// isatty returns true.
		out := struct {
			Timestamp string                   `json:"timestamp"`
			Summary   map[string]interface{}   `json:"summary"`
			Checks    []map[string]interface{} `json:"checks"`
			Usage     *DoctorUsage             `json:"usage,omitempty"`
			Attention *DoctorAttention         `json:"attention,omitempty"`
		}{
			Timestamp: nowRFC3339(),
			Summary: map[string]interface{}{
				"passed":   report.Passed,
				"warnings": report.Warnings,
				"failed":   report.Failed,
			},
			Checks:    doctorChecksToJSON(report.Checks),
			Usage:     report.Usage,
			Attention: report.Attention,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(&out); err != nil {
			usererror.Error("doctor json: %v", err)
			return 1
		}
		switch {
		case report.Failed > 0:
			return 2
		case report.Warnings > 0:
			return 1
		default:
			return 0
		}
	}

	useEmoji := isatty(os.Stdout)
	renderer := NewDoctorRenderer(os.Stdout, useEmoji)
	if err := renderer.Render(report); err != nil {
		usererror.Error("doctor render: %v", err)
		return 1
	}

	switch {
	case report.Failed > 0:
		return 2
	case report.Warnings > 0:
		return 1
	default:
		return 0
	}
}

// doctorChecksToJSON converts a slice of DoctorCheck into a JSON-safe
// shape. The CronRetention nested struct is omitted from JSON — its
// fields are accessible via the dashboard surface.
func doctorChecksToJSON(checks []DoctorCheck) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(checks))
	for _, c := range checks {
		out = append(out, map[string]interface{}{
			"name":    c.Name,
			"status":  c.Status,
			"message": c.Message,
			"details": c.Details,
		})
	}
	return out
}

// modelChecksFromProbes converts probe results into DoctorCheck rows. One
// row per (fingerprint, components[C]) tuple already provided by the probe
// result. Disabled rows surface as INFO; partial outages as WARN; healthy
// as PASS; total failures as FAIL.
func modelChecksFromProbes(results []probe.ProbeResult) []DoctorCheck {
	out := make([]DoctorCheck, 0, len(results))
	for _, r := range results {
		name := "Models"
		if len(r.Components) > 0 {
			// Use the first component label as the row name (memory,
			// critic, embedding, etc.); additional components share the
			// underlying profile and are listed in the message.
			name = strings.Title(r.Components[0])
		}
		check := DoctorCheck{Name: name}
		switch r.Status {
		case probe.ProbeHealthy:
			check.Status = "PASS"
			check.Message = fmt.Sprintf("%s · %s · %dms",
				r.Provider, r.Model, r.LatencyMs)
		case probe.ProbeDisabled:
			check.Status = "INFO"
			check.Message = "intentionally disabled"
		case probe.ProbeAuthFailed:
			check.Status = "WARN"
			check.Message = fmt.Sprintf("%s · auth failed: %s", r.Provider, r.ErrorSummary)
		case probe.ProbeModelNotFound:
			check.Status = "WARN"
			check.Message = fmt.Sprintf("%s · model not found", r.Provider)
		case probe.ProbeUnreachable:
			check.Status = "FAIL"
			check.Message = fmt.Sprintf("%s · unreachable", r.Provider)
		case probe.ProbeTimeout:
			check.Status = "FAIL"
			check.Message = fmt.Sprintf("%s · timeout", r.Provider)
		case probe.ProbeInvalidResponse:
			check.Status = "WARN"
			check.Message = fmt.Sprintf("%s · invalid response", r.Provider)
		default:
			check.Status = "WARN"
			check.Message = r.ErrorSummary
			if check.Message == "" {
				check.Message = "unknown"
			}
		}
		if r.ErrorSummary != "" && check.Status != "PASS" && check.Status != "INFO" {
			check.Details = []string{r.ErrorSummary}
		}
		out = append(out, check)
	}
	return out
}

// touch mpminternal for the import-grouping lint rule that
// other handlers in this file use; no symbol reference required.
var _ = mpminternal.CoreDB(nil)
