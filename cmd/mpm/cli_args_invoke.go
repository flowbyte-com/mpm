// cli_args_invoke.go — direct tool invocation helper for CLI handlers.
//
// Stage S5 of the approved CLI refactor (see
// docs/archive/mpm-cli-overhaul-investigation-2026-09-06.md §9).
// Lets a top-level CLI command invoke a registered tool directly
// (bypassing the JSON envelope that `mpm call <tool>` emits) so
// the CLI can render its own human or `--json` output from the
// tool's structured result.
//
// Why a separate helper rather than calling `mpm call` via
// subprocess:
//
//   - subprocess startup is slow;
//   - `mpm call` opens its own DatabaseManager (`openCallDM`),
//     producing two open DB connections per invocation;
//   - `mpm call` always emits a JSON envelope on stdout, which
//     the human-mode CLI must NOT do.
//
// invokeTool reuses the CLI singleton DatabaseManager and the
// ActiveContext built from CLI globals + env vars, then calls the
// tool's registered Handler directly. It records the same audit
// row `mpm call` records, so tool_invocations provenance is
// preserved.
//
// S1-S4 helper invariants are NOT touched by this file:
//   - splitOnDashDash, ExtractJSONFlag, parseBoundedInt, parseEnum
//     remain at their established signatures.
//   - This file is the *thin adapter layer* that delegates to the
//     canonical tool path. It does not parse CLI argv directly
//     (callers do); it does not render CLI output (callers do).
//
// DOCUMENTED INTENTIONAL ASYMMETRIES vs `mpm call` (S7 classification;
// preserved by the final-pass regression tests in
// d_invoke_tool_asymmetries_test.go):
//
//   - Mode / Persona are NOT propagated. Both `mpm call` and
//     `invokeTool` build ActiveContext without these fields;
//     they are package globals (CLI) or config files (MCP), used
//     by specific handlers to stamp memory metadata. Neither
//     surface treats them as ActiveContext fields.
//
//   - wireToolsGlobals is NOT called. `mpm call` wires blob store
//     + pointer resolver adapters because the cross-process
//     surface may invoke mpm_blob_read / mpm_resolve. The
//     in-process cognitive-verb consumers (mpm show, mpm lesson
//     add, mpm decide) do not touch blob/pointer. A future
//     in-process CLI command that needs blob/pointer can call
//     wireToolsGlobals itself before invoking; we do not
//     install global state on every cognitive invocation.
//
//   - Heartbeat is NOT bumped. `mpm call` calls dm.Heartbeat
//     because the cross-process surface is the supervision
//     boundary; in-process CLI invocations are not a session
//     liveness signal and must not silently mutate that view
//     every time an operator runs a read-mostly command.
//
//   - CheckPendingWakes is NOT folded. `mpm call` opportunistically
//     folds due scheduled/event wakes into the response envelope
//     for the agent runtime wake-on-respond loop. The dedicated
//     `mpm wake` command is the operator surface for due wakes;
//     folding into every invokeTool would conflate two surfaces'
//     contracts.
package main

import (
	"fmt"
	"os"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// invokeTool directly invokes a tool's registered Handler and returns
// the raw result map. Records an audit row in tool_invocations
// (same instrumentation as `mpm call`).
//
// Returns:
//   - (map, nil)            on success; the map is the tool's result.
//   - (nil-or-partial, err) on failure; err is deterministic.
//
// The caller is responsible for rendering the result. The CLI
// wrapper around invokeTool should:
//
//   - render human output from the result map;
//   - render `--json` output from the result map;
//   - emit error messages via usererror.Error or respond(...).
//
// This helper does NOT emit JSON envelopes. That is the difference
// from `mpm call`. CLI handlers invoke the canonical tool path
// for the semantics but own the output rendering.
func invokeTool(name string, payload map[string]interface{}) (map[string]interface{}, error) {
	dm := getDBConcrete()
	if dm == nil {
		return nil, fmt.Errorf("database unavailable")
	}
	tool, ok := tools.ByName(name)
	if !ok {
		// Internal failure — name should have been verified at
		// handler design time. Surface a precise error rather than
		// a silent no-op.
		return nil, fmt.Errorf("internal: tool %q is not registered", name)
	}
	// S7 fix: provenance env lookups. Read MPM_PROVENANCE_FRAMEWORK
	// (canonical) / MPM_FRAMEWORK (legacy alias), MPM_PROVENANCE_MODEL,
	// MPM_PROVENANCE_INVOCATION_ID, and MPM_PROVENANCE_PARENT_INVOCATION_ID
	// so CLI-originated tool calls carry the same provenance as
	// `mpm call` does. Pre-fix code hard-coded FrameworkName="mpm-cli"
	// and left InvocationID empty — the audit trail lost the
	// agent-runtime attribution when a CLI command delegated through
	// invokeTool instead of mpm call.
	frameworkName := os.Getenv("MPM_PROVENANCE_FRAMEWORK")
	if frameworkName == "" {
		frameworkName = os.Getenv("MPM_FRAMEWORK")
	}
	if frameworkName == "" {
		frameworkName = "mpm-cli"
	}
	ac := mpminternal.ActiveContext{
		SessionID:           getOrMakeSessionID(),
		Agent:               resolveAgentID(),
		Hostname:            resolveHostname(),
		Model:               os.Getenv("MPM_PROVENANCE_MODEL"),
		FrameworkName:       frameworkName,
		InvocationID:        os.Getenv("MPM_PROVENANCE_INVOCATION_ID"),
		ParentInvocationID:  os.Getenv("MPM_PROVENANCE_PARENT_INVOCATION_ID"),
	}
	startedAt := time.Now()
	result, err := tool.Handler(dm, ac, payload)
	completedAt := time.Now()
	if db := dm.SQLDB(); db != nil {
		recordToolInvocation(db, ac, name, payload,
			startedAt, completedAt, extractAction(payload), auditStatus(err), err)
	}
	if err != nil {
		// Tool returned an error. result may still carry a partial
		// map (e.g. F12-1 embedding_status="unavailable" still
		// reports memory_id); return both so the caller can render
		// the partial info.
		m, _ := result.(map[string]interface{})
		return m, err
	}
	m, _ := result.(map[string]interface{})
	if m == nil {
		return map[string]interface{}{}, nil
	}
	return m, nil
}