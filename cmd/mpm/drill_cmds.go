// drill_cmds.go — `mpm drills <subcommand>` CLI surface.
//
// Subcommands:
//
//	list         — list installed drills under $MPM_WORKSPACE/drills/
//	show <id>    — print the full drill YAML
//	run <id>     — execute a drill (synthetic or claude_code); writes
//	               a drill_runs row and prints the verdict
//	report       — 3-axis compatibility matrix (Capability × Behavior)
//	               — see drill_report.go
//
// Architectural role: this is the operator-facing entry point for the
// drill engine. It exists so the engine can be driven without
// mpm-scheduler (e.g. ad-hoc verification after a schema migration).
// Production cadence-style execution lives in internal/scheduler and
// flows through the wake contract; the CLI is the on-demand path.
//
// All user-facing errors go through usererror.Error so the test
// TestNoNewDirectStderrWrites stays green. stdout is fine for the
// success-path payloads (verdict, call list).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/mpmcli"
	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/google/uuid"
)

// DrillsDir is the canonical location for drill YAML specs. Relative
// to the workspace root so a workspace-scoped run picks up user-edited
// drills; the loader itself doesn't care about the directory.
const DrillsDir = "drills"

// handleDrills routes `mpm drills <subcommand>`.
//
// Usage displayed on unknown subcommand — keeps the help text together
// with the dispatcher so the CLI catalogue can stay terse.
func handleDrills(args []string) int {
	if len(args) < 2 {
		printDrillsHelp()
		return 0
	}
	sub := args[1]
	rest := args[2:]
	switch sub {
	case "list":
		return handleDrillsList(rest)
	case "show":
		return handleDrillsShow(rest)
	case "run":
		return handleDrillsRun(rest)
	case "report":
		return handleDrillsReport(rest)
	case "help", "--help", "-h":
		printDrillsHelp()
		return 0
	default:
		return usererror.Error("mpm drills: unknown subcommand %q", sub)
	}
}

func printDrillsHelp() {
	fmt.Println()
	fmt.Println("mpm drills — behavioural drill execution + compatibility matrix")
	fmt.Println()
	fmt.Println("Usage: mpm drills <subcommand>")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  list                List installed drills (id, framework, description)")
	fmt.Println("  show <id>           Print the full drill YAML")
	fmt.Println("  run <id>            Execute a drill and print the verdict")
	fmt.Println("                      (--framework <f>    override framework dispatch")
	fmt.Println("                       --no-compliant    run synthetic harness in non-compliant mode")
	fmt.Println("                       --timeout <secs>  override timeout_secs)")
	fmt.Println("  report              3-axis compatibility matrix (Capability × Behavior)")
	fmt.Println("  help                Show this help")
	fmt.Println()
	fmt.Println("Drills live in $MPM_WORKSPACE/drills/*.yaml.")
	fmt.Println()
}

// handleDrillsList enumerates the drills directory and prints a
// one-line summary per drill. The full spec is available via `show`.
func handleDrillsList(args []string) int {
	dir := filepath.Join(mpmcli.ResolveWorkspace(), DrillsDir)
	drills, err := core.LoadAllDrills(dir)
	if err != nil {
		return usererror.Error("load drills from %s: %v", dir, err)
	}
	if len(drills) == 0 {
		fmt.Printf("no drills found in %s\n", dir)
		return 0
	}
	fmt.Printf("%-32s %-14s %s\n", "ID", "FRAMEWORK", "DESCRIPTION")
	fmt.Println(strings.Repeat("-", 80))
	for _, d := range drills {
		fmt.Printf("%-32s %-14s %s\n", d.ID, d.Framework, d.Description)
	}
	return 0
}

