// drill_handler.go — scheduler-side executor for behavioural drills.
//
// The drill pipeline:
//
//	Wake (kind=drill)
//	  │
//	  ▼
//	DrillHandler
//	  │   1. Resolve workspace, open DatabaseManager.
//	  │   2. Insert drill_runs row, status='running'.
//	  │   3. LoadDrill(spec YAML) → DrillSpec.
//	  │   4. Dispatch by spec.Framework:
//	  │      • synthetic    → SyntheticHarness.Run + shell `mpm call` per call
//	  │      • claude_code  → ClaudeCodeHarness.Begin + Finish (subprocess)
//	  │   5. Read tool_invocations WHERE session_id.
//	  │   6. Score(drill, invocations) → Verdict.
//	  │   7. Update drill_runs row with verdict + status.
//
// Why framework dispatch lives here and not in core.LoadDrill: a single
// drill YAML is the contract; the runtime decision of "who executes
// this contract" is scheduler policy.

package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/mpmcli"
)

// DrillHandler is registered as a system wake under kind="drill".
//
// Required wake metadata:
//   - drill_id    : string, identifier matching the YAML's id field
//   - path        : string, absolute path to the drill YAML on disk
//   - compliant   : bool,   true=compliant harness run, false=non-compliant
//     (only meaningful for synthetic harness; claude_code
//     always runs the agent against the prompt normally)
//
// Errors return non-nil but the wake is still marked fired=1 — the
// scheduler's contract is "failures surface but do not block other
// wakes".
func DrillHandler(ctx context.Context, w Wake) error {
	workspace := mpmcli.ResolveWorkspace()
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		return fmt.Errorf("open dm: %w", err)
	}
	defer dm.Close()

	drillID, _ := w.Metadata["drill_id"].(string)
	path, _ := w.Metadata["path"].(string)
	compliant, _ := w.Metadata["compliant"].(bool)

	if path == "" {
		return fmt.Errorf("drill wake %s missing path metadata", w.ID)
	}
	drill, err := core.LoadDrill(path)
	if err != nil {
		return fmt.Errorf("load drill: %w", err)
	}
	if drillID != "" && drill.ID != drillID {
		return fmt.Errorf("drill_id mismatch: wake=%s yaml=%s", drillID, drill.ID)
	}

	runID := uuid.NewString()
	sessionID := uuid.NewString()
	startedAt := time.Now().Unix()

	// F-3: thread the dispatch ctx into DB writes so the mpm-lint
	// ctx-in-scope-missing check passes (ctx is in scope and now used)
	// and so cancellation propagates to the SQLite write path. Use
	// WithTx for the single-statement write per CLAUDE.md single-conn
	// discipline (the canonical pattern is WithTx for any bare Exec
	// path; ExecTracked wraps the bare *sql.DB call).
	if err := dm.WithTx(func(node core.DBNode) error {
		_, err := node.ExecTracked(`
			INSERT INTO drill_runs
			    (id, drill_id, framework, session_id, status, started_at)
			VALUES (?, ?, ?, ?, 'running', ?)`,
			0,
			runID, drill.ID, drill.Framework, sessionID, startedAt,
		)
		if err != nil {
			return fmt.Errorf("insert drill_run: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Dispatch by framework. Apply the per-framework timeout as a
	// child of the dispatch ctx so SIGTERM still cancels the drill
	// (pre-fix this was derived from Background, masking the dispatch
	// cancel signal — F-3 closes that hole).
	timeoutSecs := drill.TimeoutSecs
	if timeoutSecs <= 0 {
		timeoutSecs = core.DefaultDrillTimeout(drill.Framework)
	}
	dispatchCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	invocations, runErr := dispatchDrill(dispatchCtx, dm, *drill, sessionID, compliant)
	if runErr != nil {
		return updateDrillRunError(dm, runID, runErr)
	}

	// Score from the audit table — Score is pure (no agent trust).
	verdict := core.Score(*drill, invocations, dm.SQLDB())
	verdictJSON, _ := json.Marshal(verdict)
	completedAt := time.Now().Unix()
	durationMs := (completedAt - startedAt) * 1000
	status := "passed"
	if !verdict.Passed {
		status = "failed"
	}

	if err := dm.WithTx(func(node core.DBNode) error {
		_, err := node.ExecTracked(`
			UPDATE drill_runs
			SET status = ?, verdict = ?, completed_at = ?, duration_ms = ?
			WHERE id = ?`,
			0,
			status, string(verdictJSON), completedAt, durationMs, runID,
		)
		if err != nil {
			return fmt.Errorf("update drill_run: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// dispatchDrill routes to the harness matching drill.Framework. The
// synthetic path shells `mpm call` once per emitted call so audit rows
// land in tool_invocations (same evidence shape the real-framework
// harness produces). The claude_code path spawns the agent and lets
// the MCP-audit hook write its own audit rows.
func dispatchDrill(
	ctx context.Context,
	dm *core.DatabaseManager,
	drill core.DrillSpec,
	sessionID string,
	compliant bool,
) ([]core.ToolCall, error) {
	switch drill.Framework {
	case "synthetic":
		return runSyntheticDrill(ctx, dm, drill, sessionID, compliant)
	case "claude_code":
		return runClaudeCodeDrill(ctx, dm, drill, sessionID)
	default:
		return nil, fmt.Errorf("unsupported framework %q; add a harness implementation or update the matrix to mark it UNWIRED", drill.Framework)
	}
}

// runSyntheticDrill emits the harness's sequence and shells `mpm call`
// per call so the audit hook populates tool_invocations with the same
// session_id. Returns the ToolCalls the scorer consumes, ordered as
// the harness produced them.
func runSyntheticDrill(
	ctx context.Context,
	dm *core.DatabaseManager,
	drill core.DrillSpec,
	sessionID string,
	compliant bool,
) ([]core.ToolCall, error) {
	h := core.NewSyntheticHarness()
	calls, _, err := h.Run(ctx, drill, compliant)
	if err != nil {
		return nil, err
	}
	for _, c := range calls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(c.Payload)
		cmd := exec.CommandContext(ctx, "mpm", "call", c.ToolName,
			"--payload", string(payload),
		)
		cmd.Env = append(os.Environ(), "MPM_SESSION_ID="+sessionID)
		// Audit hook fires inside mpm call; we ignore the cmd exit
		// because Score() inspects the audit rows, not the call
		// success flag — a "policy-violating" tool (forbidden
		// action) still logs to tool_invocations with success/error
		// depending on its handler, which is exactly what the
		// forbidden-check needs to detect.
		_ = cmd.Run()
	}
	return calls, nil
}

// runClaudeCodeDrill spawns Claude Code with the drill prompt pointed
// at the local mpm-mcp. The harness's MCP config sets MPM_SESSION_ID,
// so every audit row written during this run tags itself with the
// session_id we just minted. After Claude Code exits, we read the
// audit rows by session_id and return them as ToolCalls.
//
// sessionID is the orchestrator-owned canonical session_id (drill-
// run session identity invariant, §4): the harness MUST receive the
// same id we stored in drill_runs.session_id so every audit row
// joins back to its drill_run via session_id. The harness no longer
// mints its own (drill_claude_code_harness.go §6) — passing the
// caller's id here is the only way the contract is closed.
func runClaudeCodeDrill(
	ctx context.Context,
	dm *core.DatabaseManager,
	drill core.DrillSpec,
	sessionID string,
) ([]core.ToolCall, error) {
	workspace := mpmcli.ResolveWorkspace()
	h := core.NewClaudeCodeHarness(workspace, "", "")
	if _, err := h.Launch(ctx, drill, sessionID); err != nil {
		return nil, fmt.Errorf("claude launch: %w", err)
	}
	calls, err := h.Finish(ctx, dm.SQLDB())
	if err != nil {
		return nil, fmt.Errorf("claude finish: %w", err)
	}
	return calls, nil
}

// updateDrillRunError stamps a drill_runs row with status='error' and
// the diagnostic string. Best-effort — audit failure here cannot block
// the scheduler.
func updateDrillRunError(dm *core.DatabaseManager, runID string, err error) error {
	_, e := dm.SQLDB().Exec(`
		UPDATE drill_runs SET status='error', error_message=?, completed_at=?
		WHERE id=?`, err.Error(), time.Now().Unix(), runID)
	if e != nil {
		return fmt.Errorf("update error state: %w (original: %v)", e, err)
	}
	return err
}
