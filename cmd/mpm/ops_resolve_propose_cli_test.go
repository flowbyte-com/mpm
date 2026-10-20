// ops_resolve_propose_cli_test.go — the CLI contract of
// `mpm ops resolve-contradictions --propose` (issue #21, Phase B).
//
// Three properties are pinned here, and they are the properties a script
// depends on:
//
//  1. EXACTLY ONE JSON DOCUMENT per --json invocation. The command used to
//     emit the proposal statistics as a separate line, so a consumer reading
//     stdout had to handle two documents and had no way to know whether a
//     proposal had even run. The envelope is now one object with one stable
//     key set, whatever happened.
//
//  2. The refused flag combinations are refused BEFORE any database access.
//     An operator should get the same answer whether or not a database
//     happens to exist — and a combination that is refused must not have
//     half-run.
//
//  3. A failed proposal exits NON-ZERO and stops before the queue is
//     reviewed. A half-proposal followed by a half-review would leave the
//     operator unable to tell which part of the output describes durable
//     state.
//
// Every test here runs against a disposable database under t.TempDir(). The
// live MPM database is never opened.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/stretchr/testify/require"
)

// setupResolveProposeTest installs a disposable shared-database manager as the
// CLI's database singleton.
//
// The singleton is consumed with `_ = getDB()` before the swap, because
// getDBConcrete() calls getDB() and the lazy initialiser would otherwise
// overwrite the swap. MPM_WORKSPACE is redirected to a temp dir FIRST (by
// NewTestSharedDM), so that if this is the test which fires the initialiser,
// it opens a disposable workspace rather than the operator's.
func setupResolveProposeTest(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	dm := mpminternal.NewTestSharedDM(t)
	_ = getDB() // consume sync.Once so the lazy-init cannot fire later
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = dm
	dbManagerInitErr = nil
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})
	return dm
}

// captureUserError redirects the usererror package's writer for the duration
// of fn.
//
// usererror holds its own writer reference, so redirecting os.Stderr does not
// capture it — the package would keep writing to the original os.Stderr value
// it captured at init.
func captureUserErrorOutput(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := usererror.GetWriter()
	usererror.SetWriter(&buf)
	defer usererror.SetWriter(prev)
	fn()
	return buf.String()
}

// decodeSingleEnvelope decodes exactly one JSON document from s.
//
// It fails if there is more than one document, which is the whole point: a
// consumer that reads stdout must be able to parse it as ONE object without
// having to cope with a second one trailing.
func decodeSingleEnvelope(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var env map[string]interface{}
	require.NoError(t, dec.Decode(&env), "stdout must be exactly one JSON object:\n%s", s)
	require.False(t, dec.More(),
		"stdout must contain exactly ONE JSON document:\n%s", s)
	return env
}

// envelopeString reads a string field from the envelope.
func envelopeString(t *testing.T, env map[string]interface{}, key string) string {
	t.Helper()
	v, ok := env[key]
	require.True(t, ok, "the envelope must always carry %q; keys: %v", key, envKeys(env))
	s, ok := v.(string)
	require.True(t, ok, "%q must be a string, got %T", key, v)
	return s
}

// envelopeNumber reads a numeric field from the envelope.
func envelopeNumber(t *testing.T, env map[string]interface{}, key string) float64 {
	t.Helper()
	v, ok := env[key]
	require.True(t, ok, "the envelope must always carry %q; keys: %v", key, envKeys(env))
	f, ok := v.(float64)
	require.True(t, ok, "%q must be a number, got %T", key, v)
	return f
}

// envelopeObject reads a nested object from the envelope.
func envelopeObject(t *testing.T, env map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	v, ok := env[key]
	require.True(t, ok, "the envelope must always carry %q; keys: %v", key, envKeys(env))
	obj, ok := v.(map[string]interface{})
	require.True(t, ok, "%q must be an object, got %T", key, v)
	return obj
}

func envKeys(env map[string]interface{}) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	return out
}

