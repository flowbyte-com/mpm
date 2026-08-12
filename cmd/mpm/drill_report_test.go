// drill_report_test.go — matrix computation unit tests.
//
// The report's correctness boils down to the four pure functions:
//   - harnessCapability(framework) → WIRED | NOT_WIRED
//   - rollupFrameworkBehavior(rollup) → PASS | FAIL | MIXED | NOT_TESTED
//   - buildDrillRows(drills, latestRuns) → drillRow[]
//   - buildFrameworkRows(drillRows) → frameworkRow[]
//
// These tests pin the load-bearing logic so the matrix can't lie
// about capability or behavior. The DB-touching queryLatestDrillRuns
// is covered by the integration test (`mpm drills report` against
// a real DB); re-running it here would just duplicate the SQL.

package main

import (
	"testing"

	core "github.com/flowbyte-com/mpm-core"
)

func TestHarnessCapability(t *testing.T) {
	cases := []struct {
		fw   string
		want string
	}{
		{"synthetic", "WIRED"},
		{"claude_code", "WIRED"},
		{"hermes", "NOT_WIRED"},
		{"openclaw", "NOT_WIRED"},
		{"opencode", "NOT_WIRED"},
		{"pi", "NOT_WIRED"},
		{"", "NOT_WIRED"},  // unknown
		{"x", "NOT_WIRED"}, // unknown
	}
	for _, c := range cases {
		if got := harnessCapability(c.fw); got != c.want {
			t.Errorf("harnessCapability(%q) = %q, want %q", c.fw, got, c.want)
		}
	}
}

