// wake_cron_delivery_test.go — the cron wake's delivery path.
//
// THE DEFECT THIS FILE PINS.
//
// A cron-kind wake is AGENT-OWNED: internal/core/wake_tools.go lists
// "cron" in AgentOwnedWakeKinds and says "the scheduler's ad-hoc claim
// excludes them (dispatchClaimNextAdHocWake) and the fold is their
// delivery path: the row carries the directive_id the agent has to
// consult". The producer agrees — internal/core/scheduled_tasks.go
// writes directive_id into the wake's metadata "so the agent can
// discover the directive_id at wake-time without an extra lookup", and
// FormatWakeNotification renders that directive_id into the
// <system_wake_notification> block the MCP fold prepends.
//
// But the DEFAULT branch of CheckPendingWakes — the no-kinds branch
// every CLI and MCP request runs — matched only notification wakes.
// So a cron wake was injected, became due, was invisible to every
// agent, never reached the notification block that exists to carry it,
// and was eventually retired by the cron retention sweep with its
// directive_id undelivered. The directive_id rendering was dead code.
//
// The default branch now filters to AgentOwnedWakeKinds, the same
// predicate as the wildcard branch.
//
// WHAT THESE TESTS DO NOT CLAIM.
//
// These tests pin the FOLD's behaviour. They do not claim anything
// about the scheduler's claim (that is internal/scheduler's file) and
// they do not claim that a cron wake survives the retention sweep —
// the sweep's window is the cron wake's delivery DEADLINE, and
// internal/scheduler/wake_cron_retention_interaction_test.go covers
// the interaction.
//
// Every test uses a disposable in-memory database (NewTestDM) and
// never touches the operator's.
package internal

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cronWakeMetadata is the metadata shape the scheduler's cron injection
// writes (internal/core/scheduled_tasks.go:243). It is reproduced
// exactly, including the key order, so the directive_id affordance is
// exercised against the real shape rather than an approximation.
const cronWakeMetadata = `{"kind":"cron","source":"cron","task_id":"epistemic-compaction","directive_id":"mpm-seed-epistemic-compaction"}`

// seedCronWake inserts a due cron-kind wake row directly.
//
// The row is inserted rather than scheduled so the fixture does not
// depend on the producer's kind policy: the point of these tests is
// what the FOLD does with a row that already exists.
func seedCronWake(t *testing.T, dm *DatabaseManager, id string, target time.Time) {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, 'mpm-scheduler', ?)`,
		id, target.Unix(), "cron:epistemic-compaction", cronWakeMetadata,
	)
	require.NoError(t, err, "seed cron wake %s", id)
}

// seedKindWake inserts a due wake row with an arbitrary kind.
func seedKindWake(t *testing.T, dm *DatabaseManager, id string, target time.Time, kind, metadata string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, 'test', ?)`,
		id, target.Unix(), "fixture "+id, metadata,
	)
	require.NoError(t, err, "seed wake %s", id)
}

// TestCheckPendingWakes_DefaultBranchDeliversACronWake is the headline
// regression.
//
// Pre-fix: the default (no-kinds) branch matched notification wakes
// only, so a cron wake was never delivered. The agent saw nothing, and
// the row sat pending until the retention sweep retired it.
func TestCheckPendingWakes_DefaultBranchDeliversACronWake(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const cronID = "wk-cron-default-delivery"
	seedCronWake(t, dm, cronID, now.Add(-time.Minute))

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)

	delivered := deliveredIDs(due)
	require.True(t, delivered[cronID],
		"the default fold must deliver a due cron wake: the cron wake is agent-owned "+
			"and the fold is its delivery path (AgentOwnedWakeKinds, scheduled_tasks.go)")
	require.Equal(t, 1, ownershipWakeFired(t, dm, cronID),
		"a delivered cron wake is retired, exactly like a notification wake")
}

// TestCheckPendingWakes_DefaultBranchDeliversTheCronDirectiveID pins
// the affordance that made the defect observable.
//
// The producer writes directive_id into the wake's metadata and
// FormatWakeNotification renders it into the <system_wake_notification>
// block. That rendering could never fire before this fix, because no
// cron wake could reach the notification block.
func TestCheckPendingWakes_DefaultBranchDeliversTheCronDirectiveID(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const cronID = "wk-cron-directive-id"
	seedCronWake(t, dm, cronID, now.Add(-time.Minute))

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, 1)

	meta, ok := due[0]["metadata"].(map[string]interface{})
	require.True(t, ok, "the delivered wake must carry a parsed metadata map")
	require.Equal(t, "cron", meta["kind"], "the delivered wake must keep its cron kind")
	require.Equal(t, "mpm-seed-epistemic-compaction", meta["directive_id"],
		"the delivered cron wake must carry the directive_id the agent has to consult")

	// And the notification block the MCP fold prepends must render it.
	block := FormatWakeNotification(due)
	require.Contains(t, block, "<system_wake_notification>",
		"the notification block must be rendered for a delivered cron wake")
	require.Contains(t, block, `directive_id="mpm-seed-epistemic-compaction"`,
		"the notification block must carry the cron wake's directive_id — this is the "+
			"affordance that was dead code while the default fold excluded cron wakes")
}

