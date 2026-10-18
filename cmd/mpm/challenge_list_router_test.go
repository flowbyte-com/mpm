// challenge_list_router_test.go — end-to-end router dispatch test
// for `mpm challenge list` (issue #20).
//
// The router has three relevant sub-dispatches under the
// "challenge" family:
//   - `mpm challenge <id> <evidence>`     → handleChallenge
//   - `mpm challenge list [...]`           → handleChallengeList
//   - `mpm challenge restore <id> [...]`   → handleChallengeRestore
//
// HAL defect 1 (post #20): the previous test in this file called
// handleChallengeList and runChallengeList directly, which
// sidestepped the actual CommandRouter.Execute path. That hid two
// bugs: handleChallengeList() discarded the cleaned --json args and
// re-stripped --limit from the original args, and the router still
// imposed MaxArgs: 2 on the "challenge" command, so
// `mpm challenge list --json --limit 2` was rejected before
// reaching the handler. This file replaces the bypass with real
// CommandRouter.Execute calls and pins the five invocations the
// operator needs.
//
// Every test installs a disposable dm via installTestDM so the
// global getDB() inside the handlers resolves to a test
// database, not the production workspace. The override is
// removed on test cleanup.
package main

import (
	"encoding/json"
	"strings"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driveRouter runs args through the real CommandRouter.Execute
// path and returns the combined stdout+stderr output and the
// exit code. The router is fresh for each call so there is no
// cross-test state.
func driveRouter(t *testing.T, args []string) (string, int) {
	t.Helper()
	router := NewRouter()
	var code int
	out := captureBoth(t, func() {
		code = router.Execute(args)
	})
	return out, code
}

// TestIssue20_RouterChallengeList_BareInvocation —
// `mpm challenge list` (no flags) on an empty DB. The
// human-readable table path: must exit 0, must mention
// "0 memories", must mention the discovery-surface heading.
func TestIssue20_RouterChallengeList_BareInvocation(t *testing.T) {
	dm := installTestDMRouter(t)

	out, code := driveRouter(t, []string{"challenge", "list"})

	assert.Equal(t, 0, code, "bare `mpm challenge list` must exit 0; output:\n%s", out)
	assert.Contains(t, out, "0 memories in status='challenged'",
		"empty-DB human-readable output must report 0 memories")
	assert.Contains(t, out, "No challenged memories",
		"empty-DB human-readable output must say 'No challenged memories'")
	_ = dm
}

// TestIssue20_RouterChallengeList_JSONFlag —
// `mpm challenge list --json`. The canonical envelope path:
// must exit 0, must emit valid JSON with success=true and
// the right keys.
func TestIssue20_RouterChallengeList_JSONFlag(t *testing.T) {
	installTestDMRouter(t)

	out, code := driveRouter(t, []string{"challenge", "list", "--json"})

	assert.Equal(t, 0, code, "`mpm challenge list --json` must exit 0; output:\n%s", out)

	// Strip the trailing newline that respond() appends.
	raw := strings.TrimRight(out, "\n")
	var env struct {
		Success    bool `json:"success"`
		Count      int  `json:"count"`
		TotalCount int  `json:"total_count"`
		Limit      int  `json:"limit"`
		Truncated  bool `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &env),
		"`mpm challenge list --json` must emit a parseable JSON envelope; raw: %q", raw)
	assert.True(t, env.Success, "envelope must report success=true")
	assert.Equal(t, 0, env.Count, "empty DB must report count=0")
	assert.Equal(t, 0, env.TotalCount, "empty DB must report total_count=0")
	assert.Equal(t, 50, env.Limit, "default limit must be 50")
	assert.False(t, env.Truncated, "empty DB must report truncated=false")
}

// TestIssue20_RouterChallengeList_LimitSpaceForm —
// `mpm challenge list --limit 2`. The --limit flag in the
// space-separated form: must reach the handler and apply the
// 2-row cap. This case is the human-readable path (no
// --json), so the assertion targets the table heading and
// the exit code; the --json form is exercised separately in
// LimitMaxEnforced below.
func TestIssue20_RouterChallengeList_LimitSpaceForm(t *testing.T) {
	installTestDMRouter(t)

	out, code := driveRouter(t, []string{"challenge", "list", "--limit", "2"})

	assert.Equal(t, 0, code,
		"`mpm challenge list --limit 2` must exit 0; the router must not reject it on MaxArgs; output:\n%s", out)

	// The human-readable table heading is the operator's
	// signal that the command was accepted: it must
	// mention the population count and the
	// `Challenge list` heading. With zero rows in the
	// disposable DB, the body line is `No challenged
	// memories.` and the heading line is the count.
	assert.Contains(t, out, "Challenge list",
		"the human-readable heading must appear; output:\n%s", out)
	assert.Contains(t, out, "0 memories in status='challenged'",
		"the count line must appear; output:\n%s", out)
	assert.Contains(t, out, "No challenged memories",
		"the empty-DB body must appear; output:\n%s", out)

	// The structural proof: this invocation has FOUR
	// tokens after `mpm` (`challenge list --limit 2`),
	// and the previous (pre-HAL-defect-1) router
	// MaxArgs: 2 would have rejected it before the
	// handler ran. If this test passes, MaxArgs: 2 is
	// gone and the handler ran.
}

// TestIssue20_RouterChallengeList_LimitEqualsForm —
// `mpm challenge list --limit=2`. The --limit=N single-token
// form: same outcome as the space form, but pinned
// separately so a future refactor that drops the `=` form
// fails this test loudly. Human-readable path (no --json).
func TestIssue20_RouterChallengeList_LimitEqualsForm(t *testing.T) {
	installTestDMRouter(t)

	out, code := driveRouter(t, []string{"challenge", "list", "--limit=2"})

	assert.Equal(t, 0, code,
		"`mpm challenge list --limit=2` must exit 0; the router must not reject it on MaxArgs; output:\n%s", out)

	// Same human-readable assertions as the space form.
	// The structural proof: the operator's invocation
	// contains the `=` form of the flag, which the
	// single-pass parser must accept; the previous
	// (pre-HAL-defect-1) handler would have seen
	// `--limit=2` as an unknown argument.
	assert.Contains(t, out, "Challenge list",
		"the human-readable heading must appear; output:\n%s", out)
	assert.Contains(t, out, "0 memories in status='challenged'",
		"the count line must appear; output:\n%s", out)
	assert.Contains(t, out, "No challenged memories",
		"the empty-DB body must appear; output:\n%s", out)

	// Structural: the full invocation also runs
	// through the strict-rejection check. If the
	// handler treats `--limit=2` as unknown, the
	// response would be the "unknown argument" error
	// — the absence of that string is a load-bearing
	// pin on the --limit= form.
	assert.NotContains(t, out, "unknown argument",
		"--limit=2 must NOT be treated as an unknown argument; output:\n%s", out)
}

// TestIssue20_RouterChallengeList_CombinedFlags —
// `mpm challenge list --json --limit 2`. Both flags together:
// proves the handler's single-pass argument parser does not
// drop the cleaned --json args, and that the router's
// MaxArgs no longer rejects four-arg list invocations.
func TestIssue20_RouterChallengeList_CombinedFlags(t *testing.T) {
	installTestDMRouter(t)

	out, code := driveRouter(t, []string{"challenge", "list", "--json", "--limit", "2"})

	assert.Equal(t, 0, code,
		"`mpm challenge list --json --limit 2` must exit 0; the router must not reject it on MaxArgs; output:\n%s", out)

	raw := strings.TrimRight(out, "\n")
	var env struct {
		Success bool `json:"success"`
		Limit   int  `json:"limit"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &env),
		"the combined --json --limit 2 path must emit a parseable JSON envelope; raw: %q", raw)
	assert.True(t, env.Success,
		"the --json flag must reach the handler; a no-op success=false indicates the previous bug where --json was treated as an unknown argument")
	assert.Equal(t, 2, env.Limit,
		"the --limit 2 flag must reach the handler")
}

// TestIssue20_RouterChallengeList_StrictRejection — unknown
// arguments and malformed --limit values must be rejected
// with a non-zero exit code and a meaningful error message.
// The strict-rejection contract is load-bearing: a refactor
// that silently ignores a typo would let an operator run
// the command and get surprising output.
func TestIssue20_RouterChallengeList_StrictRejection(t *testing.T) {
	installTestDMRouter(t)

	cases := []struct {
		name string
		args []string
		// substrings that must appear in the error output
		mustContain []string
	}{
		{
			name:        "unknown flag",
			args:        []string{"challenge", "list", "--bogus"},
			mustContain: []string{"unknown argument", "--bogus"},
		},
		{
			name:        "unknown positional",
			args:        []string{"challenge", "list", "surprise"},
			mustContain: []string{"unknown argument", "surprise"},
		},
		{
			name:        "missing --limit value",
			args:        []string{"challenge", "list", "--limit"},
			mustContain: []string{"--limit requires a value"},
		},
		{
			name:        "non-numeric --limit",
			args:        []string{"challenge", "list", "--limit", "abc"},
			mustContain: []string{"--limit value", "abc", "positive integer"},
		},
		{
			name:        "zero --limit",
			args:        []string{"challenge", "list", "--limit", "0"},
			mustContain: []string{"--limit value", "0", "positive integer"},
		},
		{
			name:        "negative --limit",
			args:        []string{"challenge", "list", "--limit", "-5"},
			mustContain: []string{"--limit value", "-5", "positive integer"},
		},
		{
			name:        "non-numeric --limit= form",
			args:        []string{"challenge", "list", "--limit=foo"},
			mustContain: []string{"--limit value", "foo", "positive integer"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := driveRouter(t, tc.args)
			assert.NotEqual(t, 0, code,
				"case %q must exit non-zero; output:\n%s", tc.name, out)
			for _, want := range tc.mustContain {
				assert.Contains(t, out, want,
					"case %q: error output must contain %q; output:\n%s", tc.name, want, out)
			}
		})
	}
}

// TestIssue20_RouterChallengeList_LimitMaxEnforced — the
// 1000-row safety maximum must be enforced. The cap protects
// the human-readable table from runaway widths; a refactor
// that drops the cap would let an operator render 50,000 rows
// in a single command.
func TestIssue20_RouterChallengeList_LimitMaxEnforced(t *testing.T) {
	installTestDMRouter(t)

	out, code := driveRouter(t, []string{"challenge", "list", "--json", "--limit", "5000"})

	assert.Equal(t, 0, code,
		"`mpm challenge list --limit 5000` must exit 0; the cap is silently applied, not an error; output:\n%s", out)

	raw := strings.TrimRight(out, "\n")
	var env struct {
		Success bool `json:"success"`
		Limit   int  `json:"limit"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &env),
		"the --limit 5000 path must emit a parseable JSON envelope; raw: %q", raw)
	assert.True(t, env.Success)
	assert.Equal(t, 1000, env.Limit,
		"the 1000-row safety maximum must be enforced; --limit 5000 must report limit=1000 in the envelope")
}

// installTestDMRouter installs a disposable dm as the
// global dbManager for the duration of the test. The global
// override is removed on test cleanup. Defined here (rather
// than in capability_seed_test.go) to keep the issue #20
// test plumbing in one place.
func installTestDMRouter(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	return dm
}