// ── the JSON envelope ──────────────────────────────────────────────

// The envelope has ONE stable key set, whatever the command did.
//
// This is the property a script depends on: it must be able to read
// `env["propose"]["completion"]` whether the queue was empty, populated, or
// the proposal failed.
func TestResolveContradictions_JSONEnvelopeHasOneStableKeySet(t *testing.T) {
	dm := setupResolveProposeTest(t)

	// An empty queue, no proposal.
	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json"}))
		})
	})

	env := decodeSingleEnvelope(t, out)
	for _, key := range []string{
		"queue_size", "queue_total_unresolved", "applied", "close_calls",
		"needs_adjudication", "needs_adjudication_rows", "dry_run",
		"decisions", "propose", "error",
	} {
		require.Contains(t, env, key, "the envelope must always carry %q", key)
	}
	require.Nil(t, env["error"], "a successful run carries no error")

	propose := envelopeObject(t, env, "propose")
	for _, key := range []string{
		"requested", "scan_limit_per_source", "entries_scanned", "pairs_examined",
		"candidates_found_in_scan", "scan_complete", "candidates_materialised",
		"candidates_proposed", "candidates_skipped_pending",
		"candidates_skipped_duplicate", "candidates_skipped_constrained",
		"compare_budget", "candidate_cap", "write_budget", "completion",
		"commit_state", "error", "rollback_error",
	} {
		require.Contains(t, propose, key, "the propose object must always carry %q", key)
	}

	require.Equal(t, "not_run", envelopeString(t, propose, "completion"))
	require.False(t, propose["requested"].(bool))
	require.Nil(t, propose["error"])

	// The queue is empty, so both counts are zero.
	require.Equal(t, 0.0, envelopeNumber(t, env, "queue_size"))
	require.Equal(t, 0.0, envelopeNumber(t, env, "queue_total_unresolved"))
	require.Equal(t, 0.0, envelopeNumber(t, env, "needs_adjudication"))

	// The same key set must survive a populated queue and a proposal.
	seedProposeMemory(t, dm, "", "e-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "e-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	var out2 string
	captureUserErrorOutput(t, func() {
		out2 = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "--propose"}))
		})
	})
	env2 := decodeSingleEnvelope(t, out2)
	require.ElementsMatch(t, envKeys(env), envKeys(env2),
		"the envelope must have the same keys whatever happened")

	propose2 := envelopeObject(t, env2, "propose")
	require.Equal(t, "complete", envelopeString(t, propose2, "completion"))
	require.Equal(t, 1.0, envelopeNumber(t, propose2, "candidates_proposed"))
	require.Equal(t, 1.0, envelopeNumber(t, env2, "queue_total_unresolved"))
}

// A proposed row is a REVIEW ITEM, not a verdict, and it stays in the queue.
//
// The row is reported in the dry-run decision list with Adjudicated=false and
// a verification note explaining that the operator has to read both memories.
// It is not applied, and it is not discarded.
func TestResolveContradictions_ProposalRowIsAReviewItemAndStaysQueued(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "n-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "n-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "--propose"}))
		})
	})

	env := decodeSingleEnvelope(t, out)
	require.Equal(t, 0.0, envelopeNumber(t, env, "applied"),
		"a proposal must never apply anything")
	require.True(t, env["dry_run"].(bool))

	decisions, ok := env["decisions"].([]interface{})
	require.True(t, ok)
	require.Len(t, decisions, 1, "the proposed row is reviewed in the same invocation")
	dec := decisions[0].(map[string]interface{})
	require.Equal(t, false, dec["Adjudicated"],
		"a similarity candidate is not an adjudicated contradiction")
	require.NotEmpty(t, dec["VerificationNote"])

	// The row is still in the queue: it is waiting, not discarded.
	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 1, queue)
}

