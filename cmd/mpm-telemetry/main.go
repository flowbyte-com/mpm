// cmd/mpm-telemetry/main.go — entry point for the telemetry sidecar.
//
// Architecture: separate process from mpm, owns telemetry.db, serves a
// Unix-socket collector plus a small read-only CLI surface. Substrate
// (mpm core) is unchanged.
//
// 2026-09-14 release-pass: top-level help is now INERT and SUCCESSFUL
// for every operator subcommand. Help requests (`--help`, `-h`,
// positional `help`) are caught BEFORE workspace/socket/database
// validation, so they exit 0 with no side effects. The top-level
// `mpm-telemetry --help` page uses the canonical `MPM · Telemetry`
// heading. A `version` subcommand and a top-level `--version` flag
// print the linker-stamped build identity.
package main

import (
	"fmt"
	"os"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// buildVersion is a `var` (not `const`) so the canonical ldflags
// `-ldflags "-X main.buildVersion=$(VERSION)"` can stamp the actual build
// identifier at link time. A `const` would bake into the binary at
// compile time and the linker flag would silently no-op (D-003).
var buildVersion = "dev"

// topLevelHelpFlag reports whether the top-level argv contains a
// help flag (`--help`, `-h`, or the literal token `help`). This
// is checked BEFORE subcommand dispatch so the help page is
// printed even when the user has not supplied a subcommand.
func topLevelHelpFlag(argv []string) bool {
	for _, a := range argv {
		if a == "--help" || a == "-h" || a == "help" {
			return true
		}
	}
	return false
}

// topLevelVersionFlag reports whether the top-level argv contains
// a version flag (`--version` or `-V`). Distinct from the help
// flag so the two surfaces do not collide if a future subcommand
// ever exposes a `-V` of its own.
func topLevelVersionFlag(argv []string) bool {
	for _, a := range argv {
		if a == "--version" || a == "-V" {
			return true
		}
	}
	return false
}

func main() {
	telemetry.SetBuildVersion(buildVersion)

	// 1. Top-level version flag: --version / -V at the top level
	//    (before any subcommand) prints the linker-stamped build
	//    identity and exits 0. Distinct from `version` subcommand
	//    so help/version never collide.
	if len(os.Args) >= 2 && topLevelVersionFlag(os.Args[1:2]) {
		writeVersion(os.Stdout, buildVersion)
		return
	}

	if len(os.Args) < 2 {
		// 2026-09-14 release-pass: bare `mpm-telemetry` (no
		// subcommand) now prints the top-level help page and
		// exits 0. systemd explicitly invokes
		// `mpm-telemetry serve`, so a no-subcommand invocation
		// is operator-facing help, not an error.
		//
		// Pre-fix this path printed usage to stderr and exited 2,
		// which contradicted the help text saying
		// "default if no subcommand" but never actually
		// defaulting. The explicit-subcommand behavior is
		// preserved: systemd's `mpm-telemetry serve` invocation
		// routes to runServe unchanged.
		writeTopLevelHelp(os.Stdout, buildVersion)
		return
	}

	// 2. Top-level help flag with no subcommand
	//    (`mpm-telemetry --help`, `mpm-telemetry -h`,
	//    `mpm-telemetry help`) routes to the top-level page.
	if topLevelHelpFlag(os.Args[1:2]) {
		writeTopLevelHelp(os.Stdout, buildVersion)
		return
	}

	sub := os.Args[1]
	args := os.Args[2:]

	// 3. Help routed through a specific subcommand
	//    (`mpm-telemetry serve --help`, etc.) is the per-
	//    subcommand help page. We do NOT call into the runServe /
	//    runPing / runQuery / runCost / runObserve path — those
	//    would resolve workspace / open DB / dial socket before
	//    the help request could be satisfied. Instead, dispatch
	//    to the matching writeHelp* function directly so the
	//    help request exits 0 with no side effect.
	if hasHelpFlag(args) {
		writeHelpToStdout(os.Stdout, buildVersion, sub)
		return
	}

	// 3. Subcommand dispatch.
	var err error
	switch sub {
	case "serve":
		err = runServe(args)
	case "ping":
		err = runPing(args)
	case "query":
		err = runQuery(args)
	case "cost":
		err = runCost(args)
	case "observe":
		err = runObserve(args)
	case "version":
		// 2026-09-14 release-pass: dedicated version subcommand
		// that prints the linker-stamped build identity. Tiny
		// switch case; no architectural churn. Tests assert
		// parity with `--version` and with the build stamp.
		writeVersion(os.Stdout, buildVersion)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		writeTopLevelHelp(os.Stderr, buildVersion)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// _ = telemetry.SchemaVersion keeps the telemetry import live
// even after a future refactor that removes the package from the
// binary's runtime path. Cheap compile-time guard.
var _ = telemetry.SchemaVersion
