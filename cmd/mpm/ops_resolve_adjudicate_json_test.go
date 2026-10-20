// ops_resolve_adjudicate_json_test.go — the JSON contract of
// `mpm ops resolve-contradictions --adjudicate` (issue #21, Phase B,
// final review corrections).
//
// The defect these pin
//
// An adjudication is a WRITE. It stamps a queue row as operator-adjudicated,
// and that stamp is what makes the row eligible to penalise a memory later.
// The command reported a failed adjudication by writing a line to stderr and
// exiting 1 — with no JSON document at all, even in --json mode. A consumer
// that asked for JSON got nothing: it could not tell a failed adjudication
// from a crash, and it could not tell WHICH request failed.
//
// The worse half was the partial success. An invocation that named three
// rows and failed on the second had already written the first. The report
// described only the failure, so the durable side effect was invisible.
//
// The fixes
//
//  1. A failed --json --adjudicate produces EXACTLY ONE envelope, with a
//     non-zero exit and the error inside the envelope.
//
//  2. The envelope's `adjudications` array reports every request the
//     operator made, in order: what happened to each one, and the error for
//     the one that failed. An earlier success is not concealed.
//
//  3. --json --help produces one JSON document, with the help text INSIDE
//     the envelope. Plain help text on stdout would be a second,
//     unparseable document. The human help path is unchanged.
//
// Every test here runs against a disposable database under t.TempDir(). The
// live MPM database is never opened, and no live queue row is touched.

package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// seedSharedQueueRow inserts ONE unresolved queue row into the shared queue
// and returns its id.
//
// The row is written through a SECOND connection to the shared file, not
// through the manager's pool. The manager's pool is the thing under test
// elsewhere (the federated scan), and borrowing it here would couple the two
// tests: a row seeded through the pool would inherit the pool's connection
// state.
func seedSharedQueueRow(t *testing.T, dm *mpminternal.DatabaseManager, idA, idB string) int64 {
	t.Helper()
	sharedPath := dm.SharedAttached()
	require.NotEmpty(t, sharedPath, "the shared database must be attached")

	shared, err := sql.Open("sqlite3", sharedPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shared.Close() })

	res, err := shared.Exec(`
		INSERT INTO contradiction_log
			(memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at)
		VALUES (?, ?, 'seeded', 0.91, 'similarity-scan', CURRENT_TIMESTAMP)`, idA, idB)
	require.NoError(t, err)
	rowID, err := res.LastInsertId()
	require.NoError(t, err)
	return rowID
}

// queueRowDetectedBy reads a queue row's provenance back, through a
// connection of its own.
//
// It is deliberately not read through the manager's pool: the question this
// helper answers is "what is durably stored", and the pool's connection
// state is exactly what the other tests in this file are about.
func queueRowDetectedBy(t *testing.T, dm *mpminternal.DatabaseManager, id int64) string {
	t.Helper()
	sharedPath := dm.SharedAttached()
	require.NotEmpty(t, sharedPath)

	shared, err := sql.Open("sqlite3", sharedPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shared.Close() })

	var detectedBy sql.NullString
	err = shared.QueryRow(
		`SELECT detected_by FROM contradiction_log WHERE id = ?`, id).Scan(&detectedBy)
	require.NoError(t, err, "the queue row must exist")
	return detectedBy.String
}

// adjudicationEntries reads the envelope's `adjudications` array.
func adjudicationEntries(t *testing.T, env map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := env["adjudications"]
	require.True(t, ok, "the envelope must always carry %q; keys: %v", "adjudications", envKeys(env))
	list, ok := raw.([]interface{})
	require.True(t, ok, "adjudications must be an array, got %T", raw)
	out := make([]map[string]interface{}, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]interface{})
		require.True(t, ok, "each adjudication must be an object, got %T", item)
		out = append(out, obj)
	}
	return out
}

// A failed --json --adjudicate must produce ONE envelope, a non-zero exit
// and an accurate error.
//
// Before the fix this produced no document at all: the command wrote a line
// to stderr and returned 1.
func TestResolveContradictions_JSONAdjudicateFailureProducesOneEnvelope(t *testing.T) {
	setupResolveProposeTest(t)

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json", "--adjudicate=4242:read both memories and they conflict",
			})
		})
	})

	require.Equal(t, 1, code, "a failed adjudication must exit non-zero")

	env := decodeSingleEnvelope(t, out)
	require.NotEmpty(t, envelopeErrorText(t, env),
		"the envelope must carry the failure")
	require.Contains(t, envelopeErrorText(t, env), "4242",
		"the error must name the row that failed")

	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 1, "one request was made, so one outcome is reported")
	require.Equal(t, "failed", entries[0]["status"])
	require.NotNil(t, entries[0]["error"],
		"the failed request must carry its own error")
	require.Equal(t, float64(4242), entries[0]["queue_id"])
}

