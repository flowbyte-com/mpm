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

package main

import "fmt"

// handleDrillsReport is implemented in Task 11 (3-axis compatibility
// matrix). For now it returns a placeholder so the CLI dispatches.
func handleDrillsReport(args []string) int {
	fmt.Println("drill report: pending Task 11 implementation")
	return 0
}
