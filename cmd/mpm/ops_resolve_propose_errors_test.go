// ops_resolve_propose_errors_test.go — the ERROR contract of
// `mpm ops resolve-contradictions --propose` (issue #21, Phase B
// corrective commit).
//
// The contract is: --json produces EXACTLY ONE valid JSON envelope for
// EVERY outcome, including the ones where the command refused to do
// anything at all.
//
// A consumer that receives nothing cannot tell a broken queue from a
// crash. A consumer that receives two documents cannot parse either.
// The refusals covered here are the ones an operator can type by
// accident: an out-of-range budget, a typo'd flag, a flag combination
// that contradicts itself, a database that will not open.
//
// Every one of them must:
//   - exit non-zero;
//   - produce exactly one JSON document on stdout;
//   - report the reason inside that document;
//   - be decided WITHOUT consulting the database.
//
// The last point is not a style preference. A refusal that opened a
// database to describe itself would be a refusal with side effects, and
// the whole point of refusing early is that the operator gets the same
// answer whether or not a database happens to exist.
//
// Every test here runs against a disposable database under t.TempDir(). The
// live MPM database is never opened.

package main

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// envelopeErrorText reads the envelope's error field as text.
//
// A null error means "no error", which is the empty string here. The
// distinction the envelope cares about is null-vs-populated, and this
// helper keeps that distinction visible by returning "" for null rather
// than failing.
func envelopeErrorText(t *testing.T, env map[string]interface{}) string {
	t.Helper()
	v, ok := env["error"]
	require.True(t, ok, "the envelope must always carry an error field")
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	require.True(t, ok, "the error field must be a string or null, got %T", v)
	return s
}

// poisonDatabaseManager makes any attempt to reach the database produce a
// distinctive message on the usererror stream.
//
// The refusal paths are required to be decided WITHOUT consulting the
// database, and this is how a test proves it: if the command had reached
// getDBConcrete(), the sentinel would appear in the captured stderr.
func poisonDatabaseManager(t *testing.T) {
	t.Helper()
	prevManager := dbManager
	prevErr := dbManagerInitErr
	dbManager = nil
	dbManagerInitErr = errors.New("the database must not be consulted")
	t.Cleanup(func() {
		dbManager = prevManager
		dbManagerInitErr = prevErr
	})
}

// wantsPropose reports whether an argument list asks for a proposal.
//
// The refusal envelope describes the PROPOSAL phase, so a refusal with
// no --propose reports "not_run" and a refusal with --propose reports
// "invalid_flags".
func wantsPropose(args []string) bool {
	for _, a := range args {
		if a == "--propose" {
			return true
		}
	}
	return false
}

// ── invalid budget values and scan limits ──────────────────────────

// An invalid budget or scan limit produces exactly one JSON document.
//
// The two halves of this are the point: the document must be PARSEABLE
// (a consumer can read it as one object), and it must be PRESENT (a
// consumer gets an answer rather than silence).
func TestResolveContradictions_JSONErrorContract_InvalidBudgetValues(t *testing.T) {
	setupResolveProposeTest(t)

	cases := []struct {
		name string
		args []string
	}{
		{"propose-limit all", []string{"--propose", "--propose-limit=all"}},
		{"propose-limit not a number", []string{"--propose", "--propose-limit=many"}},
		{"propose-limit zero", []string{"--propose", "--propose-limit=0"}},
		{"propose-limit negative", []string{"--propose", "--propose-limit=-5"}},
		{"propose-limit over max", []string{"--propose", "--propose-limit=2001"}},
		{"propose-limit no value", []string{"--propose", "--propose-limit"}},
		{"compare-budget not a number", []string{"--propose", "--compare-budget=lots"}},
		{"compare-budget below min", []string{"--propose", "--compare-budget=999"}},
		{"compare-budget over max", []string{"--propose", "--compare-budget=10000001"}},
		{"compare-budget no value", []string{"--propose", "--compare-budget"}},
		{"candidate-cap not a number", []string{"--propose", "--candidate-cap=lots"}},
		{"candidate-cap zero", []string{"--propose", "--candidate-cap=0"}},
		{"candidate-cap over max", []string{"--propose", "--candidate-cap=10001"}},
		{"candidate-cap no value", []string{"--propose", "--candidate-cap"}},
		{"write-budget not a number", []string{"--propose", "--write-budget=lots"}},
		{"write-budget zero", []string{"--propose", "--write-budget=0"}},
		{"write-budget over max", []string{"--propose", "--write-budget=251"}},
		{"write-budget no value", []string{"--propose", "--write-budget"}},
		{"limit not a number", []string{"--limit=zero"}},
		{"limit zero", []string{"--limit=0"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// --json FIRST and --json LAST must behave identically. A
			// refusal can happen on the very first argument, so the
			// command has to have read the WHOLE argument list before
			// it could know that a document was owed. If the --json
			// detection were a single-pass affair, the second variant
			// would silently emit nothing.
			for _, variant := range [][]string{
				append([]string{"--json"}, tc.args...),
				append(append([]string{}, tc.args...), "--json"),
			} {
				var out string
				var code int
				captureUserErrorOutput(t, func() {
					out = captureStdout(t, func() {
						code = handleOpsResolveContradictions(variant)
					})
				})

				require.Equal(t, 1, code,
					"a refused invocation must exit non-zero: %v", variant)
				env := decodeSingleEnvelope(t, out)
				require.NotEmpty(t, envelopeErrorText(t, env),
					"the refusal must be reported inside the envelope: %v", variant)

				propose := envelopeObject(t, env, "propose")
				wantCompletion := "not_run"
				if wantsPropose(tc.args) {
					wantCompletion = "invalid_flags"
					require.Equal(t, "refused", envelopeString(t, propose, "commit_state"))
				}
				require.Equal(t, wantCompletion, envelopeString(t, propose, "completion"))
				require.Equal(t, 0.0, envelopeNumber(t, propose, "candidates_proposed"),
					"a refusal writes nothing")
			}
		})
	}
}