// Under --apply, an unadjudicated row is refused and reported as needing
// adjudication — and NO memory weight moves.
//
// This is the gate that makes the proposal safe to run: a candidate can be
// produced by an automated scan, but only an operator's explicit judgement
// can let it penalise a memory.
func TestResolveContradictions_ApplyRefusesAnUnadjudicatedRow(t *testing.T) {
	dm := setupResolveProposeTest(t)

	// A row with no adjudication segment. detected_by is empty, which the
	// verification classifier treats as unadjudicated.
	require.NoError(t, dm.EnqueueContradiction(mpminternal.ContradictionEvidence{
		MemoryA: "n-a", MemoryB: "n-b",
		Evidence: "seeded candidate", Similarity: 0.93,
	}))

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "--apply"}))
		})
	})

	env := decodeSingleEnvelope(t, out)
	require.Equal(t, 1.0, envelopeNumber(t, env, "needs_adjudication"))
	require.Equal(t, 0.0, envelopeNumber(t, env, "applied"),
		"an unadjudicated row must never penalise a memory")

	rows, ok := env["needs_adjudication_rows"].([]interface{})
	require.True(t, ok)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]interface{})
	require.Contains(t, row, "id")
	require.Contains(t, row, "adjudicate_with")

	// The row is still in the queue, unresolved.
	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 1, queue)
}

// queue_size is the batch this invocation reviewed; queue_total_unresolved
// is how many unresolved rows the queue holds. They differ whenever the
// queue is longer than the batch, and conflating them is how an operator
// concludes "the queue is clear" when it is not.
func TestResolveContradictions_QueueSizeIsTheBatchNotTheTotal(t *testing.T) {
	dm := setupResolveProposeTest(t)

	// Five rows in the queue, reviewed three at a time.
	for i := 0; i < 5; i++ {
		id := "q-" + itoa(i)
		require.NoError(t, dm.EnqueueContradiction(mpminternal.ContradictionEvidence{
			MemoryA: id + "-a", MemoryB: id + "-b",
			Evidence: "seeded", Similarity: 0.9,
		}))
	}

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "--limit=3"}))
		})
	})

	env := decodeSingleEnvelope(t, out)
	require.Equal(t, 3.0, envelopeNumber(t, env, "queue_size"),
		"queue_size is the batch this invocation reviewed")
	require.Equal(t, 5.0, envelopeNumber(t, env, "queue_total_unresolved"),
		"queue_total_unresolved is the whole queue")
}

// ── the proposal failure path ──────────────────────────────────────

// A refused proposal exits NON-ZERO, reports the refusal precisely, and
// reports zero written rows.
//
// The zero is the load-bearing part: an unconfirmed write is not a write,
// and a report that said "12 proposed" after a refused transaction would
// tell the operator their queue grew when it did not.
func TestResolveContradictions_ProposalFailureExitsNonZeroAndReportsZero(t *testing.T) {
	dm := setupResolveProposeTest(t)

	for i := 0; i < 6; i++ {
		seedProposeMemory(t, dm, "", "f-"+itoa(i),
			"the workspace must use pnpm for every install", unitVector(1, 0, 0))
	}

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			// 15 pairs qualify; the write budget is 5.
			code = handleOpsResolveContradictions([]string{"--json", "--propose", "--write-budget=5"})
		})
	})

	require.Equal(t, 1, code, "a failed proposal must exit non-zero")

	env := decodeSingleEnvelope(t, out)
	propose := envelopeObject(t, env, "propose")
	require.Equal(t, "write_budget", envelopeString(t, propose, "completion"))
	require.Equal(t, 0.0, envelopeNumber(t, propose, "candidates_proposed"))
	require.Equal(t, "refused", envelopeString(t, propose, "commit_state"))
	require.NotNil(t, propose["error"])

	// Nothing was written.
	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 0, queue)
}

