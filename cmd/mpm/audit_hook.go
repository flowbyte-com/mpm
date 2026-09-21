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
	auditActorAgent = "agent"
	auditActorHuman = "human"
	auditActorDrill = "drill"
	auditActorMCP   = "agent" // MCP path still surfaces as "agent"; framework_name='mcp'
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

	invocationID := ac.InvocationID
	if invocationID == "" {
		invocationID = uuid.NewString()
	}
	id := uuid.NewString()
	// Three independent session dimensions (Stage 2C.1):
	//
	//   session_id           — legacy / local dispatcher grouping
	//                          (per-process UUID from
	//                          getOrMakeSessionID). Distinct from
	//                          both canonical identities by design.
	//   mpm_session_id       — MPM-owned continuity identity from
	//                          ActiveContext. Empty when no active
	//                          MPM session exists for the dispatch
	//                          (fresh workspace, before first
	//                          acquire). NEVER synthesized from
	//                          session_id.
	//   framework_session_id — host-owned native session identity
	//                          from ActiveContext (=
	//                          MPM_PROVENANCE_FRAMEWORK_SESSION_ID).
	//                          Empty when host has no native
	//                          session. NEVER synthesized from
	//                          mpm_session_id or session_id.
	sessionID := ac.SessionID
	if sessionID == "" {
		sessionID = "cli-default"
	}
	frameworkName := ac.FrameworkName
	if frameworkName == "" {
		frameworkName = "mpm-cli"
	}
	// Use the canonical EffectiveActorKind so the bug-fix
	// (framework=openclaw → human) cannot reappear in a new
	// audit-hook variant. Historical rows with the old broken
	// classification are normalized at read time by the same
	// helper in recent_activity.go.
	actorKind := mpminternal.EffectiveActorKind(auditActorHuman, frameworkName)

	var errorMessage string
	if err != nil {
		errorMessage = err.Error()
	}

	// Helper: write nullable column or NULL.
	nullStr := func(s string) interface{} {
		if s == "" {
			return nil
		}
		return s
	}

	// RUNTIME OUTCOME WIRING (aa04fc2a+): classify the dispatch
	// outcome at write-time. result_status stays success|error for
	// compatibility (the existing CHECK constraint enforces the
	// vocabulary). outcome_class and outcome_code are the new typed
	// axis; null on success (class is implicit) and the bounded
	// code on error via the common ClassifyError seam in
	// internal/core/tool_outcome.go.
	outcomeClass, outcomeCode := computeToolOutcome(resultStatus, err)

	_, auditErr := dm.Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms, error_message,
		     mpm_session_id, framework_session_id,
		     outcome_class, outcome_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, toolName, action, invocationID,
		actorKind, frameworkName, sha256OfPayload(payload), resultStatus,
		startedAt.Unix(), completedAt.Unix(), completedAt.Sub(startedAt).Milliseconds(),
		errorMessage,
		nullStr(ac.MPMSessionID),
		nullStr(ac.FrameworkSessionID),
		string(outcomeClass), outcomeCode,
	)
	if auditErr != nil {
		slog.Warn("audit insert failed", "tool", toolName, "err", auditErr.Error())
	}
}

// computeToolOutcome returns the closed-vocabulary outcome_class for a
// completed dispatch. result_status == "success" maps to outcome_class
// "ok" with empty outcome_code (success is the implicit absence of a
// typed failure code). result_status == "error" maps through
// mpminternal.ClassifyError, which recognises typed sentinels,
// context errors, and a narrow set of well-known message prefixes
// (see internal/core/tool_outcome.go for the full contract).
//
// Degraded-success is NOT wired here: only tool dispatch boundaries
// produce outcome_class=="degraded", and the existing handler chain
// has no degraded semantic in normal happy-path routes. Wake-context
// projection already records contextual_focus.status internally;
// wiring that to the audit row would require introspecting the
// payload (brief §14 forbids). Future stage.
func computeToolOutcome(resultStatus string, err error) (class mpminternal.ToolOutcomeClass, code string) {
	if resultStatus == "success" {
		return mpminternal.OutcomeClassOk, ""
	}
	class, code = mpminternal.ClassifyError(err)
	if class == "" {
		return mpminternal.OutcomeClassInternal, "unclassified"
	}
	if !mpminternal.ValidClass(class) {
		return mpminternal.OutcomeClassInternal, "unclassified"
	}
	return class, code
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
