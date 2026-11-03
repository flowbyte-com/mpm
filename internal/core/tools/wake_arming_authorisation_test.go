// wake_arming_authorisation_test.go — the agent-facing ARMING boundary.
//
// `mpm_wakes schedule` is the one surface through which an agent can
// put a row into the wake queue. Every kind it can name becomes a row
// the scheduler may later dispatch through a registered handler.
//
// Before this gate, that surface had no kind allowlist at all: an
// agent could arm
//
//	{"action":"schedule","params":{"reason":"…","target_time":"1m",
//	 "metadata":{"kind":"drill","path":"/tmp/drill.yaml"}}}
//
// and the scheduler would execute it as a behavioural drill; the same
// with kind=broadcast would fan a memory out to other agents. Both
// are expensive or externally consequential operations, and both were
// reachable from an ordinary, untrusted tool call.
//
// The boundary is the OWNERSHIP boundary that the fold already uses
// (internal/core/wake_tools.go AgentOwnedWakeKinds): the agent owns
// notification, cascade and cron; every other kind belongs to the
// scheduler. An agent may arm what it owns and nothing else.
//
// The gate is at the AGENT-FACING HANDLER, not in ScheduleWake. The
// DB-layer producer is shared with the internal producers — the
// scheduler's ingest handler, the cascade materializer, the wake
// reconcile loop, the cron task runner — and those must keep their
// capabilities. TestScheduleWake_StillArmsSchedulerOwnedKindsForInternalProducers
// pins that half.
//
// Every test uses a disposable database and never touches the
// operator's.
package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// schedulerOwnedKindsForArming are the kinds the scheduler dispatches
// through a registered handler. The agent may not arm any of them.
var schedulerOwnedKindsForArming = []string{
	"drill",
	"broadcast",
	"snapshot",
	"gc",
	"critic_audit",
	"cascade_summary",
}

// armWake calls the agent-facing scheduling surface exactly as the CLI
// and the MCP server reach it: through the tool registry, with the
// {action, params} envelope.
func armWake(t *testing.T, dm *internal.DatabaseManager, kind interface{}, omitKind bool) (interface{}, error) {
	t.Helper()
	params := map[string]interface{}{
		"reason":      "arming probe",
		"target_time": "24h",
	}
	if !omitKind {
		meta := map[string]interface{}{}
		if kind != nil {
			meta["kind"] = kind
		}
		params["metadata"] = meta
	}
	return handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "schedule",
		"params": params,
	})
}

// countWakes counts every row in scheduled_wakes.
func countWakes(t *testing.T, dm *internal.DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`).Scan(&n))
	return n
}

// countWakesOfKind counts the rows whose persisted metadata.kind is the
// given kind.
func countWakesOfKind(t *testing.T, dm *internal.DatabaseManager, kind string) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE json_extract(metadata, '$.kind') = ?`, kind,
	).Scan(&n))
	return n
}

// TestHandleScheduleWake_RefusesSchedulerOwnedKinds is the headline
// regression.
//
// The agent-facing surface must not arm a wake the scheduler owns. The
// refusal is total: no row is persisted, so the scheduler has nothing
// to dispatch.
func TestHandleScheduleWake_RefusesSchedulerOwnedKinds(t *testing.T) {
	for _, kind := range schedulerOwnedKindsForArming {
		t.Run(kind, func(t *testing.T) {
			dm := newTestSharedDM(t)
			before := countWakes(t, dm)

			out, err := armWake(t, dm, kind, false)
			require.Error(t, err,
				"the agent-facing schedule surface must refuse the scheduler-owned kind %q", kind)
			require.Nil(t, out,
				"a refused arming must not return a success receipt")

			// The error must NAME the kind, so the agent can
			// self-correct without reading the source.
			require.Contains(t, err.Error(), kind,
				"the refusal must name the refused kind")

			require.Equal(t, before, countWakes(t, dm),
				"a refused arming must not persist a wake row")
			require.Equal(t, 0, countWakesOfKind(t, dm, kind),
				"a refused arming must not persist a %q wake", kind)
		})
	}
}

// TestHandleScheduleWake_AllowsAgentOwnedKinds is the negative
// control.
//
// A gate that refused everything would pass every refusal test above.
// This is the test that says the gate is selective: the agent's own
// kinds still arm, and the row lands with the kind it asked for.
func TestHandleScheduleWake_AllowsAgentOwnedKinds(t *testing.T) {
	for _, kind := range []string{"notification", "cascade", "cron"} {
		t.Run(kind, func(t *testing.T) {
			dm := newTestSharedDM(t)

			out, err := armWake(t, dm, kind, false)
			require.NoError(t, err,
				"the agent must still be able to arm its own kind %q", kind)
			require.NotNil(t, out)

			require.Equal(t, 1, countWakesOfKind(t, dm, kind),
				"the arming must persist exactly one %q wake", kind)
		})
	}
}

