// cli_acceptance_20260912_test.go — regression pins for the 2026-09-12
// CLI acceptance pass. Three minimal root-cause fixes:
//
//  1. `mpm memory add --json` without --weight echoed the raw legacy
//     default (0.5) while `mpm call mpm_memory save` reports the
//     normalized column (5). The JSON envelope now echoes the persisted
//     weight when --weight was not supplied; explicit --weight keeps the
//     F-H4 echo-user-input contract.
//  2. `mpm weaken` at the weight floor (1) printed "Weakened (-1)"
//     although SQL clamped the value (MAX(weight-?,1)) and nothing
//     changed. It now reports "at minimum weight (1)".
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCLI Acceptance_DefaultWeightJSONEchoMatchesCanonical pins fix 1: a
// default add via the friendly CLI must report the same weight as the
// canonical tool surface (normalized column, 5) — not the raw 0.5 default.
func TestCLIAcceptance_DefaultWeightJSONEchoMatchesCanonical(t *testing.T) {
	setupMemoryAddTest(t)

	out := captureBoth(t, func() {
		code := handleMemoryAdd([]string{"--json", "cli-acceptance default weight probe"})
		require.Equal(t, 0, code)
	})
	var lastJSON string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			lastJSON = line
		}
	}
	require.NotEmpty(t, lastJSON, "expected JSON envelope in output:\n%s", out)
	var env map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(lastJSON), &env))
	w, ok := env["weight"].(float64)
	require.True(t, ok, "envelope must carry numeric weight (got %v)", env["weight"])
	require.InDelta(t, 5.0, w, 0.001,
		"default add JSON weight must match canonical column value 5, not raw 0.5")
}

// TestCLIAcceptance_ExplicitWeightJSONEchoPreserved pins the F-H4 side of
// fix 1: an explicit --weight still echoes user input.
func TestCLIAcceptance_ExplicitWeightJSONEchoPreserved(t *testing.T) {
	setupMemoryAddTest(t)

	out := captureBoth(t, func() {
		code := handleMemoryAdd([]string{"--weight", "7.5", "--json", "cli-acceptance explicit weight probe"})
		require.Equal(t, 0, code)
	})
	var lastJSON string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			lastJSON = line
		}
	}
	require.NotEmpty(t, lastJSON)
	var env map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(lastJSON), &env))
	w, ok := env["weight"].(float64)
	require.True(t, ok)
	require.InDelta(t, 7.5, w, 0.001, "explicit --weight must echo user input per F-H4")
}

// TestCLIAcceptance_WeakenFloorMessage pins fix 2: weakening a memory
// already at weight 1 must say so instead of claiming a decrement.
func TestCLIAcceptance_WeakenFloorMessage(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{"--weight", "1", "cli-acceptance weaken floor probe"})
	require.Equal(t, 0, code)

	var id string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id FROM memories WHERE content = 'cli-acceptance weaken floor probe'`,
	).Scan(&id))

	// Weaken twice: first hits the floor path (already at 1), second
	// confirms the floor message is stable.
	for i := 0; i < 2; i++ {
		out := captureBoth(t, func() {
			rc := handleWeaken([]string{"weaken", id})
			require.Equal(t, 0, rc)
		})
		require.Contains(t, out, "at minimum weight (1)",
			"weaken at floor must communicate the floor (got %q)", out)
	}
	var w float64
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, id,
	).Scan(&w))
	require.InDelta(t, 1.0, w, 0.001, "weight must never go below the floor of 1")
}

// TestCLIAcceptance_DecideBareKeyValue pins the 2026-09-12 acceptance
// fix: `mpm decide context="..." choice="..." rationale="..."` (the form
// taught by `mpm tour` step 4 and the decision help examples) must parse
// into structured fields. Pre-fix the whole string landed in CHOICE with
// empty CONTEXT/RATIONALE. Mirrors the M3 theory-parser precedent
// (TestParseTheoryArgs_BareKeyValue).
func TestCLIAcceptance_DecideBareKeyValue(t *testing.T) {
	ctx, choice, rat, tags, _, leftovers := parseDecisionArgs([]string{
		`context="tour step"`,
		`choice="continue"`,
		`rationale="because stable"`,
	})
	require.Equal(t, "tour step", ctx)
	require.Equal(t, "continue", choice)
	require.Equal(t, "because stable", rat)
	require.Equal(t, "", tags)
	require.Empty(t, leftovers)
}

// TestCLIAcceptance_DecideBareKeyValue_BackslashQuotes covers the tour
// demo's programmatic path, which historically passed backslash-escaped
// quotes through splitKeyValueArgs.
func TestCLIAcceptance_DecideBareKeyValue_BackslashQuotes(t *testing.T) {
	args := splitKeyValueArgs(`context=\"tour step\" choice=\"continue\" rationale=\"because stable\"`)
	ctx, choice, rat, _, _, _ := parseDecisionArgs(args)
	require.Equal(t, "tour step", ctx)
	require.Equal(t, "continue", choice)
	require.Equal(t, "because stable", rat)
}

// TestCLIAcceptance_DecideFreeTextUnaffected guards the fix: plain free
// text (including text containing "=" that does not start with a known
// key) still lands in choice.
func TestCLIAcceptance_DecideFreeTextUnaffected(t *testing.T) {
	_, choice, _, _, _, _ := parseDecisionArgs([]string{"just", "some", "free", "text"})
	require.Equal(t, "just some free text", choice)
	_, choice2, _, _, _, _ := parseDecisionArgs([]string{"a=b"})
	require.Equal(t, "a=b", choice2)
}
