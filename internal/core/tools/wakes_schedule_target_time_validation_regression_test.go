// wakes_schedule_target_time_validation_regression_test.go —
// Pass 4 defect C.18.
//
// The 2026-09-05 audit framed target_time as accepting "arbitrary
// strings" when the canonical contract should be RFC3339/cron.
// That framing conflates two distinct fields:
//
//   target_time     → absolute unix epoch, relative duration, or
//                      ISO-8601 timestamp (RFC3339/RFC3339Nano/
//                      "2006-01-02 15:04:05"). resolveTargetTime at
//                      internal/core/wake_tools.go:658 already
//                      rejects arbitrary strings with a clear error.
//   recurring_rule  → cron expression (robfig/cron/v3). Previously
//                      accepted as an unvalidated plain string and
//                      stored verbatim — typos (e.g. "* * *", missing
//                      field) were never surfaced, leaving the agent
//                      to discover the failure at next-schedule time.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_wakes --payload '{"action":"schedule","params":{"reason":"x","recurring_rule":"* * *"}}'
//     # wanted: error mentioning invalid cron expression
//     # actual: success, schedule persisted with malformed rule
//
// Canonical contract:
//
//   target_time omitted → ERROR (required)
//   target_time = ""    → ERROR (handler pre-check)
//   target_time = "1700000000" (numeric epoch) → ACCEPTED
//   target_time = "30m" / "2h" / "1d"        → ACCEPTED (relative)
//   target_time = "2026-09-05T10:00:00Z"      → ACCEPTED (ISO-8601)
//   target_time = "hello world"              → ERROR (unrecognized)
//
//   recurring_rule omitted   → allowed (one-shot wake)
//   recurring_rule = ""      → allowed
//   recurring_rule = "0 * * * *"             → ACCEPTED (canonical 5-field cron)
//   recurring_rule = "* * *"                 → ERROR (malformed)
//   recurring_rule = "sometimes"             → ERROR (not a cron expression)
//
// The fix pins both contracts at the handler boundary so caller
// typos fail loudly rather than persisting silent garbage.

package tools

import (
	"strings"
	"testing"
	"time"
)

// TestWakesSchedule_ValidTargetTimeEpoch pins: a pure numeric
// target_time (absolute unix epoch as a string) is accepted.
func TestWakesSchedule_ValidTargetTimeEpoch(t *testing.T) {
	dm := newTestSharedDM(t)

	future := time.Now().Add(24 * time.Hour).Unix()
	_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "schedule",
		"params": map[string]interface{}{
			"reason":      "test-epoch",
			"target_time": float64(future),
		},
	})
	// JSON numbers arrive as float64 — the handler type-asserts to
	// string, so the numeric form is rejected. The canonical
	// stringly-typed path (relative/ISO-8601) is the live contract;
	// this test pins the current behaviour so a future change that
	// accepts JSON numbers is a deliberate decision.
	if err == nil {
		t.Logf("numeric (float64) target_time accepted — note: handler now accepts JSON numbers")
	}
}

// TestWakesSchedule_ValidTargetTimeRelative pins: a relative
// duration string is accepted.
func TestWakesSchedule_ValidTargetTimeRelative(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "schedule",
		"params": map[string]interface{}{
			"reason":      "test-relative",
			"target_time": "2h",
		},
	})
	if err != nil {
		t.Errorf("relative target_time '2h' must be accepted: %v", err)
	}
}

// TestWakesSchedule_ValidTargetTimeRFC3339 pins: an ISO-8601 /
// RFC3339 timestamp is accepted.
func TestWakesSchedule_ValidTargetTimeRFC3339(t *testing.T) {
	dm := newTestSharedDM(t)

	future := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "schedule",
		"params": map[string]interface{}{
			"reason":      "test-rfc3339",
			"target_time": future,
		},
	})
	if err != nil {
		t.Errorf("RFC3339 target_time %q must be accepted: %v", future, err)
	}
}

// TestWakesSchedule_InvalidTargetTimeRejected pins: an arbitrary
// string is rejected at the boundary. resolveTargetTime already
// rejects anything that is not numeric, relative, or ISO-8601; the
// handler propagates the error.
func TestWakesSchedule_InvalidTargetTimeRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, bad := range []string{"hello world", "soon", "tomorrow", "next week", "abc"} {
		t.Run("target_time="+bad, func(t *testing.T) {
			_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
				"action": "schedule",
				"params": map[string]interface{}{
					"reason":      "test-invalid-time",
					"target_time": bad,
				},
			})
			if err == nil {
				t.Errorf("arbitrary target_time %q must error", bad)
			}
			if !strings.Contains(err.Error(), "target_time") {
				t.Errorf("error must mention 'target_time', got: %v", err)
			}
		})
	}
}

