// cmd/mpm-telemetry/observe.go — the HighTokenNoArtifactHunt, as a
// READ-ONLY DIAGNOSTIC.
//
// WHAT THIS COMMAND IS. An anomaly detector. It aggregates token burn
// per session from the telemetry ledger, asks the substrate how many
// durable artifacts that session produced, and REPORTS the sessions
// with a high token burn and no artifacts. Per spec §10 it is an
// observation, not an ROI metric: a session that burned tokens and
// recorded nothing may still have been valuable.
//
// WHY IT IS READ-ONLY. The trust boundary. A token count and a missing
// artifact are NOT evidence that a particular lesson exists:
//
//   - The observation's evidence — the token counts — lives in
//     telemetry.db, which no citation type can address. Every type in
//     P2's vocabulary (memory / theory / decision / lesson / work /
//     tool_invocation) resolves inside mpm.db.
//   - The observation is about an ABSENCE. It fires when a session
//     recorded NO artifacts, so the artifact the observation is about
//     does not exist, and there is nothing in mpm.db to cite. Citing
//     an unrelated artifact would be inventing a citation.
//
// The old path wrote the observation to `lessons_base` — a TRUSTED
// lesson, trusted on arrival, because lessons_base has no status
// column. That is the M5 trust boundary the rest of the substrate
// respects, and it was the last exception.
//
// THREE DEFECTS THIS FILE FIXES.
//
//  1. THE COUNT CAME FROM A TOOL THAT DOES NOT EXIST. The hunt shelled
//     out to `mpm call mpm_provenance --payload
//     {"action":"count_by_session"}`. The registry publishes 21 tools
//     and no mpm_provenance, so the hunt FAILED on every qualifying
//     session ("mpm_provenance: exit status 1") and the command exited
//     1. The count is now computed IN-PROCESS, read-only, against the
//     substrate's own attribution rule (internal/attribution) — the
//     same rule the critic's telemetry hunt uses.
//
//     A subprocess could not have been read-only anyway: every `mpm
//     call` writes a tool_invocations row into mpm.db, including a
//     call that only reads.
//
//  2. THE STORE PATH WAS WRONG IN TWO PLACES. The collector writes to
//     <workspace>/src/db/telemetry.db; docs/SPEC.md §6.6 names
//     $MPM_WORKSPACE/telemetry.db. The code's path is authoritative —
//     it is where the frames land. The path is now resolved by ONE
//     resolver (internal/telemetry.ResolveStorePath), shared with the
//     collector, the readers and the critic.
//
//  3. THE "DRY RUN" WROTE. The dry run called telemetry.Open, which
//     applies the DDL and therefore CREATES the telemetry database it
//     was looking for. A dry run that creates a database is not a dry
//     run. Both stores are now opened read-only.
//
// WHAT THE DEFAULT MODE DOES. It refuses, and says why. The command's
// documented default was "write a lesson"; that write is the trust
// boundary this issue is about, so changing the default's semantics is
// an operator decision, not a code fix. Until that decision is made,
// the default mode refuses loudly — preserving the behaviour the
// command has always had in practice (it failed, and wrote nothing)
// while making the failure legible.
//
// The exact spec amendment is proposed in
// docs/dev/session-b-139-telemetry-observe.md.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm/internal/attribution"
	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// writeRefusal is the message the default (write) mode prints. It names
// the reason, the alternative, and the fact that nothing was written.
const writeRefusal = `mpm-telemetry observe: the default mode no longer writes an observation, and nothing was written.

A telemetry observation is not evidence that a lesson exists. The token counts live in telemetry.db, which no citation type can address, and the observation is about an ABSENCE — a session that recorded nothing — so there is nothing in mpm.db to cite. Writing it would create a TRUSTED lesson (lessons_base has no status column, so a row there is trusted on arrival), which is the trust boundary this command must not cross.

Use --dry-run for the read-only diagnostic report. The change to the default's documented semantics is proposed in docs/SPEC.md §6.6 and docs/dev/session-b-139-telemetry-observe.md.`

