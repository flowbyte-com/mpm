// wake_delivery_budget_test.go — the fold's per-call delivery budget.
//
// THE DEFECT THIS FILE PINS.
//
// The opportunistic fold had no per-call bound. Its SELECT carried no
// LIMIT, FormatWakeNotification rendered every delivered wake, and the
// MCP adapter prepended that block to every tool response AFTER the
// output-policy spill decision — so the spill mechanism did not bound
// the wake block either.
//
// The consequence was an unbounded agent-visible response: 10,080
// pending cron wakes produced a ~1 MB <system_wake_notification>
// block. With the seeded one-minute cron task and the seeded triage
// directive ("alert the user IMMEDIATELY" for overdue_secs <= 86400),
// a returning agent was handed that many identical instructions.
//
// The budget has two halves, and BOTH are pinned here:
//
//   - a ROW cap per kind, so the scan, the settlement and the
//     WakesPending envelope are bounded; and
//   - a BYTE cap per kind on the RENDERED block, because a row cap
//     alone does not bound the output (the directive_id is rendered
//     verbatim).
//
// The budget is PER KIND so that one kind's backlog cannot starve
// another, and it THROTTLES rather than drops: a wake that does not
// fit stays pending and is delivered by a later call.
//
// Every test uses a disposable in-memory database (NewTestDM) and
// never touches the operator's.
package internal

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// wakeBlockOverheadAllowance is the slack the measured block size is
// compared against on top of the three per-kind byte caps.
//
// The per-kind byte cap is a TRUE bound: every rendered field is
// itself bounded, so a single rendered row can never exceed the cap
// and the cap is never exceeded at all. The allowance is the block's
// fixed wrapper — the opening marker, the "Scheduled tasks now due:"
// line and the closing marker — which is not charged to any kind.
const wakeBlockOverheadAllowance = 1024

// seedCronBacklog inserts n due cron-kind wakes, oldest first.
func seedCronBacklog(t *testing.T, dm *DatabaseManager, n int, age time.Duration) {
	t.Helper()
	now := time.Now()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("wk-budget-cron-%06d", i)
		target := now.Add(-age).Add(time.Duration(i) * time.Second)
		_, err := dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'mpm-scheduler', ?)`,
			id, target.Unix(), "cron:epistemic-compaction",
			`{"kind":"cron","source":"cron","task_id":"epistemic-compaction",`+
				`"directive_id":"mpm-seed-epistemic-compaction"}`,
		)
		require.NoError(t, err, "seed cron wake %s", id)
	}
}

// pendingCronCount counts the pending cron-kind wakes.
func pendingCronCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 0 AND `+
			`(json_extract(metadata, '$.kind') = 'cron')`,
	).Scan(&n))
	return n
}