// ── unknown flags ──────────────────────────────────────────────────

// An unknown flag produces exactly one JSON document, and the help text
// does NOT land on stdout.
//
// The help is still offered — an operator who typed a wrong flag wants
// to see the right ones — but in --json mode stdout belongs to the
// document, so the help goes to stderr.
func TestResolveContradictions_JSONErrorContract_UnknownFlag(t *testing.T) {
	setupResolveProposeTest(t)

	for _, variant := range [][]string{
		{"--json", "--propose-everything"},
		{"--propose-everything", "--json"},
		{"--json", "--nope=1"},
	} {
		var out string
		var code int
		captureUserErrorOutput(t, func() {
			out = captureStdout(t, func() {
				code = handleOpsResolveContradictions(variant)
			})
		})

		require.Equal(t, 1, code)
		env := decodeSingleEnvelope(t, out)
		require.Contains(t, envelopeErrorText(t, env), "unknown flag")
		require.NotContains(t, out, "REJECTED COMBINATIONS",
			"the help text must not be mixed into the JSON document")
		require.NotContains(t, out, "mpm ops resolve-contradictions [--propose]",
			"the help text must not be mixed into the JSON document")
	}
}

// ── rejected flag combinations ─────────────────────────────────────

// A rejected flag combination produces exactly one JSON document.
//
// The combinations are refused before any database access, so the
// envelope has to be buildable without one. It is.
func TestResolveContradictions_JSONErrorContract_RejectedFlagCombinations(t *testing.T) {
	setupResolveProposeTest(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"propose with apply", []string{"--propose", "--apply"}, "cannot be combined"},
		{"propose with adjudicate", []string{"--propose", "--adjudicate=1:because"}, "cannot be combined"},
		{"propose-limit without propose", []string{"--propose-limit=50"}, "only applies to a proposal scan"},
		{"compare-budget without propose", []string{"--compare-budget=2000"}, "only applies to a proposal scan"},
		{"candidate-cap without propose", []string{"--candidate-cap=50"}, "only applies to a proposal scan"},
		{"write-budget without propose", []string{"--write-budget=10"}, "only applies to a proposal scan"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, variant := range [][]string{
				append([]string{"--json"}, tc.args...),
				append(append([]string{}, tc.args...), "--json"),
			} {
				var out string
				var code int
				captureUserErrorOutput(t, func() {
					out = captureStdout(t, func() {
						code = handleOpsResolveContradictions(variant)
					})
				})

				require.Equal(t, 1, code)
				env := decodeSingleEnvelope(t, out)
				require.Contains(t, envelopeErrorText(t, env), tc.want)

				propose := envelopeObject(t, env, "propose")
				wantCompletion := "not_run"
				if wantsPropose(tc.args) {
					wantCompletion = "invalid_flags"
				}
				require.Equal(t, wantCompletion, envelopeString(t, propose, "completion"))
			}
		})
	}
}

// ── database initialisation failure ────────────────────────────────

// A database that will not open still produces exactly one JSON
// document.
//
// This is the case where the refusal envelope has to be built WITHOUT
// the database: the database is the thing that failed. The completion
// name says the queue could not be reached.
func TestResolveContradictions_JSONErrorContract_DatabaseInitFailure(t *testing.T) {
	_ = getDB() // consume sync.Once so the lazy-init cannot fire later
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = nil
	dbManagerInitErr = errors.New("synthetic open failure")
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})

	for _, args := range [][]string{
		{"--json", "--propose"},
		{"--json"},
	} {
		var out string
		var code int
		captureUserErrorOutput(t, func() {
			out = captureStdout(t, func() {
				code = handleOpsResolveContradictions(args)
			})
		})

		require.Equal(t, 1, code, "a database that will not open is a failure")
		env := decodeSingleEnvelope(t, out)
		require.Contains(t, envelopeErrorText(t, env), "database not initialized")

		propose := envelopeObject(t, env, "propose")
		if wantsPropose(args) {
			require.Equal(t, "db_unavailable", envelopeString(t, propose, "completion"))
			require.Equal(t, "refused", envelopeString(t, propose, "commit_state"))
		} else {
			require.Equal(t, "not_run", envelopeString(t, propose, "completion"))
		}
	}
}