// The partial-success case: one request succeeds, the next fails.
//
// The envelope must show BOTH. A report that described only the failure
// would conceal the row that was already written.
func TestResolveContradictions_JSONAdjudicatePartialFailureKeepsTheEarlierSideEffect(t *testing.T) {
	dm := setupResolveProposeTest(t)

	good := seedSharedQueueRow(t, dm, "m-a", "m-b")

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				fmt.Sprintf("--adjudicate=%d:read both memories and they conflict", good),
				"--adjudicate=9999:this row does not exist",
			})
		})
	})

	require.Equal(t, 1, code, "a failed adjudication must exit non-zero")

	env := decodeSingleEnvelope(t, out)
	require.NotEmpty(t, envelopeErrorText(t, env))
	require.Contains(t, envelopeErrorText(t, env), "9999",
		"the error must name the row that failed")

	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 2, "both requests must be reported")

	require.Equal(t, float64(good), entries[0]["queue_id"])
	require.Equal(t, "adjudicated", entries[0]["status"],
		"the earlier request succeeded, and the report must say so")
	require.Nil(t, entries[0]["error"], "a successful request carries no error")

	require.Equal(t, float64(9999), entries[1]["queue_id"])
	require.Equal(t, "failed", entries[1]["status"])
	require.NotNil(t, entries[1]["error"], "the failed request must carry its error")

	// The report must match the durable state: the first row really was
	// written, and the second really was not touched.
	detectedBy := queueRowDetectedBy(t, dm, good)
	require.Contains(t, detectedBy, "adjudicated:",
		"the earlier adjudication must be durable, not merely reported")
}

// The request after the failing one is reported, not dropped.
//
// The invocation stops at the first failure — a later request is not
// attempted on top of a broken one — but the operator is told that it was
// not attempted, rather than being left to guess.
func TestResolveContradictions_JSONAdjudicateReportsTheRequestsItDidNotAttempt(t *testing.T) {
	setupResolveProposeTest(t)

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				"--adjudicate=1111:first request fails",
				"--adjudicate=2222:second request is never attempted",
			})
		})
	})

	require.Equal(t, 1, code)

	env := decodeSingleEnvelope(t, out)
	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 2)

	require.Equal(t, "failed", entries[0]["status"])
	require.Equal(t, "not_attempted", entries[1]["status"],
		"a request after the failure is not attempted, and must say so")
	require.Nil(t, entries[1]["error"],
		"a request that was not attempted has no error of its own")
}

// --json --help must be JSON, all the way.
//
// The contract says stdout carries exactly one JSON document per --json
// invocation. Plain help text on stdout would be a second document, and a
// consumer parsing stdout would fail on it.
func TestResolveContradictions_JSONHelpIsOneJSONDocument(t *testing.T) {
	setupResolveProposeTest(t)

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{"--json", "help"})
		})
	})

	require.Equal(t, 0, code, "a help request is not a failure")

	// decodeSingleEnvelope fails if stdout holds anything other than
	// exactly one JSON object — which is the whole point.
	env := decodeSingleEnvelope(t, out)
	require.Nil(t, env["error"], "a help request carries no error")

	help := envelopeString(t, env, "help")
	require.Contains(t, help, "mpm ops resolve-contradictions")
	require.Contains(t, help, "--adjudicate=ID:REASON",
		"the JSON help must be the command's own help text")

	require.NotContains(t, out, "\nWHAT IS IN THE QUEUE",
		"plain help text must not be printed to stdout in JSON mode")
	require.True(t, strings.HasPrefix(strings.TrimSpace(out), "{"),
		"stdout must start with the JSON document")
}

// The human help path is unchanged: plain help text, on stdout, exit zero.
//
// The JSON fix must not have cost the human path its help page.
func TestResolveContradictions_NonJSONHelpStillGoesToStdout(t *testing.T) {
	setupResolveProposeTest(t)

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{"help"})
		})
	})

	require.Equal(t, 0, code)
	require.Contains(t, out, "mpm ops resolve-contradictions")
	require.Contains(t, out, "WHAT IS IN THE QUEUE")
	require.False(t, strings.HasPrefix(strings.TrimSpace(out), "{"),
		"the human help path must print help text, not JSON")
}

// The help text in the JSON envelope is the SAME text the human path prints.
//
// Two copies of the help would drift, and the copy that matters to a
// scripted consumer is the one nobody looks at.
func TestResolveContradictions_JSONHelpMatchesTheHumanHelp(t *testing.T) {
	setupResolveProposeTest(t)

	var humanOut string
	captureUserErrorOutput(t, func() {
		humanOut = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"help"}))
		})
	})

	var jsonOut string
	captureUserErrorOutput(t, func() {
		jsonOut = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "help"}))
		})
	})

	env := decodeSingleEnvelope(t, jsonOut)
	require.Equal(t, humanOut, envelopeString(t, env, "help"),
		"the JSON help must be the same text the human path prints")
}
