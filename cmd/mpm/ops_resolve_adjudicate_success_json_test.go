// ops_resolve_adjudicate_success_json_test.go — the reporting of a
// SUCCESSFUL adjudication (issue #21, Phase B, final reporting
// correction).
//
// The defect these pin
//
// An adjudication is a WRITE. The command stamps the queue row with the
// operator's judgement, and that stamp is what later lets the row
// penalise a memory.
//
// The post-adjudication JSON envelopes threw that write away. Five
// emission sites — the normal completion, the empty-queue completion, the
// propose failure, the queue-count failure and the queue-load failure —
// all hard-coded `Adjudications: []adjudicationOutcome{}` instead of
// carrying the outcomes the invocation had already accumulated. So an
// invocation that adjudicated a row successfully and then went on to
// finish, or to fail at a later stage, reported an EMPTY adjudication
// history.
//
// That is the wrong report in the direction that matters. The row WAS
// stamped; the envelope said nothing was stamped. A consumer reading only
// the envelope could not tell that a durable write had happened.
//
// The fixes
//
//  1. Every post-adjudication envelope carries the outcomes accumulated
//     so far. The pre-adjudication envelopes (refusal, help) still carry
//     an empty list, because no adjudication has happened yet.
//
//  2. The adjudication outcomes describe the requests in REQUEST order,
//     with the queue row each one named.
//
//  3. The report agrees with the durable queue state: a row reported as
//     "adjudicated" really is stamped, and a row that is not stamped is
//     not reported as adjudicated.
//
// Every test here runs against a disposable database under t.TempDir().
// The live MPM database is never opened, and no live queue row is
// touched.

package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// openSharedScratch opens a scratch database with the shared file
// ATTACHed under the name `shared`.
//
// The shared file has to be attached under that name, not opened as the
// connection's own main database: the shared schema contains trigger
// definitions that name the `shared` schema, and a connection that has no
// such schema cannot even re-parse that schema — every schema change
// against the file would fail with "cannot reference objects in database
// shared" before it reached its own statement.
func openSharedScratch(t *testing.T, dm *mpminternal.DatabaseManager) *sql.DB {
	t.Helper()
	sharedPath := dm.SharedAttached()
	require.NotEmpty(t, sharedPath, "the shared database must be attached")

	scratch, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "shared-scratch.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = scratch.Close() })

	attach := fmt.Sprintf("ATTACH DATABASE '%s' AS shared", strings.ReplaceAll(sharedPath, "'", "''"))
	_, err = scratch.Exec(attach)
	require.NoError(t, err)
	return scratch
}

// dropSharedQueueEvidenceColumn removes the `evidence` column from the
// shared queue table, leaving every other column in place.
//
// It builds a synthetic schema mismatch: a shared database written by a
// build whose queue table predates the `evidence` column. The queue COUNT
// (`SELECT COUNT(*) ... WHERE resolved_at IS NULL`) still works and the
// adjudication still works — but the queue LOAD names `evidence` and
// fails with "no such column: evidence".
//
// That is a command-stage failure that happens AFTER the adjudication has
// already been written, which is the only shape in which a later stage
// can bury an earlier success.
func dropSharedQueueEvidenceColumn(t *testing.T, dm *mpminternal.DatabaseManager) {
	t.Helper()
	scratch := openSharedScratch(t, dm)

	_, err := scratch.Exec(`ALTER TABLE shared.contradiction_log DROP COLUMN evidence`)
	require.NoError(t, err)
}

