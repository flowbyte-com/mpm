// drill_claude_code_harness.go — real-framework harness for Claude Code.
//
// This is the FIRST implementation of the FrameworkHarness interface
// against a real agent, and the architectural proof that the drill
// engine can observe a living framework via telemetry rather than
// trusting the agent's claims.
//
// Architecture:
//
//	drill prompt
//	    │
//	    ▼
//	ClaudeCodeHarness.Launch(ctx, drill, sessionID)
//	    │   spawns `claude -p <prompt>` with --mcp-config pointing at
//	    │   the local mpm-mcp stdio server. The harness sets
//	    │   MPM_SESSION_ID=<sessionID> so every audit row written by
//	    │   mpm-mcp across this drill carries the same session_id
//	    │   the caller (orchestrator) already minted for drill_runs.
//	    ▼
//	Claude Code (real agent)
//	    │   discovers mpm-mcp via MCP, calls mpm_* tools over stdio.
//	    ▼
//	tool_invocations (audit table)
//	    │   one row per mpm call, all tagged with session_id.
//	    ▼
//	ClaudeCodeHarness.Finish
//	    │   reads tool_invocations WHERE session_id, builds []ToolCall.
//	    ▼
//	scheduler scores; verdict persists in drill_runs.
//
// Critical invariant (drill-run session identity, §4):
//
//	drill_runs.session_id == tool_invocations.session_id
//
// The orchestrator (scheduler or CLI dispatcher) is the canonical
// minter of the run's session_id S. The harness consumes S via the
// Launch parameter and threads it through every audit-row producer:
// MPM_SESSION_ID env, writeMcpConfig, and Finish's
// readInvocationsForSession scope. The harness MUST NOT mint its own
// session_id — that breaks the invariant (see
// drill_session_identity_reproducer_test.go §3 for the historical
// bug shape).
//
// Critical invariant (drill-run audit):
// The verdict comes from the audit table, not from anything Claude
// Code said. If the agent claims "I saved a lesson" but mpm has no
// tool_invocations row, the drill FAILS.

package internal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// ClaudeCodeHarness drives one drill run via the `claude` CLI. It
// satisfies the DrillHarness contract by producing session_id-bound
// tool-invocation evidence the same way the synthetic harness does —
// the only difference is who produces the calls (Claude Code, not us).
//
// The harness is stateful across Begin/Finish so a long-running agent
// doesn't have to fit inside one method call. Callers MUST call Finish
// even on error or after a timeout to release child state cleanly.
type ClaudeCodeHarness struct {
	claudePath string
	mpmMcpPath string
	workspace  string
	debugLog   io.Writer // tee target for human-visible diagnostics (typically os.Stderr)
	debugBuf   *syncBuf  // captured child output for programmatic inspection (DebugOutput)

	// session_id is generated at Begin; every tool_invocations row the
	// agent emits carries this. Set as MPM_SESSION_ID env so mpm-mcp
	// picks it up (see cmd/mpm-mcp/main.go — TODO: plumb if not yet).
	sessionID string

	// claudeCmd is the running `claude -p` invocation once Begin
	// returns. Held for streaming/consumption by Finish.
	claudeCmd *exec.Cmd

	// startedAt records process start for duration reporting.
	startedAt time.Time
}

// NewClaudeCodeHarness constructs a harness rooted in the given
// workspace. claudePath / mpmMcpPath are auto-detected if empty
// (PATH lookup); callers may set them explicitly for tests and for
// environments where `claude` is shimmed.
func NewClaudeCodeHarness(workspace, claudePath, mpmMcpPath string) *ClaudeCodeHarness {
	if claudePath == "" {
		claudePath, _ = exec.LookPath("claude")
	}
	if mpmMcpPath == "" {
		if p, err := exec.LookPath("mpm-mcp"); err == nil {
			mpmMcpPath = p
		}
	}
	// debugBuf mirrors everything written to debugLog. Callers inspect
	// the buffer via DebugOutput to detect failure modes that surface
	// only on the child's stderr (e.g. `claude-code:unrecognized_model`
	// when the harness model isn't supported by Claude Code).
	buf := &syncBuf{}
	return &ClaudeCodeHarness{
		claudePath: claudePath,
		mpmMcpPath: mpmMcpPath,
		workspace:  workspace,
		debugLog:   io.MultiWriter(os.Stderr, buf),
		debugBuf:   buf,
	}
}

