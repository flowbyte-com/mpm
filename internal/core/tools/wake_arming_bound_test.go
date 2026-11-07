// wake_arming_bound_test.go — the LENGTH gate at the agent-facing
// arming boundary.
//
// THE DEFECT THIS FILE PINS.
//
// The agent-facing scheduling surface (`mpm_wakes schedule`) took the
// caller's metadata verbatim. It validated the wake's KIND — the
// ownership boundary — but nothing about the SIZE of what it stored.
//
// The directive_id is the field that matters: it is the identifier the
// agent passes back to read_directives, and it is rendered into the
// wake notification block on EVERY tool response. An unbounded
// directive_id is therefore an unbounded agent-visible response, and
// one ordinary `mpm_wakes schedule` call was enough to arm it:
//
//	handleScheduleWake(metadata.directive_id = 102,400 bytes)
//	  -> accepted, 102,433 bytes of metadata stored
//	  -> the next tool call returns a 102,786-byte response
//
// The gate REFUSES rather than shortens. A shortened directive_id
// would name no directive: the agent would look it up and find
// nothing, which is a silent correctness failure dressed as a success.
//
// This is the PREVENTION layer. The delivery path bounds the value
// too (core.boundWakeMetadata), so a wake that reached the table by
// another route is still delivered inside a bounded response. Both
// layers are needed: the prevention layer cannot speak for rows that
// already exist.
//
// Every test uses a disposable database and never touches the
// operator's.
package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// armWakeWithMetadata calls the agent-facing scheduling surface with
// the given metadata, exactly as the CLI and the MCP server reach it.
func armWakeWithMetadata(t *testing.T, dm *internal.DatabaseManager, metadata map[string]interface{}) (interface{}, error) {
	t.Helper()
	return handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "schedule",
		"params": map[string]interface{}{
			"reason":      "arming probe",
			"target_time": "24h",
			"metadata":    metadata,
		},
	})
}

// TestScheduleWakeRefusesAnOversizedDirectiveID is the headline
// regression.
//
// Pre-fix: a 102,400-byte directive_id was accepted and stored.
func TestScheduleWakeRefusesAnOversizedDirectiveID(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	oversized := strings.Repeat("D", 100*1024)
	_, err := armWakeWithMetadata(t, dm, map[string]interface{}{
		"kind":         "cron",
		"directive_id": oversized,
	})
	require.Error(t, err, "the arming path must refuse a directive_id it cannot render")
	require.Contains(t, err.Error(), "directive_id")
	require.Contains(t, err.Error(), "NOT created",
		"the refusal must say that no wake was created")

	// Nothing was armed.
	require.Equal(t, 0, countWakes(t, dm),
		"a refused wake must not be persisted")
}

// TestScheduleWakeAcceptsADirectiveIDAtTheLimit pins the boundary.
func TestScheduleWakeAcceptsADirectiveIDAtTheLimit(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	atLimit := strings.Repeat("d", internal.MaxWakeDirectiveIDBytes)
	res, err := armWakeWithMetadata(t, dm, map[string]interface{}{
		"kind":         "cron",
		"directive_id": atLimit,
	})
	require.NoError(t, err, "a directive_id exactly at the limit must be armable")
	require.NotNil(t, res)

	m, _ := res.(map[string]interface{})
	require.NotEmpty(t, m["id"])
	require.Equal(t, 1, countWakes(t, dm))
}

// TestScheduleWakeAcceptsANormalDirectiveID pins that the gate does
// not touch the ordinary case.
func TestScheduleWakeAcceptsANormalDirectiveID(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	res, err := armWakeWithMetadata(t, dm, map[string]interface{}{
		"kind":         "cron",
		"directive_id": "mpm-seed-epistemic-compaction-policy",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 1, countWakes(t, dm))
}

// TestScheduleWakeRefusesANonStringDirectiveID pins that a directive
// id the agent could never use is refused at the same boundary.
//
// A non-string directive_id persists as a JSON value that neither the
// renderer nor read_directives can use: a wake nobody can act on is an
// orphan.
func TestScheduleWakeRefusesANonStringDirectiveID(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	_, err := armWakeWithMetadata(t, dm, map[string]interface{}{
		"kind":         "cron",
		"directive_id": 42,
	})
	require.Error(t, err, "a non-string directive_id must be refused")
	require.Contains(t, err.Error(), "directive_id")
	require.Equal(t, 0, countWakes(t, dm),
		"a refused wake must not be persisted")
}

// TestScheduleWakeAcceptsAMissingDirectiveID pins that the gate is
// about the VALUE, not the field's presence.
//
// A wake with no directive_id is an ordinary notification wake.
func TestScheduleWakeAcceptsAMissingDirectiveID(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	res, err := armWakeWithMetadata(t, dm, map[string]interface{}{
		"kind": "notification",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 1, countWakes(t, dm))
}

// TestScheduleWakeStillArmsSchedulerOwnedKindsForInternalProducers
// pins that the length gate does not reach the internal producers.
//
// The gate is at the AGENT-FACING handler, exactly like the kind
// gate: the DB-layer producer is shared with the scheduler's ingest
// handler, the cascade materializer, the wake reconcile loop and the
// cron task runner, and those must keep their capabilities.
func TestScheduleWakeStillArmsSchedulerOwnedKindsForInternalProducers(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	// The internal producer arms a scheduler-owned kind through the
	// DB layer, with a directive_id the agent-facing gate would refuse.
	oversized := strings.Repeat("i", 100*1024)
	_, err := dm.ScheduleWake("internal producer", "24h", "", "mpm-scheduler",
		map[string]interface{}{
			"kind":         "drill",
			"directive_id": oversized,
		})
	require.NoError(t, err,
		"the DB-layer producer must keep its capabilities: the length gate is at the agent-facing boundary")
	require.Equal(t, 1, countWakes(t, dm))
}

// TestTheArmingGateNamesTheLimit pins that the refusal is actionable.
func TestTheArmingGateNamesTheLimit(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	oversized := strings.Repeat("d", internal.MaxWakeDirectiveIDBytes+1)
	_, err := armWakeWithMetadata(t, dm, map[string]interface{}{
		"kind":         "cron",
		"directive_id": oversized,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("%d", internal.MaxWakeDirectiveIDBytes),
		"the refusal must name the limit so the caller can comply")
}