// firedCount counts the settled wakes.
func firedCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1`,
	).Scan(&n))
	return n
}

// TestCheckPendingWakes_BoundsTheDeliveredBatchPerKind is the
// headline regression.
//
// Pre-fix: the whole backlog was delivered in one call and the block
// grew without bound.
func TestCheckPendingWakes_BoundsTheDeliveredBatchPerKind(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const backlog = 10_080
	seedCronBacklog(t, dm, backlog, 7*24*time.Hour)

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, MaxCronWakePerCheck,
		"the fold must deliver at most MaxCronWakePerCheck cron wakes per call")

	// The agent-visible block is bounded.
	block := FormatWakeNotification(due)
	limit := 3*MaxWakeNotificationBytesPerKind + wakeBlockOverheadAllowance
	require.LessOrEqual(t, len(block), limit,
		"the rendered wake block must be bounded by the delivery budget: %d bytes > %d",
		len(block), limit)

	// The reviewer's measurement was ~1 MB for this backlog. The
	// budget must be orders of magnitude below that.
	require.Less(t, len(block), 64*1024,
		"the wake block must stay small: %d bytes", len(block))

	// Nothing was settled beyond what was delivered.
	require.Equal(t, len(due), firedCount(t, dm),
		"the fold must settle exactly the wakes it delivered")
	require.Equal(t, backlog-MaxCronWakePerCheck, pendingCronCount(t, dm),
		"the excluded wakes must stay pending for a later call")
}

// TestCheckPendingWakes_ExcludedWakesAreDeliveredByLaterCalls pins
// that the budget THROTTLES rather than DROPS.
//
// A wake that did not fit the budget is still eligible: it is still
// pending, and a later call delivers it. Nothing is lost by the
// budget, and no wake is settled without being delivered.
func TestCheckPendingWakes_ExcludedWakesAreDeliveredByLaterCalls(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const backlog = 100
	seedCronBacklog(t, dm, backlog, time.Hour)

	delivered := 0
	calls := 0
	for delivered < backlog {
		due, err := dm.CheckPendingWakes(now, nil)
		require.NoError(t, err)
		require.LessOrEqual(t, len(due), MaxCronWakePerCheck,
			"call %d delivered %d wakes, over the cap", calls, len(due))
		delivered += len(due)
		calls++
		require.Less(t, calls, 100, "the backlog must drain")
	}

	require.Equal(t, backlog, delivered, "every eligible wake must be delivered")
	require.Equal(t, 0, pendingCronCount(t, dm))
	require.Equal(t, backlog, firedCount(t, dm),
		"exactly the delivered wakes are settled — the budget must not settle a wake "+
			"it did not deliver")
	require.Greater(t, calls, 1,
		"a backlog over the cap must take more than one call to drain")
}

// TestCheckPendingWakes_MixedKindsFairnessAndProgress pins the
// fairness property.
//
// A large cron backlog must not starve a notification wake, and a
// large notification backlog must not starve a cron wake. Every kind
// has its own share, so every kind makes progress on every call.
func TestCheckPendingWakes_MixedKindsFairnessAndProgress(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const per = 40
	for i := 0; i < per; i++ {
		_, err := dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'test', ?)`,
			fmt.Sprintf("wk-mix-notif-%03d", i), now.Add(-time.Hour).Unix(),
			"a notification", `{"kind":"notification"}`,
		)
		require.NoError(t, err)
		_, err = dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'mpm-scheduler', ?)`,
			fmt.Sprintf("wk-mix-cron-%03d", i), now.Add(-time.Hour).Unix(),
			"cron:epistemic-compaction",
			`{"kind":"cron","source":"cron","task_id":"epistemic-compaction",`+
				`"directive_id":"mpm-seed-epistemic-compaction"}`,
		)
		require.NoError(t, err)
		_, err = dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'cascade-materializer', ?)`,
			fmt.Sprintf("wk-mix-cascade-%03d", i), now.Add(-time.Hour).Unix(),
			"cascade: theory th-mix requires review",
			fmt.Sprintf(`{"kind":"cascade","theory_id":"th-mix-%d"}`, i),
		)
		require.NoError(t, err)
	}

	// First call: every kind must be represented.
	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)

	counts := map[string]int{}
	for _, w := range due {
		meta, _ := w["metadata"].(map[string]interface{})
		kind, _ := meta["kind"].(string)
		if kind == "" {
			kind = "notification"
		}
		counts[kind]++
	}
	require.Equal(t, MaxNotificationWakePerCheck, counts["notification"],
		"a cron backlog must not starve the notification kind")
	require.Equal(t, MaxCronWakePerCheck, counts["cron"],
		"a notification backlog must not starve the cron kind")
	require.Equal(t, MaxCascadeWakePerCheck, counts["cascade"],
		"the cascade cap is unchanged")

	// Every kind makes progress across the drain.
	seen := map[string]int{}
	total := len(due)
	for call := 0; call < 20 && total < 3*per; call++ {
		due, err = dm.CheckPendingWakes(now, nil)
		require.NoError(t, err)
		for _, w := range due {
			meta, _ := w["metadata"].(map[string]interface{})
			kind, _ := meta["kind"].(string)
			if kind == "" {
				kind = "notification"
			}
			seen[kind]++
		}
		total += len(due)
	}
	require.Equal(t, 3*per, total, "every eligible wake must eventually be delivered")
	require.Greater(t, seen["notification"], 0, "the notification kind must keep progressing")
	require.Greater(t, seen["cron"], 0, "the cron kind must keep progressing")
	require.Greater(t, seen["cascade"], 0, "the cascade kind must keep progressing")
}

