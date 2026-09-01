// engine_scheduled_e2e_test.go — end-to-end test of the
// epistemic-compaction scheduled reflex path. The test exercises
// the full chain without bypassing any layer:
//
//   1. seed.ApplyDirectives  →  mpm-seed-epistemic-compaction-policy row
//   2. dm.SeedBaselineScheduledTasks  →  epistemic-compaction task
//   3. Force the task due (backdate next_run_at to now-1s)
//   4. core.ProcessScheduledTasks  →  injects a scheduled_wakes row
//                                       with metadata.directive_id set
//   5. dm.CheckPendingWakes  →  returns the wake with metadata
//   6. core.FormatWakeNotification  →  renders the line with the
//                                        directive_id field
//
// This pins the contract that the agent sees the directive name in
// the wake notification — the agent does not need a follow-up
// lookup to discover which prime directive to consult. The actual
// compact invocation is left to the agent (verified separately by
// the existing compact_drain_test.go suite); this test confirms
// the substrate / scheduler / wake-folding contract is intact
// end-to-end.
package seed_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/seed"
)

// TestEpistemicCompactionEndToEnd runs the full reflex path. The
// test deliberately does NOT call mpm_system.compact directly —
// that would bypass the directive path, which is the architectural
// contract this mission is productizing.
func TestEpistemicCompactionEndToEnd(t *testing.T) {
	dm := newTestDM(t)

	// 1. Seed the directive + task baseline. Production boot order.
	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	tSummary, err := dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)
	require.Equal(t, len(seed.SeedScheduledTasks), len(tSummary.Created))

	// 2. Force the task due: backdate next_run_at to now-1s. The
	// daemon does this on every tick; in this test we shortcut by
	// updating the row directly. The task's cron / directive_id /
	// status are untouched — operator customization preserved.
	_, err = dm.SQLDB().Exec(`
		UPDATE scheduled_tasks
		SET next_run_at = CAST(strftime('%s','now') AS INTEGER) - 1
		WHERE id = ?`, "epistemic-compaction")
	require.NoError(t, err)

	// 3. Process the due tasks. This is what the daemon's 60s tick
	// does — it walks scheduled_tasks for due rows, injects a
	// scheduled_wakes row, and rolls over next_run_at. One
	// transaction. Daemon crash between injection and rollover
	// cannot double-fire.
	processed, err := core.ProcessScheduledTasks(dm.SQLDB())
	require.NoError(t, err)
	require.Equal(t, 1, processed, "exactly one task should have fired")

	// 4. Inspect the injected wake. It must carry the directive_id
	// in metadata so the agent knows which prime directive to
	// consult.
	var wakeID, wakeReason, wakeMetadata string
	row := dm.SQLDB().QueryRow(`
		SELECT id, reason, metadata
		FROM scheduled_wakes
		WHERE reason = 'cron:epistemic-compaction'
		ORDER BY created_at DESC LIMIT 1`)
	require.NoError(t, row.Scan(&wakeID, &wakeReason, &wakeMetadata))
	require.NotEmpty(t, wakeID, "wake id must be assigned")
	require.Equal(t, "cron:epistemic-compaction", wakeReason)
	require.Contains(t, wakeMetadata, `"directive_id":"mpm-seed-epistemic-compaction-policy"`,
		"injected wake must carry the directive_id in metadata, got %q", wakeMetadata)

	// 5. CheckPendingWakes returns the wake. Pass kinds=["cron"]
	// so the surface returns cron-injected wakes. The CLI / MCP
	// paths both use the same surface.
	due, err := dm.CheckPendingWakes(time.Now(), []string{"cron"})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(due), 1, "at least one cron wake must be due")

	// 6. Render the wake notification. The rendered line must
	// include the directive_id so the agent doesn't have to do a
	// follow-up lookup to discover the prime directive.
	rendered := core.FormatWakeNotification(due)
	require.True(t, strings.Contains(rendered, `<system_wake_notification>`),
		"expected XML wrapper, got %q", rendered)
	require.True(t, strings.Contains(rendered, `directive_id="mpm-seed-epistemic-compaction-policy"`),
		"rendered wake must include the directive_id, got %q", rendered)
	require.True(t, strings.Contains(rendered, "epistemic-compaction"),
		"rendered wake must include the task id, got %q", rendered)

	// 7. Sanity: the next-run-at was rolled over to a future
	// instant (NOT 0, NOT still in the past).
	var nextRun int64
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT next_run_at FROM scheduled_tasks WHERE id = ?`,
		"epistemic-compaction",
	).Scan(&nextRun))
	require.Greater(t, nextRun, time.Now().Unix()-1,
		"next_run_at must be rolled over to a future instant, got %d", nextRun)
}
