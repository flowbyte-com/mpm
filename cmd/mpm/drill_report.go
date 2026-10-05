// drill_report.go — 3-axis compatibility matrix.
//
// `mpm drills report` produces the matrix that the engineering
// audit (and the user) cares about. Two axes:
//
//	Capability: WIRED | NOT_WIRED
//	  Determined by static dispatch: do we have a harness for the
//	  framework declared in the drill? A drill with framework=
//	  "hermes" returns NOT_WIRED today because the dispatcher
//	  rejects it with "unsupported framework".
//
//	Behavior: PASS | FAIL | NOT_TESTED
//	  Determined by the most recent drill_run row for that
//	  (drill_id, framework) pair. PASS means the latest run produced
//	  passed=true; FAIL means passed=false; NOT_TESTED means we
//	  have no drill_run row at all (the framework may be wired,
//	  but no real run has been recorded).
//
// NOT_TESTED is the load-bearing state: it makes the absence of
// evidence impossible to confuse with actual demonstrated
// compatibility. The matrix reports in two views:
//
//	View 1: per-drill grid
//	   drill_id            capability  behavior   last_run
//	   lesson-persistence  WIRED       PASS       2026-08-12T...
//
//	View 2: by framework
//	   framework   drills  capability  behavior
//	   synthetic   3       WIRED       PASS (3/3)
//	   claude_code 1       WIRED       NOT_TESTED
//	   hermes      0       NOT_WIRED   NOT_TESTED
//
// The two views give the audit what it needs: per-drill operational
// status, and per-framework coverage rollup.
//
// Flags:
//
//	--json           emit machine-readable JSON envelope
//	--per-drill      only show drill rows (skip framework rollup)
//	--per-framework  only show framework rows (skip drill rows)

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/mpmcli"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// drillReport is the machine-readable shape of `mpm drills report`.
// Designed so the future mpm-debug web UI can render the same axes
// from the JSON envelope without re-querying drill_runs.
type drillReport struct {
	PerDrill     []drillRow     `json:"per_drill"`
	PerFramework []frameworkRow `json:"per_framework"`
	GeneratedAt  int64          `json:"generated_at"`
}

type drillRow struct {
	DrillID    string `json:"drill_id"`
	Framework  string `json:"framework"`
	Capability string `json:"capability"` // WIRED | NOT_WIRED
	Behavior   string `json:"behavior"`   // PASS | FAIL | NOT_TESTED
	LastRunAt  int64  `json:"last_run_at,omitempty"`
	RunCount   int    `json:"run_count"`
}

type frameworkRow struct {
	Framework  string `json:"framework"`
	Drills     int    `json:"drills"`
	Capability string `json:"capability"`
	Behavior   string `json:"behavior"`
	PassCount  int    `json:"pass_count"`
	FailCount  int    `json:"fail_count"`
	Untested   int    `json:"untested"`
}

// handleDrillsReport loads the drill directory, queries the latest
// drill_run per (drill_id, framework), emits per-drill and per-framework
// matrix views, and prints them in the canonical layout.
func handleDrillsReport(args []string) int {
	jsonOutput, perDrillOnly, perFrameworkOnly, rest := parseDrillReportFlags(args)
	_ = rest

	dir := filepath.Join(mpmcli.ResolveWorkspace(), DrillsDir)
	drills, err := core.LoadAllDrills(dir)
	if err != nil {
		return usererror.Error("load drills from %s: %v", dir, err)
	}

	workspace := mpmcli.ResolveWorkspace()
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		return usererror.Error("open db: %v", err)
	}
	defer dm.Close()

	latest, err := queryLatestDrillRuns(dm.SQLDB())
	if err != nil {
		return usererror.Error("query drill_runs: %v", err)
	}

	drillRows := buildDrillRows(drills, latest)
	frameworkRows := buildFrameworkRows(drillRows)

	report := drillReport{
		PerDrill:     drillRows,
		PerFramework: frameworkRows,
		GeneratedAt:  time.Now().Unix(),
	}

	if jsonOutput {
		return emitDrillReportJSON(report)
	}

	if !perFrameworkOnly {
		if err := emitDrillReportPerDrill(drillRows); err != nil {
			return usererror.Error("%v", err)
		}
	}
	if !perDrillOnly {
		if err := emitDrillReportPerFramework(frameworkRows); err != nil {
			return usererror.Error("%v", err)
		}
	}
	return 0
}

// parseDrillReportFlags extracts --json, --per-drill, --per-framework.
// Unknown flags are passed back in `rest` for forward compatibility.
func parseDrillReportFlags(args []string) (jsonOutput, perDrillOnly, perFrameworkOnly bool, rest []string) {
	for _, a := range args {
		switch a {
		case "--json":
			jsonOutput = true
		case "--per-drill":
			perDrillOnly = true
		case "--per-framework":
			perFrameworkOnly = true
		default:
			rest = append(rest, a)
		}
	}
	return
}

