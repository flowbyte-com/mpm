// drill_score.go — verdict scoring from tool-invocation evidence.
//
// Score is a PURE function over (drill, invocations, db). It does not
// call out to anything, does not write to the audit table, and has no
// time-dependent logic. This purity is the entire point of the drill
// architecture: the verdict is derived from evidence, not asserted by
// the agent under test.
//
// Composite verdict rules (any failure flips Passed=false):
//  1. tools_required  — every named tool must appear at least once
//  2. sequence        — Expect.Sequence must appear in invocations in
//                       started_at order (subsequence match, not strict)
//  3. artifacts_required — when db != nil, query for the typed artifact
//     (v1 supports "lesson" only — no tag filter; tag support arrives
//     when the first drill fixture actually demands it)
//  4. forbidden       — any tool:action pair must NOT appear

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Verdict is the JSON shape persisted in drill_runs.verdict. Reasons is
// human-readable; UI surfaces them in `mpm drills report --verbose`.
type Verdict struct {
	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons"`
}

// MarshalVerdict is the canonical serialiser for Verdict. Stored as a
// JSON column in drill_runs; readable by `mpm drills report` and the
// future mpm-debug web UI.
func MarshalVerdict(v Verdict) ([]byte, error) { return json.Marshal(v) }

// Score computes the verdict for one drill run against one slice of
// invocations (already filtered by session_id by the caller). When db
// is nil the artifacts_required check is skipped — the synthetic
// drill-engine self-test uses this to score sequence-only drills.
func Score(drill DrillSpec, invocations []ToolCall, db *sql.DB) Verdict {
	v := Verdict{Passed: true}

	// 1. tools_required — every named tool appears at least once.
	for _, tool := range drill.Expect.ToolsRequired {
		found := false
		for _, inv := range invocations {
			if inv.ToolName == tool {
				found = true
				break
			}
		}
		if !found {
			v.Passed = false
			v.Reasons = append(v.Reasons, "missing required tool: "+tool)
		}
	}

	// 2. sequence — subsequence match. The first invocation that
	// matches step[0] advances; the next invocation that matches
	// step[1] advances; and so on. Out-of-order invocations between
	// matches are tolerated (a real agent might wake_context, recall,
	// then wake again). A missing match aborts the check.
	cursor := 0
	for _, step := range drill.Expect.Sequence {
		matched := false
		for cursor < len(invocations) {
			inv := invocations[cursor]
			cursor++
			if inv.ToolName == step.Tool && inv.Action == step.Action {
				matched = true
				break
			}
		}
		if !matched {
			v.Passed = false
			v.Reasons = append(v.Reasons,
				fmt.Sprintf("sequence break: expected %s:%s", step.Tool, step.Action))
			break
		}
	}

	// 3. artifacts_required — only consulted when a DB is supplied.
	// v1 supports "lesson" rows only, with no tag filter. Expand to
	// memory/theory/decision and tag filters when a fixture demands it.
	for _, art := range drill.Expect.ArtifactsRequired {
		if db == nil {
			continue
		}
		query := buildArtifactQuery(art)
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil {
			v.Passed = false
			v.Reasons = append(v.Reasons,
				fmt.Sprintf("artifact probe error: %s: %v", art.Type, err))
			continue
		}
		if n == 0 {
			v.Passed = false
			v.Reasons = append(v.Reasons, "missing required artifact: "+art.Type)
		}
	}

	// 4. forbidden — any banned tool:action that fired flips the
	// verdict. Match shape "tool:action" matches the harness-emitted
	// ToolCall stringification.
	invIndex := make(map[string]bool, len(invocations))
	for _, inv := range invocations {
		invIndex[inv.ToolName+":"+inv.Action] = true
	}
	for _, f := range drill.Expect.Forbidden {
		if invIndex[f] {
			v.Passed = false
			v.Reasons = append(v.Reasons, "forbidden invocation present: "+f)
		}
	}

	return v
}

// buildArtifactQuery returns the SQL COUNT(*) for one artifact
// requirement. v1 supports only the "lesson" type with no tag filter
// — adding tag/memory/theory support is a v1.1 expansion that lives
// with the first fixture that demands it.
//
// Misconfigured types degrade to "no rows" (a verbose reporter will
// surface the mismatch) rather than crashing the drill mid-run, so a
// typo in a fixture YAML doesn't take the matrix down with it.
func buildArtifactQuery(art DrillArtifact) string {
	switch art.Type {
	case "lesson":
		return `SELECT COUNT(*) FROM lessons`
	}
	return `SELECT 0`
}