// TestHandleScheduleWake_UntaggedWakeStillArms pins the most common
// path: no kind at all.
//
// ScheduleWake authors kind=notification when the caller supplies
// none, so an untagged request is an ordinary notification wake and
// must not be caught by the gate.
func TestHandleScheduleWake_UntaggedWakeStillArms(t *testing.T) {
	dm := newTestSharedDM(t)

	out, err := armWake(t, dm, nil, true)
	require.NoError(t, err,
		"an untagged wake is the notification default and must still arm")
	require.NotNil(t, out)

	require.Equal(t, 1, countWakes(t, dm), "the untagged wake must persist")
	require.Equal(t, 1, countWakesOfKind(t, dm, "notification"),
		"the untagged wake must be persisted with the canonical notification default")
}

// TestHandleScheduleWake_NullAndEmptyKindAreNotificationKinds pins the
// two "no kind" spellings that the rest of the substrate already reads
// as untagged.
//
// json_extract yields NULL for a JSON null, and the fold's matcher
// reads both NULL and the empty string as untagged — that is, as a
// notification wake. Refusing them would break a path that already
// works.
func TestHandleScheduleWake_NullAndEmptyKindAreNotificationKinds(t *testing.T) {
	for name, kind := range map[string]interface{}{
		"null":  nil,
		"empty": "",
	} {
		t.Run(name, func(t *testing.T) {
			dm := newTestSharedDM(t)

			// The nil spelling has to be built explicitly: a nil
			// interface value is indistinguishable from an absent
			// key in a Go map.
			params := map[string]interface{}{
				"reason":      "null-kind probe",
				"target_time": "24h",
				"metadata":    map[string]interface{}{"kind": kind},
			}
			_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
				"action": "schedule",
				"params": params,
			})
			require.NoError(t, err,
				"a %s kind is an untagged notification wake and must still arm", name)
			require.Equal(t, 1, countWakes(t, dm))
		})
	}
}

// TestHandleScheduleWake_RefusesUnknownKind pins the fail-closed
// direction.
//
// A kind this tree does not know is not the agent's. It may be a
// system kind a newer binary introduces, a typo, or an attempt to
// smuggle a wake past the boundary. Either way the agent surface must
// refuse it rather than arm it.
func TestHandleScheduleWake_RefusesUnknownKind(t *testing.T) {
	dm := newTestSharedDM(t)
	before := countWakes(t, dm)

	out, err := armWake(t, dm, "kind_this_tree_does_not_know", false)
	require.Error(t, err,
		"an unknown wake kind must not be armable from the agent surface")
	require.Nil(t, out)
	require.Contains(t, err.Error(), "kind_this_tree_does_not_know",
		"the refusal must name the refused kind")
	require.Equal(t, before, countWakes(t, dm),
		"a refused unknown kind must not persist a wake row")
}

// TestHandleScheduleWake_RefusesMalformedKind pins the malformed
// spelling.
//
// A kind that is present but not a string persists as a non-string
// JSON value. Neither the fold's kind matcher nor the scheduler's
// handler registry recognises it, so the row is an orphan: nobody
// delivers it and nobody dispatches it. Refusing it at the boundary
// is better than arming a wake that can never be acted on.
func TestHandleScheduleWake_RefusesMalformedKind(t *testing.T) {
	malformed := map[string]interface{}{
		"number": float64(42),
		"bool":   true,
		"array":  []interface{}{"notification"},
		"object": map[string]interface{}{"kind": "notification"},
	}

	for name, kind := range malformed {
		t.Run(name, func(t *testing.T) {
			dm := newTestSharedDM(t)
			before := countWakes(t, dm)

			out, err := armWake(t, dm, kind, false)
			require.Error(t, err,
				"a non-string wake kind must not be armable from the agent surface")
			require.Nil(t, out)
			require.Contains(t, err.Error(), "metadata.kind",
				"the refusal must name the offending field")
			require.Equal(t, before, countWakes(t, dm),
				"a refused malformed kind must not persist a wake row")
		})
	}
}

// TestScheduleWake_StillArmsSchedulerOwnedKindsForInternalProducers
// pins WHERE the gate lives.
//
// The DB-layer producer is shared with the internal producers, and
// those keep their capabilities: the scheduler's ingest handler, the
// cascade materializer, the wake reconcile loop and the cron task
// runner all arm through ScheduleWake. A gate placed in the producer
// would break them.
//
// This is the negative control for the refusals above: the refusal is
// a property of the AGENT SURFACE, not of the wake table.
func TestScheduleWake_StillArmsSchedulerOwnedKindsForInternalProducers(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, kind := range []string{"snapshot", "gc", "drill", "broadcast"} {
		out, err := dm.ScheduleWake("internal arm "+kind, "24h", "", "internal-producer",
			map[string]interface{}{"kind": kind})
		require.NoError(t, err,
			"an internal producer must still be able to arm the %q kind", kind)
		require.NotNil(t, out)
		require.Equal(t, 1, countWakesOfKind(t, dm, kind),
			"the internal %q wake must persist", kind)
	}
}

