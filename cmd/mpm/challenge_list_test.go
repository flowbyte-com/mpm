// challenge_list_test.go — issue #20 regression coverage for
// `mpm challenge list` and the read-only listing primitive.
//
// Issue #20: the 41 memories left in `status='challenged'` by the
// retired auto-challenge defect had no operator-facing discovery
// surface. This file pins:
//   - the empty-DB path (count=0, status=0, no panic),
//   - the seeded-DB path (correct row count, the right columns,
//     the right per-row shape including prior/current weight
//     and the restoreable flag),
//   - the JSON envelope (canonical shape; restore_weight_change
//     is correct in the floor and clamped cases),
//   - the read-only contract (a full-table SHA snapshot before
//     and after a `mpm challenge list` call must be byte-equal),
//   - the per-row restore arithmetic for the floor-no-op and
//     clamped-from-above cases (the two states the operator is
//     being asked to distinguish when they decide per row).
//
// Every test runs against an isolated in-memory database
// (internal.NewTestDM via newTestDMForCmd). The fixture never
// touches the production workspace DB — see the safety note in
// call_evidence_test.go:102 for the rationale.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue20ListSeed plants a memory, challenges it through the
// production handler path, and returns the id. The weight the
// caller passes is the prior weight; the challenge writer
// reduces the row's weight by 2 (default slash amount for
// `mpm challenge`) and clamps at 1.0.
func issue20ListSeed(t *testing.T, dm *mpminternal.DatabaseManager, content string, priorWeight float64) string {
	t.Helper()
	id, err := dm.SaveMemory("memories", content, "", []string{"issue20"}, nil, nil, false, priorWeight)
	require.NoError(t, err)
	_, err = runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{
			"memoryId": id,
			"evidence": "issue20 test fixture",
		},
	})
	require.NoError(t, err)
	return id
}

// TestIssue20_ListEmpty — fresh DB: count is 0, exit 0, no panic,
// and the human-readable "No challenged memories" line is emitted.
func TestIssue20_ListEmpty(t *testing.T) {
	dm := newTestDMForCmd(t)

	var code int
	out := captureStdout(t, func() { code = runChallengeList(dm, false, challengeListLimitDefault) })

	assert.Equal(t, 0, code, "empty DB must exit 0")
	assert.Contains(t, out, "Challenge list")
	assert.Contains(t, out, "No challenged memories.")
}

// TestIssue20_ListSeededRows — seed three memories, challenge two,
// run `mpm challenge list` and assert the count and the visible
// columns.
func TestIssue20_ListSeededRows(t *testing.T) {
	dm := newTestDMForCmd(t)
	a := issue20ListSeed(t, dm, "issue20 row A (clamped)", 2.0) // current=1.0, prior=2.0 → restore would move
	b := issue20ListSeed(t, dm, "issue20 row B (floor)", 1.0)   // current=1.0, prior=1.0 → restore is no-op
	// Plant a third memory that is NOT challenged; it must not
	// appear in the list.
	_, err := dm.SaveMemory("memories", "issue20 untouched", "", []string{"issue20"}, nil, nil, false, 5.0)
	require.NoError(t, err)

	var code int
	out := captureStdout(t, func() { code = runChallengeList(dm, false, challengeListLimitDefault) })

	assert.Equal(t, 0, code, "list must exit 0 on the healthy path")
	assert.Contains(t, out, "2 memories in status='challenged'")
	assert.Contains(t, out, a, "row A must appear in the table")
	assert.Contains(t, out, b, "row B must appear in the table")
	assert.NotContains(t, out, "issue20 untouched",
		"a non-challenged memory must not appear in the list")
	// The weight column uses a Unicode arrow (←), not the
	// ASCII fallback. Pin the two expected renderings so the
	// format doesn't silently regress.
	assert.Contains(t, out, "1.0←2.0", "clamped row must show current←prior")
	assert.Contains(t, out, "1.0←1.0", "floor row must show current=prior")
}