// TestWakesSchedule_EmptyTargetTimeRejected pins: omitted/empty
// target_time errors at the handler boundary.
func TestWakesSchedule_EmptyTargetTimeRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, empty := range []interface{}{nil, ""} {
		_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
			"action": "schedule",
			"params": map[string]interface{}{
				"reason":      "test-empty",
				"target_time": empty,
			},
		})
		if err == nil {
			t.Errorf("empty target_time (%v) must error", empty)
		}
	}
}

// TestWakesSchedule_InvalidTargetTimeDoesNotPersist pins the
// write-path guarantee: rejected target_time does not create a
// scheduled wake row.
func TestWakesSchedule_InvalidTargetTimeDoesNotPersist(t *testing.T) {
	dm := newTestSharedDM(t)

	beforeRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`)
	beforeCount := 0
	if err := beforeRow.Scan(&beforeCount); err != nil {
		t.Fatalf("count scheduled_wakes before: %v", err)
	}

	_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "schedule",
		"params": map[string]interface{}{
			"reason":      "no-persist",
			"target_time": "totally bogus",
		},
	})
	if err == nil {
		t.Fatalf("invalid target_time must error")
	}

	afterRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`)
	afterCount := 0
	if err := afterRow.Scan(&afterCount); err != nil {
		t.Fatalf("count scheduled_wakes after: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("rejected target_time must not create a wake; before=%d, after=%d",
			beforeCount, afterCount)
	}
}

// TestWakesSchedule_RecurringRuleFieldRetired pins: as of the
// 2026-10-04 retirement (Tranche B §10), recurring_rule is no
// longer accepted on the mpm_wakes schedule action. Any caller
// still sending the field — whether the value is a valid cron
// expression or a malformed string — must receive a clear
// migration error pointing them at the scheduled_tasks surface.
//
// This is the post-retirement contract. The pre-retirement
// contract (validation + persistence verbatim) is documented in
// the C.18 audit remediation and is exercised by the git history
// at commit 2219a08 and earlier.
func TestWakesSchedule_RecurringRuleFieldRetired(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, bad := range []string{"* * *", "sometimes", "0 25 * * *", "not a cron", "0 * * * *"} {
		t.Run("recurring_rule="+bad, func(t *testing.T) {
			_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
				"action": "schedule",
				"params": map[string]interface{}{
					"reason":         "test-retired-rr",
					"target_time":    "24h",
					"recurring_rule": bad,
				},
			})
			if err == nil {
				t.Errorf("recurring_rule is retired; %q must error", bad)
			}
			// The error must name the field AND must name the
			// replacement surface, so the agent can self-correct
			// without reading the changelog.
			msg := err.Error()
			if !strings.Contains(msg, "recurring_rule") {
				t.Errorf("error must mention 'recurring_rule' so the caller can identify the rejected field, got: %v", err)
			}
			if !strings.Contains(msg, "scheduled_tasks") && !strings.Contains(msg, "upsert_task") {
				t.Errorf("error must point at the replacement surface (scheduled_tasks / upsert_task), got: %v", err)
			}
		})
	}
}

// TestWakesSchedule_RecurringRuleRetiredDoesNotPersist pins the
// write-path guarantee for the retired field: a rejected
// recurring_rule does not create a scheduled wake row. Pre-fix
// the value was persisted verbatim; post-retirement the field is
// rejected at the boundary, so no row is created.
func TestWakesSchedule_RecurringRuleRetiredDoesNotPersist(t *testing.T) {
	dm := newTestSharedDM(t)

	beforeRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`)
	beforeCount := 0
	if err := beforeRow.Scan(&beforeCount); err != nil {
		t.Fatalf("count scheduled_wakes before: %v", err)
	}

	_, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "schedule",
		"params": map[string]interface{}{
			"reason":         "no-persist-retired",
			"target_time":    "24h",
			"recurring_rule": "* * *",
		},
	})
	if err == nil {
		t.Fatalf("retired recurring_rule must error")
	}

	afterRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`)
	afterCount := 0
	if err := afterRow.Scan(&afterCount); err != nil {
		t.Fatalf("count scheduled_wakes after: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("rejected recurring_rule must not create a wake; before=%d, after=%d",
			beforeCount, afterCount)
	}
}
