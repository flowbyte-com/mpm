// recall_flagorder_test.go — end-to-end proof that handleRecall honours flags
// written after the query, and rejects the ones it cannot honour.
//
// argparse_test.go covers the parser directly and exhaustively. This file
// covers the wiring: that handleRecall actually routes through it, that the
// semantic path is reached in either order, and that a bad flag reaches the
// user as an error rather than a confident wrong answer.
//
// The signal used throughout is `search_mode` in the --json output. The
// semantic path emits it; the lexical path does not. That asymmetry is
// deliberate and is what made the original bug diagnosable — the renderer
// printed "Hybrid Recall" in one place while the JSON showed the run was
// lexical.

package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/stretchr/testify/require"
)

// recallWorkspace points the package-level DB singleton at a throwaway
// workspace so these tests never touch the live store.
func recallWorkspace(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	savedDM, savedErr := dbManager, dbManagerInitErr
	resetGlobalDB(t, ws)
	t.Cleanup(func() {
		if dbManager != nil {
			_ = dbManager.Close()
		}
		dbManager, dbManagerInitErr = savedDM, savedErr
		dbManagerOnce = sync.Once{}
	})

	dm, err := mpminternal.NewDatabaseManager(ws)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Close() })
	return dm
}

// seedRecallWords inserts one memory per term, all matching a common token, so
// a recall for that token has several candidates and --limit is observable.
func seedRecallWords(t *testing.T, dm *mpminternal.DatabaseManager, terms ...string) {
	t.Helper()
	for i, term := range terms {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, weight, created_at)
			VALUES (?, 'memories', ?, '{"weight":1}', 1, CAST(strftime('%s','now') AS INTEGER))
		`, "flagorder-"+itoaForD42(i), "flagordertoken "+term)
		require.NoError(t, err)
	}
}

// captureRecall runs handleRecall with stdout and the usererror writer
// captured, returning the exit code and both streams.
//
// stderr is captured rather than inherited because user-facing errors go
// there; asserting on inherited stderr is unreliable under `go test`, which
// interleaves the package's own diagnostics onto the same stream.
func captureRecall(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	var errBuf strings.Builder
	prevErrWriter := usererror.GetWriter()
	usererror.SetWriter(&errBuf)
	t.Cleanup(func() { usererror.SetWriter(prevErrWriter) })

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	code = handleRecall(args)

	_ = w.Close()
	os.Stdout = old
	stdout = <-done
	_ = r.Close()
	return code, stdout, errBuf.String()
}

// searchMode extracts search_mode from --json output. Absent means lexical.
func searchMode(t *testing.T, out string) string {
	t.Helper()
	var payload struct {
		Query      string `json:"query"`
		SearchMode string `json:"search_mode"`
		Memories   []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"memories"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &payload),
		"recall --json must emit parseable JSON; got: %s", out)
	return payload.SearchMode
}

func countReturned(t *testing.T, out string) int {
	t.Helper()
	var payload struct {
		Memories []json.RawMessage `json:"memories"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &payload))
	return len(payload.Memories)
}

// ── the headline defect ────────────────────────────────────────────

// `mpm recall "query" --semantic` must take the semantic path. Before the
// fix it exited 0, printed ordinary lexical results, and said nothing about
// the flag it had dropped.
//
// Detection deliberately does not require a configured embedding provider.
// With none, BOTH orders reach the semantic path and fail with the same
// "No embedding provider" error — and that failure is the proof, because the
// lexical path would have succeeded and returned rows. The test therefore also
// asserts the two orders are byte-identical, which is the invariant that was
// actually broken: one order was hybrid, the other was lexical, and nothing
// distinguished them.
func TestHandleRecall_SemanticAfterQueryTakesSemanticPath(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha", "beta", "gamma")

	beforeCode, beforeOut, beforeErr := captureRecall(t,
		"recall", "--semantic", "flagordertoken", "--json")
	afterCode, afterOut, afterErr := captureRecall(t,
		"recall", "flagordertoken", "--semantic", "--json")

	require.Equal(t, beforeCode, afterCode,
		"the two flag orders disagreed on exit code: one took a different path")
	require.Equal(t, beforeOut, afterOut,
		"the two flag orders produced different output: the flag was discarded in one of them")
	require.Equal(t, beforeErr, afterErr)

	// Whatever the outcome — hybrid results, or the no-provider error — the
	// run must not look lexical. Lexical output is JSON without search_mode.
	if beforeCode == 0 {
		require.Equal(t, "hybrid", searchMode(t, beforeOut),
			"the run succeeded but took the LEXICAL path: --semantic was discarded")
		require.Positive(t, countReturned(t, beforeOut))
	} else {
		require.Contains(t, strings.ToLower(beforeErr), "embedding provider",
			"failure must come from the semantic path, proving the flag was honoured")
	}
}

