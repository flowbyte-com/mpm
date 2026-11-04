// wake_ownership_test.go — the ownership boundary of the wake queue.
//
// The wake queue holds two kinds of work, and they have different
// owners:
//
//   - NOTIFICATION work is the AGENT's. The opportunistic fold
//     (CheckPendingWakes) delivers it to the agent and retires it
//     (fired=1) when the agent acknowledges it.
//
//   - SYSTEM work is the SCHEDULER's. A registered HandlerFunc executes
//     it on the maintenance ticker, and the wake is retired by MarkFired
//     AFTER the handler returns. docs/SPEC.md states the contract:
//     "CheckPendingWakes filters system kinds from the opportunistic
//     fold so the two surfaces don't race for the same wake."
//
// The two must not share a retirement path. A fold that retires a
// system wake is a wake whose handler NEVER RUNS: the work is silently
// lost, and nothing in the tree records the loss.
//
// The fold is the one surface both the CLI (cmd/mpm/call.go) and the
// MCP server (cmd/mpm-mcp/tools.go) call on every request, so the
// boundary belongs here — in the DB-layer producer of the fold — rather
// than in one caller's diligence.
//
// These tests pin the separation. Every test uses a disposable database
// and never touches the operator's.
package internal

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// agentOwnedWakeKinds are the kinds the agent's notification surface
// owns. The scheduler-owned kinds are the complement.
//
// The list mirrors the code, not a document:
//
//   - "notification" — the default kind; an untagged wake is one.
//   - "cascade"      — the cascade materializer's booking; the fold
//     delivers it under the per-call cap.
//   - "cron"         — the recurring-task bookkeeping rows; the
//     scheduler never claims them (dispatchClaimNextAdHocWake excludes
//     them) and the fold is their delivery path.
//
// Everything else — snapshot, critic_audit, gc, broadcast,
// cascade_summary, drill, and any kind this tree does not know — is
// scheduler-owned, because the scheduler is what runs it.
var agentOwnedWakeKinds = []string{"notification", "cascade", "cron"}

// schedulerOwnedKind is a kind the scheduler dispatches through a
// registered handler.
const schedulerOwnedKind = "drill"

