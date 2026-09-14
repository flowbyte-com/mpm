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
	"encoding/json"
	"os"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleDoctor runs DoctorService.Check and renders the report.
//
// Final release-pass: --json is now honoured. The canonical envelope is
// {timestamp, summary, checks} — checks is a JSON-serialisable slice
// of DoctorCheck. JSON output NEVER routes through the human renderer.
func handleDoctor(args []string) int {
	wantJSON := false
	for _, a := range args {
		switch a {
		case "--json", "-j":
			wantJSON = true
		case "--all", "--deep-scan", "--explain", "--fix":
			// Accepted for forward-compat. Not yet implemented at
			// the top-level; the engine-room `mpm ops doctor` keeps
			// those flags for its --deep-scan/--explain modes.
		default:
			usererror.Error("doctor: unknown flag %q", a)
			return 1
		}
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
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

	if wantJSON {
		// JSON output — never routes through the human renderer.
		// Use os.Stdout directly so the contract holds even when
		// isatty returns true.
		out := struct {
			Timestamp string                 `json:"timestamp"`
			Summary   map[string]interface{} `json:"summary"`
			Checks    []map[string]interface{} `json:"checks"`
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

// touch mpminternal for the import-grouping lint rule that
// other handlers in this file use; no symbol reference required.
var _ = mpminternal.CoreDB(nil)
