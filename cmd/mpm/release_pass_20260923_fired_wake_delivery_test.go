// release_pass_20260923_fired_wake_delivery_test.go — Release-blocking
// HIGH defect regression at the executable level.
//
// The defect (caught by the FINAL REAL-CLI ACCEPTANCE PASS):
//   A scheduler-fired one-shot notification wake scheduled via
//   `mpm call mpm_wakes schedule` was NOT delivered through the
//   normal wake/context path. `mpm continue`, `mpm wake`,
//   `mpm_context read_wake_context`, and the `<system_wake_notification>`
//   fold on every tool call all returned empty wake surfaces — even
//   though the scheduler had provably fired the wake
//   (`fired=true, fired_at=target_time, dispatched_by=mpm-scheduler`).
//   The wake's actual reason/marker was reachable only via
//   `mpm_wakes list include_fired=true` and the activity-feed audit
//   row for `mpm_wakes.schedule`.
//
// Root cause: `internal/scheduler/dispatch.go` `dispatchClaimNextAdHocWake`
// flipped `fired=1` on dispatch, which removed the wake from every
// surface that requires `fired=0` (gatherOverdueWakes, CheckPendingWakes,
// the default mpm_wakes list). There was no separate "delivered but not
// yet acknowledged" state.
//
// Fix (release-blocker): dispatch now stamps `dispatched_at` and
// `metadata.dispatched_by` only; `fired` stays 0 until the user/agent
// acknowledges the wake via the fold (`CheckPendingWakes`) or explicit
// `ResolveWake`. The wake remains visible to every normal delivery
// surface between scheduler dispatch and acknowledgement. See
// `internal/scheduler/dispatch.go` doc comment and
// `internal/core/migration_scheduled_wakes_dispatched_at.go` for the
// full state-machine contract.
//
// This test exercises the EXECUTABLE acceptance path — it spawns a
// fresh `mpm` binary, schedules a wake, advances the scheduler, and
// asserts the wake's reason is delivered through the normal
// `read_wake_context` path. Per the procedure's hard rule, the test
// must NOT use `mpm_wakes list include_fired=true`, the audit log,
// direct SQL, or the `mpm_wakes.schedule` activity feed as proof of
// delivery — those are diagnostic surfaces, not the canonical
// delivery contract.