// seedOwnershipWake inserts a wake row directly.
//
// The row is inserted rather than scheduled so the fixture does not
// depend on the producer's kind policy — the point of these tests is
// what the FOLD does with a row that already exists, including rows
// written by an older binary or by direct SQL.
func seedOwnershipWake(t *testing.T, dm *DatabaseManager, id string, target time.Time, kind string) {
	t.Helper()
	metadata := "{}"
	if kind != "" {
		metadata = `{"kind":"` + kind + `"}`
	}
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, 'test', ?)`,
		id, target.Unix(), "fixture wake "+id, metadata,
	)
	require.NoError(t, err, "seed wake %s", id)
}

// ownershipWakeFired returns the wake's fired flag.
func ownershipWakeFired(t *testing.T, dm *DatabaseManager, id string) int {
	t.Helper()
	var fired int
	require.NoError(t,
		dm.SQLDB().QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, id).Scan(&fired),
		"read fired for %s", id)
	return fired
}

// ownershipWakeKind returns the wake's metadata.kind as stored.
func ownershipWakeKind(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	var raw string
	require.NoError(t,
		dm.SQLDB().QueryRow(`SELECT COALESCE(metadata,'') FROM scheduled_wakes WHERE id = ?`, id).Scan(&raw),
		"read metadata for %s", id)
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return ""
	}
	kind, _ := meta["kind"].(string)
	return kind
}

// deliveredIDs returns the ids of the wakes a fold call surfaced.
func deliveredIDs(wakes []map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	for _, w := range wakes {
		if id, ok := w["id"].(string); ok {
			out[id] = true
		}
	}
	return out
}

// TestCheckPendingWakes_WildcardDoesNotRetireSchedulerOwnedWakes is the
// headline regression.
//
// A scheduler-owned wake that is DUE is eligible: the scheduler's
// deadline-driven Tick would pick it up and run its handler. If the
// agent-facing wildcard check retires it first, the handler never runs
// and the work is lost with no trace.
func TestCheckPendingWakes_WildcardDoesNotRetireSchedulerOwnedWakes(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const drillID = "wk-ownership-drill"
	seedOwnershipWake(t, dm, drillID, now.Add(-time.Minute), schedulerOwnedKind)

	// Establish eligibility BEFORE the check. Without this the test
	// would pass for the wrong reason: a wake that was never due, or
	// never pending, would also survive the fold.
	require.Equal(t, 0, ownershipWakeFired(t, dm, drillID),
		"fixture: the scheduler-owned wake must start pending")
	var target int64
	require.NoError(t,
		dm.SQLDB().QueryRow(`SELECT target_time FROM scheduled_wakes WHERE id = ?`, drillID).Scan(&target),
		"read target_time")
	require.LessOrEqual(t, target, now.Unix(),
		"fixture: the scheduler-owned wake must be due, so the scheduler would dispatch it")

	// The agent-facing wildcard check.
	due, err := dm.CheckPendingWakes(now, []string{"*"})
	require.NoError(t, err)

	require.Equal(t, 0, ownershipWakeFired(t, dm, drillID),
		"the agent-facing wildcard check retired a scheduler-owned wake; "+
			"its handler will never run and the work is lost")
	require.False(t, deliveredIDs(due)[drillID],
		"the agent-facing wildcard check surfaced a scheduler-owned wake to the agent")
}

// TestCheckPendingWakes_ExplicitSchedulerOwnedKindIsNeitherDeliveredNorRetired
// proves the boundary is not a wildcard-only accident.
//
// An agent that names a scheduler-owned kind explicitly gets nothing
// back, and the wake stays pending for the scheduler.
func TestCheckPendingWakes_ExplicitSchedulerOwnedKindIsNeitherDeliveredNorRetired(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const drillID = "wk-ownership-explicit-drill"
	seedOwnershipWake(t, dm, drillID, now.Add(-time.Minute), schedulerOwnedKind)

	due, err := dm.CheckPendingWakes(now, []string{schedulerOwnedKind})
	require.NoError(t, err)

	require.Empty(t, due,
		"an explicit request for a scheduler-owned kind must not surface it to the agent")
	require.Equal(t, 0, ownershipWakeFired(t, dm, drillID),
		"an explicit request for a scheduler-owned kind retired it")
}

// TestCheckPendingWakes_WildcardStillDeliversTheAgentOwnedKinds is the
// negative control.
//
// A fold that refuses everything would pass every refusal test above.
// This is the test that says the refusals are selective: the agent's
// own notification work is still delivered, under the wildcard and
// under an explicit kind.
func TestCheckPendingWakes_WildcardStillDeliversTheAgentOwnedKinds(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	for _, kind := range agentOwnedWakeKinds {
		id := "wk-ownership-agent-" + kind
		seedOwnershipWake(t, dm, id, now.Add(-time.Minute), kind)
	}
	// An untagged wake is a notification wake (Wake.Kind() defaults to
	// "notification" for an absent kind).
	seedOwnershipWake(t, dm, "wk-ownership-agent-untagged", now.Add(-time.Minute), "")

	due, err := dm.CheckPendingWakes(now, []string{"*"})
	require.NoError(t, err)

	delivered := deliveredIDs(due)
	for _, kind := range agentOwnedWakeKinds {
		id := "wk-ownership-agent-" + kind
		require.True(t, delivered[id],
			"the wildcard must still deliver the agent-owned kind %q", kind)
	}
	require.True(t, delivered["wk-ownership-agent-untagged"],
		"the wildcard must still deliver an untagged (notification) wake")
	require.Len(t, delivered, len(agentOwnedWakeKinds)+1,
		"the wildcard must deliver exactly the agent-owned wakes and nothing else")
}

// TestCheckPendingWakes_DefaultBranchStillDeliversOnlyNotificationKinds
// pins the backward-compatible default.
//
// The default (no kinds) branch is the notification-only path the CLI
// and MCP fold use on every request. It must not change.
func TestCheckPendingWakes_DefaultBranchStillDeliversOnlyNotificationKinds(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	seedOwnershipWake(t, dm, "wk-ownership-default-notification", now.Add(-time.Minute), "notification")
	seedOwnershipWake(t, dm, "wk-ownership-default-untagged", now.Add(-time.Minute), "")
	seedOwnershipWake(t, dm, "wk-ownership-default-drill", now.Add(-time.Minute), schedulerOwnedKind)

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)

	delivered := deliveredIDs(due)
	require.True(t, delivered["wk-ownership-default-notification"],
		"the default branch must still deliver a notification wake")
	require.True(t, delivered["wk-ownership-default-untagged"],
		"the default branch must still deliver an untagged wake")
	require.False(t, delivered["wk-ownership-default-drill"],
		"the default branch must not deliver a scheduler-owned wake")
	require.Equal(t, 0, ownershipWakeFired(t, dm, "wk-ownership-default-drill"),
		"the default branch retired a scheduler-owned wake")
}

// TestCheckPendingWakes_ExplicitCronKindStillDelivers pins the
// existing cron contract.
//
// A cron wake is the scheduler's bookkeeping for a recurring task, but
// the AGENT is the one who acts on it: it carries a directive_id the
// agent must consult. internal/core/seed/engine_scheduled_e2e_test.go
// and internal/core/scheduled_tasks_test.go both depend on
// kinds=["cron"] returning the row, so the boundary must leave it
// alone.
func TestCheckPendingWakes_ExplicitCronKindStillDelivers(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const cronID = "wk-ownership-cron"
	seedOwnershipWake(t, dm, cronID, now.Add(-time.Minute), "cron")

	due, err := dm.CheckPendingWakes(now, []string{"cron"})
	require.NoError(t, err)
	require.True(t, deliveredIDs(due)[cronID],
		"kinds=[\"cron\"] must still surface the cron wake to the agent")
	require.Equal(t, 1, ownershipWakeFired(t, dm, cronID),
		"a delivered cron wake is retired, as before")
}

// TestCheckPendingWakes_MalformedMetadataIsTreatedAsUntagged proves a
// malformed row cannot break the whole fold.
//
// json_extract RAISES on malformed JSON. A fold whose WHERE clause
// calls it on an unguarded column turns one corrupt row into "no wake
// is ever delivered again", because every caller swallows the query
// error. The matcher therefore probes json_valid FIRST, exactly as
// ResolveWake's matcher does.
func TestCheckPendingWakes_MalformedMetadataIsTreatedAsUntagged(t *testing.T) {
	now := time.Now()

	// One database per branch: a fold call RETIRES what it delivers,
	// so a single row cannot exercise both branches.
	wildcardDM := NewTestDM(t)
	defaultDM := NewTestDM(t)
	for _, dm := range []*DatabaseManager{wildcardDM, defaultDM} {
		_, err := dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES ('wk-ownership-malformed', ?, 'malformed metadata', 0, 'test', '{not json')`,
			now.Add(-time.Minute).Unix(),
		)
		require.NoError(t, err)
	}

	due, err := wildcardDM.CheckPendingWakes(now, []string{"*"})
	require.NoError(t, err, "a malformed metadata row must not break the fold")
	require.True(t, deliveredIDs(due)["wk-ownership-malformed"],
		"a malformed metadata row is an untagged wake and must still be delivered")

	// The same must hold for the DEFAULT branch, which is the one the
	// CLI and MCP fold call on every request. Pre-fix this branch
	// called json_extract on the raw column, so one corrupt row
	// stopped delivery for EVERY wake.
	defaultDue, err := defaultDM.CheckPendingWakes(now, nil)
	require.NoError(t, err,
		"a malformed metadata row must not break the default fold branch either")
	require.True(t, deliveredIDs(defaultDue)["wk-ownership-malformed"],
		"a malformed metadata row is an untagged wake and must still be delivered by the default branch")
}