func runObserve(args []string) error {
	// 2026-09-14 release-pass: help flags exit 0 BEFORE flag
	// parsing or workspace resolution. Pre-fix the help path
	// raised an unrelated error instead of showing help.
	if hasHelpFlag(args) {
		writeHelpObserve(os.Stdout, buildVersion)
		return nil
	}
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	since := fs.Int64("since", 0, "Unix epoch seconds; default = now-7d")
	threshold := fs.Int64("threshold", 100000, "high-token threshold (input + output)")
	minInv := fs.Int("min-invocations", 1, "minimum invocations per session")
	// --mpm selected the binary used for the cross-DB lookup. The
	// lookup is now in-process, so the flag has nothing to select.
	//
	// It is still ACCEPTED, so an operator's existing invocation does
	// not break on an unknown flag, but it is ignored and it says so.
	// The removal is proposed with the rest of the CLI amendment in
	// docs/dev/session-b-139-telemetry-observe.md.
	mpmPath := fs.String("mpm", "", "deprecated and ignored: the artifact count is computed in-process")
	dryRun := fs.Bool("dry-run", false, "read-only diagnostic: report the observations, write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *since == 0 {
		*since = time.Now().Add(-7 * 24 * time.Hour).Unix()
	}
	if strings.TrimSpace(*mpmPath) != "" {
		fmt.Fprintln(os.Stderr, "mpm-telemetry observe: --mpm is deprecated and ignored; "+
			"the artifact count is computed in-process from the substrate, read-only")
	}

	workspace := strings.TrimSpace(os.Getenv("MPM_WORKSPACE"))

	// The telemetry ledger. The canonical path, opened READ-ONLY: a
	// diagnostic must not create the store it is looking for.
	storePath, err := telemetry.ResolveStorePath(workspace)
	if err != nil {
		return err
	}
	store, err := telemetry.OpenReadOnly(storePath)
	if err != nil {
		return fmt.Errorf("open telemetry store %s: %w",
			telemetry.DescribeStorePath(storePath), err)
	}
	defer store.Close()

	// The substrate, opened READ-ONLY, for the artifact count.
	subPath, err := substrateDBPath(workspace)
	if err != nil {
		return err
	}
	subDB, err := openSubstrateReadOnly(subPath)
	if err != nil && !errors.Is(err, errSubstrateAbsent) {
		return err
	}
	subAbsent := errors.Is(err, errSubstrateAbsent)
	if subAbsent {
		// No substrate at all: nothing could have been attributed to
		// any session. That is a measurement, not a failure — but it
		// is said out loud, because "zero artifacts" and "no
		// substrate" are different claims.
		fmt.Fprintf(os.Stderr,
			"mpm-telemetry observe: no substrate at %s; every session counts as zero-artifact\n",
			subPath)
	} else {
		defer subDB.Close()
	}

	countFn := func(ctx context.Context, sessionID string) (int, error) {
		if subAbsent {
			return 0, nil
		}
		return attribution.SessionArtifactCount(ctx, subDB, sessionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	findings, err := telemetry.Hunt(ctx, store, telemetry.HuntConfig{
		Since: *since, HighTokenThreshold: *threshold, MinInvocations: *minInv,
	}, countFn)
	if err != nil {
		return err
	}

	if *dryRun {
		for _, f := range findings {
			out, err := json.MarshalIndent(telemetry.RenderObservation(f), "", "  ")
			if err != nil {
				return fmt.Errorf("render observation: %w", err)
			}
			fmt.Println(string(out))
		}
		return nil
	}

	if len(findings) == 0 {
		// Nothing was found, so nothing could have been written. This
		// preserves the command's behaviour on a quiet store: exit 0,
		// no output, no writes.
		return nil
	}

	return fmt.Errorf("%s\n\n(%d observation(s) found; nothing was written)",
		writeRefusal, len(findings))
}