// TestIssue20_ListJSONEnvelope — same fixture as above, but the
// JSON path: the envelope has the canonical shape, the count
// matches, and restore_weight_change is correct for the floor
// and clamped cases.
func TestIssue20_ListJSONEnvelope(t *testing.T) {
	dm := newTestDMForCmd(t)
	a := issue20ListSeed(t, dm, "issue20 json clamped", 2.0) // prior 2.0
	b := issue20ListSeed(t, dm, "issue20 json floor", 1.0)   // prior 1.0

	var code int
	out := captureStdout(t, func() { code = runChallengeList(dm, true, challengeListLimitDefault) })
	assert.Equal(t, 0, code, "JSON list must exit 0 on the healthy path")

	// The envelope is rendered by respond() with a trailing
	// newline; the JSON itself is everything up to the first
	// newline.
	raw := strings.TrimRight(out, "\n")
	var env struct {
		Success    bool                     `json:"success"`
		Count      int                      `json:"count"`
		Limit      int                      `json:"limit"`
		Challenged []map[string]interface{} `json:"challenged"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &env), "envelope must parse: %q", raw)

	assert.True(t, env.Success)
	assert.Equal(t, 2, env.Count, "two challenged rows expected")
	assert.Equal(t, challengeListLimitDefault, env.Limit, "limit must be reported back to the script")
	require.Len(t, env.Challenged, 2, "challenged array length must match count")

	// Find each row by id; ordering is challenged_at ASC, id ASC
	// so the order is the insertion order in this fixture.
	byID := make(map[string]map[string]interface{}, 2)
	for _, row := range env.Challenged {
		byID[row["id"].(string)] = row
	}
	require.Contains(t, byID, a)
	require.Contains(t, byID, b)

	// Clamped row: restore_weight_change = prior - current = 1.0.
	clamped := byID[a]
	assert.EqualValues(t, 1.0, clamped["restore_weight_change"],
		"clamped row must report restore_weight_change=1.0 (2.0 prior - 1.0 current)")
	assert.EqualValues(t, 1.0, clamped["current_weight"])
	assert.EqualValues(t, 2.0, clamped["prior_weight"])
	assert.Equal(t, true, clamped["restoreable"])

	// Floor row: restore_weight_change = 0.0 (no-op on weight).
	floor := byID[b]
	assert.EqualValues(t, 0.0, floor["restore_weight_change"],
		"floor row must report restore_weight_change=0.0")
	assert.EqualValues(t, 1.0, floor["current_weight"])
	assert.EqualValues(t, 1.0, floor["prior_weight"])
	assert.Equal(t, true, floor["restoreable"])
}

// TestIssue20_ListDoesNotMutate — load-bearing read-only test.
//
// Take a full-table SHA snapshot of every table `mpm challenge
// list` can read (memories, evidence, retrieval_metadata, etc.)
// before and after running the list. The two snapshots must be
// byte-equal. This is the test that proves "listing/diagnostics
// do not mutate database content" — if a future refactor of
// ListChallengedMemories introduced a write (a stray UPDATE, a
// row-level trigger, an accidental fts5 reindex on read), this
// test would fail.
func TestIssue20_ListDoesNotMutate(t *testing.T) {
	dm := newTestDMForCmd(t)
	issue20ListSeed(t, dm, "issue20 no-mutate fixture", 5.0)

	before := snapshotUserTables(t, dm)

	var code int
	_ = captureStdout(t, func() { code = runChallengeList(dm, false, challengeListLimitDefault) })
	assert.Equal(t, 0, code)

	after := snapshotUserTables(t, dm)
	if before != after {
		t.Fatalf("mpm challenge list mutated the database.\nbefore sha: %s\nafter  sha: %s", before, after)
	}
}

// TestIssue20_ListRowAtFloor — a row whose prior weight is 1.0
// must report restore_weight_change=0.0 and the table column
// must read "1.0←1.0". This is the headline F7.1 case for 27
// of the 41 live rows: restore clears the flag, weight does
// not move.
func TestIssue20_ListRowAtFloor(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := issue20ListSeed(t, dm, "issue20 floor row", 1.0)

	rows, err := dm.ListChallengedMemories()
	require.NoError(t, err)
	require.Len(t, rows, 1, "exactly one challenged row expected")

	r := rows[0]
	assert.Equal(t, id, r.ID)
	assert.Equal(t, 1.0, r.CurrentWeight, "current weight is the floor (clamp)")
	assert.Equal(t, 1.0, r.PriorWeight, "prior weight is what we seeded")
	assert.True(t, r.PriorWeightPresent)
	assert.Equal(t, 0.0, r.RestoreWeightChange,
		"a row already at the floor must report a 0.0 restore delta")
	assert.True(t, r.Restoreable, "the row is restorable (only the flag clears)")
	assert.Equal(t, "1.0←1.0", weightArrow(r),
		"the table column must read current=prior")
}

// TestIssue20_ListRowClampedFromTwo — a row whose prior weight
// is 2.0 must report restore_weight_change=1.0 and the table
// column must read "1.0←2.0". This is the headline case for
// the 4 rows of the 41 that the clamp pulled down from 2.0:
// restore moves them back to 2.0.
func TestIssue20_ListRowClampedFromTwo(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := issue20ListSeed(t, dm, "issue20 clamped row", 2.0)

	rows, err := dm.ListChallengedMemories()
	require.NoError(t, err)
	require.Len(t, rows, 1)

	r := rows[0]
	assert.Equal(t, id, r.ID)
	assert.Equal(t, 1.0, r.CurrentWeight, "the clamp brings current weight to the floor")
	assert.Equal(t, 2.0, r.PriorWeight, "prior weight is what we seeded")
	assert.True(t, r.PriorWeightPresent)
	assert.Equal(t, 1.0, r.RestoreWeightChange,
		"a row clamped from 2.0 to 1.0 must report a 1.0 restore delta")
	assert.True(t, r.Restoreable)
	assert.Equal(t, "1.0←2.0", weightArrow(r),
		"the table column must read current←prior with the actual values")
}

// TestIssue20_ListUnknownArg — the handler refuses unknown
// positional args rather than silently ignoring them. Same
// contract as handleDoctor's strict-flag handling.
//
// The strict-arg loop lives in handleChallengeList (the
// flag-handling wrapper) and runs BEFORE getDB() is consulted,
// so this test exercises the handler directly with no
// database needed. The error message goes to stderr (via
// respond's errMsg arg), so the test uses captureBoth to
// see both streams in one call.
//
// HAL defect 1 (post #20): the handler now follows the
// subcommand convention — args[0] is the literal "list"
// token (the router strips only the leading "challenge" from
// the operator's command line), and the operator's flags
// and positionals start at args[1]. This test exercises the
// post-convention strict-rejection path: args[0]="list",
// args[1]="surprise-positional" — an unknown positional
// AFTER the subcommand, which is what the operator would
// actually type.
func TestIssue20_ListUnknownArg(t *testing.T) {
	var code int
	captured := captureBoth(t, func() {
		code = handleChallengeList([]string{"list", "surprise-positional"})
	})

	assert.NotEqual(t, 0, code, "unknown arg must exit non-zero")
	assert.Contains(t, captured, "unknown argument",
		"error must mention the unknown argument")
	assert.Contains(t, captured, "surprise-positional",
		"error must echo the offending value")
}

// TestIssue20_ListWrongSubcommand — the handler refuses
// any first-arg that is not the literal "list" subcommand.
// This pins the router-convention contract: a caller that
// reaches the handler through a non-router path (e.g., a
// future refactor that invokes handlers directly) cannot
// pass arbitrary first-arg tokens.
func TestIssue20_ListWrongSubcommand(t *testing.T) {
	var code int
	captured := captureBoth(t, func() {
		code = handleChallengeList([]string{"restore"})
	})

	assert.NotEqual(t, 0, code, "wrong subcommand must exit non-zero")
	assert.Contains(t, captured, "unexpected subcommand",
		"error must mention the unexpected subcommand")
	assert.Contains(t, captured, "restore",
		"error must echo the offending subcommand")
}

// snapshotUserTables returns a deterministic hash of the
// user-visible data in the disposable test DB. Used by the
// "listing does not mutate" test. Walks the four tables the
// read-only listing and reporting surfaces can read; the
// ordering of the rows is normalized by sort so a sequence
// change does not fail this test.
func snapshotUserTables(t *testing.T, dm *mpminternal.DatabaseManager) string {
	t.Helper()
	h := sha256.New()
	tables := []string{"memories", "evidence", "retrieval_metadata", "lessons"}
	for _, tbl := range tables {
		// Not every test fixture has every table; ignore the
		// not-found error and skip silently. The point is to
		// record whatever IS there.
		rows, err := dm.SQLDB().Query("SELECT * FROM " + tbl)
		if err != nil {
			continue
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			continue
		}
		var dump []string
		for rows.Next() {
			raw := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", tbl, err)
			}
			// Render each cell with its column name so
			// column-order shifts in the schema still
			// produce a comparable hash.
			parts := make([]string, len(cols))
			for i, v := range raw {
				parts[i] = cols[i] + "=" + stringifyForHash(v)
			}
			dump = append(dump, strings.Join(parts, "|"))
		}
		rows.Close()
		sort.Strings(dump)
		h.Write([]byte(tbl))
		h.Write([]byte{'\n'})
		for _, line := range dump {
			h.Write([]byte(line))
			h.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stringifyForHash renders a SQL cell value into a string the
// hash can be computed over. Mirrors the type-switches used by
// the existing captureStdout/record helper files.
func stringifyForHash(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case string:
		return x
	default:
		// Fall through to fmt for the rest; we don't need
		// every type, we need consistency across calls.
		return strings.TrimSpace(strings.Replace(
			asStringHelper(x), "\n", "\\n", -1))
	}
}

func asStringHelper(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unprintable>"
	}
	return string(b)
}

// TestIssue20_ListTruncationReportsTotal — HAL finding 2.
//
// The audit-time 41 fit inside the default --limit=50, but a
// future operator running with a smaller --limit (or against
// a population that grew past 50 from a future
// resolve-contradictions --apply pass) needs to know whether
// the result was truncated. The original envelope only
// reported `count` and `limit`, which is ambiguous: a
// script reading `count=2, limit=2` cannot tell whether the
// database had exactly 2 rows or 200.
//
// The fix adds `total_count` and `truncated` fields. This
// test seeds 5 rows, requests --limit=2, and asserts the
// envelope reports count=2, total_count=5, truncated=true.
// It also asserts the not-truncated case (5 rows, --limit=10)
// reports total_count=5, truncated=false.
func TestIssue20_ListTruncationReportsTotal(t *testing.T) {
	dm := newTestDMForCmd(t)

	// Seed 5 challenged rows. Unique content per row
	// (SaveMemory enforces uniqueness).
	for i := 0; i < 5; i++ {
		issue20ListSeed(t, dm, fmt.Sprintf("issue20 truncation %d", i), 1.0)
	}

	// Truncated case: --limit=2 against 5 rows.
	var code int
	out := captureStdout(t, func() {
		code = runChallengeList(dm, true, 2)
	})
	require.Equal(t, 0, code)
	var env struct {
		Success    bool                     `json:"success"`
		Count      int                      `json:"count"`
		TotalCount int                      `json:"total_count"`
		Limit      int                      `json:"limit"`
		Truncated  bool                     `json:"truncated"`
		Challenged []map[string]interface{} `json:"challenged"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimRight(out, "\n")), &env))
	assert.Equal(t, 2, env.Count, "count must be the slice length, not the population")
	assert.Equal(t, 5, env.TotalCount, "total_count must be the full population")
	assert.Equal(t, 2, env.Limit)
	assert.True(t, env.Truncated, "truncated flag must be set when count < total_count")
	assert.Len(t, env.Challenged, 2)

	// Not-truncated case: --limit=10 against 5 rows.
	var code2 int
	out2 := captureStdout(t, func() {
		code2 = runChallengeList(dm, true, 10)
	})
	require.Equal(t, 0, code2)
	var env2 struct {
		Count      int  `json:"count"`
		TotalCount int  `json:"total_count"`
		Truncated  bool `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimRight(out2, "\n")), &env2))
	assert.Equal(t, 5, env2.Count)
	assert.Equal(t, 5, env2.TotalCount)
	assert.False(t, env2.Truncated, "truncated flag must be false when the limit was high enough")

	// The human-readable path also reports the truncation
	// status. Pin the wording: "showing N of M" only when
	// the slice was actually cut.
	var code3 int
	out3 := captureStdout(t, func() {
		code3 = runChallengeList(dm, false, 2)
	})
	require.Equal(t, 0, code3)
	assert.Contains(t, out3, "showing 2 of 5",
		"truncated human-readable output must say 'showing N of M'")
	assert.Contains(t, out3, "5 memories in status='challenged'",
		"human-readable output must report the total count")
}

