// audit_hook.go — central audit inserter for MCP tools/call dispatch.
//
// Every MCP tools/call routed through mcpAdapter MUST record an audit row
// in tool_invocations with framework_name='mcp'. The CLI dispatcher has
// its own copy of this helper in cmd/mpm/audit_hook.go. Both copies write
// the same INSERT shape; a future refactor moves them into a shared
// internal/audit_hook package once a third caller appears.
//
// Panic-isolated by design. See cmd/mpm/audit_hook.go for the full
// contract.

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	core "github.com/flowbyte-com/mpm-core"
)

// recordToolInvocation inserts an audit row capturing the dispatch
// metadata required by the drill scorer. Best-effort: errors are logged
// at warn but never propagated to the caller.
func recordToolInvocation(
	dm *core.DatabaseManager,
	ac core.ActiveContext,
	toolName string,
	payload map[string]interface{},
	startedAt time.Time,
	completedAt time.Time,
	action string,
	resultStatus string,
	err error,
) {
	if dm == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("audit insert panic", "tool", toolName, "panic", fmt.Sprint(r))
		}
	}()

	invocationID := ac.InvocationID
	if invocationID == "" {
		invocationID = uuid.NewString()
	}
	id := uuid.NewString()
	frameworkName := "mcp"
	if ac.FrameworkName != "" && ac.FrameworkName != "mcp" {
		frameworkName = ac.FrameworkName
	}
	// Route through the canonical classifier so future MCP writes
	// match the read-time normalization. EffectiveActorKind keeps
	// explicit agent/human overrides and reclassifies based on
	// the framework (openclaw/pi/opencode/claude-code/hermes →
	// agent; mpm-cli → human).
	actorKind := core.EffectiveActorKind("agent", frameworkName)
	sessionID := ac.SessionID
	if sessionID == "" {
		sessionID = "mcp-default"
	}

	var errorMessage string
	if err != nil {
		errorMessage = err.Error()
	}

	// nullStr: empty-string -> SQL NULL guard (mirrors cmd/mpm/audit_hook.go).
	nullStr := func(s string) interface{} {
		if s == "" {
			return nil
		}
		return s
	}

	// OBSERVABILITY FOUNDATION (2026-09-21): populate the four
	// identity columns that the new schema added. The CLI writer
	// already does this; the MCP writer historically dropped
	// mpm_session_id and framework_session_id, leaving MCP rows
	// uncorrelated with the active MPM/host session. The persist
	// invariant (mpm_session_id is MPM-owned and never synthesized
	// from session_id; framework_session_id is host-owned and
	// never synthesized from mpm_session_id) is preserved — both
	// columns are NULL when the caller did not supply a value.
	_, auditErr := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms, error_message,
		     mpm_session_id, framework_session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, toolName, action, invocationID,
		actorKind, frameworkName, sha256OfPayload(payload), resultStatus,
		startedAt.Unix(), completedAt.Unix(), completedAt.Sub(startedAt).Milliseconds(),
		errorMessage,
		nullStr(ac.MPMSessionID),
		nullStr(ac.FrameworkSessionID),
	)
	if auditErr != nil {
		slog.Warn("audit insert failed", "tool", toolName, "err", auditErr.Error())
	}
}

// sha256OfPayload hashes the JSON-encoded payload so semantically-
// equivalent payloads hash identically across the CLI/MCP boundary.
func sha256OfPayload(payload map[string]interface{}) string {
	if payload == nil {
		return fmt.Sprintf("sha256:%s", sha256.Sum256([]byte("{}")))
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("sha256:%s", strconv.Itoa(len(payload)))
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum)
}

// extractAction reads payload["action"] as a string.
func extractAction(payload map[string]interface{}) string {
	if v, ok := payload["action"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
