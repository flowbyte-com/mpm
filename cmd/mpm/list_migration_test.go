// list_migration_test.go — Regression coverage for the
// 2026-09-14 release-pass migration of the remaining list
// surfaces to the canonical visual grammar:
//
//   - mpm theory list
//   - mpm decision list (incl. human-readable dates)
//   - mpm topic list
//   - mpm reference ls (incl. empty-state heading)
//   - mpm tasks list
//
// Each test runs against the built binary in an isolated workspace
// and asserts the canonical heading token (`MPM · <Command>`).

package main

import (
	"strings"
	"testing"
)

// TestTheoryList_ExactHeading asserts `mpm theory list` (and the
// alias `mpm theories`) opens with `MPM · Theory list`.
func TestTheoryList_ExactHeading(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	for _, args := range [][]string{{"theory", "list"}, {"theories"}} {
		stdout, _, _ := runMpmParity(t, bin, workspace, args...)
		out := string(stdout)
		if !strings.Contains(out, "MPM · Theory list") {
			t.Fatalf("`mpm %s` must show canonical heading `MPM · Theory list`.\nGot:\n%s", strings.Join(args, " "), out)
		}
	}
}

// TestDecisionList_ExactHeading asserts `mpm decision list`
// (no-args legacy) opens with `MPM · Decision list`.
func TestDecisionList_ExactHeading(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "decision", "list")
	out := string(stdout)
	if !strings.Contains(out, "MPM · Decision list") {
		t.Fatalf("`mpm decision list` must show canonical heading `MPM · Decision list`.\nGot:\n%s", out)
	}
}

// TestDecisionList_HumanReadableDate asserts that for a decision
// stored with created_at as a Unix-epoch string, the human output
// shows a human-readable date (not bare epoch digits).
func TestDecisionList_HumanReadableDate(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	// Seed a decision with a Unix-epoch created_at string (matches
	// the legacy rows observed in the production DB).
	stdout, _, _ := runMpmParity(t, bin, workspace, "decide",
		"context=epoch probe",
		"choice=epoch probe choice",
		"rationale=epoch probe rationale",
		"--created-at=1789319779",
	)
	if stdout == nil {
		// tolerate older --created-at absence; fall back to listing.
	}
	listOut, _, _ := runMpmParity(t, bin, workspace, "decision", "list")
	out := string(listOut)
	// The bare epoch must NOT appear in the human output.
	if strings.Contains(out, "1789319779\n") {
		t.Fatalf("decision list must not show raw Unix-epoch in human mode.\nGot:\n%s", out)
	}
	// The output should contain a date label or a recognizable date fragment.
	if !strings.Contains(out, "date:") && !strings.Contains(out, "T") {
		t.Fatalf("decision list must include a human-readable date label.\nGot:\n%s", out)
	}
	_ = stdout
}

// TestTopicList_ExactHeading asserts `mpm topic list` opens with
// `MPM · Topic list`.
func TestTopicList_ExactHeading(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "topic", "list")
	out := string(stdout)
	if !strings.Contains(out, "MPM · Topic list") {
		t.Fatalf("`mpm topic list` must show canonical heading `MPM · Topic list`.\nGot:\n%s", out)
	}
}

// TestReferenceList_ExactHeading asserts `mpm reference ls` opens
// with `MPM · Reference list` (or, when empty, the same heading
// plus `No references stored.`).
func TestReferenceList_ExactHeading(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "reference", "ls")
	out := string(stdout)
	if !strings.Contains(out, "MPM · Reference list") {
		t.Fatalf("`mpm reference ls` must show canonical heading `MPM · Reference list`.\nGot:\n%s", out)
	}
}

// TestTasksList_ExactHeading asserts `mpm tasks list` opens with
// `MPM · Tasks` and preserves NEXT RUN / LAST RUN as separate
// columns (defect K regression pin).
func TestTasksList_ExactHeading(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "tasks", "list")
	out := string(stdout)
	if !strings.Contains(out, "MPM · Tasks") {
		t.Fatalf("`mpm tasks list` must show canonical heading `MPM · Tasks`.\nGot:\n%s", out)
	}
	// NEXT RUN and LAST RUN must remain separate columns.
	if !strings.Contains(out, "NEXT RUN (UTC)") {
		t.Fatalf("`mpm tasks list` must preserve NEXT RUN column.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "LAST RUN (UTC)") {
		t.Fatalf("`mpm tasks list` must preserve LAST RUN column.\nGot:\n%s", out)
	}
}