// TestIssue20_ListRestoreWeightChangeAbsentVsZero — HAL finding 4
// + HAL follow-up correction.
//
// Two rows that are operationally distinct must not collapse
// in EITHER the JSON envelope or the human table:
//
//   - Row A: prior weight 1.0, current weight 1.0.
//     Restore is a no-op on weight. RestoreWeightChange = 0.0
//     and RestoreWeightChangePresent = true (the value is
//     honestly 0.0, computed from a real prior weight).
//     Human table column: "1.0←1.0".
//
//   - Row B: no prior weight was recorded (PriorRetrievalPriority
//     exists, but PriorWeight is absent — the Arc 1 writer
//     residue). Restoreable = true (via retrieval_priority),
//     but the weight-delta is undefined. Without the explicit
//     Present flag, this row would report RestoreWeightChange
//     = 0.0 and collapse with Row A in the operator's view.
//     Human table column: "1.0←[absent]" — NOT "1.0←1.0", NOT
//     "1.0←?" — a marker that makes the absence obvious to
//     the operator scanning the table.
//
// The fix: RestoreWeightChangePresent distinguishes "the delta
// is genuinely zero" from "the delta is undefined." The CLI
// surface renders the absent case as null (JSON) and as
// "[absent]" (human table); the genuine floor case renders
// as 0.0 (JSON) and "1.0←1.0" (human table).
func TestIssue20_ListRestoreWeightChangeAbsentVsZero(t *testing.T) {
	dm := newTestDMForCmd(t)

	// Row A: floor no-op. Production challenge path writes a
	// prior weight baseline.
	floorID := issue20ListSeed(t, dm, "issue20 restore weight floor", 1.0)

	// Row B: no prior weight. Hand-craft the metadata so the
	// row looks like Arc 1 writer residue (prior weight
	// absent, prior retrieval_priority present).
	rpOnlyID := "issue20-rp-only-no-prior-weight"
	rpOnlyMeta := `{"status":"challenged","challenged_at":1750000000,"challenged_via":"arc1_residue","challenged_theory_id":"","challenged_prior_retrieval_priority":0.7}`
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, metadata, weight, retrieval_priority, importance, confidence)
		 VALUES (?, 'memories', ?, ?, 1.0, 0.5, 0.5, 0.5)`,
		rpOnlyID, "issue20 rp-only content", rpOnlyMeta)
	require.NoError(t, err, "seed rp-only row")

	// -------------------------------------------------------------
	// Part 1: JSON envelope
	// -------------------------------------------------------------
	var code int
	out := captureStdout(t, func() { code = runChallengeList(dm, true, challengeListLimitDefault) })
	require.Equal(t, 0, code)

	var env struct {
		Challenged []map[string]interface{} `json:"challenged"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimRight(out, "\n")), &env))
	require.Len(t, env.Challenged, 2)

	byID := make(map[string]map[string]interface{}, 2)
	for _, row := range env.Challenged {
		byID[row["id"].(string)] = row
	}
	require.Contains(t, byID, floorID)
	require.Contains(t, byID, rpOnlyID)

	// Floor row: a real 0.0, not null.
	floor := byID[floorID]
	assert.EqualValues(t, 0.0, floor["restore_weight_change"],
		"a real no-op row must report restore_weight_change=0.0, NOT null")
	assert.Equal(t, true, floor["restoreable"])

	// RP-only row: null (undefined), distinct from 0.0.
	rpOnly := byID[rpOnlyID]
	assert.Nil(t, rpOnly["restore_weight_change"],
		"a row with no prior weight must report restore_weight_change=null, NOT 0.0 — collapsing it with the floor case would mislead the operator")
	assert.Equal(t, true, rpOnly["restoreable"],
		"an RP-only row is still restorable (the retrieve-priority half); only the weight-delta is undefined")
	assert.EqualValues(t, 0.7, rpOnly["prior_retrieval_priority"],
		"prior_retrieval_priority must surface as 0.7 (the value the restore path would reinstate)")

	// -------------------------------------------------------------
	// Part 2: human table — the HAL follow-up correction
	// -------------------------------------------------------------
	// The human table must NOT show "1.0←1.0" for the absent
	// case. That would invent a historical weight value the
	// system never recorded. The honest marker is "[absent]".
	rows, err := dm.ListChallengedMemories()
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byIDWeight := map[string]string{}
	for _, r := range rows {
		byIDWeight[r.ID] = weightArrow(r)
	}

	// Row A: prior weight is genuinely 1.0, so "1.0←1.0" is
	// the honest reading.
	assert.Equal(t, "1.0←1.0", byIDWeight[floorID],
		"a row with prior weight 1.0 must render as 1.0←1.0 (current=prior, both recorded)")

	// Row B: no prior weight was recorded. The table column
	// must read "1.0←[absent]", NOT "1.0←1.0" (that would
	// invent data) and NOT "1.0←?" (ambiguous).
	assert.Equal(t, "1.0←[absent]", byIDWeight[rpOnlyID],
		"a row with no recorded prior weight must render as 1.0←[absent] — NOT 1.0←1.0, which would invent a historical weight the system never recorded")
	assert.NotEqual(t, "1.0←1.0", byIDWeight[rpOnlyID],
		"HAL follow-up: the absent-prior row must not render as 1.0←1.0 — that misleads the operator into thinking the prior weight was 1.0")
}