// TestCheckPendingWakes_UnknownKindIsNotDeliveredToTheAgent pins the
// fail-closed direction.
//
// A kind this tree does not know is not the agent's. It may be a
// system kind added by a newer binary, or a typo, or an attempt to
// smuggle a wake past the fold. Either way the agent surface must not
// deliver it.
func TestCheckPendingWakes_UnknownKindIsNotDeliveredToTheAgent(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const unknownID = "wk-ownership-unknown"
	seedOwnershipWake(t, dm, unknownID, now.Add(-time.Minute), "kind_this_tree_does_not_know")

	due, err := dm.CheckPendingWakes(now, []string{"*"})
	require.NoError(t, err)
	require.False(t, deliveredIDs(due)[unknownID],
		"an unknown wake kind must not be delivered to the agent")
	require.Equal(t, 0, ownershipWakeFired(t, dm, unknownID),
		"an unknown wake kind must not be retired by the agent surface")
}

// TestCheckPendingWakes_DuplicateAndConcurrentChecksDoNotRetireSchedulerOwnedWakes
// pins the concurrency contract.
//
// CheckPendingWakes is called by every MPM request, from the CLI and
// from the MCP server, against the same database. A scheduler-owned
// wake must survive all of them; and a notification wake must be
// delivered exactly once, not twice.
func TestCheckPendingWakes_DuplicateAndConcurrentChecksDoNotRetireSchedulerOwnedWakes(t *testing.T) {
	// A file-backed DM with WAL + busy_timeout=5000ms — the production
	// contract — so concurrent writers serialise the way they do in
	// production rather than surfacing SQLITE_BUSY the way the
	// in-memory shared-cache test DM does.
	dm := newTestFileDM(t)
	now := time.Now()

	const drillID = "wk-ownership-concurrent-drill"
	seedOwnershipWake(t, dm, drillID, now.Add(-time.Minute), schedulerOwnedKind)
	seedOwnershipWake(t, dm, "wk-ownership-concurrent-notification", now.Add(-time.Minute), "notification")

	// A duplicate, sequential check first: the second call must find
	// nothing left to deliver.
	first, err := dm.CheckPendingWakes(now, []string{"*"})
	require.NoError(t, err)
	require.True(t, deliveredIDs(first)["wk-ownership-concurrent-notification"],
		"the first check must deliver the notification wake")

	second, err := dm.CheckPendingWakes(now, []string{"*"})
	require.NoError(t, err)
	require.False(t, deliveredIDs(second)["wk-ownership-concurrent-notification"],
		"a duplicate check must not deliver the same wake twice")
	require.Equal(t, 0, ownershipWakeFired(t, dm, drillID),
		"a duplicate check retired a scheduler-owned wake")

	// Concurrent callers, now. The scheduler-owned wake must survive
	// every one of them, and the notification wake must be delivered
	// exactly once in total.
	dm2 := newTestFileDM(t)
	seedOwnershipWake(t, dm2, "wk-ownership-race-drill", now.Add(-time.Minute), schedulerOwnedKind)
	seedOwnershipWake(t, dm2, "wk-ownership-race-notification", now.Add(-time.Minute), "notification")

	const callers = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	delivered := map[string]int{}
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			due, err := dm2.CheckPendingWakes(now, []string{"*"})
			if err != nil {
				t.Errorf("concurrent check: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for id := range deliveredIDs(due) {
				delivered[id]++
			}
		}()
	}
	wg.Wait()

	require.Equal(t, 0, ownershipWakeFired(t, dm2, "wk-ownership-race-drill"),
		"a concurrent agent-facing check retired a scheduler-owned wake")
	require.Equal(t, 1, delivered["wk-ownership-race-notification"],
		"the notification wake must be delivered exactly once across %d concurrent callers", callers)
}