// A failed proposal stops BEFORE the queue is reviewed.
//
// The envelope must not contain resolution decisions from a review that
// never happened, and the queue must be untouched.
func TestResolveContradictions_ProposalFailureStopsBeforeReview(t *testing.T) {
	dm := setupResolveProposeTest(t)

	// One pre-existing row that a review would have processed.
	require.NoError(t, dm.EnqueueContradiction(mpminternal.ContradictionEvidence{
		MemoryA: "existing-a", MemoryB: "existing-b",
		Evidence: "seeded", Similarity: 0.9,
	}))

	for i := 0; i < 6; i++ {
		seedProposeMemory(t, dm, "", "f-"+itoa(i),
			"the workspace must use pnpm for every install", unitVector(1, 0, 0))
	}

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code := handleOpsResolveContradictions([]string{"--json", "--propose", "--write-budget=5"})
			require.Equal(t, 1, code)
		})
	})

	env := decodeSingleEnvelope(t, out)
	decisions, ok := env["decisions"].([]interface{})
	require.True(t, ok)
	require.Empty(t, decisions,
		"a failed proposal must not be followed by a review")

	// The pre-existing row is untouched.
	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 1, queue)
}

// A failed proposal in HUMAN mode reports the failure, says that nothing was
// written, and stops.
//
// The human path must not print the review output either — an operator
// reading a dry-run table after a failed proposal would not know the
// proposal had failed.
func TestResolveContradictions_ProposalFailureInHumanModeReportsTheError(t *testing.T) {
	dm := setupResolveProposeTest(t)

	for i := 0; i < 6; i++ {
		seedProposeMemory(t, dm, "", "f-"+itoa(i),
			"the workspace must use pnpm for every install", unitVector(1, 0, 0))
	}

	var stdout string
	var stderr string
	stdout = captureStdout(t, func() {
		stderr = captureUserErrorOutput(t, func() {
			code := handleOpsResolveContradictions([]string{"--propose", "--write-budget=5"})
			require.Equal(t, 1, code)
		})
	})

	require.Contains(t, stderr, "write budget",
		"the refusal must name the budget that refused it")
	require.Contains(t, stdout, "Proposal did NOT complete (write_budget)",
		"the human path must say the proposal did not complete")
	require.Contains(t, stdout, "0 new queue row(s)",
		"the human path must say that nothing was written")
	require.NotContains(t, stdout, "Dry-run",
		"a failed proposal must not be followed by a review")
}

// ── the flag contracts ─────────────────────────────────────────────

// The refused combinations are refused BEFORE any database access.
//
// The proof is the poisoned manager: if the command consulted the database
// at all, it would report "database not initialized" instead of the refusal
// message. Seeing the refusal message therefore PROVES the database was not
// consulted.
func TestResolveContradictions_RefusedCombinationsHappenBeforeAnyDatabaseAccess(t *testing.T) {
	dm := setupResolveProposeTest(t)

	// Poison the manager so that any attempt to reach the database
	// produces a distinctive message.
	prevManager := dbManager
	prevErr := dbManagerInitErr
	dbManager = nil
	dbManagerInitErr = errors.New("the database must not be consulted")
	t.Cleanup(func() {
		dbManager = prevManager
		dbManagerInitErr = prevErr
	})

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
			var stderr string
			var code int
			captureStdout(t, func() {
				stderr = captureUserErrorOutput(t, func() {
					code = handleOpsResolveContradictions(tc.args)
				})
			})
			require.Equal(t, 1, code)
			require.Contains(t, stderr, tc.want)
			require.NotContains(t, stderr, "the database must not be consulted",
				"the refusal must happen before the database is consulted")
			require.NotContains(t, stderr, "database not initialized")
		})
	}

	// The queue is untouched.
	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 0, queue)
}

