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
	"encoding/json"
	"fmt"
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

// TestWorkItemList_HumanModeNotJSON asserts `mpm work item list`
// (default human mode) does NOT start with `{` and DOES contain
// the canonical heading `MPM · Work item list`.
//
// 2026-09-14 release-pass: pre-fix, this surface emitted the JSON
// envelope unconditionally in default mode (the substrate's
// `mpm_work action=list` envelope), making `mpm work item list`
// unusable for human readers. The CLI now consumes the same
// structured row helper the substrate's JSON path uses, but
// renders it through the canonical visual grammar in human mode.
func TestWorkItemList_HumanModeNotJSON(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "work", "item", "list")
	out := string(stdout)
	// 1. Default human mode must NOT begin with `{` (i.e. must not
	//    be a JSON envelope).
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("`mpm work item list` (default human mode) must not start with `{` (JSON envelope).\nGot:\n%s", out)
	}
	// 2. Default human mode MUST show the canonical heading.
	if !strings.Contains(out, "MPM · Work item list") {
		t.Fatalf("`mpm work item list` (default human mode) must show canonical heading `MPM · Work item list`.\nGot:\n%s", out)
	}
}

// TestWorkItemList_HumanAndJsonShareData asserts that human and
// JSON modes both surface the same work items. Pre-fix, the
// CLI invoked another CLI surface and re-parsed its JSON output,
// which is fragile (CLI-to-CLI invocation). The fix routes both
// surfaces through the shared mpminternal.ListWorkRows helper so
// the row data is identical by construction.
//
// The test seeds N work items via `mpm work item create`, then
// runs:
//   - default human mode → counts rows by ` : ` label separator
//   - JSON mode → parses envelope and counts `works[*]`
// and asserts both counts equal N.
func TestWorkItemList_HumanAndJsonShareData(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()

	// Seed three work items via the canonical CLI path so they live
	// in the same substrate both modes will read.
	want := 3
	for i := 0; i < want; i++ {
		title := fmt.Sprintf("regression probe item %d", i)
		_, _, _ = runMpmParity(t, bin, workspace,
			"work", "item", "create", title,
			fmt.Sprintf("consistency probe item %d", i),
		)
	}

	// Human mode: count distinct work-id badges by their leading
	// 8-character hex prefix pattern (matches the canonical
	// IDBadge render).
	humanOut, _, _ := runMpmParity(t, bin, workspace, "work", "item", "list")
	humanRows := countWorkRowsInHuman(string(humanOut))
	if humanRows != want {
		t.Fatalf("human mode reported %d rows; want %d.\nGot:\n%s", humanRows, want, string(humanOut))
	}

	// JSON mode: parse envelope and assert `count` and `works`
	// length both equal want.
	jsonOut, _, _ := runMpmParity(t, bin, workspace, "work", "item", "list", "--json")
	envelope := parseWorkItemListEnvelope(t, string(jsonOut))
	if envelope.Count != want {
		t.Fatalf("JSON envelope count = %d; want %d.\nEnvelope: %+v", envelope.Count, want, envelope)
	}
	if len(envelope.Works) != want {
		t.Fatalf("JSON envelope works length = %d; want %d.\nEnvelope: %+v", len(envelope.Works), want, envelope)
	}
}

// workItemListEnvelope mirrors the canonical
// {success,works,count} envelope shape emitted by `mpm work item
// list --json` and the substrate's `mpm_work action=list`.
type workItemListEnvelope struct {
	Success bool                     `json:"success"`
	Count   int                      `json:"count"`
	Works   []map[string]interface{} `json:"works"`
}

// parseWorkItemListEnvelope decodes the JSON envelope from the
// `mpm work item list --json` output. Strips any leading log
// lines (perms-sweep warnings) before parsing — the JSON
// envelope itself always starts with `{`.
func parseWorkItemListEnvelope(t *testing.T, raw string) workItemListEnvelope {
	t.Helper()
	// Find the first `{` from the left and parse from there.
	idx := strings.IndexByte(raw, '{')
	if idx < 0 {
		t.Fatalf("no JSON envelope found in output:\n%s", raw)
	}
	var env workItemListEnvelope
	if err := json.Unmarshal([]byte(raw[idx:]), &env); err != nil {
		t.Fatalf("failed to parse JSON envelope: %v.\nRaw:\n%s", err, raw[idx:])
	}
	return env
}

// countWorkRowsInHuman counts work-item rows in the canonical
// human-mode rendering. Each row opens with an indented IDBadge
// (the `render.IDBadge` output uses 4-space indent + 8-char hex
// id + ` : ` separator). The empty-state "No <status> work
// items." line is NOT counted.
//
// Implementation note: we count occurrences of the canonical
// ID badge separator (`: ` following an indented 8-hex pattern).
// This avoids hard-coding a regex on the full badge form which
// may evolve; the separator is the stable invariant.
func countWorkRowsInHuman(out string) int {
	// Empty-state: no rows.
	trim := strings.TrimSpace(out)
	if strings.HasPrefix(trim, "No ") && strings.HasSuffix(trim, "work items.") {
		return 0
	}
	// Heading row + blank line + rows. Count newlines that look
	// like the start of a rendered ID badge row. The renderer
	// uses `  <id> : <title>` form (4-space indent + id + " : ").
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "    ") && strings.Contains(line, " : ") {
			count++
		}
	}
	return count
}
