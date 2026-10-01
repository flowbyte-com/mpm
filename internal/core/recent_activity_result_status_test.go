// recent_activity_result_status_test.go — regression coverage for the
// result_status selector on RecentActivityQueryParams.
//
// # The defect this pins
//
// recent_activity.go hardcoded `result_status = 'success'` into its
// WHERE clause. Failed invocations were always WRITTEN to
// tool_invocations (both writers derive the value from `err != nil`,
// and the schema CHECK admits 'error'), but no query could ever read
// them back. An operator debugging a failed call had a durable record
// on disk and no surface to read it through.
//
// # The fix this pins
//
// An explicit three-state selector. The default resolves to Success so
// that every pre-existing caller — which constructs the struct without
// the new field — keeps the exact surface it had. The tests below are
// mostly about that NOT changing.
//
// All fixtures are hermetic: NewTestSharedDM roots both the local and
// shared database in t.TempDir() and redirects MPM_WORKSPACE, so no
// assertion here can reach the live MPM database.
package internal

import (
	"strings"
	"testing"
	"time"
)

// seedStatusPair inserts one successful and one failed invocation of
// the same mutating action, so that a query's result can only be
// explained by its status filter — never by action-class filtering,
// ordering, or the fixture differing in any other respect.
func seedStatusPair(t *testing.T, dm *DatabaseManager) (successID, errorID string) {
	t.Helper()
	now := time.Now().Unix()
	successID = "rs-ok"
	errorID = "rs-err"

	seedToolInvocation(t, dm, seedArgs{
		ID: successID, Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "rs-inv-ok", ResultStatus: "success",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: errorID, Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now + 1, CompletedAt: now + 2,
		InvocationID: "rs-inv-err", ResultStatus: "error",
		ErrorMessage: "synthetic failure",
	})
	return successID, errorID
}

// ids extracts the ID field from a result set for order-independent
// comparison. The selector's contract is membership, not ordering, so
// asserting on a sorted set keeps a failure diagnostic readable.
func ids(events []RecentActivityEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

func hasEventID(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestRecentActivity_ResultStatus_DefaultReturnsSuccessOnly covers
// requirements 1 and 2 together, because they are one assertion: the
// zero-value struct must return the success row and must not return the
// error row.
//
// "Must not" is the half that matters. A selector that merely added
// the success row would still pass a positive-only test, and would
// have silently changed what every existing caller means.
func TestRecentActivity_ResultStatus_DefaultReturnsSuccessOnly(t *testing.T) {
	dm := NewTestSharedDM(t)
	successID, errorID := seedStatusPair(t, dm)

	// Zero value: the struct exactly as every pre-existing caller
	// builds it, with ResultStatus left unset.
	events, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}

	got := ids(events)
	if !hasEventID(got, successID) {
		t.Errorf("default query omitted the successful row %q; got %v", successID, got)
	}
	if hasEventID(got, errorID) {
		t.Errorf("default query returned the failed row %q; got %v. "+
			"Failures must stay invisible unless a caller opts in", errorID, got)
	}
	if len(got) != 1 {
		t.Errorf("default query returned %d events, want exactly 1: %v", len(got), got)
	}
}

// TestRecentActivity_ResultStatus_ExplicitSuccessMatchesDefault pins
// that spelling the default out loud is the same query as leaving it
// unset. Without this, "" and "success" could drift apart and an
// explicit opt-in would mean something different from the implicit one.
func TestRecentActivity_ResultStatus_ExplicitSuccessMatchesDefault(t *testing.T) {
	dm := NewTestSharedDM(t)
	seedStatusPair(t, dm)

	implicit, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity (implicit): %v", err)
	}
	explicit, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, ResultStatus: RecentActivityStatusSuccess,
	})
	if err != nil {
		t.Fatalf("RecentActivity (explicit success): %v", err)
	}

	if a, b := strings.Join(ids(implicit), ","), strings.Join(ids(explicit), ","); a != b {
		t.Errorf("explicit success = [%s], implicit default = [%s]; "+
			"the two spellings of the same intent must agree", b, a)
	}
}

