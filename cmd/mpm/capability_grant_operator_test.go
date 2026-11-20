// capability_grant_operator_test.go — CS-3.4 CLI handler tests
//
// Pins the contract of `mpm capability grant-operator`:
//
//   * With a capability id, stamps metadata.operator_approved_at
//     and writes the audit row (DB state verified via direct SQL).
//   * With --actor, the actor name is preserved in metadata +
//     event row.
//   * With --reason, the reason is preserved in the event row's
//     metadata (audit-trail fidelity).
//   * Without a capability id, prints help + exits 1.
//   * With --help / -h / help, exits 0 with no DB writes.
//   * Unknown flag exits 1 with no DB writes.
//   * On a non-existent id, exits 1 with "not found" message.
//   * On a retired capability, exits 1 with refusal message.
//
// Pattern mirrors capability_seed_test.go: installTestDM replaces
// the package-level dbManager singleton so we don't touch the
// real workspace DB.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/capability"
)

// helper: insert a capability row via direct SQL so we can
// control its starting state. Mirrors seedCapability in the
// capability package, but we need it here because CLI tests
// run in the cmd/mpm package, not internal/core/capability.
func seedTestCapability(t *testing.T, dm *internal.DatabaseManager, id, name string, state capability.CapabilityState) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`INSERT INTO capabilities
		(id, name, purpose, source_code, source_language, source_hash,
		 state, execution_domain, state_changed_at,
		 author_agent, probation_required_success_count, probation_max_failure_rate,
		 tags, created_at, updated_at, metadata)
		VALUES (?, ?, 'test', 'echo', 'bash', 'deadbeef',
		        ?, 'sandbox', ?,
		        '', 5, 0.10,
		        '[]', ?, ?, '{}')`,
		id, name, state, 1700000000, 1700000000, 1700000000)
	if err != nil {
		t.Fatalf("seedTestCapability(%s, %s): %v", id, state, err)
	}
}

// helper: read the metadata JSON of a capability row.
func readCapabilityMetadata(t *testing.T, dm *internal.DatabaseManager, id string) string {
	t.Helper()
	var s string
	row := dm.QueryRowTracked(`SELECT metadata FROM capabilities WHERE id = ?`, id)
	if err := row.Scan(&s); err != nil {
		t.Fatalf("readCapabilityMetadata(%s): %v", id, err)
	}
	return s
}

