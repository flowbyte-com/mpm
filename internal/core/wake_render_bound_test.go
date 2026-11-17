// wake_render_bound_test.go — the byte bound on the AGENT-VISIBLE
// wake payload.
//
// THE DEFECT THIS FILE PINS.
//
// The per-kind delivery budget measured the bytes of the rendered
// <system_wake_notification> block. A cap measured on a BATCH is
// only a bound on the RESPONSE if every member of the batch is itself
// bounded — and the members were not:
//
//   - the directive_id was rendered VERBATIM, so one wake could carry
//     a 200 KB directive_id and make the block fifty times its own
//     per-kind cap (measured: a 204,957-byte block against a
//     4,096-byte cap);
//   - the same wake's STRUCTURED row — the WakesPending payload the
//     core tool handlers and `mpm call` return — carried the
//     caller's metadata verbatim and was 205,020 bytes. Bounding only
//     the rendered block would NOT have closed the hole, because
//     `mpm call` returns the structured row;
//   - the agent-facing arming path accepted a 102,400-byte
//     directive_id, so one ordinary `mpm_wakes schedule` call was
//     enough to arm the oversized wake;
//   - the oversized row consumed the whole kind budget, so the
//     HEALTHY wakes behind it were not delivered in that call;
//   - the rendered reason was cut on a BYTE boundary, so a multi-byte
//     character could be split in half; the renderer then escaped the
//     orphan byte as \xNN, which mangles what the agent reads and
//     makes the byte cost unpredictable.
//
// THE FIX, IN ONE PARAGRAPH.
//
// Every rendered field is bounded in bytes and cut at a CHARACTER
// boundary; every caller-supplied string in a delivered row is
// bounded; the arming path refuses a directive_id too long to render;
// and the "first row is always delivered" exemption is gone, because
// a bounded row always fits.
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
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// plantWake inserts one pending wake with the given metadata.
func plantWake(t *testing.T, dm *DatabaseManager, id, reason, metadata string, age time.Duration) {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, 'mpm-scheduler', ?)`,
		id, time.Now().Add(-age).Unix(), reason, metadata)
	require.NoError(t, err, "plant wake %s", id)
}

// TestRenderedWakeRowIsBoundedForAnOversizedDirective is the headline
// regression.
//
// Pre-fix: one wake with a 200 KB directive_id rendered a 204,957-byte
// block — fifty times the per-kind cap.
func TestRenderedWakeRowIsBoundedForAnOversizedDirective(t *testing.T) {
	dm := NewTestDM(t)

	oversized := strings.Repeat("A", 200*1024)
	plantWake(t, dm, "wk-oversized-directive", "probe",
		fmt.Sprintf(`{"kind":"cron","directive_id":%q}`, oversized), time.Hour)

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.Len(t, due, 1, "the oversized wake must still be DELIVERED, not dropped")

	block := FormatWakeNotification(due)
	require.LessOrEqual(t, len(block), MaxWakeNotificationBytesPerKind,
		"the rendered block must respect the per-kind byte cap: %d bytes > %d",
		len(block), MaxWakeNotificationBytesPerKind)

	// The cut is VISIBLE. The agent must be able to tell that the
	// directive_id it was handed is not the whole one.
	require.Contains(t, block, "truncated",
		"a directive_id that was cut must say so: the agent passes this value back to read_directives")
	require.Contains(t, block, "204800 bytes",
		"the marker must name the original byte length, so the agent knows how much was withheld")
}

// TestTheRenderedBlockIsBoundedPerKind pins that NO kind can exceed
// its own byte cap — the property the exemption used to break.
func TestTheRenderedBlockIsBoundedPerKind(t *testing.T) {
	dm := NewTestDM(t)

	oversized := strings.Repeat("B", 300*1024)
	// One oversized wake of EVERY agent-owned kind, plus healthy rows
	// behind each of them.
	for _, kind := range AgentOwnedWakeKinds {
		for i := 0; i < 4; i++ {
			plantWake(t, dm, fmt.Sprintf("wk-big-%s-%d", kind, i), "probe",
				fmt.Sprintf(`{"kind":%q,"directive_id":%q}`, kind, oversized), time.Hour)
		}
	}

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, due)

	block := FormatWakeNotification(due)
	require.LessOrEqual(t, len(block), 3*MaxWakeNotificationBytesPerKind,
		"the rendered block must respect the sum of the per-kind caps: %d bytes", len(block))

	// Each kind's own contribution is within its own cap.
	perKind := map[string]int{}
	for _, w := range due {
		kind := "notification"
		if meta, ok := w["metadata"].(map[string]interface{}); ok {
			if k, ok := meta["kind"].(string); ok && k != "" {
				kind = k
			}
		}
		perKind[kind] += len(formatWakeNotificationRow(w))
	}
	for kind, size := range perKind {
		require.LessOrEqual(t, size, MaxWakeNotificationBytesPerKind,
			"kind %q contributed %d bytes, over its own %d-byte cap",
			kind, size, MaxWakeNotificationBytesPerKind)
	}
}

// TestAnOversizedWakeDoesNotStarveTheWakesBehindIt pins the property
// the exemption used to break.
//
// Pre-fix: the oversized row consumed the whole kind budget, so the
// five healthy wakes behind it were NOT delivered in that call.
func TestAnOversizedWakeDoesNotStarveTheWakesBehindIt(t *testing.T) {
	dm := NewTestDM(t)

	oversized := strings.Repeat("C", 200*1024)
	plantWake(t, dm, "wk-big-head", "oversized",
		fmt.Sprintf(`{"kind":"cron","directive_id":%q}`, oversized), 2*time.Hour)
	for i := 0; i < 5; i++ {
		plantWake(t, dm, fmt.Sprintf("wk-healthy-%02d", i), "healthy",
			`{"kind":"cron","directive_id":"mpm-seed-small"}`, time.Hour)
	}

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.Len(t, due, 6,
		"the oversized wake must not starve the healthy wakes behind it: all six fit in one call")

	ids := make([]string, 0, len(due))
	for _, w := range due {
		ids = append(ids, fmt.Sprintf("%v", w["id"]))
	}
	for i := 0; i < 5; i++ {
		require.Contains(t, ids, fmt.Sprintf("wk-healthy-%02d", i),
			"the healthy wake %d must be delivered in the SAME call as the oversized one", i)
	}
}

// TestTheTruncationDoesNotSplitACharacter pins the UTF-8 property.
//
// Pre-fix: the reason was cut on a byte boundary, so a multi-byte
// character was split and the renderer escaped the orphan byte as
// \xNN — the agent read a mangled character, and the line's byte cost
// was unpredictable.
func TestTheTruncationDoesNotSplitACharacter(t *testing.T) {
	dm := NewTestDM(t)

	// 100 snowmen: 300 bytes, 100 characters. The cut at 80 bytes
	// lands in the middle of a snowman.
	reason := strings.Repeat("☃", 100)
	plantWake(t, dm, "wk-utf8", reason, `{"kind":"cron"}`, time.Hour)

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.Len(t, due, 1)

	block := FormatWakeNotification(due)
	require.True(t, utf8.ValidString(block),
		"the rendered block must be valid UTF-8")
	require.NotContains(t, block, `\x`,
		"the renderer must not escape an orphan byte: that is what a split character looks like")

	// The reason the agent reads must be whole characters.
	_, rendered, ok := strings.Cut(block, `reason="`)
	require.True(t, ok, "the rendered block must carry the reason")
	value, _, _ := strings.Cut(rendered, `"`)
	require.True(t, utf8.ValidString(value),
		"the rendered reason must be valid UTF-8")
	for _, r := range value {
		require.True(t, r == '☃' || r == '…',
			"the rendered reason must carry whole characters, got %q", r)
	}
}