// TestRecentActivity_ResultStatus_Error returns the failed row and NOT
// the successful one.
//
// The "not" is load-bearing. The obvious half-bug — widening the
// predicate to `result_status != 'success'` or dropping it — would pass
// a test that only asserts the error row is present, while handing
// every opted-in caller a different result set than it asked for.
func TestRecentActivity_ResultStatus_Error(t *testing.T) {
	dm := NewTestSharedDM(t)
	successID, errorID := seedStatusPair(t, dm)

	events, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, ResultStatus: RecentActivityStatusError,
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}

	got := ids(events)
	if !hasEventID(got, errorID) {
		t.Errorf("result_status=error omitted the failed row %q; got %v", errorID, got)
	}
	if hasEventID(got, successID) {
		t.Errorf("result_status=error returned the successful row %q; got %v. "+
			"the selector is exclusive, not a widening", successID, got)
	}
	if len(got) != 1 {
		t.Errorf("result_status=error returned %d events, want exactly 1: %v", len(got), got)
	}
	if events[0].Status != "error" {
		t.Errorf("event status = %q, want error", events[0].Status)
	}
}

// TestRecentActivity_ResultStatus_All returns both rows.
//
// This is the case that proves the defensive `status == "error"`
// continue inside filterActivityPage was actually conditioned on the
// selector. Left unconditional it discards the error row here, and the
// parameter looks accepted while doing nothing.
func TestRecentActivity_ResultStatus_All(t *testing.T) {
	dm := NewTestSharedDM(t)
	successID, errorID := seedStatusPair(t, dm)

	events, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, ResultStatus: RecentActivityStatusAll,
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}

	got := ids(events)
	if !hasEventID(got, successID) {
		t.Errorf("result_status=all omitted the successful row %q; got %v", successID, got)
	}
	if !hasEventID(got, errorID) {
		t.Errorf("result_status=all omitted the failed row %q; got %v. "+
			"The unconditional error skip in filterActivityPage is still "+
			"discarding it", errorID, got)
	}
	if len(got) != 2 {
		t.Errorf("result_status=all returned %d events, want exactly 2: %v", len(got), got)
	}
}

// TestRecentActivity_ResultStatus_RejectsUnknown pins that an
// unrecognised value is an error rather than a fallback.
//
// The dangerous fallback is "all": a caller writing "failures" or
// "ERRROR" would silently receive every row, including the ones it
// was trying to exclude, with no signal that the spelling was wrong.
// The second-dangerous fallback is "success": a typo would look like a
// correct empty result. Rejecting is the only outcome that is honest
// under both.
func TestRecentActivity_ResultStatus_RejectsUnknown(t *testing.T) {
	dm := NewTestSharedDM(t)
	seedStatusPair(t, dm)

	for _, bad := range []string{
		"failures", "ERRROR", "Success", "success,error", "*", "succeeded", "0",
	} {
		t.Run(bad, func(t *testing.T) {
			events, err := dm.RecentActivity(RecentActivityQueryParams{
				Limit: 50, ResultStatus: bad,
			})
			if err == nil {
				t.Fatalf("result_status=%q was accepted and returned %v; "+
					"an unknown selector must be rejected, never widened", bad, ids(events))
			}
			if len(events) != 0 {
				t.Errorf("result_status=%q returned %d events alongside its error; "+
					"a rejected query must return no rows", bad, len(events))
			}
			// The message must name the accepted values, or the caller
			// cannot correct the call without reading the source.
			for _, want := range []string{RecentActivityStatusSuccess, RecentActivityStatusError, RecentActivityStatusAll} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error for %q does not mention the accepted value %q: %v",
						bad, want, err)
				}
			}
		})
	}
}