// TestCheckPendingWakes_CascadeCapIsPreserved pins that the existing
// cascade cap still holds on the default path.
func TestCheckPendingWakes_CascadeCapIsPreserved(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const total = MaxCascadeWakePerCheck + 5
	for i := 0; i < total; i++ {
		_, err := dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'cascade-materializer', ?)`,
			fmt.Sprintf("wk-cap-cascade-%02d", i), now.Add(-time.Minute).Unix(),
			"cascade: theory th-cap requires review",
			fmt.Sprintf(`{"kind":"cascade","theory_id":"th-cap-%d"}`, i),
		)
		require.NoError(t, err)
	}

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, MaxCascadeWakePerCheck,
		"the cascade cap must be preserved: %d delivered, cap %d",
		len(due), MaxCascadeWakePerCheck)

	// The OLDEST cascade wakes are the ones delivered — the cap
	// truncates the correct end.
	ids := make([]string, 0, len(due))
	for _, w := range due {
		ids = append(ids, w["id"].(string))
	}
	require.Equal(t, "wk-cap-cascade-00", ids[0])
	require.Equal(t, fmt.Sprintf("wk-cap-cascade-%02d", MaxCascadeWakePerCheck-1), ids[len(ids)-1])

	// The remainder stays pending and is delivered later — three at a
	// time, because the cascade cap is a PER-CALL cap.
	require.Equal(t, 5, pendingCascadeCount(t, dm))
	second, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, second, MaxCascadeWakePerCheck,
		"the capped remainder must be delivered by a later call, three at a time")
	third, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, third, 2, "the last of the cascade remainder must be delivered too")
	require.Equal(t, 0, pendingCascadeCount(t, dm))
}

// pendingCascadeCount counts the pending cascade-kind wakes.
func pendingCascadeCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 0 AND `+
			`(json_extract(metadata, '$.kind') = 'cascade')`,
	).Scan(&n))
	return n
}

// TestCheckPendingWakes_CascadeClassificationIsCanonical pins the
// LOW-3 finding.
//
// The cascade cap used to classify a row with an ad-hoc byte match
// (`strings.Contains(metadata, `"kind":"cascade"`)`), so the SAME
// JSON written with a space after the colon escaped the cap. Every
// other kind test in the substrate reads the kind through the
// canonical JSON-safe expression, and the cap is now the only bound on
// cascade delivery, so it must too.
func TestCheckPendingWakes_CascadeClassificationIsCanonical(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const total = MaxCascadeWakePerCheck + 2
	for i := 0; i < total; i++ {
		// The spaced form: valid JSON, same semantics, different bytes.
		_, err := dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'cascade-materializer', ?)`,
			fmt.Sprintf("wk-spaced-cascade-%02d", i), now.Add(-time.Minute).Unix(),
			"cascade: theory th-spaced requires review",
			fmt.Sprintf(`{"kind": "cascade", "theory_id": "th-spaced-%d"}`, i),
		)
		require.NoError(t, err)
	}

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, MaxCascadeWakePerCheck,
		"the cascade cap must classify the row the same way whatever the JSON "+
			"whitespace: a spaced `\"kind\": \"cascade\"` must not escape the cap")
}