// The parser boundaries. Each of these is a value the operator can type and
// get a wrong answer from if it is not refused.
func TestResolveContradictions_ParserBoundaries(t *testing.T) {
	setupResolveProposeTest(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"propose-limit all", []string{"--propose", "--propose-limit=all"}, "is not supported"},
		{"propose-limit zero", []string{"--propose", "--propose-limit=0"}, "must be between 1 and 2000"},
		{"propose-limit over max", []string{"--propose", "--propose-limit=2001"}, "must be between 1 and 2000"},
		{"propose-limit not a number", []string{"--propose", "--propose-limit=many"}, "must be an integer"},
		{"propose-limit negative", []string{"--propose", "--propose-limit=-5"}, "must be between 1 and 2000"},
		{"propose-limit no value", []string{"--propose", "--propose-limit"}, "needs a value"},
		{"compare-budget below min", []string{"--propose", "--compare-budget=999"}, "must be between 1000 and 10000000"},
		{"compare-budget over max", []string{"--propose", "--compare-budget=10000001"}, "must be between 1000 and 10000000"},
		{"compare-budget not a number", []string{"--propose", "--compare-budget=lots"}, "must be an integer"},
		{"compare-budget no value", []string{"--propose", "--compare-budget"}, "needs a value"},
		{"candidate-cap zero", []string{"--propose", "--candidate-cap=0"}, "must be between 1 and 10000"},
		{"candidate-cap over max", []string{"--propose", "--candidate-cap=10001"}, "must be between 1 and 10000"},
		{"candidate-cap no value", []string{"--propose", "--candidate-cap"}, "needs a value"},
		{"write-budget zero", []string{"--propose", "--write-budget=0"}, "must be between 1 and 250"},
		{"write-budget over max", []string{"--propose", "--write-budget=251"}, "must be between 1 and 250"},
		{"write-budget no value", []string{"--propose", "--write-budget"}, "needs a value"},
		{"unknown flag", []string{"--propose-everything"}, "unknown flag"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr string
			var code int
			captureStdout(t, func() {
				stderr = captureUserErrorOutput(t, func() {
					code = handleOpsResolveContradictions(tc.args)
				})
			})
			require.Equal(t, 1, code)
			require.Contains(t, stderr, tc.want)
		})
	}
}

// The boundary values that ARE legal are accepted.
func TestResolveContradictions_LegalBudgetBoundariesAreAccepted(t *testing.T) {
	setupResolveProposeTest(t)

	for _, args := range [][]string{
		{"--propose", "--propose-limit=1"},
		{"--propose", "--propose-limit=2000"},
		{"--propose", "--compare-budget=1000"},
		{"--propose", "--compare-budget=10000000"},
		{"--propose", "--candidate-cap=1"},
		{"--propose", "--candidate-cap=10000"},
		{"--propose", "--write-budget=1"},
		{"--propose", "--write-budget=250"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr string
			captureStdout(t, func() {
				stderr = captureUserErrorOutput(t, func() {
					code := handleOpsResolveContradictions(args)
					require.Equal(t, 0, code,
						"a legal budget must be accepted; stderr was: %s", stderr)
				})
			})
		})
	}
}

// The default scan limit is 100 memories PER SOURCE, and an explicit limit
// overrides it.
//
// The limit is per source because the federated scan reads local and shared
// separately. A limit of 100 therefore admits up to 200 memories before the
// deduplication, and the report says which window was actually compared.
func TestResolveContradictions_ScanLimitDefaultsAndOverride(t *testing.T) {
	setupResolveProposeTest(t)

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "--propose"}))
		})
	})
	propose := envelopeObject(t, decodeSingleEnvelope(t, out), "propose")
	require.Equal(t, 100.0, envelopeNumber(t, propose, "scan_limit_per_source"),
		"the default scan limit is 100 memories per source")
	require.Equal(t, 100000.0, envelopeNumber(t, propose, "compare_budget"))
	require.Equal(t, 10000.0, envelopeNumber(t, propose, "candidate_cap"))
	require.Equal(t, 250.0, envelopeNumber(t, propose, "write_budget"))

	// An explicit limit overrides the default.
	var out2 string
	captureUserErrorOutput(t, func() {
		out2 = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions(
				[]string{"--json", "--propose", "--propose-limit=7", "--compare-budget=5000",
					"--candidate-cap=50", "--write-budget=9"}))
		})
	})
	propose2 := envelopeObject(t, decodeSingleEnvelope(t, out2), "propose")
	require.Equal(t, 7.0, envelopeNumber(t, propose2, "scan_limit_per_source"))
	require.Equal(t, 5000.0, envelopeNumber(t, propose2, "compare_budget"))
	require.Equal(t, 50.0, envelopeNumber(t, propose2, "candidate_cap"))
	require.Equal(t, 9.0, envelopeNumber(t, propose2, "write_budget"))
}

