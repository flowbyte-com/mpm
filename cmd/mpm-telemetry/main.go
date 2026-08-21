// cmd/mpm-telemetry/main.go — entry point for the telemetry sidecar.
//
// Architecture: separate process from mpm, owns telemetry.db, serves a
// Unix-socket collector plus a small read-only CLI surface. Substrate
// (mpm core) is unchanged.
package main

import (
	"fmt"
	"os"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

const buildVersion = "dev"

func main() {
	// Inject build version into the telemetry package before any subcommand runs.
	telemetry.SetBuildVersion(buildVersion)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

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
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "mpm-telemetry — execution-economics collector")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  serve                        Run the socket collector (default if no subcommand)")
	fmt.Fprintln(os.Stderr, "  ping                         Handshake with a running collector")
	fmt.Fprintln(os.Stderr, "  query invocation <id>        Read one row")
	fmt.Fprintln(os.Stderr, "  query session <id>           Aggregate rows for a session")
	fmt.Fprintln(os.Stderr, "  query since <unix-seconds>   Read rows newer than cutoff")
	fmt.Fprintln(os.Stderr, "  cost --pricing <file>        Apply external pricing (read-time)")
	fmt.Fprintln(os.Stderr, "  observe [--since] [--threshold] [--min-invocations]")
	fmt.Fprintln(os.Stderr, "                                Run HighTokenNoArtifactHunt")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Env:")
	fmt.Fprintln(os.Stderr, "  MPM_WORKSPACE            Telemetry socket + db directory")
	fmt.Fprintln(os.Stderr, "  MPM_TELEMETRY_SOCKET     Override derived socket path")
	fmt.Fprintf(os.Stderr, "\nBuild: %s\n", buildVersion)

	// Silence unused-import on telemetry during skeleton step.
	_ = telemetry.SchemaVersion
}