// TestTheDeliveredRowIsBounded pins the STRUCTURED payload.
//
// This is a different surface from the rendered block, with a
// different bound. Pre-fix, the same oversized wake produced a
// 205,020-byte structured row: the delivered row carried the caller's
// metadata verbatim, so `mpm call` returned an unbounded response even
// if the rendered block had been bounded.
func TestTheDeliveredRowIsBounded(t *testing.T) {
	dm := NewTestDM(t)

	oversized := strings.Repeat("D", 200*1024)
	plantWake(t, dm, "wk-oversized-structured", "probe",
		fmt.Sprintf(`{"kind":"cron","directive_id":%q}`, oversized), time.Hour)

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.Len(t, due, 1)

	encoded, err := json.Marshal(due[0])
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 2048,
		"the structured delivered row must be bounded: %d bytes", len(encoded))

	meta, ok := due[0]["metadata"].(map[string]interface{})
	require.True(t, ok)
	id, _ := meta["directive_id"].(string)
	require.LessOrEqual(t, len(id), MaxWakeDeliveredStringBytes,
		"the delivered directive_id must be bounded: %d bytes", len(id))
	require.Contains(t, id, "truncated",
		"a delivered directive_id that was cut must say so")
}

// TestTheStoredRowIsNotModified pins that the bound is on the DELIVERED
// view, not on the source of truth.
//
// The persisted row keeps the caller's value in full. An operator (or
// an agent, through `mpm_wakes list`) must still be able to read what
// was actually armed; the delivery bound is a delivery bound.
func TestTheStoredRowIsNotModified(t *testing.T) {
	dm := NewTestDM(t)

	oversized := strings.Repeat("E", 100*1024)
	plantWake(t, dm, "wk-stored-intact", "probe",
		fmt.Sprintf(`{"kind":"cron","directive_id":%q}`, oversized), time.Hour)

	var stored string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT metadata FROM scheduled_wakes WHERE id = ?`, "wk-stored-intact").Scan(&stored))
	require.Contains(t, stored, oversized[:1024],
		"the persisted metadata must keep the caller's value in full")
	require.Greater(t, len(stored), 100*1024,
		"the persisted metadata must not be truncated at the source")

	_, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
}

// TestAnOversizedWakeIsNeitherDeletedNorSilentlySettled pins the
// handling of a wake that was ALREADY PERSISTED oversized.
//
// It must be DELIVERED (the obligation is honoured, not dropped), it
// must not be DELETED (the row is the audit trail), and the cut must
// be VISIBLE in both surfaces (the agent is not silently handed a
// value it cannot use).
func TestAnOversizedWakeIsNeitherDeletedNorSilentlySettled(t *testing.T) {
	dm := NewTestDM(t)

	oversized := strings.Repeat("F", 100*1024)
	plantWake(t, dm, "wk-legacy-oversized", "legacy",
		fmt.Sprintf(`{"kind":"cron","directive_id":%q}`, oversized), time.Hour)

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.Len(t, due, 1, "an already-persisted oversized wake must still be delivered")

	// The row is still there — not deleted.
	var count int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE id = ?`, "wk-legacy-oversized").Scan(&count))
	require.Equal(t, 1, count, "the wake row must not be deleted")

	// It was delivered, so it was settled — and the settled set is
	// exactly what was delivered.
	var fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1`).Scan(&fired))
	require.Equal(t, len(due), fired,
		"the fold must settle exactly the wakes it delivered")

	// The cut is visible in the structured payload too.
	meta, ok := due[0]["metadata"].(map[string]interface{})
	require.True(t, ok)
	id, _ := meta["directive_id"].(string)
	require.Contains(t, id, "truncated",
		"the structured payload must say that the directive_id was cut")

	// And in the rendered block.
	require.Contains(t, FormatWakeNotification(due), "truncated",
		"the rendered block must say that the directive_id was cut")
}

// TestAnOversizedWakeIsNotLost pins the throttle-not-drop property
// for the oversized case: the wake is delivered (bounded), and
// everything behind it drains too.
func TestAnOversizedWakeIsNotLost(t *testing.T) {
	dm := NewTestDM(t)

	oversized := strings.Repeat("G", 100*1024)
	for i := 0; i < 40; i++ {
		plantWake(t, dm, fmt.Sprintf("wk-drain-%02d", i), "cron:epistemic-compaction",
			fmt.Sprintf(`{"kind":"cron","directive_id":%q}`, oversized), time.Hour)
	}

	delivered := 0
	for call := 0; call < 10; call++ {
		due, err := dm.CheckPendingWakes(time.Now(), nil)
		require.NoError(t, err)
		delivered += len(due)
		block := FormatWakeNotification(due)
		require.LessOrEqual(t, len(block), 3*MaxWakeNotificationBytesPerKind,
			"call %d: the rendered block must stay bounded: %d bytes", call, len(block))
		if delivered >= 40 {
			break
		}
	}
	require.Equal(t, 40, delivered,
		"every oversized wake must eventually be delivered: the budget throttles, it never drops")
}

// TestABoundedDirectiveIDIsDeliveredUnchanged pins that the bound does
// not touch a value that fits.
//
// A directive id that fits is delivered EXACTLY, in both surfaces: the
// agent's lookup seam must keep working.
func TestABoundedDirectiveIDIsDeliveredUnchanged(t *testing.T) {
	dm := NewTestDM(t)

	const directiveID = "mpm-seed-epistemic-compaction-policy"
	plantWake(t, dm, "wk-normal", "cron:epistemic-compaction",
		fmt.Sprintf(`{"kind":"cron","source":"cron","task_id":"epistemic-compaction","directive_id":%q}`,
			directiveID), time.Hour)

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.Len(t, due, 1)

	meta, ok := due[0]["metadata"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, directiveID, meta["directive_id"],
		"a directive_id that fits must be delivered unchanged")
	require.Equal(t, "cron", meta["kind"],
		"the kind must be delivered unchanged")
	require.Equal(t, "cron", meta["source"],
		"the other metadata keys must survive the projection")

	block := FormatWakeNotification(due)
	require.Contains(t, block, fmt.Sprintf("directive_id=%q", directiveID),
		"the rendered block must carry the whole directive_id")
	require.NotContains(t, block, "truncated",
		"a directive_id that fits must not be marked as cut")
}

// TestTheDeliveredRowKeepsTheRealMetadataShape pins that the metadata
// projection does not disturb the production shapes.
func TestTheDeliveredRowKeepsTheRealMetadataShape(t *testing.T) {
	dm := NewTestDM(t)

	plantWake(t, dm, "wk-shape-cron", "cron:epistemic-compaction",
		`{"kind":"cron","source":"cron","task_id":"epistemic-compaction",`+
			`"directive_id":"mpm-seed-epistemic-compaction-policy"}`, time.Hour)
	plantWake(t, dm, "wk-shape-cascade", "cascade:foundation",
		`{"kind":"cascade","theory_id":"7383f1572d73bc9a",`+
			`"invalidation_event_id":"evt-1"}`, time.Hour)

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.Len(t, due, 2)

	for _, w := range due {
		meta, ok := w["metadata"].(map[string]interface{})
		require.True(t, ok)
		require.NotEmpty(t, meta["kind"])
		encoded, err := json.Marshal(meta)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), MaxWakeDeliveredMetadataBytes,
			"the delivered metadata document must be within its bound: %d bytes", len(encoded))
	}
}

// TestTheMetadataProjectionIsDeterministic pins that two deliveries of
// the same row produce the same projection.
//
// The projection drops keys to fit a byte budget, so the ORDER it
// picks them in has to be fixed — otherwise the same wake would
// render differently from call to call.
func TestTheMetadataProjectionIsDeterministic(t *testing.T) {
	meta := map[string]interface{}{
		"kind":         "cron",
		"a_key":        "a",
		"b_key":        "b",
		"c_key":        "c",
		"d_key":        "d",
		"e_key":        "e",
		"f_key":        "f",
		"g_key":        "g",
		"h_key":        "h",
		"i_key":        "i",
		"j_key":        "j",
		"k_key":        "k",
		"l_key":        "l",
		"m_key":        "m",
		"n_key":        "n",
		"o_key":        "o",
		"p_key":        "q",
		"directive_id": "mpm-seed-x",
	}
	first := boundWakeMetadata(meta)
	second := boundWakeMetadata(meta)
	require.Equal(t, first, second, "the projection must be deterministic")

	// The protected keys survive, and the omission is visible.
	require.Equal(t, "cron", first["kind"])
	require.Equal(t, "mpm-seed-x", first["directive_id"])
	require.Contains(t, first, "_mpm_meta_truncated",
		"an omitted key must be visible, not silently lost")
}

// TestTheMetadataProjectionStaysWithinItsBoundInTheWorstCase pins
// the metadata bound against its WORST case.
//
// The projection reserves room for the truncation marker, and the
// marker's own length grows with the number of omitted keys — so the
// reservation has to hold for every count, not just for a small one.
func TestTheMetadataProjectionStaysWithinItsBoundInTheWorstCase(t *testing.T) {
	meta := map[string]interface{}{
		"kind":         "cron",
		"directive_id": "mpm-seed-x",
	}
	for i := 0; i < 200; i++ {
		meta[fmt.Sprintf("key_%03d", i)] = strings.Repeat("v", 4096)
	}

	projected := boundWakeMetadata(meta)
	encoded, err := json.Marshal(projected)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), MaxWakeDeliveredMetadataBytes,
		"the delivered metadata document must be within its bound in the worst case: %d bytes > %d",
		len(encoded), MaxWakeDeliveredMetadataBytes)

	// The protected keys survive, and the omission is visible.
	require.Equal(t, "cron", projected["kind"])
	require.Equal(t, "mpm-seed-x", projected["directive_id"])
	require.Contains(t, projected, "_mpm_meta_truncated")
}

// TestTheRenderedRowAlwaysFitsTheKindCap pins the invariant the
// removed exemption used to be about: a single rendered row can never
// exceed a kind's byte cap, so the budget always admits at least one
// row and can never starve a kind.
func TestTheRenderedRowAlwaysFitsTheKindCap(t *testing.T) {
	worst := map[string]interface{}{
		"id":           strings.Repeat("i", 4096),
		"reason":       strings.Repeat("r", 4096),
		"theory_id":    strings.Repeat("t", 4096),
		"created_by":   strings.Repeat("c", 4096),
		"overdue_secs": int64(1 << 40),
		"metadata": map[string]interface{}{
			"kind":         strings.Repeat("k", 4096),
			"directive_id": strings.Repeat("d", 4096),
		},
	}
	rendered := formatWakeNotificationRow(worst)
	require.LessOrEqual(t, len(rendered), MaxWakeNotificationBytesPerKind,
		"a single rendered row must always fit inside a kind's cap: %d bytes > %d",
		len(rendered), MaxWakeNotificationBytesPerKind)
}