// --propose --dry-run STILL INSERTS proposal rows.
//
// --dry-run governs the RESOLUTION phase. --propose is a separate phase, and
// a scan that found candidates and enqueued none would be a scan that did
// nothing. The help text says so explicitly, and this pins it.
func TestResolveContradictions_ProposeWithDryRunStillInsertsRows(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "d-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "d-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions(
				[]string{"--json", "--propose", "--dry-run"}))
		})
	})

	env := decodeSingleEnvelope(t, out)
	require.True(t, env["dry_run"].(bool))
	propose := envelopeObject(t, env, "propose")
	require.Equal(t, 1.0, envelopeNumber(t, propose, "candidates_proposed"))

	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 1, queue,
		"--propose --dry-run STILL INSERTS PROPOSAL ROWS")
}

// A proposal WITHOUT --propose adds nothing to the queue. The write is
// opt-in by name.
func TestResolveContradictions_WithoutProposeNothingIsQueued(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "o-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "o-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json"}))
		})
	})

	propose := envelopeObject(t, decodeSingleEnvelope(t, out), "propose")
	require.False(t, propose["requested"].(bool))
	require.Equal(t, "not_run", envelopeString(t, propose, "completion"))

	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 0, queue,
		"without --propose the queue must not grow")
}

// A repeat proposal writes nothing and says so.
//
// The count is of CONFIRMED COMMITTED rows, so a repeat over unchanged data
// reports zero — an operator reading a non-zero number on every run would
// learn nothing from it.
func TestResolveContradictions_RepeatProposalReportsZero(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "r-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "r-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "--propose"}))
		})
	})
	first := envelopeObject(t, decodeSingleEnvelope(t, out), "propose")
	require.Equal(t, 1.0, envelopeNumber(t, first, "candidates_proposed"))

	for i := 0; i < 2; i++ {
		var out2 string
		captureUserErrorOutput(t, func() {
			out2 = captureStdout(t, func() {
				require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "--propose"}))
			})
		})
		second := envelopeObject(t, decodeSingleEnvelope(t, out2), "propose")
		require.Equal(t, 1.0, envelopeNumber(t, second, "candidates_found_in_scan"),
			"the candidate IS found again")
		require.Equal(t, 0.0, envelopeNumber(t, second, "candidates_proposed"),
			"but nothing is written again")
		require.Equal(t, 1.0, envelopeNumber(t, second, "candidates_skipped_pending"))
	}

	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 1, queue)
}

// The --help output must not touch the database.
//
// The help text is the operator's first stop, and it must be safe to run
// anywhere — including on an installation with no database at all.
func TestResolveContradictions_HelpIsInert(t *testing.T) {
	setupResolveProposeTest(t)

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--help"}))
		})
	})

	// The router's help dispatch depends on this prefix.
	require.Contains(t, out, "mpm ops resolve-contradictions [--propose]")
	require.Contains(t, out, "PROPOSAL BUDGETS")
	require.Contains(t, out, "TRANSACTIONAL SEMANTICS")
	require.Contains(t, out, "REJECTED COMBINATIONS")
	require.Contains(t, out, "--propose-limit=all is NOT supported")
}