func TestRollupBehavior(t *testing.T) {
	cases := []struct {
		name string
		fr   frameworkRow
		want string
	}{
		{
			name: "no drills declared",
			fr:   frameworkRow{Framework: "x", Drills: 0},
			want: "NOT_TESTED",
		},
		{
			name: "all untested",
			fr:   frameworkRow{Framework: "x", Drills: 2, Untested: 2},
			want: "NOT_TESTED",
		},
		{
			name: "all pass",
			fr:   frameworkRow{Framework: "x", Drills: 3, PassCount: 3},
			want: "PASS",
		},
		{
			name: "any fail flips FAIL",
			fr:   frameworkRow{Framework: "x", Drills: 3, PassCount: 2, FailCount: 1},
			want: "FAIL",
		},
		{
			name: "mixed pass/untested",
			fr:   frameworkRow{Framework: "x", Drills: 3, PassCount: 1, Untested: 2},
			want: "MIXED",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rollupBehavior(&c.fr); got != c.want {
				t.Errorf("rollupBehavior = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildDrillRows_NoRunsMeansNotTested(t *testing.T) {
	drills := []core.DrillSpec{
		{ID: "a", Framework: "synthetic"},
		{ID: "b", Framework: "claude_code"},
		{ID: "c", Framework: "hermes"},
	}
	rows := buildDrillRows(drills, nil)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.Behavior != "NOT_TESTED" {
			t.Errorf("drill %s: behavior = %q, want NOT_TESTED", r.DrillID, r.Behavior)
		}
	}
	// Rows are sorted by (framework, drill_id) — look up by framework.
	want := map[string]string{
		"synthetic":   "WIRED",
		"claude_code": "WIRED",
		"hermes":      "NOT_WIRED",
	}
	for _, r := range rows {
		if r.Capability != want[r.Framework] {
			t.Errorf("drill %s (framework %s): capability = %q, want %q",
				r.DrillID, r.Framework, r.Capability, want[r.Framework])
		}
	}
}

func TestBuildDrillRows_PassedRunBecomesPass(t *testing.T) {
	drills := []core.DrillSpec{
		{ID: "a", Framework: "synthetic"},
	}
	latest := []latestRun{
		{DrillID: "a", Framework: "synthetic", Status: "passed", Passed: true, StartedAt: 100, RunCount: 1},
	}
	rows := buildDrillRows(drills, latest)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Behavior != "PASS" {
		t.Errorf("behavior = %q, want PASS", rows[0].Behavior)
	}
	if rows[0].LastRunAt != 100 || rows[0].RunCount != 1 {
		t.Errorf("row fields wrong: %+v", rows[0])
	}
}

func TestBuildDrillRows_RunWithFailedVerdictBecomesFail(t *testing.T) {
	drills := []core.DrillSpec{
		{ID: "a", Framework: "synthetic"},
	}
	latest := []latestRun{
		{DrillID: "a", Framework: "synthetic", Status: "failed", Passed: false, StartedAt: 200, RunCount: 3},
	}
	rows := buildDrillRows(drills, latest)
	if rows[0].Behavior != "FAIL" {
		t.Errorf("behavior = %q, want FAIL", rows[0].Behavior)
	}
}

func TestBuildDrillRows_StatusErrorBecomesFail(t *testing.T) {
	drills := []core.DrillSpec{
		{ID: "a", Framework: "synthetic"},
	}
	latest := []latestRun{
		{DrillID: "a", Framework: "synthetic", Status: "error", Passed: false, StartedAt: 50, RunCount: 1},
	}
	rows := buildDrillRows(drills, latest)
	if rows[0].Behavior != "FAIL" {
		t.Errorf("behavior = %q, want FAIL", rows[0].Behavior)
	}
}

func TestBuildFrameworkRows_AllWiredFrameworkBecomesWired(t *testing.T) {
	drills := []core.DrillSpec{
		{ID: "a", Framework: "synthetic"},
		{ID: "b", Framework: "synthetic"},
	}
	latest := []latestRun{
		{DrillID: "a", Framework: "synthetic", Status: "passed", Passed: true, RunCount: 1},
		{DrillID: "b", Framework: "synthetic", Status: "passed", Passed: true, RunCount: 1},
	}
	drillRows := buildDrillRows(drills, latest)
	fwRows := buildFrameworkRows(drillRows)
	if len(fwRows) < 1 {
		t.Fatalf("got %d framework rows, want at least 1", len(fwRows))
	}
	var fr *frameworkRow
	for i := range fwRows {
		if fwRows[i].Framework == "synthetic" {
			fr = &fwRows[i]
		}
	}
	if fr == nil {
		t.Fatal("synthetic row missing")
	}
	if fr.Capability != "WIRED" {
		t.Errorf("capability = %q, want WIRED", fr.Capability)
	}
	if fr.Behavior != "PASS" {
		t.Errorf("behavior = %q, want PASS", fr.Behavior)
	}
	if fr.PassCount != 2 || fr.FailCount != 0 {
		t.Errorf("counts: pass=%d fail=%d, want 2/0", fr.PassCount, fr.FailCount)
	}
}

func TestBuildFrameworkRows_AnyNotWiredFlipsFramework(t *testing.T) {
	drills := []core.DrillSpec{
		{ID: "a", Framework: "hermes"},
	}
	drillRows := buildDrillRows(drills, nil)
	fwRows := buildFrameworkRows(drillRows)
	var fr *frameworkRow
	for i := range fwRows {
		if fwRows[i].Framework == "hermes" {
			fr = &fwRows[i]
		}
	}
	if fr == nil {
		t.Fatal("hermes row missing")
	}
	if fr.Capability != "NOT_WIRED" {
		t.Errorf("capability = %q, want NOT_WIRED", fr.Capability)
	}
}

func TestBuildFrameworkRows_DeclaredFrameworksAlwaysPresent(t *testing.T) {
	// No drills installed at all — the matrix still has to surface
	// every framework declared in declaredFrameworks() so the audit
	// can see "0 drills, NOT_WIRED" for hermes etc.
	fwRows := buildFrameworkRows(nil)
	declared := declaredFrameworks()
	if len(fwRows) != len(declared) {
		t.Errorf("got %d framework rows, want %d (every declared framework must surface)", len(fwRows), len(declared))
	}
	for _, fr := range fwRows {
		want, ok := declared[fr.Framework]
		if !ok {
			t.Errorf("undeclared framework %q surfaced", fr.Framework)
		}
		if fr.Capability != want {
			t.Errorf("framework %s: capability = %q, want %q", fr.Framework, fr.Capability, want)
		}
		if fr.Drills != 0 {
			t.Errorf("framework %s: drills = %d, want 0", fr.Framework, fr.Drills)
		}
	}
}

func TestParseDrillReportFlags(t *testing.T) {
	json, pd, pf, rest := parseDrillReportFlags([]string{"--json", "--per-drill", "trailing"})
	if !json || !pd || pf {
		t.Errorf("json=%v pd=%v pf=%v, want true true false", json, pd, pf)
	}
	if len(rest) != 1 || rest[0] != "trailing" {
		t.Errorf("rest = %v, want [trailing]", rest)
	}

	json, pd, pf, _ = parseDrillReportFlags([]string{"--per-framework"})
	if !pf || json || pd {
		t.Errorf("json=%v pd=%v pf=%v, want false false true", json, pd, pf)
	}
}
