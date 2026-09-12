// renderer_doctor_cron_retention_test.go — pins the human-readable
// rendering of the cron-retention diagnostic block.
//
// A fresh agent reading `mpm doctor` output should see enough context
// to distinguish startup stabilization from steady state from
// genuine degradation — without reading scheduler source code.
// These tests pin that contract.

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm/internal/scheduler"
)

// TestRenderer_DoctorReport_CronRetentionFields_JSON pins the JSON
// serialization of CronRetentionStatus. The struct's JSON tags are
// part of the diagnostic contract; renames break consumers (e.g.
// alerting scripts, observability tools).
func TestRenderer_DoctorReport_CronRetentionFields_JSON(t *testing.T) {
	cr := &CronRetentionStatus{
		Pending:                       86,
		EligibleBacklog:               26,
		RetentionWindowSec:            int64(scheduler.CronRetentionWindow.Seconds()),
		SweepCadenceSec:               int64(scheduler.CronRetentionCadence.Seconds()),
		NormalLimit:                   scheduler.CronRetentionNormalLimit,
		CatchUpLimit:                  scheduler.CronRetentionCatchUpLimit,
		Phase:                         "startup_stabilization",
		SchedulerUptimeSec:            5100,
		LastExpectedSweepAgoSec:       1500,
		SecondsUntilNextExpectedSweep: 2100,
		Interpretation:                "Test interpretation.",
	}
	data, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Each field must appear with the canonical snake_case name.
	wantFields := map[string]any{
		"pending":                          float64(86),
		"eligible_backlog":                 float64(26),
		"retention_window_seconds":         float64(scheduler.CronRetentionWindow.Seconds()),
		"sweep_cadence_seconds":            float64(scheduler.CronRetentionCadence.Seconds()),
		"normal_limit":                     float64(scheduler.CronRetentionNormalLimit),
		"catchup_limit":                    float64(scheduler.CronRetentionCatchUpLimit),
		"phase":                            "startup_stabilization",
		"scheduler_uptime_seconds":         float64(5100),
		"last_expected_sweep_ago_seconds":   float64(1500),
		"seconds_until_next_expected_sweep": float64(2100),
		"interpretation":                   "Test interpretation.",
	}
	for k, want := range wantFields {
		if got[k] != want {
			t.Errorf("field %q = %v, want %v", k, got[k], want)
		}
	}
}

// TestRenderer_DoctorReport_CronRetentionPhaseBlock pins the
// human-readable rendering of the cron-retention block under the
// Scheduler check. The block must include Phase + uptime + pool +
// sweep timing so an agent sees the diagnostic-contract fields
// without needing to parse JSON.
func TestRenderer_DoctorReport_CronRetentionPhaseBlock(t *testing.T) {
	var buf bytes.Buffer
	r := NewDoctorRenderer(&buf, false) // text labels (no emoji) for stable assertion

	cr := &CronRetentionStatus{
		Pending:                       86,
		EligibleBacklog:               26,
		RetentionWindowSec:            int64(scheduler.CronRetentionWindow.Seconds()),
		SweepCadenceSec:               int64(scheduler.CronRetentionCadence.Seconds()),
		NormalLimit:                   scheduler.CronRetentionNormalLimit,
		CatchUpLimit:                  scheduler.CronRetentionCatchUpLimit,
		Phase:                         "startup_stabilization",
		SchedulerUptimeSec:            5100,
		LastExpectedSweepAgoSec:       1500,
		SecondsUntilNextExpectedSweep: 2100,
	}
	r.renderCronRetention(cr)

	out := buf.String()

	// Required substrings — the operator-facing surface.
	wantSubstrings := []string{
		"startup_stabilization",                          // phase name
		"Pending cron wakes: 86",                         // pending
		"eligible backlog: 26",                           // eligible backlog
		"retention window: 1h 0m",                        // retention window
		"cadence: 1h 0m",                                 // cadence (1h)
		"Last expected sweep: 25m 0s ago",                // sweep timing
		"next expected sweep: 35m 0s",                    // next sweep timing
		"Scheduler uptime: 1h 25m",                       // uptime
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderer_DoctorReport_CronRetentionInterpretationAppended
// pins: when Interpretation is set, it's rendered as a faint
// explanation line under the cron-retention block.
func TestRenderer_DoctorReport_CronRetentionInterpretationAppended(t *testing.T) {
	var buf bytes.Buffer
	r := NewDoctorRenderer(&buf, false)

	cr := &CronRetentionStatus{
		Phase:          "startup_stabilization",
		Interpretation: "Recent cron wakes are intentionally retained.",
	}
	r.renderCronRetention(cr)

	if !strings.Contains(buf.String(), "Recent cron wakes are intentionally retained.") {
		t.Errorf("interpretation not rendered; got: %q", buf.String())
	}
}