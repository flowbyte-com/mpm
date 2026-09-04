// f5_list_wakes_kinds_filter_regression_test.go — regression guard for
// the 2026-09-04 residual-inventory finding F-5.
//
// F-5 P2: mpm_wakes list advertised `kinds: string[]` in its JSON Schema
// (registry_list.go:351) but handleListWakes silently discarded the
// parameter — every agent that filtered on kinds got the unfiltered
// result set. Pre-fix the schema and handler were out of contract: a
// correctly-built agent using `kinds=["notification"]` would receive
// cascade, gc, and broadcast wakes too.
//
// The fix routes `kinds` through readKindsParam (already used by
// handleCheckWakes for the same schema field) and post-filters the
// ListScheduledWakes result set by metadata.kind. The same semantics
// as CheckPendingWakes are preserved: nil/empty → no filter
// (backward-compat default in CheckPendingWakes is "notification only",
// but for handleListWakes the absence of `kinds` historically meant
// "all wakes" — see TestHandleListWakes_DefaultsAndFilters, which
// counts 2 with no kinds and would break if we changed the default).
package tools

import (
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// scheduleWakeWithKind schedules a future wake with the given metadata.kind
// value. handleScheduleWake passes metadata through to ScheduleWake, which
// stores it as a JSON column; ListScheduledWakes reads it back via
// json_extract on the wake row. This is the same shape the production
// code path uses (cascade wakes, gc wakes, etc. set metadata.kind).
func scheduleWakeWithKind(t *testing.T, dm *internal.DatabaseManager, reason string, kind string) {
	t.Helper()
	future := time.Now().Add(2 * time.Hour).Unix()
	if _, err := handleScheduleWake(dm, internal.ActiveContext{}, map[string]interface{}{
		"reason":      reason,
		"target_time": time.Unix(future, 0).UTC().Format(time.RFC3339),
		"metadata":    map[string]interface{}{"kind": kind},
	}); err != nil {
		t.Fatalf("schedule %s: %v", reason, err)
	}
}

// TestF5_ListWakes_KindsFilterApplied is the primary regression guard.
// Pre-fix: kinds=["notification"] returned all 3 wakes (silently
// discarding the filter). Post-fix: only the notification-kind wake
// is returned.
func TestF5_ListWakes_KindsFilterApplied(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	scheduleWakeWithKind(t, dm, "sched-cascade", "cascade")
	scheduleWakeWithKind(t, dm, "sched-notification", "notification")
	scheduleWakeWithKind(t, dm, "sched-gc", "gc")

	// Filter to notification only.
	res, err := handleListWakes(dm, internal.ActiveContext{}, map[string]interface{}{
		"kinds": []interface{}{"notification"},
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})

	count, _ := m["count"].(int)
	require.Equal(t, 1, count,
		"kinds=[\"notification\"] must return 1 wake, got %d (pre-fix this returns all 3)", count)

	wakes, ok := m["wakes"].([]map[string]interface{})
	require.True(t, ok)
	require.Len(t, wakes, 1,
		"kinds=[\"notification\"] must surface exactly one wake")
	require.Equal(t, "sched-notification", wakes[0]["reason"],
		"the returned wake must be the notification-kind one, got reason=%q", wakes[0]["reason"])
}

// TestF5_ListWakes_MultipleKindsMatch verifies that kinds=["notification","gc"]
// returns both notification-kind and gc-kind wakes (logical OR semantics).
func TestF5_ListWakes_MultipleKindsMatch(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	scheduleWakeWithKind(t, dm, "sched-cascade", "cascade")
	scheduleWakeWithKind(t, dm, "sched-notification", "notification")
	scheduleWakeWithKind(t, dm, "sched-gc", "gc")

	res, err := handleListWakes(dm, internal.ActiveContext{}, map[string]interface{}{
		"kinds": []interface{}{"notification", "gc"},
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})

	count, _ := m["count"].(int)
	require.Equal(t, 2, count,
		"kinds=[\"notification\",\"gc\"] must return 2 wakes, got %d", count)

	wakes, _ := m["wakes"].([]map[string]interface{})
	seen := map[string]bool{}
	for _, w := range wakes {
		seen[w["reason"].(string)] = true
	}
	require.True(t, seen["sched-notification"], "notification wake missing from filtered list")
	require.True(t, seen["sched-gc"], "gc wake missing from filtered list")
	require.False(t, seen["sched-cascade"], "cascade wake leaked into non-cascade filter")
}

// TestF5_ListWakes_KindsAbsentReturnsAll is the backward-compat guard:
// when `kinds` is absent, the handler must return ALL wakes (no
// backward-incompatible default). TestHandleListWakes_DefaultsAndFilters
// already pins this behaviour for the no-kinds case (count=2), but we
// restate it here with kinds explicitly populated on every row so the
// difference between "no filter" and "filter that matches all" is
// unambiguous.
func TestF5_ListWakes_KindsAbsentReturnsAll(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	scheduleWakeWithKind(t, dm, "sched-cascade", "cascade")
	scheduleWakeWithKind(t, dm, "sched-notification", "notification")
	scheduleWakeWithKind(t, dm, "sched-gc", "gc")

	// No kinds key at all → unfiltered.
	res, err := handleListWakes(dm, internal.ActiveContext{}, map[string]interface{}{})
	require.NoError(t, err)
	m := res.(map[string]interface{})

	count, _ := m["count"].(int)
	require.Equal(t, 3, count,
		"absent kinds must return all 3 wakes (no backward-incompatible default), got %d", count)
}

// TestF5_ListWakes_ResponseEchoesKinds confirms the response payload
// includes the applied kinds filter (echoes what the agent sent) so
// downstream consumers can confirm the filter took effect.
func TestF5_ListWakes_ResponseEchoesKinds(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	scheduleWakeWithKind(t, dm, "sched-notification", "notification")

	res, err := handleListWakes(dm, internal.ActiveContext{}, map[string]interface{}{
		"kinds": []interface{}{"notification"},
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})

	raw, ok := m["kinds"]
	require.True(t, ok,
		"response must echo kinds so agent can confirm filter was applied (pre-fix the field is absent)")
	require.NotNil(t, raw,
		"response kinds field must not be nil when kinds was specified")
}