// TestCheckPendingWakes_DefaultBranchDeliversCronAndNotificationTogether
// pins coexistence: the two kinds must not shadow one another.
func TestCheckPendingWakes_DefaultBranchDeliversCronAndNotificationTogether(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const notifID = "wk-coexist-notification"
	const cronID = "wk-coexist-cron"
	const untaggedID = "wk-coexist-untagged"

	// The notification is the OLDEST, so the ordering assertion below is
	// about chronology, not about kind priority.
	seedKindWake(t, dm, notifID, now.Add(-3*time.Minute), "notification", `{"kind":"notification"}`)
	seedCronWake(t, dm, cronID, now.Add(-2*time.Minute))
	seedKindWake(t, dm, untaggedID, now.Add(-time.Minute), "", "{}")

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)

	delivered := deliveredIDs(due)
	require.True(t, delivered[notifID], "the notification wake must still be delivered")
	require.True(t, delivered[cronID], "the cron wake must be delivered in the same fold")
	require.True(t, delivered[untaggedID], "the untagged wake must still be delivered")
	require.Len(t, due, 3, "one fold must deliver all three agent-owned kinds")

	// Chronological order is preserved across the kind split.
	ids := make([]string, 0, len(due))
	for _, w := range due {
		ids = append(ids, w["id"].(string))
	}
	require.Equal(t, []string{notifID, cronID, untaggedID}, ids,
		"the fold must keep chronological order across kinds")

	for _, id := range []string{notifID, cronID, untaggedID} {
		require.Equal(t, 1, ownershipWakeFired(t, dm, id),
			"every delivered wake must be retired: %s", id)
	}
}

// TestCheckPendingWakes_DefaultBranchNeverDeliversSchedulerOwnedKinds
// is the ownership invariant, restated for the widened default.
//
// Widening the default branch must not cross the ownership boundary.
// The scheduler's kinds must still reach their handler, not the fold.
func TestCheckPendingWakes_DefaultBranchNeverDeliversSchedulerOwnedKinds(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const gcID = "wk-ownership-default-gc"
	const snapshotID = "wk-ownership-default-snapshot"
	const cascadeSummaryID = "wk-ownership-default-cascade-summary"

	seedKindWake(t, dm, gcID, now.Add(-time.Minute), "gc", `{"kind":"gc"}`)
	seedKindWake(t, dm, snapshotID, now.Add(-time.Minute), "snapshot", `{"kind":"snapshot"}`)
	seedKindWake(t, dm, cascadeSummaryID, now.Add(-time.Minute), "cascade_summary", `{"kind":"cascade_summary"}`)

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)

	delivered := deliveredIDs(due)
	for _, id := range []string{gcID, snapshotID, cascadeSummaryID} {
		require.False(t, delivered[id],
			"the default fold must not deliver the scheduler-owned wake %s: a fold that "+
				"retires it loses the work, because the handler never runs", id)
		require.Equal(t, 0, ownershipWakeFired(t, dm, id),
			"the default fold retired the scheduler-owned wake %s", id)
	}
}

// TestCheckPendingWakes_DefaultBranchDeliversACronWakeExactlyOnce pins
// the duplicate-prevention guarantee: the fold RETIRES what it
// delivers, so a second fold must not deliver the same row again.
func TestCheckPendingWakes_DefaultBranchDeliversACronWakeExactlyOnce(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const cronID = "wk-cron-exactly-once"
	seedCronWake(t, dm, cronID, now.Add(-time.Minute))

	first, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.True(t, deliveredIDs(first)[cronID])

	second, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Empty(t, second,
		"the second fold must not re-deliver a cron wake the first fold already retired")
	require.Equal(t, 1, ownershipWakeFired(t, dm, cronID),
		"the cron wake must be retired exactly once")
}