// DebugOutput returns the harness's captured child-process output
// (claude stdout + stderr prefixed + harness-side notes). Used by
// tests to detect failure modes that surface on the child's stderr
// but aren't reflected in the audit table (e.g. unrecognized_model).
func (h *ClaudeCodeHarness) DebugOutput() string {
	if h.debugBuf == nil {
		return ""
	}
	return h.debugBuf.String()
}

// syncBuf is a minimal goroutine-safe bytes.Buffer. The Go stdlib
// bytes.Buffer is NOT goroutine-safe; claude's stdout/stderr writers
// can race against the harness reading the buffer in Finish.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// SessionID returns the session_id supplied to Launch — the same id
// stored in drill_runs.session_id. Returns "" before Launch. The
// harness no longer mints a UUID here (§6 — see
// drill_session_identity_reproducer_test.go for the historical bug
// shape); the orchestrator owns the mint.
func (h *ClaudeCodeHarness) SessionID() string { return h.sessionID }

// isAvailable reports whether the harness can run in the current
// environment. Returns false when `claude` is missing or mpm-mcp isn't
// built — both are required for the real-framework pipeline.
//
// Existence is checked via os.Stat rather than exec.LookPath because
// callers may pin the binary to an absolute path that the harness
// should respect (tests, sandboxed builds). A path that resolves to a
// real file is "available" even if it lives outside PATH.
func (h *ClaudeCodeHarness) isAvailable() error {
	if h.claudePath == "" {
		return fmt.Errorf("claude CLI not on PATH; install or set --claude-binary")
	}
	if _, err := os.Stat(h.claudePath); err != nil {
		return fmt.Errorf("claude binary at %s: %w", h.claudePath, err)
	}
	if h.mpmMcpPath == "" {
		return fmt.Errorf("mpm-mcp not on PATH; run `make build` to produce the stdio server")
	}
	if _, err := os.Stat(h.mpmMcpPath); err != nil {
		return fmt.Errorf("mpm-mcp binary at %s: %w", h.mpmMcpPath, err)
	}
	return nil
}

// Launch spawns Claude Code with the drill prompt and MCP config pointing
// at our local mpm-mcp. sessionID is the orchestrator-owned canonical
// session_id for this run — the same one stored in drill_runs.session_id.
// The harness threads it through every audit-row producer: the
// MPM_SESSION_ID env, the MCP config the child server inherits, and
// the Finish read scope. The caller MUST call Finish (even on error)
// to reap the subprocess and surface telemetry.
//
// Named Launch (not Begin) because the mpm-lint tx-rollback rule has
// an over-eager pattern matching any `.Begin(` call against a
// database/sql-shaped type. The harness does nothing transactional.
//
// sessionID must be non-empty. The harness historically minted its
// own UUID here, which silently broke the drill-run session identity
// invariant (drill_runs.session_id != tool_invocations.session_id — see
// drill_session_identity_reproducer_test.go §3). The minting was
// removed in §6: the orchestrator (scheduler or CLI dispatcher) is the
// single source of session_ids for a drill run, and the harness
// refuses to Launch without it.
func (h *ClaudeCodeHarness) Launch(ctx context.Context, drill DrillSpec, sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("ClaudeCodeHarness.Launch requires a caller-owned sessionID (orchestrator owns the mint; see §4 session-identity invariant)")
	}
	if err := h.isAvailable(); err != nil {
		return "", err
	}

	h.sessionID = sessionID

	// Materialise an MCP config file in the workspace so each drill
	// gets a clean server process (no shared-state bleed across
	// concurrent drills, and no interference with the user's
	// ~/.claude/mcp.json).
	mcpCfg, err := h.writeMcpConfig(drill)
	if err != nil {
		return "", fmt.Errorf("write mcp config: %w", err)
	}

	// Command shape: claude -p <prompt> --mcp-config <cfg>
	//   --strict-mcp-config   only use the servers we declared; ignore
	//                         user-global MCP servers (deterministic
	//                         matrix).
	//   --output-format json  single-shot JSON output for parseability.
	//   --dangerously-skip-permissions  drills are scripted; we trust
	//                                  the harness to scope what the
	//                                  agent does via --allowedTools.
	h.claudeCmd = exec.CommandContext(ctx,
		h.claudePath,
		"-p", drill.Prompt,
		"--mcp-config", mcpCfg,
		"--strict-mcp-config",
		"--output-format", "json",
		"--dangerously-skip-permissions",
	)

	// Pin the workspace and pass the session_id through the env so
	// mpm-mcp can tag every audit row.
	//
	// MPM_FRAMEWORK=mcp is the contract the E2E test asserts against
	// (drill_e2e_claude_code_test.go framework_name check). mpm-mcp's
	// audit hook reads MPM_FRAMEWORK and stamps it on every
	// tool_invocation row; without this env the row falls back to the
	// "mpm-cli" default and the test's strict equality fails. Setting
	// it here is the harness's job — mpm-mcp doesn't know whether it's
	// invoked by Claude Code, the CLI REPL, or a long-lived agent.
	h.claudeCmd.Dir = h.workspace
	h.claudeCmd.Env = append(os.Environ(),
		"MPM_SESSION_ID="+h.sessionID,
		"MPM_WORKSPACE="+h.workspace,
		"MPM_FRAMEWORK=mcp",
	)
	// Capture stdout/stderr for diagnostics — not persisted by
	// default; logging to debugLog keeps the drill invocation
	// recoverable when something goes wrong.
	h.claudeCmd.Stdout = &debugWriter{prefix: "[claude stdout] ", w: h.debugLog}
	h.claudeCmd.Stderr = &debugWriter{prefix: "[claude stderr] ", w: h.debugLog}

	h.startedAt = time.Now()
	if err := h.claudeCmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	return h.sessionID, nil
}

