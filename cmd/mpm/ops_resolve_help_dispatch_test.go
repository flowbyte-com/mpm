// ops_resolve_help_dispatch_test.go — regression test for the
// `mpm ops resolve-contradictions --help` dispatch problem (issue #10
// finalisation, 2026-10-14).
//
// The defect
//
// printResolveHelp documents --adjudicate=ID:REASON, and the handler
// recognises a help token and returns 0 before opening a database. Both
// were true, and neither was reachable: `ops resolve-contradictions` was
// absent from subcommandHelpOwners, so Execute intercepted the help token
// and rendered the generic `mpm ops` family page. The command's own page
// was dead code.
//
// What that cost
//
// The `mpm ops` page lists every subcommand on one line, including
//
//	resolve-contradictions [--dry-run/--apply/--json/--limit=N
//	                       /--adjudicate=ID:REASON] ...
//
// so the flag is technically *mentioned*. But the family page says nothing
// about what adjudication is, that it is PER ROW, that there is
// deliberately no batch form, or that --apply does nothing until a row has
// been adjudicated — which is the workflow the whole #10 gate exists to
// enforce. An operator reading only the reachable help could run --apply on
// a freshly-proposed queue and reasonably expect rows to resolve.
//
// That is why this test asserts WHICH PAGE was printed, not merely that
// the word "adjudicate" appears. The word appears in both; asserting on it
// alone would pass before the fix and pin nothing.
//
// Safety
//
// Owning the path is only safe if the handler is inert on a help token.
// The help branch returns before getDBConcrete(), so no database is opened
// and no queue row is read or written. This test proves the inertness
// itself rather than trusting the reading: the database and workspace
// digests are compared before and after.
//
// Every command runs against an isolated MPM_WORKSPACE + HOME with all
// model providers pointed at a closed port. Nothing touches the operator's
// real database.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// familyPageMarker is a string unique to the generic `mpm ops` page that
// Execute rendered in place of the command's own help. Asserting its
// absence is what makes this test pin the dispatch rather than the flag.
const familyPageMarker = "Engine Room"

// commandPageMarker is the command's own usage signature, printed only by
// printResolveHelp.
const commandPageMarker = "mpm ops resolve-contradictions [--propose]"

// runResolveHelp executes `mpm ops resolve-contradictions <flag>` in the
// isolated environment and returns its combined output and exit code.
func runResolveHelp(t *testing.T, bin, workspace string, env []string, flag string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, "ops", "resolve-contradictions", flag)
	cmd.Env = env
	cmd.Dir = workspace
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %s: %v\n%s", flag, err, out)
		}
	}
	return string(out), code
}

// asExitError is errors.As specialised to *exec.ExitError, kept local so
// the import list of this file stays minimal and obvious.
func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestOpsResolveContradictions_HelpReachesCommandPage(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}

	// Both spellings must work. parseFlags rewrites -h and --help to the
	// same literal token, but only if the router's rewrite still covers
	// both; a regression in one is invisible in the other.
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			bin := buildTestBinary(t)

			tmp := t.TempDir()
			workspace := filepath.Join(tmp, "workspace")
			home := filepath.Join(tmp, "home")
			for _, d := range []string{workspace, home} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatalf("mkdir %s: %v", d, err)
				}
			}
			// Seed first: this creates the database and normalises
			// workspace permissions, so the baseline digests below are
			// taken against a settled workspace rather than one the first
			// command is still initialising.
			seedCanaries(t, bin, workspace, home)
			dbPath := findDB(t, workspace)
			baseDB := digestDB(t, dbPath)
			baseFS := digestFS(t, workspace)

			out, code := runResolveHelp(t, bin, workspace, isolatedEnv(workspace, home), flag)

			if code != 0 {
				t.Errorf("%s must exit 0; got %d\n%s", flag, code, clip(out, 400))
			}

			if !strings.Contains(out, commandPageMarker) {
				t.Errorf("%s printed the wrong page: the command's own usage signature %q is absent, "+
					"which means Execute intercepted and rendered the family page.\n%s",
					flag, commandPageMarker, clip(out, 600))
			}
			if strings.Contains(out, familyPageMarker) {
				t.Errorf("%s still rendered the generic `mpm ops` family page (%q found).\n%s",
					flag, familyPageMarker, clip(out, 600))
			}

			// The flag that decides whether a row may penalise a memory
			// must be documented on the page an operator actually reaches.
			for _, want := range []string{"--adjudicate=ID:REASON", "ADJUDICATED"} {
				if !strings.Contains(out, want) {
					t.Errorf("%s output does not document %q", flag, want)
				}
			}

			// And the help request must be inert: no database opened, no
			// queue row written, nothing enqueued by an implicit --propose.
			if got := digestDB(t, dbPath); got != baseDB {
				t.Errorf("%s MUTATED the database; the help branch must return before getDBConcrete()", flag)
			}
			if got := digestFS(t, workspace); got != baseFS {
				t.Errorf("%s MUTATED the workspace; the help branch must return before getDBConcrete()", flag)
			}
		})
	}
}

// TestOpsResolveContradictions_HelpPageIsTheOwnerPage pins the table
// membership itself, so the entry cannot be silently dropped from
// subcommandHelpOwners without this test failing for the reason stated
// above rather than as an unexplained behaviour change.
func TestOpsResolveContradictions_HelpPageIsTheOwnerPage(t *testing.T) {
	if !subcommandHelpOwners["ops resolve-contradictions"] {
		t.Error(`"ops resolve-contradictions" is missing from subcommandHelpOwners; without it Execute ` +
			"intercepts and renders the generic `mpm ops` page instead of the command's own")
	}
	if reason, ok := knownNonOwningSubcommandHelp["ops resolve-contradictions"]; ok {
		t.Errorf(`"ops resolve-contradictions" is still classified as a known non-owner (%q); `+
			"the path is classified twice, which describes its behaviour two incompatible ways", reason)
	}
}