// queryLatestDrillRuns returns one row per (drill_id, framework): the
// most recent row's status + verdict + started_at. NOT_TESTED comes
// from the absence of a row, which the caller derives by comparing
// against the installed-drills list.
//
// drill_runs.session_id is the orchestrator-owned canonical session
// id (§4 — drill-run session identity invariant). It stays in the
// schema and remains queryable for debugging/provenance joins
// (drill_runs ↔ tool_invocations), but it is intentionally not
// surfaced through this report query — surfacing it would require a
// product requirement to display session_ids in the matrix that does
// not exist today.
type latestRun struct {
	DrillID   string
	Framework string
	Status    string // passed | failed | error | running
	Passed    bool   // derived from verdict
	StartedAt int64
	RunCount  int
}

func queryLatestDrillRuns(db *sql.DB) ([]latestRun, error) {
	rows, err := db.Query(`
		SELECT drill_id, framework, status, verdict, started_at
		FROM drill_runs dr
		WHERE dr.started_at = (
			SELECT MAX(dr2.started_at) FROM drill_runs dr2
			WHERE dr2.drill_id = dr.drill_id AND dr2.framework = dr.framework
		)
		ORDER BY drill_id, framework`)
	if err != nil {
		return nil, fmt.Errorf("select latest: %w", err)
	}
	defer rows.Close()

	var out []latestRun
	for rows.Next() {
		var lr latestRun
		var verdictStr sql.NullString
		var status string
		if err := rows.Scan(&lr.DrillID, &lr.Framework, &status, &verdictStr, &lr.StartedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		lr.Status = status
		if verdictStr.Valid {
			var v struct {
				Passed bool `json:"passed"`
			}
			if jerr := json.Unmarshal([]byte(verdictStr.String), &v); jerr == nil {
				lr.Passed = v.Passed
			}
		}
		out = append(out, lr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Run counts per (drill_id, framework) — drives the run_count
	// column. One extra query keeps the per-drill row honest about
	// how many times we've actually executed.
	cntRows, err := db.Query(`
		SELECT drill_id, framework, COUNT(*) FROM drill_runs
		GROUP BY drill_id, framework`)
	if err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}
	defer cntRows.Close()
	counts := map[string]int{}
	for cntRows.Next() {
		var d, f string
		var n int
		if err := cntRows.Scan(&d, &f, &n); err != nil {
			return nil, fmt.Errorf("count scan: %w", err)
		}
		counts[d+"\x00"+f] = n
	}
	for i := range out {
		out[i].RunCount = counts[out[i].DrillID+"\x00"+out[i].Framework]
	}
	return out, nil
}

// buildDrillRows merges the installed-drills list with the latest-run
// table. Drills with no rows are NOT_TESTED; drills with rows get
// PASS/FAIL based on the verdict. Capability is determined by the
// static harness-dispatch table.
func buildDrillRows(drills []core.DrillSpec, latest []latestRun) []drillRow {
	idx := map[string]latestRun{}
	for _, lr := range latest {
		idx[lr.DrillID+"\x00"+lr.Framework] = lr
	}

	out := make([]drillRow, 0, len(drills))
	for _, d := range drills {
		key := d.ID + "\x00" + d.Framework
		lr, hasRun := idx[key]
		row := drillRow{
			DrillID:    d.ID,
			Framework:  d.Framework,
			Capability: harnessCapability(d.Framework),
		}
		if !hasRun {
			row.Behavior = "NOT_TESTED"
		} else {
			row.Behavior = "NOT_TESTED"
			if lr.Status == "passed" && lr.Passed {
				row.Behavior = "PASS"
			} else if lr.Status == "failed" || lr.Status == "error" {
				row.Behavior = "FAIL"
			}
			row.LastRunAt = lr.StartedAt
			row.RunCount = lr.RunCount
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Framework != out[j].Framework {
			return out[i].Framework < out[j].Framework
		}
		return out[i].DrillID < out[j].DrillID
	})
	return out
}

// buildFrameworkRows rolls the per-drill rows up by framework. The
// rollup capability is the WORST of any drill for that framework
// (NOT_WIRED wins over WIRED) — a framework is "capable" only if
// every drill for it can dispatch. The behavior is the rollup of
// per-drill pass/fail counts.
func buildFrameworkRows(rows []drillRow) []frameworkRow {
	byFw := map[string]*frameworkRow{}
	for _, r := range rows {
		fr, ok := byFw[r.Framework]
		if !ok {
			fr = &frameworkRow{Framework: r.Framework, Capability: "WIRED"}
			byFw[r.Framework] = fr
		}
		fr.Drills++
		if r.Capability == "NOT_WIRED" {
			fr.Capability = "NOT_WIRED"
		}
		switch r.Behavior {
		case "PASS":
			fr.PassCount++
		case "FAIL":
			fr.FailCount++
		default:
			fr.Untested++
		}
	}
	// Include frameworks that have no drills installed but that the
	// orchestrator knows about. This is the "intent vs. evidence"
	// surface: a row like "hermes 0 NOT_WIRED" tells the reader that
	// the framework was at least considered at the harness layer.
	for fw, cap := range declaredFrameworks() {
		if _, ok := byFw[fw]; !ok {
			byFw[fw] = &frameworkRow{
				Framework:  fw,
				Drills:     0,
				Capability: cap,
				Behavior:   "NOT_TESTED",
			}
		}
	}

	out := make([]frameworkRow, 0, len(byFw))
	for _, fr := range byFw {
		fr.Behavior = rollupBehavior(fr)
		out = append(out, *fr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Framework < out[j].Framework })
	return out
}

// rollupBehavior picks the framework-level status from pass/fail
// counts. NOT_TESTED dominates when no drill has been run; otherwise
// PASS dominates when all runs passed; FAIL when at least one did.
func rollupBehavior(fr *frameworkRow) string {
	if fr.Drills == 0 {
		return "NOT_TESTED"
	}
	if fr.Untested == fr.Drills {
		return "NOT_TESTED"
	}
	if fr.FailCount > 0 {
		return "FAIL"
	}
	if fr.PassCount == fr.Drills {
		return "PASS"
	}
	return "MIXED"
}

// declaredFrameworks returns the canonical list of frameworks the
// orchestrator recognises. Adding a row to dispatchDrillCLI without
// adding it here makes the report incomplete — these are the rows
// the matrix MUST surface so the audit can say "0 drills, NOT_WIRED"
// for a framework we at least considered.
func declaredFrameworks() map[string]string {
	return map[string]string{
		"synthetic":   "WIRED",
		"claude_code": "WIRED",
		"hermes":      "NOT_WIRED",
		"openclaw":    "NOT_WIRED",
		"opencode":    "NOT_WIRED",
		"pi":          "NOT_WIRED",
	}
}

// harnessCapability returns WIRED if we have a harness for the
// framework, NOT_WIRED otherwise. The dispatcher is the source of
// truth — if dispatchDrillCLI's switch would reject the framework,
// the report says NOT_WIRED.
func harnessCapability(framework string) string {
	switch framework {
	case "synthetic", "claude_code":
		return "WIRED"
	}
	return "NOT_WIRED"
}

func emitDrillReportPerDrill(rows []drillRow) error {
	if len(rows) == 0 {
		fmt.Println("no drills installed")
		return nil
	}
	fmt.Println("Per-drill matrix:")
	fmt.Println()
	fmt.Printf("%-32s %-14s %-12s %-12s %-8s %s\n",
		"DRILL_ID", "FRAMEWORK", "CAPABILITY", "BEHAVIOR", "RUNS", "LAST_RUN")
	fmt.Println(strings.Repeat("-", 100))
	for _, r := range rows {
		lastRun := "—"
		if r.LastRunAt > 0 {
			lastRun = time.Unix(r.LastRunAt, 0).UTC().Format("2006-01-02T15:04:05Z")
		}
		fmt.Printf("%-32s %-14s %-12s %-12s %-8d %s\n",
			r.DrillID, r.Framework, r.Capability, r.Behavior, r.RunCount, lastRun)
	}
	fmt.Println()
	return nil
}

func emitDrillReportPerFramework(rows []frameworkRow) error {
	if len(rows) == 0 {
		return nil
	}
	fmt.Println("Per-framework rollup:")
	fmt.Println()
	fmt.Printf("%-14s %-8s %-12s %-12s %-8s %-8s %-8s\n",
		"FRAMEWORK", "DRILLS", "CAPABILITY", "BEHAVIOR", "PASS", "FAIL", "UNTESTED")
	fmt.Println(strings.Repeat("-", 80))
	for _, r := range rows {
		fmt.Printf("%-14s %-8d %-12s %-12s %-8d %-8d %-8d\n",
			r.Framework, r.Drills, r.Capability, r.Behavior,
			r.PassCount, r.FailCount, r.Untested)
	}
	fmt.Println()
	return nil
}

func emitDrillReportJSON(report drillReport) int {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return usererror.Error("marshal report: %v", err)
	}
	fmt.Println(string(b))
	return 0
}