// handleDrillsShow prints the on-disk YAML for a single drill ID.
// Walks $MPM_WORKSPACE/drills/ matching by id field (filename may
// differ from the id — drill YAML id is the contract).
func handleDrillsShow(args []string) int {
	if len(args) < 1 {
		return usererror.Error("drills show requires a drill id")
	}
	wantID := args[0]
	path, err := findDrillPath(wantID)
	if err != nil {
		return usererror.Error("%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return usererror.Error("read %s: %v", path, err)
	}
	fmt.Print(string(data))
	return 0
}

// handleDrillsRun executes one drill synchronously and prints the
// verdict. Mirrors scheduler.DrillHandler but driven directly by the
// CLI instead of the wake machinery — useful for one-off verification
// after a schema change or harness update.
//
// Flags:
//
//	--framework <f>    override the YAML's framework (e.g. test a
//	                   synthetic spec against claude_code)
//	--no-compliant     synthetic harness drops the first step
//	                   (sequence-break test)
//	--timeout <secs>   override the YAML's timeout_secs
//
// Always inserts a drill_runs row so `mpm drills report` can see the
// outcome (both successes and failures surface in the matrix).
func handleDrillsRun(args []string) int {
	if len(args) < 1 {
		return usererror.Error("drills run requires a drill id")
	}
	drillID := args[0]
	rest := args[1:]

	framework, compliant, timeout, rest := parseDrillRunFlags(rest)
	_ = rest

	path, err := findDrillPath(drillID)
	if err != nil {
		return usererror.Error("%v", err)
	}
	drill, err := core.LoadDrill(path)
	if err != nil {
		return usererror.Error("load drill: %v", err)
	}
	if framework != "" {
		drill.Framework = framework
	}
	if timeout > 0 {
		drill.TimeoutSecs = timeout
	}

	workspace := mpmcli.ResolveWorkspace()
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		return usererror.Error("open db: %v", err)
	}
	defer dm.Close()

	runID := uuid.NewString()
	sessionID := uuid.NewString()
	startedAt := time.Now().Unix()
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO drill_runs (id, drill_id, framework, session_id, status, started_at)
		VALUES (?, ?, ?, ?, 'running', ?)`,
		runID, drill.ID, drill.Framework, sessionID, startedAt,
	); err != nil {
		return usererror.Error("insert drill_run: %v", err)
	}

	timeoutSecs := drill.TimeoutSecs
	if timeoutSecs <= 0 {
		timeoutSecs = core.DefaultDrillTimeout(drill.Framework)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	calls, runErr := dispatchDrillCLI(ctx, dm, *drill, sessionID, compliant)
	if runErr != nil {
		_, _ = dm.SQLDB().Exec(`
			UPDATE drill_runs SET status='error', error_message=?, completed_at=?
			WHERE id=?`, runErr.Error(), time.Now().Unix(), runID)
		return usererror.Error("drill run failed: %v", runErr)
	}

	verdict := core.Score(*drill, calls, dm.SQLDB())
	verdictJSON, _ := json.Marshal(verdict)
	completedAt := time.Now().Unix()
	status := "passed"
	if !verdict.Passed {
		status = "failed"
	}
	if _, err := dm.SQLDB().Exec(`
		UPDATE drill_runs SET status=?, verdict=?, completed_at=?, duration_ms=?
		WHERE id=?`,
		status, string(verdictJSON), completedAt, (completedAt-startedAt)*1000, runID,
	); err != nil {
		return usererror.Error("update drill_run: %v", err)
	}

	fmt.Printf("drill=%s framework=%s status=%s\n", drill.ID, drill.Framework, status)
	fmt.Printf("verdict=%s\n", string(verdictJSON))
	fmt.Printf("calls=%d\n", len(calls))
	for _, c := range calls {
		fmt.Printf("  - %s:%s\n", c.ToolName, c.Action)
	}
	if !verdict.Passed {
		return 1
	}
	return 0
}

// parseDrillRunFlags extracts --framework, --no-compliant, and
// --timeout from the args passed to `mpm drills run`. Other args
// (currently none) are returned in `rest` for future expansion.
func parseDrillRunFlags(args []string) (framework string, compliant bool, timeout int, rest []string) {
	compliant = true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--framework":
			if i+1 < len(args) {
				i++
				framework = args[i]
			}
		case "--no-compliant":
			compliant = false
		case "--timeout":
			if i+1 < len(args) {
				i++
				_, _ = fmt.Sscanf(args[i], "%d", &timeout)
			}
		default:
			rest = append(rest, a)
		}
	}
	return
}

// findDrillPath locates the YAML file for a given drill id. The id
// field is the contract; the filename may be anything *.yaml. We
// iterate the directory to keep the lookup robust against renames.
func findDrillPath(drillID string) (string, error) {
	dir := filepath.Join(mpmcli.ResolveWorkspace(), DrillsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w (set MPM_WORKSPACE or create the drills/ directory)", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		spec, err := core.LoadDrill(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if spec.ID == drillID {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("drill %q not found in %s", drillID, dir)
}

// dispatchDrillCLI mirrors scheduler.dispatchDrill but lives in the
// CLI to avoid a shared-package churn for what's logically a tiny
// dispatch (the scheduler path is the same shape but uses a Wake).
//
// Note: keep the two implementations in sync. The first place we
// noticed divergence was the missing-tool error path; flagging here
// so the next harness (Hermes, OpenClaw, OpenCode, Pi) lands in
// both at once.
func dispatchDrillCLI(
	ctx context.Context,
	dm *core.DatabaseManager,
	drill core.DrillSpec,
	sessionID string,
	compliant bool,
) ([]core.ToolCall, error) {
	switch drill.Framework {
	case "synthetic":
		return runSyntheticDrillCLI(ctx, drill, sessionID, compliant)
	case "claude_code":
		return runClaudeCodeDrillCLI(ctx, drill, sessionID)
	default:
		return nil, fmt.Errorf("unsupported framework %q; add a harness implementation or run `mpm drills report` to mark it UNWIRED", drill.Framework)
	}
}

// runSyntheticDrillCLI emits the harness's sequence and shells
// `mpm call` per call so the audit hook populates tool_invocations
// with the same session_id. The CLI uses the local resolved binary
// (`os.Executable`) if available, falling back to whatever `mpm`
// resolves on PATH.
func runSyntheticDrillCLI(
	ctx context.Context,
	drill core.DrillSpec,
	sessionID string,
	compliant bool,
) ([]core.ToolCall, error) {
	h := core.NewSyntheticHarness()
	calls, _, err := h.Run(ctx, drill, compliant)
	if err != nil {
		return nil, err
	}
	mpmBin, _ := os.Executable()
	if resolveErr := exec.Command(mpmBin, "version").Run(); resolveErr != nil {
		mpmBin = "mpm" // fall back to PATH
	}
	for _, c := range calls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(c.Payload)
		cmd := exec.CommandContext(ctx, mpmBin, "call", c.ToolName,
			"--payload", string(payload))
		cmd.Env = append(os.Environ(), "MPM_SESSION_ID="+sessionID)
		// Audit hook fires inside `mpm call`. We ignore cmd exit
		// because Score() inspects the rows, not the call success
		// flag — a "policy-violating" tool (forbidden action) still
		// logs to tool_invocations with success/error depending on
		// its handler, which is exactly the forbidden check needs.
		_ = cmd.Run()
	}
	return calls, nil
}

// runClaudeCodeDrillCLI spawns Claude Code with the drill prompt
// pointed at the local mpm-mcp. The harness's MCP config sets
// MPM_SESSION_ID, so every audit row written during this run tags
// itself with the session_id we just minted.
//
// sessionID is the orchestrator-owned canonical session_id (drill-
// run session identity invariant, §4): the harness MUST receive the
// same id we stored in drill_runs.session_id so every audit row
// joins back to its drill_run via session_id. The harness no longer
// mints its own (drill_claude_code_harness.go §6) — passing the
// caller's id here is the only way the contract is closed. The
// CLI's drill_runs row is written at handleDrillsRun's INSERT above
// with this same sessionID, so the two stay aligned.
func runClaudeCodeDrillCLI(
	ctx context.Context,
	drill core.DrillSpec,
	sessionID string,
) ([]core.ToolCall, error) {
	workspace := mpmcli.ResolveWorkspace()
	h := core.NewClaudeCodeHarness(workspace, "", "")
	if _, err := h.Launch(ctx, drill, sessionID); err != nil {
		return nil, fmt.Errorf("claude launch: %w", err)
	}
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		return nil, fmt.Errorf("open dm for finish: %w", err)
	}
	defer dm.Close()
	calls, err := h.Finish(ctx, dm.SQLDB())
	if err != nil {
		return nil, fmt.Errorf("claude finish: %w", err)
	}
	return calls, nil
}