// TestCheckPendingWakes_SchedulerOwnedWakeSurvivesAgentFacingReadOperations
// pins that the agent's read surfaces neither retire nor reclassify a
// scheduler-owned wake.
//
// "Existing scheduler-owned rows must not be silently reclassified,
// deleted or retired." Listing and digesting are read-only; the row
// must come back exactly as it was, and must still be pending for the
// scheduler afterwards.
func TestCheckPendingWakes_SchedulerOwnedWakeSurvivesAgentFacingReadOperations(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const drillID = "wk-ownership-survives-drill"
	seedOwnershipWake(t, dm, drillID, now.Add(-time.Minute), schedulerOwnedKind)

	items, err := dm.ListScheduledWakes(false, false, 100)
	require.NoError(t, err)
	found := false
	for _, item := range items {
		if id, _ := item["id"].(string); id == drillID {
			found = true
		}
	}
	require.True(t, found, "the read surface must still list a scheduler-owned wake")

	digest, err := dm.DigestScheduledWakes(5)
	require.NoError(t, err)
	require.NotNil(t, digest, "the digest must still be produced")

	require.Equal(t, 0, ownershipWakeFired(t, dm, drillID),
		"a read surface retired a scheduler-owned wake")
	require.Equal(t, schedulerOwnedKind, ownershipWakeKind(t, dm, drillID),
		"a read surface reclassified a scheduler-owned wake")
}