// TestRecentActivity_ResultStatus_ParameterizedSQL asserts the value
// reaches SQLite as a bound parameter rather than being formatted into
// the statement. The selector is validated against a closed vocabulary
// before the query is built, so an injected string is already
// impossible here — this test exists to keep it that way if the
// validation is ever moved after query construction.
func TestRecentActivity_ResultStatus_ParameterizedSQL(t *testing.T) {
	// A syntactically valid predicate fragment. If it were spliced
	// into the SQL rather than bound, this would change the result set.
	const injection = "success' OR '1'='1"

	dm := NewTestSharedDM(t)
	successID, _ := seedStatusPair(t, dm)

	// It is not in the vocabulary, so it must be rejected outright.
	// If a future change ever made the filter unparameterized AND
	// unvalidated, the WHERE clause would become tautological and both
	// rows would come back — which this assertion catches.
	events, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, ResultStatus: injection,
	})
	if err == nil {
		t.Fatalf("injection-shaped value accepted, returning %v", ids(events))
	}
	if len(events) != 0 {
		t.Errorf("rejected query returned %d events", len(events))
	}
	if hasEventID(ids(events), successID) {
		t.Error("rejected query leaked a row")
	}
}

// TestRecentActivity_ResultStatus_ComposesWithOtherFilters proves the
// new predicate is ANDed with the existing ones rather than replacing
// them. A filter that replaced the WHERE clause would drop the
// since/framework/actor constraints; one that was ordered wrong against
// its placeholder would mis-bind and return nothing.
func TestRecentActivity_ResultStatus_ComposesWithOtherFilters(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()

	// Matching framework, error row.
	seedToolInvocation(t, dm, seedArgs{
		ID: "cf-err-openclaw", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "cf-inv-err", ResultStatus: "error",
	})
	// Non-matching framework, error row — must stay hidden.
	seedToolInvocation(t, dm, seedArgs{
		ID: "cf-err-pi", Tool: "mpm_memory", Action: "save",
		FrameworkName: "pi", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "cf-inv-err-pi", ResultStatus: "error",
	})
	// Matching framework but before the since cutoff — must stay hidden.
	seedToolInvocation(t, dm, seedArgs{
		ID: "cf-err-old", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now - 10_000, CompletedAt: now - 10_000 + 1,
		InvocationID: "cf-inv-err-old", ResultStatus: "error",
	})

	events, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit:         50,
		ResultStatus:  RecentActivityStatusError,
		FrameworkName: "openclaw",
		Since:         now - 100,
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}

	got := ids(events)
	if !hasEventID(got, "cf-err-openclaw") {
		t.Errorf("missing the row matching every filter; got %v", got)
	}
	if hasEventID(got, "cf-err-pi") {
		t.Errorf("framework filter not applied alongside result_status; got %v", got)
	}
	if hasEventID(got, "cf-err-old") {
		t.Errorf("since filter not applied alongside result_status; got %v", got)
	}
	if len(got) != 1 {
		t.Errorf("got %d events, want 1: %v", len(got), got)
	}
}

// TestRecentActivity_ResultStatus_ExistingCallersUnchanged is the
// compatibility proof the parameter exists to protect.
//
// The two in-module production callers — gatherRecentActivity (wake
// context) and addActivityCandidates (contextual candidates) — both
// construct RecentActivityQueryParams without ResultStatus. This test
// exercises their actual code paths against a fixture containing both
// outcomes, and asserts neither surfaces a failure.
//
// A test that only called RecentActivity directly would not have
// caught someone later adding ResultStatus: "all" to one of these call
// sites to "fix" an agent complaint, which is precisely the change
// this pins shut.
func TestRecentActivity_ResultStatus_ExistingCallersUnchanged(t *testing.T) {
	t.Run("gatherRecentActivity", func(t *testing.T) {
		dm := NewTestSharedDM(t)
		_, errorID := seedStatusPair(t, dm)

		activity := dm.gatherRecentActivity(10)
		for _, a := range activity {
			if a.ID == errorID {
				t.Errorf("wake context surfaced failed invocation %q; "+
					"gatherRecentActivity must stay on the default success view", errorID)
			}
		}
	})

	t.Run("addActivityCandidates", func(t *testing.T) {
		dm := NewTestSharedDM(t)
		_, errorID := seedStatusPair(t, dm)

		acc := make(map[string]*candidateAccumulator)
		_, _ = addActivityCandidates(dm, ContextQuery{FrameworkName: "openclaw"}, 10, acc)

		if _, ok := acc[errorID]; ok {
			t.Errorf("contextual candidates surfaced failed invocation %q; "+
				"addActivityCandidates must stay on the default success view", errorID)
		}
	})
}
