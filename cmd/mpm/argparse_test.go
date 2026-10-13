// argparse_test.go — regression net for silently discarded flags.
//
// The bug these pin: Go's flag.Parse goes positional-only at the first
// non-dash argument, so `mpm recall "query" --semantic` ran as a plain lexical
// recall. It exited 0. Nothing in the output said the flag had been dropped.
//
// That is worse than an error, because the command produces a confident-looking
// answer to a question it was never asked. It produced a wrong conclusion in a
// real experiment: a benchmark compared `--semantic` against lexical recall,
// measured different reinforcement behaviour, and attributed it to the
// embedding path when the flag had never been applied at all.
//
// The flag set below mirrors handleRecall's declarations. Keeping the two in
// step is deliberate: TestHandleRecall_AllDeclaredFlagsAreCovered fails if a
// flag is added to one and not the other, because an uncovered flag would
// silently keep the defect.

package main

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// recallFlagNames lists every flag handleRecall declares. A new flag must be
// added here to receive position coverage.
var recallFlagNames = []string{
	"since", "until", "limit", "stale-days", "json", "collection",
	"token-budget", "weight-below", "before", "semantic", "vector-weight",
	"as-of", "why", "projected",
}

// recallFlagset is a parsed recall invocation, with every flag exposed so two
// parses can be compared field by field without depending on which flag was
// under test.
type recallFlagset struct {
	fs *flag.FlagSet

	limit       *int
	staleDays   *int
	tokenBudget *int
	weightBelow *int
	since       *string
	until       *string
	collection  *string
	before      *string
	asOf        *string
	vectorWt    *float64
	json        *bool
	semantic    *bool
	why         *bool
	projected   *bool
}

// newRecallTestFlagSet builds the same FlagSet handleRecall does. Defaults
// match production so that a test comparing two parses sees a difference only
// when a flag was actually dropped.
func newRecallTestFlagSet(t *testing.T) *recallFlagset {
	t.Helper()
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	r := &recallFlagset{fs: fs}
	r.since = fs.String("since", "", "")
	r.until = fs.String("until", "", "")
	r.limit = fs.Int("limit", 15, "")
	r.staleDays = fs.Int("stale-days", 14, "")
	r.json = fs.Bool("json", false, "")
	r.collection = fs.String("collection", "memories", "")
	r.tokenBudget = fs.Int("token-budget", 0, "")
	r.weightBelow = fs.Int("weight-below", 0, "")
	r.before = fs.String("before", "", "")
	r.semantic = fs.Bool("semantic", false, "")
	r.vectorWt = fs.Float64("vector-weight", 0.5, "")
	r.asOf = fs.String("as-of", "", "")
	r.why = fs.Bool("why", false, "")
	r.projected = fs.Bool("projected", false, "")
	return r
}

// parse runs parseArgsInterleaved and fails the test on error.
func (r *recallFlagset) parse(t *testing.T, args ...string) *recallFlagset {
	t.Helper()
	require.NoError(t, parseArgsInterleaved(r.fs, args),
		"every flag must parse in any position")
	return r
}

// query is the first positional.
func (r *recallFlagset) query() string { return r.fs.Arg(0) }

// ── the two orders, for every flag ─────────────────────────────────