// ── --limit ────────────────────────────────────────────────────────

func TestHandleRecall_LimitAfterQueryIsHonoured(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha", "beta", "gamma", "delta", "epsilon")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"before query", []string{"recall", "--limit", "2", "flagordertoken"}},
		{"after query", []string{"recall", "flagordertoken", "--limit", "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, _ := captureRecall(t, append(tc.args, "--json")...)
			require.Equal(t, 0, code, "recall must succeed: %s", out)
			require.Equal(t, 2, countReturned(t, out),
				"--limit must cap the result set in either position")
		})
	}
}

// A zero or negative limit is not "no results" or "all results" — it is a
// mistake, and SQLite's two different interpretations of it would otherwise
// be reported as though they were answers.
func TestHandleRecall_NonPositiveLimitIsRejected(t *testing.T) {
	recallWorkspace(t)

	for _, v := range []string{"0", "-5"} {
		t.Run(v, func(t *testing.T) {
			for _, args := range [][]string{
				{"recall", "--limit", v, "query"},
				{"recall", "query", "--limit", v},
			} {
				code, out, _ := captureRecall(t, append(args, "--json")...)
				require.Equal(t, 1, code,
					"--limit %s must be rejected, not silently reinterpreted", v)
				require.NotContains(t, out, `"memories"`,
					"a rejected invocation must not emit result JSON")
			}
		})
	}
}

// ── --why ──────────────────────────────────────────────────────────

func TestHandleRecall_WhyAfterQueryIsHonoured(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha")

	_, withWhy, _ := captureRecall(t, "recall", "flagordertoken", "--why", "--json")
	require.Contains(t, withWhy, "rationale",
		"--why must produce per-result provenance when written after the query")
}

// ── rejection ──────────────────────────────────────────────────────

func TestHandleRecall_UnsupportedFlagIsRejectedInBothPositions(t *testing.T) {
	recallWorkspace(t)

	for _, args := range [][]string{
		{"recall", "--no-such-flag", "query"},
		{"recall", "query", "--no-such-flag"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, _ := captureRecall(t, append(args, "--json")...)
			require.Equal(t, 1, code,
				"an unsupported flag must exit non-zero in any position; "+
					"silently discarding it produces a confident wrong answer")
		})
	}
}

// The error must name the offending flag, or the user has no way to know what
// to remove.
func TestHandleRecall_UnsupportedFlagErrorNamesTheFlag(t *testing.T) {
	recallWorkspace(t)
	_, _, errOut := captureRecall(t, "recall", "query", "--totally-bogus")
	require.Contains(t, strings.ToLower(errOut), "totally-bogus",
		"the user must be told WHICH flag to remove")
}

// ── conflicting options ────────────────────────────────────────────

// An inverted date range is self-contradictory. Reaching SQL it returns zero
// rows, which reads as "nothing matches" rather than "these flags disagree".
func TestHandleRecall_InvertedDateRangeIsRejected(t *testing.T) {
	recallWorkspace(t)

	for _, args := range [][]string{
		{"recall", "--since", "2026-06-01", "--until", "2026-01-01", "query"},
		{"recall", "query", "--since", "2026-06-01", "--until", "2026-01-01"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, errOut := captureRecall(t, append(args, "--json")...)
			require.Equal(t, 1, code,
				"an empty date range must be rejected, not reported as no matches")
			require.Contains(t, errOut, "--since",
				"the error must name the flags that disagree")
		})
	}
}

// A valid range in either position must still work — the rejection above must
// not become a blanket refusal of --since/--until.
func TestHandleRecall_ValidDateRangeIsAccepted(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha")

	for _, args := range [][]string{
		{"recall", "--since", "2020-01-01", "--until", "2030-01-01", "flagordertoken"},
		{"recall", "flagordertoken", "--since", "2020-01-01", "--until", "2030-01-01"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// Only acceptance is asserted here. Whether the date filter itself
			// matches rows is a separate concern with its own coverage; this
			// test exists to prove the rejection above did not become a
			// blanket refusal of --since/--until.
			code, _, errOut := captureRecall(t, append(args, "--json")...)
			require.Equal(t, 0, code,
				"a valid range must be accepted, not rejected: %s", errOut)
		})
	}
}

// ── positionals ────────────────────────────────────────────────────

// Taking only the first positional silently discarded the rest of the query.
// The tour demo relies on this path (`strings.Fields("first memory")`), so the
// fix is to rejoin rather than to reject.
func TestHandleRecall_UnquotedMultiWordQueryIsJoinedNotTruncated(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha")

	_, out, _ := captureRecall(t, "recall", "flagordertoken", "alpha", "--json")
	require.Contains(t, out, "flagordertoken alpha",
		"every positional must reach the query, not just the first")
}
