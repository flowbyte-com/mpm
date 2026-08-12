// audit_hook.go — central audit inserter for tool dispatch (CLI and MCP).
//
// Every `mpm call <tool>` and every MCP tools/call MUST route through
// recordToolInvocation so the drill orchestrator can derive per-session
// tool-call sequences from the tool_invocations table. Skipping this hook
// makes the producing framework invisible to the compatibility matrix.
//
// Panic-isolated by design: an audit failure (table missing, DB locked)
// must NEVER block the underlying tool call or alter its exit code. Audit
// is observability, not correctness.

package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// Actor kinds used by recordToolInvocation. `drill` separates synthetic
// harness-driven rows from real agent runs so the report command can
// filter them.
const (
	auditActorAgent  = "agent"
	auditActorHuman  = "human"
	auditActorDrill  = "drill"
	auditActorMCP    = "agent" // MCP path still surfaces as "agent"; framework_name='mcp'
)

// recordToolInvocation inserts a row into tool_invocations capturing the
// dispatch metadata required by the drill scorer. The insert is best-effort:
// any error is logged at slog.Warn but does not propagate (audit failures
// must not block the tool call).
//
// Parameters mirror the table columns; nil/zero values fall back to safe
// defaults ("unknown", "0", empty-string) so a partial audit row is still
// useful for debugging without poisoning the score.
func recordToolInvocation(
	dm sqlDBLike,
	ac mpminternal.ActiveContext,
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
		// Defensive: a panic in the audit path must never propagate.
		if r := recover(); r != nil {
			slog.Warn("audit insert panic", "tool", toolName, "panic", fmt.Sprint(r))
		}
	}()

	invocationID := uuid.NewString()
	id := uuid.NewString()
	sessionID := ac.SessionID
	if sessionID == "" {
		sessionID = "cli-default"
	}
	frameworkName := ac.FrameworkName
	if frameworkName == "" {
		frameworkName = "mpm-cli"
	}
	actorKind := auditActorHuman
	if frameworkName == "mcp" {
		actorKind = auditActorMCP
	}

	var errorMessage string
	if err != nil {
		errorMessage = err.Error()
	}

	_, auditErr := dm.Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, toolName, action, invocationID,
		actorKind, frameworkName, sha256OfPayload(payload), resultStatus,
		startedAt.Unix(), completedAt.Unix(), completedAt.Sub(startedAt).Milliseconds(),
		errorMessage,
	)
	if auditErr != nil {
		slog.Warn("audit insert failed", "tool", toolName, "err", auditErr.Error())
	}
}

// sqlDBLike is the minimal surface we need to insert audit rows. Both
// *mpminternal.DatabaseManager (via SQLDB()) and the CoreDB interface
// satisfy this — accepting a tiny contract keeps the helper testable
// without dragging in the full CoreDB surface.
type sqlDBLike interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// extractAction reads payload["action"] as a string, defaulting to "".
func extractAction(payload map[string]interface{}) string {
	if v, ok := payload["action"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// sha256OfPayload hashes the JSON-encoded payload. Keys are sorted by json
// encoding so semantically-equivalent payloads hash identically across the
// CLI/MCP boundary.
func sha256OfPayload(payload map[string]interface{}) string {
	if payload == nil {
		return fmt.Sprintf("sha256:%s", sha256.Sum256([]byte("{}")))
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Cannot marshal: fall back to a stable numeric placeholder so the
		// row still inserts.
		return fmt.Sprintf("sha256:%s", strconv.Itoa(len(payload)))
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum)
}