// The headline test. For each flag handleRecall accepts, flags-first and
// flags-last must produce IDENTICAL state and an identical query. Any
// divergence is a silently discarded flag.
func TestParseArgsInterleaved_FlagOrderDoesNotMatter(t *testing.T) {
	for _, name := range recallFlagNames {
		name := name
		t.Run(name, func(t *testing.T) {
			f := newRecallTestFlagSet(t).fs.Lookup(name)
			require.NotNil(t, f, "%s must be declared", name)

			_, isBool := f.Value.(interface{ IsBoolFlag() bool })
			isBool = isBool && f.Value.(interface{ IsBoolFlag() bool }).IsBoolFlag()

			// A value distinguishable from the default, of a type the flag
			// can actually accept.
			val := "value"
			switch {
			case isBool:
				// No separate value.
			case name == "limit" || name == "stale-days":
				val = "7"
			case name == "collection":
				val = "lessons"
			case name == "vector-weight":
				val = "0.9"
			case name == "as-of":
				val = "2026-01-01"
			case name == "before" || name == "since" || name == "until":
				val = "2026-01-01"
			case name == "token-budget":
				val = "500"
			case name == "weight-below":
				val = "3"
			}

			tokens := []string{"--" + name, val}
			if isBool {
				tokens = []string{"--" + name}
			}

			// Flags-first is the canonical order flag.Parse was always given.
			before := newRecallTestFlagSet(t).parse(t, tokens...)
			// Flags-last is the defect: the whole flag/value pair sits after
			// the positional, which is how a caller naturally types it.
			after := newRecallTestFlagSet(t).parse(t,
				append([]string{"a query"}, tokens...)...)

			require.Equal(t, *before.limit, *after.limit,
				"--limit differs by position: the flag was discarded in one order")
			require.Equal(t, *before.since, *after.since, "--since differs by position")
			require.Equal(t, *before.until, *after.until, "--until differs by position")
			require.Equal(t, *before.staleDays, *after.staleDays, "--stale-days differs by position")
			require.Equal(t, *before.tokenBudget, *after.tokenBudget, "--token-budget differs by position")
			require.Equal(t, *before.weightBelow, *after.weightBelow, "--weight-below differs by position")
			require.Equal(t, *before.before, *after.before, "--before differs by position")
			require.Equal(t, *before.collection, *after.collection, "--collection differs by position")
			require.Equal(t, *before.asOf, *after.asOf, "--as-of differs by position")
			require.Equal(t, *before.vectorWt, *after.vectorWt, "--vector-weight differs by position")
			require.Equal(t, *before.json, *after.json, "--json differs by position")
			require.Equal(t, *before.semantic, *after.semantic,
				"--semantic differs by position: this is the original defect")
			require.Equal(t, *before.why, *after.why, "--why differs by position")
			require.Equal(t, *before.projected, *after.projected, "--projected differs by position")

			require.Equal(t, "a query", after.query(),
				"the query must be identical; flag position must not change what is searched for")
		})
	}
}

// ── the specific cases called out ──────────────────────────────────

func TestParseArgsInterleaved_SemanticBothOrders(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantQuery string
	}{
		{"flag first", []string{"--semantic", "some query"}, "some query"},
		{"flag last", []string{"some query", "--semantic"}, "some query"},
		{"multi-word query, flag last", []string{"some", "query", "--semantic"}, "some query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecallTestFlagSet(t).parse(t, tc.args...)
			require.True(t, *r.semantic,
				"--semantic must be honoured in every position")
			require.Equal(t, tc.wantQuery, strings.Join(r.fs.Args(), " "))
		})
	}
}

func TestParseArgsInterleaved_LimitAndWhyBothOrders(t *testing.T) {
	for _, args := range [][]string{
		{"--limit", "3", "--why", "query"},
		{"query", "--limit", "3", "--why"},
		{"--why", "query", "--limit", "3"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := newRecallTestFlagSet(t).parse(t, args...)
			require.Equal(t, 3, *r.limit, "--limit must be honoured")
			require.True(t, *r.why, "--why must be honoured")
			require.Equal(t, "query", r.query())
		})
	}
}

func TestParseArgsInterleaved_EqualsForm(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t, "--limit=7", "query", "--as-of=2026-01-01")
	require.Equal(t, 7, *r.limit)
	require.Equal(t, "2026-01-01", *r.asOf)
	require.Equal(t, "query", r.query())
}

func TestParseArgsInterleaved_ShortDashFormStillWorks(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t, "query", "-json")
	require.True(t, *r.json,
		"the single-dash form must keep working; some callers use it")
}

