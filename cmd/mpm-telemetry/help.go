// cmd/mpm-telemetry/help.go — help pages for the mpm-telemetry
// operator CLI.
//
// 2026-09-14 release-pass: brought the help surfaces into the
// canonical MPM visual grammar (plain text — no Bubble Tea, no
// lipgloss). Every help function must be INERT: it does not
// resolve the workspace, does not open a database, does not dial
// the socket, does not start serve. The flag dispatcher in each
// run* function short-circuits to the matching help* function on
// `--help`/`-h` so a help request exits 0 before any side
// effect.
//
// The textual contract:
//   - Top-level: `MPM · Telemetry` heading
//   - Per-subcommand: `MPM · Telemetry · <sub>` heading
//   - Layout: heading + Commands + Environment + Build
//
// We deliberately do NOT pull in `cmd/mpm/render` — that package
// imports the full substrate (db, config, etc.) and pulling it
// into a tiny sidecar would be a cross-binary dependency for
// purely textual help. Matching the textual grammar (the `MPM ·`
// heading token and the same section labels) is the contract.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// telemetryHelpHeading is the canonical heading token used at the
// top of every help surface in this binary. It is a literal so
// the build artifact's plain-text help is stable across
// refactors.
const telemetryHelpHeading = "MPM · Telemetry"

// writeHeading writes the canonical `MPM · <Title>` heading to
// w. The `·` is the same mid-dot separator the main `mpm`
// binary uses in its render.Heading.
func writeHeading(w io.Writer, title string) {
	if title == "" {
		fmt.Fprintln(w, telemetryHelpHeading)
		return
	}
	fmt.Fprintln(w, telemetryHelpHeading+" · "+title)
}

// writeTopLevelHelp prints the top-level `mpm-telemetry --help`
// page. The structure follows the suggested layout from the
// release-pass brief: heading + Commands + Environment + Build.
//
// The Build line preserves the literal `Build:` token from the
// pre-fix output so the existing version-stamping regression
// tests (TestD003_BuildVersionStamped,
// TestD003_DefaultVersionIsDev) continue to pass. The
// surrounding layout (heading + section blocks) is updated to
// the canonical MPM visual grammar.
func writeTopLevelHelp(w io.Writer, buildID string) {
	writeHeading(w, "")
	fmt.Fprintln(w)
	writeCommandsBlock(w)
	writeEnvironmentBlock(w)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Build: %s\n", buildID)
}

// writeCommandsBlock is shared between top-level help and any
// subcommand that wants to advertise the full command catalogue.
func writeCommandsBlock(w io.Writer) {
	fmt.Fprintln(w, "Commands")
	fmt.Fprintln(w, "  serve                 Run the telemetry collector")
	fmt.Fprintln(w, "  ping                  Check a running collector")
	fmt.Fprintln(w, "  query invocation ...  Read one invocation")
	fmt.Fprintln(w, "  query session ...     Aggregate a session")
	fmt.Fprintln(w, "  query since ...       Read recent rows")
	fmt.Fprintln(w, "  cost ...              Apply pricing at read time")
	fmt.Fprintln(w, "  observe ...           Run telemetry observations")
}

// writeEnvironmentBlock is shared between top-level help and any
// subcommand that wants to remind the operator which env vars
// drive runtime behavior.
func writeEnvironmentBlock(w io.Writer) {
	fmt.Fprintln(w, "Environment")
	fmt.Fprintln(w, "  MPM_WORKSPACE")
	fmt.Fprintln(w, "  MPM_TELEMETRY_SOCKET")
}

// hasHelpFlag reports whether args contains `--help`, `-h`, or
// the literal token `help` (which main.go normalises the
// top-level flag into). The check is case-sensitive and matches
// the exact tokens the dispatcher recognises.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			return true
		}
	}
	return false
}