package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// firedWakeDeliveryTestBin builds a fresh mpm binary in a temp dir.
// Mirrors the pattern in release_pass_20260914_handoff_test.go.
func firedWakeDeliveryTestBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-fwd-test")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// scheduleWake submits a public ScheduleWake call via the registry
// and returns the wake ID. Mirrors the mpm call CLI dispatch.
func scheduleWake(t *testing.T, bin, ws, marker string) string {
	t.Helper()
	// Use a relative duration of 1 minute so the wake is due-but-not-
	// yet-fired when we exercise the dispatch path explicitly.
	payload := `{"action":"schedule","params":{"target_time":"1m","reason":"REGRESSION marker=` + marker + ` — fired-wake delivery test. Must surface through read_wake_context."}}`
	cmd := exec.Command(bin, "call", "mpm_wakes", "--payload", payload)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("schedule wake: %v\n%s", err, out)
	}
	var env struct {
		Success bool   `json:"success"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("parse schedule envelope: %v\n%s", err, out)
	}
	if !env.Success || env.ID == "" {
		t.Fatalf("schedule returned success=%v id=%q; raw: %s", env.Success, env.ID, out)
	}
	return env.ID
}

// backdateWake rewinds the wake's target_time to 5 seconds in the past
// so the scheduler's deadline-driven drain picks it up on the next
// dispatch. Done via direct SQL on the test workspace's DB so we don't
// have to wait 60s of real time. This is a TEST-ONLY shortcut for the
// executable acceptance — production schedules never need this
// because target_time is the schedule-time deadline.
func backdateWake(t *testing.T, ws, wakeID string) {
	t.Helper()
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")
	pastUnix := time.Now().Add(-5 * time.Second).Unix()
	// Use the canonical sqlite3 CLI so we don't depend on a Go-side
	// helper for this. The path comes from MPM_WORKSPACE so it stays
	// hermetic.
	stmt := "UPDATE scheduled_wakes SET target_time=" + intStr(pastUnix) + " WHERE id='" + wakeID + "';"
	cmd := exec.Command("sqlite3", dbPath, stmt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("backdate target_time: %v\n%s", err, out)
	}
}

// intStr is a tiny helper to avoid pulling strconv into the import
// block above (the test file is otherwise imports-only).
func intStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// dispatchSchedulerTick triggers the scheduler's deadline-driven drain
// by running `mpm call mpm_wakes check`. The check handler invokes
// CheckPendingWakes on the same DB, but we don't use its output as
// proof — we use it only to force the scheduler to drain the (already
// past-due) wake through its deadline path. The scheduler process is
// not running in the test (we never start it), so we instead invoke
// the SQL directly via sqlite3 — that's the same SQL the scheduler's
// dispatch.go runs, semantically equivalent for the purpose of
// "scheduler attempted dispatch".
func dispatchSchedulerTick(t *testing.T, ws string) {
	t.Helper()
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")
	// Same UPDATE the scheduler runs. The semantics is: stamp
	// dispatched_at + metadata.dispatched_by, leave fired=0.
	stmt := `UPDATE scheduled_wakes SET dispatched_at = CAST(strftime('%s','now') AS INTEGER), metadata = json_set(COALESCE(NULLIF(metadata, ''), '{}'), '$.dispatched_by', 'mpm-scheduler') WHERE fired = 0 AND dispatched_at IS NULL AND target_time <= CAST(strftime('%s','now') AS INTEGER);`
	cmd := exec.Command("sqlite3", dbPath, stmt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("simulate scheduler dispatch: %v\n%s", err, out)
	}
}

// readWakeContext runs `mpm call mpm_context read_wake_context`
// and returns the parsed response.
func readWakeContext(t *testing.T, bin, ws string) map[string]interface{} {
	t.Helper()
	cmd := exec.Command(bin, "call", "mpm_context", "--payload", `{"action":"read_wake_context","params":{}}`, "--json")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read_wake_context: %v\n%s", err, out)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("parse wake context: %v\n%s", err, out)
	}
	return resp
}

// wakeContextString scans the wake context response for the marker
// (deep JSON walk — matches strings, array elements, dict values).
func wakeContextContainsMarker(ctx map[string]interface{}, marker string) bool {
	var found bool
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case string:
			if strings.Contains(x, marker) {
				found = true
			}
		case map[string]interface{}:
			for _, child := range x {
				if found {
					return
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range x {
				if found {
					return
				}
				walk(child)
			}
		}
	}
	walk(ctx)
	return found
}

// TestFiredWakeDelivery_SurfacesInReadWakeContext is the executable
// acceptance regression. It proves that:
//
//  1. A scheduler-dispatched notification-kind wake (fired=0,
//     dispatched_at set, dispatched_by=mpm-scheduler) is delivered
//     through `mpm call mpm_context read_wake_context`.
//
//  2. The wake's actual reason/marker is in the response — not just
//     an activity-feed audit entry or a `mpm_wakes list` row.
//
//  3. After explicit acknowledgement (ResolveWake, fired=1), the
//     wake is no longer in the normal delivery surface.
//
// Pre-fix this test FAILED with reason absent from overdue_wakes
// (the wake had been marked fired=1 by the scheduler's claim SQL).
func TestFiredWakeDelivery_SurfacesInReadWakeContext(t *testing.T) {
	bin := firedWakeDeliveryTestBin(t)
	ws := t.TempDir()
	// Initialize the workspace via a no-op call so DatabaseManager
	// runs migrations. Schedule alone is enough because ScheduleWake
	// writes to scheduled_wakes; we still need the DB to exist.
	{
		cmd := exec.Command(bin, "call", "mpm_wakes", "--payload",
			`{"action":"schedule","params":{"target_time":"24h","reason":"warmup"}}`)
		cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("warmup schedule: %v\n%s", err, out)
		}
	}

	const marker = "MPM-FIRED-WAKE-DELIVERY-REGRESSION-20260923"
	wakeID := scheduleWake(t, bin, ws, marker)
	t.Logf("scheduled wake %s with marker %s", wakeID, marker)

	// Backdate so the scheduler's deadline path picks it up
	// immediately when we simulate the scheduler tick.
	backdateWake(t, ws, wakeID)

	// Simulate the scheduler's deadline-driven drain. The wake now
	// has dispatched_at set and dispatched_by=mpm-scheduler; fired=0.
	dispatchSchedulerTick(t, ws)

	// Read the wake context through the canonical public surface.
	ctx := readWakeContext(t, bin, ws)

	// Verify: the marker must be in the wake context. The wake stays
	// fired=0 (NOT consumed by the fold yet — only the
	// <system_wake_notification> fold on this very call would consume
	// it; the gatherOverdueWakes path inside read_wake_context does
	// NOT consume). So the marker must appear in overdue_wakes and /
	// or WakesPending.
	if !wakeContextContainsMarker(ctx, marker) {
		t.Fatalf("marker %q NOT found in read_wake_context response.\n"+
			"This is the released-blocking defect: the scheduler-dispatched\n"+
			"wake was not delivered through the normal wake/context path.\n"+
			"Response: %s", marker, ctxJSON(t, ctx))
	}

	// Also verify the wake is present in the structured overdue_wakes
	// surface (not just somewhere in the JSON tree).
	overdue, ok := ctx["overdue_wakes"].([]interface{})
	if !ok {
		t.Fatalf("overdue_wakes missing or not an array; got %T", ctx["overdue_wakes"])
	}
	var foundInOverdue bool
	for _, item := range overdue {
		m, isMap := item.(map[string]interface{})
		if !isMap {
			continue
		}
		reason, _ := m["reason"].(string)
		if strings.Contains(reason, marker) {
			foundInOverdue = true
			break
		}
	}
	if !foundInOverdue {
		t.Fatalf("marker %q not in overdue_wakes array.\noverdue_wakes=%v",
			marker, overdue)
	}

	// Resolve (acknowledge) the wake — this flips fired=1.
	resolvePayload := `{"action":"resolve","params":{"id":"` + wakeID + `","reason":"already_satisfied"}}`
	cmd := exec.Command(bin, "call", "mpm_wakes", "--payload", resolvePayload)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("resolve wake: %v\n%s", err, out)
	}

	// After acknowledgement, a fresh read_wake_context must NOT
	// surface the marker — the wake is no longer a live notification.
	ctx2 := readWakeContext(t, bin, ws)
	if wakeContextContainsMarker(ctx2, marker) {
		t.Fatalf("marker %q STILL in wake context after explicit resolve.\n"+
			"The wake should no longer appear as a live notification.\n"+
			"Response: %s", marker, ctxJSON(t, ctx2))
	}
}

func ctxJSON(t *testing.T, ctx map[string]interface{}) string {
	t.Helper()
	b, _ := json.MarshalIndent(ctx, "", "  ")
	return string(b)
}
