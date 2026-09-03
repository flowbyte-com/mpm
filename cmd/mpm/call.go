package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// openCallDM returns a freshly-opened workspace DatabaseManager and a
// close function the caller must defer. Tests that need to inject a
// temp DB should open one directly and pass it to the registry handler
// — the registry's HandlerFunc signature takes dm explicitly, so there's
// no need for a global override.
func openCallDM() (mpminternal.CoreDB, func(), error) {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return nil, nil, fmt.Errorf("db: %w", err)
	}
	return dm, func() { dm.Close() }, nil
}

// handleCall is the main entry point for `mpm call <tool>`.
//
// Payload is read from one of three sources (in priority order):
//  1. --payload <json>     inline JSON arg (avoid with apostrophes/quotes)
//  2. --payload-file <p>   JSON file path (no shell escaping)
//  3. stdin                pipe or redirect (`echo ... | mpm call ...` or `< file`)
//
// If none are present, returns an empty payload (some tools need no input).
//
// Error contract (F1): every failure that occurs before or during handler
// dispatch emits a parseable {"success":false,"error":...} envelope on
// STDOUT. `mpm call` is a machine interface; agent adapters read stdout
// only, so an error that writes nothing to stdout manifests downstream as
// "no parseable JSON" instead of a precise message.
func handleCall(args []string) int {
	if len(args) < 1 {
		writeEnvelope(os.Stdout, map[string]interface{}{
			"success": false,
			"error":   "usage: mpm call <tool_name> [--payload <json> | --payload-file <path>] | (stdin)",
		})
		printError("usage: mpm call <tool_name> [--payload <json> | --payload-file <path>] | (stdin)")
		return 1
	}

	toolName := args[0]

	payload, err := parsePayload(args[1:])
	if err != nil {
		writeEnvelope(os.Stdout, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("failed to parse payload: %v", err),
		})
		return 1
	}

	// Legacy tool-name aliasing (F1): the retired `mpm_session` surface is
	// still registered by agent plugins (opencode-mpm, pi-mpm). Routing it
	// here keeps those callers working against the split
	// mpm_handoff/mpm_scratchpad tools instead of failing with empty stdout.
	resolvedName := toolName
	if action, target, params, ok := resolveLegacySessionAlias(toolName, payload); ok {
		resolvedName = target
		// Rebuild the canonical {action, params} envelope the registry
		// handlers expect.
		payload = map[string]interface{}{"action": action, "params": params}
	}

	tool, ok := tools.ByName(resolvedName)
	if !ok {
		writeEnvelope(os.Stdout, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("unknown tool: %s. available: %s", toolName, strings.Join(tools.Names(), ", ")),
		})
		return 1
	}

	// Open DB for this call. The CLI dispatcher owns the lifetime;
	// handlers in the registry receive dm as their first arg.
	dm, closeDM, err := openCallDM()
	if err != nil {
		printError("failed to open db: %v", err)
		return 1
	}
	defer closeDM()

	// F8 fix (2026-08-27): wire the blob store + pointer resolver before
	// dispatch. The MCP server does this once at boot in
	// cmd/mpm-mcp/tools.go RegisterAllTools; the CLI did not, which
	// meant `mpm call mpm_blob_read` returned "blob store not initialized"
	// even though the user's intent was supported. Wiring is per-call:
	// cheap, idempotent (SetBlobStore overwrites the previous global),
	// and survives closeDM via the close of any resources the wiring
	// opened. If wiring fails (e.g., blob dir not writable), we log and
	// continue — the handler will surface the canonical "blob store not
	// initialized" error rather than crashing the call.
	wireToolsGlobals(dm)

	// Build the ActiveContext from CLI-detected mode/persona.
	// The MCP server constructs this once at boot and passes it directly;
	// the CLI derives it from the active.json file on disk.
	//
	// Provenance env vars allow non-MPM callers (Claude Code, OpenCode, etc.)
	// to identify themselves properly. MPM_PROVENANCE_FRAMEWORK is the
	// canonical name; MPM_FRAMEWORK is accepted as a legacy alias.
	injectActiveContext()
	defer clearActiveContext()
	frameworkName := os.Getenv("MPM_PROVENANCE_FRAMEWORK")
	if frameworkName == "" {
		frameworkName = os.Getenv("MPM_FRAMEWORK")
	}
	if frameworkName == "" {
		frameworkName = "mpm-cli"
	}
	invocationID := os.Getenv("MPM_PROVENANCE_INVOCATION_ID")
	if invocationID == "" {
		invocationID = uuid.NewString()
	}
	ac := mpminternal.ActiveContext{
		Mode:       activeMode,
		Persona:    activePersona,
		SessionID:  getOrMakeSessionID(),
		Agent:      resolveAgentID(),
		Hostname:   resolveHostname(),
		Model:      os.Getenv("MPM_PROVENANCE_MODEL"),
		FrameworkName: frameworkName,
		InvocationID: invocationID,
		ParentInvocationID: os.Getenv("MPM_PROVENANCE_PARENT_INVOCATION_ID"),
	}

	// F-D2: explicit per-call provenance override. When the payload
	// carries framework / model / session_id / agent, those values
	// take precedence over the env-derived defaults. This is the
	// documented mechanism for MCP / agent-runtime callers to identify
	// themselves when MPM_PROVENANCE_FRAMEWORK is unset or set to the
	// generic "mpm-cli" default. Precedence:
	//   1. explicit trusted runtime provenance (env-supplied host ID)
	//   2. payload-supplied provenance (this block)
	//   3. surface-derived provenance (mpm-cli fallback)
	// An empty payload value is ignored — the env var wins.
	if v, ok := payloadStringField(payload, "framework"); ok && v != "" {
		ac.FrameworkName = v
	}
	if v, ok := payloadStringField(payload, "model"); ok && v != "" {
		ac.Model = v
	}
	if v, ok := payloadStringField(payload, "session_id"); ok && v != "" {
		ac.SessionID = v
	}
	if v, ok := payloadStringField(payload, "agent"); ok && v != "" {
		ac.Agent = v
	}
	if v, ok := payloadStringField(payload, "invocation_id"); ok && v != "" {
		ac.InvocationID = v
	}

	// Passive heartbeat (Arc 2): every MPM call that touches the
	// shared DB silently bumps shared.sessions. Non-fatal: a
	// heartbeat failure must NOT block the call. The DB call is
	// a single INSERT OR REPLACE under the hood; in the worst
	// case the shared DB is busy and we skip the heartbeat this
	// turn. Discovery window is 24h so transient skips are fine.
	_ = dm.Heartbeat(ac.SessionID, ac.Agent, ac.Hostname, nil)

	startedAt := time.Now()
	result, err := tool.Handler(dm, ac, payload)
	completedAt := time.Now()
	// Audit insert is best-effort and panic-isolated; failure here does
	// NOT affect the call's exit code or downstream error handling. See
	// audit_hook.go for the isolation guarantees.
	recordToolInvocation(dm.SQLDB(), ac, resolvedName, payload,
		startedAt, completedAt, extractAction(payload), auditStatus(err), err)
	if err != nil {
		// Structured error envelope. Both success and error envelopes route
		// through writeEnvelope so the stdout/stderr contract is uniform:
		// JSON envelope on stdout, zap log lines on stderr. The previous
		// implementation wrote the error envelope to stderr, which broke
		// every agent adapter that follows the documented contract by
		// reading only stdout (see lesson 2b22765cd1b13a81).
		writeEnvelope(os.Stdout, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return 1
	}

	// Opportunistic wake fold — compatibility safety net for the
	// deadline-driven mpm-scheduler daemon. As of the deadline-driven
	// work, mpm-scheduler is the authoritative dispatcher for
	// notification-kind scheduled_wakes at their target_time. This
	// fold remains so that:
	//
	//   1. When the daemon is unavailable (down for restart, missing
	//      install, single-shot mpm call from a CI script) the agent
	//      still sees due wakes in the response payload.
	//   2. Latency on cross-process writes is bounded: any `mpm call`
	//      triggers a fold, picking up wakes the bounded-sleep floor in
	//      Run() would otherwise have to wait up to `interval` for.
	//
	// Atomic safety: dm.CheckPendingWakes performs an UPDATE WHERE
	// fired=0 in a single transaction. The daemon's dispatchClaim
	// uses the same shape. When both run on the same database, exactly
	// one wins; the loser sees the row already claimed (fired=1).
	// No double-fire, no double-delivery to the agent.
	//
	// Cost: one indexed SELECT + UPDATE per `mpm call` (sub-millisecond
	// on WAL with the partial idx_sw_pending index). The dispatch
	// daemon and this fold share the cost budget; both are read-only
	// cheap.
	//
	// Arc 2: also fold EventWakesPending from shared.event_wakes when the
	// session has an ID. The receiving agent sees incoming epistemic
	// events alongside its own scheduled tasks.
	if resultMap, ok := result.(map[string]interface{}); ok {
		if due, dErr := dm.CheckPendingWakes(time.Now(), nil); dErr == nil && len(due) > 0 {
			resultMap["WakesPending"] = due
			resultMap["WakesPendingCount"] = len(due)
		}
		if ac.SessionID != "" {
			if eventWakes, eErr := dm.CheckPendingEventWakes(ac.SessionID); eErr == nil && len(eventWakes) > 0 {
				resultMap["EventWakesPending"] = eventWakes
				resultMap["EventWakesPendingCount"] = len(eventWakes)
			}
		}
		result = resultMap
	}

	writeEnvelope(os.Stdout, result)
	return 0
}