// ── the refusals never touch the database ──────────────────────────

// An early refusal is decided WITHOUT consulting the database.
//
// The proof is the poisoned manager: if the command had reached
// getDBConcrete(), the sentinel would have been printed. Its ABSENCE is
// the assertion.
func TestResolveContradictions_JSONErrorContract_EarlyRefusalNeverConsultsTheDatabase(t *testing.T) {
	setupResolveProposeTest(t)
	poisonDatabaseManager(t)

	cases := [][]string{
		{"--json", "--propose", "--compare-budget=abc"},
		{"--json", "--propose", "--propose-limit=all"},
		{"--json", "--propose", "--apply"},
		{"--json", "--propose", "--adjudicate=1:because"},
		{"--json", "--compare-budget=2000"},
		{"--json", "--nope"},
	}

	for _, args := range cases {
		var stderr string
		var code int
		out := captureStdout(t, func() {
			stderr = captureUserErrorOutput(t, func() {
				code = handleOpsResolveContradictions(args)
			})
		})

		require.Equal(t, 1, code, "a refused invocation must exit non-zero: %v", args)
		env := decodeSingleEnvelope(t, out)
		require.NotEmpty(t, envelopeErrorText(t, env),
			"the refusal must be reported inside the envelope: %v", args)

		require.NotContains(t, stderr, "the database must not be consulted",
			"the refusal must happen before the database is consulted: %v", args)
		require.NotContains(t, stderr, "opening database",
			"the refusal must happen before the database is opened: %v", args)
		require.NotContains(t, stderr, "database not initialized",
			"the refusal must happen before the database is opened: %v", args)
	}
}

// ── the ordinary failure paths ─────────────────────────────────────

// A refused proposal still produces exactly one JSON document.
//
// The refusal is reported at BOTH levels: the top-level error says the
// invocation failed, and the nested propose object says which phase
// refused and what the commit state is.
func TestResolveContradictions_JSONErrorContract_ProposalFailure(t *testing.T) {
	dm := setupResolveProposeTest(t)

	for i := 0; i < 6; i++ {
		seedProposeMemory(t, dm, "", "j-"+itoa(i),
			"the workspace must use pnpm for every install", unitVector(1, 0, 0))
	}

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{"--json", "--propose", "--write-budget=5"})
		})
	})
	require.Equal(t, 1, code)

	env := decodeSingleEnvelope(t, out)
	require.NotEmpty(t, envelopeErrorText(t, env))

	propose := envelopeObject(t, env, "propose")
	require.Equal(t, "write_budget", envelopeString(t, propose, "completion"))
	require.Equal(t, "refused", envelopeString(t, propose, "commit_state"))
	require.NotEmpty(t, envelopeString(t, propose, "error"))
	require.Nil(t, propose["rollback_error"],
		"the rollback succeeded, so there is no rollback error")
}

// A queue that cannot be LOADED still produces exactly one JSON
// document.
//
// The count is a COUNT(*), which succeeds on a row whose similarity
// column holds text; the load scans that column into a float, which
// fails. That difference is what makes the load the failing step.
func TestResolveContradictions_JSONErrorContract_QueueLoadFailure(t *testing.T) {
	dm := setupResolveProposeTest(t)

	// Write a row the loader cannot scan. The similarity column has
	// REAL affinity, so a non-numeric string stays TEXT.
	sharedPath := dm.SharedAttached()
	require.NotEmpty(t, sharedPath, "the shared database must be attached")
	shared, err := sql.Open("sqlite3", sharedPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shared.Close() })
	_, err = shared.Exec(`
		INSERT INTO contradiction_log
			(memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at)
		VALUES ('load-a', 'load-b', 'seeded', 'not-a-number', 'seeded', CURRENT_TIMESTAMP)`)
	require.NoError(t, err)

	// The COUNT succeeds...
	count, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 1, count)

	// ...but the LOAD fails.
	_, err = dm.LoadContradictionQueue(100)
	require.Error(t, err, "the loader must not be able to scan a non-numeric similarity")

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{"--json"})
		})
	})
	require.Equal(t, 1, code)

	env := decodeSingleEnvelope(t, out)
	require.NotEmpty(t, envelopeErrorText(t, env))
	require.Contains(t, strings.ToLower(envelopeErrorText(t, env)), "load")
	require.Equal(t, 1.0, envelopeNumber(t, env, "queue_total_unresolved"),
		"the count succeeded, so the total is known")
}