// installResolveOnAdjudicationTrigger installs a trigger on the shared
// queue table that resolves a row the moment it is adjudicated.
//
// It simulates a concurrent resolution: another operator, or another
// process, settles the row between this invocation's adjudication and its
// review. The row is therefore genuinely resolved by the time the queue
// is loaded, and the invocation reaches the EMPTY-QUEUE completion — the
// one post-adjudication path that a plain run cannot reach, because an
// adjudicated row is still unresolved and would otherwise still be in the
// queue.
//
// The trigger has to live in the SHARED database, so the helper
// connection attaches the shared file under the name `shared` rather than
// opening it as its own main database.
func installResolveOnAdjudicationTrigger(t *testing.T, dm *mpminternal.DatabaseManager) {
	t.Helper()
	scratch := openSharedScratch(t, dm)

	// The body names the table unqualified: inside a trigger, an
	// unqualified name resolves in the trigger's own schema, and SQLite
	// forbids qualifying a table inside a trigger body.
	_, err := scratch.Exec(`
		CREATE TRIGGER shared.resolve_on_adjudication
		AFTER UPDATE OF detected_by ON shared.contradiction_log
		BEGIN
			UPDATE contradiction_log SET resolved_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
		END;`)
	require.NoError(t, err)
}

// durableUnresolvedQueueCount counts the unresolved rows in the shared
// queue, through a connection of its own.
//
// It answers "what is durably stored", which is the question the report
// has to agree with.
func durableUnresolvedQueueCount(t *testing.T, dm *mpminternal.DatabaseManager) int {
	t.Helper()
	sharedPath := dm.SharedAttached()
	require.NotEmpty(t, sharedPath)

	shared, err := sql.Open("sqlite3", sharedPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shared.Close() })

	var n int
	err = shared.QueryRow(
		`SELECT COUNT(*) FROM contradiction_log WHERE resolved_at IS NULL`).Scan(&n)
	require.NoError(t, err)
	return n
}

// One successful adjudication is reported as one successful adjudication.
//
// The report must name the row the operator named, say that it was
// stamped, and carry no error.
func TestResolveContradictions_JSONAdjudicateSuccessReportsTheAdjudicatedRow(t *testing.T) {
	dm := setupResolveProposeTest(t)
	rowID := seedSharedQueueRow(t, dm, "s-a", "s-b")

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				fmt.Sprintf("--adjudicate=%d:read both memories and they conflict", rowID),
			})
		})
	})

	require.Equal(t, 0, code, "a successful adjudication is not a failure")

	env := decodeSingleEnvelope(t, out)
	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 1, "one request was made, so one outcome is reported")
	require.Equal(t, float64(rowID), entries[0]["queue_id"],
		"the report must name the row the operator named")
	require.Equal(t, "adjudicated", entries[0]["status"],
		"the row was stamped, and the report must say so")
	require.Nil(t, entries[0]["error"], "a successful request carries no error")
}

// Two successful adjudications are reported as two, in REQUEST order.
//
// The rows are adjudicated in DESCENDING id order on purpose: a report
// that listed them in ascending id order would look identical to a
// report that listed them in request order, and only one of them is the
// contract.
func TestResolveContradictions_JSONAdjudicateSuccessReportsBothRowsInRequestOrder(t *testing.T) {
	dm := setupResolveProposeTest(t)
	lower := seedSharedQueueRow(t, dm, "r-a", "r-b")
	higher := seedSharedQueueRow(t, dm, "r-c", "r-d")
	require.Greater(t, higher, lower, "the fixture must produce ascending ids")

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				fmt.Sprintf("--adjudicate=%d:the second request, asked for first", higher),
				fmt.Sprintf("--adjudicate=%d:the first request, asked for second", lower),
			})
		})
	})

	require.Equal(t, 0, code)

	env := decodeSingleEnvelope(t, out)
	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 2, "both requests must be reported")

	require.Equal(t, float64(higher), entries[0]["queue_id"],
		"the report must follow the order the operator asked in")
	require.Equal(t, "adjudicated", entries[0]["status"])
	require.Nil(t, entries[0]["error"])

	require.Equal(t, float64(lower), entries[1]["queue_id"])
	require.Equal(t, "adjudicated", entries[1]["status"])
	require.Nil(t, entries[1]["error"])

	// Both writes really happened.
	require.Contains(t, queueRowDetectedBy(t, dm, higher), "adjudicated:")
	require.Contains(t, queueRowDetectedBy(t, dm, lower), "adjudicated:")
}