// resolveLegacySessionAlias maps the retired `mpm_session` tool onto its
// current registry equivalents. The session surface was split into
// mpm_handoff (cross-session) and mpm_scratchpad (intra-session), but the
// agent plugins still register `mpm_session` with the old action names —
// every such call used to die as "no parseable JSON" (F1).
//
// Accepts both wire shapes: {action, params:{...}} and legacy top-level
// {action, ...fields}. Returns (mapped action, target tool, params, ok).
func resolveLegacySessionAlias(toolName string, payload map[string]interface{}) (string, string, map[string]interface{}, bool) {
	if toolName != "mpm_session" {
		return "", "", nil, false
	}
	action, _ := payload["action"].(string)
	params, _ := payload["params"].(map[string]interface{})
	if params == nil {
		// Legacy shape: fields at top level alongside action.
		params = make(map[string]interface{}, len(payload))
		for k, v := range payload {
			if k != "action" {
				params[k] = v
			}
		}
	} else {
		// Copy so we never mutate a map shared with the caller.
		cp := make(map[string]interface{}, len(params))
		for k, v := range params {
			cp[k] = v
		}
		params = cp
	}
	switch action {
	case "end":
		return "write", "mpm_handoff", params, true
	case "handoff":
		return "read", "mpm_handoff", params, true
	case "list_handoffs":
		return "list", "mpm_handoff", params, true
	case "flush":
		return "flush", "mpm_scratchpad", params, true
	case "read":
		return "read", "mpm_scratchpad", params, true
	case "discard":
		return "discard", "mpm_scratchpad", params, true
	case "promote_scratchpad":
		return "promote", "mpm_scratchpad", params, true
	default:
		return "", "", nil, false
	}
}