// The help text must document the budgets the implementation actually has,
// or an operator cannot compute what a command will do.
func TestResolveContradictions_HelpDocumentsTheApprovedLimits(t *testing.T) {
	setupResolveProposeTest(t)

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--help"}))
		})
	})

	for _, want := range []string{
		"default 100",                // the per-source scan limit
		"max 2000",                   // the per-source scan ceiling
		"default 100,000",            // the comparison budget
		"range 1,000..10,000,000",    // the comparison budget range
		"default and maximum 10,000", // the candidate cap
		"default and maximum 250",    // the write budget
		"OLDEST FIRST",               // the review order
		"queue_total_unresolved",     // the total-vs-batch distinction
		"SIMILARITY IS EVIDENCE OF RELATIONSHIP, NOT PROOF OF CONTRADICTION",
	} {
		require.Contains(t, out, want,
			"the help text must document %q", want)
	}
}

// The human-readable output must not describe an incomplete candidate count
// as an exact corpus count.
func TestResolveContradictions_HumanOutputDoesNotClaimACorpusTotal(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "h-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "h-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--propose"}))
		})
	})

	require.Contains(t, out, "Scanned 2 memories")
	require.Contains(t, out, "1 new queue row")
	require.NotContains(t, out, "total contradictions found")
	require.NotContains(t, out, "across the corpus")
}

// The usererror writer must not be left redirected after a test.
func TestResolveContradictions_UserErrorWriterIsRestored(t *testing.T) {
	before := usererror.GetWriter()
	captureUserErrorOutput(t, func() {})
	require.Equal(t, before, usererror.GetWriter())
}

// ── failure classification ─────────────────────────────────────────

// A candidate that fails validation is not a database failure.
//
// The batch is refused before the first INSERT, so reporting it as
// "db_insert" would describe a write that was never attempted — and would
// send an operator looking at the database for a defect that is in the
// producer.
func TestClassifyProposalFailure_InvalidCandidateIsNotADatabaseFailure(t *testing.T) {
	err := &mpminternal.ErrInvalidCandidate{
		MemoryA:    "mem-a",
		MemoryB:    "mem-b",
		Underlying: errors.New("self-contradiction"),
	}
	require.Equal(t, "invalid_candidate", classifyProposalFailure(mpminternal.TransactionResult{}, err))
}

// An unusable budget is a write-budget refusal, not a database failure.
func TestClassifyProposalFailure_InvalidBudgetIsAWriteBudgetRefusal(t *testing.T) {
	err := &mpminternal.ErrInvalidWriteBudget{WriteBudget: 0, Candidates: 3}
	require.Equal(t, "write_budget", classifyProposalFailure(mpminternal.TransactionResult{}, err))
}

// An over-budget batch is still a write-budget refusal.
func TestClassifyProposalFailure_OverBudgetBatchIsAWriteBudgetRefusal(t *testing.T) {
	err := &mpminternal.ErrWriteBudgetExceeded{Eligible: 3, WriteBudget: 1}
	require.Equal(t, "write_budget", classifyProposalFailure(mpminternal.TransactionResult{}, err))
}

// A queue that cannot be reached must still produce EXACTLY ONE JSON
// DOCUMENT.
//
// The count is a reporting step that runs after the proposal. If it
// aborted the invocation before the envelope was written, a --json
// consumer would receive nothing at all — and could not tell a broken
// queue from a crash. The envelope carries the failure instead.
func TestResolveContradictions_JSONEmitsOneEnvelopeWhenTheQueueCannotBeCounted(t *testing.T) {
	dm := mpminternal.NewTestLocalOnlyDM(t)
	_ = getDB() // consume sync.Once so the lazy-init cannot fire later
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = dm
	dbManagerInitErr = nil
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})

	var out string
	rc := 0
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			rc = handleOpsResolveContradictions([]string{"--json"})
		})
	})

	require.Equal(t, 1, rc, "a queue that cannot be counted is a failure")
	env := decodeSingleEnvelope(t, out)

	// The total must be null, NOT zero. Zero says the queue is empty;
	// null says it could not be counted. Those are different claims and
	// only one of them is true.
	total, present := env["queue_total_unresolved"]
	require.True(t, present, "the key must be present even when the count failed")
	require.Nil(t, total, "an unknown total must be null, not zero")

	require.NotNil(t, env["error"], "the failure must be reported in the envelope")
	require.Contains(t, strings.ToLower(env["error"].(string)), "shared")
}

