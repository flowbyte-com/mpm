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
//
// MPM #28 (issue #28): the standard report path uses a read-only
// handle (getDBReadOnly) so the inspection is observational and the
// writable singleton is not instantiated for an inspection-only
// invocation. The probe.RunActiveProbes call passes nil for dm so
// the probe cache is not written (the live probe results are still
// rendered into the report). --explain and --deep-scan are handled
// by the same dispatcher as `mpm ops doctor` so the two entry points
// cannot diverge.
func handleDoctor(args []string) int {
	opts, err := parseDoctorFlags(args)
	if err != nil {
		return doctorFlagError(err)
	}

	// --explain and --deep-scan are handled by runDoctorCommand's
	// dispatcher via the legacy `mpm ops doctor` route. But the
	// top-level `mpm doctor` handler also accepts these flags and
	// can short-circuit them here. Both paths use the same
	// read-only handle so the contracts align.
	if opts.explain {
		runDoctorExplain()
		return 0
	}

	if opts.deepScan {
		ro, dberr := getDBReadOnly()
		if dberr != nil {
			renderDoctorDatabaseUnavailable(os.Stderr, dberr)
			return doctorMissingDBExitCode()
		}
		defer ro.Close()
		if opts.fix {
			rw := getDBConcrete()
			if rw == nil {
				usererror.Error("doctor deep-scan --fix: cannot open database")
				return 1
			}
			runDoctorDeepScanFix(rw)
			return 0
		}
		runDoctorDeepScanInspect(ro)
		return 0
	}

	ro, dberr := getDBReadOnly()
	if dberr != nil {
		// The standard report cannot run without a database.
		// Render a top-level "Database" check with the precise
		// remediation hint, then exit with the preserved
		// missing-database exit code.
		report := DoctorReport{Checks: []DoctorCheck{}}
		renderDoctorDatabaseUnavailableToReport(&report, dberr)
		report.Tally()
		// Mirror the unavailable message to stderr so the operator
		// sees it on the terminal (the report will only show on
		// the json/--json path; the standard report prints via
		// NewDoctorRenderer which expects rows in the report).
		renderDoctorDatabaseUnavailable(os.Stderr, dberr)
		// On --json, encode the report; otherwise render via the
		// human renderer. Branching on opts.json here means the
		// --json path is never contaminated by ANSI escapes from
		// printDoctorSummary.
		if opts.json {
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
				Checks: doctorChecksToJSON(report.Checks),
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
		// Human path. Print the summary directly (the renderer
		// is for multi-check reports; a single unavailable-DB
		// row is rendered via the stderr message above and the
		// summary footer here).
		printDoctorSummary(&report)
		return doctorMissingDBExitCode()
	}
	defer ro.Close()

	svc := NewDoctorService(ro)
	if svc == nil {
		usererror.Error("doctor: failed to wire service")
		return 1
	}

	report, err := svc.Check()
	if err != nil {
		usererror.Error("doctor: %v", err)
		return 1
	}

	// Model connectivity probes — runs concurrently with bounded fan-out.
	// The dm is passed as nil so the probe cache is NOT persisted; the
	// live probe results are still rendered into the report (operator
	// decision: "doctor remains allowed to perform live network probes
	// without persisting their results"). See internal/core/probe/run.go
	// for the dm=nil gate.
	if cfg, lerr := config.LoadConfig(); lerr == nil && cfg != nil {
		if authCheck := authSchemeConfigCheck(cfg); authCheck != nil {
			report.Checks = append(report.Checks, *authCheck)
		}
		if pres, perr := probe.RunActiveProbes(context.Background(), cfg, nil); perr == nil && len(pres) > 0 {
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

// authSchemeConfigCheck reports profiles whose auth_scheme value is not
// a supported scheme.
//
// The value used to be validated only when it was TYPED at the CLI. A
// hand-edited mpm_config.json carrying `auth_scheme: "basic"` — or a
// plain typo — was silently dropped, the endpoint was authenticated with
// a DERIVED scheme, and the operator saw a bare 401 with no explanation.
//
// The row is a WARN when something is wrong, and is omitted entirely
// when nothing is: a clean configuration should not add a line to the
// operator's report.
func authSchemeConfigCheck(cfg *config.Config) *DoctorCheck {
	issues := cfg.InvalidAuthSchemes()
	if len(issues) == 0 {
		return nil
	}
	return &DoctorCheck{
		Name:    "Auth schemes",
		Status:  "WARN",
		Message: strings.Join(issues, "; "),
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
		authReason := authFailureReason(r.AuthFailure)
		switch r.Status {
		case probe.ProbeHealthy:
			check.Status = "PASS"
			check.Message = fmt.Sprintf("%s · %s · %dms",
				r.Provider, r.Model, r.LatencyMs)
			// Name the authentication scheme that worked. Without it the
			// operator cannot tell which header the endpoint was asked
			// for, and a future endpoint change would be invisible.
			if r.AuthScheme != "" {
				check.Message = fmt.Sprintf("%s · %s · %s · %dms",
					r.Provider, r.Model, r.AuthScheme, r.LatencyMs)
			}
		case probe.ProbeDisabled:
			check.Status = "INFO"
			check.Message = "intentionally disabled"
		case probe.ProbeAuthFailed:
			check.Status = "WARN"
			// An invalid credential, an unbilled subscription and an
			// under-privileged key are three different problems with
			// three different remedies. Report which one it is rather
			// than collapsing them into one "auth failed" line.
			if authReason != "" {
				check.Message = fmt.Sprintf("%s · auth failed (%s): %s",
					r.Provider, authReason, r.ErrorSummary)
			} else {
				check.Message = fmt.Sprintf("%s · auth failed: %s", r.Provider, r.ErrorSummary)
			}
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
		case probe.ProbeRedirectRefused:
			// MPM refused to follow a redirect that would have carried
			// the credential off the configured origin. The endpoint is
			// reachable; this is a security decision, not a fault of
			// the endpoint. The refusal's own message is the most
			// useful thing the operator can be told.
			check.Status = "FAIL"
			check.Message = fmt.Sprintf("%s · redirect refused (the credential would have left the configured origin)", r.Provider)
			if r.ErrorSummary != "" {
				check.Message = fmt.Sprintf("%s · redirect refused: %s", r.Provider, r.ErrorSummary)
			}
		default:
			check.Status = "WARN"
			check.Message = r.ErrorSummary
			if check.Message == "" {
				check.Message = "unknown"
			}
			// A billing refusal lands here (its status is not
			// ProbeAuthFailed), but the operator still needs to know
			// WHICH refusal it was.
			if authReason != "" {
				check.Message = fmt.Sprintf("%s (%s)", authReason, check.Message)
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

// authFailureReason renders the authentication-failure sub-kind as an
// operator-facing phrase.
//
// The probe reports the sub-kind (invalid_credential / subscription /
// permission); this maps it to the wording an operator can act on. An
// unrecognised or absent sub-kind yields an empty string, and the
// caller falls back to the generic "auth failed" phrasing.
func authFailureReason(kind string) string {
	switch kind {
	case "invalid_credential":
		return "invalid credential"
	case "subscription":
		return "subscription expired or not billed"
	case "permission":
		return "key not permitted for this model"
	default:
		return ""
	}
}
