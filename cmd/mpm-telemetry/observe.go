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
// WHAT THE DEFAULT MODE DOES. It REPORTS, and writes nothing.
//
// This is the approved policy (2026-10-28): `mpm-telemetry observe` is a
// read-only diagnostic by default. It used to promise a write — a
// `type: "observation"` lesson in the substrate — and that promise was
// the trust-boundary exception this issue is about.
//
// The write is gone, and it is gone for a reason that does not depend
// on anyone's discretion at run time:
//
//   - A telemetry observation is NOT evidence that a lesson exists. The
//     token counts live in telemetry.db, which no citation type can
//     address, and the observation is about an ABSENCE — a session that
//     recorded nothing — so there is nothing in mpm.db to cite.
//   - A trusted lesson (lessons_base has no status column, so a row
//     there is trusted on arrival) would therefore be an UNSUPPORTED
//     claim, and a pending theory would be an unresolvable one.
//
// So the observation is reported, explicitly labelled as an unverified
// diagnostic, and nothing is written: not a trusted lesson, not a
// pending theory, not a row of any kind. Arbitration is a human act.
//
// `--dry-run` is kept as a compatibility option: it used to be the
// "do not write" mode, and it now means exactly what the default
// already means.
//
// `--mpm` is DEPRECATED. It selected the binary the old cross-DB
// lookup shelled out to; there is no subprocess any more, so the flag
// has nothing to select. It is still accepted — an operator's existing
// invocation must not break on an unknown flag — but it is IGNORED, and
// it says so, loudly, on stderr. Silently accepting a flag that does
// nothing is how an operator keeps believing the command writes.
//
// The exact specification amendment is in docs/SPEC.md §6.6.

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

// mpmFlagDeprecation is the warning printed when --mpm is supplied.
//
// It is a WARNING, not a note, and it is not suppressible: a flag that
// no longer does anything must say so on every run that passes it,
// because the operator who still passes it is the one still believing
// the command shells out to `mpm` — and therefore, possibly, still
// believing it writes.
const mpmFlagDeprecation = `warning: --mpm is DEPRECATED and has no effect.

mpm-telemetry observe no longer shells out to mpm. The artifact count is
computed IN-PROCESS, read-only, from the substrate's own attribution
rule — so this flag has nothing to select, and the binary it names is
never executed.

--mpm will be removed in the next release.`

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
	// not break on an unknown flag, but it is IGNORED and it WARNS.
	// It will be removed in the next release.
	mpmPath := fs.String("mpm", "", "DEPRECATED and ignored: the artifact count is computed in-process")
	// --dry-run is kept as a COMPATIBILITY option. It used to be the
	// "do not write" mode; the default is now read-only too, so the
	// flag means exactly what the default already means.
	dryRun := fs.Bool("dry-run", false, "compatibility alias: observe is already read-only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *since == 0 {
		*since = time.Now().Add(-7 * 24 * time.Hour).Unix()
	}
	if strings.TrimSpace(*mpmPath) != "" {
		fmt.Fprintln(os.Stderr, mpmFlagDeprecation)
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

	// REPORT. The default mode and --dry-run do the SAME thing: the
	// command is a read-only diagnostic, so there is no "write" mode
	// left to opt out of.
	//
	// --dry-run is accepted for compatibility and means exactly what
	// the default already means; it is referenced here so that the
	// intent is explicit rather than an unused variable.
	_ = *dryRun
	//
	// A quiet store produces no output and exit 0 — there is nothing
	// to report, which is not a failure.
	for _, f := range findings {
		out, err := json.MarshalIndent(telemetry.RenderObservation(f), "", "  ")
		if err != nil {
			return fmt.Errorf("render observation: %w", err)
		}
		fmt.Println(string(out))
	}
	return nil
}