// TestCheckPendingWakes_ByteBudgetBoundsAnOversizedRow pins the byte
// half of the budget.
//
// The rendered line's directive_id is not length-bounded — it is an
// identifier the agent has to use, so truncating it would render it
// unusable. A row cap therefore cannot bound the block on its own,
// and the budget measures the bytes the agent actually receives.
//
// The oversized row is still DELIVERED (the budget throttles the
// batch; it never makes a wake undeliverable), and it is delivered
// ALONE: a kind's byte usage may exceed its cap by at most its first
// row, and once that row has consumed the allowance the rows behind
// it wait for the next call. They are not lost — they stay pending
// and the next call delivers them — and the agent's response stays
// bounded by ONE oversized row rather than by one per delivered wake.
func TestCheckPendingWakes_ByteBudgetBoundsAnOversizedRow(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const hugeDirective = 64 * 1024
	hugeDirectiveID := strings.Repeat("d", hugeDirective)
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES ('wk-budget-huge', ?, 'cron:huge', 0, 'mpm-scheduler', ?)`,
		now.Add(-time.Minute).Unix(),
		fmt.Sprintf(`{"kind":"cron","source":"cron","directive_id":%q}`, hugeDirectiveID),
	)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		_, err := dm.SQLDB().Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'mpm-scheduler', ?)`,
			fmt.Sprintf("wk-budget-small-%02d", i), now.Add(-time.Minute).Unix(),
			"cron:small",
			`{"kind":"cron","source":"cron","directive_id":"mpm-seed-epistemic-compaction"}`,
		)
		require.NoError(t, err)
	}

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)

	// The oversized row is DELIVERED — the budget throttles the batch,
	// it never makes a wake undeliverable.
	ids := map[string]bool{}
	for _, w := range due {
		ids[w["id"].(string)] = true
	}
	require.True(t, ids["wk-budget-huge"],
		"the oversized row must still be delivered: the budget throttles the batch, it "+
			"never makes a wake undeliverable")

	// AND THE ROWS BEHIND IT ARE DELIVERED IN THE SAME CALL.
	//
	// This is the property the field bounds bought. Before them, the
	// oversized row consumed its kind's whole allowance, so the ten
	// rows behind it were not delivered until the next call — one
	// call of starvation, produced by the very row the budget was
	// supposed to throttle. Now every rendered field is bounded, so
	// the oversized row costs only its BOUNDED size against the
	// budget and the healthy rows behind it fit alongside it.
	require.Len(t, due, 11,
		"the oversized row must not starve the ten rows behind it: they fit in the same call")
	for i := 0; i < 10; i++ {
		require.True(t, ids[fmt.Sprintf("wk-budget-small-%02d", i)],
			"the healthy row %d must be delivered in the same call as the oversized one", i)
	}

	// The block is BOUNDED — the oversized row does not make it
	// oversized, because the directive_id it carries is bounded.
	block := FormatWakeNotification(due)
	require.LessOrEqual(t, len(block), MaxWakeNotificationBytesPerKind+wakeBlockOverheadAllowance,
		"the block must respect the per-kind byte cap: %d bytes", len(block))

	// The cut is VISIBLE: the agent can tell the directive_id it was
	// handed is not the whole one.
	require.Contains(t, block, "truncated",
		"a directive_id that was cut must say so")
}

// TestCheckPendingWakes_AgentInactiveReceivesABoundedBurst pins the
// behaviour an agent sees when it returns after an absence.
//
// One one-minute cron task produces ~60 wakes per hour, so an agent
// that has been away for an hour faces that pool. The budget must
// bound the burst, and the rest must stay deliverable.
func TestCheckPendingWakes_AgentInactiveReceivesABoundedBurst(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	// One hour of one-minute production.
	seedCronBacklog(t, dm, 60, time.Hour)

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, MaxCronWakePerCheck,
		"the returning agent must receive a bounded burst")

	block := FormatWakeNotification(due)
	require.LessOrEqual(t, len(block), MaxWakeNotificationBytesPerKind+wakeBlockOverheadAllowance,
		"the burst must fit the cron kind's byte budget: %d bytes", len(block))

	// The rest stays eligible.
	require.Equal(t, 60-MaxCronWakePerCheck, pendingCronCount(t, dm))

	// A second call delivers the next slice.
	second, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, second, MaxCronWakePerCheck)
}

// TestCheckPendingWakes_ExplicitKindFilterRespectsTheBudget pins
// that the budget applies to the explicit filter too.
//
// `kinds=["cron"]` selects only cron wakes — but it must not select
// an unbounded number of them.
func TestCheckPendingWakes_ExplicitKindFilterRespectsTheBudget(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	seedCronBacklog(t, dm, 500, time.Hour)

	due, err := dm.CheckPendingWakes(now, []string{"cron"})
	require.NoError(t, err)
	require.Len(t, due, MaxCronWakePerCheck,
		"an explicit kinds filter must respect the per-call budget")
	for _, w := range due {
		meta, _ := w["metadata"].(map[string]interface{})
		require.Equal(t, "cron", meta["kind"],
			"an explicit kinds filter must still select only the named kind")
	}
}