// A successful adjudication survives a LATER command-stage failure.
//
// The queue load fails here — the shared queue table is missing the
// `evidence` column the load selects — but the adjudication that ran
// before it was already written. The error envelope must still say so.
//
// Before the fix this envelope reported an empty adjudication history:
// the invocation would have said "nothing was adjudicated, and by the way
// the queue could not be read", which reads as a failed adjudication.
func TestResolveContradictions_JSONAdjudicateSuccessSurvivesALaterQueueLoadFailure(t *testing.T) {
	dm := setupResolveProposeTest(t)
	rowID := seedSharedQueueRow(t, dm, "l-a", "l-b")
	dropSharedQueueEvidenceColumn(t, dm)

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				fmt.Sprintf("--adjudicate=%d:read both memories and they conflict", rowID),
			})
		})
	})

	require.Equal(t, 1, code, "a queue that cannot be loaded is a failure")

	env := decodeSingleEnvelope(t, out)
	require.NotEmpty(t, envelopeErrorText(t, env),
		"the later stage's failure must be reported")

	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 1, "the adjudication must still be reported")
	require.Equal(t, float64(rowID), entries[0]["queue_id"])
	require.Equal(t, "adjudicated", entries[0]["status"],
		"a later stage failing does not un-adjudicate an earlier write")
	require.Nil(t, entries[0]["error"])

	// The write was durable, not merely reported.
	require.Contains(t, queueRowDetectedBy(t, dm, rowID), "adjudicated:")
}

// A successful adjudication survives the EMPTY-QUEUE completion.
//
// The trigger resolves the row as soon as it is adjudicated, so the queue
// is empty by the time it is loaded. The empty-queue envelope is still a
// POST-adjudication envelope, and it must carry the adjudication.
func TestResolveContradictions_JSONAdjudicateSuccessSurvivesTheEmptyQueueCompletion(t *testing.T) {
	dm := setupResolveProposeTest(t)
	installResolveOnAdjudicationTrigger(t, dm)
	rowID := seedSharedQueueRow(t, dm, "e-a", "e-b")

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				fmt.Sprintf("--adjudicate=%d:read both memories and they conflict", rowID),
			})
		})
	})

	require.Equal(t, 0, code)

	env := decodeSingleEnvelope(t, out)
	require.Equal(t, 0.0, envelopeNumber(t, env, "queue_size"),
		"the row was resolved by the trigger, so the queue is empty")

	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 1, "the adjudication must be reported by the empty-queue envelope")
	require.Equal(t, float64(rowID), entries[0]["queue_id"])
	require.Equal(t, "adjudicated", entries[0]["status"])
	require.Nil(t, entries[0]["error"])
}

// The report agrees with the durable queue state.
//
// An adjudication is a WRITE, so "adjudicated" in the report is a claim
// about stored state. It has to be true: the row must really carry the
// stamp, and it must still be an unresolved row (an adjudication judges a
// pair; it does not settle it).
func TestResolveContradictions_JSONAdjudicationReportMatchesTheDurableQueueState(t *testing.T) {
	dm := setupResolveProposeTest(t)
	rowID := seedSharedQueueRow(t, dm, "d-a", "d-b")

	// Before: the row is unresolved and carries no judgement.
	require.NotContains(t, queueRowDetectedBy(t, dm, rowID), "adjudicated:",
		"a fresh row carries no adjudication")
	require.Equal(t, 1, durableUnresolvedQueueCount(t, dm))

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				fmt.Sprintf("--adjudicate=%d:read both memories and they conflict", rowID),
			})
		})
	})
	require.Equal(t, 0, code)

	// After: the row carries the stamp, and it is still unresolved.
	require.Contains(t, queueRowDetectedBy(t, dm, rowID), "adjudicated:",
		"the stamp must be durable, not merely reported")
	require.Equal(t, 1, durableUnresolvedQueueCount(t, dm),
		"an adjudication judges a pair; it does not resolve it")

	// And the report says exactly that.
	env := decodeSingleEnvelope(t, out)
	entries := adjudicationEntries(t, env)
	require.Len(t, entries, 1)
	require.Equal(t, "adjudicated", entries[0]["status"])
	require.Equal(t, float64(rowID), entries[0]["queue_id"])

	// A row that was NOT adjudicated is not reported as adjudicated: the
	// report is about the requests the operator made, not about the queue.
	require.Equal(t, 1.0, envelopeNumber(t, env, "queue_total_unresolved"),
		"the durable count and the reported count must agree")
}

