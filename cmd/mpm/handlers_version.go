package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/internal/buildinfo"
)

// versionBinaryName identifies this executable in the provenance output.
// The other four binaries report their own names the same way.
const versionBinaryName = "mpm"

// versionJSON is the machine-readable form of the version surface.
//
// The provenance fields are inlined; the update result is present only
// when the operator asked for a check.
type versionJSON struct {
	buildinfo.Info
	Update *buildinfo.UpdateResult `json:"update,omitempty"`
}

// handleVersion renders the build provenance of this binary.
//
//	mpm version
//	MPM 0.1.0-dev+g06c029ba.t1792679720
//	  binary:  mpm
//	  commit:  06c029ba11ce18c7f10aa60b0f0497ece2edda7c
//	  build:   development
//	  built:   2026-10-22T14:35:20Z
//	  dirty:   no
//
// The first line is the build identity: the product version plus the
// build metadata that says which commit the binary came from, whether
// the source tree was clean, and when it was built. It is a Semantic
// Versioning version with build metadata.
//
// The dirty line reads "no", "yes" or "unknown". "unknown" means the
// build's source tree could not be compared against a revision — an
// exported source tree with no Git metadata, or a binary that was never
// stamped — which is not the same statement as "clean".
//
// Flags:
//
//	--json   emit the machine-readable form
//	--check  compare the embedded commit against the upstream main branch
//
// --check is the ONLY path that touches the network, and it never
// transmits anything about the operator's MPM installation: it sends the
// upstream repository name and two public source revision identifiers.
func handleVersion(args []string) int {
	jsonOut := false
	check := false

	for _, a := range args {
		switch a {
		case "--json", "-j":
			jsonOut = true
		case "--check":
			check = true
		default:
			usererror.Error("version does not accept %q", a)
			usererror.Usage("mpm version [--json] [--check]")
			return 1
		}
	}

	info := buildinfo.Current(versionBinaryName)

	var update *buildinfo.UpdateResult
	if check {
		result := buildinfo.DefaultChecker().Check(context.Background(), info.Commit)
		update = &result
	}

	if jsonOut {
		payload := versionJSON{Info: info, Update: update}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return usererror.Error("could not render the version as JSON: %v", err)
		}
		fmt.Println(string(raw))
		return 0
	}

	fmt.Print(buildinfo.FormatHuman(info))
	if update != nil {
		fmt.Println()
		fmt.Println(formatUpdateSummary(*update, info))
	}
	return 0
}

// versionCommandRequested reports whether argv is a request for the
// version surface.
//
// Only the FIRST argument decides. `mpm recall version` searches for the
// word "version"; it is not a version request.
func versionCommandRequested(args []string) bool {
	return buildinfo.ArgvRequestsVersion(args)
}

// runVersionCommand handles a version request before any other startup
// work happens.
//
// Version reporting must be fast, offline and side-effect-free: it is
// what an operator runs when something looks wrong, and it must answer
// even when the configuration directory or the database is unusable.
// Intercepting it here means the command never runs the permission
// sweep, never reads the scheduler health bridge and never opens the
// database.
//
// The second return value reports whether the request was handled here.
// A help request falls through, so the router's existing per-command
// help machinery keeps working.
func runVersionCommand(args []string) (int, bool) {
	if !versionCommandRequested(args) {
		return 0, false
	}
	for _, a := range args {
		// "help" is the router's rewrite artifact for -h/--help. It
		// must keep its help meaning, so it falls through to the
		// router's per-command help page rather than being reported
		// as an unknown version flag.
		if a == "-h" || a == "--help" || a == "help" {
			return 0, false
		}
	}
	return handleVersion(args[1:]), true
}

// formatUpdateSummary renders the update result as one operator-readable
// line.
//
// The distance is reported only when the upstream could establish it from
// verified commit ancestry; an unknown result says so instead of
// guessing.
func formatUpdateSummary(result buildinfo.UpdateResult, info buildinfo.Info) string {
	upstream := result.UpstreamCommit
	if upstream == "" {
		upstream = "unknown"
	}

	switch result.Status {
	case buildinfo.UpdateCurrent:
		return fmt.Sprintf("Update: current — this build matches upstream main (%s)", upstream)
	case buildinfo.UpdateBehind:
		return fmt.Sprintf("Update: behind — %d commit(s) behind upstream main (%s → %s)",
			result.BehindBy, info.ShortCommit, upstream)
	case buildinfo.UpdateAhead:
		return fmt.Sprintf("Update: ahead — %d commit(s) ahead of upstream main (%s → %s)",
			result.AheadBy, info.ShortCommit, upstream)
	case buildinfo.UpdateDiverged:
		return fmt.Sprintf("Update: diverged — %d commit(s) behind and %d commit(s) ahead of upstream main (%s → %s)",
			result.BehindBy, result.AheadBy, info.ShortCommit, upstream)
	default:
		reason := strings.TrimSpace(result.Error)
		if reason == "" {
			reason = "the upstream state could not be determined"
		}
		return "Update: unknown — " + reason
	}
}