// TestCheckPendingWakes_ExplicitKindFiltersAreUnchangedByTheWidening
// pins that the explicit-filter branch is untouched.
//
// The default branch widened; the explicit branch did not. A request
// that names one kind must still see exactly that kind.
func TestCheckPendingWakes_ExplicitKindFiltersAreUnchangedByTheWidening(t *testing.T) {
	cases := []struct {
		name     string
		kinds    []string
		wantIDs  []string
		wantNone bool
	}{
		{
			name:    "explicit cron sees only cron",
			kinds:   []string{"cron"},
			wantIDs: []string{"wk-kinds-cron"},
		},
		{
			name:    "explicit notification sees only notification",
			kinds:   []string{"notification"},
			wantIDs: []string{"wk-kinds-notification"},
		},
		{
			name:    "wildcard sees every agent-owned kind",
			kinds:   []string{"*"},
			wantIDs: []string{"wk-kinds-cron", "wk-kinds-notification", "wk-kinds-untagged"},
		},
		{
			name:     "a scheduler-owned kind matches nothing",
			kinds:    []string{"gc"},
			wantNone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := NewTestDM(t)
			now := time.Now()

			seedCronWake(t, dm, "wk-kinds-cron", now.Add(-time.Minute))
			seedKindWake(t, dm, "wk-kinds-notification", now.Add(-time.Minute), "notification", `{"kind":"notification"}`)
			seedKindWake(t, dm, "wk-kinds-untagged", now.Add(-time.Minute), "", "{}")
			seedKindWake(t, dm, "wk-kinds-gc", now.Add(-time.Minute), "gc", `{"kind":"gc"}`)

			due, err := dm.CheckPendingWakes(now, tc.kinds)
			require.NoError(t, err)
			delivered := deliveredIDs(due)

			if tc.wantNone {
				require.Empty(t, due, "the request must match nothing")
				return
			}
			for _, id := range tc.wantIDs {
				require.True(t, delivered[id], "expected %s to be delivered", id)
			}
			require.Len(t, due, len(tc.wantIDs),
				"the explicit filter must not widen: delivered %v", delivered)
		})
	}
}

// TestCheckPendingWakes_DefaultBranchAppliesTheCascadeCap pins the
// per-check cap on cascade wakes.
//
// The cascade materializer documents "immediate delivery on next
// check_wakes" and "metadata tags the wake as a cascade delivery so
// check_wakes can apply its per-check delivery cap". check_wakes with
// no kinds argument uses the DEFAULT branch, so the cap has to be
// reachable from the default path — and the wakes beyond the cap must
// stay pending so a later call delivers them.
func TestCheckPendingWakes_DefaultBranchAppliesTheCascadeCap(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const total = MaxCascadeWakePerCheck + 2
	for i := 0; i < total; i++ {
		seedKindWake(t, dm, fmt.Sprintf("wk-cascade-cap-%02d", i), now.Add(-time.Minute),
			"cascade", fmt.Sprintf(`{"kind":"cascade","theory_id":"theory-%d"}`, i))
	}
	// A cron wake alongside the cascade backlog: the cap must not
	// starve it.
	seedCronWake(t, dm, "wk-cascade-cap-cron", now.Add(-time.Minute))

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)

	delivered := deliveredIDs(due)
	cascadeDelivered := 0
	for id := range delivered {
		if strings.HasPrefix(id, "wk-cascade-cap-") && id != "wk-cascade-cap-cron" {
			cascadeDelivered++
		}
	}
	require.Equal(t, MaxCascadeWakePerCheck, cascadeDelivered,
		"the default fold must cap cascade delivery at MaxCascadeWakePerCheck")
	require.True(t, delivered["wk-cascade-cap-cron"],
		"the cascade cap must not starve a cron wake")

	// The rows beyond the cap stay pending — they are not retired.
	var pending int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes
		 WHERE fired = 0 AND id LIKE 'wk-cascade-cap-%' AND id <> 'wk-cascade-cap-cron'`,
	).Scan(&pending))
	require.Equal(t, 2, pending,
		"the cascade wakes beyond the cap must remain pending for a later fold")

	// A second fold delivers the remainder.
	second, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, second, 2, "the second fold must deliver the capped remainder")
}

// TestCronWakeMetadataIsWhatTheProducerWrites guards the fixture
// against drift: the test metadata must keep the shape the real
// injection writes, including the directive_id the agent consults.
func TestCronWakeMetadataIsWhatTheProducerWrites(t *testing.T) {
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(cronWakeMetadata), &parsed),
		"the cron wake metadata fixture must be valid JSON")
	require.Equal(t, "cron", parsed["kind"])
	require.Equal(t, "cron", parsed["source"])
	require.Equal(t, "epistemic-compaction", parsed["task_id"])
	require.Equal(t, "mpm-seed-epistemic-compaction", parsed["directive_id"],
		"the producer writes directive_id into the wake metadata; the fixture must keep it")
}