// The JSON path emits ONE document and nothing else.
//
// A consumer that asked for JSON must be able to parse stdout as a single
// object. The human path's per-row confirmation line is useful to a
// watching operator and is noise to a parser, so it belongs to the human
// path only.
func TestResolveContradictions_JSONAdjudicateEmitsOneDocumentAndNothingElse(t *testing.T) {
	dm := setupResolveProposeTest(t)
	rowID := seedSharedQueueRow(t, dm, "o-a", "o-b")

	var out string
	var errOut string
	var code int
	errOut = captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{
				"--json",
				fmt.Sprintf("--adjudicate=%d:read both memories and they conflict", rowID),
			})
		})
	})
	require.Equal(t, 0, code)

	// decodeSingleEnvelope fails if stdout holds anything other than
	// exactly one JSON object.
	env := decodeSingleEnvelope(t, out)
	require.Equal(t, "adjudicated", adjudicationEntries(t, env)[0]["status"])

	trimmed := strings.TrimSpace(out)
	require.True(t, strings.HasPrefix(trimmed, "{"),
		"stdout must start with the JSON document, got:\n%s", out)
	require.True(t, strings.HasSuffix(trimmed, "}"),
		"stdout must end with the JSON document, got:\n%s", out)
	require.NotContains(t, out, "Adjudicated queue row",
		"the human confirmation line must not be printed in JSON mode")
	require.Empty(t, errOut,
		"a successful JSON invocation has nothing to say on stderr, got:\n%s", errOut)

	// The human path keeps its confirmation line.
	var humanOut string
	captureUserErrorOutput(t, func() {
		humanOut = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{
				fmt.Sprintf("--adjudicate=%d:read both memories and they conflict", rowID),
			}))
		})
	})
	require.Contains(t, humanOut, fmt.Sprintf("Adjudicated queue row %d", rowID),
		"the human path still confirms each write as it happens")
}

// The PRE-adjudication envelopes still carry an empty adjudication list.
//
// The refusal and the help are answered before any adjudication runs, so
// there is nothing to report. They must say so with an empty list rather
// than with a null, and they must not be affected by the correction that
// carries real outcomes into the post-adjudication envelopes.
func TestResolveContradictions_PreAdjudicationEnvelopesStillCarryAnEmptyAdjudicationList(t *testing.T) {
	setupResolveProposeTest(t)

	var helpOut string
	captureUserErrorOutput(t, func() {
		helpOut = captureStdout(t, func() {
			require.Equal(t, 0, handleOpsResolveContradictions([]string{"--json", "help"}))
		})
	})
	helpEnv := decodeSingleEnvelope(t, helpOut)
	helpEntries := adjudicationEntries(t, helpEnv)
	require.Len(t, helpEntries, 0,
		"a help request adjudicates nothing, so it reports nothing")
	require.NotNil(t, helpEnv["adjudications"],
		"the key is present and empty, not absent and not null")

	// A refused invocation is refused BEFORE any work, so it too carries
	// an empty list.
	var refusalOut string
	captureUserErrorOutput(t, func() {
		refusalOut = captureStdout(t, func() {
			require.Equal(t, 1, handleOpsResolveContradictions([]string{
				"--json", "--propose", "--adjudicate=1:cannot combine these",
			}))
		})
	})
	refusalEnv := decodeSingleEnvelope(t, refusalOut)
	refusalEntries := adjudicationEntries(t, refusalEnv)
	require.Len(t, refusalEntries, 0,
		"a refused invocation adjudicates nothing, so it reports nothing")
	require.NotNil(t, refusalEnv["adjudications"],
		"the key is present and empty, not absent and not null")
}