// writeEnvelope serializes v as JSON and writes it as a single line to w,
// followed by a newline. Both the success and error paths of `mpm call`
// route through this so the stdout/stderr contract is uniform:
//
//	stderr  → zap log lines (file perms sweep, audit, etc.)
//	stdout  → JSON envelope (success or failure)
//
// Centralizing the write means a future change to the envelope format
// (e.g. NDJSON, JSON Lines) is a one-line edit, and the contract is
// testable in isolation. See TestCallErrorEnvelope_RoutesToStdout for
// the contract test.
func writeEnvelope(w io.Writer, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		// json.Marshal of a concrete map[string]interface{} or registry
		// handler result can't fail — this is a defensive guard for future
		// payload shapes. Surface a synthetic error envelope so the caller
		// still gets parseable JSON.
		data = []byte(`{"success":false,"error":"envelope marshal failed"}`)
	}
	_, _ = fmt.Fprintln(w, string(data))
}

// parsePayload extracts JSON from --payload flag, --payload-file flag, or stdin.
// If none are present, returns an empty map (some tools need no input).
//
// Stdin is detected via hasStdinData() (ModeCharDevice check), which correctly
// detects pipes and file redirection. The previous implementation used
// stat.Size() > 0, which silently failed for pipes — Stat returns Size=0 for
// pipes even when data is available, so the input was never read.
func parsePayload(args []string) (map[string]interface{}, error) {
	// Last-flag-wins for --payload and --payload-file, matching standard CLI
	// convention (git, kubectl, etc.). Iterate fully, remember the most
	// recent hit, return after the loop. If both are present, the later one
	// in argv order wins.
	var (
		inlineJSON string
		inlineSet  bool
		filePath   string
		fileSet    bool
	)
	for i := 0; i < len(args); i++ {
		if args[i] == "--payload" && i+1 < len(args) {
			inlineJSON = args[i+1]
			inlineSet = true
			fileSet = false // --payload overrides any earlier --payload-file
			i++
			continue
		}
		if args[i] == "--payload-file" && i+1 < len(args) {
			filePath = args[i+1]
			fileSet = true
			inlineSet = false // --payload-file overrides any earlier --payload
			i++
			continue
		}
	}
	if inlineSet {
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(inlineJSON), &p); err != nil {
			return nil, fmt.Errorf("--payload: %w", err)
		}
		return p, nil
	}
	if fileSet {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("--payload-file: %w", err)
		}
		// Trim BOM if present (some editors write UTF-8 BOM)
		s := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return nil, fmt.Errorf("--payload-file: %w", err)
		}
		return p, nil
	}
	// No flag; try stdin if data is piped or redirected.
	// hasStdinData uses ModeCharDevice — correct for both pipes (Size=0 but
	// has data) and file redirection (Size>0).
	if hasStdinData() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		// Trim BOM if present (some editors write UTF-8 BOM)
		s := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
		if len(s) > 0 {
			var p map[string]interface{}
			if err := json.Unmarshal([]byte(s), &p); err != nil {
				return nil, fmt.Errorf("stdin: %w", err)
			}
			return p, nil
		}
	}
	return map[string]interface{}{}, nil
}

