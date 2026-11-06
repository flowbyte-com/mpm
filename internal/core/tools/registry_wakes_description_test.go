// registry_wakes_description_test.go — the mpm_wakes description must
// name the ownership refusals the resolve action can produce.
//
// WHY THIS EXISTS
// ---------------
// `mpm_wakes action=resolve` is the only audited way for an agent to
// retire a wake. It refuses in two cases that are DECISIONS, not
// transport failures:
//
//   - not_a_wake        — the wake's kind is scheduler-owned, so the
//     agent may not retire it;
//   - corrupt_metadata  — the row's metadata is present, non-empty and
//     not a parseable JSON document, so the row's OWNER cannot be
//     determined at all.
//
// The second refusal is the one an agent cannot infer from the argument
// it sent: the call is well-formed, the id exists, the reason is
// valid, and the answer is still "no". It arrived with issue #178's
// W4 defect — a malformed metadata document turns one row into a
// statement-wide failure — and the refusal it produces is the whole
// point of the fix.
//
// The runtime envelope carries the status and a self-describing error,
// so an agent that hits the refusal can recover. But the description is
// what an agent READS BEFORE it calls, and it used to name neither
// refusal's rule. An agent that did not know the ownership rule could
// reasonably retry with a different reason, or conclude the wake was
// broken and try to force it.
//
// WHAT THIS PINS
// --------------
//   1. The resolve path still RETURNS each ownership refusal — read
//      from the implementation's code, not from a comment.
//   2. The registered mpm_wakes description names every one of them.
//
// HOW THE LINK IS MADE
// --------------------
// The implementation's full set of refusal statuses is DERIVED from the
// code: `internal/core/wake_tools.go` returns each one as a
// `wakeResolveStatusError{status: "..."}` literal, and this test
// collects those literals (with Go's line comments stripped, so a
// status that survives only in a comment does not count). Each
// ownership refusal must appear in that derived set — a rename or a
// deleted refusal path fails here rather than turning the description
// into a claim about a status the implementation no longer produces.
//
// The ownership set itself is PINNED explicitly, because "ownership
// refusal" is a policy distinction the implementation does not mark:
// the transport failures (lookup_failed, update_failed,
// rows_affected_failed) are returned the same way, and naming them in
// an always-on tool schema would inflate every agent's context for no
// gain.
//
// A rewrite of the description that keeps the two ownership refusals
// named passes; a rewrite that quietly drops one fails.
package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// wakesOwnershipRefusals are the resolve refusals that are ownership
// decisions: the agent's request was well-formed and the answer is
// still "you may not retire this". They are the ones a description
// must name, because an agent cannot infer them from its own call.
var wakesOwnershipRefusals = []string{
	"not_a_wake",
	"corrupt_metadata",
}

// wakesStatusLiteral matches the status a refusal carries. It is the
// form the implementation actually writes:
//
//	&wakeResolveStatusError{status: "corrupt_metadata", err: ...}
//
// The literal is matched on the COMPOSITE, not on a bare `status: "x"`
// field, so an unrelated struct field with the same shape elsewhere in
// the file cannot widen the derived set.
var wakesStatusLiteral = regexp.MustCompile(`wakeResolveStatusError\{status:\s*"([a-z_]+)"`)

// wakesImplementationSource returns the source that defines the resolve
// refusal statuses, with Go's line comments stripped.
//
// The comment stripping is load-bearing: without it a status that
// survives only in a comment would count as "the implementation still
// returns it", and the link between the description and the code would
// be a fiction.
func wakesImplementationSource(t *testing.T) string {
	t.Helper()
	// The test lives in internal/core/tools; the implementation is in
	// the parent package's directory.
	path := filepath.Join("..", "wake_tools.go")
	src, err := os.ReadFile(path)
	require.NoError(t, err, "read the wake implementation: %s", path)

	var kept []string
	for _, line := range strings.Split(string(src), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// wakesImplementedStatuses returns the set of refusal statuses the
// resolve path actually returns, read from the implementation's code.
func wakesImplementedStatuses(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range wakesStatusLiteral.FindAllStringSubmatch(
		wakesImplementationSource(t), -1) {
		out[m[1]] = true
	}
	return out
}

// TestMpmWakesDescriptionNamesTheOwnershipRefusals pins that the
// registered description names the refusals.
func TestMpmWakesDescriptionNamesTheOwnershipRefusals(t *testing.T) {
	tool, ok := ByName("mpm_wakes")
	require.True(t, ok, "mpm_wakes must be registered")
	require.NotEmpty(t, tool.Description, "mpm_wakes must carry a description")

	implemented := wakesImplementedStatuses(t)
	require.NotEmpty(t, implemented,
		"no refusal status literal was found in the resolve implementation — "+
			"the implementation's shape changed and this test would pass vacuously")

	for _, status := range wakesOwnershipRefusals {
		require.True(t, implemented[status],
			"the resolve implementation no longer returns the status %q — "+
				"this test's list is stale and the description's claim about "+
				"it must be re-derived", status)
		require.Contains(t, tool.Description, status,
			"the mpm_wakes description does not name the ownership refusal %q. "+
				"An agent cannot infer an ownership decision from the call it "+
				"made, so the description has to state it", status)
	}
}

// TestMpmWakesDescriptionStatesTheNoWriteRule pins the second half of
// the refusal contract: a refusal must leave the row alone. That is
// what makes the refusal safe to receive — the agent's call changed
// nothing, so it can decide what to do next without having half-
// retired a wake.
func TestMpmWakesDescriptionStatesTheNoWriteRule(t *testing.T) {
	tool, ok := ByName("mpm_wakes")
	require.True(t, ok, "mpm_wakes must be registered")
	desc := tool.Description

	lower := strings.ToLower(desc)
	require.True(t,
		strings.Contains(lower, "leaves the row untouched") ||
			strings.Contains(lower, "the row is not written"),
		"the mpm_wakes description must state that a refusal leaves the row "+
			"untouched — that is what makes the refusal safe to receive")
	require.Contains(t, desc, "success=false",
		"the mpm_wakes description must state the shape of a refusal "+
			"(success=false with a status)")
}