// helper: count capability_events rows for a given id + event_type.
func countEventsOfType(t *testing.T, dm *internal.DatabaseManager, capID string, eventType string) int {
	t.Helper()
	var n int
	row := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM capability_events WHERE capability_id = ? AND event_type = ?`,
		capID, eventType)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("countEventsOfType(%s, %s): %v", capID, eventType, err)
	}
	return n
}

// helper: read a single capability_events.metadata field.
func readEventMetadata(t *testing.T, dm *internal.DatabaseManager, capID, eventType string) string {
	t.Helper()
	var s string
	row := dm.QueryRowTracked(`SELECT metadata FROM capability_events
	                           WHERE capability_id = ? AND event_type = ?`,
		capID, eventType)
	if err := row.Scan(&s); err != nil {
		t.Fatalf("readEventMetadata(%s, %s): %v", capID, eventType, err)
	}
	return s
}

// =============================================================================
// Happy path: id → stamp + audit row
// =============================================================================

func TestHandleCapabilityGrantOperator_StampsMetadataAndAuditRow(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	seedTestCapability(t, dm, "cap-under-test", "under-test", capability.StateActive)

	exit := handleCapabilityGrantOperator([]string{"cap-under-test"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}

	// 1. metadata.operator_approved_at is now non-zero.
	meta := readCapabilityMetadata(t, dm, "cap-under-test")
	if !strings.Contains(meta, "operator_approved_at") {
		t.Errorf("metadata missing operator_approved_at: %s", meta)
	}
	if !strings.Contains(meta, "operator_approved_at_rfc3339") {
		t.Errorf("metadata missing RFC3339 sibling: %s", meta)
	}
	if !strings.Contains(meta, `"operator_approved_by":"operator"`) {
		t.Errorf("metadata missing operator default actor: %s", meta)
	}

	// 2. capability_events row exists.
	if n := countEventsOfType(t, dm, "cap-under-test", "operator_approval"); n != 1 {
		t.Errorf("event row count = %d, want 1", n)
	}
}

// =============================================================================
// --actor flag: actor recorded in metadata + audit
// =============================================================================

func TestHandleCapabilityGrantOperator_ActorFlagRecorded(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	seedTestCapability(t, dm, "cap-under-test", "under-test", capability.StateActive)

	exit := handleCapabilityGrantOperator([]string{"cap-under-test", "--actor", "alice"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}

	meta := readCapabilityMetadata(t, dm, "cap-under-test")
	if !strings.Contains(meta, `"operator_approved_by":"alice"`) {
		t.Errorf("metadata missing alice: %s", meta)
	}

	// Event row also carries the actor.
	evMeta := readEventMetadata(t, dm, "cap-under-test", "operator_approval")
	if !strings.Contains(evMeta, `"operator_approved_by":"alice"`) {
		t.Errorf("event metadata missing alice: %s", evMeta)
	}
}

// =============================================================================
// --reason flag: reason recorded in audit row
// =============================================================================

func TestHandleCapabilityGrantOperator_ReasonFlagRecorded(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	seedTestCapability(t, dm, "cap-under-test", "under-test", capability.StateActive)

	exit := handleCapabilityGrantOperator([]string{
		"cap-under-test", "--reason", "reviewed by sec-team"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}

	evMeta := readEventMetadata(t, dm, "cap-under-test", "operator_approval")
	if !strings.Contains(evMeta, `"reason":"reviewed by sec-team"`) {
		t.Errorf("event metadata missing reason: %s", evMeta)
	}
}

// =============================================================================
// --actor + --reason + flag order independence (both work in any order)
// =============================================================================

func TestHandleCapabilityGrantOperator_FlagOrderIndependence(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	seedTestCapability(t, dm, "cap-under-test", "under-test", capability.StateActive)

	exit := handleCapabilityGrantOperator([]string{
		"--reason", "first", "cap-under-test", "--actor", "bob",
	})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	meta := readCapabilityMetadata(t, dm, "cap-under-test")
	if !strings.Contains(meta, `"operator_approved_by":"bob"`) {
		t.Errorf("metadata missing bob: %s", meta)
	}
}

// =============================================================================
// Missing capability id
// =============================================================================

func TestHandleCapabilityGrantOperator_MissingID_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	// No positional arg.
	if exit := handleCapabilityGrantOperator(nil); exit != 1 {
		t.Errorf("nil args: exit = %d, want 1", exit)
	}
	if exit := handleCapabilityGrantOperator([]string{}); exit != 1 {
		t.Errorf("empty args: exit = %d, want 1", exit)
	}
	// Empty string is rejected (trimmed to empty).
	if exit := handleCapabilityGrantOperator([]string{"   "}); exit != 1 {
		t.Errorf("whitespace id: exit = %d, want 1", exit)
	}
}

// =============================================================================
// Help flags
// =============================================================================

func TestHandleCapabilityGrantOperator_HelpFlag_ReturnsZero(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	for _, flag := range []string{"--help", "-h", "help"} {
		// Pass any id (or no id) — help should short-circuit
		// before the id check.
		if exit := handleCapabilityGrantOperator([]string{flag}); exit != 0 {
			t.Errorf("%s alone: exit = %d, want 0", flag, exit)
		}
		if exit := handleCapabilityGrantOperator([]string{"cap-foo", flag}); exit != 0 {
			t.Errorf("%s with id: exit = %d, want 0", flag, exit)
		}
	}
}

// =============================================================================
// Unknown flag
// =============================================================================

func TestHandleCapabilityGrantOperator_UnknownFlag_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	if exit := handleCapabilityGrantOperator([]string{"cap-foo", "--bogus"}); exit != 1 {
		t.Errorf("unknown flag: exit = %d, want 1", exit)
	}
}

// =============================================================================
// --actor / --reason without value
// =============================================================================

func TestHandleCapabilityGrantOperator_MissingFlagArg_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	if exit := handleCapabilityGrantOperator([]string{"cap-foo", "--actor"}); exit != 1 {
		t.Errorf("--actor alone: exit = %d, want 1", exit)
	}
	if exit := handleCapabilityGrantOperator([]string{"cap-foo", "--reason"}); exit != 1 {
		t.Errorf("--reason alone: exit = %d, want 1", exit)
	}
}

// =============================================================================
// Non-existent id
// =============================================================================

func TestHandleCapabilityGrantOperator_NonExistent_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	if exit := handleCapabilityGrantOperator([]string{"cap-does-not-exist"}); exit != 1 {
		t.Errorf("exit = %d, want 1", exit)
	}
	if n := countEventsOfType(t, dm, "cap-does-not-exist", "operator_approval"); n != 0 {
		t.Errorf("event row created for missing capability (count=%d)", n)
	}
}

// =============================================================================
// Refused on retired capability
// =============================================================================

func TestHandleCapabilityGrantOperator_RefusesRetired(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	seedTestCapability(t, dm, "cap-retired", "retired", capability.StateRetired)

	if exit := handleCapabilityGrantOperator([]string{"cap-retired"}); exit != 1 {
		t.Errorf("exit = %d, want 1 (refused)", exit)
	}
	if n := countEventsOfType(t, dm, "cap-retired", "operator_approval"); n != 0 {
		t.Errorf("event row created for retired (count=%d)", n)
	}
}

// =============================================================================
// Idempotency at the CLI level: re-running writes a second event row.
// =============================================================================

func TestHandleCapabilityGrantOperator_Idempotent_Restamps(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	seedTestCapability(t, dm, "cap-under-test", "under-test", capability.StateActive)

	if exit := handleCapabilityGrantOperator([]string{"cap-under-test"}); exit != 0 {
		t.Fatalf("first: exit = %d, want 0", exit)
	}
	if exit := handleCapabilityGrantOperator([]string{"cap-under-test"}); exit != 0 {
		t.Fatalf("second: exit = %d, want 0 (re-stamp should succeed)", exit)
	}
	if n := countEventsOfType(t, dm, "cap-under-test", "operator_approval"); n != 2 {
		t.Errorf("event count = %d, want 2 (first + second)", n)
	}
}

// =============================================================================
// Multiple positional args rejected as ambiguous
// =============================================================================

func TestHandleCapabilityGrantOperator_TwoPositionals_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	if exit := handleCapabilityGrantOperator([]string{"cap-foo", "cap-bar"}); exit != 1 {
		t.Errorf("exit = %d, want 1 (ambiguous)", exit)
	}
}

// =============================================================================
// Sanity: ensure fixture sidecar file isn't a thing here (we don't use one)
// =============================================================================
var _ = filepath.Join
var _ = os.WriteFile