// TestHandleScheduleWake_RefusalLeavesExistingSystemWakesUntouched
// pins that a refused arming changes nothing else.
//
// "Existing scheduler-owned rows must not be silently reclassified,
// deleted or retired." A refused arming must not touch a row that is
// already there: it does not delete it, does not retire it, and does
// not rewrite its kind.
func TestHandleScheduleWake_RefusalLeavesExistingSystemWakesUntouched(t *testing.T) {
	dm := newTestSharedDM(t)

	// An existing scheduler-owned row, armed by an internal producer.
	receipt, err := dm.ScheduleWake("existing drill", "24h", "", "internal-producer",
		map[string]interface{}{"kind": "drill"})
	require.NoError(t, err)
	existingID, _ := receipt["id"].(string)
	require.NotEmpty(t, existingID, "the fixture wake must have an id")
	require.Equal(t, 1, countWakesOfKind(t, dm, "drill"),
		"the fixture must be the only drill row")

	// The agent tries to arm another one.
	_, err = armWake(t, dm, "drill", false)
	require.Error(t, err)

	// The existing row must be byte-for-byte what it was.
	var fired int
	var kind string
	var createdBy string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired, json_extract(metadata, '$.kind'), created_by FROM scheduled_wakes WHERE id = ?`,
		existingID,
	).Scan(&fired, &kind, &createdBy))
	require.Equal(t, 0, fired, "a refused arming retired an existing system wake")
	require.Equal(t, "drill", kind, "a refused arming reclassified an existing system wake")
	require.Equal(t, "internal-producer", createdBy,
		"a refused arming rewrote an existing system wake")
	require.Equal(t, 1, countWakesOfKind(t, dm, "drill"),
		"a refused arming must not add a second drill row")
}

// TestHandleScheduleWake_ConcurrentArmingAttemptsCannotArmASystemKind
// pins the concurrency contract.
//
// The scheduling surface is reachable from every session and every
// agent. Concurrent attempts to arm a scheduler-owned kind must all
// fail, and must leave no row behind for the scheduler to dispatch.
func TestHandleScheduleWake_ConcurrentArmingAttemptsCannotArmASystemKind(t *testing.T) {
	dm := newTestSharedDM(t)

	const callers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded int
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := armWake(t, dm, "drill", false)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			}
		}()
	}
	wg.Wait()

	require.Equal(t, 0, succeeded,
		"no concurrent agent-facing attempt may arm a scheduler-owned kind")
	require.Equal(t, 0, countWakes(t, dm),
		"a refused concurrent arming must leave no wake row behind")
}

// TestHandleScheduleWake_RefusalMessageNamesTheAgentOwnedKinds pins
// the error's usefulness.
//
// The refusal has to tell the caller what it MAY arm, or the agent
// cannot self-correct — it would just retry with the same kind.
func TestHandleScheduleWake_RefusalMessageNamesTheAgentOwnedKinds(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := armWake(t, dm, "drill", false)
	require.Error(t, err)

	msg := err.Error()
	for _, owned := range internal.AgentOwnedWakeKinds {
		require.True(t, strings.Contains(msg, owned),
			"the refusal must name the armable kind %q; got: %v", owned, msg)
	}
	require.Contains(t, msg, "scheduler",
		"the refusal must say why the kind is refused; got: %v", msg)
}

// TestHandleScheduleWake_ArmedNotificationWakeIsStillDelivered pins the
// end-to-end notification path.
//
// Arming a wake the agent owns must still result in delivery: the
// gate must not arm the row and then hide it from the fold.
func TestHandleScheduleWake_ArmedNotificationWakeIsStillDelivered(t *testing.T) {
	dm := newTestSharedDM(t)

	out, err := armWake(t, dm, "notification", false)
	require.NoError(t, err)
	require.NotNil(t, out, "the receipt must not be nil")

	// Bring the wake due, then run the fold the way the CLI and the
	// MCP server do.
	var wakeID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id FROM scheduled_wakes WHERE json_extract(metadata, '$.kind') = 'notification'`,
	).Scan(&wakeID))
	_, err = dm.SQLDB().Exec(`UPDATE scheduled_wakes SET target_time = ? WHERE id = ?`,
		time.Now().Add(-time.Minute).Unix(), wakeID)
	require.NoError(t, err)

	due, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)

	delivered := false
	for _, w := range due {
		if id, _ := w["id"].(string); id == wakeID {
			delivered = true
		}
	}
	require.True(t, delivered,
		"an armed notification wake must still be delivered by the fold")
}

// TestHandleScheduleWake_ArmedWakePersistsItsKind pins the persisted
// shape: the kind the caller asked for is the kind that is stored, so
// the ownership check on the read side sees the same value the write
// side authorised.
func TestHandleScheduleWake_ArmedWakePersistsItsKind(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := armWake(t, dm, "cascade", false)
	require.NoError(t, err)

	var raw string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COALESCE(metadata,'') FROM scheduled_wakes WHERE json_extract(metadata, '$.kind') = 'cascade'`,
	).Scan(&raw))

	var meta map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(raw), &meta))
	require.Equal(t, "cascade", meta["kind"])
}