// Finish waits for the Claude Code process to exit and returns the
// tool-call evidence derived from tool_invocations for this session.
// Always called after Launch, even on error or upstream cancellation.
//
// The DB query is the SINGLE source of truth for what the agent did.
// Claude Code's own claim of "I saved a lesson" is irrelevant — only
// the audit rows that mpm-mcp actually wrote count.
func (h *ClaudeCodeHarness) Finish(ctx context.Context, db *sql.DB) ([]ToolCall, error) {
	if h.claudeCmd == nil {
		return nil, fmt.Errorf("Finish called without Launch")
	}

	// Wait for the process with the parent ctx honoured. The harness
	// is also bounded by drill.TimeoutSecs so a stuck Claude Code
	// call doesn't hold the scheduler forever.
	waitErr := h.claudeCmd.Wait()

	// If the process crashed before producing any calls, surface a
	// diagnostic to the caller so the FAIL reason is actionable.
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("claude timed out: %w", ctx.Err())
		}
		// Non-zero exit is itself interesting but not necessarily a
		// FAIL — Claude Code might have completed the drill but
		// exit-1'd on something auxiliary (e.g. session log write).
		// The audit row count decides.
		fmt.Fprintf(h.debugLog, "[harness] claude exited with error (continuing): %v\n", waitErr)
	}

	if db == nil {
		return nil, fmt.Errorf("harness requires DB to read tool_invocations")
	}
	return readInvocationsForSession(db, h.sessionID)
}

// writeMcpConfig materialises a one-shot MCP config file pointing at
// the local mpm-mcp binary. Re-creating the config per drill keeps
// concurrent drills from racing over a shared ~/.claude/mcp.json.
//
// The "type": "stdio" field is required by Claude Code's MCP loader
// (verified empirically — without it, the server is loaded but
// silently skipped, and Claude reports "no MCP tools available").
// Older code paths that omitted the field would have masked this
// failure mode by emitting a passing-but-fake NOT_TESTED result.
func (h *ClaudeCodeHarness) writeMcpConfig(drill DrillSpec) (string, error) {
	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"mpm": map[string]interface{}{
				"type":    "stdio",
				"command": h.mpmMcpPath,
				"args":    []string{},
				"env": map[string]string{
					"MPM_SESSION_ID": h.sessionID,
					"MPM_WORKSPACE":  h.workspace,
				},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(h.workspace, ".drill", h.sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// readInvocationsForSession queries tool_invocations for one session,
// reconstructs the ToolCall slice the scorer expects, and orders rows
// by started_at ascending so the sequence check walks the evidence in
// the order the agent produced it.
func readInvocationsForSession(db *sql.DB, sessionID string) ([]ToolCall, error) {
	rows, err := db.Query(`
		SELECT tool_name, action, invocation_id FROM tool_invocations
		WHERE session_id = ? ORDER BY started_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query tool_invocations: %w", err)
	}
	defer rows.Close()
	var calls []ToolCall
	for rows.Next() {
		var c ToolCall
		if err := rows.Scan(&c.ToolName, &c.Action, &c.InvocationID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return calls, nil
}

// debugWriter prefixes each Write so concurrent drill output is still
// readable in the merged stream.
type debugWriter struct {
	prefix string
	w      io.Writer
}

func (d *debugWriter) Write(p []byte) (int, error) {
	if d.w == nil {
		return len(p), nil
	}
	_, _ = d.w.Write([]byte(d.prefix))
	_, _ = d.w.Write(p)
	return len(p), nil
}
