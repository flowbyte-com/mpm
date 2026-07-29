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
	"os"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleDoctor runs DoctorService.Check and renders the report.
//
// Future flags: --json (machine-readable), --all (additional
// slower checks). For Wave 2 the canonical form is the dashboard;
// --json lands in a follow-up.
func handleDoctor(args []string) int {
	for _, a := range args {
		switch a {
		case "--json", "-j", "--all", "--deep-scan", "--explain", "--fix":
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

// touch mpminternal for the import-grouping lint rule that
// other handlers in this file use; no symbol reference required.
var _ = mpminternal.CoreDB(nil)