// writeHelpServe prints the canonical help page for
// `mpm-telemetry serve`.
func writeHelpServe(w io.Writer, buildID string) {
	writeHeading(w, "serve")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run the telemetry collector (Unix-socket NDJSON listener).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage")
	fmt.Fprintln(w, "  mpm-telemetry serve [--quiet]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags")
	fmt.Fprintln(w, "  --quiet   suppress startup banner on stderr")
	fmt.Fprintln(w)
	writeEnvironmentBlock(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Notes")
	fmt.Fprintln(w, "  systemd invokes this subcommand explicitly. Do not rely on a default subcommand.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Build: %s\n", buildID)
}

// writeHelpPing prints the canonical help page for
// `mpm-telemetry ping`.
func writeHelpPing(w io.Writer, buildID string) {
	writeHeading(w, "ping")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Handshake with a running telemetry collector.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage")
	fmt.Fprintln(w, "  mpm-telemetry ping")
	fmt.Fprintln(w)
	writeEnvironmentBlock(w)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Build: %s\n", buildID)
}

// writeHelpQuery prints the canonical help page for
// `mpm-telemetry query`.
func writeHelpQuery(w io.Writer, buildID string) {
	writeHeading(w, "query")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Read rows from the telemetry database (NDJSON output).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage")
	fmt.Fprintln(w, "  mpm-telemetry query invocation <id>")
	fmt.Fprintln(w, "  mpm-telemetry query session    <id>")
	fmt.Fprintln(w, "  mpm-telemetry query since      <unix-seconds>")
	fmt.Fprintln(w)
	writeEnvironmentBlock(w)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Build: %s\n", buildID)
}

// writeHelpCost prints the canonical help page for
// `mpm-telemetry cost`.
func writeHelpCost(w io.Writer, buildID string) {
	writeHeading(w, "cost")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Project cost against an external pricing catalog at read time.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage")
	fmt.Fprintln(w, "  mpm-telemetry cost --pricing <file> [--since <unix-seconds>]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags")
	fmt.Fprintln(w, "  --pricing  path to pricing catalog JSON (required)")
	fmt.Fprintln(w, "  --since    Unix epoch seconds; default = 0 (all rows)")
	fmt.Fprintln(w)
	writeEnvironmentBlock(w)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Build: %s\n", buildID)
}

// writeHelpObserve prints the canonical help page for
// `mpm-telemetry observe`.
func writeHelpObserve(w io.Writer, buildID string) {
	writeHeading(w, "observe")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run HighTokenNoArtifactHunt observation cycle.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage")
	fmt.Fprintln(w, "  mpm-telemetry observe [--since <unix-seconds>] [--threshold <n>] [--min-invocations <n>] [--mpm <path>] [--dry-run]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags")
	fmt.Fprintln(w, "  --since             Unix epoch seconds; default = now-7d")
	fmt.Fprintln(w, "  --threshold         high-token threshold (input + output); default = 100000")
	fmt.Fprintln(w, "  --min-invocations   minimum invocations per session; default = 1")
	fmt.Fprintln(w, "  --mpm               path to mpm binary for cross-DB lookups; default = mpm")
	fmt.Fprintln(w, "  --dry-run           print findings instead of calling mpm")
	fmt.Fprintln(w)
	writeEnvironmentBlock(w)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Build: %s\n", buildID)
}

// writeVersion prints the linker-stamped build identity. Used by
// both the `version` subcommand and the `--version` top-level
// flag.
func writeVersion(w io.Writer, buildID string) {
	fmt.Fprintf(w, "MPM · Telemetry · version\n")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", buildID)
}

// splitTrimmed is a small helper used by help-aware arg parsing:
// it trims surrounding whitespace and returns the trimmed
// argument. Used by the runQuery dispatcher so the help token
// (when pre-scan rewrites `--help` to `help`) does not pollute
// the query kind slot.
func splitTrimmed(s string) string { return strings.TrimSpace(s) }

// writeHelpToStdout is a tiny convenience: print help to stdout
// (the same channel every mpm CLI uses for help). Avoids the
// "help on stderr" trap some CLIs fall into.
func writeHelpToStdout(w io.Writer, buildID string, sub string) {
	switch sub {
	case "serve":
		writeHelpServe(w, buildID)
	case "ping":
		writeHelpPing(w, buildID)
	case "query":
		writeHelpQuery(w, buildID)
	case "cost":
		writeHelpCost(w, buildID)
	case "observe":
		writeHelpObserve(w, buildID)
	default:
		writeTopLevelHelp(w, buildID)
	}
}

// _ = os.Stdout keeps the os import used in the file even if the
// helper indirection above moves around. Cheap compile-time
// guard against dead-import errors after future refactors.
var _ = os.Stdout