func TestParseArgsInterleaved_ManyFlagsInterleaved(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t,
		"--semantic", "deploy", "--limit", "2", "pipeline", "--why",
		"--collection", "lessons", "runs on", "--json", "tuesday")
	require.True(t, *r.semantic)
	require.Equal(t, 2, *r.limit)
	require.True(t, *r.why)
	require.Equal(t, "lessons", *r.collection)
	require.True(t, *r.json)
	require.Equal(t, "deploy pipeline runs on tuesday", strings.Join(r.fs.Args(), " "),
		"positional words must rejoin in their original order, in order")
}

// ── rejection, not silence ────────────────────────────────────────

func TestParseArgsInterleaved_UnknownFlagIsRejected(t *testing.T) {
	for _, args := range [][]string{
		{"query", "--not-a-flag"}, // the dangerous position
		{"--not-a-flag", "query"},
		{"query", "--not-a-flag", "value"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := newRecallTestFlagSet(t)
			err := parseArgsInterleaved(r.fs, args)
			require.Error(t, err,
				"an unknown flag must be an error, never a silent no-op")
			require.Contains(t, err.Error(), "not-a-flag",
				"the error must name the flag so the user knows what to remove")
		})
	}
}

func TestParseArgsInterleaved_MissingValueIsRejected(t *testing.T) {
	r := newRecallTestFlagSet(t)
	err := parseArgsInterleaved(r.fs, []string{"query", "--limit"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "needs an argument")
}

// ── things that must NOT be mistaken for flags ────────────────────

func TestParseArgsInterleaved_QueryMentioningAFlag(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t, "how do I use --limit in mpm")
	require.Equal(t, "how do I use --limit in mpm", r.query(),
		"a query mentioning a flag name must not be reinterpreted as one")
}

func TestParseArgsInterleaved_DoubleDashTerminator(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t, "--", "--semantic", "--why")
	require.Equal(t, 2, r.fs.NArg(), "everything after -- is positional")
	require.False(t, *r.semantic,
		"a flag after -- is a literal argument, not a flag")
}

func TestParseArgsInterleaved_NegativeNumbersAreValues(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t, "-5", "-3.5", "query")
	require.Equal(t, 3, r.fs.NArg())
	require.Equal(t, "-5", r.query())
}

func TestParseArgsInterleaved_FlagValueBeginningWithADash(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t, "query", "--as-of", "-1")
	require.Equal(t, "-1", *r.asOf,
		"a value beginning with a dash must be taken whole")
}

func TestParseArgsInterleaved_BooleanDoesNotSwallowTheNextToken(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t, "--json", "query")
	require.True(t, *r.json)
	require.Equal(t, "query", r.query(),
		"--json must not consume the query as its value")
}

func TestParseArgsInterleaved_RepeatedFlagLastWins(t *testing.T) {
	// Matches flag.Parse's own behaviour, so reordering changes nothing for a
	// caller who already wrote the canonical order.
	r := newRecallTestFlagSet(t).parse(t, "--limit", "5", "query", "--limit", "9")
	require.Equal(t, 9, *r.limit)
}

func TestParseArgsInterleaved_EmptyArgs(t *testing.T) {
	r := newRecallTestFlagSet(t).parse(t)
	require.Equal(t, 0, r.fs.NArg())
}

// ── coverage guard ────────────────────────────────────────────────

// If a flag is added to handleRecall and not to recallFlagNames, this fails.
// Without it the flag-order table test above silently stops covering the new
// flag — the same drift that produced the original defect.
func TestHandleRecall_AllDeclaredFlagsAreCovered(t *testing.T) {
	fs := newRecallTestFlagSet(t).fs
	declared := make([]string, 0, len(recallFlagNames))
	fs.VisitAll(func(f *flag.Flag) { declared = append(declared, f.Name) })
	require.ElementsMatch(t, recallFlagNames, declared,
		"recallFlagNames must match the FlagSet handleRecall builds; "+
			"add the new flag so it gets position coverage")
}