// payloadStringField reads a top-level string field from a payload. It
// accepts both the canonical {action, params} envelope (looking in
// params) and a flat payload (looking at top level). Returns ok=false
// if the field is absent or not a string; ok=true with empty string if
// the field is present but empty — caller decides whether empty
// overrides.
func payloadStringField(payload map[string]interface{}, key string) (string, bool) {
	if v, ok := payload[key]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	if params, ok := payload["params"].(map[string]interface{}); ok {
		if v, ok := params[key]; ok {
			if s, ok := v.(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

// must is a tiny helper for JSON marshalling where marshalling errors are
// programming bugs (we always pass concrete map[string]interface{} or
// []byte payloads, both of which marshal cleanly). The trailing-error arg
// matches the (data, err) return shape of json.Marshal so call sites read
// naturally: `must(json.Marshal(x))`.
func must(data []byte, _ error) []byte { return data }

// runHandler invokes a registry handler with the test DM and an empty
// ActiveContext. Tests that previously called callFoo(payload) directly
// should use this instead — it routes through the same code path
// production uses, so CLI/MCP parity is verified by construction.
//
// The handler name must match a tool in tools.Registry. Tests that want
// to assert on a specific tool look up the entry via tools.ByName.
func runHandler(dm *mpminternal.DatabaseManager, name string, payload map[string]interface{}) (interface{}, error) {
	tool, ok := tools.ByName(name)
	if !ok {
		return nil, fmt.Errorf("test: unknown tool %q", name)
	}
	ac := mpminternal.ActiveContext{SessionID: "test-run-handler"}
	startedAt := time.Now()
	result, err := tool.Handler(dm, ac, payload)
	completedAt := time.Now()
	recordToolInvocation(dm.SQLDB(), ac, name, payload,
		startedAt, completedAt, extractAction(payload), auditStatus(err), err)
	return result, err
}

// auditStatus maps a non-nil error to "error" and a nil error to "success",
// matching the CHECK constraint on tool_invocations.result_status.
func auditStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}