// A queue that cannot be counted must not be reported as an empty queue.
//
// This is the false-zero defect in its clearest form: the pre-existing
// code swallowed the count error whenever a proposal had also failed, and
// reported queue_total_unresolved: 0 — a claim that the operator's whole
// worklist is empty.
func TestResolveContradictions_JSONDoesNotReportAFailedCountAsZero(t *testing.T) {
	dm := mpminternal.NewTestLocalOnlyDM(t)
	_ = getDB()
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = dm
	dbManagerInitErr = nil
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})

	var out string
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			require.Equal(t, 1, handleOpsResolveContradictions([]string{"--json", "--propose"}))
		})
	})

	env := decodeSingleEnvelope(t, out)
	require.Nil(t, env["queue_total_unresolved"])
	require.NotNil(t, env["error"])
}

// The human path must not claim that nothing was written when the
// outcome could not be established.
//
// "Nothing was written" is a claim about durable state. When the commit
// state is uncertain, the state is unknown — and telling the operator
// that nothing was written is precisely the sentence that stops them
// looking.
func TestPrintProposalSummary_UncertainOutcomeDoesNotClaimNothingWasWritten(t *testing.T) {
	summary := proposeSummary{
		Requested:             true,
		EntriesScanned:        10,
		CandidatesFoundInScan: 2,
		Completion:            "rollback_failed",
		CommitState:           "uncertain",
	}

	out := captureStdout(t, func() {
		printProposalSummary(summary)
	})

	require.Contains(t, out, "rollback_failed")
	require.NotContains(t, out, "Nothing was written")
	require.Contains(t, strings.ToLower(out), "verify the queue")
}

// A confirmed rollback proves that nothing was written, and the human
// path may say so.
func TestPrintProposalSummary_ConfirmedRollbackSaysNothingWasWritten(t *testing.T) {
	summary := proposeSummary{
		Requested:             true,
		EntriesScanned:        10,
		CandidatesFoundInScan: 2,
		Completion:            "db_commit_rolled_back",
		CommitState:           "rolled_back",
	}

	out := captureStdout(t, func() {
		printProposalSummary(summary)
	})

	require.Contains(t, out, "Nothing was written")
}

// A queue that cannot be reached is not an insert failure.
//
// "db_insert" means an INSERT failed and the rollback was confirmed. When
// the shared schema is not attached, no INSERT was ever attempted — the
// batch never reached the database. Reporting the latter as the former
// sends the operator looking for a write defect that does not exist.
func TestClassifyProposalFailure_UnavailableQueueIsNotAnInsertFailure(t *testing.T) {
	err := &mpminternal.ErrSharedUnavailable{}
	require.Equal(t, "db_unavailable", classifyProposalFailure(mpminternal.TransactionResult{}, err))
}

// The same holds when the database itself cannot be reached.
func TestClassifyProposalFailure_UnavailableDatabaseIsNotAnInsertFailure(t *testing.T) {
	err := &mpminternal.ErrDatabaseUnavailable{Underlying: errors.New("connection refused")}
	require.Equal(t, "db_unavailable", classifyProposalFailure(mpminternal.TransactionResult{}, err))
}

// An untyped failure inside the transaction is still not an insert
// failure. The statement that failed was not an INSERT.
func TestClassifyProposalFailure_UntypedFailureIsNotAnInsertFailure(t *testing.T) {
	err := errors.New("read unresolved queue rows: disk I/O error")
	require.Equal(t, "db_error", classifyProposalFailure(mpminternal.TransactionResult{}, err))
}
